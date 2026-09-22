package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/glossary"
	"github.com/AhsokaTano26/NikuCooker/internal/jobs"
	"github.com/AhsokaTano26/NikuCooker/internal/project"
	"github.com/AhsokaTano26/NikuCooker/internal/qc"
	"github.com/AhsokaTano26/NikuCooker/internal/segments"
)

// startedAt is when this process began serving, for the uptime figure.
var startedAt = time.Now()

// ---------------------------------------------------------------------------
// Projects
// ---------------------------------------------------------------------------

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	limit, offset, apiErr := limitOffset(r, 50)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	status := project.Status(r.URL.Query().Get("status"))
	if status == "all" {
		status = ""
	}

	projects, total, err := s.app.Projects.List(r.Context(), project.ListQuery{
		Status: status,
		Search: r.URL.Query().Get("q"),
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		s.fail(w, classify(err))
		return
	}

	views := make([]projectView, 0, len(projects))
	for _, p := range projects {
		views = append(views, s.projectViewFor(r.Context(), p))
	}

	s.respond(w, http.StatusOK, newPage(views, total, limit, offset))
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	id, apiErr := projectID(r)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	p, err := s.app.Projects.Get(r.Context(), id)
	if err != nil {
		s.fail(w, classify(err))
		return
	}

	s.respond(w, http.StatusOK, s.projectViewFor(r.Context(), p))
}

// createProjectBody is the request shape for creating a project.
type createProjectBody struct {
	Name           string         `json:"name"`
	SourceLanguage string         `json:"source_language"`
	TargetLanguage string         `json:"target_language"`
	Style          string         `json:"style"`
	Source         sourceSelector `json:"source"`
	Config         map[string]any `json:"config"`
}

// sourceSelector says where the media comes from.
type sourceSelector struct {
	Kind string `json:"kind"` // "path" | "upload"
	Path string `json:"path,omitempty"`

	// UploadID names a previously uploaded file. Uploads arrive in a later
	// phase; the field exists so the contract does not have to change shape
	// when they do.
	UploadID string `json:"upload_id,omitempty"`
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var body createProjectBody
	if apiErr := decode(r, &body); apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	if body.Source.Kind != "path" {
		s.fail(w, Invalid(
			"only source.kind \"path\" is supported; uploading a file from the browser arrives in a later phase"))
		return
	}

	// Reading an arbitrary server-side path is a real capability, and it is
	// opt-in (config: server.allow_path_source). Handing it out implicitly
	// would let anything that can reach this port read any file the process
	// can.
	if !s.app.Config().Server.AllowPathSource {
		s.fail(w, Failed(http.StatusForbidden, CodeInvalid,
			"creating a project from a server-side path is disabled; set server.allow_path_source to enable it"))
		return
	}

	absolute, apiErr := validateSourcePath(body.Source.Path)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	created, err := s.app.Projects.Create(r.Context(), project.CreateRequest{
		Name:           body.Name,
		SourceLanguage: body.SourceLanguage,
		TargetLanguage: body.TargetLanguage,
		Style:          body.Style,
		Config:         body.Config,
		SourceName:     fileBase(absolute),
		SourceOrigin:   absolute,
	})
	if err != nil {
		s.fail(w, Invalid(err.Error()).Wrap(err))
		return
	}

	s.emitProject(created.ID)

	s.respond(w, http.StatusCreated, s.projectViewFor(r.Context(), created))
}

