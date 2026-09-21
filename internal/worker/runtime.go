package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Runtime is a resolved Python interpreter.
type Runtime struct {
	// Python is the interpreter to spawn.
	Python string
	// Dir is the working directory to spawn it in, chosen so that
	// `python -m nikucooker_ai` resolves.
	Dir string
	// Version is the interpreter's reported version, e.g. "3.12.14".
	Version string
	// Source names how it was found, for the doctor report: a user who has
	// three Pythons needs to know which one won.
	Source string
}

// Failure codes, matching docs/platform.md §9.
const (
	CodePythonNotFound       = "PYTHON_NOT_FOUND"
	CodePythonUnsupported    = "PYTHON_VERSION_UNSUPPORTED"
	CodeVenvMissing          = "AI_VENV_MISSING"
	CodeDepMissing           = "AI_DEP_MISSING"
	CodeSchemaDigestMismatch = "SCHEMA_DIGEST_MISMATCH"
	CodeStartupTimeout       = "WORKER_STARTUP_TIMEOUT"
)

// RuntimeError carries a stable code and a remediation.
//
// The remediation is not decoration: "AI dependencies: fail" with no next step
// is a support ticket, and this is the cheapest place to prevent one.
type RuntimeError struct {
	Code        string
	Message     string
	Remediation string
	Err         error
}

func (e *RuntimeError) Error() string {
	if e.Remediation == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s\n  fix: %s", e.Code, e.Message, e.Remediation)
}

func (e *RuntimeError) Unwrap() error { return e.Err }

// SelfCheckReport is the worker's environment report.
type SelfCheckReport struct {
	OK           bool             `json:"ok"`
	Python       string           `json:"python"`
	Platform     string           `json:"platform"`
	SchemaDigest string           `json:"schema_digest"`
	DigestError  string           `json:"schema_digest_error"`
	Checks       []SelfCheckEntry `json:"checks"`
}

