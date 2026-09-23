// Package api serves the REST surface.
//
// Every handler here does the same thing: read a request, ask the application
// for an answer, render it. No business logic lives in this package — the CLI
// calls the same methods on the same object — so the two front ends cannot
// disagree about what an operation does. What does live here is the translation
// between HTTP and the domain: status codes, the error envelope, pagination.
//
// Errors are returned as a stable code with a human message (docs/api.md §1.3).
// Clients branch on the code and never on the prose, which is what makes the
// messages free to improve without breaking anything.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/AhsokaTano26/NikuCooker/internal/app"
	"github.com/AhsokaTano26/NikuCooker/internal/artifact"
	"github.com/AhsokaTano26/NikuCooker/internal/config"
	"github.com/AhsokaTano26/NikuCooker/internal/glossary"
	"github.com/AhsokaTano26/NikuCooker/internal/jobs"
	"github.com/AhsokaTano26/NikuCooker/internal/pipeline"
	"github.com/AhsokaTano26/NikuCooker/internal/project"
	"github.com/AhsokaTano26/NikuCooker/internal/provider"
	"github.com/AhsokaTano26/NikuCooker/internal/segments"
	"github.com/AhsokaTano26/NikuCooker/internal/settings"
)

// Options configures the API.
type Options struct {
	App *app.App
	Log *slog.Logger

	// Version and Commit are reported by the system endpoint.
	Version string
	Commit  string

	// RequestShutdown ends the process when the interface asks it to.
	//
	// Nil when there is no process to end — an embedded server, or a test —
	// and the endpoint says so rather than pretending to have done it.
	RequestShutdown func()
}

// Server holds the routing table.
type Server struct {
	app *app.App
	log *slog.Logger

	// mux is the table Routes built. Kept so that unrouted can ask it what a
	// request would have matched.
	mux *http.ServeMux

	version string
	commit  string

	requestShutdown func()
}

// New builds the API and returns its handler.
func New(opts Options) (*Server, error) {
	if opts.App == nil {
		return nil, errors.New("api: the application is required")
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}

	server := &Server{
		app:             opts.App,
		log:             opts.Log,
		version:         opts.Version,
		commit:          opts.Commit,
		requestShutdown: opts.RequestShutdown,
	}
	server.mux = server.buildRoutes()
	return server, nil
}

// Routes returns the API's routing table, rooted at /api/v1.
func (s *Server) Routes() *http.ServeMux { return s.mux }

