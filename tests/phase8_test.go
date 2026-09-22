package tests

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/AhsokaTano26/NikuCooker/internal/segments"
	"github.com/AhsokaTano26/NikuCooker/internal/subtitle"
)

// The Phase 8 exit test: the editor and the review queue.
//
// Two properties matter more than the rest.
//
// The first is that editing *means* something. A correction that the next run
// silently overwrites, or that never reaches the exported file, is worse than
// no editor at all — it teaches the user that their work does not count.
//
// The second is that splitting and merging keep the project coherent. Both
// renumber every line after the change, and a half-applied renumber leaves the
// transcript describing something that never existed.

// seedLines writes a set of lines into a project.
func ptr[T any](value T) *T { return &value }

func (h *apiHarness) seedLines(projectID string, set *subtitle.Set) {
	h.t.Helper()

	if err := h.app.Segments.ReplaceFromArtifacts(context.Background(), projectID, set, segments.Lineage{}); err != nil {
		h.t.Fatalf("seed lines: %v", err)
	}
}

// fixtureSet builds a small translated project.
func fixtureSet() *subtitle.Set {
	translated := func(text string) *string { return &text }

	return &subtitle.Set{
		SourceLanguage: "ja",
		TargetLanguage: "zh-Hans",
		Segments: []*subtitle.Segment{
			{
				ID: "seg_0001", Start: 0, End: 4,
				SourceText: "こんにちは世界", TranslatedText: translated("你好世界"),
				Words: []subtitle.WordTimestamp{
					{Start: 0, End: 2, Text: "こんにちは"},
					{Start: 2, End: 4, Text: "世界"},
				},
				Tags: []string{},
			},
			{
				ID: "seg_0002", Start: 5, End: 9,
				SourceText: "さようなら", TranslatedText: translated("再见"),
				Words: []subtitle.WordTimestamp{
					{Start: 5, End: 7, Text: "さようなら"},
					{Start: 7, End: 9, Text: ""},
				},
				Tags: []string{},
			},
			{
				ID: "seg_0003", Start: 10, End: 14,
				SourceText: "おやすみ", TranslatedText: translated("晚安"),
				Words: []subtitle.WordTimestamp{
					{Start: 10, End: 12, Text: "おやすみ"},
					{Start: 12, End: 14, Text: ""},
				},
				Tags: []string{},
			},
		},
	}
}

// A correction must survive the next run.
//
// This is the property the whole editor rests on. Without it the editor is a
// text box whose contents vanish, and a user who loses an hour of work once
// does not open it again.
func TestPhase8EditsSurviveARun(t *testing.T) {
	h := newAPIHarness(t)
	ctx := context.Background()
	projectID := h.seedProject("edit-survives")
	h.seedLines(projectID, fixtureSet())

	lineID := segments.DeriveID(projectID, 1)

	status, body, raw := h.request("PUT",
		"/api/v1/projects/"+projectID+"/segments/"+lineID,
		map[string]any{"translated_text": "您好，世界"})
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}

	if body["is_edited"] != true {
		t.Errorf("the edit did not mark the line as edited: %v", body)
	}
	if body["translated_text"] != "您好，世界" {
		t.Errorf("translated_text = %v", body["translated_text"])
	}

	// The pipeline runs again with a different translation for that line.
	replacement := fixtureSet()
	replacement.Segments[0].TranslatedText = nil
	value := "哈喽"
	replacement.Segments[0].SetTranslation(value)
	h.seedLines(projectID, replacement)

	stored, err := h.app.Segments.Get(ctx, projectID, lineID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.TranslatedText == nil || *stored.TranslatedText != "您好，世界" {
		t.Errorf("a re-run overwrote a human correction: %v", stored.TranslatedText)
	}
}

