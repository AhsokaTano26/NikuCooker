// Package provision installs the AI worker's Python environment.
//
// A release archive carries the worker's source and a copy of uv, but not
// Python and not the dependencies — that would add several hundred megabytes to
// every download for a user who may never run a job. Instead the environment is
// built on the user's machine, the first time they ask for it, from the uv.lock
// that shipped with their binary.
//
// The work is two uv commands and a self-check. What makes it a package rather
// than a function is that it takes minutes, can be cancelled halfway, and has
// to answer "what is happening" the whole time. See docs/platform.md §10.
package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/platform"
)

// Phase is a step of provisioning.
//
// Phases, and never a percentage. uv exposes no machine-readable progress —
// what it writes is a terminal animation — so any fraction offered here would
// be invented, and an invented denominator moves backwards. The interface
// renders this list and the current step, which is true.
type Phase string

const (
	PhaseDetect       Phase = "detect"
	PhaseInterpreter  Phase = "interpreter"
	PhaseDependencies Phase = "dependencies"
	PhaseVerify       Phase = "verify"
)

// Phases lists the steps in order, for a caller that wants to render them all
// rather than only the one running.
func Phases() []Phase {
	return []Phase{PhaseDetect, PhaseInterpreter, PhaseDependencies, PhaseVerify}
}

// Status is where a provisioning run is.
type Status string

const (
	StatusIdle      Status = "idle"
	StatusRunning   Status = "running"
	StatusReady     Status = "ready"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// Terminal reports whether a status is an ending.
func (s Status) Terminal() bool {
	return s == StatusReady || s == StatusFailed || s == StatusCancelled
}

// Errors a caller may want to branch on.
var (
	// ErrBusy reports that a run is already in progress.
	ErrBusy = errors.New("provision: an install is already running")

	// ErrNoUV reports that no copy of uv could be found.
	ErrNoUV = errors.New("provision: no uv was found beside the program or on PATH")

	// ErrNoSpace reports too little free space to start.
	ErrNoSpace = errors.New("provision: not enough free space")
)

// MinFreeBytes is what provisioning refuses to start without.
//
// CPython is around 60 MB, the dependency set around 400 MB, and uv's cache
// holds a second copy of most of it. The floor is comfortably above the sum
// because running out *during* the install is the failure that leaves a
// half-built environment behind, and that costs the user another full download
// to recover from.
const MinFreeBytes = 2 << 30

// Progress is one phase transition.
type Progress struct {
	Phase   Phase
	Status  Status
	Message string
	Err     error
}

// Request is one run.
type Request struct {
	// AIDir is the directory holding pyproject.toml, uv.lock and the
	// nikucooker_ai package. Must be absolute: uv records it in the editable
	// install, and a relative path there breaks the environment as soon as the
	// process runs from somewhere else.
	AIDir string

	// RuntimeDir is where everything is installed. Must be absolute.
	RuntimeDir string

	// UV is the uv executable to run.
	UV string

	// Python is the interpreter version to install, e.g. "3.12".
	Python string

	// Extra names an optional dependency group, such as "cuda". Empty in the
	// current interface, which installs the CPU set.
	Extra string

	// Environ is the environment the child inherits. Passed through untouched
	// so that a proxy or a trust store the user has configured keeps working.
	Environ []string

	// BinaryVersion and CodeRevision are recorded in the manifest, so that an
	// environment built by one release can be recognised by another.
	BinaryVersion string
	CodeRevision  string

	// OnProgress receives each phase transition. Nil discards them.
	OnProgress func(Progress)
}

// Snapshot is the state of provisioning, for the API.
type Snapshot struct {
	Status Status
	Phase  Phase

	Message string

	ErrorCode    string
	ErrorMessage string
	Remediation  string

	Python     string
	StartedAt  time.Time
	FinishedAt *time.Time
}

// Manager owns at most one provisioning run.
type Manager struct {
	log *slog.Logger

	mu      sync.Mutex
	state   Snapshot
	running bool
	cancel  context.CancelFunc
	proc    *exec.Cmd

	// onProgress belongs to the run in flight. Kept so that the terminal
	// transition is announced through the same channel as the rest, rather than
	// a caller having to poll State and guess when to stop.
	onProgress func(Progress)
}

// NewManager builds a manager that has never run.
func NewManager(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{log: log, state: Snapshot{Status: StatusIdle}}
}

// State reports the current state.
func (m *Manager) State() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Start begins a run, or reports ErrBusy.
//
// Deliberately not queued: a user who clicks twice wants one install, and a
// queued second run would download everything again for no reason.
func (m *Manager) Start(req Request) error {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return ErrBusy
	}

	// Detached from any request: the client that asked may close the tab, and
	// several hundred megabytes should still arrive. Cancellation is explicit.
	ctx, cancel := context.WithCancel(context.Background())

	m.running = true
	m.cancel = cancel
	m.proc = nil
	m.onProgress = req.OnProgress
	m.state = Snapshot{
		Status:    StatusRunning,
		Phase:     PhaseDetect,
		StartedAt: time.Now(),
	}
	m.mu.Unlock()

	go func() {
		defer cancel()
		m.run(ctx, req)
	}()

	return nil
}

