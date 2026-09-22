package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/artifact"
	"github.com/AhsokaTano26/NikuCooker/internal/stage"
)

// Observer receives stage lifecycle notifications.
//
// The pipeline knows nothing about databases, events or job state; it reports
// what happened and lets the caller decide what that means. That is what allows
// the same executor to drive a CLI run, a job, and a test.
type Observer interface {
	// StageEntered is called before a stage starts running.
	StageEntered(ctx context.Context, plan *StagePlan)

	// StageProgress is called at most a few times a second per stage.
	StageProgress(ctx context.Context, plan *StagePlan, fraction float64, message string)

	// StageSettled is called once a stage reaches a terminal state, whether it
	// ran, was cached, was skipped, or failed.
	StageSettled(ctx context.Context, plan *StagePlan)
}

// NopObserver ignores everything.
type NopObserver struct{}

func (NopObserver) StageEntered(context.Context, *StagePlan)                   {}
func (NopObserver) StageProgress(context.Context, *StagePlan, float64, string) {}
func (NopObserver) StageSettled(context.Context, *StagePlan)                   {}

// progressInterval bounds how often a stage's progress reaches the observer.
//
// Progress events are coalesced rather than emitted per inference step: the
// event stream should carry state changes, not a counter. Four per second is
// enough for a progress bar to look live and cheap enough to fan out.
const progressInterval = 250 * time.Millisecond

// Run executes the plan.
//
// Stages already cached are reported and skipped. Everything else runs in
// pipeline order, and a stage whose dependency did not succeed is skipped
// rather than attempted.
func Run(ctx context.Context, plan *Plan, opts Options, obs Observer) error {
	if obs == nil {
		obs = NopObserver{}
	}
	if opts.Log == nil {
		return errors.New("pipeline: executor requires a logger")
	}

	byName := make(map[string]*StagePlan, len(plan.Stages))
	for _, sp := range plan.Stages {
		byName[sp.Stage.Spec().Name] = sp
	}

	// Report the states the plan already settled, so the caller sees the whole
	// pipeline rather than only the part that ran.
	for _, sp := range plan.Stages {
		if sp.State.Terminal() {
			obs.StageSettled(ctx, sp)
		}
	}

	for _, sp := range plan.Stages {
		if sp.State != StatePending {
			continue
		}

		if err := ctx.Err(); err != nil {
			cancelRemaining(plan, obs, ctx)
			return err
		}

		// Re-checked at execution time rather than trusted from the plan: an
		// earlier stage in this same run may have failed since.
		if reason, blocked := blockedBy(sp.Stage, byName, opts.Only); blocked {
			sp.State = StateSkipped
			sp.Reason = reason
			obs.StageSettled(ctx, sp)
			continue
		}

		spec := sp.Stage.Spec()
		inputs := make(map[string]*artifact.Artifact, len(spec.Depends)+len(spec.OptionalDepends))
		missing := ""
		for _, dep := range spec.Depends {
			// A dependency inside this run has just produced its artifact; one
			// outside it was resolved from the store when the plan was built.
			if dp, ok := byName[dep]; ok {
				if dp.Artifact != nil {
					inputs[dep] = dp.Artifact
					continue
				}
			}
			if a, ok := sp.externalInputs[dep]; ok {
				inputs[dep] = a
				continue
			}
			missing = dep
			break
		}
		if missing != "" {
			sp.State = StateSkipped
			sp.Reason = fmt.Sprintf("no artifact for input %q", missing)
			obs.StageSettled(ctx, sp)
			continue
		}

		// Optional inputs are collected only when they exist, and their absence
		// is never a reason to skip. A stage that declared one is written to
		// work without it, and Env.Inputs simply will not carry the key.
		for _, dep := range spec.OptionalDepends {
			if dp, ok := byName[dep]; ok && dp.Artifact != nil {
				inputs[dep] = dp.Artifact
				continue
			}
			if a, ok := sp.externalInputs[dep]; ok {
				inputs[dep] = a
			}
		}

		runStage(ctx, sp, opts, inputs, obs)
	}

	if err := ctx.Err(); err != nil {
		cancelRemaining(plan, obs, ctx)
		return err
	}
	return nil
}

