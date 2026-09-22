// Package pipeline turns a registry of stages into a run.
//
// It computes a plan first — deciding for each stage whether a valid artifact
// already exists — and only then executes the stages that need it. Separating
// the two means a run's "what will happen" is answerable before anything is
// spent, which is what the API returns on `POST /run` and what lets a user see
// that a retry will skip nine of eleven stages.
package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/artifact"
	"github.com/AhsokaTano26/NikuCooker/internal/config"
	"github.com/AhsokaTano26/NikuCooker/internal/media"
	"github.com/AhsokaTano26/NikuCooker/internal/stage"
)

// State is a stage's state within a plan or a run.
type State string

const (
	StatePending   State = "pending"
	StateRunning   State = "running"
	StateCached    State = "cached"
	StateCompleted State = "completed"
	StateFailed    State = "failed"
	StateSkipped   State = "skipped"
	StateCancelled State = "cancelled"
)

// Terminal reports whether nothing further will happen to a stage in this run.
func (s State) Terminal() bool {
	switch s {
	case StateCached, StateCompleted, StateFailed, StateSkipped, StateCancelled:
		return true
	default:
		return false
	}
}

// Succeeded reports whether the stage's output is usable by its dependents.
//
// Cached and completed are equivalent downstream: both mean this artifact is
// authoritative. They stay distinct so a user can see whether a re-run actually
// recomputed anything.
func (s State) Succeeded() bool {
	return s == StateCached || s == StateCompleted
}

// StagePlan is one stage's place in a run.
type StagePlan struct {
	Stage stage.Stage

	// Ordinal is the position in the full pipeline definition, one-based.
	//
	// It comes from the definition rather than from the index in this slice, so
	// that disabling an optional stage leaves a gap instead of renumbering
	// everything after it — which would break a client that cached the list.
	Ordinal int

	// Key is the artifact key this stage produces.
	//
	// It is known for a pending stage as well as a cached one: keys are derived
	// in pipeline order, so by the time a stage is planned every one of its
	// inputs has a key whether or not it has run yet. That is what lets a
	// downstream stage's key be computed before its upstream stages execute.
	Key string

	State    State
	Artifact *artifact.Artifact
	Reason   string
	Err      error

	// externalInputs holds artifacts for dependencies that are not part of this
	// run, resolved from the store — the range-selection case.
	externalInputs map[string]*artifact.Artifact

	// Progress is 0..1 within this stage.
	Progress float64
	Duration time.Duration
	Result   *stage.Result
	Attempt  int
}

// Plan is a computed run.
type Plan struct {
	Stages []*StagePlan
}

// Options configures a plan.
type Options struct {
	Registry  *stage.Registry
	Config    *config.Config
	Store     *artifact.Store
	ProjectID string
	JobID     string

	// Project is handed to every stage. It carries the language pair and style,
	// which stages need and which are not configuration.
	Project stage.ProjectInfo

	// Services are the database-backed collaborators stages may use. They are
	// built by the caller because building them needs the database, which a
	// stage cannot reach.
	Services stage.Services

	// CodeRevision participates in every artifact key. It is what stops a
	// rebuilt binary from serving output produced by an older implementation.
	CodeRevision string

	// SourceFingerprint identifies the source media. Computed once by the
	// caller, because it is read from disk and every stage that needs it would
	// otherwise read it again.
	SourceFingerprint string

	// SourcePath is the absolute path to the project's source media.
	SourcePath string

	// Media is the FFmpeg service handed to stages.
	Media *media.Service

	// Worker runs inference requests.
	Worker stage.WorkerPool

	// Models resolves model names to on-disk directories.
	Models stage.ModelLocator

	// Force ignores the artifact cache and recomputes everything selected.
	Force bool

	// Only restricts the run to these stages. Their dependencies must already
	// have artifacts; they are not run implicitly.
	Only []string

	// FromStage and ToStage restrict the run to an inclusive range by pipeline
	// order.
	FromStage string
	ToStage   string

	Log *slog.Logger
}

