// Package protocol defines the wire contract between the Go core and the
// Python AI worker.
//
// The transport is newline-delimited JSON over the worker's stdin and stdout:
// one message per line, UTF-8, no embedded newlines. Go owns the worker's
// lifecycle and is always the parent process.
//
// This package is the single source of truth for the message shapes. The Python
// side mirrors them in ai/nikucooker_ai/protocol/, and both sides are held
// together by the golden fixtures in testdata/ plus the schema digest they
// produce. See docs/ipc-protocol.md.
//
// Classification rule: a message carrying a "method" field is a request; a
// message carrying a "type" field is an event. The two never mix.
package protocol

import "encoding/json"

// Version is the protocol version carried in the "v" field of every message in
// both directions.
//
// A message without a matching version is rejected rather than guessed at: the
// cost of a wrong guess is a silently misinterpreted field, and the cost of
// rejection is one clear error message.
const Version = 1

// Event types emitted by the worker.
const (
	// TypeProgress reports partial completion of an in-flight request.
	TypeProgress = "progress"
	// TypeResult is the terminal success event for a request.
	TypeResult = "result"
	// TypeError is the terminal failure event for a request.
	TypeError = "error"
	// TypeReady is emitted once at startup, before any request is served.
	TypeReady = "ready"
	// TypeFatal is emitted immediately before the worker exits on an
	// unrecoverable error. It is not tied to a request.
	TypeFatal = "fatal"
)

// WorkerName identifies this worker implementation in the ready handshake.
const WorkerName = "nikucooker-ai"

// Request is a Go → worker message.
//
// Params carries the method-specific payload. It is nil for methods that take
// none, which is distinct from an empty object only in that both encode the
// same way once marshalled with omitempty.
type Request struct {
	V      int    `json:"v"`
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

// Event is implemented by every worker → Go message.
type Event interface {
	// EventType is the wire value of the "type" field.
	EventType() string
	// RequestID is the request this event belongs to, or "" for lifecycle
	// events that belong to no request.
	RequestID() string
}

// ProgressEvent reports partial completion of a request.
//
// Progress is monotonically non-decreasing within a request and is the liveness
// signal the core's stall detection relies on: a request that stops producing
// progress events is killed rather than waited on.
type ProgressEvent struct {
	V        int             `json:"v"`
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Progress float64         `json:"progress"`
	Message  string          `json:"message,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
}

func (e *ProgressEvent) EventType() string { return TypeProgress }
func (e *ProgressEvent) RequestID() string { return e.ID }

// ResultEvent is the terminal success event for a request.
//
// Result is left raw so that the caller decodes it into the method's result
// type. Large payloads use the result_path indirection instead of travelling
// inline; see ASRTranscribeResult.
type ResultEvent struct {
	V         int             `json:"v"`
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Result    json.RawMessage `json:"result"`
	ElapsedMS *int64          `json:"elapsed_ms,omitempty"`
}

func (e *ResultEvent) EventType() string { return TypeResult }
func (e *ResultEvent) RequestID() string { return e.ID }

// ErrorEvent is the terminal failure event for a request.
type ErrorEvent struct {
	V    int    `json:"v"`
	ID   string `json:"id"`
	Type string `json:"type"`
	Err  *Error `json:"error"`
}

func (e *ErrorEvent) EventType() string { return TypeError }
func (e *ErrorEvent) RequestID() string { return e.ID }

// ReadyEvent is the startup handshake.
//
// SchemaDigest is compared against the core's own before any request is sent.
// This catches the most common support problem in this architecture — a user
// upgrades the Go binary and forgets to reinstall the Python environment — with
// one precise message rather than a stream of deserialisation errors.
type ReadyEvent struct {
	V             int          `json:"v"`
	Type          string       `json:"type"`
	Worker        string       `json:"worker"`
	WorkerVersion string       `json:"worker_version"`
	Protocol      ProtocolSpan `json:"protocol"`
	SchemaDigest  string       `json:"schema_digest"`
	Python        string       `json:"python"`
	Platform      string       `json:"platform"`
	Capabilities  Capabilities `json:"capabilities"`
}

func (e *ReadyEvent) EventType() string { return TypeReady }
func (e *ReadyEvent) RequestID() string { return "" }

// FatalEvent precedes an unrecoverable worker exit.
type FatalEvent struct {
	V    int    `json:"v"`
	Type string `json:"type"`
	Err  *Error `json:"error"`
}

func (e *FatalEvent) EventType() string { return TypeFatal }
func (e *FatalEvent) RequestID() string { return "" }

// ProtocolSpan is the inclusive range of protocol versions a worker supports.
//
// It is a range rather than a single value so that version 2 does not require
// guessing which side is old.
type ProtocolSpan struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// Supports reports whether this span includes the given version.
func (s ProtocolSpan) Supports(v int) bool { return v >= s.Min && v <= s.Max }

// Capabilities describes what the worker can actually do on this host.
//
// Notes carries the reason for any degraded capability, so that a fallback is
// visible to the user rather than something they have to infer from a
// suspiciously slow run.
type Capabilities struct {
	ASR     Providers `json:"asr"`
	VAD     Providers `json:"vad"`
	Devices []string  `json:"devices"`
	Notes   []string  `json:"notes,omitempty"`
}

// Providers lists the implementation names available for one capability.
type Providers struct {
	Providers []string `json:"providers"`
}
