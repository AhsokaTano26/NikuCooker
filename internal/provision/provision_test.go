package provision

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/platform"
)

// fakeUV writes a stand-in for uv.
//
// A script rather than a mock object, because what is under test is how a real
// child process is invoked: its arguments, its environment, and what its output
// becomes. A mock would assert the call this code intends to make, which is the
// half that is already known to be right.
//
// Unix-only. The behaviour under test — phase order, environment redirection,
// where the environment lands — is identical on Windows; only this scaffolding
// differs, and the rest of the package is tested without it.
func fakeUV(t *testing.T, syncScript string) string {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("the fake uv is a shell script")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "uv")

	script := `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_UV_LOG"

case "$1" in
  --version)
    echo "uv 0.0.0-fake"
    ;;
  python)
    mkdir -p "$UV_PYTHON_INSTALL_DIR"
    ;;
  sync)
` + syncScript + `
    ;;
esac
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// aWorkingSync creates the environment the way uv would, including an
// interpreter that answers the self-check.
const aWorkingSync = `
    mkdir -p "$UV_PROJECT_ENVIRONMENT/bin"
    cat > "$UV_PROJECT_ENVIRONMENT/bin/python" <<'PY'
#!/bin/sh
echo '{"ok":true,"python":"3.12.0","checks":[]}'
PY
    chmod +x "$UV_PROJECT_ENVIRONMENT/bin/python"
`

// aFailingSync reports an error the way uv does, at the end of its output.
const aFailingSync = `
    echo "Resolved 42 packages in 1.2s"
    echo "Downloading ctranslate2 (38.2MiB)"
    echo "error: Failed to fetch: invalid peer certificate: UnknownIssuer"
    exit 1
