// Package platform resolves the host-specific things the core needs before it
// can run anything: which Python interpreter to launch the worker with, and
// whether it is new enough to import the AI package.
//
// It is separate from the worker because these questions are asked in two
// places. The worker asks them to start a process, and the doctor command asks
// them to explain to a user why starting one fails. A diagnosis that used a
// different resolution order from the thing it was diagnosing would be worse
// than no diagnosis at all.
package platform

import (
	"context"
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

// ErrNoPython reports that no usable interpreter was found.
var ErrNoPython = errors.New("platform: no Python interpreter was found")

// PythonVersion is a parsed major.minor.patch triple.
type PythonVersion struct {
	Major int
	Minor int
	Patch int
}

func (v PythonVersion) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Compare orders two versions.
func (v PythonVersion) Compare(other PythonVersion) int {
	for _, pair := range [][2]int{
		{v.Major, other.Major},
		{v.Minor, other.Minor},
		{v.Patch, other.Patch},
	} {
		switch {
		case pair[0] < pair[1]:
			return -1
		case pair[0] > pair[1]:
			return 1
		}
	}
	return 0
}

// ResolveOptions configures ResolvePython.
type ResolveOptions struct {
	// Explicit is the configured interpreter path, or "" / "auto" to search.
	Explicit string

	// AIDir is the directory the worker package lives in. A virtual environment
	// beside it is preferred, because that is where a development install puts
	// the dependencies.
	AIDir string

	// RequireImport makes the search skip interpreters that cannot import the
	// worker package.
	//
	// It is on by default, and it is what turns "python3 is on PATH" into
	// "python3 can actually run this". A system interpreter that lacks
	// faster-whisper is a worse answer than reporting that none was found,
	// because the failure it produces is an import traceback from inside a
	// subprocess.
	RequireImport bool

	// Timeout bounds each probe. Zero uses a default.
	Timeout time.Duration
}

// pythonProbeTimeout bounds one interpreter invocation.
//
// Importing the worker package loads ctranslate2, which takes a second or two
// on a cold filesystem. A probe that has not answered in this long is not going
// to.
const pythonProbeTimeout = 30 * time.Second

// ResolvePython finds the interpreter to run the AI worker with.
func ResolvePython(ctx context.Context, opts ResolveOptions) (string, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = pythonProbeTimeout
	}

	explicit := strings.TrimSpace(opts.Explicit)
	if explicit != "" && !strings.EqualFold(explicit, "auto") {
		// Taken at face value, and verified. Silently falling back from a
		// configured interpreter would hide a typo behind a system Python that
		// happens to work, and the user would never learn which one ran.
		if err := verify(ctx, explicit, opts); err != nil {
			return "", fmt.Errorf("platform: the configured interpreter %s is not usable: %w", explicit, err)
		}
		return explicit, nil
	}

	for _, candidate := range candidates(opts.AIDir) {
		path, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		if err := verify(ctx, path, opts); err != nil {
			continue
		}
		return path, nil
	}

	return "", fmt.Errorf(
		"%w; install one, or set ai.python in the configuration. If the AI environment exists, "+
			"point ai.dir at the directory containing nikucooker_ai", ErrNoPython)
}

// candidates lists the interpreters to try, in order.
//
// The virtual environment beside the AI package comes first: that is where a
// development install puts the dependencies, and it is far more likely to be
// usable than whatever `python3` resolves to on a machine with several.
func candidates(aiDir string) []string {
	var found []string

	if aiDir != "" {
		if runtime.GOOS == "windows" {
			found = append(found,
				filepath.Join(aiDir, ".venv", "Scripts", "python.exe"),
				filepath.Join(aiDir, "venv", "Scripts", "python.exe"),
			)
		} else {
			found = append(found,
				filepath.Join(aiDir, ".venv", "bin", "python"),
				filepath.Join(aiDir, "venv", "bin", "python"),
			)
		}
	}

	// Then whatever is on PATH. python3 before python, because on the platforms
	// where both exist `python` is more likely to be a stale Python 2.
	return append(found, "python3", "python")
}

// verify checks that an interpreter runs and optionally that it can import the
// worker.
func verify(ctx context.Context, path string, opts ResolveOptions) error {
	if opts.RequireImport {
		return runProbe(ctx, path, opts.AIDir, "import nikucooker_ai", opts.Timeout)
	}
	return runProbe(ctx, path, opts.AIDir, "pass", opts.Timeout)
}