// An edit must invalidate the stages that turn lines into files.
//
// Otherwise the exported subtitles and the rendered video would carry the
// previous text, and nothing would say why.
func TestPhase8EditsInvalidateTheOutputStages(t *testing.T) {
	h := newAPIHarness(t)
	ctx := context.Background()
	projectID := h.seedProject("edit-invalidates")
	h.seedLines(projectID, fixtureSet())

	before, err := h.app.Segments.LinesHash(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if before == "" {
		t.Fatal("the lines hash is empty, so it cannot key anything")
	}

	lineID := segments.DeriveID(projectID, 1)
	if _, err := h.app.Segments.Apply(ctx, projectID, lineID, segments.Edit{
		TranslatedText: ptr("改过的译文"),
	}); err != nil {
		t.Fatal(err)
	}

	after, err := h.app.Segments.LinesHash(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Error("editing a line did not change the lines hash, so the output stages would serve a stale file")
	}

	// A review decision is not an edit, and must not re-render a film because
	// someone ticked a checkbox.
	if _, err := h.app.Segments.SetReviewState(ctx, projectID, lineID, segments.ReviewApproved, false); err != nil {
		t.Fatal(err)
	}

	reviewed, err := h.app.Segments.LinesHash(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if reviewed != after {
		t.Error("a review decision changed the lines hash, so ticking a checkbox would re-render the video")
	}
}

// Approving a line must not mark it hand-edited.
//
// Conflating the two would mean working through a review queue froze every line
// against later improvement.
func TestPhase8ReviewIsNotAnEdit(t *testing.T) {
	h := newAPIHarness(t)
	ctx := context.Background()
	projectID := h.seedProject("review")
	h.seedLines(projectID, fixtureSet())

	lineID := segments.DeriveID(projectID, 2)

	status, body, raw := h.request("POST",
		"/api/v1/projects/"+projectID+"/segments/"+lineID+"/review",
		map[string]any{"review_state": "approved"})
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}

	if body["review_state"] != "approved" {
		t.Errorf("review_state = %v", body["review_state"])
	}
	if body["is_edited"] == true {
		t.Error("approving a line marked it hand-edited, which would freeze it against a later run")
	}
	if body["needs_review"] == true {
		t.Error("an approved line is still flagged for review")
	}

	stored, err := h.app.Segments.Get(ctx, projectID, lineID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.TranslatedText == nil || *stored.TranslatedText != "再见" {
		t.Errorf("approving a line changed its text: %v", stored.TranslatedText)
	}
}

// A nonsense review state must be refused rather than stored.
func TestPhase8ReviewStateIsValidated(t *testing.T) {
	h := newAPIHarness(t)
	projectID := h.seedProject("review-invalid")
	h.seedLines(projectID, fixtureSet())

	lineID := segments.DeriveID(projectID, 1)

	for _, state := range []string{"", "maybe", "none", "pending"} {
		status, body, raw := h.request("POST",
			"/api/v1/projects/"+projectID+"/segments/"+lineID+"/review",
			map[string]any{"review_state": state})

		if status != http.StatusBadRequest {
			t.Errorf("review_state=%q returned %d, want 400: %s", state, status, raw)
			continue
		}
		if code := errorCode(body); code != "INVALID_REQUEST" {
			t.Errorf("review_state=%q gave code %q", state, code)
		}
	}
}

// ---------------------------------------------------------------------------
// Split and merge
// ---------------------------------------------------------------------------

// Splitting must produce two lines whose text and timings agree.
func TestPhase8SplitProducesCoherentHalves(t *testing.T) {
	h := newAPIHarness(t)
	ctx := context.Background()
	projectID := h.seedProject("split")
	h.seedLines(projectID, fixtureSet())

	lineID := segments.DeriveID(projectID, 1)

	status, body, raw := h.request("POST",
		"/api/v1/projects/"+projectID+"/segments/"+lineID+"/split",
		map[string]any{"at": 2.0})
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}

	first, _ := body["first"].(map[string]any)
	second, _ := body["second"].(map[string]any)

	if first["source_text"] != "こんにちは" {
		t.Errorf("first half = %v, want こんにちは", first["source_text"])
	}
	if second["source_text"] != "世界" {
		t.Errorf("second half = %v, want 世界", second["source_text"])
	}

	// The halves must meet exactly, with no gap and no overlap. Anything else
	// is a pair of lines that cannot both be shown.
	if first["end"].(float64) != second["start"].(float64) {
		t.Errorf("the halves do not meet: %v then %v", first["end"], second["start"])
	}

	// The translation is dropped rather than divided, and both halves say so.
	if first["translated_text"] != nil {
		t.Errorf("the first half kept a translation that no longer matches its text: %v", first["translated_text"])
	}
	if first["needs_review"] != true || second["needs_review"] != true {
		t.Error("the halves were not flagged for review, so the lost translation would be silent")
	}

	// And the project now has four lines, renumbered from one.
	total, _, err := h.app.Segments.Counts(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 {
		t.Errorf("the project has %d lines after splitting one of three", total)
	}

	// The lines after the split must have been renumbered, not duplicated.
	assertOrdinalsAreUnique(t, h, projectID)
}

// A split point that does not fall between two words must be refused.
func TestPhase8SplitRefusesAnImpossiblePoint(t *testing.T) {
	h := newAPIHarness(t)
	projectID := h.seedProject("split-invalid")
	h.seedLines(projectID, fixtureSet())

	lineID := segments.DeriveID(projectID, 1)

	for _, at := range []float64{0, 5, -1, 2.5} {
		status, body, raw := h.request("POST",
			"/api/v1/projects/"+projectID+"/segments/"+lineID+"/split",
			map[string]any{"at": at})

		if status != http.StatusBadRequest {
			t.Errorf("at=%v returned %d, want 400: %s", at, status, raw)
			continue
		}
		// The message has to be readable. It is the only thing telling the user
		// which times would work.
		nested, _ := body["error"].(map[string]any)
		if message, _ := nested["message"].(string); len(message) < 10 {
			t.Errorf("at=%v gave an unhelpful message: %q", at, message)
		}
	}

	// Nothing changed.
	total, _, err := h.app.Segments.Counts(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Errorf("a refused split changed the line count to %d", total)
	}
}

// A line with no word timings cannot be split, and must say why.
func TestPhase8SplitRefusesALineWithoutTimings(t *testing.T) {
	h := newAPIHarness(t)
	projectID := h.seedProject("split-no-words")

	translated := func(text string) *string { return &text }
	h.seedLines(projectID, &subtitle.Set{
		SourceLanguage: "ja",
		TargetLanguage: "zh-Hans",
		Segments: []*subtitle.Segment{{
			ID: "seg_0001", Start: 0, End: 4,
			SourceText: "こんにちは世界", TranslatedText: translated("你好世界"),
			Tags: []string{},
		}},
	})

	lineID := segments.DeriveID(projectID, 1)

	status, body, raw := h.request("POST",
		"/api/v1/projects/"+projectID+"/segments/"+lineID+"/split",
		map[string]any{"at": 2.0})

	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", status, raw)
	}

	nested, _ := body["error"].(map[string]any)
	message, _ := nested["message"].(string)
	if !strings.Contains(message, "word timings") {
		t.Errorf("the message does not explain the cause: %q", message)
	}
}

// Merging must join the text and the timings, and leave the project shorter.
func TestPhase8MergeProducesOneLine(t *testing.T) {
	h := newAPIHarness(t)
	ctx := context.Background()
	projectID := h.seedProject("merge")
	h.seedLines(projectID, fixtureSet())

	lineID := segments.DeriveID(projectID, 1)

	status, body, raw := h.request("POST",
		"/api/v1/projects/"+projectID+"/segments/"+lineID+"/merge",
		map[string]any{"with_next": true})
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}

	merged, _ := body["merged"].(map[string]any)

	// The two lines were 0–4 and 5–9. Merged, the result spans both.
	if merged["start"].(float64) != 0 || merged["end"].(float64) != 9 {
		t.Errorf("the merged line spans [%v, %v], want [0, 9]", merged["start"], merged["end"])
	}
	if merged["source_text"] != "こんにちは世界さようなら" {
		t.Errorf("merged text = %q", merged["source_text"])
	}
	// Latin words would take a space; Japanese must not.
	if strings.Contains(merged["source_text"].(string), " ") {
		t.Errorf("a space was inserted between two Japanese lines: %q", merged["source_text"])
	}

	// The translations join too, without a separator for CJK.
	if merged["translated_text"] != "你好世界再见" {
		t.Errorf("merged translation = %q", merged["translated_text"])
	}

	total, _, err := h.app.Segments.Counts(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("the project has %d lines after merging two of three", total)
	}

	assertOrdinalsAreUnique(t, h, projectID)
}