// Build computes the plan.
func Build(ctx context.Context, opts Options) (*Plan, error) {
	if opts.Registry == nil {
		return nil, fmt.Errorf("pipeline: no stage registry")
	}
	if opts.Store == nil {
		return nil, fmt.Errorf("pipeline: no artifact store")
	}
	if opts.Config == nil {
		return nil, fmt.Errorf("pipeline: no configuration")
	}

	selected, err := selectStages(opts)
	if err != nil {
		return nil, err
	}

	plan := &Plan{Stages: make([]*StagePlan, 0, len(selected))}
	byName := make(map[string]*StagePlan, len(selected))

	for _, entry := range selected {
		sp := &StagePlan{
			Stage:   entry.stage,
			Ordinal: entry.ordinal,
			State:   StatePending,
			Attempt: 1,
		}
		plan.Stages = append(plan.Stages, sp)
		byName[entry.stage.Spec().Name] = sp

		// A stage whose dependency did not succeed cannot run. Marking it
		// skipped — rather than leaving it pending — is what makes the plan
		// honest about what a run will actually do.
		if reason, blocked := blockedBy(entry.stage, byName, opts.Only); blocked {
			sp.State = StateSkipped
			sp.Reason = reason
			continue
		}

		_, external, err := resolveInputs(ctx, entry.stage, byName, opts.Store)
		if err != nil {
			return nil, err
		}
		sp.externalInputs = external

		// A dependency with no artifact is only a problem when it will never
		// produce one. A dependency that is still pending will run before this
		// stage does, so it must not skip the dependent — treating "pending" as
		// "missing" skips every stage after the first one that needs to run.
		if missing := unsatisfied(entry.stage, byName, external); missing != "" {
			sp.State = StateSkipped
			sp.Reason = fmt.Sprintf("no artifact for input %q", missing)
			continue
		}

		inputs, err := artifactsFor(entry.stage, byName, external)
		if err != nil {
			return nil, err
		}

		key, err := keyFor(ctx, entry.stage, opts, inputs, upstreamKeys(entry.stage, byName, external))
		if err != nil {
			return nil, err
		}
		sp.Key = key

		if opts.Force {
			sp.State = StatePending
			continue
		}

		cached, err := opts.Store.Lookup(ctx, key)
		if err != nil {
			return nil, err
		}
		if cached != nil {
			sp.State = StateCached
			sp.Artifact = cached
			sp.Progress = 1
			continue
		}
		sp.State = StatePending
	}

	return plan, nil
}

// keyFor derives a stage's artifact key from its inputs and configuration.
func keyFor(
	ctx context.Context,
	s stage.Stage,
	opts Options,
	inputs map[string]*artifact.Artifact,
	upstream map[string]string,
) (string, error) {
	spec := s.Spec()

	env := &stage.Env{
		ProjectID:         opts.ProjectID,
		ProjectDir:        opts.Store.ProjectDir(),
		JobID:             opts.JobID,
		SourcePath:        opts.SourcePath,
		Media:             opts.Media,
		Worker:            opts.Worker,
		Models:            opts.Models,
		Config:            opts.Config,
		Inputs:            inputs,
		Artifacts:         opts.Store,
		SourceFingerprint: opts.SourceFingerprint,
		Clock:             stage.SystemClock{},
	}

	// A fingerprint failure is fatal to the plan: a key that silently omits an
	// input would describe work other than what will run, and the cache would
	// then serve the wrong artifact.
	fingerprint, err := s.Fingerprint(ctx, env)
	if err != nil {
		return "", fmt.Errorf("pipeline: stage %q: fingerprint: %w", spec.Name, err)
	}

	key, _, _, err := artifact.DeriveKey(artifact.KeyInputs{
		Stage:             spec.Name,
		StageVersion:      spec.Version,
		CodeRevision:      opts.CodeRevision,
		Config:            s.ConfigSubtree(opts.Config),
		Fingerprint:       fingerprint,
		UpstreamKeys:      upstream,
		SourceFingerprint: sourceFor(s, opts),
	})
	if err != nil {
		return "", fmt.Errorf("pipeline: stage %q: %w", spec.Name, err)
	}
	return key, nil
}

// upstreamKeys maps each dependency to the key its artifact has or will have.
//
// A dependency already planned contributes the key derived when it was planned,
// whether it is cached or still pending. Deriving keys in pipeline order is
// what makes a downstream key computable before its upstream stages have run.
func upstreamKeys(s stage.Stage, planned map[string]*StagePlan, external map[string]*artifact.Artifact) map[string]string {
	out := make(map[string]string, len(s.Spec().Depends))

	for _, dep := range s.Spec().Depends {
		if dp, ok := planned[dep]; ok {
			if dp.Key != "" {
				out[dep] = dp.Key
			} else if dp.Artifact != nil {
				out[dep] = dp.Artifact.Key
			}
			continue
		}
		if a := external[dep]; a != nil {
			out[dep] = a.Key
		}
	}
	return out
}