func (s *Server) updateProject(w http.ResponseWriter, r *http.Request) {
	id, apiErr := projectID(r)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	var body struct {
		Name           *string        `json:"name"`
		SourceLanguage *string        `json:"source_language"`
		TargetLanguage *string        `json:"target_language"`
		Style          *string        `json:"style"`
		Config         map[string]any `json:"config"`
	}
	if apiErr := decode(r, &body); apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	updated, err := s.app.Projects.Update(r.Context(), id, project.UpdateRequest{
		Name:           body.Name,
		SourceLanguage: body.SourceLanguage,
		TargetLanguage: body.TargetLanguage,
		Style:          body.Style,
		Config:         body.Config,
	})
	if err != nil {
		s.fail(w, classify(err))
		return
	}

	s.emitProject(id)

	s.respond(w, http.StatusOK, s.projectViewFor(r.Context(), updated))
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	id, apiErr := projectID(r)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	confirmed, apiErr := queryBool(r, "confirm", false)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}
	if !confirmed {
		// Deleting a project is not something to do because a request arrived.
		// The default is refusal, and the caller has to have meant it.
		s.fail(w, Invalid("pass confirm=true to delete a project"))
		return
	}

	deleteFiles, apiErr := queryBool(r, "delete_files", false)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	if err := s.app.Projects.Delete(r.Context(), id, deleteFiles); err != nil {
		s.fail(w, classify(err))
		return
	}

	s.app.Events.Emit(eventsForProject(id, "project.deleted"))

	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Pipeline
// ---------------------------------------------------------------------------

func (s *Server) getPipeline(w http.ResponseWriter, r *http.Request) {
	id, apiErr := projectID(r)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	if _, err := s.app.Projects.Get(r.Context(), id); err != nil {
		s.fail(w, classify(err))
		return
	}

	job := s.latestJob(r.Context(), id)
	plan, job := s.planFor(r.Context(), id, job)

	s.respond(w, http.StatusOK, s.pipelineView(r.Context(), id, plan, job))
}

// runBody is the request shape for starting a run.
type runBody struct {
	FromStage *string  `json:"from_stage"`
	ToStage   *string  `json:"to_stage"`
	Stages    []string `json:"stages"`
	Force     bool     `json:"force"`
}