// Merging the last line must be refused rather than producing a phantom.
func TestPhase8MergeAtTheEndIsRefused(t *testing.T) {
	h := newAPIHarness(t)
	projectID := h.seedProject("merge-end")
	h.seedLines(projectID, fixtureSet())

	lastID := segments.DeriveID(projectID, 3)

	status, body, raw := h.request("POST",
		"/api/v1/projects/"+projectID+"/segments/"+lastID+"/merge",
		map[string]any{"with_next": true})

	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", status, raw)
	}
	nested, _ := body["error"].(map[string]any)
	if message, _ := nested["message"].(string); !strings.Contains(message, "after this one") {
		t.Errorf("the message does not explain the cause: %q", message)
	}
}

// A split followed by a merge must return the project to where it started.
//
// The round trip is what proves the two operations agree about what a line is.
// If they did not, one of them would be silently losing text or timing.
func TestPhase8SplitThenMergeRoundTrips(t *testing.T) {
	h := newAPIHarness(t)
	ctx := context.Background()
	projectID := h.seedProject("roundtrip")
	h.seedLines(projectID, fixtureSet())

	lineID := segments.DeriveID(projectID, 1)

	if status, _, raw := h.request("POST",
		"/api/v1/projects/"+projectID+"/segments/"+lineID+"/split",
		map[string]any{"at": 2.0}); status != http.StatusOK {
		t.Fatalf("split failed: %s", raw)
	}

	// After the split, the first half is at ordinal 1.
	firstHalf := segments.DeriveID(projectID, 1)

	if status, _, raw := h.request("POST",
		"/api/v1/projects/"+projectID+"/segments/"+firstHalf+"/merge",
		map[string]any{"with_next": true}); status != http.StatusOK {
		t.Fatalf("merge failed: %s", raw)
	}

	total, _, err := h.app.Segments.Counts(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Fatalf("the round trip left %d lines, want 3", total)
	}

	// The source text must be exactly what it was.
	reunited, err := h.app.Segments.Get(ctx, projectID, segments.DeriveID(projectID, 1))
	if err != nil {
		t.Fatal(err)
	}
	if reunited.SourceText != "こんにちは世界" {
		t.Errorf("the round trip changed the text to %q", reunited.SourceText)
	}
	if reunited.Start != 0 || reunited.End != 4 {
		t.Errorf("the round trip changed the timing to [%v, %v]", reunited.Start, reunited.End)
	}

	assertOrdinalsAreUnique(t, h, projectID)
}

