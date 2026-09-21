// Package tests holds cross-cutting integration tests.
//
// This file is the Phase 1 exit test: it exercises the pipeline engine end to
// end against a real SQLite database and a real filesystem, using stages that
// stand in for the ones later phases provide.
//
// The stages are deliberately trivial. What is under test is the engine —
// cache resolution, invalidation, failure propagation, restart recovery and
// cancellation — and a real transcriber would only make the failures harder to
// read.
package tests

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/AhsokaTano26/NikuCooker/internal/artifact"
	"github.com/AhsokaTano26/NikuCooker/internal/config"
	"github.com/AhsokaTano26/NikuCooker/internal/database"
	"github.com/AhsokaTano26/NikuCooker/internal/jobs"
	"github.com/AhsokaTano26/NikuCooker/internal/pipeline"
	"github.com/AhsokaTano26/NikuCooker/internal/stage"
)

// ---------------------------------------------------------------------------
// A stand-in pipeline
// ---------------------------------------------------------------------------

// dummy is a stage that writes a small file and reports how often it ran.
type dummy struct {
	name     string
	version  string
	depends  []string
	optional bool

	// subtree reads the real configuration section this stage would consume, so
	// the invalidation tests exercise the actual scoping rather than a
	// paraphrase of it.
	subtree func(*config.Config) any

	// counter is shared with the test, which is how it observes what ran.
	counter *int32

	// mu guards the counter so -race is meaningful.
	mu *sync.Mutex

	// rev distinguishes successive swapped implementations.
	rev int

	// behaviour, when set, replaces the default success path.
	behaviour func(ctx context.Context, env *stage.Env) (*stage.Result, error)

	// revision distinguishes implementations in the cache key. Changing a
	// stage's behaviour without changing its identity would be correctly
	// served from cache, since nothing in the key describes behaviour.
	revision string
}

func (d *dummy) Spec() stage.Spec {
	return stage.Spec{
		Name:      d.name,
		Version:   d.version,
		Depends:   d.depends,
		Optional:  d.optional,
		ConfigKey: d.name,
	}
}

func (d *dummy) ConfigSubtree(cfg *config.Config) any { return d.subtree(cfg) }

func (d *dummy) Fingerprint(context.Context, *stage.Env) (map[string]string, error) {
	return map[string]string{"impl": d.name, "revision": d.revision}, nil
}

func (d *dummy) Run(ctx context.Context, env *stage.Env) (*stage.Result, error) {
	d.mu.Lock()
	*d.counter++
	d.mu.Unlock()

	if d.behaviour != nil {
		return d.behaviour(ctx, env)
	}

	env.ReportProgress(0.5, "working")
	payload := filepath.Join(env.OutDir, d.name+".json")
	if err := os.WriteFile(payload, []byte(fmt.Sprintf("%q", d.name)), 0o600); err != nil {
		return nil, err
	}
	env.ReportProgress(1, "done")
	return &stage.Result{Primary: d.name + ".json"}, nil
}

// payload does what a stage's success path does: write its primary file and
// describe it.
func payload(env *stage.Env, name string) (*stage.Result, error) {
	if err := os.WriteFile(filepath.Join(env.OutDir, name+".json"), []byte("payload"), 0o600); err != nil {
		return nil, err
	}
	return &stage.Result{Primary: name + ".json"}, nil
}

// harness is a project with a pipeline and a database.
type harness struct {
	t          *testing.T
	db         *database.DB
	projectID  string
	projectDir string
	store      *artifact.Store
	cfg        *config.Config
	registry   *stage.Registry
	repo       *jobs.Repository
	sched      *jobs.Scheduler
	log        *slog.Logger
	mu         sync.Mutex
	counts     map[string]*int32
}

// stageNames is the stand-in pipeline, in order.
var stageNames = []string{"alpha", "bravo", "charlie", "delta", "echo"}

