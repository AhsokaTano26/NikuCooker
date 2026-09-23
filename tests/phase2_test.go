package tests

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/worker"
	"github.com/AhsokaTano26/NikuCooker/pkg/protocol"
)

// The Phase 2 exit test.
//
// It spawns the real Python worker and exercises the failure modes that are
// invisible until production: a stream corrupted by a library write, a crash
// mid-request, a wedged process, and a request that stops making progress.
//
// These tests need a real interpreter, so they skip rather than fail when the
// environment is absent — a test that fails in CI because a dependency is not
// installed is a test that gets disabled.

// workerRepoRoot locates the repository root from the test's working directory.
func workerRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// pythonForTests returns the project virtualenv's interpreter, or skips.
//
// The search is done once per test binary. Each candidate probe runs the
// worker's own self-check, which imports ctranslate2 and onnxruntime from a
// cold filesystem — seconds, and the same seconds for every test that asks.
func pythonForTests(t *testing.T) string {
	t.Helper()

	pythonOnce.Do(func() { pythonPath, pythonWhy = findPythonForTests() })
	if pythonPath == "" {
		t.Skipf("no AI worker environment: %s; run `make ai-install` first", pythonWhy)
	}
	return pythonPath
}

var (
	pythonOnce sync.Once
	pythonPath string
	pythonWhy  string
)

// findPythonForTests returns the interpreter the integration tests should use,
// or an empty path and the reason there is none.
func findPythonForTests() (string, string) {
	root, err := filepath.Abs("..")
	if err != nil {
		return "", err.Error()
	}

	candidates := []string{
		filepath.Join(root, "ai", ".venv", "bin", "python"),
		filepath.Join(root, "ai", ".venv", "Scripts", "python.exe"),
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, ""
		}
	}

	// Fall back to a system interpreter only if it can actually run the worker.
	//
	// The probe is the worker's own self-check, which is the question the core
	// asks at startup — are the dependencies importable — and the only one that
	// answers it. Two weaker ones were tried and both let this fail instead of
	// skipping:
	//
	//	import nikucooker_ai           passes anywhere; the source directory is
	//	                               the working directory, so it is on
	//	                               sys.path.
	//	import nikucooker_ai.protocol  passes on a runner image that happens to
	//	                               ship pydantic, and then the tests fail on
	//	                               the next import down.
	//
	// A vendored copy of the check is not the point; the point is that a test
	// which cannot run says so, rather than failing and looking like a bug.
	system, err := exec.LookPath("python3")
	if err != nil {
		return "", "python3 is not on PATH either"
	}

	check := exec.Command(system, "-m", "nikucooker_ai", "--selfcheck")
	check.Dir = filepath.Join(root, "ai")
	output, err := check.Output()
	if err != nil {
		return "", fmt.Sprintf("the interpreter at %s cannot run the worker: %v", system, err)
	}
	if !bytes.Contains(output, []byte(`"ok":true`)) {
		return "", fmt.Sprintf("the interpreter at %s reports a broken environment", system)
	}
	return system, ""
}

// workerConfig builds a config pointing at the project's worker.
func workerConfig(t *testing.T) worker.Config {
	t.Helper()

	root := workerRepoRoot(t)
	digest, err := protocol.SchemaDigest()
	if err != nil {
		t.Fatalf("SchemaDigest: %v", err)
	}

	return worker.Config{
		Python:           pythonForTests(t),
		Args:             []string{"-m", "nikucooker_ai"},
		Dir:              filepath.Join(root, "ai"),
		SchemaDigest:     digest,
		CodeRevision:     "test",
		StartupTimeout:   60 * time.Second,
		StallTimeout:     30 * time.Second,
		ShutdownTimeout:  5 * time.Second,
		ModelLoadTimeout: 30 * time.Second,
		Log:              slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}
}

func startWorker(t *testing.T, cfg worker.Config) *worker.Worker {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	w, err := worker.Start(ctx, cfg)
	if err != nil {
		t.Fatalf("worker.Start: %v", err)
	}
	t.Cleanup(func() { _ = w.Kill() })
	return w
}

