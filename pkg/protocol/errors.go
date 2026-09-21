package protocol

import "fmt"

// Error codes.
//
// This is a closed set. Adding a code is a protocol change and must be mirrored
// in ai/nikucooker_ai/protocol/errors.py; the schema digest will catch the
// omission, but the point of listing them here is that a reviewer can see the
// whole surface at once.
const (
	CodeInvalidParams       = "INVALID_PARAMS"
	CodeUnsupportedMethod   = "UNSUPPORTED_METHOD"
	CodeUnsupportedProtocol = "UNSUPPORTED_PROTOCOL_VERSION"
	CodeDependencyMissing   = "DEPENDENCY_MISSING"
	CodeModelNotFound       = "MODEL_NOT_FOUND"
	CodeModelLoadFailed     = "MODEL_LOAD_FAILED"
	CodeModelAlreadyLoaded  = "MODEL_ALREADY_LOADED"
	CodeDeviceUnavailable   = "DEVICE_UNAVAILABLE"
	CodeOutOfMemory         = "OUT_OF_MEMORY"
	CodeAudioReadFailed     = "AUDIO_READ_FAILED"
	CodeAudioDecodeFailed   = "AUDIO_DECODE_FAILED"
	CodeInferenceFailed     = "INFERENCE_FAILED"
	CodeCancelled           = "CANCELLED"
	CodeTimeout             = "TIMEOUT"
	CodeInternal            = "INTERNAL"
)

// Error is the payload of an error or fatal event.
//
// Message is written for a human and may change between releases; Code is the
// stable identifier that callers branch on. Details is code-specific structured
// context for the UI.
//
// Message must never contain a secret. It may contain a filesystem path, which
// is what makes a failure diagnosable without reproducing it.
type Error struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil protocol error>"
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// NewError builds an error payload.
func NewError(code, message string, retryable bool) *Error {
	return &Error{Code: code, Message: message, Retryable: retryable}
}

// WithDetail returns a copy of the error with one structured detail added.
//
// Details are immutable-by-copy rather than mutated in place so that a shared
// sentinel error can never be amended by one call site and observed by another.
func (e *Error) WithDetail(key string, value any) *Error {
	if e == nil {
		return nil
	}
	details := make(map[string]any, len(e.Details)+1)
	for k, v := range e.Details {
		details[k] = v
	}
	details[key] = value
	return &Error{Code: e.Code, Message: e.Message, Retryable: e.Retryable, Details: details}
}

// retryableCodes records which codes describe a condition that might not recur.
//
// Retryability describes the request, not the moment: OUT_OF_MEMORY is
// "retryable" in the sense that the same request may succeed once other work
// finishes. Whether to actually retry is the core's decision, made against its
// own budget.
var retryableCodes = map[string]bool{
	CodeModelLoadFailed:   true,
	CodeDeviceUnavailable: true,
	CodeOutOfMemory:       true,
	CodeInferenceFailed:   true,
	CodeTimeout:           true,
	CodeInternal:          true,
}

// IsRetryable reports whether the code is in the retryable set.
//
// It exists so that a worker which sets Retryable incorrectly is not the only
// thing standing between a transient failure and a failed job.
func IsRetryable(code string) bool { return retryableCodes[code] }
