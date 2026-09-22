package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/pipeline"
)

// Scheduler enforces "one job per project at a time" and owns cancellation.
//
// The constraint is enforced here rather than by a database index: a unique
// partial index on (project_id, status) would need to exempt every terminal
// state, and it would fight the startup reconciliation that rewrites those
// rows.
type Scheduler struct {
	mu      sync.Mutex
	running map[string]*execution
}

type execution struct {
	jobID  string
	cancel context.CancelFunc
}

// NewScheduler builds an empty scheduler.
func NewScheduler() *Scheduler {
	return &Scheduler{running: make(map[string]*execution)}
}

// Begin claims a project for a job and returns a context that Cancel will end.
func (s *Scheduler) Begin(ctx context.Context, projectID, jobID string) (context.Context, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.running[projectID]; ok {
		return nil, fmt.Errorf("%w (job %s)", ErrAlreadyRunning, existing.jobID)
	}

	jobCtx, cancel := context.WithCancel(ctx)
	s.running[projectID] = &execution{jobID: jobID, cancel: cancel}
	return jobCtx, nil
}

// Finish releases a project.
func (s *Scheduler) Finish(projectID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if execution, ok := s.running[projectID]; ok {
		// Cancelling an already-finished context is a no-op, and doing it here
		// guarantees the heartbeat goroutine stops even on the success path.
		execution.cancel()
		delete(s.running, projectID)
	}
}

// Cancel stops the project's running job. It reports whether one was running.
func (s *Scheduler) Cancel(projectID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	execution, ok := s.running[projectID]
	if !ok {
		return false
	}
	execution.cancel()
	return true
}

// AnyRunning reports whether any project has a job in progress.
//
// Used to refuse an action that would break a run in flight — deleting a model
// a job may be using — rather than discovering the conflict halfway through it.
func (s *Scheduler) AnyRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.running) > 0
}

// Running reports the job currently running for a project, if any.
func (s *Scheduler) Running(projectID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	execution, ok := s.running[projectID]
	if !ok {
		return "", false
	}
	return execution.jobID, true
}

// ---------------------------------------------------------------------------
// Runner
// ---------------------------------------------------------------------------

// Runner persists pipeline progress as job state.
//
// It is the bridge between the pipeline, which knows nothing about databases,
// and the job records a user sees. Every method runs on the goroutine executing
// the plan, so the writes are serialised by construction.
type Runner struct {
	repo *Repository
	log  *slog.Logger

	jobID     string
	projectID string

	// ordinals maps a stage name to its position in the pipeline definition.
	ordinals map[string]int

	lastProgressWrite time.Time
	stageStarted      map[string]time.Time
}

// NewRunner builds a runner for one job.
func NewRunner(repo *Repository, jobID, projectID string, ordinals map[string]int, log *slog.Logger) *Runner {
	return &Runner{
		repo:         repo,
		log:          log,
		jobID:        jobID,
		projectID:    projectID,
		ordinals:     ordinals,
		stageStarted: make(map[string]time.Time),
	}
}

// StageEntered records that a stage started.
func (r *Runner) StageEntered(ctx context.Context, plan *pipeline.StagePlan) {
	name := plan.Stage.Spec().Name
	r.stageStarted[name] = time.Now()

	now := time.Now().UTC()
	err := r.repo.UpsertStageRun(ctx, StageRun{
		JobID:     r.jobID,
		ProjectID: r.projectID,
		Name:      name,
		Ordinal:   r.ordinalOf(plan),
		Status:    string(pipeline.StateRunning),
		Attempt:   plan.Attempt,
		StartedAt: &now,
	})
	if err != nil {
		// Losing a status write is not a reason to abandon the work in
		// progress; the artifact is the thing that matters, and the plan's
		// in-memory state is still correct.
		r.log.Warn("could not record stage start", "stage", name, "error", err)
	}
}

// StageProgress records progress, throttled.
func (r *Runner) StageProgress(ctx context.Context, plan *pipeline.StagePlan, fraction float64, message string) {
	// Stage progress is cheap to hold in memory and expensive to write; the
	// job's overall progress is what the UI reads continuously, so the two are
	// written at different cadences.
	if time.Since(r.lastProgressWrite) < time.Second {
		return
	}
	r.lastProgressWrite = time.Now()

	if err := r.repo.SetProgress(ctx, r.jobID, r.overallProgress(plan, fraction)); err != nil {
		r.log.Warn("could not record job progress", "error", err)
	}
}

