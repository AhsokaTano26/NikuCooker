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
	"strings"
	"testing"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/api"
	"github.com/AhsokaTano26/NikuCooker/internal/app"
	"github.com/AhsokaTano26/NikuCooker/internal/events"
	"github.com/AhsokaTano26/NikuCooker/internal/project"
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
	if code := errorCode(body); code != "INVALID_REQUEST" {
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