// Cancel stops the run in progress and reports whether there was one.
func (m *Manager) Cancel() bool {
	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return false
	}
	cancel, proc := m.cancel, m.proc
	m.mu.Unlock()

	// The process first, so that the context cancellation is not left waiting
	// on a download that is mid-write.
	if proc != nil {
		platform.KillProcessTree(proc)
	}
	cancel()
	return true
}

// run performs the steps in order and records how it ended.
func (m *Manager) run(ctx context.Context, req Request) {
	steps := []struct {
		phase Phase
		run   func(context.Context, Request) error
	}{
		{PhaseDetect, m.detect},
		{PhaseInterpreter, m.interpreter},
		{PhaseDependencies, m.dependencies},
		{PhaseVerify, m.verify},
	}

	for _, step := range steps {
		if err := ctx.Err(); err != nil {
			m.finish(StatusCancelled, step.phase, "", err)
			return
		}

		m.report(Progress{Phase: step.phase, Status: StatusRunning})

		if err := step.run(ctx, req); err != nil {
			if ctx.Err() != nil {
				m.cleanCancelled(req)
				m.finish(StatusCancelled, step.phase, "", ctx.Err())
				return
			}
			// uv's own last words, because they name the actual problem far
			// more often than anything this layer could say about it.
			message := err.Error()
			m.finish(StatusFailed, step.phase, message, err)
			return
		}
	}

	// Recorded before the terminal event, so that a reader who arrives on the
	// event finds the manifest already there.
	if err := writeManifest(req); err != nil {
		m.log.Warn("could not record the provisioned environment", "error", err)
	}

	m.finish(StatusReady, PhaseVerify, "", nil)
}

// detect fails fast, before anything is downloaded.
func (m *Manager) detect(ctx context.Context, req Request) error {
	// uv first: if the bundled copy cannot run there is nothing to be done, and
	// finding out now costs nothing.
	out, err := m.exec(ctx, req, "uv", "--version")
	if err != nil {
		return fmt.Errorf("the copy of uv could not be run: %w: %s", err, platform.LastLines(out, 3))
	}

	for _, name := range []string{"pyproject.toml", "uv.lock"} {
		if !platform.Exists(filepath.Join(req.AIDir, name)) {
			return fmt.Errorf("the AI worker's source is incomplete: %s is not in %s", name, req.AIDir)
		}
	}

	if err := os.MkdirAll(req.RuntimeDir, 0o755); err != nil {
		return fmt.Errorf("cannot write to the data directory: %w", err)
	}

	// A nil reading means "could not tell", which must not block.
	if free := platform.FreeSpace(req.RuntimeDir); free != nil && *free < MinFreeBytes {
		return fmt.Errorf("%w: %s available in %s, about %s needed",
			ErrNoSpace, bytes(*free), req.RuntimeDir, bytes(MinFreeBytes))
	}

	return nil
}

// interpreter installs the Python that will run the worker.
func (m *Manager) interpreter(ctx context.Context, req Request) error {
	// A venv that exists but cannot run makes uv refuse with a message about an
	// invalid environment, which is unactionable. Removing it first is the
	// difference between a retry that works and a retry that fails the same way.
	repairVenv(req)

	out, err := m.exec(ctx, req, "uv", "python", "install", req.Python)
	if err != nil {
		return fmt.Errorf("could not install Python %s: %w: %s",
			req.Python, err, platform.LastLines(out, 5))
	}
	return nil
}