func (s *Server) startRun(w http.ResponseWriter, r *http.Request) {
	id, apiErr := projectID(r)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	var body runBody
	if r.ContentLength != 0 {
		if apiErr := decode(r, &body); apiErr != nil {
			s.fail(w, apiErr)
			return
		}
	}

	result, err := s.app.Start(r.Context(), appRunOptions(id, body))
	if err != nil {
		s.fail(w, classify(err))
		return
	}

	// The plan is returned so the UI can render the pipeline immediately,
	// rather than waiting for the first event to arrive.
	s.respond(w, http.StatusAccepted,
		s.pipelineView(r.Context(), id, result.Plan, s.latestJob(r.Context(), id)))
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	id, apiErr := projectID(r)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	if !s.app.Scheduler.Cancel(id) {
		s.fail(w, conflict(CodeNotRunning, "this project has no run in progress"))
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) pauseRun(w http.ResponseWriter, r *http.Request) {
	// Pausing is not implemented, and answering 204 would be a lie the UI acts
	// on: it would show a paused job that is still transcribing.
	s.fail(w, Failed(http.StatusNotImplemented, CodeUnavailable,
		"pausing a run is not supported; cancel it and re-run, which resumes from the cached stages"))
}

func (s *Server) resumeRun(w http.ResponseWriter, r *http.Request) {
	s.fail(w, Failed(http.StatusNotImplemented, CodeUnavailable,
		"pausing a run is not supported, so there is nothing to resume"))
}

func (s *Server) runStage(w http.ResponseWriter, r *http.Request) {
	id, apiErr := projectID(r)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	stage := r.PathValue("stage")
	if _, ok := s.app.Registry().Get(stage); !ok {
		s.fail(w, Invalid("there is no stage named "+stage).
			With("stages", s.stageNames()))
		return
	}

	var body struct {
		Force bool `json:"force"`
	}
	if r.ContentLength != 0 {
		if apiErr := decode(r, &body); apiErr != nil {
			s.fail(w, apiErr)
			return
		}
	}

	// A single-stage run still runs whatever of its inputs is missing, which is
	// what makes "re-run the translation" work on a project that has never been
	// transcribed: the plan walks back to the first stage without an artifact.
	if _, err := s.app.Start(r.Context(), appRunOptions(id, runBody{
		Stages: []string{stage},
		Force:  body.Force,
	})); err != nil {
		s.fail(w, classify(err))
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

// ---------------------------------------------------------------------------
// Segments
// ---------------------------------------------------------------------------

func (s *Server) listSegments(w http.ResponseWriter, r *http.Request) {
	id, apiErr := projectID(r)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	limit, offset, apiErr := limitOffset(r, 200)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	needsReview, apiErr := queryBool(r, "needs_review", false)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	records, total, err := s.app.Segments.List(r.Context(), id, segments.ListQuery{
		Limit:       limit,
		Offset:      offset,
		NeedsReview: needsReview,
		Search:      r.URL.Query().Get("q"),
		Order:       r.URL.Query().Get("order"),
	})
	if err != nil {
		s.fail(w, classify(err))
		return
	}

	// Findings are attached only when asked for. A page of a thousand lines
	// carrying a thousand finding lists is a response nobody needs and
	// everybody pays for.
	includeQC, apiErr := queryBool(r, "include_qc", false)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}
	if includeQC {
		s.attachFindings(r.Context(), id, records)
	}

	s.respond(w, http.StatusOK, newPage(records, total, limit, offset))
}

func (s *Server) getSegment(w http.ResponseWriter, r *http.Request) {
	id, apiErr := projectID(r)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	record, err := s.app.Segments.Get(r.Context(), id, r.PathValue("segmentID"))
	if err != nil {
		s.fail(w, classify(err))
		return
	}

	records := []segments.Record{*record}
	s.attachFindings(r.Context(), id, records)

	s.respond(w, http.StatusOK, records[0])
}

// attachFindings loads the QC findings for a page of lines.
func (s *Server) attachFindings(ctx context.Context, projectID string, records []segments.Record) {
	if len(records) == 0 {
		return
	}

	bySegment, err := s.app.QC.ForSegments(ctx, projectID, idsOf(records))
	if err != nil {
		// The lines are still worth returning without their findings. Failing
		// the whole request would hide the transcript because a secondary
		// query failed.
		s.log.Warn("could not load the quality findings", "project_id", projectID, "error", err)
		return
	}

	for i := range records {
		if findings, ok := bySegment[records[i].ID]; ok {
			records[i].QC = findings
		}
	}
}

// ---------------------------------------------------------------------------
// QC
// ---------------------------------------------------------------------------

func (s *Server) listFindings(w http.ResponseWriter, r *http.Request) {
	id, apiErr := projectID(r)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	limit, offset, apiErr := limitOffset(r, 200)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	resolved, apiErr := queryBool(r, "resolved", false)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	findings, total, err := s.app.QC.List(r.Context(), id, qc.ListQuery{
		Severity: qc.Severity(r.URL.Query().Get("severity")),
		Code:     r.URL.Query().Get("code"),
		Resolved: resolved,
		Limit:    limit,
		Offset:   offset,
	})
	if err != nil {
		s.fail(w, classify(err))
		return
	}

	summary, err := s.app.QC.Summary(r.Context(), id)
	if err != nil {
		s.fail(w, classify(err))
		return
	}

	body := struct {
		page[qc.StoredFinding]
		Summary qcSummary `json:"summary"`
	}{
		page:    newPage(findings, total, limit, offset),
		Summary: qcSummary{Error: summary[qc.SeverityError], Warning: summary[qc.SeverityWarning], Info: summary[qc.SeverityInfo]},
	}

	s.respond(w, http.StatusOK, body)
}

func (s *Server) resolveFinding(w http.ResponseWriter, r *http.Request) {
	id, apiErr := projectID(r)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	var body struct {
		Resolved bool `json:"resolved"`
	}
	if apiErr := decode(r, &body); apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	if err := s.app.QC.Resolve(r.Context(), id, r.PathValue("findingID"), body.Resolved); err != nil {
		s.fail(w, classify(err))
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Glossary
// ---------------------------------------------------------------------------

func (s *Server) listGlossary(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")

	includeDisabled, apiErr := queryBool(r, "include_disabled", true)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	entries, err := s.app.Glossary.List(r.Context(), projectID, includeDisabled)
	if err != nil {
		s.fail(w, classify(err))
		return
	}

	views := make([]glossaryView, 0, len(entries))
	for _, entry := range entries {
		views = append(views, glossaryView{
			ID: entry.ID, ProjectID: entry.ProjectID,
			Source: entry.Source, Target: entry.Target,
			Type: string(entry.Type), Note: entry.Note,
			Priority: entry.Priority, Enabled: entry.Enabled,
			Origin:    string(entry.Origin),
			CreatedAt: entry.CreatedAt, UpdatedAt: entry.UpdatedAt,
		})
	}

	s.respond(w, http.StatusOK, struct {
		Items []glossaryView `json:"items"`
	}{Items: views})
}

func (s *Server) saveGlossary(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProjectID *string `json:"project_id"`
		Source    string  `json:"source"`
		Target    string  `json:"target"`
		Type      string  `json:"type"`
		Note      string  `json:"note"`
		Priority  int     `json:"priority"`
		Enabled   *bool   `json:"enabled"`
	}
	if apiErr := decode(r, &body); apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	entry := &glossary.Entry{
		ProjectID: body.ProjectID,
		Source:    body.Source,
		Target:    body.Target,
		Type:      glossary.Type(body.Type),
		Note:      body.Note,
		Priority:  body.Priority,
		Enabled:   true,
	}
	if body.Enabled != nil {
		entry.Enabled = *body.Enabled
	}

	if err := s.app.Glossary.Save(r.Context(), entry); err != nil {
		s.fail(w, Invalid(err.Error()).Wrap(err))
		return
	}

	s.respond(w, http.StatusCreated, glossaryView{
		ID: entry.ID, ProjectID: entry.ProjectID,
		Source: entry.Source, Target: entry.Target,
		Type: string(entry.Type), Note: entry.Note,
		Priority: entry.Priority, Enabled: entry.Enabled,
		Origin:    string(entry.Origin),
		CreatedAt: entry.CreatedAt, UpdatedAt: entry.UpdatedAt,
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// projectViewFor builds a project's view, including the counts it carries.
//
// The duration is taken from the last line rather than from the probe artifact.
// The artifact is the accurate answer, but reading it means decoding a file per
// project per request — and on a list of fifty projects that is fifty file
// reads to display a number nobody acts on. The end of the last subtitle is the
// same figure for anything that has been through segmentation.
func (s *Server) projectViewFor(ctx context.Context, p *project.Project) projectView {
	view := projectView{
		ID:             p.ID,
		Name:           p.Name,
		SourceLanguage: p.SourceLanguage,
		TargetLanguage: p.TargetLanguage,
		Style:          p.Style,
		Status:         string(p.Status),
		CreatedAt:      p.CreatedAt,
		UpdatedAt:      p.UpdatedAt,
	}

	if count, needsReview, err := s.app.Segments.Counts(ctx, p.ID); err == nil {
		view.SegmentCount = count
		view.NeedsReviewCount = needsReview
	}
	if duration, err := s.app.Segments.DurationOf(ctx, p.ID); err == nil && duration > 0 {
		view.Duration = &duration
	}

	if job := s.latestJob(ctx, p.ID); job != nil && !job.Status.Terminal() {
		view.CurrentJob = &jobView{
			ID: job.ID, Status: string(job.Status), Progress: job.Progress,
		}
	}

	return view
}

// latestJob returns a project's most recent job.
func (s *Server) latestJob(ctx context.Context, projectID string) *jobs.Job {
	projectJobs, _, err := s.app.Jobs.ListByProject(ctx, projectID, 1, 0)
	if err != nil || len(projectJobs) == 0 {
		return nil
	}
	return projectJobs[0]
}

func idsOf(records []segments.Record) []string {
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.ID)
	}
	return ids
}

// validateSourcePath checks a caller-supplied media path.
func validateSourcePath(path string) (string, *Error) {
	if path == "" {
		return "", Invalid("a source path is required")
	}

	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", NotFound("no file at " + path)
		}
		return "", Invalid("cannot read " + path + ": " + err.Error())
	}
	if info.IsDir() {
		return "", Invalid(path + " is a directory, not a media file")
	}
	if info.Size() == 0 {
		return "", Invalid(path + " is empty")
	}
	return path, nil
}

func fileBase(path string) string {
	// filepath.Base rather than string surgery: this is the one place a
	// user-supplied path becomes a filename, and it is worth being exact.
	return filepath.Base(path)
}
