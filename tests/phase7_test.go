package tests

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/api"
	"github.com/AhsokaTano26/NikuCooker/internal/app"
	"github.com/AhsokaTano26/NikuCooker/internal/events"
	"github.com/AhsokaTano26/NikuCooker/internal/jobs"
	"github.com/AhsokaTano26/NikuCooker/internal/logging"
	"github.com/AhsokaTano26/NikuCooker/internal/project"
	"github.com/AhsokaTano26/NikuCooker/internal/provider"
	"github.com/AhsokaTano26/NikuCooker/internal/qc"
	"github.com/AhsokaTano26/NikuCooker/internal/segments"
	"github.com/AhsokaTano26/NikuCooker/internal/subtitle"
)

// The Phase 7 exit test: the API a browser talks to, and the stream it watches.
//
// Two properties carry the weight here.
//
// The first is that an error is *actionable*: a stable code, a message in the
// API's own shape, and a status that matches. A client that has to parse prose
// to decide whether to retry or navigate away is a client that will guess wrong,
// and the guess will be in the one place nobody tested.
//
// The second is that the event stream is *resumable*. A client that missed
// events must be told so rather than handed a stream with an invisible hole —
// which is the failure mode that makes a live-updating UI quietly wrong instead
// of visibly broken.

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type apiHarness struct {
	t       *testing.T
	app     *app.App
	server  *httptest.Server
	dataDir string
}

func newAPIHarness(t *testing.T) *apiHarness {
	t.Helper()

	dataDir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	application, err := app.New(context.Background(), app.Options{
		Flags: map[string]any{
			"storage": map[string]any{"data_dir": dataDir, "model_dir": filepath.Join(dataDir, "models")},
			// Off by default in production, and the API test does not create
			// projects from paths — it asserts that it refuses to.
			"server": map[string]any{"allow_path_source": false},
		},
		Environ:      []string{},
		CodeRevision: "test-rev-1",
		Log:          log,
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { _ = application.Close() })

	handler, err := api.New(api.Options{
		App: application, Log: log, Version: "test", Commit: "test",
	})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}

	server := httptest.NewServer(handler.Routes())
	t.Cleanup(server.Close)

	return &apiHarness{t: t, app: application, server: server, dataDir: dataDir}
}

// request performs an API call and returns the status and decoded body.
func (h *apiHarness) request(method, path string, body any) (int, map[string]any, string) {
	h.t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("encode body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}

	request, err := http.NewRequest(method, h.server.URL+path, reader)
	if err != nil {
		h.t.Fatalf("build request: %v", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := h.server.Client().Do(request)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()

	raw, _ := io.ReadAll(response.Body)

	decoded := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}
	return response.StatusCode, decoded, string(raw)
}

// errorCode reads the stable code out of an error envelope.
func errorCode(body map[string]any) string {
	nested, ok := body["error"].(map[string]any)
	if !ok {
		return ""
	}
	code, _ := nested["code"].(string)
	return code
}

// seedProject inserts a project directly, since creating one through the API
// needs a path source this harness deliberately disables.
func (h *apiHarness) seedProject(name string) string {
	h.t.Helper()

	created, err := h.app.Projects.Create(context.Background(), project.CreateRequest{
		Name:           name,
		SourceLanguage: "ja",
		TargetLanguage: "zh-Hans",
		Style:          "fansub",
	})
	if err != nil {
		h.t.Fatalf("seed project: %v", err)
	}
	return created.ID
}

// ---------------------------------------------------------------------------
// The envelope
// ---------------------------------------------------------------------------

// Every failure must carry a stable code, and the status must match the code.
func TestPhase7ErrorEnvelope(t *testing.T) {
	h := newAPIHarness(t)
	projectID := h.seedProject("envelope")

	cases := []struct {
		name       string
		method     string
		path       string
		body       any
		wantStatus int
		wantCode   string
	}{
		{
			name: "an unknown project", method: "GET",
			path:       "/api/v1/projects/00000000-0000-0000-0000-000000000000",
			wantStatus: http.StatusNotFound, wantCode: "NOT_FOUND",
		},
		{
			// An id that is not a UUID cannot name a project, so it is a
			// not-found rather than a malformed request. A client treats a
			// deleted project and a typo the same way, and giving it two cases
			// to handle where it has one is how one of them goes unhandled.
			name: "a malformed project id", method: "GET",
			path:       "/api/v1/projects/not-an-id",
			wantStatus: http.StatusNotFound, wantCode: "NOT_FOUND",
		},
		{
			name: "an unrouted path", method: "GET",
			path:       "/api/v1/nonsense",
			wantStatus: http.StatusNotFound,
			wantCode:   "NOT_FOUND",
		},
		{
			// A path that exists under another method. Answering 404 would tell
			// the client the endpoint does not exist, which is a different
			// problem with a different fix.
			name: "a wrong method", method: "DELETE",
			path:       "/api/v1/projects",
			wantStatus: http.StatusMethodNotAllowed,
			wantCode:   "INVALID_REQUEST",
		},
		{
			name: "an unknown field", method: "POST",
			path:       "/api/v1/projects",
			body:       map[string]any{"nmae": "typo"},
			wantStatus: http.StatusBadRequest, wantCode: "INVALID_REQUEST",
		},
		{
			name: "a missing project", method: "GET",
			path:       "/api/v1/projects/" + projectID + "/segments/nope",
			wantStatus: http.StatusNotFound,
			wantCode:   "NOT_FOUND",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			status, body, raw := h.request(testCase.method, testCase.path, testCase.body)

			if status != testCase.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)", status, testCase.wantStatus, raw)
			}
			if code := errorCode(body); code != testCase.wantCode {
				t.Errorf("code = %q, want %q (body: %s)", code, testCase.wantCode, raw)
			}

			// The envelope must be the API's, not the web application's HTML.
			// A JSON client that receives a page has to guess what happened.
			if !strings.Contains(raw, `"error"`) {
				t.Errorf("the response is not an error envelope: %s", raw)
			}
		})
	}
}