// artifactsFor collects the artifacts a stage's Fingerprint may inspect.
//
// A pending dependency has no artifact yet, so it is absent rather than nil:
// a stage that fingerprints on an input's *presence* gets a truthful answer
// instead of a nil pointer it has to guard.
func artifactsFor(
	s stage.Stage,
	planned map[string]*StagePlan,
	external map[string]*artifact.Artifact,
) (map[string]*artifact.Artifact, error) {
	out := make(map[string]*artifact.Artifact, len(s.Spec().Depends))

	for _, dep := range s.Spec().Depends {
		if dp, ok := planned[dep]; ok {
			if dp.Artifact != nil {
				out[dep] = dp.Artifact
			}
			continue
		}
		if a := external[dep]; a != nil {
			out[dep] = a
		}
	}
	return out, nil
}

// sourceFor returns the source fingerprint for stages that read the media.
//
// A stage that declares no dependency on "probe" is taken not to read the
// source. That is a convention rather than a declaration, but it is the only
// one every stage already follows, and getting it wrong costs a cache miss
// rather than a wrong artifact.
func sourceFor(s stage.Stage, opts Options) string {
	for _, dep := range s.Spec().Depends {
		if dep == "probe" {
			return opts.SourceFingerprint
		}
	}
	return ""
}

// blockedBy reports whether a dependency that *is* part of this run has failed
// to produce an artifact.
//
// A dependency absent from the plan is not blocking: a range or single-stage
// run deliberately leaves upstream stages out, and their artifacts are resolved
// from the store. Whether one is genuinely missing is unsatisfied's question,
// not this one's.
// Optional dependencies are deliberately not consulted. A stage that uses
// another's output when it is available must still run when that stage failed:
// translating without the analysis document is worse, not impossible, and
// failing translation because an unrelated stage broke would be wrong.
func blockedBy(s stage.Stage, planned map[string]*StagePlan, _ []string) (string, bool) {
	for _, dep := range s.Spec().Depends {
		dp, ok := planned[dep]
		if !ok {
			continue
		}
		switch dp.State {
		case StateSkipped:
			return fmt.Sprintf("dependency %q was skipped", dep), true
		case StateFailed, StateCancelled:
			return fmt.Sprintf("dependency %q %s", dep, dp.State), true
		}
		// Pending or running: its outcome is not yet known, and the executor
		// re-checks before this stage starts.
	}
	return "", false
}

// resolveInputs collects the artifacts a stage consumes.
//
// It returns two maps: artifacts already known from the plan, and artifacts
// resolved from the store for dependencies that are not part of this run.
//
// The second case is what makes a range run work. Selecting `charlie..echo`
// leaves bravo out of the plan, and charlie still needs bravo's output — which
// exists, from an earlier run, and is exactly what a partial re-run is for.
func resolveInputs(
	ctx context.Context,
	s stage.Stage,
	planned map[string]*StagePlan,
	store *artifact.Store,
) (planInputs, external map[string]*artifact.Artifact, err error) {
	spec := s.Spec()
	deps := append(append([]string(nil), spec.Depends...), spec.OptionalDepends...)
	planInputs = make(map[string]*artifact.Artifact, len(deps))
	external = make(map[string]*artifact.Artifact, len(deps))

	for _, dep := range deps {
		if dp, ok := planned[dep]; ok {
			if dp.Artifact != nil {
				planInputs[dep] = dp.Artifact
			}
			// Otherwise it is still to run; the executor supplies it.
			continue
		}

		// Not in this run: fall back to whatever the stage last produced.
		latest, lookupErr := store.Latest(ctx, dep)
		if lookupErr != nil {
			return nil, nil, fmt.Errorf("pipeline: resolve input %q for stage %q: %w", dep, s.Spec().Name, lookupErr)
		}
		if latest != nil {
			external[dep] = latest
		}
	}
	return planInputs, external, nil
}