// SelfCheckEntry is one named check.
type SelfCheckEntry struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Version string `json:"version,omitempty"`
	Value   string `json:"value,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Failed returns the checks that did not pass.
func (r *SelfCheckReport) Failed() []SelfCheckEntry {
	var out []SelfCheckEntry
	for _, c := range r.Checks {
		if !c.OK {
			out = append(out, c)
		}
	}
	return out
}

// RuntimeManager resolves and validates the Python environment.
type RuntimeManager struct {
	// Python is the configured interpreter, or "auto".
	Python string
	// AcceptedVersion is the version range the AI dependency set supports.
	AcceptedVersion string
	// Dir is the directory to spawn from, so `python -m nikucooker_ai`
	// resolves. Normally the ai/ directory of a source checkout.
	Dir string
	// DataDir is where a bundled runtime would live. Reserved for the future;
	// the candidate is checked so the path exists before it is needed.
	DataDir string

	// LookPath is injectable so resolution can be tested without a real
	// filesystem or a real Python.
	LookPath func(string) (string, error)
	// RunSelfCheck is injectable for the same reason.
	RunSelfCheck func(ctx context.Context, rt *Runtime) (*SelfCheckReport, error)
}

// Resolve finds a usable interpreter, in precedence order.
//
// The order matters: an explicit configuration beats everything, and the venv
// beside the source beats the system interpreter. Preferring the system Python
// over the venv a developer created on purpose produces the "but I installed
// it!" conversation.
func (m *RuntimeManager) Resolve(ctx context.Context) (*Runtime, error) {
	lookPath := m.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}

	var candidates []*Runtime

	if m.Python != "" && m.Python != "auto" {
		if _, err := os.Stat(m.Python); err != nil {
			return nil, &RuntimeError{
				Code:    CodePythonNotFound,
				Message: fmt.Sprintf("ai.python points at %s, which cannot be read", m.Python),
				Err:     err,
			}
		}
		candidates = append(candidates, &Runtime{Python: m.Python, Dir: m.Dir, Source: "configured (ai.python)"})
	}

	candidates = append(candidates, m.standardCandidates(lookPath)...)

	var lastErr error
	for _, candidate := range candidates {
		version, err := probeVersion(ctx, candidate.Python)
		if err != nil {
			lastErr = err
			continue
		}
		if err := m.checkVersion(version); err != nil {
			lastErr = err
			continue
		}
		candidate.Version = version
		if candidate.Dir == "" {
			candidate.Dir = m.Dir
		}
		return candidate, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, &RuntimeError{
		Code:    CodePythonNotFound,
		Message: "no Python interpreter was found",
		Remediation: "install Python 3.12 and run `uv sync` in ai/, " +
			"or set ai.python to an interpreter path",
	}
}

// standardCandidates lists the usual places, in the order they should be tried.
func (m *RuntimeManager) standardCandidates(lookPath func(string) (string, error)) []*Runtime {
	var out []*Runtime

	// A bundled runtime, when one exists. Not shipped in v1 — the slot is here
	// so that adding it later is a candidate rather than a redesign.
	if m.DataDir != "" {
		bundled := filepath.Join(m.DataDir, "runtime", "python", "bin", pythonExecutable())
		if _, err := os.Stat(bundled); err == nil {
			out = append(out, &Runtime{Python: bundled, Dir: m.Dir, Source: "bundled runtime"})
		}
	}

	// The project virtualenv. Checked before the system interpreter because a
	// developer working from a checkout created it on purpose.
	if m.Dir != "" {
		venv := filepath.Join(m.Dir, ".venv", "bin", pythonExecutable())
		if runtime.GOOS == "windows" {
			venv = filepath.Join(m.Dir, ".venv", "Scripts", "python.exe")
		}
		if _, err := os.Stat(venv); err == nil {
			out = append(out, &Runtime{Python: venv, Dir: m.Dir, Source: "project virtualenv (ai/.venv)"})
		}
	}

	// uv-managed interpreters, which is how a machine without a suitable system
	// Python gets one.
	if uv, err := lookPath("uv"); err == nil {
		if found, err := uvPythonFind(context.Background(), uv); err == nil && found != "" {
			out = append(out, &Runtime{Python: found, Dir: m.Dir, Source: "uv-managed interpreter"})
		}
	}

	// Last resort, and validated strictly: a system Python outside the accepted
	// range cannot install the dependencies, and saying so is far more useful
	// than a wall of resolution errors.
	for _, name := range []string{"python3", "python"} {
		if path, err := lookPath(name); err == nil {
			out = append(out, &Runtime{Python: path, Dir: m.Dir, Source: "system " + name})
		}
	}

	return out
}

func pythonExecutable() string {
	if runtime.GOOS == "windows" {
		return "python.exe"
	}
	return "python"
}

// probeVersion asks an interpreter for its version.
func probeVersion(ctx context.Context, python string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// A vector, never a shell string.
	cmd := exec.CommandContext(ctx, python, "-c",
		"import sys; print('%d.%d.%d' % sys.version_info[:3])")
	out, err := cmd.Output()
	if err != nil {
		return "", &RuntimeError{
			Code:    CodePythonNotFound,
			Message: fmt.Sprintf("%s could not be executed", python),
			Err:     err,
		}
	}
	return strings.TrimSpace(string(out)), nil
}

// uvPythonFind asks uv for an interpreter it manages.
func uvPythonFind(ctx context.Context, uv string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, uv, "python", "find", "3.12").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// checkVersion enforces the accepted range.
//
// A range rather than a minimum, because a too-new Python is as unusable as a
// too-old one: the ceiling is set by how far behind CTranslate2's wheels lag.
func (m *RuntimeManager) checkVersion(version string) error {
	if m.AcceptedVersion == "" {
		return nil
	}

	major, minor, err := parseMajorMinor(version)
	if err != nil {
		return &RuntimeError{
			Code:    CodePythonUnsupported,
			Message: fmt.Sprintf("could not parse Python version %q", version),
		}
	}

	lo, hi, err := parseRange(m.AcceptedVersion)
	if err != nil {
		return nil // an unparseable range must not block every interpreter
	}

	if compareMinor(major, minor, lo) < 0 || compareMinor(major, minor, hi) >= 0 {
		return &RuntimeError{
			Code: CodePythonUnsupported,
			Message: fmt.Sprintf(
				"found Python %s, this project needs %s", version, m.AcceptedVersion),
			Remediation: "run `uv python install 3.12` in ai/, or set ai.python to a supported interpreter",
		}
	}
	return nil
}

type minor [2]int

func parseMajorMinor(version string) (int, int, error) {
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return 0, 0, errors.New("not a version")
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, err
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, err
	}
	return major, minor, nil
}

// parseRange reads a ">=A.B,<C.D" constraint.
func parseRange(spec string) (lo, hi minor, err error) {
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, ">="):
			lo, err = parseBound(strings.TrimPrefix(part, ">="))
		case strings.HasPrefix(part, "<"):
			hi, err = parseBound(strings.TrimPrefix(part, "<"))
		}
		if err != nil {
			return minor{}, minor{}, err
		}
	}
	var zero minor
	if lo == zero && hi == zero {
		return minor{}, minor{}, errors.New("empty range")
	}
	return lo, hi, nil
}

func parseBound(s string) (minor, error) {
	major, m, err := parseMajorMinor(strings.TrimSpace(s))
	if err != nil {
		return minor{}, err
	}
	return minor{major, m}, nil
}

func compareMinor(aMajor, aMinor int, b minor) int {
	if aMajor != b[0] {
		if aMajor < b[0] {
			return -1
		}
		return 1
	}
	if aMinor != b[1] {
		if aMinor < b[1] {
			return -1
		}
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// Self-check
// ---------------------------------------------------------------------------

// CheckEnvironment runs the worker in diagnostic mode and reports what it found.
//
// It goes through the real entry point rather than `python -c "import x"`, so a
// broken `__main__`, a shadowed module or a bad package layout fails here — and
// it reports *which* dependency is missing and which version is present, which
// is what a remediation needs.
func (m *RuntimeManager) CheckEnvironment(ctx context.Context, rt *Runtime) (*SelfCheckReport, error) {
	if m.RunSelfCheck != nil {
		return m.RunSelfCheck(ctx, rt)
	}

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, rt.Python, "-m", "nikucooker_ai", "--selfcheck")
	cmd.Dir = rt.Dir

	var stderr strings.Builder
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return nil, &RuntimeError{
			Code:        CodeDepMissing,
			Message:     fmt.Sprintf("the AI worker could not run:\n%s", strings.TrimSpace(stderr.String())),
			Remediation: "run `cd ai && uv sync`",
			Err:         err,
		}
	}

	var report SelfCheckReport
	if err := json.Unmarshal(out, &report); err != nil {
		return nil, &RuntimeError{
			Code:        CodeDepMissing,
			Message:     fmt.Sprintf("the AI worker produced output that is not a report: %q", truncate(string(out), 200)),
			Remediation: "run `cd ai && uv sync`",
			Err:         err,
		}
	}
	return &report, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
