package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/AhsokaTano26/NikuCooker/internal/artifact"
	"github.com/AhsokaTano26/NikuCooker/internal/events"
	"github.com/AhsokaTano26/NikuCooker/internal/jobs"
	"github.com/AhsokaTano26/NikuCooker/internal/logging"
	"github.com/AhsokaTano26/NikuCooker/internal/pipeline"
	"github.com/AhsokaTano26/NikuCooker/internal/project"
	"github.com/AhsokaTano26/NikuCooker/internal/qc"
	"github.com/AhsokaTano26/NikuCooker/internal/segments"
	"github.com/AhsokaTano26/NikuCooker/internal/stage"
	"github.com/AhsokaTano26/NikuCooker/internal/subtitle"
)

// Start plans a run and executes it in the background.
//
// The plan is built synchronously and returned to the caller, because the
// problems planning can find — an unknown project, a stage that does not exist,
// a project already running — are the caller's to report with a status code. A
// run that failed inside a goroutine would reach the user as a job that
// mysteriously did nothing.
func (a *App) Start(ctx context.Context, opts RunOptions) (*RunResult, error) {
	prepared, err := a.prepare(ctx, opts)
	if err != nil {
		return nil, err
	}

	jobCtx, err := a.Scheduler.Begin(context.WithoutCancel(ctx), prepared.ProjectID, prepared.JobID)
	if err != nil {
		return nil, err
	}

	if err := a.Jobs.Create(ctx, jobFor(prepared.JobID, prepared.ProjectID, opts)); err != nil {
		a.Scheduler.Finish(prepared.ProjectID)
		return nil, err
	}

	go a.execute(jobCtx, prepared, opts)

	return &RunResult{JobID: prepared.JobID, ProjectID: prepared.ProjectID, Plan: prepared.plan}, nil
}

// jobFor builds the job record a run is tracked under.
//
// One place, because the schema requires a single-stage job to name its stage:
// `kind = 'stage'` with no `target_stage` is rejected by a CHECK constraint, and
// building the record in two places would mean remembering that in two places.
func jobFor(jobID, projectID string, opts RunOptions) *jobs.Job {
	job := &jobs.Job{
		ID:        jobID,
		ProjectID: projectID,
		Kind:      jobs.KindFull,
		Force:     opts.Force,
		Status:    jobs.StatusPending,
	}
	if len(opts.Only) == 1 {
		job.Kind = jobs.KindStage
		job.TargetStage = opts.Only[0]
	}
	return job
}

// prepared is everything a run needs, resolved and validated.
type prepared struct {
	ProjectID string
	JobID     string
	plan      *pipeline.Plan
	options   pipeline.Options
}

// prepare validates and plans a run without starting one.
func (a *App) prepare(ctx context.Context, opts RunOptions) (*prepared, error) {
	if opts.ProjectID == "" {
		return nil, errors.New("app: no project was named")
	}

	prj, err := a.Projects.Get(ctx, opts.ProjectID)
	if err != nil {
		return nil, err
	}

	jobID := opts.JobID
	if jobID == "" {
		jobID = "job_" + uuid.Must(uuid.NewV7()).String()
	}

	cfg, err := a.projectConfig(prj)
	if err != nil {
		return nil, err
	}

	store := a.Artifacts.For(prj.ID)
	if _, err := store.SweepTempDirectories(); err != nil {
		a.log.Warn("could not sweep abandoned temporary directories", "error", err)
	}

	sourcePath, err := a.sourcePath(prj)
	if err != nil {
		return nil, err
	}

	sourceFingerprint, err := artifact.FingerprintSource(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("app: identify the source media: %w", err)
	}

	planOpts := pipeline.Options{
		Registry:  a.registry,
		Config:    cfg,
		Store:     store,
		ProjectID: prj.ID,
		JobID:     jobID,
		Project: stage.ProjectInfo{
			Name:           prj.Name,
			SourceLanguage: prj.SourceLanguage,
			TargetLanguage: prj.TargetLanguage,
			Style:          prj.Style,
		},
		Services:          a.stageServices(),
		CodeRevision:      a.codeRevision,
		SourceFingerprint: sourceFingerprint,
		SourcePath:        sourcePath,
		Media:             a.Media,
		Models:            a.Models,
		Force:             opts.Force,
		Only:              opts.Only,
		FromStage:         opts.FromStage,
		ToStage:           opts.ToStage,
		Log:               a.log,
	}

	plan, err := pipeline.Build(ctx, planOpts)
	if err != nil {
		return nil, err
	}

	// Resolved after the plan, so that a run consisting entirely of cached
	// stages never starts a Python process.
	if stageNeedsWorker(plan) {
		pool, err := a.Worker(ctx)
		if err != nil {
			return nil, err
		}
		planOpts.Worker = pool
	}

	return &prepared{
		ProjectID: prj.ID,
		JobID:     jobID,
		plan:      plan,
		options:   planOpts,
	}, nil
}