// assertOrdinalsAreUnique checks the project has no duplicate positions.
func assertOrdinalsAreUnique(t *testing.T, h *apiHarness, projectID string) {
	t.Helper()

	status, body, raw := h.request("GET",
		"/api/v1/projects/"+projectID+"/segments?limit=500", nil)
	if status != http.StatusOK {
		t.Fatalf("listing lines: %s", raw)
	}

	items, _ := body["items"].([]any)
	seen := map[float64]bool{}

	for i, item := range items {
		line, _ := item.(map[string]any)

		ordinal, ok := line["ordinal"].(float64)
		if !ok {
			t.Fatalf("line %d has no ordinal: %v", i, line)
		}
		if seen[ordinal] {
			t.Fatalf("two lines claim ordinal %v", ordinal)
		}
		seen[ordinal] = true

		if int(ordinal) != i+1 {
			t.Errorf("line at index %d has ordinal %v; the ordering and the positions disagree",
				i, ordinal)
		}
	}
}

// ---------------------------------------------------------------------------
// Export
// ---------------------------------------------------------------------------

// The exported file must contain the edits, not the artifact.
func TestPhase8ExportIncludesEdits(t *testing.T) {
	h := newAPIHarness(t)
	projectID := h.seedProject("export")
	h.seedLines(projectID, fixtureSet())

	lineID := segments.DeriveID(projectID, 1)
	if status, _, raw := h.request("PUT",
		"/api/v1/projects/"+projectID+"/segments/"+lineID,
		map[string]any{"translated_text": "您好呀"}); status != http.StatusOK {
		t.Fatalf("edit failed: %s", raw)
	}

	response, err := h.server.Client().Get(h.server.URL + "/api/v1/projects/" + projectID + "/subtitles.srt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}

	var builder strings.Builder
	if _, err := io.Copy(&builder, response.Body); err != nil {
		t.Fatal(err)
	}
	content := builder.String()

	if !strings.Contains(content, "您好呀") {
		t.Errorf("the export does not contain the user's edit:\n%s", content)
	}
	if !strings.Contains(content, "-->") {
		t.Errorf("the export is not a SubRip file:\n%s", content)
	}

	// The filename must be offered in both forms, because a browser that only
	// understands the legacy one would mangle a Japanese name.
	disposition := response.Header.Get("Content-Disposition")
	if !strings.Contains(disposition, "filename*=") {
		t.Errorf("the download offers no UTF-8 filename: %q", disposition)
	}
}

// An export of a project with no lines is a 404, not an empty file.
func TestPhase8ExportOfAnEmptyProject(t *testing.T) {
	h := newAPIHarness(t)
	projectID := h.seedProject("empty")

	response, err := h.server.Client().Get(h.server.URL + "/api/v1/projects/" + projectID + "/subtitles.srt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", response.StatusCode)
	}
}