// runStage executes one stage and records its outcome.
func runStage(
	ctx context.Context,
	sp *StagePlan,
	opts Options,
	inputs map[string]*artifact.Artifact,
	obs Observer,
) {
	spec := sp.Stage.Spec()
	started := time.Now()

	sp.State = StateRunning
	sp.Progress = 0
	obs.StageEntered(ctx, sp)

	log := opts.Log.With("stage", spec.Name, "job_id", opts.JobID, "project_id", opts.ProjectID)

	env := &stage.Env{
		ProjectID:         opts.ProjectID,
		ProjectDir:        opts.Store.ProjectDir(),
		JobID:             opts.JobID,
		Project:           opts.Project,
		Services:          opts.Services,
		SourcePath:        opts.SourcePath,
		Media:             opts.Media,
		Worker:            opts.Worker,
		Models:            opts.Models,
		Config:            opts.Config,
		Inputs:            inputs,
		Artifacts:         opts.Store,
		Progress:          throttled(sp, obs, ctx),
		Log:               log,
		Clock:             stage.SystemClock{},
		SourceFingerprint: opts.SourceFingerprint,
		StartedAt:         started,
	}

	writer, err := opts.Store.Begin(artifact.KeyInputs{
		Stage:             spec.Name,
		StageVersion:      spec.Version,
		CodeRevision:      opts.CodeRevision,
		Config:            sp.Stage.ConfigSubtree(opts.Config),
		Fingerprint:       fingerprintOf(ctx, sp.Stage, env),
		UpstreamKeys:      keysOf(inputs),
		SourceFingerprint: sourceFor(sp.Stage, opts),
	})
	if err != nil {
		settle(sp, StateFailed, nil, err, started, obs, ctx)
		return
	}
	env.OutDir = writer.Dir()
	sp.Artifact = nil

	result, runErr := sp.Stage.Run(ctx, env)
	if runErr != nil {
		// Recorded on a context that is still live: the common case for a
		// failure is a cancellation, and a cancelled context cannot write the
		// row that tells the user the stage was cancelled.
		abortCtx := context.WithoutCancel(ctx)
		if abortErr := writer.Abort(abortCtx); abortErr != nil {
			log.Error("recording the failed artifact also failed", "error", abortErr)
		}
		settle(sp, failureState(ctx, runErr), nil, runErr, started, obs, ctx)
		return
	}

	// A stage that returns no result and no error is a bug in that stage, not a
	// reason to panic the process. Reporting it as a failed stage keeps the
	// blast radius to the one thing that is broken.
	if result == nil {
		if abortErr := writer.Abort(context.WithoutCancel(ctx)); abortErr != nil {
			log.Error("recording the failed artifact also failed", "error", abortErr)
		}
		settle(sp, StateFailed, nil, fmt.Errorf("stage %q returned no result and no error", spec.Name), started, obs, ctx)
		return
	}

	published, commitErr := writer.Commit(ctx, *result)
	if commitErr != nil {
		settle(sp, StateFailed, nil, commitErr, started, obs, ctx)
		return
	}

	sp.Artifact = published
	sp.Result = result
	sp.Progress = 1

	// Retention is housekeeping, not correctness, so a failure to prune must
	// not fail a stage that succeeded.
	if opts.Config.Artifact.Retention >= 0 {
		if _, gcErr := opts.Store.GC(ctx, spec.Name, opts.Config.Artifact.Retention); gcErr != nil {
			log.Warn("pruning old artifacts failed", "error", gcErr)
		}
	}

	settle(sp, StateCompleted, published, nil, started, obs, ctx)

	log.Info("stage completed",
		"duration_ms", time.Since(started).Milliseconds(),
		"artifact", published.ID,
		"size_bytes", published.SizeBytes)
}

// failureState distinguishes a cancellation from a failure.
//
// A user who cancels a job should not see it reported as a failure, and a
// retry should be offered in one case and not necessarily in the other.
func failureState(ctx context.Context, err error) State {
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return StateCancelled
	}
	return StateFailed
}

func settle(
	sp *StagePlan,
	state State,
	a *artifact.Artifact,
	err error,
	started time.Time,
	obs Observer,
	ctx context.Context,
) {
	sp.State = state
	sp.Artifact = a
	sp.Err = err
	sp.Duration = time.Since(started)
	if err == nil {
		sp.Progress = 1
	}
	obs.StageSettled(ctx, sp)
}

// throttled wraps the observer's progress callback, dropping updates that
// arrive too close together.
//
// The final update of a stage is always delivered, because a progress bar that
// stops at 93% looks stuck even though the stage finished.
func throttled(sp *StagePlan, obs Observer, ctx context.Context) func(float64, string) {
	var last time.Time
	return func(fraction float64, message string) {
		sp.Progress = fraction
		if fraction < 1 && time.Since(last) < progressInterval {
			return
		}
		last = time.Now()
		obs.StageProgress(ctx, sp, fraction, message)
	}
}

func keysOf(inputs map[string]*artifact.Artifact) map[string]string {
	out := make(map[string]string, len(inputs))
	for name, a := range inputs {
		if a != nil {
			out[name] = a.Key
		}
	}
	return out
}

// fingerprintOf re-derives a stage's fingerprint at execution time.
//
// The plan computed it too; recomputing keeps the published artifact's key
// derived from the same inputs the plan used, without threading the value
// through StagePlan. A fingerprint that fails here yields an empty map, and the
// stage's artifact is then keyed on a configuration that does not describe it —
// so the Run error path is what actually protects this.
func fingerprintOf(ctx context.Context, s stage.Stage, env *stage.Env) map[string]string {
	fingerprint, err := s.Fingerprint(ctx, env)
	if err != nil {
		env.Log.Warn("could not fingerprint stage inputs", "error", err)
		return nil
	}
	return fingerprint
}

func cancelRemaining(plan *Plan, obs Observer, ctx context.Context) {
	for _, sp := range plan.Stages {
		if sp.State == StatePending || sp.State == StateRunning {
			sp.State = StateCancelled
			sp.Reason = "cancelled"
			obs.StageSettled(ctx, sp)
		}
	}
}
