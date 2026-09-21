package protocol

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// MaxLineBytes bounds a single protocol line.
//
// This limit is not arbitrary and the reader is not a bufio.Scanner. Scanner's
// default token limit is 64 KiB and it stops silently at the first line that
// exceeds it — and an ASR result for a two-hour video exceeds that by a factor
// of thirty. The failure is a truncated stream with no error, which is why the
// reader below accumulates explicitly and refuses anything oversized.
const MaxLineBytes = 64 << 20 // 64 MiB

// ErrLineTooLong is returned for a line exceeding MaxLineBytes.
//
// Exceeding the limit is a protocol violation, not a recoverable condition: the
// stream is desynchronised at that point, so continuing would mean misparsing
// whatever follows.
var ErrLineTooLong = errors.New("protocol: message exceeds maximum line length")

// Reader reads newline-delimited JSON messages.
//
// It is not safe for concurrent use; one reader belongs to one goroutine.
type Reader struct {
	r   *bufio.Reader
	max int
}

// NewReader wraps r for protocol reading.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReaderSize(r, 64<<10), max: MaxLineBytes}
}

// ReadLine returns the next non-empty message, with the line terminator
// stripped.
//
// Blank lines are skipped rather than reported: a stray newline is a common
// artifact of a shell pipeline and carries no meaning. A trailing carriage
// return is stripped so that a worker running on Windows behaves identically to
// one on Unix.
func (r *Reader) ReadLine() ([]byte, error) {
	for {
		line, err := r.readBounded()
		if err != nil {
			return nil, err
		}
		line = bytes.TrimRight(line, "\r\n")
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		return line, nil
	}
}

// readBounded accumulates one line, refusing to grow past the limit.
func (r *Reader) readBounded() ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.r.ReadSlice('\n')
		// append copies immediately: chunk aliases the reader's internal buffer
		// and is invalidated by the next read.
		buf = append(buf, chunk...)

		if len(buf) > r.max {
			return nil, fmt.Errorf("%w: limit %d bytes", ErrLineTooLong, r.max)
		}
		if err == nil {
			return buf, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(buf) > 0 {
			// A final line without a terminator is still a message.
			return buf, nil
		}
		return nil, err
	}
}

// Writer writes newline-delimited JSON messages.
//
// Safe for concurrent use: the core may emit progress from a reader goroutine
// while a request writer is separate, and interleaving two half-lines would
// desynchronise the stream irrecoverably.
type Writer struct {
	mu sync.Mutex
	w  *bufio.Writer
}

// NewWriter wraps w for protocol writing.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: bufio.NewWriterSize(w, 64<<10)}
}

// WriteMessage marshals v onto a single line and flushes it.
//
// The flush is unconditional. A buffered message that has not reached the pipe
// is indistinguishable from a hang to the other side, and this protocol has no
// other liveness signal.
func (w *Writer) WriteMessage(v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("protocol: marshal: %w", err)
	}
	// encoding/json escapes newlines inside strings, so this can only trip if a
	// type implements MarshalJSON and emits one. That would corrupt framing for
	// every subsequent message, so it is worth the check.
	if bytes.ContainsRune(payload, '\n') {
		return errors.New("protocol: marshalled message contains a newline")
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if _, err := w.w.Write(payload); err != nil {
		return fmt.Errorf("protocol: write: %w", err)
	}
	if err := w.w.WriteByte('\n'); err != nil {
		return fmt.Errorf("protocol: write: %w", err)
	}
	return w.w.Flush()
}

// DecodeRequest decodes a Go → worker request line.
func DecodeRequest(line []byte) (*Request, error) {
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		return nil, fmt.Errorf("protocol: malformed request: %w", err)
	}
	if req.V != Version {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrVersionMismatch, req.V, Version)
	}
	if req.ID == "" {
		return nil, errors.New("protocol: request has no id")
	}
	if req.Method == "" {
		return nil, errors.New("protocol: request has no method")
	}
	return &req, nil
}

// DecodeEvent decodes a worker → Go event line into its concrete type.
//
// The type tag is read first and the line is then decoded into the matching
// struct, so a field belonging to a different event type is a decode error
// rather than a silently ignored key.
func DecodeEvent(line []byte) (Event, error) {
	var probe struct {
		V    int    `json:"v"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return nil, fmt.Errorf("protocol: malformed event: %w", err)
	}
	if probe.V != Version {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrVersionMismatch, probe.V, Version)
	}

	var (
		event Event
		err   error
	)
	switch probe.Type {
	case TypeProgress:
		var e ProgressEvent
		err, event = json.Unmarshal(line, &e), &e
	case TypeResult:
		var e ResultEvent
		err, event = json.Unmarshal(line, &e), &e
	case TypeError:
		var e ErrorEvent
		err, event = json.Unmarshal(line, &e), &e
	case TypeReady:
		var e ReadyEvent
		err, event = json.Unmarshal(line, &e), &e
	case TypeFatal:
		var e FatalEvent
		err, event = json.Unmarshal(line, &e), &e
	default:
		return nil, fmt.Errorf("protocol: unknown event type %q", probe.Type)
	}

	if err != nil {
		return nil, fmt.Errorf("protocol: malformed %s event: %w", probe.Type, err)
	}
	return event, nil
}

// ErrVersionMismatch reports a message whose protocol version this build does
// not speak.
var ErrVersionMismatch = errors.New("protocol: unsupported version")