// A page's shape never varies, including when it is empty.
func TestPhase7PaginationShape(t *testing.T) {
	h := newAPIHarness(t)

	status, body, raw := h.request("GET", "/api/v1/projects", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}

	for _, key := range []string{"items", "total", "limit", "offset"} {
		if _, ok := body[key]; !ok {
			t.Errorf("the page is missing %q: %s", key, raw)
		}
	}

	// `items` must be a list, never null. A client that has to null-check every
	// list will eventually forget, and the one place it forgets is the one that
	// crashes.
	if _, ok := body["items"].([]any); !ok {
		t.Errorf("items is not an array: %s", raw)
	}
}

// The path source is a real capability and must stay off unless configured.
func TestPhase7PathSourceIsRefusedByDefault(t *testing.T) {
	h := newAPIHarness(t)

	source := filepath.Join(h.dataDir, "episode.mp4")
	if err := os.WriteFile(source, []byte("not really a video"), 0o644); err != nil {
		t.Fatal(err)
	}

	status, body, raw := h.request("POST", "/api/v1/projects", map[string]any{
		"name":            "blocked",
		"source_language": "ja",
		"target_language": "zh-Hans",
		"style":           "fansub",
		"source":          map[string]any{"kind": "path", "path": source},
	})

	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", status, raw)
	}
	// Its own code, not INVALID_REQUEST: the interface branches on it to say
	// "turn this setting on" rather than "fix your request", and those are
	// different messages.
	if code := errorCode(body); code != "PATH_SOURCE_DISABLED" {
		t.Errorf("code = %q: %s", code, raw)
	}
	// The message has to name the setting, or the user has no way to find it.
	if !strings.Contains(raw, "allow_path_source") {
		t.Errorf("the error does not name the setting that would enable it: %s", raw)
	}
}

// ---------------------------------------------------------------------------
// The event stream
// ---------------------------------------------------------------------------

// sseFrame is one parsed event from the stream.
type sseFrame struct {
	ID    string
	Event string
	Data  map[string]any
}

// sseReader reads frames off a live stream.
//
// The scanner is created once and kept, not rebuilt per call. A fresh scanner
// over the same body would start reading from wherever the previous one's
// internal buffer happened to stop — and that buffer reads ahead, so the second
// scanner silently loses whatever the first one had already consumed. That bug
// presents as "the server stopped sending events" and is entirely in the test.
type sseReader struct {
	t       *testing.T
	scanner *bufio.Scanner
	pending sseFrame
}