`

// request builds a request against a throwaway directory tree.
func request(t *testing.T, uv string) (Request, string) {
	t.Helper()

	root := t.TempDir()
	aiDir := filepath.Join(root, "ai")
	runtimeDir := filepath.Join(root, "data", "runtime")

	if err := os.MkdirAll(aiDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pyproject.toml", "uv.lock"} {
		if err := os.WriteFile(filepath.Join(aiDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	log := filepath.Join(root, "uv.log")
	t.Setenv("FAKE_UV_LOG", log)

	return Request{
		AIDir:      aiDir,
		RuntimeDir: runtimeDir,
		UV:         uv,
		Python:     "3.12",
		Environ:    os.Environ(),
	}, log
}

// wait blocks until the run reaches a terminal state.
func wait(t *testing.T, m *Manager) Snapshot {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if state := m.State(); state.Status.Terminal() {
			return state
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("provisioning never finished; state is %+v", m.State())
	return Snapshot{}
}

// The whole point: afterwards, the environment is exactly where the resolver
// will look for it.
//
// This is the failure this feature is most likely to have and least likely to
// notice — installing to a directory the resolver does not check produces a
// successful install that changes nothing, and every other test here would
// still pass.
func TestAFinishedRunLeavesTheEnvironmentWhereItIsResolved(t *testing.T) {
	m := NewManager(nil)
	req, logPath := request(t, fakeUV(t, aWorkingSync))

	if err := m.Start(req); err != nil {
		t.Fatalf("Start: %v", err)
	}

	state := wait(t, m)
	if state.Status != StatusReady {
		t.Fatalf("status = %s (%s), want ready", state.Status, state.ErrorMessage)
	}

	// The interpreter the resolver reports for this runtime directory.
	want := platform.RuntimeVenvPython(req.RuntimeDir)
	if state.Python != want {
		t.Errorf("python = %q, want %q", state.Python, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("the resolver would look at %s and find nothing: %v", want, err)
	}

	// And the manifest is there, so a later failure can say where this came from.
	if _, err := ReadManifest(platform.RuntimeManifestPath(req.RuntimeDir)); err != nil {
		t.Errorf("no manifest was written: %v", err)
	}

	// Every phase ran, in order.
	commands, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(commands)
	for _, want := range []string{"--version", "python install 3.12", "sync --frozen --no-dev"} {
		if !strings.Contains(got, want) {
			t.Errorf("uv was never asked to %q; it ran:\n%s", want, got)
		}
	}
}

// Where uv is told to write, and where the user's home is left alone.
func TestEverythingIsWrittenUnderTheDataDirectory(t *testing.T) {
	req, _ := request(t, "/nonexistent/uv")

	env := strings.Join(uvEnv(req), "\n")

	for key, want := range map[string]string{
		"UV_PYTHON_INSTALL_DIR":  platform.RuntimePythonDir(req.RuntimeDir),
		"UV_PROJECT_ENVIRONMENT": platform.RuntimeVenvDir(req.RuntimeDir),
		"UV_CACHE_DIR":           platform.RuntimeCacheDir(req.RuntimeDir),
	} {
		if !strings.Contains(env, key+"="+want) {
			t.Errorf("%s is not redirected into the data directory:\n%s", key, env)
		}
	}
}

// A failure reports what uv said, not a paraphrase of it.
//
// uv's last lines name the actual problem. Anything this layer writes instead
// is a guess about a message it has already received.
func TestAFailureCarriesUvsOwnWords(t *testing.T) {
	m := NewManager(nil)
	req, _ := request(t, fakeUV(t, aFailingSync))

	if err := m.Start(req); err != nil {
		t.Fatalf("Start: %v", err)
	}

	state := wait(t, m)
	if state.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", state.Status)
	}
	if state.Phase != PhaseDependencies {
		t.Errorf("phase = %s, want the phase that failed", state.Phase)
	}
	if !strings.Contains(state.ErrorMessage, "UnknownIssuer") {
		t.Errorf("uv's own error is missing: %q", state.ErrorMessage)
	}

	// And the certificate failure is recognised, because the advice is the only
	// part of this a user can act on.
	if state.ErrorCode != "PROVISION_TLS" {
		t.Errorf("error code = %q, want PROVISION_TLS", state.ErrorCode)
	}
	if state.Remediation == "" {
		t.Error("no remediation was offered for a certificate failure")
	}
}

// A second click is refused rather than starting a second download.
func TestASecondRunIsRefusedWhileOneIsInFlight(t *testing.T) {
	m := NewManager(nil)
	// A sync that takes long enough to still be running when the second Start
	// arrives.
	slow := fakeUV(t, "\t\t sleep 2\n"+aWorkingSync)
	req, _ := request(t, slow)

	if err := m.Start(req); err != nil {
		t.Fatalf("first Start: %v", err)
	}

	if err := m.Start(req); err != ErrBusy {
		t.Errorf("second Start = %v, want ErrBusy", err)
	}

	wait(t, m)

	// And once it is over, a retry is allowed — the whole point of the button
	// that appears on failure.
	if err := m.Start(req); err != nil {
		t.Errorf("Start after completion = %v, want it to be accepted", err)
	}
	wait(t, m)
}

// A cancelled run leaves nothing that would make the retry fail.
//
// uv refuses to reuse a virtual environment whose interpreter does not run, so
// a half-created one turns "cancel, then try again" into a second failure with
// a message about an invalid environment.
func TestCancellingRemovesThePartialEnvironment(t *testing.T) {
	m := NewManager(nil)
	slow := fakeUV(t, "\t\t sleep 3\n"+aWorkingSync)
	req, _ := request(t, slow)

	// Make a broken venv, as an interrupted earlier run would.
	venv := platform.RuntimeVenvDir(req.RuntimeDir)
	if err := os.MkdirAll(filepath.Join(venv, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(platform.RuntimeVenvPython(req.RuntimeDir), []byte("not a real python"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := m.Start(req); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Long enough to be inside the sync.
	time.Sleep(500 * time.Millisecond)
	if !m.Cancel() {
		t.Fatal("Cancel reported nothing to cancel")
	}

	state := wait(t, m)
	if state.Status != StatusCancelled {
		t.Fatalf("status = %s, want cancelled", state.Status)
	}
	if _, err := os.Stat(venv); !os.IsNotExist(err) {
		t.Error("the partial environment survived the cancellation")
	}

	// The interpreter uv installed is kept: it is still good, and discarding it
	// would make the retry download it again.
	if _, err := os.Stat(platform.RuntimePythonDir(req.RuntimeDir)); err != nil {
		t.Errorf("the installed interpreter was discarded: %v", err)
	}
}

func TestFindUVPrefersTheBundledCopy(t *testing.T) {
	dir := t.TempDir()
	bundled := filepath.Join(dir, "uv")
	if runtime.GOOS == "windows" {
		bundled = filepath.Join(dir, "uv.exe")
	}
	if err := os.WriteFile(bundled, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	onPath := func(string) (string, error) { return "/usr/bin/uv", nil }

	got, err := FindUV(dir, onPath)
	if err != nil {
		t.Fatalf("FindUV: %v", err)
	}
	if got != bundled {
		t.Errorf("FindUV = %q, want the bundled %q", got, bundled)
	}

	// With nothing beside the executable, PATH answers — which is what makes
	// provisioning work from a checkout.
	got, err = FindUV(t.TempDir(), onPath)
	if err != nil || got != "/usr/bin/uv" {
		t.Errorf("FindUV with no bundled copy = %q, %v; want the PATH one", got, err)
	}
}

func TestFindUVReportsWhenThereIsNone(t *testing.T) {
	_, err := FindUV(t.TempDir(), func(string) (string, error) {
		return "", os.ErrNotExist
	})
	if err != ErrNoUV {
		t.Errorf("FindUV = %v, want ErrNoUV", err)
	}
}

// The self-check's verdict, not uv's exit code, decides whether it worked.
func TestFailedChecksAreNamed(t *testing.T) {
	output := `{"ok":false,"checks":[{"name":"faster-whisper","ok":false,"error":"not importable"},{"name":"pydantic","ok":true}]}`

	failed := failedChecks(output)
	if len(failed) != 1 || !strings.Contains(failed[0], "faster-whisper") {
		t.Errorf("failed checks = %v, want the one that failed", failed)
	}

	// Incidental output before the reported line is tolerated.
	if got := failedChecks("noise\n" + output); len(got) != 1 {
		t.Errorf("a prefixed line defeated the parser: %v", got)
	}

	// Not this layer's business to judge a format it did not define.
	if got := failedChecks("this is not json"); got != nil {
		t.Errorf("unparseable output was read as failure: %v", got)
	}
}

func TestRemedyMatchesTheFailuresPeopleHit(t *testing.T) {
	cases := map[string]string{
		"error: invalid peer certificate: UnknownIssuer": "SSL_CERT_FILE",
		"error: No space left on device":                 "disk",
		"error: Permission denied (os error 13)":         "permissions",
	}

	for message, want := range cases {
		got := remedy(message)
		if !strings.Contains(got, want) {
			t.Errorf("remedy(%q) = %q, want it to mention %q", message, got, want)
		}
	}

	if got := remedy("something nobody predicted"); got != "" {
		t.Errorf("a remedy was invented for an unknown failure: %q", got)
	}
}

func TestLineBufferKeepsTheTail(t *testing.T) {
	buffer := newLineBuffer(3)

	if _, err := buffer.Write([]byte("one\ntwo\nthree\nfour\n")); err != nil {
		t.Fatal(err)
	}
	if got := buffer.String(); got != "two\nthree\nfour" {
		t.Errorf("buffer = %q, want the last three lines", got)
	}

	// A line split across writes is still one line.
	fresh := newLineBuffer(5)
	_, _ = fresh.Write([]byte("hel"))
	_, _ = fresh.Write([]byte("lo\nworld"))
	if got := fresh.String(); got != "hello\nworld" {
		t.Errorf("split write = %q", got)
	}
}