// buildRoutes builds the API's routing table.
func (s *Server) buildRoutes() *http.ServeMux {
	mux := http.NewServeMux()

	// Go's pattern matching carries the method and the wildcards, so a route
	// that only accepts POST is registered as POST and everything else is a 405
	// without a handler having to check.
	mux.HandleFunc("GET /api/v1/system", s.getSystem)
	mux.HandleFunc("GET /api/v1/system/health", s.getHealth)
	mux.HandleFunc("POST /api/v1/system/shutdown", s.requestProcessShutdown)
	mux.HandleFunc("POST /api/v1/system/runtime/provision", s.provisionRuntime)
	mux.HandleFunc("POST /api/v1/system/runtime/provision/cancel", s.cancelProvision)
	mux.HandleFunc("GET /api/v1/events", s.getEvents)

	mux.HandleFunc("POST /api/v1/uploads", s.createUpload)
	mux.HandleFunc("DELETE /api/v1/uploads/{uploadID}", s.deleteUpload)

	mux.HandleFunc("GET /api/v1/projects", s.listProjects)
	mux.HandleFunc("POST /api/v1/projects", s.createProject)
	mux.HandleFunc("GET /api/v1/projects/{id}", s.getProject)
	mux.HandleFunc("PATCH /api/v1/projects/{id}", s.updateProject)
	mux.HandleFunc("DELETE /api/v1/projects/{id}", s.deleteProject)

	mux.HandleFunc("GET /api/v1/projects/{id}/pipeline", s.getPipeline)
	mux.HandleFunc("POST /api/v1/projects/{id}/run", s.startRun)
	mux.HandleFunc("POST /api/v1/projects/{id}/cancel", s.cancelRun)
	mux.HandleFunc("POST /api/v1/projects/{id}/pause", s.pauseRun)
	mux.HandleFunc("POST /api/v1/projects/{id}/resume", s.resumeRun)
	mux.HandleFunc("POST /api/v1/projects/{id}/stages/{stage}/run", s.runStage)

	mux.HandleFunc("GET /api/v1/projects/{id}/segments", s.listSegments)
	mux.HandleFunc("GET /api/v1/projects/{id}/segments/{segmentID}", s.getSegment)
	mux.HandleFunc("PUT /api/v1/projects/{id}/segments/{segmentID}", s.updateSegment)
	mux.HandleFunc("POST /api/v1/projects/{id}/segments/{segmentID}/review", s.reviewSegment)
	mux.HandleFunc("POST /api/v1/projects/{id}/segments/{segmentID}/split", s.splitSegment)
	mux.HandleFunc("POST /api/v1/projects/{id}/segments/{segmentID}/merge", s.mergeSegment)
	mux.HandleFunc("POST /api/v1/projects/{id}/segments/{segmentID}/translate", s.translateSegment)

	// What the last run published, and the files themselves. This is where a
	// finished project's results are, as opposed to the artifact cache they
	// were copied out of.
	mux.HandleFunc("GET /api/v1/projects/{id}/files", s.listProjectFiles)
	mux.HandleFunc("DELETE /api/v1/projects/{id}/files/{kind}", s.deleteProjectFiles)
	mux.HandleFunc("GET /api/v1/projects/{id}/logs/{name}", s.downloadLog)

	mux.HandleFunc("GET /api/v1/projects/{id}/outputs", s.listOutputs)
	mux.HandleFunc("GET /api/v1/projects/{id}/outputs/{name}", s.downloadOutput)

	// The current lines as a subtitle file, generated from the table rather
	// than from an artifact — so it includes edits the user has not re-run the
	// pipeline for. Not the same thing as the files above, which are what a run
	// produced.
	//
	// Two routes rather than one with a wildcard suffix: a Go pattern's wildcard
	// must be a whole path segment, and "subtitles.{format}" is not one. The
	// file extension in the URL is worth two lines of registration.
	mux.HandleFunc("GET /api/v1/projects/{id}/subtitles.srt", s.downloadSRT)
	mux.HandleFunc("GET /api/v1/projects/{id}/subtitles.ass", s.downloadASS)

	mux.HandleFunc("GET /api/v1/projects/{id}/qc", s.listFindings)
	mux.HandleFunc("PATCH /api/v1/projects/{id}/qc/{findingID}", s.resolveFinding)

	mux.HandleFunc("GET /api/v1/glossary", s.listGlossary)
	mux.HandleFunc("POST /api/v1/glossary", s.saveGlossary)

	mux.HandleFunc("GET /api/v1/models", s.listModels)
	mux.HandleFunc("POST /api/v1/models/{modelID}/download", s.downloadModel)
	mux.HandleFunc("DELETE /api/v1/models/{modelID}", s.deleteModel)

	mux.HandleFunc("GET /api/v1/providers", s.listProviders)
	mux.HandleFunc("POST /api/v1/providers", s.createProvider)
	mux.HandleFunc("PATCH /api/v1/providers/{providerID}", s.updateProvider)
	mux.HandleFunc("DELETE /api/v1/providers/{providerID}", s.deleteProvider)
	mux.HandleFunc("POST /api/v1/providers/{providerID}/test", s.testProvider)

	mux.HandleFunc("GET /api/v1/settings", s.getSettings)
	mux.HandleFunc("PATCH /api/v1/settings", s.updateSettings)
	mux.HandleFunc("DELETE /api/v1/settings/{key}", s.deleteSetting)
	mux.HandleFunc("GET /api/v1/logs", s.listLogs)

	// Anything else under the API prefix is answered in the API's own shape,
	// rather than with the HTML the static handler would otherwise return. A
	// client that asked for JSON and got a page has to guess what happened.
	mux.HandleFunc("/api/v1/", s.unrouted)

	return mux
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// ErrorCode is a stable identifier a client branches on.
type ErrorCode string

const (
	CodeNotFound ErrorCode = "NOT_FOUND"

	// CodePathSourceDisabled is its own code because the client's response is
	// its own: this is not a bad request to correct, it is a setting to turn on,
	// and the interface needs to tell the two apart to say so.
	CodePathSourceDisabled ErrorCode = "PATH_SOURCE_DISABLED"
	CodeInvalid            ErrorCode = "INVALID_REQUEST"
	CodeConflict           ErrorCode = "CONFLICT"
	CodeInternal           ErrorCode = "INTERNAL"
	CodeUnavailable        ErrorCode = "UNAVAILABLE"
	CodeAlreadyRun         ErrorCode = "ALREADY_RUNNING"
	CodeNotRunning         ErrorCode = "NOT_RUNNING"
)

// Error is the envelope every failure is rendered as.
type Error struct {
	Code    ErrorCode      `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`

	// status is the HTTP status, and is not serialised: the code is the
	// contract, the status is transport.
	status int

	// cause is logged and never sent. Internal errors carry file paths, SQL and
	// stack-shaped detail, and a local tool still should not hand those to a
	// browser.
	cause error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.cause }

// errorBody is the wire shape.
type errorBody struct {
	Error struct {
		Code    ErrorCode      `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details,omitempty"`
	} `json:"error"`
}

// Failed builds an error response.
func Failed(status int, code ErrorCode, message string) *Error {
	return &Error{Code: code, Message: message, status: status}
}

// Wrap attaches an underlying cause.
func (e *Error) Wrap(cause error) *Error {
	e.cause = cause
	return e
}

// With attaches a detail field.
func (e *Error) With(key string, value any) *Error {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	e.Details[key] = value
	return e
}

// NotFound builds a 404.
func NotFound(what string) *Error {
	return Failed(http.StatusNotFound, CodeNotFound, what+" was not found")
}

// Invalid builds a 400.
func Invalid(message string) *Error {
	return Failed(http.StatusBadRequest, CodeInvalid, message)
}

// conflict builds a 409.
func conflict(code ErrorCode, message string) *Error {
	return Failed(http.StatusConflict, code, message)
}

// internal builds a 500.
func internal(cause error) *Error {
	return Failed(http.StatusInternalServerError, CodeInternal,
		"the server could not complete the request").Wrap(cause)
}

// classify maps a domain error onto the API's shape.
//
// It exists so that a handler does not have to know every error a service can
// return. Anything unrecognised becomes a 500, which is correct: an error this
// function does not understand is one nobody has decided how to present, and
// guessing would give the client a code it might act on wrongly.
func classify(err error) *Error {
	if err == nil {
		return nil
	}

	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr
	}

	switch {
	case errors.Is(err, project.ErrNotFound):
		return NotFound("project")
	case errors.Is(err, segments.ErrNotFound):
		return NotFound("segment")
	case errors.Is(err, segments.ErrInvalid):
		// A request the user can fix. The repository's message is written to be
		// read by them, so it is passed through rather than replaced.
		return Invalid(strings.TrimPrefix(err.Error(), "segments: invalid: "))
	case errors.Is(err, glossary.ErrNotFound):
		return NotFound("glossary entry")
	case errors.Is(err, config.ErrInvalid):
		// A value the configuration rejected. The message names the key and
		// what is wrong with it, which is exactly what the form should show.
		return Invalid(strings.TrimPrefix(err.Error(), "config: "))
	case errors.Is(err, settings.ErrUnknownKey), errors.Is(err, settings.ErrInvalidValue):
		// A request the user can fix, and the message is written to be read by
		// them: it names the key and what was wrong with the value.
		return Invalid(strings.TrimPrefix(err.Error(), "settings: "))
	case errors.Is(err, provider.ErrNotFound):
		// Missing rather than by design: deleting a provider that is already
		// gone is a 404, not the 500 that a sentinel nobody mapped produces.
		return NotFound("provider")
	case errors.Is(err, project.ErrUploadNotFound):
		// A malformed id reaches here as well as a missing one, and they are
		// deliberately not distinguished: an id that was never issued and an id
		// that was consumed are the same thing to a client, and saying which
		// would mean answering questions about uploads that are not this
		// client's.
		return NotFound("upload")
	case errors.Is(err, jobs.ErrNotFound):
		return NotFound("job")
	case errors.Is(err, jobs.ErrAlreadyRunning):
		return conflict(CodeAlreadyRun, "this project is already running")
	case errors.Is(err, provider.ErrNotConfigured):
		// A configuration problem the user can fix, and the message names what
		// to set. Reporting it as a 500 would send them looking at the server
		// rather than at their settings.
		return Failed(http.StatusPreconditionFailed, CodeUnavailable,
			strings.TrimPrefix(err.Error(), "provider: "))
	case errors.Is(err, context.Canceled):
		return Failed(http.StatusRequestTimeout, CodeInvalid, "the request was cancelled")
	}

	return internal(err)
}