// dependencies installs everything in the lockfile.
func (m *Manager) dependencies(ctx context.Context, req Request) error {
	// --frozen so that a lockfile shipped with the binary is never rewritten on
	// a user's machine; --no-dev because the archive ships no dev group.
	args := []string{"sync", "--frozen", "--no-dev"}
	if req.Extra != "" {
		args = append(args, "--extra", req.Extra)
	}

	out, err := m.exec(ctx, req, "uv", args...)
	if err != nil {
		return fmt.Errorf("could not install the dependencies: %w: %s",
			err, platform.LastLines(out, 5))
	}
	return nil
}

// verify is the authority on whether the install worked.
//
// uv exiting zero is not that: it means uv believes it did what it was asked,
// which is a different claim from "the worker can be imported". The self-check
// is the same one the core runs at handshake time, so an environment that
// passes here is one that will not fail later.
func (m *Manager) verify(ctx context.Context, req Request) error {
	python := platform.RuntimeVenvPython(req.RuntimeDir)

	output, err := runSelfCheck(ctx, python, req.AIDir)
	if err != nil {
		return fmt.Errorf("the installed environment does not work: %w: %s",
			err, platform.LastLines(output, 8))
	}

	if failed := failedChecks(output); len(failed) > 0 {
		return fmt.Errorf("the installed environment does not work: %s", strings.Join(failed, "; "))
	}

	m.mu.Lock()
	m.state.Python = python
	m.mu.Unlock()

	return nil
}

// exec runs one uv command, streaming its output into a bounded buffer.
func (m *Manager) exec(ctx context.Context, req Request, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, req.UV, args...)
	cmd.Dir = req.AIDir
	// Stdin is /dev/null for the same reason FFmpeg's is: an inherited stdin in
	// a container is where the worker protocol lives.
	cmd.Stdin = nil
	cmd.Env = append(append([]string{}, req.Environ...), uvEnv(req)...)

	buffer := newLineBuffer(maxBufferedLines)
	cmd.Stdout = buffer
	cmd.Stderr = buffer

	// Started and published under one lock, and not with Run.
	//
	// exec.Cmd fills in its Process field inside Start, and a cancel that read
	// the command out of m.proc before that write would be racing the runtime
	// for the pointer — and, having lost, would kill nothing while reporting
	// that it had. Publishing only a started command closes that: a cancel
	// either sees no process, because this one has not started, or sees one it
	// can kill. The lock is held across fork and exec, which is microseconds.
	//
	// Run is Start and Wait together, so this does both by hand.
	m.mu.Lock()
	if err := cmd.Start(); err != nil {
		m.mu.Unlock()
		return buffer.String(), err
	}
	m.proc = cmd
	m.mu.Unlock()

	defer m.setProcess(nil)

	err := cmd.Wait()
	return buffer.String(), err
}

// setProcess records the command a cancel would have to kill.
//
// Nil to clear it, which is how a finished command stops being killable — the
// pid may have been reused by the time anyone tried.
func (m *Manager) setProcess(cmd *exec.Cmd) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.proc = cmd
}

// report announces a phase transition.
func (m *Manager) report(p Progress) {
	m.mu.Lock()
	m.state.Phase = p.Phase
	m.state.Status = StatusRunning
	m.state.Message = p.Message
	notify := m.onProgress
	m.mu.Unlock()

	if notify != nil {
		notify(p)
	}
}

// finish records a terminal state and announces it.
func (m *Manager) finish(status Status, phase Phase, message string, cause error) {
	now := time.Now()

	m.mu.Lock()
	m.running = false
	m.cancel = nil
	m.proc = nil

	m.state.Status = status
	m.state.Phase = phase
	m.state.Message = message
	m.state.FinishedAt = &now

	switch status {
	case StatusFailed:
		m.state.ErrorMessage = message
		m.state.Remediation = remedy(message)
	case StatusCancelled:
		m.state.ErrorMessage = "the install was cancelled"
	case StatusReady:
		m.state.ErrorMessage = ""
		m.state.Remediation = ""
		m.state.ErrorCode = ""
	}
	if status == StatusFailed {
		m.state.ErrorCode = errorCode(message)
	}

	notify := m.onProgress
	m.onProgress = nil
	m.mu.Unlock()

	m.log.Info("provisioning finished", "status", string(status), "phase", string(phase))

	// Announced outside the lock: the callback reaches the event bus, and
	// holding a mutex that State() also takes would let a slow subscriber block
	// a status read.
	if notify != nil {
		notify(Progress{Phase: phase, Status: status, Message: message, Err: cause})
	}
}