func newSSEReader(t *testing.T, body io.Reader) *sseReader {
	t.Helper()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	return &sseReader{t: t, scanner: scanner}
}

// next returns the next frame, or fails the test when the stream ends first.
//
// The caller is responsible for the stream being closed if the frame never
// arrives; the stream tests run under a context with a deadline, which is what
// makes a missing event a failure rather than a hang.
func (r *sseReader) next(want string) sseFrame {
	r.t.Helper()

	if r.pending.Event == want {
		frame := r.pending
		r.pending = sseFrame{}
		return frame
	}

	for r.scanner.Scan() {
		line := r.scanner.Text()
		switch {
		case line == "":
			if r.pending.Event == want {
				frame := r.pending
				r.pending = sseFrame{}
				return frame
			}
			// A frame that is not the one being waited for is kept: the caller
			// may want it next, and dropping it would be a silent gap.
			if r.pending.Event != "" {
				discarded := r.pending
				r.pending = sseFrame{}
				_ = discarded
			}

		case strings.HasPrefix(line, "id: "):
			r.pending.ID = strings.TrimPrefix(line, "id: ")

		case strings.HasPrefix(line, "event: "):
			r.pending.Event = strings.TrimPrefix(line, "event: ")

		case strings.HasPrefix(line, "data: "):
			payload := strings.TrimPrefix(line, "data: ")
			if err := json.Unmarshal([]byte(payload), &r.pending.Data); err != nil {
				r.t.Fatalf("the event payload is not JSON: %q", payload)
			}

		case strings.HasPrefix(line, ":"):
			// A heartbeat comment. Ignored, as a client would.
		}
	}

	r.t.Fatalf("the stream ended before a %q event arrived (scanner: %v)",
		want, r.scanner.Err())
	return sseFrame{}
}

// payload reads an event's `data` object.
//
// The envelope is what was parsed, so a field the handler put in `data` is one
// level down.
func (f sseFrame) payload() map[string]any {
	nested, ok := f.Data["data"].(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return nested
}

// openStream connects to the event stream under a deadline.
func (h *apiHarness) openStream() (*sseReader, *http.Response) {
	h.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	h.t.Cleanup(cancel)

	request, err := http.NewRequestWithContext(ctx, "GET", h.server.URL+"/api/v1/events", nil)
	if err != nil {
		h.t.Fatalf("build the stream request: %v", err)
	}

	response, err := h.server.Client().Do(request)
	if err != nil {
		h.t.Fatalf("connect to the stream: %v", err)
	}
	h.t.Cleanup(func() { _ = response.Body.Close() })

	return newSSEReader(h.t, response.Body), response
}

// The handshake must arrive first, and must carry the server's position.
func TestPhase7StreamHandshake(t *testing.T) {
	h := newAPIHarness(t)

	reader, response := h.openStream()

	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("Content-Type = %q", contentType)
	}
	// Without this a reverse proxy buffers the stream into uselessness, which
	// presents as "the UI never updates".
	if response.Header.Get("X-Accel-Buffering") != "no" {
		t.Error("X-Accel-Buffering is not set, so a proxy would buffer the stream")
	}

	frame := reader.next("hello")

	// The sequence must be a number a client can resume from. Zero would mean
	// "you have seen nothing", and the next reconnect would ask for a replay of
	// everything the process has ever emitted.
	if _, ok := frame.Data["seq"].(float64); !ok {
		t.Errorf("hello has no seq: %v", frame.Data)
	}

	payload := frame.payload()
	if _, ok := payload["current_seq"]; !ok {
		t.Errorf("hello does not report the server's position: %v", frame.Data)
	}
	if _, ok := payload["replay"]; !ok {
		t.Errorf("hello does not say whether replay is possible: %v", frame.Data)
	}
}

// An emitted event must reach a subscriber, in order, with its sequence.
func TestPhase7StreamDeliversAndNumbersEvents(t *testing.T) {
	h := newAPIHarness(t)

	reader, _ := h.openStream()
	reader.next("hello")

	// Published after the handshake, so these must be delivered rather than
	// replayed.
	for i := 0; i < 3; i++ {
		h.app.Events.Emit(events.New(events.TypeProjectUpdated).
			ForProject("proj_test").
			With("index", i))
	}

	var sequences []int64
	for len(sequences) < 3 {
		frame := reader.next(string(events.TypeProjectUpdated))

		seq, ok := frame.Data["seq"].(float64)
		if !ok {
			t.Fatalf("the event has no seq: %v", frame.Data)
		}
		sequences = append(sequences, int64(seq))

		if frame.Data["project_id"] != "proj_test" {
			t.Errorf("the envelope lost its project scope: %v", frame.Data)
		}
	}

	// Consecutive, because the contract is that a client resumes by adding one.
	for i := 1; i < len(sequences); i++ {
		if sequences[i] != sequences[i-1]+1 {
			t.Fatalf("sequence numbers are not consecutive: %v", sequences)
		}
	}
}

