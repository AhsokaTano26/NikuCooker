// Package worker owns the Python AI worker's lifecycle.
//
// Go is always the parent: it spawns the process, performs the handshake, sends
// requests, reads events, and kills it when it misbehaves. The worker is
// stateless with respect to projects — it receives file paths and configuration
// and returns structured results.
//
// See docs/ipc-protocol.md for the wire contract.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"log/slog"

	"github.com/AhsokaTano26/NikuCooker/pkg/protocol"
)

// State is a worker's lifecycle state.
type State string

const (
	StateStarting State = "starting"
	StateReady    State = "ready"
	StateBusy     State = "busy"
	StateCrashed  State = "crashed"
	StateFailed   State = "failed"
	StateStopped  State = "stopped"
)

// Errors callers may branch on.
var (
	// ErrCrashed reports that the process died with a request in flight.
	ErrCrashed = errors.New("worker: process exited unexpectedly")
	// ErrStalled reports that a request stopped making progress.
	ErrStalled = errors.New("worker: no progress before the stall timeout")
	// ErrSchemaMismatch reports that the two sides disagree about data shapes.
	ErrSchemaMismatch = errors.New("worker: schema digest mismatch")
	// ErrNotReady reports a call on a worker that is not usable.
	ErrNotReady = errors.New("worker: not ready")
)

// Config describes how to start one worker.
type Config struct {
	// Python is the interpreter path.
	Python string
	// Args are the arguments after the interpreter, e.g. ["-m", "nikucooker_ai"].
	Args []string
	// Dir is the working directory. The worker imports its own package, so this
	// is where the package is importable from.
	Dir string
	// Env is the child's environment. When nil, the parent's is inherited.
	Env []string

	// CodeRevision and SchemaDigest are what the core expects the worker to
	// report. SchemaDigest is compared exactly.
	CodeRevision string
	SchemaDigest string

	StartupTimeout   time.Duration
	StallTimeout     time.Duration
	ModelLoadTimeout time.Duration
	ShutdownTimeout  time.Duration

	Log *slog.Logger
}

// Worker is one Python process.
type Worker struct {
	cfg Config

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	writer *protocol.Writer

	// stderrTail keeps the last few stderr lines so a failure can quote the
	// traceback that caused it. Almost every worker startup failure is an
	// import error whose explanation went to stderr.
	stderr *ringBuffer

	mu      sync.Mutex
	pending map[string]*call
	state   State
	ready   *protocol.ReadyEvent
	exited  chan struct{}
	exitErr error

	seq atomic.Int64
}

// call is one in-flight request.
type call struct {
	id       string
	method   string
	events   chan protocol.Event
	progress func(protocol.ProgressEvent)
	lastSeen atomic.Int64 // unix nanos of the last progress event
}