// unsatisfied names a dependency that will never produce an artifact for this
// stage, or "" if all of them will.
//
// A dependency that is merely pending is not unsatisfied: it will run before
// this stage does.
// Optional dependencies are never unsatisfied. Their absence is a state the
// depending stage is written to handle, so reporting it here would turn a
// supported configuration into a skipped stage.
func unsatisfied(s stage.Stage, planned map[string]*StagePlan, external map[string]*artifact.Artifact) string {
	for _, dep := range s.Spec().Depends {
		dp, inPlan := planned[dep]
		if !inPlan {
			// Outside this run, so it must already have an artifact.
			if external[dep] == nil {
				return dep
			}
			continue
		}
		// Terminal without an artifact means it will never supply one.
		if dp.State.Terminal() && dp.Artifact == nil {
			return dep
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Stage selection
// ---------------------------------------------------------------------------

type selectedStage struct {
	stage   stage.Stage
	ordinal int
}

// selectStages resolves which stages a run covers.
func selectStages(opts Options) ([]selectedStage, error) {
	all := opts.Registry.Ordered()

	disabled, err := disabledSet(opts.Registry, opts.Config)
	if err != nil {
		return nil, err
	}

	byName := make(map[string]stage.Stage, len(all))
	for _, s := range all {
		byName[s.Spec().Name] = s
	}

	// An explicit stage list overrides the range fields; the API rejects
	// supplying both, and this is the second line of defence.
	if len(opts.Only) > 0 {
		var out []selectedStage
		seen := map[string]bool{}
		for _, name := range opts.Only {
			s, ok := byName[name]
			if !ok {
				return nil, fmt.Errorf("pipeline: no stage named %q", name)
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, selectedStage{stage: s, ordinal: ordinalOf(all, name)})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ordinal < out[j].ordinal })
		return out, nil
	}

	from, to := 1, len(all)
	if opts.FromStage != "" {
		s, ok := byName[opts.FromStage]
		if !ok {
			return nil, fmt.Errorf("pipeline: no stage named %q", opts.FromStage)
		}
		from = ordinalOf(all, s.Spec().Name)
	}
	if opts.ToStage != "" {
		s, ok := byName[opts.ToStage]
		if !ok {
			return nil, fmt.Errorf("pipeline: no stage named %q", opts.ToStage)
		}
		to = ordinalOf(all, s.Spec().Name)
	}
	if from > to {
		return nil, fmt.Errorf("pipeline: from_stage %q comes after to_stage %q", opts.FromStage, opts.ToStage)
	}

	var out []selectedStage
	for i, s := range all {
		ordinal := i + 1
		if disabled[s.Spec().Name] {
			continue
		}
		if ordinal < from || ordinal > to {
			continue
		}
		out = append(out, selectedStage{stage: s, ordinal: ordinal})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("pipeline: the selected range contains no enabled stages")
	}
	return out, nil
}

// disabledSet validates the disabled list against the registry.
//
// Disabling a required stage would leave its dependents without an input, so it
// is refused here rather than producing a run that quietly cannot finish.
func disabledSet(registry *stage.Registry, cfg *config.Config) (map[string]bool, error) {
	disabled := map[string]bool{}
	if len(cfg.Pipeline.Disabled) == 0 {
		return disabled, nil
	}

	optional := map[string]bool{}
	for _, name := range registry.OptionalNames() {
		optional[name] = true
	}

	for _, name := range cfg.Pipeline.Disabled {
		if _, ok := registry.Get(name); !ok {
			return nil, fmt.Errorf("pipeline.disabled: no stage named %q (optional stages: %s)",
				name, strings.Join(registry.OptionalNames(), ", "))
		}
		if !optional[name] {
			return nil, fmt.Errorf(
				"pipeline.disabled: %q is not optional; disabling it would leave its dependents without an input",
				name)
		}
		disabled[name] = true
	}
	return disabled, nil
}

func ordinalOf(all []stage.Stage, name string) int {
	for i, s := range all {
		if s.Spec().Name == name {
			return i + 1
		}
	}
	return 0
}

// NeedsRun reports whether the plan will execute anything.
//
// A user who re-runs and gets "everything cached" has learned something useful;
// this is what lets the API say so before starting a job.
func (p *Plan) NeedsRun() bool {
	for _, sp := range p.Stages {
		if sp.State == StatePending {
			return true
		}
	}
	return false
}

// Pending returns the stages that will execute, in order.
func (p *Plan) Pending() []*StagePlan {
	var out []*StagePlan
	for _, sp := range p.Stages {
		if sp.State == StatePending {
			out = append(out, sp)
		}
	}
	return out
}