// execute runs the plan and records everything that follows from it.
func (a *App) execute(ctx context.Context, work *prepared, opts RunOptions) {
	defer a.Scheduler.Finish(work.ProjectID)

	started := time.Now().UTC()
	if err := a.Jobs.SetStatus(ctx, work.JobID, jobs.StatusRunning, "", ""); err != nil {
		a.log.Warn("could not mark the job running", "job_id", work.JobID, "error", err)
	}
	a.emit(events.New(events.TypeJobStatus).
		ForProject(work.ProjectID).
		With("job_id", work.JobID).
		With("status", string(jobs.StatusRunning)).
		With("previous", string(jobs.StatusPending)))

	// A heartbeat tells the next process that this job was running when the
	// current one died, which is what makes startup reconciliation able to tell
	// an abandoned job from one that is still going.
	stopHeartbeat := a.Jobs.HeartbeatLoop(ctx, work.JobID, 10*time.Second, a.log)
	defer stopHeartbeat()

	// The run's log file, and the logger that reaches it.
	//
	// Attached here rather than in prepare, because preparing happens for a
	// pipeline view as well as for a run, and a page that only looks at a
	// project should not leave a file behind.
	//
	// Set on the options the pipeline and every stage read, so one assignment
	// covers the whole run — including the stages, which each derive their
	// logger from it with their own attributes added.
	if stopLog := a.attachRunLog(ctx, work); stopLog != nil {
		defer stopLog()
	}

	// The run's own logger, for what the run says about itself. Falls back to
	// the application's when there is no file, so the lines are logged either
	// way rather than only when logging to disk happens to have worked.
	runLog := work.options.Log
	if runLog == nil {
		runLog = a.log
	}

	runLog.Info("run started",
		"job_id", work.JobID, "project_id", work.ProjectID,
		"stages", len(work.plan.Stages), "force", opts.Force)

	// The run's own observer persists stage state and publishes events; the
	// caller's, when there is one, is what draws a progress bar. Both receive
	// every transition, and neither knows about the other.
	observer := &runObserver{
		runner:    jobs.NewRunner(a.Jobs, work.JobID, work.ProjectID, a.ordinals(), a.log),
		app:       a,
		jobID:     work.JobID,
		projectID: work.ProjectID,
	}

	runErr := pipeline.Run(ctx, work.plan, work.options, fanOut(observer, opts.Observer))

	a.persist(ctx, work, opts)
	a.publishOutputs(ctx, work.ProjectID)
	a.pruneCache(ctx)

	status, code, message := jobOutcome(ctx, runErr, work.plan)
	// Recorded on a context that is still live: the common cause of a failed
	// run is a cancellation, and a cancelled context cannot write the row that
	// tells the user it was cancelled.
	finishCtx := context.WithoutCancel(ctx)
	if err := a.Jobs.SetStatus(finishCtx, work.JobID, status, code, message); err != nil {
		a.log.Warn("could not record the job outcome", "job_id", work.JobID, "error", err)
	}

	a.emit(events.New(events.TypeJobStatus).
		ForProject(work.ProjectID).
		With("job_id", work.JobID).
		With("status", string(status)).
		With("error_code", code).
		With("error_message", message).
		With("elapsed_s", time.Since(started).Seconds()))

	// Through the run's logger rather than the application's, so the file holds
	// the whole run — including the outcome, which is the line someone opens it
	// for. The stages already log through this one; the run's own start and
	// finish were the two lines missing.
	runLog.Info("run finished",
		"job_id", work.JobID, "project_id", work.ProjectID,
		"status", status, "elapsed_s", time.Since(started).Seconds())
}