// ---------------------------------------------------------------------------
// Responding
// ---------------------------------------------------------------------------

// respond writes a JSON body.
func (s *Server) respond(w http.ResponseWriter, status int, body any) {
	if body == nil {
		w.WriteHeader(status)
		return
	}

	// Encoded into a buffer before the header is written, so that a value that
	// fails to marshal produces a clean 500 rather than a truncated body under
	// a 200 — which a client would report as a parse error somewhere far from
	// the cause.
	encoded, err := json.Marshal(body)
	if err != nil {
		s.log.Error("could not encode a response", "error", err)
		s.fail(w, internal(err))
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

// fail writes an error response.
func (s *Server) fail(w http.ResponseWriter, err *Error) {
	if err == nil {
		return
	}

	status := err.status
	if status == 0 {
		status = http.StatusInternalServerError
	}

	if status >= 500 {
		// The one place the cause is read. Everything below 500 is a statement
		// about the request and is safe to show; a 500 is a statement about the
		// server and its detail is for the log.
		s.log.Error("request failed",
			"code", string(err.Code), "status", status, "error", err.cause)
	}

	var body errorBody
	body.Error.Code = err.Code
	body.Error.Message = err.Message
	body.Error.Details = err.Details

	encoded, marshalErr := json.Marshal(body)
	if marshalErr != nil {
		http.Error(w, `{"error":{"code":"INTERNAL","message":"encoding failed"}}`,
			http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

// unrouted answers a request no route matched.
//
// It distinguishes a path that does not exist from a path that exists under a
// different method. Go's mux would do that itself, but only when nothing matches
// the path at all — and this catch-all matches everything, which is what gives
// an unknown API path a JSON body instead of the web application's HTML.
//
// Asking the mux which handler a request would reach is the standard library's
// own answer to that question, so the two cannot drift apart.
func (s *Server) unrouted(w http.ResponseWriter, r *http.Request) {
	// Probed with the *other* methods. Asking with the request's own method
	// returns this catch-all, which is the pattern currently running and says
	// nothing about whether the path exists elsewhere.
	var allowed []string
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete} {
		if method == r.Method {
			continue
		}
		probe := r.Clone(r.Context())
		probe.Method = method

		if _, pattern := s.mux.Handler(probe); pattern != "" && pattern != catchAll {
			allowed = append(allowed, method)
		}
	}

	if len(allowed) > 0 {
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		s.fail(w, Failed(http.StatusMethodNotAllowed, CodeInvalid,
			r.Method+" is not allowed on this endpoint"))
		return
	}

	s.fail(w, NotFound("that endpoint"))
}

// catchAll is the pattern an unrouted request resolves to.
const catchAll = "/api/v1/"

// ---------------------------------------------------------------------------
// Request helpers
// ---------------------------------------------------------------------------

// decode reads a JSON body, rejecting the mistakes that would otherwise be
// silently ignored.
func decode(r *http.Request, v any) *Error {
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBodyBytes))

	// An unknown field is almost always a typo or a client built against a
	// different version. Ignoring it means the request succeeds and does
	// something subtly different from what was asked, which is much harder to
	// diagnose than a rejection.
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(v); err != nil {
		return Invalid("the request body is not valid JSON for this endpoint: " + err.Error())
	}
	return nil
}

// maxBodyBytes bounds a request body.
//
// Generous, because a project's per-project configuration overlay arrives on
// this path, and small enough that a mistaken upload does not sit in memory.
const maxBodyBytes = 1 << 20

// queryInt reads an integer query parameter.
func queryInt(r *http.Request, name string, fallback int) (int, *Error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, Invalid(fmt.Sprintf("%s must be a number", name))
	}
	return value, nil
}

// queryBool reads a boolean query parameter.
func queryBool(r *http.Request, name string, fallback bool) (bool, *Error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, Invalid(fmt.Sprintf("%s must be true or false", name))
	}
	return value, nil
}