// ---------------------------------------------------------------------------
// Handshake
// ---------------------------------------------------------------------------

func TestPhase2Handshake(t *testing.T) {
	w := startWorker(t, workerConfig(t))

	if got := w.State(); got != worker.StateReady {
		t.Fatalf("state after Start = %s, want ready", got)
	}

	ready := w.Ready()
	if ready == nil {
		t.Fatal("no ready payload")
	}
	if ready.Worker != protocol.WorkerName {
		t.Errorf("worker = %q, want %q", ready.Worker, protocol.WorkerName)
	}
	// The digest in the handshake is what the core compared to decide to start
	// at all, so a mismatch here means the comparison was skipped.
	digest, err := protocol.SchemaDigest()
	if err != nil {
		t.Fatal(err)
	}
	if ready.SchemaDigest != digest {
		t.Errorf("worker digest = %s, core digest = %s", ready.SchemaDigest, digest)
	}
	if !ready.Protocol.Supports(protocol.Version) {
		t.Errorf("worker speaks %v, this build speaks %d", ready.Protocol, protocol.Version)
	}
	if ready.Python == "" || ready.Platform == "" {
		t.Error("handshake does not describe the interpreter")
	}
}

func TestPhase2SchemaMismatchRefusesToStart(t *testing.T) {
	// The usual cause is an upgraded binary with a stale environment, and it
	// has to produce one precise message rather than a stream of
	// deserialisation errors.
	cfg := workerConfig(t)
	cfg.SchemaDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	w, err := worker.Start(ctx, cfg)
	if err == nil {
		_ = w.Kill()
		t.Fatal("a schema mismatch was accepted")
	}
	if !errors.Is(err, worker.ErrSchemaMismatch) {
		t.Fatalf("error = %v, want ErrSchemaMismatch", err)
	}
	// The message must name both digests and the fix, or it is a support ticket.
	for _, want := range []string{"sha256:", "uv sync"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestPhase2StartupTimeoutQuotesStderr(t *testing.T) {
	// A worker that never announces itself is almost always an import error
	// whose traceback went to stderr, so the error has to carry it.
	cfg := workerConfig(t)
	cfg.Python = pythonForTests(t)
	cfg.Args = []string{"-c", "import sys, time; sys.stderr.write('ImportError: no module named x\\n'); time.sleep(60)"}
	cfg.StartupTimeout = 2 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	w, err := worker.Start(ctx, cfg)
	elapsed := time.Since(start)

	if err == nil {
		_ = w.Kill()
		t.Fatal("a worker that never announces ready was accepted")
	}
	if elapsed > 20*time.Second {
		t.Fatalf("startup took %s; the timeout did not fire", elapsed)
	}
	if !strings.Contains(err.Error(), "ImportError") {
		t.Errorf("error does not quote the worker's stderr:\n%v", err)
	}
}

// ---------------------------------------------------------------------------
// Requests
// ---------------------------------------------------------------------------

func TestPhase2EchoRoundTrip(t *testing.T) {
	w := startWorker(t, workerConfig(t))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	raw, err := w.Call(ctx, protocol.MethodEcho,
		map[string]any{"hello": "world", "unicode": "こんにちは"}, nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(string(raw), "こんにちは") {
		t.Errorf("the round trip lost non-ASCII text: %s", raw)
	}

	result, err := worker.CallTyped[protocol.HealthResult](ctx, w, protocol.MethodHealth, nil, nil)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if !result.Alive {
		t.Error("health reports the worker is not alive")
	}
}

func TestPhase2ProgressReachesTheCaller(t *testing.T) {
	w := startWorker(t, workerConfig(t))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var updates int
	_, err := w.Call(ctx, "debug.delay", map[string]any{"seconds": 1.0}, func(e protocol.ProgressEvent) {
		updates++
		if e.Progress < 0 || e.Progress > 1 {
			t.Errorf("progress %v is outside 0–1", e.Progress)
		}
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	// Progress is also the core's liveness signal, so a request that runs
	// without emitting any would be killed as stalled in production.
	if updates == 0 {
		t.Error("no progress arrived during a one-second request")
	}
}

func TestPhase2UnknownMethodIsNamed(t *testing.T) {
	w := startWorker(t, workerConfig(t))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A method that does not exist, rather than one that does not exist *yet*:
	// naming a real-but-unimplemented method here makes this test expire the
	// moment that method lands, which it did once.
	_, err := w.Call(ctx, "definitely.not.a.method", map[string]any{}, nil)
	if err == nil {
		t.Fatal("an unknown method returned success")
	}

	// The worker must name what is missing rather than pretending or crashing.
	if !strings.Contains(err.Error(), "UNSUPPORTED_METHOD") && !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("error does not identify the missing capability: %v", err)
	}
	if w.State() != worker.StateReady {
		t.Errorf("an unknown method left the worker in state %s", w.State())
	}
}

// ---------------------------------------------------------------------------
// Cancellation
// ---------------------------------------------------------------------------

func TestPhase2CancelStopsARunningRequest(t *testing.T) {
	w := startWorker(t, workerConfig(t))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// A request that would run far longer than the test.
	done := make(chan error, 1)
	go func() {
		_, err := w.Call(ctx, "debug.delay", map[string]any{"seconds": 120.0}, nil)
		done <- err
	}()

	// Let it get under way, then cancel through the pool's public path.
	time.Sleep(300 * time.Millisecond)
	if err := w.Cancel(ctx, "req_1"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the cancelled request reported success")
		}
		if !strings.Contains(err.Error(), protocol.CodeCancelled) {
			t.Errorf("error = %v, want CANCELLED", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the request was not cancelled")
	}

	// And the worker is still usable afterwards, which is the point of
	// cancelling cooperatively.
	if w.State() != worker.StateReady {
		t.Errorf("state after cancel = %s, want ready", w.State())
	}
	if _, err := w.Call(ctx, protocol.MethodEcho, map[string]any{"ok": true}, nil); err != nil {
		t.Errorf("the worker is unusable after a cancellation: %v", err)
	}
}

func TestPhase2CancelOfAFinishedRequestIsNotAnError(t *testing.T) {
	// Losing the race is a normal outcome, not a failure: reporting it as an
	// error would make a routine case look like a fault.
	w := startWorker(t, workerConfig(t))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := w.Call(ctx, protocol.MethodEcho, map[string]any{}, nil); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if err := w.Cancel(ctx, "req_1"); err != nil {
		t.Errorf("cancelling a finished request returned an error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Crash and recovery
// ---------------------------------------------------------------------------

func TestPhase2CrashFailsTheInFlightRequest(t *testing.T) {
	// The failure mode this guards against is a crash that looks like a hang:
	// the request never completes and nothing explains why.
	cfg := workerConfig(t)
	w := startWorker(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := w.Call(ctx, "debug.delay", map[string]any{"seconds": 120.0}, nil)
		done <- err
	}()

	// Let the request get under way, then kill the process underneath it.
	time.Sleep(500 * time.Millisecond)
	if err := w.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a request survived the worker being killed")
		}
		if !errors.Is(err, worker.ErrCrashed) {
			t.Fatalf("error = %v, want ErrCrashed", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the request hung instead of failing when the worker died")
	}

	if w.State() != worker.StateStopped && w.State() != worker.StateCrashed {
		t.Errorf("state after a kill = %s", w.State())
	}
}

func TestPhase2PoolStartsAFreshWorkerAfterACrash(t *testing.T) {
	// A crash must cost the in-flight request, not the pool.
	pool := worker.NewPool(workerConfig(t), 1)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	first, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := first.Call(ctx, protocol.MethodEcho, map[string]any{"n": 1}, nil); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := first.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	// Returned to the pool in a dead state; the pool must discard it rather
	// than hand it to the next caller as a trap.
	pool.Release(first)

	second, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire after a crash: %v", err)
	}
	defer pool.Release(second)

	if second.Pid() == first.Pid() {
		t.Fatal("the pool handed back the dead worker")
	}
	if _, err := second.Call(ctx, protocol.MethodEcho, map[string]any{"n": 2}, nil); err != nil {
		t.Fatalf("the replacement worker is unusable: %v", err)
	}
}

func TestPhase2StalledRequestIsKilledNotWaitedOn(t *testing.T) {
	// A request that stops reporting progress is either wedged or doing
	// something the core cannot see. Either way, waiting forever is worse than
	// failing: the alternative is a job that never ends and never explains.
	cfg := workerConfig(t)
	cfg.StallTimeout = 500 * time.Millisecond

	w := startWorker(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	start := time.Now()
	_, err := w.Call(ctx, "debug.delay", map[string]any{"seconds": 60.0, "silent": true}, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a silent request was allowed to run to completion")
	}
	if !errors.Is(err, worker.ErrStalled) {
		t.Fatalf("error = %v, want ErrStalled", err)
	}
	if elapsed > 30*time.Second {
		t.Errorf("stall detection took %s; it did not fire at the configured timeout", elapsed)
	}
}

func TestPhase2RestartBudgetLatches(t *testing.T) {
	// A worker that cannot start is a broken environment. Retrying forever
	// spins, fills the log, and fails every job anyway, so the pool stops and
	// keeps the diagnosis.
	cfg := workerConfig(t)
	cfg.Python = pythonForTests(t)
	cfg.Args = []string{"-c", "import sys; sys.exit(3)"}
	cfg.StartupTimeout = 5 * time.Second

	pool := worker.NewPool(cfg, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	var lastErr error
	for range worker.RestartBudget + 2 {
		if _, err := pool.Acquire(ctx); err != nil {
			lastErr = err
			continue
		}
	}

	if lastErr == nil {
		t.Fatal("acquiring from a pool that can never start a worker succeeded")
	}
	if !errors.Is(lastErr, worker.ErrBudgetExhausted) {
		t.Fatalf("error = %v, want ErrBudgetExhausted", lastErr)
	}
	if pool.Latched() == nil {
		t.Error("the pool did not latch after exhausting its restart budget")
	}

	// A latched pool fails immediately rather than spawning another process.
	start := time.Now()
	if _, err := pool.Acquire(ctx); err == nil {
		t.Error("a latched pool started a worker")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("a latched pool took %s to refuse", elapsed)
	}

	// ...and recovers once the environment is fixed.
	pool.Reset()
	if pool.Latched() != nil {
		t.Error("Reset did not clear the latched error")
	}
}

// ---------------------------------------------------------------------------
// Shutdown
// ---------------------------------------------------------------------------

func TestPhase2ShutdownIsClean(t *testing.T) {
	cfg := workerConfig(t)
	root := workerRepoRoot(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	w, err := worker.Start(ctx, cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	pid := w.Pid()
	if pid == 0 {
		t.Fatal("the worker has no pid")
	}

	if err := w.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if w.State() != worker.StateStopped {
		t.Errorf("state after shutdown = %s, want stopped", w.State())
	}

	// The process must actually be gone, not merely unreferenced: an orphan
	// holding a loaded model is several gigabytes of memory nobody accounts for.
	assertProcessGone(t, pid, root)
}

// assertProcessGone verifies a pid is no longer running.
func assertProcessGone(t *testing.T, pid int, _ string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("process %d is still running", pid)
}

func processAlive(pid int) bool {
	if runtime.GOOS == "windows" {
		out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH").Output()
		return err == nil && strings.Contains(string(out), fmt.Sprint(pid))
	}
	// Signal 0 performs the permission and existence checks without delivering
	// anything.
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 performs the existence and permission checks without delivering
	// anything.
	return process.Signal(syscall.Signal(0)) == nil
}
