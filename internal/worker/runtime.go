package worker

import "fmt"

// The vocabulary a broken environment is reported in.
//
// These are the codes docs/platform.md §9 and docs/api.md §10 publish, and they
// are kept here because this is the package that owns the question "can a
// worker run". Nothing constructs them yet: the live resolution path
// (platform.ResolvePython, and buildWorker in the app) reports plain errors,
// and the first-run installer reports its own PROVISION_* codes through
// internal/provision.
//
// They are declared rather than deleted so that the published codes and the
// code agree on their names — a client written against the document should not
// be the first thing to discover a rename. Whoever plumbs them through next
// will find the definitions where the document says they are.
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