// Start spawns a worker and completes the handshake.
func Start(ctx context.Context, cfg Config) (*Worker, error) {
	if cfg.Python == "" {
		return nil, errors.New("worker: no Python interpreter configured")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.StartupTimeout <= 0 {
		cfg.StartupTimeout = 60 * time.Second
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 10 * time.Second
	}

	cmd := exec.Command(cfg.Python, cfg.Args...)
	cmd.Dir = cfg.Dir
	if cfg.Env != nil {
		cmd.Env = cfg.Env
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("worker: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("worker: stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("worker: stderr pipe: %w", err)
	}

	w := &Worker{
		cfg:     cfg,
		cmd:     cmd,
		stdin:   stdin,
		writer:  protocol.NewWriter(stdin),
		stderr:  newRingBuffer(50),
		pending: make(map[string]*call),
		state:   StateStarting,
		exited:  make(chan struct{}),
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("worker: start %s: %w", cfg.Python, err)
	}

	go w.drainStderr(stderrPipe)
	go w.readEvents(stdout)
	go w.reap()

	if err := w.awaitReady(ctx); err != nil {
		_ = w.Kill()
		return nil, err
	}
	return w, nil
}

// reap waits for the process to exit and fails anything still in flight.
func (w *Worker) reap() {
	err := w.cmd.Wait()

	w.mu.Lock()
	if w.state != StateStopped {
		w.state = StateCrashed
	}
	w.exitErr = err
	pending := make([]*call, 0, len(w.pending))
	for _, c := range w.pending {
		pending = append(pending, c)
	}
	w.pending = map[string]*call{}
	w.mu.Unlock()

	close(w.exited)

	// Every in-flight request fails as retryable: the work may well succeed on
	// a fresh process, and the alternative — leaving them to time out — makes a
	// crash look like a hang.
	for _, c := range pending {
		c.events <- &protocol.ErrorEvent{
			V: protocol.Version, ID: c.id, Type: protocol.TypeError,
			Err: protocol.NewError(protocol.CodeInternal,
				"the AI worker exited before this request finished", true).
				WithDetail("cause", errString(err)).
				WithDetail("stderr", strings.Join(w.stderr.Lines(), "\n")),
		}
	}
}

func errString(err error) string {
	if err == nil {
		return "exit status 0"
	}
	return err.Error()
}

// drainStderr forwards the worker's stderr into the core's log stream and keeps
// a tail of it for diagnostics.
func (w *Worker) drainStderr(r io.Reader) {
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)

	for {
		n, err := r.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			for {
				idx := indexByte(buf, '\n')
				if idx < 0 {
					break
				}
				line := strings.TrimRight(string(buf[:idx]), "\r")
				buf = buf[idx+1:]
				if line != "" {
					w.stderr.Add(line)
					w.cfg.Log.Debug("worker", "component", "ai-worker", "line", line)
				}
			}
		}
		if err != nil {
			// A partial final line is still worth keeping: a Python traceback
			// killed mid-write is exactly when the tail matters most.
			if len(buf) > 0 {
				w.stderr.Add(strings.TrimRight(string(buf), "\r\n"))
			}
			return
		}
	}
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// readEvents dispatches worker events to their request.
func (w *Worker) readEvents(r io.Reader) {
	reader := protocol.NewReader(r)

	for {
		line, err := reader.ReadLine()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				w.cfg.Log.Warn("worker protocol read failed", "error", err,
					"stderr", strings.Join(w.stderr.Lines(), "\n"))
			}
			return
		}

		event, err := protocol.DecodeEvent(line)
		if err != nil {
			// A frame we cannot parse means the stream is no longer trustworthy:
			// whatever follows may be anything. Killing the worker is the only
			// recovery that does not guess.
			w.cfg.Log.Error("worker sent a malformed message", "error", err,
				"line_bytes", len(line))
			_ = w.Kill()
			return
		}

		switch e := event.(type) {
		case *protocol.ReadyEvent:
			w.mu.Lock()
			w.ready = e
			w.state = StateReady
			w.mu.Unlock()

		case *protocol.FatalEvent:
			w.cfg.Log.Error("worker reported a fatal error",
				"code", e.Err.Code, "message", e.Err.Message)
			w.stderr.Add("fatal: " + e.Err.Message)
			w.mu.Lock()
			w.state = StateFailed
			w.mu.Unlock()

		case *protocol.ProgressEvent:
			w.mu.Lock()
			c := w.pending[e.ID]
			w.mu.Unlock()
			if c != nil {
				c.lastSeen.Store(time.Now().UnixNano())
				if c.progress != nil {
					c.progress(*e)
				}
			}

		case *protocol.ResultEvent:
			w.deliver(e.ID, e)

		case *protocol.ErrorEvent:
			w.deliver(e.ID, e)
		}
	}
}

func (w *Worker) deliver(id string, event protocol.Event) {
	w.mu.Lock()
	c := w.pending[id]
	delete(w.pending, id)
	w.mu.Unlock()

	if c != nil {
		c.events <- event
		close(c.events)
	}
}

// awaitReady completes the startup handshake.
func (w *Worker) awaitReady(ctx context.Context) error {
	deadline := time.NewTimer(w.cfg.StartupTimeout)
	defer deadline.Stop()

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-w.exited:
			return fmt.Errorf("worker: exited during startup: %s\n%s",
				errString(w.exitErr), strings.Join(w.stderr.Lines(), "\n"))

		case <-deadline.C:
			return fmt.Errorf("worker: no ready message within %s\n%s",
				w.cfg.StartupTimeout, strings.Join(w.stderr.Lines(), "\n"))

		case <-ticker.C:
			w.mu.Lock()
			ready := w.ready
			w.mu.Unlock()

			if ready == nil {
				continue
			}
			return w.checkHandshake(ready)
		}
	}
}