// An unsupported format must be refused rather than guessed at.
//
// The route is registered per format, so an unknown one falls through to the
// API's catch-all — which is the point: a client asking for something the
// server does not produce gets a JSON 404 in the API's shape, not the web
// application's HTML.
func TestPhase8ExportFormatIsValidated(t *testing.T) {
	h := newAPIHarness(t)
	projectID := h.seedProject("format")
	h.seedLines(projectID, fixtureSet())

	for _, format := range []string{"vtt", "txt", "exe"} {
		status, body, raw := h.request("GET",
			"/api/v1/projects/"+projectID+"/subtitles."+url.PathEscape(format), nil)

		if status != http.StatusNotFound {
			t.Errorf("format=%q returned %d, want 404: %s", format, status, raw)
			continue
		}
		if code := errorCode(body); code != "NOT_FOUND" {
			t.Errorf("format=%q gave code %q", format, code)
		}
	}

	// The two that are supported must still work.
	for _, format := range []string{"srt", "ass"} {
		status, _, raw := h.request("GET",
			"/api/v1/projects/"+projectID+"/subtitles."+format, nil)
		if status != http.StatusOK {
			t.Errorf("format=%q returned %d: %s", format, status, raw)
		}
	}
}

// ---------------------------------------------------------------------------
// Guardrails
// ---------------------------------------------------------------------------