// attachRunLog gives a run a logger that also writes to its own log file.
//
// Returns a function to close the file, or nil when there is nowhere to write
// — a run must not fail because its logging did. The file is a record of the
// run, not a part of it.
func (a *App) attachRunLog(ctx context.Context, work *prepared) func() {
	cfg := a.Config()
	projectDir := project.Dir(a.dataDir, work.ProjectID)

	path := filepath.Join(projectDir, project.DirLogs, work.JobID+".log")

	file, err := logging.CreateRunLog(path, int64(cfg.Retention.LogMaxMB)<<20)
	if err != nil {
		a.log.Warn("could not open the run's log file; this run will not be recorded to disk",
			"project_id", work.ProjectID, "job_id", work.JobID, "error", err)
		return nil
	}

	work.options.Log = slog.New(logging.NewMulti(
		a.log.Handler(),
		logging.NewFileHandler(file),
	))

	_ = ctx
	return func() {
		if err := file.Close(); err != nil {
			a.log.Warn("could not close the run's log file", "path", path, "error", err)
		}
	}
}

// pruneCache keeps the translation cache within its configured size.
//
// Run at the end of a job rather than on a timer: the cache only grows when a
// run adds to it, so that is the only moment the bound can be exceeded — and a
// background sweep would contend with the writer for no reason.
//
// Failing is not fatal. A cache above its limit costs disk; a run that fails
// because housekeeping did is a worse trade.
func (a *App) pruneCache(ctx context.Context) {
	limit := a.Config().Translation.CacheMaxEntries
	if limit <= 0 {
		return
	}

	removed, err := a.Cache.Prune(context.WithoutCancel(ctx), limit)
	if err != nil {
		a.log.Warn("could not trim the translation cache", "error", err)
		return
	}
	if removed > 0 {
		a.log.Info("trimmed the translation cache", "removed", removed, "limit", limit)
	}
}

// jobOutcome maps a run's result onto a job status.
//
// The plan is consulted as well as the returned error, because the executor
// reports a stage's failure on that stage rather than returning it. A run whose
// render failed returns no error at all — the stages that could run did, and
// their artifacts are real — so judging by the returned error alone records it
// as completed, and the job list then says a run succeeded while the video it
// exists to produce is missing.
//
// This is the rule the CLI applies to decide its exit code, deliberately: a
// script and the interface reading the same run must not disagree about whether
// it worked.
func jobOutcome(ctx context.Context, runErr error, plan *pipeline.Plan) (jobs.Status, string, string) {
	switch {
	case errors.Is(runErr, context.Canceled) || ctx.Err() != nil:
		return jobs.StatusCancelled, "CANCELLED", "the run was cancelled"
	case runErr != nil:
		return jobs.StatusFailed, "RUN_FAILED", runErr.Error()
	}

	for _, sp := range plan.Stages {
		name := sp.Stage.Spec().Name
		switch {
		case sp.Err != nil:
			return jobs.StatusFailed, "STAGE_FAILED", fmt.Sprintf("%s: %v", name, sp.Err)
		case sp.Blocked:
			// Not an error the pipeline raised, but the run did not do what it
			// was asked: a stage that was selected could not run.
			return jobs.StatusFailed, "STAGE_BLOCKED", fmt.Sprintf("%s: %s", name, sp.Reason)
		}
	}
	return jobs.StatusCompleted, "", ""
}

