// Package logging keeps a window of recent log records where the interface can
// read them.
//
// The alternative is the user opening a terminal, which is exactly the thing a
// web interface exists to avoid. "Why did my run fail" is answered by the last
// few hundred lines, and asking someone to find their compose logs to read them
// is how a failure turns into a support ticket.
package logging

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Record is one log line as the interface shows it.
type Record struct {
	Seq   int64          `json:"seq"`
	Time  time.Time      `json:"time"`
	Level string         `json:"level"`
	Msg   string         `json:"msg"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

// DefaultCapacity is how many records are retained.
//
// A few thousand lines is the tail of a run; beyond that nobody scrolls, and
// the memory is better spent on the work.
const DefaultCapacity = 2000

// Buffer is a ring of recent log records.
type Buffer struct {
	mu sync.Mutex

	records []Record
	start   int
	seq     int64

	// now is injectable so a test can assert ordering without sleeping.
	now func() time.Time
}

// NewBuffer builds a buffer.
func NewBuffer(capacity int) *Buffer {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	return &Buffer{
		records: make([]Record, 0, capacity),
		now:     time.Now,
	}
}

// Add appends a record, evicting the oldest when full.
func (b *Buffer) Add(record Record) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.seq++
	record.Seq = b.seq
	if record.Time.IsZero() {
		record.Time = b.now().UTC()
	}

	if len(b.records) < cap(b.records) {
		b.records = append(b.records, record)
	} else {
		// Overwritten in place. A ring keeps the allocations at zero for the
		// lifetime of the process, which matters because this runs while a job
		// is executing.
		b.records[b.start] = record
		b.start = (b.start + 1) % len(b.records)
	}
	return record.Seq
}

// Tail returns the most recent records, oldest first.
//
// `after` selects everything newer than a sequence number, which is how a view
// follows the log without refetching it — the same contract the event stream
// uses, so a client that has learned one has learned both.
func (b *Buffer) Tail(after int64, limit int) []Record {
	b.mu.Lock()
	defer b.mu.Unlock()

	if limit <= 0 || limit > cap(b.records) {
		limit = cap(b.records)
	}

	out := make([]Record, 0, len(b.records))
	for i := 0; i < len(b.records); i++ {
		record := b.records[(b.start+i)%len(b.records)]
		if record.Seq > after {
			out = append(out, record)
		}
	}

	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// CurrentSeq reports the most recent sequence number.
func (b *Buffer) CurrentSeq() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seq
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// Handler writes records to a buffer as well as to another handler.
//
// It wraps rather than replaces: the terminal or the container's stdout is still
// where a person looks when the server is not running, and the buffer is what
// the interface reads when it is.
type Handler struct {
	next   slog.Handler
	buffer *Buffer
	attrs  []slog.Attr
	groups []string
}

// NewHandler wraps a handler, tee-ing into a buffer.
func NewHandler(next slog.Handler, buffer *Buffer) *Handler {
	return &Handler{next: next, buffer: buffer}
}

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, record slog.Record) error {
	entry := Record{
		Time:  record.Time,
		Level: levelName(record.Level),
		Msg:   record.Message,
	}

	attrs := map[string]any{}
	for _, attr := range h.attrs {
		attrs[attr.Key] = attr.Value.Any()
	}
	record.Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value.Any()
		return true
	})
	if len(attrs) > 0 {
		entry.Attrs = attrs
	}

	h.buffer.Add(entry)

	// Written to the wrapped handler unchanged, so the terminal's output has
	// exactly the attributes the caller supplied — adding the groups here would
	// duplicate what that handler already applies.
	return h.next.Handle(ctx, record)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{
		next:   h.next.WithAttrs(attrs),
		buffer: h.buffer,
		attrs:  append(append([]slog.Attr(nil), h.attrs...), attrs...),
		groups: h.groups,
	}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{
		next:   h.next.WithGroup(name),
		buffer: h.buffer,
		attrs:  h.attrs,
		groups: append(append([]string(nil), h.groups...), name),
	}
}

// levelName renders a level the way the interface shows it.
func levelName(level slog.Level) string {
	switch {
	case level < slog.LevelInfo:
		return "debug"
	case level < slog.LevelWarn:
		return "info"
	case level < slog.LevelError:
		return "warn"
	default:
		return "error"
	}
}