// page describes a paginated response.
type page[T any] struct {
	Items  []T `json:"items"`
	Total  int `json:"total"`
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

// newPage builds a page, ensuring the item list is never null.
//
// A client that has to null-check every list will eventually forget, and the one
// place it forgets is the one that crashes.
func newPage[T any](items []T, total, limit, offset int) page[T] {
	if items == nil {
		items = []T{}
	}
	return page[T]{Items: items, Total: total, Limit: limit, Offset: offset}
}

// limitOffset normalises pagination parameters.
func limitOffset(r *http.Request, defaultLimit int) (limit, offset int, err *Error) {
	limit, err = queryInt(r, "limit", defaultLimit)
	if err != nil {
		return 0, 0, err
	}
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > 500 {
		limit = 500
	}

	offset, err = queryInt(r, "offset", 0)
	if err != nil {
		return 0, 0, err
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset, nil
}

// projectID reads the path's project identifier.
func projectID(r *http.Request) (string, *Error) {
	id := r.PathValue("id")
	if strings.TrimSpace(id) == "" {
		return "", Invalid("a project id is required")
	}
	return id, nil
}

// stageNames lists the registered stages, for error messages that name them.
func (s *Server) stageNames() []string {
	return s.app.Registry().Names()
}

// stageLabels renders a stage name for display.
//
// The server localises, not the client. A UI that mapped stage names to labels
// itself would show a raw identifier for any stage added after it shipped.
func stageLabel(name string) string {
	switch name {
	case "probe":
		return "Reading the container"
	case "audio":
		return "Extracting audio"
	case "vad":
		return "Detecting speech"
	case "asr":
		return "Transcribing"
	case "segmentation":
		return "Splitting into lines"
	case "context":
		return "Analysing the work"
	case "translation":
		return "Translating"
	case "polish":
		return "Polishing"
	case "qc":
		return "Checking quality"
	case "subtitle":
		return "Writing subtitles"
	case "render":
		return "Rendering"
	default:
		return name
	}
}

// runState renders a plan as the API's pipeline view.
func (s *Server) pipelineView(ctx context.Context, projectID string, plan *pipeline.Plan, job *jobs.Job) pipelineView {
	view := pipelineView{ProjectID: projectID, Stages: []stageView{}}

	if job != nil {
		view.Job = &jobView{ID: job.ID, Status: string(job.Status), Progress: job.Progress}
	}

	for _, sp := range plan.Stages {
		name := sp.Stage.Spec().Name
		stage := stageView{
			Name:       name,
			Label:      stageLabel(name),
			Ordinal:    sp.Ordinal,
			Status:     string(sp.State),
			Progress:   sp.Progress,
			DurationMS: sp.Duration.Milliseconds(),
		}
		if sp.Artifact != nil {
			stage.ArtifactID = sp.Artifact.ID
		}
		if sp.Reason != "" {
			stage.Reason = sp.Reason
		}
		if sp.Err != nil {
			stage.ErrorCode = "STAGE_FAILED"
			stage.ErrorMessage = sp.Err.Error()
		}
		if sp.Result != nil {
			stage.Metadata = sp.Result.Metadata
		}
		view.Stages = append(view.Stages, stage)
	}

	return view
}

// planFor reconstructs a project's most recent pipeline state.
//
// The in-memory plan exists only while a run is executing, so after a restart
// the pipeline view is rebuilt from what the database recorded. The stage list
// always comes from the registry, so a UI shows every stage the current binary
// has — including one added since the last run, which correctly reads as
// pending.
func (s *Server) planFor(ctx context.Context, projectID string, run *jobs.Job) (*pipeline.Plan, *jobs.Job) {
	plan := &pipeline.Plan{Stages: []*pipeline.StagePlan{}}

	recorded := map[string]jobs.StageRun{}
	ordinals := map[string]int{}
	for i, name := range s.app.Registry().Names() {
		ordinals[name] = i + 1
	}

	if run != nil {
		runs, err := s.app.Jobs.StageRuns(ctx, run.ID)
		if err != nil {
			s.log.Warn("could not read the stage runs", "job_id", run.ID, "error", err)
		}
		for _, stageRun := range runs {
			recorded[stageRun.Name] = stageRun
		}
	}

	for _, staged := range s.app.Registry().Ordered() {
		name := staged.Spec().Name
		sp := &pipeline.StagePlan{
			Stage:   staged,
			Ordinal: ordinals[name],
			State:   pipeline.StatePending,
		}
		if stageRun, ok := recorded[name]; ok {
			sp.State = pipeline.State(stageRun.Status)
			sp.Progress = stageRun.Progress
			sp.Duration = stageRun.Elapsed()
			sp.Reason = stageRun.Reason()
			if stageRun.ArtifactID != "" {
				// Only the identifier is needed: the view reports which artifact
				// a stage produced, not its contents, and a full record would
				// mean a database read per stage per page load.
				sp.Artifact = &artifact.Artifact{ID: stageRun.ArtifactID}
			}
			if stageRun.ErrorMessage != "" {
				sp.Err = errors.New(stageRun.ErrorMessage)
			}
		}
		plan.Stages = append(plan.Stages, sp)
	}

	return plan, run
}