// cleanCancelled removes what a cancelled run left half-built.
//
// Only the venv: it is the thing uv checks before reusing, and a venv without a
// working interpreter is exactly what makes a retry fail. The interpreter and
// the download cache are left alone because uv verifies both itself, and
// discarding them would make the retry download everything again.
func (m *Manager) cleanCancelled(req Request) {
	if err := os.RemoveAll(platform.RuntimeVenvDir(req.RuntimeDir)); err != nil {
		m.log.Warn("could not remove the partial environment", "error", err)
	}
}

// uvEnv redirects everything uv writes into the data directory.
//
// Without this, provisioning puts a Python installation and a wheel cache in
// the user's home directory, where a tool that claims to be self-contained has
// no business writing, and where "remove the AI environment" would not remove
// it.
func uvEnv(req Request) []string {
	return []string{
		"UV_PYTHON_INSTALL_DIR=" + platform.RuntimePythonDir(req.RuntimeDir),
		"UV_PROJECT_ENVIRONMENT=" + platform.RuntimeVenvDir(req.RuntimeDir),
		"UV_CACHE_DIR=" + platform.RuntimeCacheDir(req.RuntimeDir),
		// A terminal animation is not progress a program can report, and the
		// escape sequences in it would end up in error messages.
		"UV_NO_PROGRESS=1",
		// The interpreter we just installed, not whatever the machine has.
		"UV_PYTHON_PREFERENCE=only-managed",
	}
}

// runSelfCheck asks the new environment what it can import.
func runSelfCheck(ctx context.Context, python, dir string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, selfCheckTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, python, "-m", "nikucooker_ai", "--selfcheck")
	cmd.Dir = dir
	cmd.Stdin = nil

	output, err := cmd.CombinedOutput()
	return string(output), err
}

// selfCheckTimeout bounds the self-check.
//
// Importing ctranslate2 and onnxruntime from a cold filesystem is seconds, not
// milliseconds, and the first run also loads the VAD model.
const selfCheckTimeout = 2 * time.Minute

// selfCheck is the shape the worker prints on one line.
//
// Local rather than shared: it is the worker's output format, and the deserialiser
// living next to the code that consumes it is what keeps a change on the Python
// side from silently breaking a struct three packages away.
type selfCheck struct {
	OK     bool   `json:"ok"`
	Python string `json:"python"`
	Checks []struct {
		Name  string `json:"name"`
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	} `json:"checks"`
}

// failedChecks names the checks that did not pass, or nothing when they all did.
//
// Unparseable output is not treated as failure: uv exiting zero and the
// interpreter exiting zero is the answer that matters, and a stricter reading
// of a diagnostic format would turn a working environment into a refusal.
func failedChecks(output string) []string {
	line := lastJSONLine(output)
	if line == "" {
		return nil
	}

	var report selfCheck
	if err := json.Unmarshal([]byte(line), &report); err != nil {
		return nil
	}

	var failed []string
	for _, check := range report.Checks {
		if !check.OK {
			failed = append(failed, check.Name+": "+check.Error)
		}
	}
	return failed
}

// lastJSONLine finds the reported line, tolerating incidental output before it.
func lastJSONLine(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "{") {
			return line
		}
	}
	return ""
}

// repairVenv removes a virtual environment that cannot run.
func repairVenv(req Request) {
	venv := platform.RuntimeVenvDir(req.RuntimeDir)
	if !platform.Exists(venv) {
		return
	}

	python := platform.RuntimeVenvPython(req.RuntimeDir)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// `pass` is enough: the question is only whether the interpreter starts.
	if err := exec.CommandContext(ctx, python, "-c", "pass").Run(); err == nil {
		return
	}

	_ = os.RemoveAll(venv)
}

// bytes renders a size the way the interface does.
func bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for value := n / unit; value >= unit; value /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