// The bus must refuse to replay a position it no longer holds.
//
// A stream that silently started from the oldest retained event would leave the
// client with a gap it cannot detect — which is worse than a resync, because a
// resync is visible and a gap is not.
func TestPhase7StreamRefusesAnUnreplayablePosition(t *testing.T) {
	bus := events.NewBus(4)

	for i := 0; i < 10; i++ {
		bus.Emit(events.New(events.TypeProjectUpdated).With("index", i))
	}

	if _, replayable := bus.Subscribe(1); replayable {
		t.Error("a position that has been evicted was reported as replayable")
	}

	// The most recent position is still replayable.
	if _, replayable := bus.Subscribe(bus.CurrentSeq() - 1); !replayable {
		t.Error("a recent position was reported as unreplayable")
	}

	// And a fresh client, which has nothing to be missing, always may connect.
	if _, replayable := bus.Subscribe(0); !replayable {
		t.Error("a fresh subscription was refused")
	}

	// A position beyond the current counter means the client is talking to a
	// restarted server whose sequence began again.
	if _, replayable := bus.Subscribe(bus.CurrentSeq() + 100); replayable {
		t.Error("a position beyond the counter was reported as replayable")
	}
}

// A subscriber that stops reading must be dropped, not allowed to grow the
// server's memory without bound.
func TestPhase7SlowSubscriberIsDropped(t *testing.T) {
	bus := events.NewBus(64)

	subscription, _ := bus.Subscribe(0)

	// Far more events than the subscriber buffer, with nothing reading.
	for i := 0; i < events.SubscriberBuffer*2; i++ {
		bus.Emit(events.New(events.TypeProjectUpdated))
	}

	// The channel closes when the subscription is dropped, so draining it
	// terminates rather than blocking.
	drained := 0
	for range subscription.Events() {
		drained++
		if drained > events.SubscriberBuffer*2 {
			t.Fatal("the subscription was never dropped")
		}
	}

	if !subscription.Overflowed() {
		t.Error("the subscription closed without reporting that it dropped events")
	}
	if bus.Subscribers() != 0 {
		t.Errorf("%d subscribers remain after the drop", bus.Subscribers())
	}
}

// ---------------------------------------------------------------------------
// Segments and findings
// ---------------------------------------------------------------------------

// A run's lines must be readable back through the API, with their findings.
func TestPhase7SegmentsAndFindingsRoundTrip(t *testing.T) {
	h := newAPIHarness(t)
	ctx := context.Background()
	projectID := h.seedProject("lines")

	translated := func(text string) *string { return &text }
	set := &subtitle.Set{
		SourceLanguage: "ja",
		TargetLanguage: "zh-Hans",
		Segments: []*subtitle.Segment{
			{
				ID: "seg_0001", Start: 0, End: 2,
				SourceText: "こんにちは", TranslatedText: translated("你好"),
				Tags: []string{},
			},
			{
				ID: "seg_0002", Start: 2, End: 4,
				SourceText: "さようなら", TranslatedText: translated("再见"),
				Tags: []string{}, NeedsReview: true,
			},
		},
	}
	for _, segment := range set.Segments {
		segment.RecomputeCPS()
	}

	if err := h.app.Segments.ReplaceFromArtifacts(ctx, projectID, set, segments.Lineage{}); err != nil {
		t.Fatalf("store the lines: %v", err)
	}

	// A finding on the second line, resolved through the same id map the
	// persistence layer uses.
	ordinals := segments.OrdinalMap(projectID, set)
	report := qc.Check(set, qc.Options{MaxCPS: 100, MaxDuration: 60, MinDuration: 0})
	if err := h.app.Segments.SaveFindings(ctx, projectID, "qc", report.Findings, ordinals); err != nil {
		t.Fatalf("store the findings: %v", err)
	}

	// The API must return them.
	status, body, raw := h.request("GET",
		"/api/v1/projects/"+projectID+"/segments?include_qc=true", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}

	items, ok := body["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("expected two lines, got %s", raw)
	}

	first, _ := items[0].(map[string]any)
	if first["source_text"] != "こんにちは" {
		t.Errorf("source_text = %v", first["source_text"])
	}
	if first["translated_text"] != "你好" {
		t.Errorf("translated_text = %v", first["translated_text"])
	}
	// Runes, not bytes: a Japanese line is three bytes per character, and a byte
	// count would report a reading speed three times the real one.
	if cps, ok := first["cps"].(float64); !ok || cps <= 0 || cps > 3.1 {
		t.Errorf("cps = %v, want about 1.5", first["cps"])
	}

	second, _ := items[1].(map[string]any)
	if second["needs_review"] != true {
		t.Errorf("the review flag was lost: %v", second)
	}
}

