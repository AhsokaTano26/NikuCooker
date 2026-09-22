package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/artifact"
	"github.com/AhsokaTano26/NikuCooker/internal/config"
	"github.com/AhsokaTano26/NikuCooker/internal/database"
	"github.com/AhsokaTano26/NikuCooker/internal/glossary"
	"github.com/AhsokaTano26/NikuCooker/internal/pipeline"
	"github.com/AhsokaTano26/NikuCooker/internal/provider"
	"github.com/AhsokaTano26/NikuCooker/internal/stage"
	contextstage "github.com/AhsokaTano26/NikuCooker/internal/stage/context"
	"github.com/AhsokaTano26/NikuCooker/internal/stage/segmentation"
	stagestranslation "github.com/AhsokaTano26/NikuCooker/internal/stage/translation"
	"github.com/AhsokaTano26/NikuCooker/internal/subtitle"
	translationcore "github.com/AhsokaTano26/NikuCooker/internal/translation"
	"github.com/AhsokaTano26/NikuCooker/pkg/protocol"
)

// The Phase 5 exit test: transcript in, translated subtitles out, and — the part
// that decides whether the system is usable — not paying twice for the same
// line.
//
// Two dependencies are stood in for, and it is worth being precise about what
// that does and does not leave untested.
//
// The recogniser is replaced by a stage that emits a fixed transcript. Phase 4
// tests the real one; what matters here is everything downstream of it, and
// running Whisper again would add a model download and a minute of CPU to every
// assertion below.
//
// The language model is a real HTTP server speaking the OpenAI protocol, reached
// through the real client. Nothing about the request construction, the auth
// header, the JSON-mode negotiation, the response parsing or the retry logic is
// simulated — the server records what it was asked, which is how the cache and
// prompt assertions are made.

// ---------------------------------------------------------------------------
// A recogniser that returns a fixed transcript
// ---------------------------------------------------------------------------

// transcriptStage stands in for ASR. Its transcript can be edited between runs,
// which is how the re-translation test simulates a user changing a line.
type transcriptStage struct {
	mu       sync.Mutex
	segments []protocol.ASRSegment
	language string
	runs     int
}

func (s *transcriptStage) Spec() stage.Spec {
	return stage.Spec{Name: "asr", Version: "1", ConfigKey: "asr"}
}

func (s *transcriptStage) ConfigSubtree(*config.Config) any { return nil }

func (s *transcriptStage) Fingerprint(context.Context, *stage.Env) (map[string]string, error) {
	return nil, nil
}

func (s *transcriptStage) Run(_ context.Context, env *stage.Env) (*stage.Result, error) {
	s.mu.Lock()
	s.runs++
	segments := append([]protocol.ASRSegment(nil), s.segments...)
	s.mu.Unlock()

	raw, err := json.Marshal(protocol.ASRResult{Language: s.language, Segments: segments})
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(env.OutDir, "asr.json"), raw, 0o644); err != nil {
		return nil, err
	}
	return &stage.Result{Primary: "asr.json", Model: "fixture"}, nil
}

// retime replaces one segment's text, leaving its timings alone.
//
// The timings are what the splitter works from, so leaving them identical means
// the same lines come out with the same ids and only the text differs — exactly
// the shape of a user correcting a mistranscribed line.
func (s *transcriptStage) retime(index int, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.segments[index].Text = text
	for i := range s.segments[index].Words {
		s.segments[index].Words[i].Text = ""
	}
	// Words with no text are dropped by the splitter, so the segment falls back
	// to being one word spanning its full range.
	s.segments[index].Words = nil
}

// ---------------------------------------------------------------------------
// A language model that records what it is asked
// ---------------------------------------------------------------------------

type recordedRequest struct {
	Model    string
	System   string
	User     string
	JSONMode bool

	// MaxTokens is what the caller asked to allow. Recorded because it is part
	// of the request's meaning: a budget too small for the answer truncates it,
	// and the reply alone does not say which side chose that.
	MaxTokens int
}

func (r recordedRequest) fullPrompt() string { return r.System + "\n" + r.User }

// linePattern finds the batch payload inside a rendered prompt.
var linePattern = regexp.MustCompile(`"id":"(seg_\d+)","source_text":"((?:[^"\\]|\\.)*)"`)

// requestedLines extracts the id→source pairs the prompt asked to translate.
func (r recordedRequest) requestedLines() map[string]string {
	out := map[string]string{}
	for _, match := range linePattern.FindAllStringSubmatch(r.User, -1) {
		var text string
		if err := json.Unmarshal([]byte(`"`+match[2]+`"`), &text); err != nil {
			text = match[2]
		}
		out[match[1]] = text
	}
	return out
}

// fakeLLM is an OpenAI-compatible endpoint.
type fakeLLM struct {
	mu       sync.Mutex
	server   *httptest.Server
	requests []recordedRequest

	// reply, when set, replaces the default translation behaviour. It receives
	// the request number, counting from one, so a test can script a bad first
	// answer and a good second one.
	reply func(attempt int, req recordedRequest) (int, string)
}

