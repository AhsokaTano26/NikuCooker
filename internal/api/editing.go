package api

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"

	"github.com/AhsokaTano26/NikuCooker/internal/events"
	"github.com/AhsokaTano26/NikuCooker/internal/segments"
	"github.com/AhsokaTano26/NikuCooker/internal/subtitle"
)

// The editor's write endpoints.
//
// Every one of them answers with the line as it now stands, rather than with a
// 204. An editor that has to refetch after every keystroke is an editor that
// flickers, and the server has the answer in hand — sending it back is a few
// hundred bytes and removes any chance of the client's copy diverging from the
// server's, which is the failure that makes a live editor untrustworthy.
//
// Every one of them also publishes a `segment.updated` event, so a second tab,
// or the review queue, or a bulk retranslation running elsewhere, sees the
// change without polling.

// updateSegmentBody is the request shape for editing a line.
type updateSegmentBody struct {
	TranslatedText *string   `json:"translated_text"`
	Start          *float64  `json:"start"`
	End            *float64  `json:"end"`
	Speaker        *string   `json:"speaker"`
	Tags           *[]string `json:"tags"`
	ReviewState    *string   `json:"review_state"`
	NeedsReview    *bool     `json:"needs_review"`
}

func (s *Server) updateSegment(w http.ResponseWriter, r *http.Request) {
	projectID, apiErr := projectID(r)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	var body updateSegmentBody
	if apiErr := decode(r, &body); apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	edit := segments.Edit{
		TranslatedText: body.TranslatedText,
		Start:          body.Start,
		End:            body.End,
		Speaker:        body.Speaker,
		Tags:           body.Tags,
		NeedsReview:    body.NeedsReview,
	}
	if body.ReviewState != nil {
		state := segments.ReviewState(*body.ReviewState)
		edit.ReviewState = &state
	}

	record, err := s.app.Segments.Apply(r.Context(), projectID, r.PathValue("segmentID"), edit)
	if err != nil {
		s.fail(w, classify(err))
		return
	}

	s.publishSegment(projectID, record)
	s.respond(w, http.StatusOK, record)
}

// reviewSegmentBody is the request shape for a review decision.
type reviewSegmentBody struct {
	ReviewState string `json:"review_state"`
}

func (s *Server) reviewSegment(w http.ResponseWriter, r *http.Request) {
	projectID, apiErr := projectID(r)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	var body reviewSegmentBody
	if apiErr := decode(r, &body); apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	// A decision is not an edit, so this takes the other path: the line keeps
	// its text and its is_edited flag, and a later run is still free to improve
	// a translation a reviewer merely approved.
	state := segments.ReviewState(body.ReviewState)
	if !state.Reviewed() {
		s.fail(w, Invalid(
			"review_state must be approved, rejected, pending or none"))
		return
	}

	record, err := s.app.Segments.SetReviewState(r.Context(), projectID,
		r.PathValue("segmentID"), state, state == segments.ReviewPending)
	if err != nil {
		s.fail(w, classify(err))
		return
	}

	s.publishSegment(projectID, record)
	s.respond(w, http.StatusOK, record)
}

// splitBody is the request shape for splitting a line.
type splitBody struct {
	At float64 `json:"at"`
}

func (s *Server) splitSegment(w http.ResponseWriter, r *http.Request) {
	projectID, apiErr := projectID(r)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	var body splitBody
	if apiErr := decode(r, &body); apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	first, second, err := s.app.Segments.Split(r.Context(), projectID, r.PathValue("segmentID"), body.At)
	if err != nil {
		s.fail(w, classify(err))
		return
	}

	// Splitting shifts every line after it, so the client cannot patch its
	// copy — it has to refetch. The event says so rather than leaving a view
	// quietly showing the pre-split ordinals.
	s.afterStructuralChange(r, projectID)

	s.respond(w, http.StatusOK, struct {
		First  *segments.Record `json:"first"`
		Second *segments.Record `json:"second"`
	}{First: first, Second: second})
}

// mergeBody is the request shape for merging a line.
type mergeBody struct {
	WithNext bool `json:"with_next"`
}

func (s *Server) mergeSegment(w http.ResponseWriter, r *http.Request) {
	projectID, apiErr := projectID(r)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	body := mergeBody{WithNext: true}
	if r.ContentLength != 0 {
		if apiErr := decode(r, &body); apiErr != nil {
			s.fail(w, apiErr)
			return
		}
	}

	merged, absorbed, err := s.app.Segments.Merge(r.Context(), projectID, r.PathValue("segmentID"), body.WithNext)
	if err != nil {
		s.fail(w, classify(err))
		return
	}

	s.afterStructuralChange(r, projectID)

	s.respond(w, http.StatusOK, struct {
		Merged   *segments.Record `json:"merged"`
		Absorbed *segments.Record `json:"absorbed"`
	}{Merged: merged, Absorbed: absorbed})
}

// translateSegmentBody is the request shape for retranslating one line.
type translateSegmentBody struct {
	Style string `json:"style"`
}

