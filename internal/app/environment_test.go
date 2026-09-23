package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// fakeInterpreter writes a stand-in that reports whether it can import the
// worker.
//
// A script rather than a real Python: the check's job is to tell "there is no
// interpreter" from "there is one that cannot import the package", and both are
// reachable without installing several hundred megabytes.
func fakeInterpreter(t *testing.T, canImport bool) string {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("the fake interpreter is a shell script")
	}

	path := filepath.Join(t.TempDir(), "python")
	body := "#!/bin/sh\nexit 0\n"
	if !canImport {
		body = "#!/bin/sh\necho \"ModuleNotFoundError: No module named 'nikucooker_ai'\" >&2\nexit 1\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// pathWithoutPython builds a PATH that has what the application needs to start
// and no Python at all.
//
// Emptying PATH instead would fail in New, which refuses to construct an
// application it cannot run a media probe with — so the tool has to survive
// while the interpreter does not.
func pathWithoutPython(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		found, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s is not installed", tool)
		}

		// Under the name it was found by, not under the bare tool name: on
		// Windows the executable is ffmpeg.exe and LookPath will not find a
		// file called ffmpeg, so a copy named for the bare tool is a PATH that
		// has no FFmpeg on it — which fails the test for the wrong reason.
		link := filepath.Join(dir, filepath.Base(found))
		if err := os.Symlink(found, link); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// aiDirWithPackage makes a directory the search will accept as the worker's
// source.
func aiDirWithPackage(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "nikucooker_ai"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The three answers, and the two that call for an install.
//
// This is the check the server runs at startup, and everything downstream is
// gated on it: the interface offers to install only when it says something is
// missing. A check that could not tell "no environment" from "a broken one"
// would send someone to reinstall a machine whose problem is a setting.
func TestTheEnvironmentCheckReportsWhatItFound(t *testing.T) {
	t.Run("nothing to run", func(t *testing.T) {
		// A PATH holding FFmpeg and no Python at all, which is what a fresh
		// Windows box looks like — and what the first-run install exists for.
		t.Setenv("PATH", pathWithoutPython(t))

		application := appWithAIDir(t, aiDirWithPackage(t))
		status := application.CheckEnvironmentNow(context.Background())

		if status.State != EnvironmentMissing {
			t.Errorf("state = %s, want missing (%s)", status.State, status.Detail)
		}
		if !status.NeedsInstall() {
			t.Error("a missing environment was not reported as needing an install")
		}
	})

	t.Run("an interpreter that cannot import the worker", func(t *testing.T) {
		application := appWithAIDir(t, aiDirWithPackage(t))
		application.config.Load().cfg.AI.Python = fakeInterpreter(t, false)

		status := application.CheckEnvironmentNow(context.Background())

		if status.State != EnvironmentBroken {
			t.Errorf("state = %s, want failed (%s)", status.State, status.Detail)
		}
		if !status.NeedsInstall() {
			t.Error("a broken environment was not reported as needing an install")
		}
		// The interpreter's own words, not a summary of them.
		if status.Detail == "" {
			t.Error("no detail was given for a failure the user has to act on")
		}
	})

	t.Run("a working environment", func(t *testing.T) {
		application := appWithAIDir(t, aiDirWithPackage(t))
		python := fakeInterpreter(t, true)
		application.config.Load().cfg.AI.Python = python

		status := application.CheckEnvironmentNow(context.Background())

		if status.State != EnvironmentReady {
			t.Fatalf("state = %s, want ready (%s)", status.State, status.Detail)
		}
		if status.Python != python {
			t.Errorf("python = %q, want %q", status.Python, python)
		}
		// The whole point: a working environment is never offered an install.
		if status.NeedsInstall() {
			t.Error("a working environment was reported as needing an install")
		}
	})
}

// Before the check has run, the answer is "we do not know" — not "missing".
//
// The difference decides whether the interface shows a spinner or an install
// button, and a server that answered "missing" while still looking would offer
// a 300 MB download to a machine that may not need one.
func TestAnUnrunCheckIsNotAMissingEnvironment(t *testing.T) {
	application := appWithAIDir(t, aiDirWithPackage(t))

	status := application.Environment()
	if status.State != EnvironmentChecking {
		t.Errorf("state = %s, want %s", status.State, EnvironmentChecking)
	}
	if status.NeedsInstall() {
		t.Error("an unfinished check was reported as needing an install")
	}
}