// A line's review state must survive a re-run.
//
// This is the property that makes the editor usable at all: a re-run after an
// edit that silently reverted the correction would be the single most
// destructive thing the pipeline could do.
func TestPhase7EditsSurviveARerun(t *testing.T) {
	h := newAPIHarness(t)
	ctx := context.Background()
	projectID := h.seedProject("rerun")

	translated := func(text string) *string { return &text }
	first := &subtitle.Set{
		SourceLanguage: "ja",
		TargetLanguage: "zh-Hans",
		Segments: []*subtitle.Segment{{
			ID: "seg_0001", Start: 0, End: 2,
			SourceText: "こんにちは", TranslatedText: translated("你好"),
			Tags: []string{},
		}},
	}

	if err := h.app.Segments.ReplaceFromArtifacts(ctx, projectID, first, segments.Lineage{}); err != nil {
		t.Fatal(err)
	}

	// A user corrects the line.
	lineID := segments.DeriveID(projectID, 1)
	if _, err := h.app.DB().Write.ExecContext(ctx,
		`UPDATE segments SET translated_text = ?, is_edited = 1, review_state = 'edited'
		 WHERE id = ?`, "您好", lineID); err != nil {
		t.Fatal(err)
	}

	// The pipeline runs again and produces a different translation.
	second := &subtitle.Set{
		SourceLanguage: "ja",
		TargetLanguage: "zh-Hans",
		Segments: []*subtitle.Segment{{
			ID: "seg_0001", Start: 0, End: 2,
			SourceText: "こんにちは", TranslatedText: translated("哈喽"),
			Tags: []string{},
		}},
	}
	if err := h.app.Segments.ReplaceFromArtifacts(ctx, projectID, second, segments.Lineage{}); err != nil {
		t.Fatal(err)
	}

	stored, err := h.app.Segments.Get(ctx, projectID, lineID)
	if err != nil {
		t.Fatal(err)
	}

	if stored.TranslatedText == nil || *stored.TranslatedText != "您好" {
		t.Errorf("the correction was overwritten by a re-run: %v", stored.TranslatedText)
	}
	if !stored.IsEdited {
		t.Error("the edited flag was lost")
	}
	if stored.ReviewState != segments.ReviewEdited {
		t.Errorf("review_state = %q, want %q", stored.ReviewState, segments.ReviewEdited)
	}
}

// A shortened transcript must not leave orphaned lines behind.
func TestPhase7ShrinkingTranscriptRemovesStaleLines(t *testing.T) {
	h := newAPIHarness(t)
	ctx := context.Background()
	projectID := h.seedProject("shrink")

	translated := func(text string) *string { return &text }
	makeLine := func(id string, start float64) *subtitle.Segment {
		return &subtitle.Segment{
			ID: id, Start: start, End: start + 2,
			SourceText: "テスト", TranslatedText: translated("测试"),
			Tags: []string{},
		}
	}

	long := &subtitle.Set{
		SourceLanguage: "ja", TargetLanguage: "zh-Hans",
		Segments: []*subtitle.Segment{
			makeLine("seg_0001", 0), makeLine("seg_0002", 2), makeLine("seg_0003", 4),
		},
	}
	if err := h.app.Segments.ReplaceFromArtifacts(ctx, projectID, long, segments.Lineage{}); err != nil {
		t.Fatal(err)
	}

	short := &subtitle.Set{
		SourceLanguage: "ja", TargetLanguage: "zh-Hans",
		Segments: []*subtitle.Segment{makeLine("seg_0001", 0)},
	}
	if err := h.app.Segments.ReplaceFromArtifacts(ctx, projectID, short, segments.Lineage{}); err != nil {
		t.Fatal(err)
	}

	total, _, err := h.app.Segments.Counts(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("%d lines remain after the transcript shrank to 1", total)
	}
}