func newFakeLLM(t *testing.T) *fakeLLM {
	t.Helper()

	fake := &fakeLLM{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		var envelope struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			ResponseFormat *struct {
				Type string `json:"type"`
			} `json:"response_format"`
			// Either spelling: the client switches field names when a server
			// rejects one.
			MaxTokens           int `json:"max_tokens"`
			MaxCompletionTokens int `json:"max_completion_tokens"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			http.Error(w, `{"error":{"message":"bad request"}}`, http.StatusBadRequest)
			return
		}

		recorded := recordedRequest{
			Model:     envelope.Model,
			JSONMode:  envelope.ResponseFormat != nil,
			MaxTokens: envelope.MaxTokens,
		}
		if recorded.MaxTokens == 0 {
			recorded.MaxTokens = envelope.MaxCompletionTokens
		}
		for _, message := range envelope.Messages {
			if message.Role == "system" {
				recorded.System = message.Content
			} else {
				recorded.User += message.Content
			}
		}

		fake.mu.Lock()
		fake.requests = append(fake.requests, recorded)
		attempt := len(fake.requests)
		handler := fake.reply
		fake.mu.Unlock()

		status, payload := 0, ""
		if handler != nil {
			status, payload = handler(attempt, recorded)
		}
		// Zero means "no opinion", which is how a handler asks for the default
		// reply without having to construct one.
		if status == 0 {
			status = http.StatusOK
		}
		if payload == "" {
			payload = defaultTranslationReply(recorded)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(fake.server.Close)

	return fake
}

// defaultTranslationReply translates by echoing the source with a marker.
//
// Deterministic, and distinctive enough that a mistranslation is visible in an
// assertion: if a line reads "[zh]こんにちは" the pipeline never touched it, and
// if it reads anything else something else produced it.
func defaultTranslationReply(req recordedRequest) string {
	type item struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}

	lines := req.requestedLines()
	// Sorted via the id, so the reply order is stable and a test comparing two
	// replies is comparing content rather than map iteration.
	ids := make([]string, 0, len(lines))
	for id := range lines {
		ids = append(ids, id)
	}
	sortStrings(ids)

	items := make([]item, 0, len(ids))
	for _, id := range ids {
		items = append(items, item{ID: id, Text: "[zh]" + lines[id]})
	}

	// Two levels of encoding, as the API has: `content` is a string whose
	// value happens to be JSON, not a nested object.
	inner, _ := json.Marshal(map[string]any{"translations": items})
	content, _ := json.Marshal(string(inner))
	return fmt.Sprintf(`{
		"model": "fake-model",
		"choices": [{"message": {"role": "assistant", "content": %s}, "finish_reason": "stop"}],
		"usage": {"prompt_tokens": 100, "completion_tokens": 50, "total_tokens": 150}
	}`, string(content))
}

// requestCount reports how many requests have been served.
func (f *fakeLLM) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// reset forgets the recorded requests, keeping the server.
func (f *fakeLLM) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = nil
}

func (f *fakeLLM) last() recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

func (f *fakeLLM) all() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type phase5Harness struct {
	t          *testing.T
	db         *database.DB
	projectID  string
	projectDir string
	store      *artifact.Store
	cfg        *config.Config
	registry   *stage.Registry
	transcript *transcriptStage
	llm        *fakeLLM
	glossary   *glossary.Service
	log        *slog.Logger
}

// fixtureTranscript is recogniser output with the shape the splitter exists to
// handle: segments several seconds long, containing more than one sentence, with
// word timings.
func fixtureTranscript() []protocol.ASRSegment {
	return []protocol.ASRSegment{
		{
			Start: 0.0, End: 4.0, Text: "こんにちは。今日はよろしくお願いします。",
			Words: []protocol.WordTimestamp{
				{Start: 0.0, End: 0.7, Text: "こんにちは。"},
				{Start: 0.8, End: 1.4, Text: "今日は"},
				{Start: 1.5, End: 2.6, Text: "よろしく"},
				{Start: 2.7, End: 3.9, Text: "お願いします。"},
			},
		},
		{
			Start: 4.6, End: 8.0, Text: "それでは始めていきましょう。",
			Words: []protocol.WordTimestamp{
				{Start: 4.6, End: 5.3, Text: "それでは"},
				{Start: 5.4, End: 6.6, Text: "始めていき"},
				{Start: 6.7, End: 7.9, Text: "ましょう。"},
			},
		},
		{
			Start: 9.0, End: 12.5, Text: "はい、わかりました。",
			Words: []protocol.WordTimestamp{
				{Start: 9.0, End: 9.5, Text: "はい、"},
				{Start: 9.6, End: 11.0, Text: "わかりました。"},
			},
		},
	}
}

func newPhase5Harness(t *testing.T) *phase5Harness {
	t.Helper()

	root := t.TempDir()
	projectDir := filepath.Join(root, "projects", "proj_phase5")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}

	db, err := database.Open(database.Options{Path: filepath.Join(root, "niku.db")})
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := database.Migrate(context.Background(), db); err != nil {
		t.Fatalf("database.Migrate: %v", err)
	}

	const projectID = "proj_phase5"
	if _, err := db.Write.ExecContext(context.Background(), `
		INSERT INTO projects (id, name, source_path, source_language, target_language,
		                      style, status, config_json, created_at, updated_at)
		VALUES (?, 'phase 5', 'source/episode01.mp4', 'ja', 'zh-Hans',
		        'fansub', 'active', '{}', 'now', 'now')`, projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	llm := newFakeLLM(t)

	providers := provider.NewService(db)
	if err := providers.Save(context.Background(), &provider.Provider{
		Name:    "fake",
		Kind:    provider.KindLLM,
		Type:    provider.TypeOpenAICompatible,
		BaseURL: llm.server.URL,
		APIKey:  "test-key",
		Model:   "fake-model",
		Enabled: true,
	}); err != nil {
		t.Fatalf("seed provider: %v", err)
	}

	transcript := &transcriptStage{segments: fixtureTranscript(), language: "ja"}

	registry, err := stage.NewRegistry(
		transcript,
		segmentation.New(),
		stagestranslation.New(),
	)
	if err != nil {
		t.Fatalf("stage.NewRegistry: %v", err)
	}

	cfg := config.Default()
	cfg.Pipeline.Disabled = nil
	cfg.Translation.BatchSize = 10
	cfg.Translation.ContextLines = 1
	cfg.Translation.Concurrency = 1
	cfg.Translation.Timeout = 30 * time.Second
	cfg.Subtitle.MaxCPS = 18

	return &phase5Harness{
		t:          t,
		db:         db,
		projectID:  projectID,
		projectDir: projectDir,
		store:      artifact.NewStore(db, projectID, projectDir),
		cfg:        cfg,
		registry:   registry,
		transcript: transcript,
		llm:        llm,
		glossary:   glossary.NewService(db),
		log:        slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}
}

// registerContext rebuilds the registry with the optional analysis stage
// included.
//
// Registration order is pipeline order, so context goes after segmentation and
// before translation — the order its optional dependency requires.
func (h *phase5Harness) registerContext() {
	h.t.Helper()

	registry, err := stage.NewRegistry(
		h.transcript,
		segmentation.New(),
		contextstage.New(),
		stagestranslation.New(),
	)
	if err != nil {
		h.t.Fatalf("stage.NewRegistry with context: %v", err)
	}
	h.registry = registry
}

func (h *phase5Harness) options(force bool) pipeline.Options {
	return pipeline.Options{
		Registry:          h.registry,
		Config:            h.cfg,
		Store:             h.store,
		ProjectID:         h.projectID,
		JobID:             "job_phase5",
		CodeRevision:      "test-rev-1",
		SourceFingerprint: "v1:size=1:h=fixed",
		Project: stage.ProjectInfo{
			Name:           "phase 5",
			SourceLanguage: "ja",
			TargetLanguage: "zh-Hans",
			Style:          "fansub",
		},
		Services: stage.Services{
			Providers:   provider.NewService(h.db),
			Glossary:    h.glossary,
			Translation: translationcore.NewCache(h.db),
		},
		Force: force,
		Log:   h.log,
	}
}

// run executes the pipeline and returns the translated set.
func (h *phase5Harness) run(force bool) *subtitle.Set {
	h.t.Helper()

	ctx := context.Background()
	opts := h.options(force)

	plan, err := pipeline.Build(ctx, opts)
	if err != nil {
		h.t.Fatalf("pipeline.Build: %v", err)
	}
	if err := pipeline.Run(ctx, plan, opts, pipeline.NopObserver{}); err != nil {
		h.t.Fatalf("pipeline.Run: %v", err)
	}

	for _, sp := range plan.Stages {
		if sp.Stage.Spec().Name != "translation" {
			continue
		}
		// Either is a success: "completed" means this run computed it, and
		// "cached" means a previous run did. Both mean an artifact exists.
		if sp.State != pipeline.StateCompleted && sp.State != pipeline.StateCached {
			h.t.Fatalf("translation ended in state %s (reason: %q, error: %v), want completed or cached",
				sp.State, sp.Reason, sp.Err)
		}
		if sp.Artifact == nil {
			h.t.Fatalf("translation finished in state %s with no artifact", sp.State)
		}
		var set subtitle.Set
		if err := h.store.Decode(sp.Artifact, &set); err != nil {
			h.t.Fatalf("decode the translated set: %v", err)
		}
		return &set
	}

	h.t.Fatal("the plan contained no translation stage")
	return nil
}

// ---------------------------------------------------------------------------
// The expensive property: a re-run costs nothing
// ---------------------------------------------------------------------------

// Translating the same lines twice must not pay twice.
//
// Force is set on the second run so the stage cache cannot be what makes this
// pass. The artifact cache would trivially return the previous result; what is
// being tested is the *line* cache, which is what makes an edit to one line
// cheap.
func TestPhase5ReRunMakesNoRequests(t *testing.T) {
	h := newPhase5Harness(t)

	set := h.run(false)
	if len(set.Segments) == 0 {
		t.Fatal("the first run produced no lines")
	}
	if set.CountTranslated() != len(set.Segments) {
		t.Fatalf("the first run translated %d of %d lines",
			set.CountTranslated(), len(set.Segments))
	}

	first := h.llm.requestCount()
	if first == 0 {
		t.Fatal("the first run made no requests, so the cache is not what is being tested")
	}

	h.llm.reset()
	again := h.run(true)

	if got := h.llm.requestCount(); got != 0 {
		t.Errorf("the second run made %d requests, want 0; the line cache did not hit", got)
	}
	if again.CountTranslated() != len(again.Segments) {
		t.Errorf("the second run produced %d translations, want %d",
			again.CountTranslated(), len(again.Segments))
	}
	for i, segment := range again.Segments {
		if segment.TranslatedText == nil || *segment.TranslatedText != *set.Segments[i].TranslatedText {
			t.Errorf("line %s differs between runs: %v then %v",
				segment.ID, set.Segments[i].TranslatedText, segment.TranslatedText)
		}
	}
}

// Editing a few lines must re-translate those lines and nothing else.
//
// This is the property that makes the system usable on a long work: a two-hour
// film is two thousand lines, and a tool that re-translates all of them because
// one was wrong is a tool nobody runs twice.
func TestPhase5EditingThreeLinesTranslatesThreeLines(t *testing.T) {
	h := newPhase5Harness(t)
	h.run(false)

	h.llm.reset()

	// Three segments get corrected text, with their timings untouched.
	h.transcript.retime(0, "こんばんは。")
	h.transcript.retime(1, "それでは始めましょう。")
	h.transcript.retime(2, "はい、承知しました。")

	// The transcript stage produces a fresh artifact, so the segmentation stage
	// reruns and the translation stage gets a new input — but the lines the user
	// did not touch carry the same text, and the line cache is keyed on that.
	h.run(true)

	requested := map[string]bool{}
	for _, request := range h.llm.all() {
		for id := range request.requestedLines() {
			requested[id] = true
		}
	}

	if len(requested) == 0 {
		t.Fatal("no lines were requested after editing three of them")
	}

	// Every requested line must be one whose text changed. The splitter assigns
	// ids in order, so rebuilding the current lines recovers which text each id
	// now refers to.
	current := buildLines(t, h.transcript)
	for id := range requested {
		text, ok := current[id]
		if !ok {
			t.Errorf("line %s was requested but the current lines have no such id", id)
			continue
		}
		if !editedTexts[text] {
			t.Errorf("line %s (%q) was re-translated but its text did not change", id, text)
		}
	}
	if len(requested) != len(editedTexts) {
		t.Errorf("re-translated %d lines, want %d", len(requested), len(editedTexts))
	}

	if requests := h.llm.requestCount(); requests > 1 {
		t.Errorf("the edited lines took %d requests; they fit in one batch", requests)
	}
}

// editedTexts are the corrections applied by the re-translation test.
var editedTexts = map[string]bool{
	"こんばんは。":      true,
	"それでは始めましょう。": true,
	"はい、承知しました。":  true,
}

// buildLines runs the splitter over the stage's current transcript, which is how
// a test recovers the id-to-text mapping the pipeline is working from.
func buildLines(t *testing.T, source *transcriptStage) map[string]string {
	t.Helper()

	source.mu.Lock()
	segments := append([]protocol.ASRSegment(nil), source.segments...)
	source.mu.Unlock()

	lines, err := subtitle.Build(segments, "ja", "zh-Hans", subtitle.DefaultOptions())
	if err != nil {
		t.Fatalf("subtitle.Build: %v", err)
	}

	out := make(map[string]string, len(lines))
	for _, line := range lines {
		out[line.ID] = line.SourceText
	}
	return out
}

// ---------------------------------------------------------------------------
// Replying badly
// ---------------------------------------------------------------------------

// A reply wrapped in a code fence, or missing a line, must not cost a whole
// batch.
//
// Models do all three of these, and a pipeline that treats any of them as fatal
// fails on material that is genuinely translatable.
func TestPhase5RecoversFromMalformedReplies(t *testing.T) {
	cases := []struct {
		name string

		// first renders a deliberately wrong reply for request 1.
		first func(id string) string
	}{
		{
			name: "wrapped in a code fence",
			first: func(id string) string {
				return envelope("```json\n" + modelContent(map[string]string{id: "围栏"}) + "\n```")
			},
		},
		{
			name: "preceded by prose",
			first: func(id string) string {
				return envelope("Here is the translation you asked for:\n\n" +
					modelContent(map[string]string{id: "散文"}))
			},
		},
		{
			name: "a line is missing",
			first: func(string) string {
				// A reply that is valid JSON, is a well-formed translation list,
				// and simply omits everything. The worst case, because nothing
				// about it looks wrong until it is compared to the request.
				return translationReply(map[string]string{})
			},
		},
		{
			name: "a line comes back empty",
			first: func(id string) string {
				return translationReply(map[string]string{id: ""})
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newPhase5Harness(t)

			h.llm.reply = func(attempt int, req recordedRequest) (int, string) {
				if attempt > 1 {
					// The retry gets the honest default reply.
					return 0, ""
				}
				lines := req.requestedLines()
				ids := make([]string, 0, len(lines))
				for id := range lines {
					ids = append(ids, id)
				}
				sortStrings(ids)
				return 0, testCase.first(ids[0])
			}

			set := h.run(false)

			if set.CountTranslated() != len(set.Segments) {
				t.Fatalf("recovered %d of %d lines after a malformed reply",
					set.CountTranslated(), len(set.Segments))
			}
			if h.llm.requestCount() < 2 {
				t.Fatalf("the reply was malformed but no retry was made (%d request(s))",
					h.llm.requestCount())
			}

			// The retry must tell the model what was wrong. Re-asking the same
			// question unchanged tends to produce the same answer.
			last := h.llm.last()
			if !strings.Contains(last.User, "Correction") {
				t.Errorf("the retry did not carry a correction note:\n%s", last.User)
			}
		})
	}
}

// translationReply builds a well-formed HTTP response carrying the given
// translations.
func translationReply(translations map[string]string) string {
	return envelope(modelContent(translations))
}

// modelContent renders the JSON a model would put inside its message.
//
// Separate from the HTTP envelope so that a test can put arbitrary text there —
// which is how the malformed-reply cases are built. The distinction matters:
// a reply wrapped in a code fence is a well-formed HTTP response carrying
// content that is not bare JSON.
func modelContent(translations map[string]string) string {
	type item struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}

	ids := make([]string, 0, len(translations))
	for id := range translations {
		ids = append(ids, id)
	}
	sortStrings(ids)

	items := make([]item, 0, len(ids))
	for _, id := range ids {
		items = append(items, item{ID: id, Text: translations[id]})
	}

	encoded, _ := json.Marshal(map[string]any{"translations": items})
	return string(encoded)
}

// envelope wraps arbitrary model output in a well-formed HTTP response.
func envelope(content string) string {
	quoted, _ := json.Marshal(content)
	return fmt.Sprintf(`{
		"model": "fake-model",
		"choices": [{"message": {"role": "assistant", "content": %s}, "finish_reason": "stop"}],
		"usage": {"prompt_tokens": 100, "completion_tokens": 50}
	}`, string(quoted))
}

// A context line the model volunteers a translation for must not be written into
// a segment this batch was not responsible for.
func TestPhase5IgnoresTranslationsForUnrequestedLines(t *testing.T) {
	h := newPhase5Harness(t)

	h.llm.reply = func(attempt int, req recordedRequest) (int, string) {
		if attempt > 1 {
			return 0, ""
		}

		translations := map[string]string{
			"seg_0001": "被翻译",
			"seg_9999": "不该出现",
		}
		for id := range req.requestedLines() {
			translations[id] = "[zh]" + id
		}
		return 0, translationReply(translations)
	}

	set := h.run(false)

	for _, segment := range set.Segments {
		if segment.TranslatedText != nil && *segment.TranslatedText == "不该出现" {
			t.Fatalf("line %s received a translation meant for a line that was not requested",
				segment.ID)
		}
		if segment.TranslatedText == nil {
			t.Fatalf("line %s was not translated", segment.ID)
		}
	}
}

// ---------------------------------------------------------------------------
// The glossary
// ---------------------------------------------------------------------------

// An enabled entry must reach the prompt verbatim, and a disabled one must not.
func TestPhase5GlossaryReachesThePrompt(t *testing.T) {
	h := newPhase5Harness(t)
	ctx := context.Background()

	projectID := h.projectID
	enabled := &glossary.Entry{
		ProjectID: &projectID,
		Source:    "始めていき",
		Target:    "开始",
		Type:      glossary.TypeTerm,
		Priority:  10,
		Enabled:   true,
	}
	if err := h.glossary.Save(ctx, enabled); err != nil {
		t.Fatalf("save glossary entry: %v", err)
	}

	global := &glossary.Entry{
		Source:   "絶対に現れない語",
		Target:   "不会出现",
		Enabled:  true,
		Priority: 50,
	}
	if err := h.glossary.Save(ctx, global); err != nil {
		t.Fatalf("save global entry: %v", err)
	}

	disabled := &glossary.Entry{
		ProjectID: &projectID,
		Source:    "よろしく",
		Target:    "请多关照",
		Enabled:   false,
	}
	if err := h.glossary.Save(ctx, disabled); err != nil {
		t.Fatalf("save disabled entry: %v", err)
	}

	h.run(false)

	combined := ""
	for _, request := range h.llm.all() {
		combined += request.fullPrompt()
	}

	if !strings.Contains(combined, "始めていき → 开始") {
		t.Errorf("the enabled entry did not reach the prompt:\n%s", combined)
	}
	if strings.Contains(combined, "不会出现") {
		t.Errorf("an entry for a term absent from the lines was sent anyway:\n%s", combined)
	}
	if strings.Contains(combined, "请多关照") {
		t.Errorf("a disabled entry reached the prompt:\n%s", combined)
	}
}

// ---------------------------------------------------------------------------
// The context stage is optional
// ---------------------------------------------------------------------------

// Translation must work when the analysis pass is disabled.
//
// This is the reason context is an optional dependency rather than a required
// one: a project without a configured provider for the analysis, or a user who
// does not want to pay for the extra call, still gets subtitles.
func TestPhase5TranslationRunsWithoutContext(t *testing.T) {
	h := newPhase5Harness(t)

	// "context" is not registered at all, which is what a disabled optional
	// stage looks like to the plan.
	set := h.run(false)

	if set.CountTranslated() != len(set.Segments) {
		t.Fatalf("translated %d of %d lines without a context document",
			set.CountTranslated(), len(set.Segments))
	}

	combined := ""
	for _, request := range h.llm.all() {
		combined += request.fullPrompt()
	}
	if !strings.Contains(combined, "No background information is available") {
		t.Errorf("the prompt did not say that context was unavailable:\n%s", combined)
	}
}

// ---------------------------------------------------------------------------
// The context stage, when it is present
// ---------------------------------------------------------------------------

// analysisContent is a well-formed analysis document.
func analysisContent() string {
	document := map[string]any{
		"summary": "A radio show in which two hosts discuss the week's news.",
		"setting": "A recording studio in Tokyo, present day.",
		"tone":    "Informal and comic.",
		"characters": []map[string]string{
			{"name_ja": "たなか", "name_zh": "田中", "role": "the host"},
			{"name_ja": "すずき", "name_zh": "铃木", "role": "the guest"},
		},
		"terms": []map[string]string{
			{"source": "始めていき", "target": "开场", "type": "term", "note": "the show's opener"},
		},
		"notes": []string{"The hosts speak over each other frequently."},
	}
	encoded, _ := json.Marshal(document)
	return string(encoded)
}

// With the analysis pass enabled, its document must reach the translation
// prompt.
//
// This is the half of the optional-dependency design that the other tests do not
// cover: they all run without a context stage, which is the easy case. Here the
// stage is registered, runs against a real endpoint, and its output has to
// arrive in the translator's prompt — which is also the only place the analysis
// prompt template is rendered at all, so a mismatch between the template's
// variables and the data it is given surfaces here rather than in production.
func TestPhase5ContextDocumentReachesThePrompt(t *testing.T) {
	h := newPhase5Harness(t)
	h.registerContext()

	h.llm.reply = func(_ int, req recordedRequest) (int, string) {
		// The analysis pass is the request that is not asking for a batch of
		// lines to translate.
		if strings.Contains(req.User, "You are preparing to translate") {
			return 0, envelope(analysisContent())
		}
		return 0, ""
	}

	ctx := context.Background()
	opts := h.options(false)

	plan, err := pipeline.Build(ctx, opts)
	if err != nil {
		t.Fatalf("pipeline.Build: %v", err)
	}
	if err := pipeline.Run(ctx, plan, opts, pipeline.NopObserver{}); err != nil {
		t.Fatalf("pipeline.Run: %v", err)
	}

	states := map[string]pipeline.State{}
	for _, sp := range plan.Stages {
		states[sp.Stage.Spec().Name] = sp.State
	}
	for _, name := range []string{"segmentation", "context", "translation"} {
		if states[name] != pipeline.StateCompleted {
			t.Fatalf("stage %s ended in state %s, want completed", name, states[name])
		}
	}

	var combined string
	for _, request := range h.llm.all() {
		combined += request.fullPrompt()
	}

	// The document must arrive rendered, not as raw JSON.
	for _, want := range []string{
		"A radio show in which two hosts discuss the week's news.",
		"铃木", // the character names were rendered, not passed through as JSON
		"the show's opener",
		"conversation",
	} {
		if !strings.Contains(combined, want) {
			t.Errorf("the translation prompt does not contain %q:\n%s", want, combined)
		}
	}

	// And the analysis request must have carried a sample of the transcript.
	var sawTranscript bool
	for _, request := range h.llm.all() {
		if strings.Contains(request.User, "seg_0001") &&
			strings.Contains(request.User, "こんにちは") {
			sawTranscript = true
		}
	}
	if !sawTranscript {
		t.Error("the analysis request did not contain the transcript")
	}
}

// isAnalysisRequest distinguishes the analysis pass from a translation batch.
//
// There is no marker for it in the request: both go to the same endpoint, and
// both ask for JSON. What tells them apart is the instruction, which is the
// whole reason the analysis stage exists as a separate prompt.
func isAnalysisRequest(req recordedRequest) bool {
	return strings.Contains(req.User, "You are preparing to translate")
}

// analysisRequests returns the analysis pass's requests, in order.
func analysisRequests(h *phase5Harness) []recordedRequest {
	var out []recordedRequest
	for _, request := range h.llm.all() {
		if isAnalysisRequest(request) {
			out = append(out, request)
		}
	}
	return out
}

// runPipeline executes the pipeline and reports how each stage ended.
//
// Unlike run, it does not fail the test when a stage fails: the tests below are
// about a stage failing on purpose, and a helper that called t.Fatal on the way
// past would make them untestable.
func runPipeline(t *testing.T, h *phase5Harness) (map[string]pipeline.State, error) {
	t.Helper()

	ctx := context.Background()
	opts := h.options(false)

	plan, err := pipeline.Build(ctx, opts)
	if err != nil {
		t.Fatalf("pipeline.Build: %v", err)
	}

	runErr := pipeline.Run(ctx, plan, opts, pipeline.NopObserver{})

	states := map[string]pipeline.State{}
	for _, sp := range plan.Stages {
		states[sp.Stage.Spec().Name] = sp.State
	}
	return states, runErr
}

// An analysis reply that cannot be read is asked for again, with the parser's
// complaint attached.
//
// This is a measured failure, not a hypothetical one: a model closed an array
// with a brace — `"terms": [{…} }` — which survives the brace matcher and is
// rejected by the decoder. Nothing about that reply is recoverable, so there is
// no partial document to salvage; what makes it worth retrying rather than
// aborting is that the run it interrupts has already paid for transcription and
// segmentation, and the model fixes the mistake when it is told about it.
func TestPhase5RetriesAnUnreadableAnalysis(t *testing.T) {
	h := newPhase5Harness(t)
	h.registerContext()

	h.llm.reply = func(attempt int, req recordedRequest) (int, string) {
		if !isAnalysisRequest(req) {
			return 0, ""
		}
		if attempt > 1 {
			return 0, envelope(analysisContent())
		}
		return 0, envelope(`{"summary":"x","terms":[{"source":"a","target":"b"} }`)
	}

	states, err := runPipeline(t, h)
	if err != nil {
		t.Fatalf("pipeline.Run: %v", err)
	}
	if states["context"] != pipeline.StateCompleted {
		t.Fatalf("context ended in state %s, want completed", states["context"])
	}

	requests := analysisRequests(h)
	if len(requests) != 2 {
		t.Fatalf("the analysis was requested %d times, want 2", len(requests))
	}

	// The retry has to be a *different* request. Sending the same prompt again
	// and hoping is not a retry, it is a coin flip.
	retry := requests[1].User

	if !strings.Contains(retry, "## Correction") {
		t.Error("the retry carried no correction")
	}
	// The parser's own words, so the model is told which bracket was wrong
	// rather than asked again to be careful.
	if !strings.Contains(retry, "after array element") {
		t.Errorf("the correction does not say what the parser objected to:\n%s", retry)
	}
	// Built from the original prompt rather than from the attempt before it, so
	// the transcript survives the correction.
	if !strings.Contains(retry, "こんにちは") {
		t.Error("the retry lost the transcript")
	}
	if strings.Count(retry, "## Correction") != 1 {
		t.Error("the retry carries more than one correction")
	}
}

// A model that cannot produce a readable analysis is given up on, and what it
// said is written down.
//
// Two failures are being guarded against, and the second is the one that
// actually happened. Retrying forever would spend a run on a provider that is
// not going to answer. Reporting only the parser's error — "not an analysis
// document: invalid character '}' after array element" — leaves the reply
// itself unreadable, which makes a model's typo indistinguishable from a bug in
// the parser, and it took reading the model's own output to tell them apart.
func TestPhase5GivesUpOnAnUnreadableAnalysis(t *testing.T) {
	h := newPhase5Harness(t)
	h.registerContext()

	// The stage logs at warn, and the harness's logger is configured to drop
	// anything below error — which is right for every other test in this file
	// and wrong for this one.
	var logs strings.Builder
	h.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

	h.llm.reply = func(_ int, req recordedRequest) (int, string) {
		if !isAnalysisRequest(req) {
			return 0, ""
		}
		return 0, envelope(`{"summary":"x","terms":[{"source":"a"} }`)
	}

	states, _ := runPipeline(t, h)

	if states["context"] != pipeline.StateFailed {
		t.Errorf("context ended in state %s, want failed", states["context"])
	}

	// And the rest of the pipeline ran anyway. The analysis is an optional
	// dependency of translation, so a failure here costs the translation its
	// context and nothing else — which is the difference between a run that
	// produces subtitles without the analysis and a run that produces nothing.
	// This is not incidental: it is what the stage's Optional flag and the
	// translation prompt's "no context was available" path are for.
	if states["translation"] != pipeline.StateCompleted && states["translation"] != pipeline.StateCached {
		t.Errorf("translation ended in state %s, want completed or cached", states["translation"])
	}

	// Bounded. The exact number is a judgement about how many attempts a typo
	// is worth; that there is one at all is the property under test.
	if got := len(analysisRequests(h)); got != 3 {
		t.Errorf("the analysis was requested %d times, want 3 and no more", got)
	}

	// The reply made it to the log, which is the only place it can be read.
	if !strings.Contains(logs.String(), "source") {
		t.Errorf("the unusable reply was not logged:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "could not be read") {
		t.Errorf("the failure was not logged:\n%s", logs.String())
	}

	// And the stage failing is itself in the log, rather than only in the
	// plan's state. A log that ends where the stage went wrong reads as a run
	// that stopped mid-sentence, and the reason is exactly what it is opened to
	// find.
	if !strings.Contains(logs.String(), "stage failed") {
		t.Errorf("the stage failure was not logged:\n%s", logs.String())
	}
}

// A single-endpoint setup must work without a database record.
//
// The configuration documents base_url and model as a fallback for exactly this
// case, and a documented setting that does nothing is worse than one that is
// absent: the user has no way to tell whether they configured it wrong or
// whether it was never read.
func TestPhase5InlineProviderConfiguration(t *testing.T) {
	h := newPhase5Harness(t)

	if _, err := h.db.Write.ExecContext(context.Background(),
		`DELETE FROM providers`); err != nil {
		t.Fatalf("remove providers: %v", err)
	}

	h.cfg.Translation.BaseURL = h.llm.server.URL
	h.cfg.Translation.APIKey = "inline-key"
	h.cfg.Translation.Model = "inline-model"

	set := h.run(false)

	if set.CountTranslated() != len(set.Segments) {
		t.Fatalf("translated %d of %d lines using the inline endpoint",
			set.CountTranslated(), len(set.Segments))
	}
	if h.llm.last().Model != "inline-model" {
		t.Errorf("model = %q, want the configured inline-model", h.llm.last().Model)
	}
}

// Naming a provider in configuration must select that one, not merely require
// that some provider exists.
func TestPhase5NamedProviderIsUsed(t *testing.T) {
	h := newPhase5Harness(t)
	ctx := context.Background()

	// A second enabled provider, so the choice is genuinely ambiguous unless the
	// configured name is honoured.
	other := newFakeLLM(t)
	if err := provider.NewService(h.db).Save(ctx, &provider.Provider{
		Name:    "other",
		Kind:    provider.KindLLM,
		Type:    provider.TypeOpenAICompatible,
		BaseURL: other.server.URL,
		Model:   "other-model",
		Enabled: true,
	}); err != nil {
		t.Fatalf("seed second provider: %v", err)
	}

	h.cfg.Translation.Provider = "fake"
	set := h.run(false)

	if set.CountTranslated() != len(set.Segments) {
		t.Fatalf("translated %d of %d lines", set.CountTranslated(), len(set.Segments))
	}
	if other.requestCount() != 0 {
		t.Errorf("the unnamed provider was used %d time(s); the configured name was ignored",
			other.requestCount())
	}
	if h.llm.requestCount() == 0 {
		t.Error("the configured provider was never called")
	}
}

// ---------------------------------------------------------------------------
// Timing
// ---------------------------------------------------------------------------

// A line that is too fast to read must be given more time when there is silence
// to borrow, and flagged when there is not.
func TestPhase5ReflowEnforcesMaxCPS(t *testing.T) {
	translated := func(text string) *string { return &text }

	t.Run("borrows the silence that follows", func(t *testing.T) {
		segments := []*subtitle.Segment{
			{
				ID: "seg_0001", Start: 0, End: 1,
				SourceText: "短い", TranslatedText: translated("这是一句非常长的话需要更多的时间来阅读"),
			},
			{
				ID: "seg_0002", Start: 5, End: 7,
				SourceText: "次の行", TranslatedText: translated("下一行"),
			},
		}

		report := subtitle.Reflow(segments, subtitle.ReflowOptions{
			MaxCPS: 9, MaxDuration: 7, MinGap: 0.083,
		})

		if report.Extended != 1 {
			t.Fatalf("extended %d lines, want 1", report.Extended)
		}
		if segments[0].CPS == nil || *segments[0].CPS > 9 {
			t.Errorf("line 1 is still at %v CPS", segments[0].CPS)
		}
		if segments[0].End >= segments[1].Start {
			t.Errorf("line 1 ends at %v, at or past line 2 which starts at %v",
				segments[0].End, segments[1].Start)
		}
		if len(report.Unresolved) != 0 {
			t.Errorf("flagged %v, but the line fits after extending", report.Unresolved)
		}
	})

	t.Run("flags a line that cannot be fixed by timing", func(t *testing.T) {
		// Speech with no silence in it. Every line is pressed against its
		// neighbours, so there is nothing to borrow and the correction has to
		// report the line rather than quietly accept it.
		segments := []*subtitle.Segment{
			{
				ID: "seg_0001", Start: 0, End: 1,
				SourceText:     "長い",
				TranslatedText: translated("这是一句非常长的话无论给多少时间都没有办法读完它"),
			},
			{
				ID: "seg_0002", Start: 1, End: 3,
				SourceText:     "次",
				TranslatedText: translated("下一句"),
			},
			{
				ID: "seg_0003", Start: 3, End: 5,
				SourceText:     "最後",
				TranslatedText: translated("最后一句"),
			},
		}

		report := subtitle.Reflow(segments, subtitle.ReflowOptions{
			MaxCPS: 9, MaxDuration: 7, MinGap: 0.083,
		})

		if len(report.Unresolved) != 1 || report.Unresolved[0] != "seg_0001" {
			t.Fatalf("unresolved = %v, want [seg_0001]", report.Unresolved)
		}
		if !segments[0].NeedsReview {
			t.Error("an unreadable line was not marked for review")
		}
		if !hasTag(segments[0], subtitle.TagCPSExceeded) {
			t.Errorf("the line carries tags %v, want %s",
				segments[0].Tags, subtitle.TagCPSExceeded)
		}

		// The correction must not have run the line over its neighbour.
		if segments[0].End > segments[1].Start {
			t.Errorf("line 1 ends at %v, past line 2 at %v", segments[0].End, segments[1].Start)
		}
	})
}

func hasTag(segment *subtitle.Segment, tag string) bool {
	for _, existing := range segment.Tags {
		if existing == tag {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Request shape
// ---------------------------------------------------------------------------

// The request must be a batched, JSON-mode, authenticated call — the three
// things a provider silently ignores rather than rejecting.
func TestPhase5RequestsAreWellFormed(t *testing.T) {
	h := newPhase5Harness(t)
	h.run(false)

	request := h.llm.last()

	if !request.JSONMode {
		t.Error("the request did not ask for a JSON response")
	}
	if request.Model != "fake-model" {
		t.Errorf("model = %q, want fake-model", request.Model)
	}

	lineCount := len(request.requestedLines())
	if lineCount < 2 {
		t.Errorf("only %d line(s) were sent in one request; batching is not happening", lineCount)
	}
	if lineCount > h.cfg.Translation.BatchSize {
		t.Errorf("%d lines were sent, over the configured batch size of %d",
			lineCount, h.cfg.Translation.BatchSize)
	}
}

// The cache key must cover the things that change a translation. A field left
// out is a way for a translation produced under one set of conditions to be
// served under another.
func TestPhase5CacheKeyCoversItsInputs(t *testing.T) {
	base := translationcore.KeyInputs{
		Source:        "こんにちは",
		Style:         "fansub",
		Provider:      "fake",
		Model:         "fake-model",
		PromptVersion: "abc",
		ContextHash:   "ctx",
		GlossaryHash:  "gls",
	}

	baseline, err := translationcore.DeriveKey(base)
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}

	variations := map[string]func(*translationcore.KeyInputs){
		"source":         func(k *translationcore.KeyInputs) { k.Source = "こんばんは" },
		"style":          func(k *translationcore.KeyInputs) { k.Style = "literal" },
		"provider":       func(k *translationcore.KeyInputs) { k.Provider = "other" },
		"model":          func(k *translationcore.KeyInputs) { k.Model = "other-model" },
		"prompt version": func(k *translationcore.KeyInputs) { k.PromptVersion = "def" },
		"context":        func(k *translationcore.KeyInputs) { k.ContextHash = "other" },
		"glossary":       func(k *translationcore.KeyInputs) { k.GlossaryHash = "other" },
	}

	for name, mutate := range variations {
		changed := base
		mutate(&changed)

		key, err := translationcore.DeriveKey(changed)
		if err != nil {
			t.Fatalf("DeriveKey(%s): %v", name, err)
		}
		if key == baseline {
			t.Errorf("changing the %s did not change the cache key", name)
		}
	}

	// And the key must be stable, or nothing ever hits.
	again, err := translationcore.DeriveKey(base)
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	if again != baseline {
		t.Errorf("the key is not stable: %s then %s", baseline, again)
	}
}

// A batch the model will not complete must not fail the whole job, and the line
// it never answered must be flagged rather than silently left in Japanese.
func TestPhase5UntranslatableLineIsFlagged(t *testing.T) {
	h := newPhase5Harness(t)

	h.llm.reply = func(int, recordedRequest) (int, string) {
		// Always incomplete.
		return 0, translationReply(map[string]string{})
	}

	set := h.run(false)

	if set.CountTranslated() != 0 {
		t.Fatalf("translated %d lines from replies that never contained any", set.CountTranslated())
	}
	for _, segment := range set.Segments {
		if !segment.NeedsReview {
			t.Errorf("line %s was not flagged for review", segment.ID)
		}
		if !hasTag(segment, subtitle.TagTranslationFailed) {
			t.Errorf("line %s carries tags %v, want %s",
				segment.ID, segment.Tags, subtitle.TagTranslationFailed)
		}
	}
}

// A provider that is not configured must produce an error naming the problem,
// not a panic or a silent empty result.
func TestPhase5MissingProviderIsReported(t *testing.T) {
	h := newPhase5Harness(t)

	if _, err := h.db.Write.ExecContext(context.Background(),
		`DELETE FROM providers`); err != nil {
		t.Fatalf("remove providers: %v", err)
	}

	ctx := context.Background()
	opts := h.options(true)

	plan, err := pipeline.Build(ctx, opts)
	if err != nil {
		t.Fatalf("pipeline.Build: %v", err)
	}
	// A stage failure is recorded on the plan rather than returned from Run,
	// because one stage failing does not abandon the others.
	if err := pipeline.Run(ctx, plan, opts, pipeline.NopObserver{}); err != nil {
		t.Fatalf("pipeline.Run: %v", err)
	}

	// The failure has to be readable. "no enabled llm provider is configured"
	// tells the user what to do; "stage failed" does not.
	var found bool
	for _, sp := range plan.Stages {
		if sp.Stage.Spec().Name != "translation" {
			continue
		}
		if sp.State != pipeline.StateFailed {
			t.Fatalf("translation ended in state %s with no provider, want failed", sp.State)
		}
		found = true
		if sp.Err == nil {
			t.Fatal("the stage failed without recording an error")
		}
		if !strings.Contains(sp.Err.Error(), "provider") {
			t.Errorf("the error does not name the cause: %v", sp.Err)
		}
	}
	if !found {
		t.Error("the plan contained no translation stage")
	}
}

// The translation cache must stay within its configured size.
//
// It grows with every line every user translates, and nothing else ever removes
// from it — so without a trim the table is unbounded on an install that runs
// for long enough, and the setting that claims to bound it does nothing.
func TestPhase5CacheIsPrunedToItsLimit(t *testing.T) {
	h := newPhase5Harness(t)
	ctx := context.Background()

	cache := translationcore.NewCache(h.db)

	entries := make([]translationcore.CacheEntry, 0, 60)
	for i := 0; i < 60; i++ {
		entries = append(entries, translationcore.CacheEntry{
			Key:        "trc_" + strings.Repeat("0", 28) + pad(i),
			SourceText: "line " + pad(i),
			Translated: "译文",
			Provider:   "fake", Model: "fake-model",
			PromptVers: "v1", Style: "fansub",
		})
	}
	if err := cache.Put(ctx, entries); err != nil {
		t.Fatalf("store translations: %v", err)
	}

	total, _, err := cache.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total != 60 {
		t.Fatalf("stored %d translations, want 60", total)
	}

	// Nothing to do when the limit is above the size.
	if removed, err := cache.Prune(ctx, 1000); err != nil || removed != 0 {
		t.Errorf("pruning below the size removed %d (err: %v)", removed, err)
	}

	removed, err := cache.Prune(ctx, 10)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed != 50 {
		t.Errorf("removed %d entries, want 50", removed)
	}

	after, _, err := cache.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after != 10 {
		t.Errorf("the cache holds %d entries after pruning to 10", after)
	}

	// Zero means no ceiling, which is what the configuration documents.
	if removed, err := cache.Prune(ctx, 0); err != nil || removed != 0 {
		t.Errorf("a zero limit removed %d entries (err: %v)", removed, err)
	}
}

// pad renders a small integer as a fixed-width decimal.
func pad(n int) string {
	return strconv.Itoa(1000 + n)[1:]
}
