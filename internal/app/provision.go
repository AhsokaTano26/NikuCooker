package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/AhsokaTano26/NikuCooker/internal/provision"
)

// StartProvision begins installing the AI environment, and returns as soon as
// it is running.
//
// The work belongs to provision.Manager. What lives here is the wiring only
// this layer knows: which ai/ directory the worker will be launched from, where
// the data directory is, and the fact that succeeding has to invalidate the
// memoised worker resolution — otherwise the failure that was just fixed is the
// one that gets reported on the next run.
//
// It does not wait, because the caller is an HTTP handler with a response to
// send and an install that takes minutes to run. The invalidation still happens
// on success: a goroutine watches for the terminal state.
func (a *App) StartProvision(onProgress func(provision.Progress)) error {
	req, err := a.provisionRequest()
	if err != nil {
		return err
	}

	finished := make(chan struct{})
	req.OnProgress = func(p provision.Progress) {
		if onProgress != nil {
			onProgress(p)
		}
		if p.Status.Terminal() {
			select {
			case <-finished:
			default:
				close(finished)
			}
		}
	}

	if err := a.provision.Start(req); err != nil {
		return err
	}

	go func() {
		<-finished

		// Re-checked whether it succeeded or not: on success the answer has
		// changed, and on failure the check is what tells the interface whether
		// anything was left behind. Without this the page would report what was
		// true before the install — "missing" over an environment that now
		// works, which reads as the button having done nothing.
		status := a.checkEnvironment(context.WithoutCancel(context.Background()))
		a.publishEnvironment(status)

		if status.State == EnvironmentReady {
			a.InvalidateWorker()
		}
	}()

	return nil
}

// CancelProvision stops an install in progress and reports whether there was one.
func (a *App) CancelProvision() bool { return a.provision.Cancel() }

// ProvisioningState reports the state of the install, for the API.
func (a *App) ProvisioningState() provision.Snapshot { return a.provision.State() }

// ProvisioningAvailable reports whether this installation can install an AI
// environment here, and why not when it cannot.
//
// The interface asks before offering the button. A capability that is off is a
// refusal waiting to happen, and a button that fails is worse than a sentence
// saying why it was not offered.
func (a *App) ProvisioningAvailable() (bool, string) {
	cfg := a.Config()

	// An explicit interpreter is a decision the user made. Offering to install
	// one would either be ignored — it is, explicit wins — or would silently
	// change which one runs.
	if python := strings.TrimSpace(cfg.AI.Python); python != "" && !strings.EqualFold(python, "auto") {
		return false, fmt.Sprintf(
			"ai.python is set to %s, so no environment is installed from here; clear it to use the one this page installs",
			python)
	}

	if _, err := a.AIDir(); err != nil {
		return false, "the AI worker's source is missing next to the program; " +
			"extract the archive again, keeping its ai/ directory beside the binary"
	}

	exeDir := ""
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	if _, err := provision.FindUV(exeDir, exec.LookPath); err != nil {
		return false, "no copy of uv was found beside the program or on PATH, " +
			"so there is nothing to install with"
	}

	if dir := a.RuntimeDir(); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return false, fmt.Sprintf("the data directory is not writable: %v", err)
		}
	}

	return true, ""
}

// provisionRequest assembles what an install needs.
func (a *App) provisionRequest() (provision.Request, error) {
	dir, err := a.AIDir()
	if err != nil {
		return provision.Request{}, err
	}

	// Absolute, because uv records it: the editable install of the worker
	// package bakes the directory in, so a relative path here would produce an
	// environment that works from exactly one working directory.
	//
	// config.ResolvePaths normalises storage.data_dir and storage.model_dir but
	// not ai.dir, so a relative ai.dir in a configuration file is a real
	// possibility and must be fixed up here rather than trusted.
	dir, err = filepath.Abs(dir)
	if err != nil {
		return provision.Request{}, fmt.Errorf("app: resolve the AI directory: %w", err)
	}

	runtimeDir := a.RuntimeDir()
	if runtimeDir == "" {
		return provision.Request{}, errors.New("app: there is no data directory to install into")
	}
	runtimeDir, err = filepath.Abs(runtimeDir)
	if err != nil {
		return provision.Request{}, fmt.Errorf("app: resolve the runtime directory: %w", err)
	}

	exeDir := ""
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	uv, err := provision.FindUV(exeDir, exec.LookPath)
	if err != nil {
		return provision.Request{}, err
	}

	return provision.Request{
		AIDir:      dir,
		RuntimeDir: runtimeDir,
		UV:         uv,
		Python:     pythonFloor(a.Config().AI.PythonVersion),
		Environ:    os.Environ(),
	}, nil
}

// provisionHint is appended to "no interpreter was found" when the interface
// could install one.
//
// Empty when ai.python is set: the user named an interpreter, so the answer to
// "it is not usable" is to fix that path, not to offer a different one.
func (a *App) provisionHint() string {
	if python := strings.TrimSpace(a.Config().AI.Python); python != "" && !strings.EqualFold(python, "auto") {
		return ""
	}
	if ok, _ := a.ProvisioningAvailable(); !ok {
		return ""
	}
	return "install the AI runtime from the System page of the interface"
}

// pythonFloor reads the version to install out of the accepted range.
//
// ">=3.12,<3.15" asks for 3.12. Installing the floor rather than the ceiling is
// the conservative choice: it is the version the dependency set was verified
// against, and the one every remediation string in this codebase names.
func pythonFloor(constraint string) string {
	trimmed := strings.TrimSpace(constraint)
	if trimmed == "" {
		return "3.12"
	}

	for _, part := range strings.Split(trimmed, ",") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, ">=") {
			continue
		}
		if version := strings.TrimSpace(strings.TrimPrefix(part, ">=")); version != "" {
			return version
		}
	}
	return "3.12"
}