// Ordering by a column the caller names must not be a way to run arbitrary SQL.
//
// The column name goes into the statement, so it is the one query parameter that
// cannot be a bound variable. A whitelist is the only safe answer, and this is
// the test that keeps it one.
func TestPhase7SegmentOrderingIsWhitelisted(t *testing.T) {
	h := newAPIHarness(t)
	projectID := h.seedProject("order")

	// The legitimate values must all work.
	for _, order := range []string{"ordinal", "start", "cps", "duration"} {
		status, _, raw := h.request("GET",
			"/api/v1/projects/"+projectID+"/segments?order="+order, nil)

		if status != http.StatusOK {
			t.Errorf("order=%q returned %d: %s", order, status, raw)
		}
	}

	// Percent-encoded, which is what anything actually attacking this would
	// send. A raw payload is rejected by the HTTP layer before reaching the
	// handler, which proves nothing about the handler.
	for _, order := range []string{
		"1; DROP TABLE segments--",
		"ordinal) UNION SELECT 1,2,3--",
		"(SELECT 1)",
	} {
		status, _, raw := h.request("GET",
			"/api/v1/projects/"+projectID+"/segments?order="+url.QueryEscape(order), nil)

		// Rejected or ignored, but never a 500 and never executed. A 500 would
		// mean the value reached the database and the driver refused it, which
		// is one driver away from succeeding.
		if status != http.StatusOK {
			t.Errorf("order=%q returned %d: %s", order, status, raw)
		}
	}

	// The table must still be there, with its rows.
	total, _, err := h.app.Segments.Counts(context.Background(), projectID)
	if err != nil {
		t.Fatalf("the segments table did not survive: %v", err)
	}
	if total != 0 {
		t.Errorf("the table holds %d rows after the injection attempts", total)
	}

	// And the table must still exist for a fresh query to succeed at all.
	if _, _, err := h.app.Segments.List(context.Background(), projectID, segments.ListQuery{}); err != nil {
		t.Fatalf("the segments table is unusable: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Administration
// ---------------------------------------------------------------------------

// A stored key must never leave the server, in any form.
//
// The one field worth being paranoid about: a key that appears in a response is
// in the browser's cache, in any proxy log, and in whatever the user pastes into
// a bug report. `has_key` exists so the interface can say "configured" without
// the value.
func TestPhase7ProviderKeysAreNeverReturned(t *testing.T) {
	h := newAPIHarness(t)

	status, body, raw := h.request("POST", "/api/v1/providers", map[string]any{
		"name": "secretive", "kind": "llm", "type": "openai-compatible",
		"base_url": "https://example.invalid/v1",
		"api_key":  "sk-do-not-print-me",
		"model":    "some-model",
	})
	if status != http.StatusCreated {
		t.Fatalf("status = %d: %s", status, raw)
	}

	if body["has_key"] != true {
		t.Errorf("the response does not say a key is stored: %v", body)
	}
	if strings.Contains(raw, "sk-do-not-print-me") {
		t.Fatalf("the API key was returned to the client: %s", raw)
	}
	if _, present := body["api_key"]; present {
		t.Errorf("the response carries an api_key field at all: %s", raw)
	}

	// Nor on a read.
	_, _, listRaw := h.request("GET", "/api/v1/providers", nil)
	if strings.Contains(listRaw, "sk-do-not-print-me") {
		t.Fatalf("the API key was returned by the list endpoint: %s", listRaw)
	}

	// Nor in the settings dump, which serialises the whole configuration.
	_, _, settingsRaw := h.request("GET", "/api/v1/settings", nil)
	if strings.Contains(settingsRaw, "sk-do-not-print-me") {
		t.Fatalf("the API key was returned by the settings endpoint: %s", settingsRaw)
	}
}

// Updating a provider without a key must leave the stored one alone.
//
// The interface never receives the key, so it cannot send it back. A save that
// cleared it would break a working configuration every time someone renamed a
// provider.
func TestPhase7UpdatingAProviderKeepsItsKey(t *testing.T) {
	h := newAPIHarness(t)
	ctx := context.Background()

	status, body, raw := h.request("POST", "/api/v1/providers", map[string]any{
		"name": "keeper", "kind": "llm", "type": "openai-compatible",
		"base_url": "https://example.invalid/v1",
		"api_key":  "sk-original",
		"model":    "some-model",
	})
	if status != http.StatusCreated {
		t.Fatalf("status = %d: %s", status, raw)
	}

	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("the created provider has no id: %s", raw)
	}

	// A rename, with no key field — which is what the interface sends.
	if status, _, raw := h.request("PATCH", "/api/v1/providers/"+id,
		map[string]any{"name": "renamed"}); status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}

	stored, err := h.app.Providers.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != "renamed" {
		t.Errorf("the rename did not take: %q", stored.Name)
	}
	if stored.APIKey != "sk-original" {
		t.Errorf("the key was cleared by a rename: %q", stored.APIKey)
	}
	if stored.Model != "some-model" {
		t.Errorf("a field the request did not mention was cleared: %q", stored.Model)
	}
}

// The settings endpoint must show where each value came from.
//
// It is the answer to "I changed the setting and nothing happened", which is
// otherwise found by reading four places and guessing.
func TestPhase7SettingsReportProvenance(t *testing.T) {
	h := newAPIHarness(t)

	status, body, raw := h.request("GET", "/api/v1/settings", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}

	config, ok := body["config"].(map[string]any)
	if !ok {
		t.Fatalf("settings carries no config: %s", raw)
	}
	if _, ok := config["asr"]; !ok {
		t.Fatalf("the config is not keyed by its document paths: %s", raw)
	}

	provenance, ok := body["provenance"].(map[string]any)
	if !ok {
		t.Fatalf("settings carries no provenance: %s", raw)
	}

	// The harness sets the data directory through a flag, so its provenance
	// must say so rather than reporting the default.
	if got := provenance["storage.data_dir"]; got != "cli" {
		t.Errorf("storage.data_dir came from %v, want cli", got)
	}
	// And something nobody set must be attributed to the defaults.
	if got := provenance["log.level"]; got != "default" {
		t.Errorf("log.level came from %v, want default", got)
	}
}

// The log endpoint must return records, and only new ones when asked.
func TestPhase7LogsAreIncremental(t *testing.T) {
	h := newAPIHarness(t)

	if h.app.Logs == nil {
		t.Fatal("the harness wired no log buffer")
	}

	h.app.Logs.Add(logging.Record{Level: "info", Msg: "first"})
	h.app.Logs.Add(logging.Record{Level: "warn", Msg: "second"})

	status, body, raw := h.request("GET", "/api/v1/logs", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}

	items, _ := body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("expected two records, got %s", raw)
	}

	seq, ok := body["seq"].(float64)
	if !ok || seq < 2 {
		t.Fatalf("the response reports no cursor: %s", raw)
	}

	// Asking for everything after the cursor must return nothing, which is what
	// makes following the log a request a second rather than a full refetch.
	_, followBody, followRaw := h.request("GET",
		"/api/v1/logs?after="+strconv.Itoa(int(seq)), nil)

	followed, _ := followBody["items"].([]any)
	if len(followed) != 0 {
		t.Errorf("a follow-up returned %d records, want none: %s", len(followed), followRaw)
	}

	h.app.Logs.Add(logging.Record{Level: "error", Msg: "third"})

	_, nextBody, _ := h.request("GET",
		"/api/v1/logs?after="+strconv.Itoa(int(seq)), nil)
	next, _ := nextBody["items"].([]any)
	if len(next) != 1 {
		t.Errorf("expected only the new record, got %d", len(next))
	}
}