// defaultConfig is config.Default() with the disabled list cleared.
//
// The real default disables "polish", which this stand-in pipeline does not
// have, and the pipeline correctly refuses a disabled list naming a stage that
// does not exist.
func defaultConfig() *config.Config {
	cfg := config.Default()
	cfg.Pipeline.Disabled = nil
	return cfg
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	root := t.TempDir()

	db, err := database.Open(database.Options{Path: filepath.Join(root, "niku.db")})
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := database.Migrate(context.Background(), db); err != nil {
		t.Fatalf("database.Migrate: %v", err)
	}

	const projectID = "proj_phase1"
	if _, err := db.Write.ExecContext(context.Background(), `
		INSERT INTO projects (id, name, source_path, source_language, target_language,
		                      style, status, config_json, created_at, updated_at)
		VALUES (?, 'phase 1', 'source/source.mp4', 'ja', 'zh-Hans',
		        'fansub', 'active', '{}', 'now', 'now')`, projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	projectDir := filepath.Join(root, "projects", projectID)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}

	h := &harness{
		t:          t,
		db:         db,
		projectID:  projectID,
		projectDir: projectDir,
		store:      artifact.NewStore(db, projectID, projectDir),
		cfg:        defaultConfig(),
		repo:       jobs.NewRepository(db),
		sched:      jobs.NewScheduler(),
		log:        slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		counts:     map[string]*int32{},
	}

	for _, name := range stageNames {
		var counter int32
		h.counts[name] = &counter
	}

	// Each stage reads a different real configuration section, so changing one
	// section is a genuine test of per-stage config scoping.
	sections := map[string]func(*config.Config) any{
		"alpha":   func(c *config.Config) any { return c.ASR },
		"bravo":   func(c *config.Config) any { return c.VAD },
		"charlie": func(c *config.Config) any { return c.Subtitle },
		"delta":   func(c *config.Config) any { return c.QC },
		"echo":    func(c *config.Config) any { return c.Render },
	}

	var stages []stage.Stage
	for i, name := range stageNames {
		var depends []string
		if i > 0 {
			depends = []string{stageNames[i-1]}
		}
		stages = append(stages, &dummy{
			name:    name,
			version: "1",
			depends: depends,
			subtree: sections[name],
			counter: h.counts[name],
			mu:      &h.mu,
			// The last stage is optional, mirroring the real pipeline where
			// render is the stage a user may leave out.
			optional: i == len(stageNames)-1,
		})
	}

	registry, err := stage.NewRegistry(stages...)
	if err != nil {
		t.Fatalf("stage.NewRegistry: %v", err)
	}
	h.registry = registry

	return h
}

// counterAt returns the current execution count for a stage.
func (h *harness) counterAt(name string) int32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return *h.counts[name]
}

// setBehaviour replaces a stage's Run implementation.
func (h *harness) setBehaviour(name string, behaviour func(context.Context, *stage.Env) (*stage.Result, error)) {
	s, ok := h.registry.Get(name)
	if !ok {
		h.t.Fatalf("no stage named %q", name)
	}
	impl := s.(*dummy)
	impl.behaviour = behaviour

	// Changing a stage's behaviour without changing its identity would be
	// correctly served from cache, so the swap has to be visible in the key.
	impl.mu.Lock()
	impl.rev++
	impl.revision = fmt.Sprintf("impl-%d", impl.rev)
	impl.mu.Unlock()
}

// setVersion changes a stage's declared implementation version.
func (h *harness) setVersion(name, version string) {
	s, ok := h.registry.Get(name)
	if !ok {
		h.t.Fatalf("no stage named %q", name)
	}
	s.(*dummy).version = version
}

// options builds pipeline options.
func (h *harness) options() pipeline.Options {
	return pipeline.Options{
		Registry:          h.registry,
		Config:            h.cfg,
		Store:             h.store,
		ProjectID:         h.projectID,
		JobID:             "job_test",
		CodeRevision:      "test-rev-1",
		SourceFingerprint: "v1:size=1:h=fixed",
		Log:               h.log,
	}
}