// persist writes the run's output into the project's rows.
//
// The artifacts are the record of what a run produced; these tables are the
// project's current state, which a user may then edit. A failure here does not
// fail the run — the artifacts are on disk and the work is not lost — but it is
// logged loudly, because it means the UI will show stale lines.
//
// It runs as soon as the lines exist rather than at the end of the run, because
// the stages after translation read the table: writing it afterwards would mean
// the subtitles and the render were built from the artifact, and every edit a
// user had made would be missing from the file they asked for.
func (a *App) persist(ctx context.Context, work *prepared, opts RunOptions) {
	// A cancelled run's partial output is deliberately not written. Half a
	// transcript presented as the project's current state is worse than the
	// previous state, which was at least complete.
	ctx = context.WithoutCancel(ctx)

	// By now the lines themselves are stored — the observer wrote them when
	// translation settled, so that the stages after it could read them. What is
	// left is the report, which is written once the run is over because the
	// findings describe the run rather than the project.
	set, lineage := linesFromPlan(a, work)
	if set == nil {
		return
	}
	_ = lineage

	ordinals := segments.OrdinalMap(work.ProjectID, set)

	if report := qcFromPlan(a, work); report != nil {
		if err := a.Segments.SaveFindings(ctx, work.ProjectID, "qc", report.Findings, ordinals); err != nil {
			a.log.Error("could not store the quality findings", "project_id", work.ProjectID, "error", err)
		}
	}

	total, needsReview, err := a.Segments.Counts(ctx, work.ProjectID)
	if err != nil {
		a.log.Warn("could not count the project's lines", "error", err)
	}

	a.emit(events.New(events.TypeSegmentsReplaced).
		ForProject(work.ProjectID).
		With("count", total).
		With("needs_review", needsReview))

	a.emit(events.New(events.TypeProjectUpdated).
		ForProject(work.ProjectID).
		With("changed", []string{"segment_count", "needs_review_count"}))
}

// ---------------------------------------------------------------------------
// Observer
// ---------------------------------------------------------------------------

// fanOut sends every notification to both observers.
//
// A nil second observer is the API's case: it has no progress bar, only the
// event stream the first observer publishes to.
func fanOut(primary, secondary pipeline.Observer) pipeline.Observer {
	if secondary == nil {
		return primary
	}
	return &multiObserver{first: primary, second: secondary}
}

type multiObserver struct {
	first  pipeline.Observer
	second pipeline.Observer
}

func (m *multiObserver) StageEntered(ctx context.Context, plan *pipeline.StagePlan) {
	m.first.StageEntered(ctx, plan)
	m.second.StageEntered(ctx, plan)
}

func (m *multiObserver) StageProgress(ctx context.Context, plan *pipeline.StagePlan, fraction float64, message string) {
	m.first.StageProgress(ctx, plan, fraction, message)
	m.second.StageProgress(ctx, plan, fraction, message)
}

func (m *multiObserver) StageSettled(ctx context.Context, plan *pipeline.StagePlan) {
	m.first.StageSettled(ctx, plan)
	m.second.StageSettled(ctx, plan)
}

// runObserver records stage state and mirrors it onto the event stream.
//
// One type rather than two observers, because the two outputs describe the same
// transitions and two observers would be two chances for them to disagree about
// what just happened.
type runObserver struct {
	runner    *jobs.Runner
	app       *App
	jobID     string
	projectID string
}

func (o *runObserver) StageEntered(ctx context.Context, plan *pipeline.StagePlan) {
	o.runner.StageEntered(ctx, plan)

	o.app.emit(events.New(events.TypeStageStatus).
		ForProject(o.projectID).
		ForJob(o.jobID).
		ForStage(plan.Stage.Spec().Name).
		With("status", string(pipeline.StateRunning)).
		With("ordinal", plan.Ordinal))
}

func (o *runObserver) StageProgress(ctx context.Context, plan *pipeline.StagePlan, fraction float64, message string) {
	o.runner.StageProgress(ctx, plan, fraction, message)

	o.app.emit(events.New(events.TypeStageProgress).
		ForProject(o.projectID).
		ForJob(o.jobID).
		ForStage(plan.Stage.Spec().Name).
		With("progress", fraction).
		With("message", message))
}