func (s *Server) translateSegment(w http.ResponseWriter, r *http.Request) {
	projectID, apiErr := projectID(r)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	var body translateSegmentBody
	if r.ContentLength != 0 {
		if apiErr := decode(r, &body); apiErr != nil {
			s.fail(w, apiErr)
			return
		}
	}

	record, err := s.app.RetranslateLine(r.Context(), projectID, r.PathValue("segmentID"), body.Style)
	if err != nil {
		s.fail(w, classify(err))
		return
	}

	s.publishSegment(projectID, record)
	s.respond(w, http.StatusOK, record)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// publishSegment announces a changed line.
//
// The whole record is inlined rather than a diff. A line is a few hundred
// bytes, and shipping all of it removes any possibility of the client's copy
// diverging from the server's — which is exactly what makes a live-updating
// editor untrustworthy when it happens.
func (s *Server) publishSegment(projectID string, record *segments.Record) {
	if record == nil {
		return
	}
	s.app.Events.Emit(events.New(events.TypeSegmentUpdated).
		ForProject(projectID).
		With("segment", record))
}

// afterStructuralChange announces that a project's lines were renumbered.
//
// Inserting or removing a line shifts every ordinal after it, so a client
// patching one record would be left with a list whose ids and positions no
// longer agree. The event tells it to refetch rather than to patch.
func (s *Server) afterStructuralChange(r *http.Request, projectID string) {
	total, needsReview, err := s.app.Segments.Counts(r.Context(), projectID)
	if err != nil {
		s.log.Warn("could not count the project's lines", "error", err)
	}

	s.app.Events.Emit(events.New(events.TypeSegmentsReplaced).
		ForProject(projectID).
		With("count", total).
		With("needs_review", needsReview))
}

// ---------------------------------------------------------------------------
// Download
// ---------------------------------------------------------------------------

// downloadSubtitles renders the project's lines as a subtitle file.
//
// Generated from the table, not from the subtitle artifact. The artifact is
// what a run produced; the table is what the project currently says, and a user
// who has spent an hour in the editor is asking for the latter. Re-running the
// pipeline would produce the same file, but only after paying for every stage
// that has since gone stale.
func (s *Server) downloadSRT(w http.ResponseWriter, r *http.Request) {
	s.downloadSubtitles(w, r, "srt")
}

func (s *Server) downloadASS(w http.ResponseWriter, r *http.Request) {
	s.downloadSubtitles(w, r, "ass")
}

func (s *Server) downloadSubtitles(w http.ResponseWriter, r *http.Request, format string) {
	projectID, apiErr := projectID(r)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	prj, err := s.app.Projects.Get(r.Context(), projectID)
	if err != nil {
		s.fail(w, classify(err))
		return
	}

	set, err := s.app.Segments.CurrentLines(r.Context(), projectID)
	if err != nil {
		s.fail(w, classify(err))
		return
	}
	if set == nil || len(set.Segments) == 0 {
		s.fail(w, NotFound("this project has no subtitle lines yet"))
		return
	}

	preset, err := subtitle.PresetByName(s.app.Config().Subtitle.Preset)
	if err != nil {
		s.fail(w, internal(err))
		return
	}

	// Rendered into a buffer before anything is written, so a failure produces
	// an error response rather than a truncated download the user only notices
	// when their player refuses the file.
	var buffer bytes.Buffer
	writeErr := func() error {
		if format == "srt" {
			return subtitle.WriteSRT(&buffer, set, s.app.Config().Subtitle.Bilingual)
		}
		return subtitle.WriteASS(&buffer, set, preset, s.app.Config().Subtitle.Bilingual, prj.Name)
	}()
	if writeErr != nil {
		s.fail(w, internal(writeErr))
		return
	}

	// The filename carries the project's name, which may be Japanese. Sending
	// it in both forms is what makes the download work in a browser that
	// prefers one and a client that only understands the other.
	filename := sanitiseFilename(prj.Name) + "." + format

	w.Header().Set("Content-Type", subtitleContentType(format))
	w.Header().Set("Content-Disposition",
		`attachment; filename="`+asciiFallback(filename)+`"; filename*=UTF-8''`+url.PathEscape(filename))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buffer.Bytes())
}

func subtitleContentType(format string) string {
	if format == "srt" {
		return "application/x-subrip; charset=utf-8"
	}
	return "text/x-ssa; charset=utf-8"
}

// sanitiseFilename removes what cannot appear in a filename.
func sanitiseFilename(name string) string {
	replacer := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_",
		"?", "_", "\"", "_", "<", "_", ">", "_", "|", "_",
		"\n", "_", "\r", "_",
	)
	cleaned := strings.TrimSpace(replacer.Replace(name))
	if cleaned == "" {
		return "subtitles"
	}
	return cleaned
}

// asciiFallback strips a filename to printable ASCII for the legacy header.
//
// The `filename` parameter predates UTF-8 and a client that only understands it
// will mangle a Japanese name. `filename*` carries the real one; this is the
// fallback, and replacing each character rather than dropping it keeps the two
// recognisably the same.
func asciiFallback(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r >= 0x20 && r < 0x7f && r != '"' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	return b.String()
}