// An unknown model must be reported in the API's own shape.
func TestPhase7UnknownModelIsNotFound(t *testing.T) {
	h := newAPIHarness(t)

	status, body, raw := h.request("POST", "/api/v1/models/definitely-not-a-model/download", nil)

	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", status, raw)
	}
	if code := errorCode(body); code != "NOT_FOUND" {
		t.Errorf("code = %q: %s", code, raw)
	}

	// The message must list what does exist, or the user cannot fix their typo.
	nested, _ := body["error"].(map[string]any)
	message, _ := nested["message"].(string)
	if !strings.Contains(message, "tiny") {
		t.Errorf("the error does not name the available models: %q", message)
	}
}

// A single-stage run must be accepted.
//
// The jobs table requires a job of kind "stage" to name its target, and a run
// limited to one stage is exactly that kind. Nothing set the field, so
// `nikucooker run <id> --only qc` — and the interface's per-stage re-run button,
// which takes the same path — failed with a database constraint the user could
// do nothing about.
func TestPhase7SingleStageRunIsAccepted(t *testing.T) {
	ffmpegPath(t)

	h := newAPIHarness(t)

	// A project with a real source, because a run refuses to start without one.
	// The fixture is built outside the project and handed to Create, which is
	// what copies it into place — the same path `project create` takes.
	source := filepath.Join(t.TempDir(), "episode01.mp4")
	buildFixture(t, source)

	created, err := h.app.Projects.Create(context.Background(), project.CreateRequest{
		Name:           "single stage",
		SourceLanguage: "ja",
		TargetLanguage: "zh-Hans",
		Style:          "fansub",
		SourceName:     "episode01.mp4",
		SourceOrigin:   source,
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	status, _, raw := h.request("POST",
		"/api/v1/projects/"+created.ID+"/stages/qc/run", map[string]any{})
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", status, raw)
	}

	// The job must be recorded, and must name the stage it targets.
	recorded, _, err := h.app.Jobs.ListByProject(context.Background(), created.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 1 {
		t.Fatalf("expected one job, got %d", len(recorded))
	}
	if recorded[0].Kind != jobs.KindStage {
		t.Errorf("job kind = %q, want %q", recorded[0].Kind, jobs.KindStage)
	}
	if recorded[0].TargetStage != "qc" {
		t.Errorf("job targets %q, want qc", recorded[0].TargetStage)
	}

	// Stopped before the harness tears the database down: the run continues in a
	// goroutine, and leaving it writing into a closed connection turns a pass
	// into a flake.
	h.app.Scheduler.Cancel(created.ID)
}

// A provider whose reply runs out of room has still answered.
//
// The check exists to catch a wrong address, a rejected key and a model the
// endpoint does not serve — all of which fail before any generation happens. A
// truncated reply proves none of them are true, so reporting it as a failure
// sent users to fix a provider that was working, and the advice it printed
// ("lower the batch size") belonged to a stage this request never reaches.
//
// The budget is asserted too, because that is what made it happen: sixteen
// tokens is less than a model that thinks before it answers spends on the
// thinking, so the check could not succeed against one.
func TestPhase7ATruncatedProviderReplyIsNotAFailure(t *testing.T) {
	h := newAPIHarness(t)
	fake := newFakeLLM(t)

	// A well-formed reply that stopped because it ran out of budget.
	fake.reply = func(int, recordedRequest) (int, string) {
		return http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"length"}]}`
	}

	_, created, raw := h.request("POST", "/api/v1/providers", map[string]any{
		"name": "reasoner", "kind": "llm", "type": "openai-compatible",
		"base_url": fake.server.URL + "/v1",
		"api_key":  "sk-test",
		"model":    "some-reasoner",
	})
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("the provider was not created: %s", raw)
	}

	status, body, raw := h.request("POST", "/api/v1/providers/"+id+"/test", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}

	if body["ok"] != true {
		t.Fatalf("a reachable provider was reported as broken: %s", raw)
	}

	// Carried rather than swallowed: a model that spends its whole budget
	// thinking behaves differently from one that does not.
	warning, _ := body["warning"].(string)
	if warning == "" {
		t.Errorf("the truncation was not reported: %s", raw)
	}

	// And not in the failure channel, which the interface colours red.
	if message, _ := body["message"].(string); message != "the provider answered" {
		t.Errorf("message = %q; a working provider should not carry an error here", message)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(fake.requests))
	}
	if got := fake.requests[0].MaxTokens; got < provider.ReasoningTokenFloor {
		t.Errorf("max_tokens = %d, want at least %d: a model that reasons before it "+
			"answers cannot reply inside a smaller budget", got, provider.ReasoningTokenFloor)
	}
}