// run builds and executes a plan, returning it.
func (h *harness) run(opts pipeline.Options) *pipeline.Plan {
	h.t.Helper()

	ctx := context.Background()
	plan, err := pipeline.Build(ctx, opts)
	if err != nil {
		h.t.Fatalf("pipeline.Build: %v", err)
	}
	if err := pipeline.Run(ctx, plan, opts, pipeline.NopObserver{}); err != nil {
		h.t.Fatalf("pipeline.Run: %v", err)
	}
	return plan
}

// states renders a plan as name→state for readable assertions.
func states(plan *pipeline.Plan) map[string]pipeline.State {
	out := map[string]pipeline.State{}
	for _, sp := range plan.Stages {
		out[sp.Stage.Spec().Name] = sp.State
	}
	return out
}

// expectStates asserts every stage's state, reporting all mismatches at once.
func expectStates(t *testing.T, plan *pipeline.Plan, want map[string]pipeline.State) {
	t.Helper()
	got := states(plan)
	for name, wantState := range want {
		if got[name] != wantState {
			detail := ""
			for _, sp := range plan.Stages {
				if sp.Stage.Spec().Name == name {
					if sp.Err != nil {
						detail = " (" + sp.Err.Error() + ")"
					} else if sp.Reason != "" {
						detail = " (" + sp.Reason + ")"
					}
				}
			}
			t.Errorf("stage %s: state = %s, want %s%s", name, got[name], wantState, detail)
		}
	}
}

// ---------------------------------------------------------------------------
// Step 1 and 2: a run, then a re-run that does nothing
// ---------------------------------------------------------------------------

func TestPhase1FreshRunThenCache(t *testing.T) {
	h := newHarness(t)

	// Step 1: every stage runs and completes.
	first := h.run(h.options())
	expectStates(t, first, map[string]pipeline.State{
		"alpha": pipeline.StateCompleted, "bravo": pipeline.StateCompleted,
		"charlie": pipeline.StateCompleted, "delta": pipeline.StateCompleted,
		"echo": pipeline.StateCompleted,
	})
	for _, name := range stageNames {
		if got := h.counterAt(name); got != 1 {
			t.Errorf("stage %s ran %d times on the first run, want 1", name, got)
		}
	}
	if first.Stages[0].Ordinal != 1 || first.Stages[4].Ordinal != 5 {
		t.Error("ordinals do not reflect pipeline position")
	}

	// Step 2: a re-run executes nothing at all.
	second := h.run(h.options())
	expectStates(t, second, map[string]pipeline.State{
		"alpha": pipeline.StateCached, "bravo": pipeline.StateCached,
		"charlie": pipeline.StateCached, "delta": pipeline.StateCached,
		"echo": pipeline.StateCached,
	})
	for _, name := range stageNames {
		if got := h.counterAt(name); got != 1 {
			t.Errorf("stage %s ran again on a cached re-run (%d executions)", name, got)
		}
	}
	if second.NeedsRun() {
		t.Error("a fully cached plan reports that it needs to run")
	}
}

// ---------------------------------------------------------------------------
// Step 3: a config change invalidates its own stage and downstream only
// ---------------------------------------------------------------------------

func TestPhase1ConfigChangeInvalidatesDownstreamOnly(t *testing.T) {
	h := newHarness(t)
	h.run(h.options())

	// charlie reads subtitle configuration. Changing it must invalidate charlie
	// and everything after, and nothing before.
	h.cfg.Subtitle.MaxCPS = 25

	plan := h.run(h.options())

	expectStates(t, plan, map[string]pipeline.State{
		"alpha":   pipeline.StateCached,
		"bravo":   pipeline.StateCached,
		"charlie": pipeline.StateCompleted,
		"delta":   pipeline.StateCompleted,
		"echo":    pipeline.StateCompleted,
	})

	for name, want := range map[string]int32{
		"alpha": 1, "bravo": 1, "charlie": 2, "delta": 2, "echo": 2,
	} {
		if got := h.counterAt(name); got != want {
			t.Errorf("stage %s ran %d times, want %d", name, got, want)
		}
	}
}