// checkHandshake validates the worker's reported versions.
func (w *Worker) checkHandshake(ready *protocol.ReadyEvent) error {
	if !ready.Protocol.Supports(protocol.Version) {
		return fmt.Errorf(
			"worker: speaks protocol %d–%d but this core speaks %d; rebuild whichever is older",
			ready.Protocol.Min, ready.Protocol.Max, protocol.Version)
	}

	// Compared exactly. There is no compatibility mode: a mismatch means the
	// two sides disagree about data shapes, and guessing is worse than
	// stopping. The usual cause is an upgraded binary with a stale environment.
	if w.cfg.SchemaDigest != "" && ready.SchemaDigest != w.cfg.SchemaDigest {
		return fmt.Errorf("%w:\n  core:   %s\n  worker: %s\n"+
			"the Python environment is out of step with this binary; reinstall it (uv sync in ai/)",
			ErrSchemaMismatch, w.cfg.SchemaDigest, ready.SchemaDigest)
	}
	return nil
}

// Ready returns the handshake payload.
func (w *Worker) Ready() *protocol.ReadyEvent {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ready
}

// State reports the worker's lifecycle state.
func (w *Worker) State() State {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state
}

// StderrTail returns recent stderr lines, newest last.
func (w *Worker) StderrTail() []string { return w.stderr.Lines() }

// Call sends a request and waits for its terminal event.
//
// Progress events are forwarded to onProgress, which may be nil. The request
// fails on cancellation, on stall, or on worker death — never by hanging.
func (w *Worker) Call(ctx context.Context, method string, params any, onProgress func(protocol.ProgressEvent)) (json.RawMessage, error) {
	w.mu.Lock()
	if w.state != StateReady && w.state != StateBusy {
		state := w.state
		w.mu.Unlock()
		return nil, fmt.Errorf("%w (state %s)", ErrNotReady, state)
	}
	// A data method makes the worker busy; a control method does not, which is
	// what lets a cancel be answered while a transcription is running.
	if !protocol.IsControlMethod(method) {
		w.state = StateBusy
	}
	w.mu.Unlock()

	if !protocol.IsControlMethod(method) {
		defer func() {
			w.mu.Lock()
			// A crashed worker stays crashed; only a live one returns to ready.
			if w.state == StateBusy {
				w.state = StateReady
			}
			w.mu.Unlock()
		}()
	}

	id := fmt.Sprintf("req_%d", w.seq.Add(1))
	c := &call{
		id:       id,
		method:   method,
		events:   make(chan protocol.Event, 8),
		progress: onProgress,
	}
	c.lastSeen.Store(time.Now().UnixNano())

	w.mu.Lock()
	w.pending[id] = c
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		delete(w.pending, id)
		w.mu.Unlock()
	}()

	if err := w.writer.WriteMessage(protocol.Request{
		V: protocol.Version, ID: id, Method: method, Params: params,
	}); err != nil {
		return nil, fmt.Errorf("worker: send %s: %w", method, err)
	}

	// Only control methods use an absolute deadline. A transcription may
	// legitimately run for an hour, so its liveness is measured by whether
	// progress is still arriving.
	var absolute <-chan time.Time
	if protocol.IsControlMethod(method) {
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		absolute = timer.C
	}

	stall := w.stallTimeoutFor(method)
	ticker := time.NewTicker(min(stall/4, time.Second))
	defer ticker.Stop()

	for {
		select {
		case event, ok := <-c.events:
			if !ok {
				return nil, fmt.Errorf("%w (request %s)", ErrCrashed, id)
			}
			switch e := event.(type) {
			case *protocol.ResultEvent:
				return e.Result, nil
			case *protocol.ErrorEvent:
				return nil, e.Err
			}

		case <-ticker.C:
			if time.Since(time.Unix(0, c.lastSeen.Load())) > stall {
				// Stop waiting, but do not leave the worker running the work:
				// a stalled request that is still consuming the CPU is worse
				// than one that is abandoned.
				_ = w.Cancel(context.WithoutCancel(ctx), id)
				return nil, fmt.Errorf("%w: %s produced no progress for %s", ErrStalled, method, stall)
			}

		case <-absolute:
			_ = w.Cancel(context.WithoutCancel(ctx), id)
			return nil, fmt.Errorf("%w: %s did not answer within 30s", ErrStalled, method)

		case <-ctx.Done():
			// Best effort: the caller is going away, so a failure to send is
			// not actionable here.
			_ = w.Cancel(context.WithoutCancel(ctx), id)
			return nil, ctx.Err()

		case <-w.exited:
			return nil, fmt.Errorf("%w (request %s)\n%s", ErrCrashed, id, strings.Join(w.stderr.Lines(), "\n"))
		}
	}
}