// runProbe runs a one-line program with the interpreter.
func runProbe(ctx context.Context, python, dir, program string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, python, "-c", program)
	if dir != "" {
		cmd.Dir = dir
	}
	// Stdin is /dev/null for the same reason FFmpeg's is: an inherited stdin in
	// a container is where the worker protocol lives.
	cmd.Stdin = nil

	output, err := cmd.CombinedOutput()
	if err != nil {
		// The interpreter's own words, not a generic failure. An import error
		// naming the missing module is the whole answer, and replacing it with
		// "probe failed" throws away the only useful part.
		return fmt.Errorf("%w: %s", err, firstLines(string(output), 3))
	}
	return nil
}

// Version runs an interpreter and reads the version it reports.
func Version(ctx context.Context, python string) (PythonVersion, error) {
	ctx, cancel := context.WithTimeout(ctx, pythonProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, python, "-c",
		"import sys; print('%d.%d.%d' % sys.version_info[:3])")
	cmd.Stdin = nil

	output, err := cmd.Output()
	if err != nil {
		return PythonVersion{}, fmt.Errorf("platform: %s did not report a version: %w", python, err)
	}

	return parseVersion(strings.TrimSpace(string(output)))
}

func parseVersion(text string) (PythonVersion, error) {
	parts := strings.Split(text, ".")
	if len(parts) < 2 {
		return PythonVersion{}, fmt.Errorf("platform: %q is not a version", text)
	}

	var version PythonVersion
	fields := []*int{&version.Major, &version.Minor, &version.Patch}
	for i, field := range fields {
		if i >= len(parts) {
			break
		}
		// A suffix like "12" in "3.12.0rc1" is dropped: patch level is not
		// something this project's constraint ever depends on.
		digits := strings.TrimRight(parts[i], "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ+")
		value, err := strconv.Atoi(digits)
		if err != nil {
			return PythonVersion{}, fmt.Errorf("platform: %q is not a version", text)
		}
		*field = value
	}
	return version, nil
}

// ---------------------------------------------------------------------------
// Version constraints
// ---------------------------------------------------------------------------

// Constraint is the subset of PEP 440 that this project's configuration uses:
// comma-separated comparisons against a version.
//
// A full implementation of PEP 440 is a library's worth of work — epochs,
// pre-releases, compatible-release operators, wildcards — and none of it is
// reachable from a setting whose only job is to say "the AI dependencies need
// Python 3.12 through 3.14". Writing the subset is honest; pulling in a
// dependency for the rest would not be.
type Constraint struct {
	comparisons []comparison
}

type comparison struct {
	operator string
	version  PythonVersion
}

// ParseConstraint reads a constraint such as ">=3.12,<3.15".
//
// An empty constraint accepts everything.
func ParseConstraint(text string) (Constraint, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return Constraint{}, nil
	}

	var constraints Constraint
	for _, part := range strings.Split(trimmed, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		operator, rest := splitOperator(part)
		if operator == "" {
			return Constraint{}, fmt.Errorf(
				"platform: %q is not a version constraint; expected comparisons like >=3.12,<3.15", text)
		}

		version, err := parseVersion(rest)
		if err != nil {
			return Constraint{}, fmt.Errorf("platform: %q: %w", part, err)
		}
		constraints.comparisons = append(constraints.comparisons, comparison{operator, version})
	}
	return constraints, nil
}

// splitOperator separates a comparison's operator from its version.
func splitOperator(text string) (string, string) {
	for _, operator := range []string{">=", "<=", "==", "!=", ">", "<", "~="} {
		if strings.HasPrefix(text, operator) {
			return operator, strings.TrimSpace(strings.TrimPrefix(text, operator))
		}
	}
	return "", text
}

// Allows reports whether a version satisfies the constraint.
func (c Constraint) Allows(version PythonVersion) bool {
	for _, comparison := range c.comparisons {
		order := version.Compare(comparison.version)

		ok := false
		switch comparison.operator {
		case ">=":
			ok = order >= 0
		case "<=":
			ok = order <= 0
		case ">":
			ok = order > 0
		case "<":
			ok = order < 0
		case "==":
			ok = order == 0
		case "!=":
			ok = order != 0
		case "~=":
			// The compatible-release operator: "~=3.12" means "3.12 or later,
			// but not 4.0". It is approximate here in the same way it is in
			// PEP 440 for a two-component version, which is the only form this
			// project uses.
			ok = order >= 0 && version.Major == comparison.version.Major
		}
		if !ok {
			return false
		}
	}
	return true
}

// IsEmpty reports whether the constraint accepts everything.
func (c Constraint) IsEmpty() bool { return len(c.comparisons) == 0 }

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// firstLines returns the first n lines of text, for an error message.
func firstLines(text string, n int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > n {
		lines = append(lines[:n], "…")
	}
	return strings.Join(lines, "\n  ")
}

// Exists reports whether a path exists.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