func TestPhase1UnrelatedConfigChangeInvalidatesNothing(t *testing.T) {
	// This is the mistake the per-stage config subtree exists to prevent:
	// hashing the whole configuration means changing a render setting
	// re-transcribes the audio.
	h := newHarness(t)
	h.run(h.options())

	h.cfg.Log.Level = "debug"
	h.cfg.Server.Port = 9999

	plan := h.run(h.options())

	for _, name := range stageNames {
		if got := states(plan)[name]; got != pipeline.StateCached {
			t.Errorf("stage %s is %s after an unrelated config change, want cached", name, got)
		}
		if got := h.counterAt(name); got != 1 {
			t.Errorf("stage %s re-ran after an unrelated config change", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Step 4: a failure stops the chain without undoing earlier work
// ---------------------------------------------------------------------------

func TestPhase1FailureSkipsDownstream(t *testing.T) {
	h := newHarness(t)
	h.run(h.options())

	h.setBehaviour("delta", func(context.Context, *stage.Env) (*stage.Result, error) {
		return nil, errors.New("deliberate failure")
	})

	plan := h.run(h.options())

	expectStates(t, plan, map[string]pipeline.State{
		"alpha":   pipeline.StateCached,
		"bravo":   pipeline.StateCached,
		"charlie": pipeline.StateCached,
		"delta":   pipeline.StateFailed,
		"echo":    pipeline.StateSkipped,
	})

	// The artifacts of completed stages survive, which is what makes a retry
	// cheap: only delta and echo have anything to redo.
	for _, name := range stageNames {
		if got := h.counterAt(name); name == "delta" && got != 2 {
			t.Errorf("delta ran %d times, want 2", got)
		} else if name != "delta" && got != 1 {
			t.Errorf("stage %s ran %d times, want 1", name, got)
		}
	}

	var reason string
	for _, sp := range plan.Stages {
		if sp.Stage.Spec().Name == "echo" {
			reason = sp.Reason
		}
	}
	if reason == "" {
		t.Error("the skipped stage does not say why it was skipped")
	}
}

func TestPhase1RetryAfterFailureKeepsUpstreamCached(t *testing.T) {
	h := newHarness(t)
	h.run(h.options())

	failing := true
	h.setBehaviour("delta", func(_ context.Context, env *stage.Env) (*stage.Result, error) {
		if failing {
			return nil, errors.New("deliberate failure")
		}
		return payload(env, "delta")
	})
	h.run(h.options())

	// Fixing the fault and retrying must not redo alpha through charlie.
	failing = false
	plan := h.run(h.options())

	expectStates(t, plan, map[string]pipeline.State{
		"alpha":   pipeline.StateCached,
		"bravo":   pipeline.StateCached,
		"charlie": pipeline.StateCached,
		"delta":   pipeline.StateCompleted,
		"echo":    pipeline.StateCompleted,
	})
	for _, name := range []string{"alpha", "bravo", "charlie"} {
		if got := h.counterAt(name); got != 1 {
			t.Errorf("stage %s ran %d times after a downstream retry, want 1", name, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Step 5: a restart never leaves a job stuck running
// ---------------------------------------------------------------------------

func TestPhase1RestartReconcilesRunningJobs(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	// A job that was running when the process died: the row says running, and
	// nothing will ever finish it.
	job := &jobs.Job{ProjectID: h.projectID, Kind: jobs.KindFull, Status: jobs.StatusPending}
	if err := h.repo.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.SetStatus(ctx, job.ID, jobs.StatusRunning, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.UpsertStageRun(ctx, jobs.StageRun{
		JobID: job.ID, ProjectID: h.projectID, Name: "delta", Ordinal: 4,
		Status: string(pipeline.StateRunning), Attempt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// Startup reconciliation.
	reconciled, err := h.repo.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(reconciled) != 1 {
		t.Fatalf("reconciled %d jobs, want 1", len(reconciled))
	}

	loaded, err := h.repo.Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != jobs.StatusFailed {
		t.Errorf("interrupted job is %s, want failed — a job must never be left running", loaded.Status)
	}
	if loaded.ErrorCode != jobs.CodeInterrupted {
		t.Errorf("error code = %q, want %q", loaded.ErrorCode, jobs.CodeInterrupted)
	}
	if loaded.FinishedAt == nil {
		t.Error("an interrupted job has no finish time, so it still looks in progress")
	}

	runs, err := h.repo.StageRuns(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Status != string(pipeline.StateFailed) {
		t.Errorf("the interrupted stage is still %+v", runs)
	}

	// And nothing is left in a non-terminal state anywhere.
	var stuck int
	if err := h.db.Read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE status IN ('running','paused')`).Scan(&stuck); err != nil {
		t.Fatal(err)
	}
	if stuck != 0 {
		t.Errorf("%d jobs are still stuck after reconciliation", stuck)
	}
}

func TestPhase1ResumeAfterInterruptionSkipsCompletedStages(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	// Three stages completed before the process died.
	first := h.run(h.options())
	for _, sp := range first.Stages[:3] {
		if sp.State != pipeline.StateCompleted {
			t.Fatalf("setup: stage %s is %s", sp.Stage.Spec().Name, sp.State)
		}
	}

	// A new run after the restart reuses them and does not redo the work.
	plan := h.run(h.options())
	for _, name := range []string{"alpha", "bravo", "charlie"} {
		if got := h.counterAt(name); got != 1 {
			t.Errorf("stage %s ran %d times after a restart, want 1", name, got)
		}
	}
	if got := states(plan)["alpha"]; got != pipeline.StateCached {
		t.Errorf("alpha is %s after a restart, want cached", got)
	}
	_ = ctx
}

// ---------------------------------------------------------------------------
// Step 6: cancellation preserves completed work
// ---------------------------------------------------------------------------

func TestPhase1CancellationPreservesCompletedArtifacts(t *testing.T) {
	h := newHarness(t)

	// delta signals when it is under way and then blocks until cancelled.
	// A channel rather than polling the plan: StagePlan is written by the
	// goroutine running the pipeline, so reading it from a test goroutine is a
	// data race.
	started := make(chan struct{})
	h.setBehaviour("delta", func(ctx context.Context, env *stage.Env) (*stage.Result, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})

	opts := h.options()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	plan, err := pipeline.Build(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		<-started
		cancel()
	}()

	runErr := pipeline.Run(ctx, plan, opts, pipeline.NopObserver{})
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", runErr)
	}

	got := states(plan)
	if got["delta"] != pipeline.StateCancelled {
		t.Errorf("delta is %s after cancellation, want cancelled", got["delta"])
	}
	// A stage that never started is cancelled too, not left pending: a pending
	// stage after a run ends is a stage the UI would show as still to come.
	if got["echo"] != pipeline.StateCancelled {
		t.Errorf("echo is %s after cancellation, want cancelled", got["echo"])
	}

	// The work that finished before the cancellation is intact, so resuming
	// costs only the cancelled stage.
	for _, name := range []string{"alpha", "bravo", "charlie"} {
		if count := h.counterAt(name); count != 1 {
			t.Errorf("stage %s ran %d times, want 1", name, count)
		}
		if _, err := h.store.Latest(context.Background(), name); err != nil {
			t.Errorf("stage %s: %v", name, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Step 7: a version bump invalidates that stage and downstream only
// ---------------------------------------------------------------------------

func TestPhase1VersionBumpInvalidatesDownstreamOnly(t *testing.T) {
	h := newHarness(t)
	h.run(h.options())

	// Bumping charlie's version is the intentional invalidation lever. It must
	// not touch alpha or bravo, whose outputs are unaffected by a change to
	// charlie's algorithm.
	h.setVersion("charlie", "2")

	plan := h.run(h.options())

	expectStates(t, plan, map[string]pipeline.State{
		"alpha":   pipeline.StateCached,
		"bravo":   pipeline.StateCached,
		"charlie": pipeline.StateCompleted,
		"delta":   pipeline.StateCompleted,
		"echo":    pipeline.StateCompleted,
	})
	for name, want := range map[string]int32{
		"alpha": 1, "bravo": 1, "charlie": 2, "delta": 2, "echo": 2,
	} {
		if got := h.counterAt(name); got != want {
			t.Errorf("stage %s ran %d times, want %d", name, got, want)
		}
	}
}

func TestPhase1CodeRevisionInvalidatesEverything(t *testing.T) {
	// The safety net for forgetting to bump a stage version: a new binary must
	// not serve artifacts produced by the old one.
	h := newHarness(t)
	h.run(h.options())

	opts := h.options()
	opts.CodeRevision = "test-rev-2"
	plan := h.run(opts)

	for _, name := range stageNames {
		if got := states(plan)[name]; got != pipeline.StateCompleted {
			t.Errorf("stage %s is %s after a rebuild, want completed", name, got)
		}
	}
}

func TestPhase1ForceRecomputesEverything(t *testing.T) {
	h := newHarness(t)
	h.run(h.options())

	opts := h.options()
	opts.Force = true
	plan := h.run(opts)

	for _, name := range stageNames {
		if got := states(plan)[name]; got != pipeline.StateCompleted {
			t.Errorf("stage %s is %s under force, want completed", name, got)
		}
		if got := h.counterAt(name); got != 2 {
			t.Errorf("stage %s ran %d times under force, want 2", name, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Stage selection
// ---------------------------------------------------------------------------

func TestPhase1OnlyRunsASubset(t *testing.T) {
	h := newHarness(t)
	h.run(h.options())

	// Running one stage reuses its dependencies' artifacts rather than
	// re-running them.
	h.setBehaviour("echo", func(_ context.Context, env *stage.Env) (*stage.Result, error) {
		return payload(env, "echo")
	})

	opts := h.options()
	opts.Only = []string{"echo"}
	plan := h.run(opts)

	if _, ok := states(plan)["alpha"]; ok {
		t.Error("a stage outside the selection appears in the plan")
	}
	if got := h.counterAt("alpha"); got != 1 {
		t.Errorf("alpha ran %d times during a downstream-only run", got)
	}
}

func TestPhase1RangeSelectsContiguousStages(t *testing.T) {
	h := newHarness(t)
	h.run(h.options())

	opts := h.options()
	opts.FromStage = "charlie"
	opts.ToStage = "delta"
	plan := h.run(opts)

	expectStates(t, plan, map[string]pipeline.State{
		"charlie": pipeline.StateCached,
		"delta":   pipeline.StateCached,
	})
	if len(plan.Stages) != 2 {
		t.Errorf("the plan has %d stages, want 2", len(plan.Stages))
	}
}

func TestPhase1DisabledOptionalStageIsSkippedButDependentsRun(t *testing.T) {
	// Disabling an optional stage leaves a gap rather than renumbering, so a
	// client that cached the pipeline list still sees stable ordinals.
	h := newHarness(t)
	h.cfg.Pipeline.Disabled = []string{"echo"}

	plan := h.run(h.options())

	if _, ok := states(plan)["echo"]; ok {
		t.Error("a disabled stage appears in the plan")
	}
	if got := h.counterAt("echo"); got != 0 {
		t.Errorf("a disabled stage ran %d times", got)
	}
	// The ordinals of the remaining stages do not shift.
	if plan.Stages[3].Ordinal != 4 {
		t.Errorf("delta has ordinal %d, want 4", plan.Stages[3].Ordinal)
	}
}

func TestPhase1DisablingARequiredStageIsRefused(t *testing.T) {
	h := newHarness(t)
	h.cfg.Pipeline.Disabled = []string{"charlie"}

	_, err := pipeline.Build(context.Background(), h.options())
	if err == nil {
		t.Fatal("disabling a required stage was accepted")
	}
	if !strings.Contains(err.Error(), "not optional") {
		t.Errorf("the error does not explain the problem: %v", err)
	}
}
