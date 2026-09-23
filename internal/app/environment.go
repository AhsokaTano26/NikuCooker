package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/events"
	"github.com/AhsokaTano26/NikuCooker/internal/platform"
)

// EnvironmentState is what the startup check found.
//
// It is the answer to one question — can this installation transcribe anything?
// — and it is deliberately not the same question as "has an environment been
// installed". A checkout with a working `ai/.venv` has never been provisioned
// and needs nothing; a data directory with a half-removed runtime has one and
// cannot run a job. Only the first answers the question a user has.
type EnvironmentState string

const (
	// EnvironmentChecking is the state between starting the server and the
	// answer arriving. Reported rather than hidden, because "we do not know
	// yet" and "it is missing" call for different things on screen.
	EnvironmentChecking EnvironmentState = "starting"

	// EnvironmentReady means an interpreter resolved and imported the worker.
	EnvironmentReady EnvironmentState = "ready"

	// EnvironmentMissing means nothing resolved: no provisioned environment, no
	// virtualenv beside the worker's source, and nothing on PATH that can run
	// it. This is the machine the first-run install exists for.
	EnvironmentMissing EnvironmentState = "missing"

	// EnvironmentBroken means an interpreter resolved but could not import the
	// worker package — a half-removed environment, or one built from a
	// different lockfile. Installing again is the fix; nothing else is.
	EnvironmentBroken EnvironmentState = "failed"
)

// EnvironmentStatus is the result of the check.
type EnvironmentStatus struct {
	State EnvironmentState

	// Python is the interpreter, when one resolved.
	Python string

	// Detail says what is wrong, in the interpreter's or the search's own
	// words. Empty when ready.
	Detail string
}

// NeedsInstall reports whether this is a state an install would fix.
func (s EnvironmentStatus) NeedsInstall() bool {
	return s.State == EnvironmentMissing || s.State == EnvironmentBroken
}

// StartEnvironmentCheck looks for a usable worker environment, in the
// background, and announces what it found.
//
// Run at startup rather than on the System page, because a user who cannot
// transcribe should not have to go looking for the reason. The check spawns
// Python, so it does not block the server from listening: the interface is
// usable in under a second, and the answer arrives shortly after.
//
// Returns immediately. The result is read with Environment.
func (a *App) StartEnvironmentCheck(ctx context.Context) {
	a.environmentOnce.Do(func() {
		go func() {
			status := a.checkEnvironment(ctx)
			a.publishEnvironment(status)
		}()
	})
}

// Environment reports the last check's result.
func (a *App) Environment() EnvironmentStatus {
	a.environmentMu.Lock()
	defer a.environmentMu.Unlock()

	if a.environment == nil {
		// Nobody started the check — a CLI command, which has no interface to
		// report to and asks the question directly when it needs the answer.
		return EnvironmentStatus{State: EnvironmentChecking}
	}
	return *a.environment
}

// CheckEnvironmentNow runs the check and waits for it.
//
// For a caller that needs the answer before going on — the doctor command, and
// a test — where the background form would only be a race.
func (a *App) CheckEnvironmentNow(ctx context.Context) EnvironmentStatus {
	status := a.checkEnvironment(ctx)
	a.publishEnvironment(status)
	return status
}

// publishEnvironment records the result and announces it.
func (a *App) publishEnvironment(status EnvironmentStatus) {
	a.environmentMu.Lock()
	previous := a.environment
	a.environment = &status
	a.environmentMu.Unlock()

	if a.Events == nil {
		return
	}

	event := events.New(events.TypeWorkerStatus).
		With("status", string(status.State))
	if status.Python != "" {
		event = event.With("python", status.Python)
	}
	if previous != nil {
		event = event.With("previous", string(previous.State))
	}
	if status.Detail != "" {
		event = event.With("detail", status.Detail)
	}

	a.Events.Emit(event)
}

// checkEnvironment does the work.
func (a *App) checkEnvironment(ctx context.Context) EnvironmentStatus {
	// A bound on the whole check. A wedged interpreter must not leave the
	// interface saying "checking" for the life of the process.
	ctx, cancel := context.WithTimeout(ctx, environmentCheckTimeout)
	defer cancel()

	dir, err := a.AIDir()
	if err != nil {
		// Nothing to import from. An archive extracted without its ai/
		// directory looks exactly like this, and no install can help until the
		// files are there.
		return EnvironmentStatus{
			State:  EnvironmentMissing,
			Detail: err.Error(),
		}
	}

	// Resolved without requiring the import first, so that "there is no
	// interpreter" and "there is one and it cannot import the package" come
	// back as different answers.
	python, err := platform.ResolvePython(ctx, platform.ResolveOptions{
		Explicit:   a.Config().AI.Python,
		AIDir:      dir,
		RuntimeDir: a.RuntimeDir(),
		Hint:       a.provisionHint(),
	})
	if err != nil {
		state := EnvironmentMissing
		if !errors.Is(err, platform.ErrNoPython) {
			// A configured interpreter that will not run is not something an
			// install fixes: it is a path to correct.
			state = EnvironmentBroken
		}
		return EnvironmentStatus{State: state, Detail: err.Error()}
	}

	if err := platform.CheckImport(ctx, python, dir, environmentCheckTimeout); err != nil {
		return EnvironmentStatus{
			State:  EnvironmentBroken,
			Python: python,
			Detail: firstLine(err.Error()),
		}
	}

	return EnvironmentStatus{State: EnvironmentReady, Python: python}
}

// environmentCheckTimeout bounds the whole check.
//
// Importing the worker pulls in ctranslate2 and onnxruntime, which is seconds
// on a cold filesystem — but not minutes, and a check that never answers is
// worse than one that answers "broken".
const environmentCheckTimeout = 45 * time.Second

// firstLine keeps an error to something that fits on one line of an interface.
func firstLine(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		return strings.TrimSpace(text[:index])
	}
	return strings.TrimSpace(text)
}