// Editing must not be a way to write an impossible line.
func TestPhase8EditingValidatesTheResult(t *testing.T) {
	h := newAPIHarness(t)
	ctx := context.Background()
	projectID := h.seedProject("validate")
	h.seedLines(projectID, fixtureSet())

	lineID := segments.DeriveID(projectID, 1)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"an inverted interval", map[string]any{"start": 3.0, "end": 1.0}},
		{"a negative start", map[string]any{"start": -1.0}},
		{"a zero-length line", map[string]any{"start": 2.0, "end": 2.0}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			status, body, raw := h.request("PUT",
				"/api/v1/projects/"+projectID+"/segments/"+lineID, testCase.body)

			if status != http.StatusBadRequest {
				t.Errorf("status = %d, want 400: %s", status, raw)
				return
			}
			if code := errorCode(body); code != "INVALID_REQUEST" {
				t.Errorf("code = %q", code)
			}
		})
	}

	// The line is unchanged. A rejected edit that left the row half-written
	// would be worse than one that wrote it.
	stored, err := h.app.Segments.Get(ctx, projectID, lineID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Start != 0 || stored.End != 4 {
		t.Errorf("a rejected edit changed the timing to [%v, %v]", stored.Start, stored.End)
	}
}

// An edit must be announced, so a second view sees it without polling.
func TestPhase8EditsAreAnnounced(t *testing.T) {
	h := newAPIHarness(t)

	reader, _ := h.openStream()
	reader.next("hello")

	projectID := h.seedProject("announce")
	h.seedLines(projectID, fixtureSet())

	lineID := segments.DeriveID(projectID, 1)
	if status, _, raw := h.request("PUT",
		"/api/v1/projects/"+projectID+"/segments/"+lineID,
		map[string]any{"translated_text": "新的译文"}); status != http.StatusOK {
		t.Fatalf("edit failed: %s", raw)
	}

	frame := reader.next("segment.updated")

	if frame.Data["project_id"] != projectID {
		t.Errorf("the event is not scoped to the project: %v", frame.Data)
	}

	payload := frame.payload()
	segment, ok := payload["segment"].(map[string]any)
	if !ok {
		t.Fatalf("the event does not carry the line: %v", frame.Data)
	}
	// The whole record, not a diff: a partial update leaves the other tab's
	// copy free to diverge.
	if segment["translated_text"] != "新的译文" {
		t.Errorf("the event carries a stale line: %v", segment)
	}
	if segment["id"] != lineID {
		t.Errorf("the event is about %v, want %s", segment["id"], lineID)
	}
}

// A structural change must say so, because a client cannot patch for it.
func TestPhase8StructuralChangesAreAnnounced(t *testing.T) {
	h := newAPIHarness(t)

	reader, _ := h.openStream()
	reader.next("hello")

	projectID := h.seedProject("structural")
	h.seedLines(projectID, fixtureSet())

	lineID := segments.DeriveID(projectID, 1)
	if status, _, raw := h.request("POST",
		"/api/v1/projects/"+projectID+"/segments/"+lineID+"/split",
		map[string]any{"at": 2.0}); status != http.StatusOK {
		t.Fatalf("split failed: %s", raw)
	}

	frame := reader.next("segments.replaced")

	payload := frame.payload()
	if count, ok := payload["count"].(float64); !ok || count != 4 {
		t.Errorf("the event reports %v lines, want 4", payload["count"])
	}
}

// A retranslation with no provider configured must fail with a readable error
// rather than a 500 or a silent no-op.
func TestPhase8RetranslateWithoutAProvider(t *testing.T) {
	h := newAPIHarness(t)
	projectID := h.seedProject("retranslate")
	h.seedLines(projectID, fixtureSet())

	lineID := segments.DeriveID(projectID, 1)

	status, body, raw := h.request("POST",
		"/api/v1/projects/"+projectID+"/segments/"+lineID+"/translate",
		map[string]any{})

	if status == http.StatusOK {
		t.Fatalf("retranslating succeeded with no provider configured: %s", raw)
	}

	// The message must name the cause. "Internal error" would send the user
	// looking in the wrong place entirely.
	nested, _ := body["error"].(map[string]any)
	message, _ := nested["message"].(string)
	if !strings.Contains(strings.ToLower(message), "provider") {
		t.Errorf("the error does not name the missing provider: %q", message)
	}
}