// stallTimeoutFor exempts model loading from the progress-based timeout.
//
// A load genuinely reports progress sparingly — often not at all until it
// finishes — so a short stall timeout would kill it mid-load, and the retry
// would die the same way.
func (w *Worker) stallTimeoutFor(method string) time.Duration {
	if method == protocol.MethodModelLoad {
		if w.cfg.ModelLoadTimeout > 0 {
			return w.cfg.ModelLoadTimeout
		}
		return 10 * time.Minute
	}
	if w.cfg.StallTimeout > 0 {
		return w.cfg.StallTimeout
	}
	return 2 * time.Minute
}

// CallTyped sends a request and decodes its result.
func CallTyped[T any](ctx context.Context, w *Worker, method string, params any, onProgress func(protocol.ProgressEvent)) (*T, error) {
	raw, err := w.Call(ctx, method, params, onProgress)
	if err != nil {
		return nil, err
	}
	var out T
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("worker: decode %s result: %w", method, err)
		}
	}
	return &out, nil
}

// Cancel asks the worker to stop a request.
//
// It goes through Call, so it is a normal request on the control path. The
// protocol guarantees the target's terminal event arrives before this one is
// answered, so a returned success means the request is genuinely finished.
func (w *Worker) Cancel(ctx context.Context, targetID string) error {
	var result protocol.CancelResult
	raw, err := w.Call(ctx, protocol.MethodCancel, protocol.CancelParams{TargetID: targetID}, nil)
	if err != nil {
		return err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &result); err != nil {
			return fmt.Errorf("worker: decode cancel result: %w", err)
		}
	}
	return nil
}

// Shutdown asks the worker to exit, then makes sure it did.
//
// The polite request is best effort. A worker that does not exit promptly is
// killed: artifacts are all on disk, so a re-run is cheap, and blocking
// shutdown on a wedged process is not.
func (w *Worker) Shutdown(ctx context.Context) error {
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.cfg.ShutdownTimeout)
	defer cancel()

	if _, err := w.Call(shutdownCtx, protocol.MethodShutdown, nil, nil); err != nil {
		w.cfg.Log.Debug("worker did not acknowledge shutdown", "error", err)
	}

	select {
	case <-w.exited:
		w.markStopped()
		return nil
	case <-time.After(w.cfg.ShutdownTimeout):
		return w.Kill()
	}
}

// Kill terminates the process immediately.
func (w *Worker) Kill() error {
	select {
	case <-w.exited:
		return nil
	default:
	}

	w.markStopped()
	killProcessTree(w.cmd)

	select {
	case <-w.exited:
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("worker: process did not exit after being killed")
	}
}

func (w *Worker) markStopped() {
	w.mu.Lock()
	w.state = StateStopped
	w.mu.Unlock()
}

// Exited reports a channel closed when the process ends.
func (w *Worker) Exited() <-chan struct{} { return w.exited }

// Pid reports the process id, or 0 when it never started.
func (w *Worker) Pid() int {
	if w.cmd.Process == nil {
		return 0
	}
	return w.cmd.Process.Pid
}

// ---------------------------------------------------------------------------
// stderr tail
// ---------------------------------------------------------------------------

// ringBuffer keeps the last n lines.
type ringBuffer struct {
	mu    sync.Mutex
	max   int
	lines []string
}

func newRingBuffer(max int) *ringBuffer {
	return &ringBuffer{max: max}
}

func (r *ringBuffer) Add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.lines = append(r.lines, line)
	if len(r.lines) > r.max {
		r.lines = r.lines[len(r.lines)-r.max:]
	}
}

func (r *ringBuffer) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]string, len(r.lines))
	copy(out, r.lines)
	return out
}

// killProcessTree terminates a worker and anything it spawned.
//
// On Windows a plain Kill leaves children behind, because Python's process
// model does not propagate termination. taskkill /T is the only reliable
// process-tree kill there.
func killProcessTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}

	if runtime.GOOS == "windows" {
		// The pid comes from os/exec, never from user input, so there is no
		// interpolation concern; the arguments are still passed as a vector.
		kill := exec.Command("taskkill", "/T", "/F", "/PID", fmt.Sprint(cmd.Process.Pid))
		_ = kill.Run()
		return
	}

	_ = cmd.Process.Kill()
}