// StageSettled records a stage's final state.
func (r *Runner) StageSettled(ctx context.Context, plan *pipeline.StagePlan) {
	name := plan.Stage.Spec().Name

	run := StageRun{
		JobID:     r.jobID,
		ProjectID: r.projectID,
		Name:      name,
		Ordinal:   r.ordinalOf(plan),
		Status:    string(plan.State),
		Progress:  plan.Progress,
		Attempt:   plan.Attempt,
	}
	if plan.Artifact != nil {
		run.ArtifactID = plan.Artifact.ID
	}
	if plan.Err != nil {
		run.ErrorCode = "STAGE_FAILED"
		run.ErrorMessage = plan.Err.Error()
	}
	if started, ok := r.stageStarted[name]; ok {
		run.StartedAt = &started
	}
	now := time.Now().UTC()
	run.FinishedAt = &now

	if err := r.repo.UpsertStageRun(ctx, run); err != nil {
		r.log.Warn("could not record stage outcome", "stage", name, "error", err)
	}
}

func (r *Runner) ordinalOf(plan *pipeline.StagePlan) int {
	// The plan's ordinal comes from the pipeline definition; the map is a
	// fallback for a stage the runner was not told about.
	if plan.Ordinal > 0 {
		return plan.Ordinal
	}
	return r.ordinals[plan.Stage.Spec().Name]
}

// overallProgress weights a stage's fraction by its position in the pipeline.
//
// Weighting by ordinal rather than by expected duration is a deliberate
// approximation: real per-stage durations vary by orders of magnitude, but a
// user watching a bar mostly needs it to move forward at a plausible rate, and
// stage boundaries to be visible. The per-stage detail is on the pipeline
// endpoint.
func (r *Runner) overallProgress(plan *pipeline.StagePlan, fraction float64) float64 {
	total := len(r.ordinals)
	if total == 0 {
		return fraction
	}
	index := plan.Ordinal - 1
	if index < 0 {
		index = 0
	}
	return clamp01((float64(index) + clamp01(fraction)) / float64(total))
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// ---------------------------------------------------------------------------
// Heartbeat
// ---------------------------------------------------------------------------

// HeartbeatLoop periodically records that a job is alive, returning a stop
// function.
//
// Without it, startup reconciliation cannot distinguish a job this process is
// running from one the previous process abandoned.
func (r *Repository) HeartbeatLoop(ctx context.Context, jobID string, interval time.Duration, log *slog.Logger) func() {
	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := r.Heartbeat(ctx, jobID); err != nil {
					log.Warn("could not record job heartbeat", "job_id", jobID, "error", err)
				}
			}
		}
	}()

	return func() {
		close(stop)
		<-done
	}
}

// ---------------------------------------------------------------------------
// Startup reconciliation
// ---------------------------------------------------------------------------

// Reconcile resolves jobs left running by a previous process.
//
// A running job is never trusted after a restart: this process cannot know
// whether the stage it was executing completed, and the artifacts of completed
// stages are project-scoped, so nothing is lost by re-running the interrupted
// one. The invariant is that restart never resumes work it cannot prove it is
// doing, and never leaves a job stuck in running.
func (r *Repository) Reconcile(ctx context.Context) ([]string, error) {
	rows, err := r.db.Read.QueryContext(ctx,
		`SELECT id, project_id FROM jobs WHERE status IN ('running','paused')`)
	if err != nil {
		return nil, fmt.Errorf("jobs: find interrupted jobs: %w", err)
	}

	type interrupted struct{ id, projectID string }
	var found []interrupted
	for rows.Next() {
		var item interrupted
		if err := rows.Scan(&item.id, &item.projectID); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("jobs: scan interrupted job: %w", err)
		}
		found = append(found, item)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("jobs: find interrupted jobs: %w", err)
	}

	var reconciled []string
	for _, item := range found {
		// Running and paused both transition to failed here. A paused job whose
		// process died is not resumable in place either: resuming creates a new
		// job, and the artifact cache makes that cheap.
		if err := r.SetStatus(ctx, item.id, StatusFailed, CodeInterrupted,
			"the process exited while this job was running"); err != nil {
			return reconciled, fmt.Errorf("jobs: reconcile %s: %w", item.id, err)
		}

		// Any stage left running is likewise unresolvable.
		if _, err := r.db.Write.ExecContext(ctx, `
			UPDATE stages SET status = 'failed', error_code = ?, error_message = ?
			WHERE job_id = ? AND status = 'running'`, CodeInterrupted,
			"the process exited while this stage was running", item.id); err != nil {
			return reconciled, fmt.Errorf("jobs: reconcile stages of %s: %w", item.id, err)
		}

		reconciled = append(reconciled, item.id)
	}
	return reconciled, nil
}