func (o *runObserver) StageSettled(ctx context.Context, plan *pipeline.StagePlan) {
	o.runner.StageSettled(ctx, plan)

	// The moment the lines exist, they are written to the project's rows. Every
	// stage after this one reads them from there, so leaving it until the run
	// finished would build the subtitles from the artifact and drop whatever
	// the user had edited.
	if name := plan.Stage.Spec().Name; name == "translation" || name == "polish" {
		if plan.State.Succeeded() {
			o.app.persistLines(ctx, o.projectID, plan)
		}
	}

	event := events.New(events.TypeStageStatus).
		ForProject(o.projectID).
		ForJob(o.jobID).
		ForStage(plan.Stage.Spec().Name).
		With("status", string(plan.State)).
		With("ordinal", plan.Ordinal).
		With("progress", plan.Progress).
		With("duration_ms", plan.Duration.Milliseconds()).
		With("attempt", plan.Attempt)

	if plan.Artifact != nil {
		event = event.With("artifact_id", plan.Artifact.ID)
	}
	if plan.Reason != "" {
		event = event.With("reason", plan.Reason)
	}
	if plan.Err != nil {
		event = event.With("error_code", "STAGE_FAILED").With("error_message", plan.Err.Error())
	}
	if plan.Result != nil && len(plan.Result.Metadata) > 0 {
		event = event.With("metadata", plan.Result.Metadata)
	}

	o.app.emit(event)
}

// ordinals maps a stage name to its position, for the runner's progress bar.
func (a *App) ordinals() map[string]int {
	ordinals := map[string]int{}
	for i, name := range a.registry.Names() {
		ordinals[name] = i + 1
	}
	return ordinals
}

// emit publishes an event, if anything is listening.
func (a *App) emit(event events.Event) {
	if a.Events == nil {
		return
	}
	a.Events.Emit(event)
}

// persistLines writes one stage's output into the project's rows.
func (a *App) persistLines(ctx context.Context, projectID string, plan *pipeline.StagePlan) {
	if plan.Artifact == nil {
		return
	}

	// Detached from the run's context: this is a write that must land even if
	// the run is being cancelled, and the rows it produces are already complete.
	ctx = context.WithoutCancel(ctx)

	var set subtitle.Set
	if err := a.Artifacts.For(projectID).Decode(plan.Artifact, &set); err != nil {
		a.log.Error("could not read a stage's lines", "stage", plan.Stage.Spec().Name, "error", err)
		return
	}

	lineage := segments.Lineage{TranslationArtifactID: plan.Artifact.ID}
	if err := a.Segments.ReplaceFromArtifacts(ctx, projectID, &set, lineage); err != nil {
		a.log.Error("could not store the project's lines", "project_id", projectID, "error", err)
	}
}

// ---------------------------------------------------------------------------
// Reading a run's output back
// ---------------------------------------------------------------------------

// linesFromPlan finds the lines a completed run produced.
//
// The polished set wins when it exists, for the same reason the subtitle stage
// prefers it: it is what the user will actually see.
func linesFromPlan(a *App, work *prepared) (*subtitle.Set, segments.Lineage) {
	candidates := []string{"polish", "translation"}
	for _, name := range candidates {
		sp := findStage(work.plan, name)
		if sp == nil || sp.Artifact == nil || !sp.State.Succeeded() {
			continue
		}

		var set subtitle.Set
		if err := a.Artifacts.For(work.ProjectID).Decode(sp.Artifact, &set); err != nil {
			a.log.Error("could not read the run's lines", "stage", name, "error", err)
			continue
		}

		lineage := segments.Lineage{TranslationArtifactID: sp.Artifact.ID}
		if segmentation := findStage(work.plan, "segmentation"); segmentation != nil && segmentation.Artifact != nil {
			lineage.SegmentationArtifactID = segmentation.Artifact.ID
		}
		return &set, lineage
	}
	return nil, segments.Lineage{}
}

// qcFromPlan reads the quality report a run produced.
//
// Decoded from the artifact rather than kept on the plan: the report is an
// artifact like any other, and a second in-memory copy would be a second thing
// to keep in step.
func qcFromPlan(a *App, work *prepared) *qc.Report {
	sp := findStage(work.plan, "qc")
	if sp == nil || sp.Artifact == nil || !sp.State.Succeeded() {
		return nil
	}

	var report qc.Report
	if err := a.Artifacts.For(work.ProjectID).Decode(sp.Artifact, &report); err != nil {
		a.log.Warn("could not read the quality report", "error", err)
		return nil
	}
	return &report
}

func findStage(plan *pipeline.Plan, name string) *pipeline.StagePlan {
	for _, sp := range plan.Stages {
		if sp.Stage.Spec().Name == name {
			return sp
		}
	}
	return nil
}
