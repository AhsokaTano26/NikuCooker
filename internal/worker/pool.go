package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/AhsokaTano26/NikuCooker/pkg/protocol"
)

// Restart budget. A worker that dies repeatedly is a broken environment, not a
// transient fault, and restarting it forever would spin and fill the disk with
// logs while every job failed anyway.
const (
	RestartWindow = 5 * time.Minute
	RestartBudget = 3
)

// ErrBudgetExhausted reports that a worker has died too often to keep trying.
var ErrBudgetExhausted = errors.New("worker: restart budget exhausted")

// Pool hands out workers, starting them on demand.
//
// Concurrency is bounded by a token channel rather than a semaphore on the
// workers themselves: a worker must be started while a token is held, so the
// cap applies to processes rather than to leases.
type Pool struct {
	cfg Config
	log *slog.Logger

	tokens chan struct{}

	mu       sync.Mutex
	idle     []*Worker
	restarts []time.Time
	latched  error
}

// NewPool builds a pool of at most size workers.
func NewPool(cfg Config, size int) *Pool {
	if size < 1 {
		size = 1
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}

	p := &Pool{
		cfg:    cfg,
		log:    cfg.Log,
		tokens: make(chan struct{}, size),
	}
	// Pre-filled: taking a token means "I may hold one worker".
	for range size {
		p.tokens <- struct{}{}
	}
	return p
}

// Acquire returns a ready worker, starting one if none is idle.
//
// The caller must call Release when finished, or the pool loses that slot.
func (p *Pool) Acquire(ctx context.Context) (*Worker, error) {
	select {
	case <-p.tokens:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	w, err := p.obtain(ctx)
	if err != nil {
		p.tokens <- struct{}{}
		return nil, err
	}
	return w, nil
}

func (p *Pool) obtain(ctx context.Context) (*Worker, error) {
	// A latched pool fails immediately rather than spawning a fourth doomed
	// process: the user needs the diagnosis, not another retry loop.
	p.mu.Lock()
	if p.latched != nil {
		err := p.latched
		p.mu.Unlock()
		return nil, err
	}
	p.mu.Unlock()

	if w := p.takeIdle(); w != nil {
		return w, nil
	}

	w, err := Start(ctx, p.cfg)
	if err != nil {
		return nil, p.recordFailure(err)
	}
	return w, nil
}

// takeIdle returns a live idle worker, discarding any that have died.
func (p *Pool) takeIdle() *Worker {
	p.mu.Lock()
	defer p.mu.Unlock()

	for len(p.idle) > 0 {
		w := p.idle[len(p.idle)-1]
		p.idle = p.idle[:len(p.idle)-1]

		switch w.State() {
		case StateReady, StateBusy:
			return w
		default:
			// Dead. Drop it and look at the next one.
		}
	}
	return nil
}

// recordFailure applies the restart budget and latches the pool when it is
// spent.
func (p *Pool) recordFailure(cause error) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	recent := p.restarts[:0]
	for _, at := range p.restarts {
		if now.Sub(at) < RestartWindow {
			recent = append(recent, at)
		}
	}
	p.restarts = append(recent, now)

	if len(p.restarts) > RestartBudget {
		p.latched = fmt.Errorf(
			"%w: the AI worker failed to start %d times in %s; last error: %w",
			ErrBudgetExhausted, len(p.restarts), RestartWindow, cause)
		return p.latched
	}
	return cause
}

// Release returns a worker to the pool.
//
// A worker that is no longer usable is dropped rather than kept as a trap for
// the next caller.
func (p *Pool) Release(w *Worker) {
	if w == nil {
		return
	}

	switch w.State() {
	case StateReady, StateBusy:
		p.mu.Lock()
		p.idle = append(p.idle, w)
		p.mu.Unlock()
	default:
		// Already dead; nothing to keep.
	}

	p.tokens <- struct{}{}
}

// State reports the pool's aggregate state for the API and the dashboard.
func (p *Pool) State() State {
	p.mu.Lock()
	latched := p.latched
	idle := len(p.idle)
	p.mu.Unlock()

	if latched != nil {
		return StateFailed
	}
	if idle > 0 {
		return StateReady
	}
	// No idle worker: either none has been started yet or all are busy. Both
	// read as "not immediately available", which is what a caller needs to know.
	return StateStarting
}

// Latched reports the error that stopped the pool, if any.
func (p *Pool) Latched() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.latched
}

// Reset clears the restart budget and the latched error.
//
// Exposed so that a user who has fixed their environment — reinstalled the
// Python dependencies, say — can recover without restarting the core.
func (p *Pool) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.restarts = nil
	p.latched = nil
}

// Shutdown stops every idle worker.
func (p *Pool) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	idle := p.idle
	p.idle = nil
	p.mu.Unlock()

	var firstErr error
	for _, w := range idle {
		if err := w.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Call runs a request against a pooled worker.
//
// It is the only entry point stages should use: acquiring and releasing around
// every call is the sort of pairing that survives right up until an early
// return, and a leaked slot is a pool that gradually stops working.
func (p *Pool) Call(
	ctx context.Context,
	method string,
	params any,
	onProgress func(protocol.ProgressEvent),
) (result []byte, err error) {
	w, err := p.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer p.Release(w)

	raw, err := w.Call(ctx, method, params, onProgress)
	if err != nil {
		return nil, err
	}
	return raw, nil
}
