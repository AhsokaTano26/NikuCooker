package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/AhsokaTano26/NikuCooker/internal/artifact"
	"github.com/AhsokaTano26/NikuCooker/internal/events"
	"github.com/AhsokaTano26/NikuCooker/internal/jobs"
	"github.com/AhsokaTano26/NikuCooker/internal/pipeline"
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

	job := &jobs.Job{
		ID:        prepared.JobID,
		ProjectID: prepared.ProjectID,
		Kind:      kindFor(opts),
		Force:     opts.Force,
		Status:    jobs.StatusPending,
	}
	if err := a.Jobs.Create(ctx, job); err != nil {
		a.Scheduler.Finish(prepared.ProjectID)
		return nil, err
	}

	go a.execute(jobCtx, prepared, opts)

	return &RunResult{JobID: prepared.JobID, Plan: prepared.plan}, nil
}

// kindFor reports whether a run covers the whole pipeline or one stage.
func kindFor(opts RunOptions) jobs.Kind {
	if len(opts.Only) == 1 {
		return jobs.KindStage
	}
	return jobs.KindFull
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
		Services: stage.Services{
			Providers:   a.Providers,
			Glossary:    a.Glossary,
			Translation: a.Cache,
		},
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

	status, code, message := jobOutcome(ctx, runErr)
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

	a.log.Info("run finished",
		"job_id", work.JobID, "project_id", work.ProjectID,
		"status", status, "elapsed_s", time.Since(started).Seconds())
}

// jobOutcome maps a run's result onto a job status.
func jobOutcome(ctx context.Context, runErr error) (jobs.Status, string, string) {
	switch {
	case runErr == nil:
		return jobs.StatusCompleted, "", ""
	case errors.Is(runErr, context.Canceled) || ctx.Err() != nil:
		return jobs.StatusCancelled, "CANCELLED", "the run was cancelled"
	default:
		return jobs.StatusFailed, "RUN_FAILED", runErr.Error()
	}
}

// persist writes the run's output into the project's rows.
//
// The artifacts are the record of what a run produced; these tables are the
// project's current state, which a user may then edit. A failure here does not
// fail the run — the artifacts are on disk and the work is not lost — but it is
// logged loudly, because it means the UI will show stale lines.
func (a *App) persist(ctx context.Context, work *prepared, opts RunOptions) {
	// A cancelled run's partial output is deliberately not written. Half a
	// transcript presented as the project's current state is worse than the
	// previous state, which was at least complete.
	ctx = context.WithoutCancel(ctx)

	set, lineage := linesFromPlan(a, work)
	if set == nil {
		return
	}

	if err := a.Segments.ReplaceFromArtifacts(ctx, work.ProjectID, set, lineage); err != nil {
		a.log.Error("could not store the project's lines", "project_id", work.ProjectID, "error", err)
		return
	}

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
