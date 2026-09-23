package platform

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The provisioned environment is preferred over everything the search would
// otherwise find.
//
// This ordering is the whole point of provisioning: an environment this
// application created from the uv.lock that shipped with the binary is the one
// interpreter whose contents are known. If `python3` won instead, a user who
// clicked install would still be running on whatever their machine happened to
// have, and the install would appear to have done nothing.
func TestProvisionedEnvironmentIsPreferred(t *testing.T) {
	got := candidates("/ai", "/data/runtime")
	if len(got) == 0 {
		t.Fatal("no candidates")
	}

	want := RuntimeVenvPython("/data/runtime")
	if got[0] != want {
		t.Errorf("first candidate = %q, want the provisioned interpreter %q", got[0], want)
	}

	// And the checkout's environment is still there, behind it.
	venv := filepath.Join("/ai", ".venv", "bin", "python")
	if runtime.GOOS == "windows" {
		venv = filepath.Join("/ai", ".venv", "Scripts", "python.exe")
	}
	if !slicesContains(got, venv) {
		t.Errorf("the project virtualenv is no longer a candidate: %v", got)
	}
}

// With nothing provisioned the search is exactly what it was before.
//
// The empty runtime directory is the normal case on every machine that never
// clicked install, which is most of them — it must not add a candidate that can
// never resolve, and must not reorder the ones that exist.
func TestWithoutProvisioningTheOrderIsUnchanged(t *testing.T) {
	got := candidates("/ai", "")

	for _, candidate := range got {
		if strings.Contains(candidate, "runtime") {
			t.Errorf("a runtime candidate appeared with no runtime directory: %q", candidate)
		}
	}

	// PATH is still the last resort.
	if got[len(got)-1] != "python" {
		t.Errorf("last candidate = %q, want the PATH fallback", got[len(got)-1])
	}
}

// The hint reaches the error, which is the only place a user is told that the
// interface can install the thing that is missing.
func TestTheHintIsPartOfTheFailure(t *testing.T) {
	// An empty PATH is what a machine with no Python looks like to LookPath.
	t.Setenv("PATH", t.TempDir())

	_, err := ResolvePython(context.Background(), ResolveOptions{
		AIDir:      t.TempDir(),
		RuntimeDir: t.TempDir(),
		Hint:       "install the AI runtime from the System page",
	})
	if err == nil {
		t.Fatal("resolution succeeded with an empty PATH")
	}
	if !strings.Contains(err.Error(), "System page") {
		t.Errorf("the hint is not in the error: %v", err)
	}
}

func slicesContains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}
