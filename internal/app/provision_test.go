package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/AhsokaTano26/NikuCooker/internal/platform"
)

// provisionedPython is the interpreter path a finished install produces.
func provisionedPython(t *testing.T, runtimeDir string) string {
	t.Helper()
	return platform.RuntimeVenvPython(runtimeDir)
}

// appWithAIDir builds an application whose AI directory is a path we control.
func appWithAIDir(t *testing.T, aiDir string) *App {
	t.Helper()

	dataDir := t.TempDir()
	application, err := New(context.Background(), Options{
		Flags: map[string]any{
			"storage": map[string]any{
				"data_dir":  dataDir,
				"model_dir": filepath.Join(dataDir, "models"),
			},
			"ai": map[string]any{"dir": aiDir},
		},
		Environ:      []string{},
		CodeRevision: "test-rev-1",
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { _ = application.Close() })

	return application
}

// A resolution failure is forgotten when the thing that caused it is fixed.
//
// This is what makes installing the environment work at all. Before, the
// failure was latched in a sync.Once for the life of the process: a user who
// clicked install, waited several minutes for it to succeed, and retried would
// be told again that there is no environment — with one now sitting on disk.
func TestInvalidateWorkerUnlatchesAFailedResolution(t *testing.T) {
	aiDir := t.TempDir()
	application := appWithAIDir(t, aiDir)

	if _, err := application.AIDir(); err == nil {
		t.Fatal("a directory with no worker package resolved")
	}

	// What provisioning does not create, but extracting the archive does.
	if err := os.MkdirAll(filepath.Join(aiDir, "nikucooker_ai"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Still the remembered failure: nothing has said the world changed.
	if _, err := application.AIDir(); err == nil {
		t.Fatal("the failure was not memoised, so this test proves nothing")
	}

	application.InvalidateWorker()

	resolved, err := application.AIDir()
	if err != nil {
		t.Fatalf("after invalidation the package is still not found: %v", err)
	}
	if resolved != aiDir {
		t.Errorf("resolved %q, want %q", resolved, aiDir)
	}
}

// The interpreter is resolved from a provisioned environment.
//
// The whole feature in one assertion: after an install, the interpreter the
// worker will be launched with is the one under the data directory. If the
// resolver did not look there, everything else would still pass and the install
// would have achieved nothing.
//
// Unix-only, and not because the behaviour differs. The stand-in has to be
// something the resolver can actually run, and a shell script is not: Windows
// needs a real image, and a copy of an interpreter does not run without the
// libraries beside it. The ordering itself — which candidate comes first, on
// both platforms — is tested without running anything in
// platform.TestProvisionedEnvironmentIsPreferred, which is where a change to it
// would be caught.
func TestTheProvisionedInterpreterIsWhatGetsResolved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a runnable interpreter stand-in cannot be staged portably")
	}

	aiDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(aiDir, "nikucooker_ai"), 0o755); err != nil {
		t.Fatal(err)
	}

	application := appWithAIDir(t, aiDir)

	// A stand-in for the interpreter, at the path provisioning would create.
	runtimeDir := application.RuntimeDir()
	if runtimeDir == "" {
		t.Fatal("the application reported no runtime directory")
	}

	python := provisionedPython(t, runtimeDir)
	if err := os.MkdirAll(filepath.Dir(python), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(python, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Requiring the import would reject this stand-in, so the question asked
	// here is the one the doctor asks: where does the search land?
	resolved, err := application.ResolvePython(context.Background())
	if err != nil {
		t.Fatalf("ResolvePython: %v", err)
	}
	if resolved != python {
		t.Errorf("resolved %q, want the provisioned %q", resolved, python)
	}
}
