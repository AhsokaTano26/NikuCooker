package logging

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultMaxBytes bounds one run's log file.
//
// Eight megabytes is a few hundred thousand lines — far more than any run
// produces in ordinary use, and small enough that a run stuck in a retry loop
// cannot fill a disk. The cap exists for the second case, not the first.
const DefaultMaxBytes int64 = 8 << 20

// RunLog is one run's log, written to a file and bounded.
//
// The in-memory buffer answers "what is happening now" and is lost on restart.
// This answers "what happened during that run", which is the question actually
// asked — usually days later, about a run that failed while nobody was
// watching. It is the reason the project's logs directory exists.
//
// The size cap is enforced here rather than by the caller because every caller
// would have to remember, and the one that forgot would be the one whose run
// looped.
type RunLog struct {
	mu     sync.Mutex
	file   *os.File
	path   string
	limit  int64
	writer *cappedWriter
	closed bool
}

// CreateRunLog opens a log file, creating the directory if it is not there.
func CreateRunLog(path string, limit int64) (*RunLog, error) {
	if limit <= 0 {
		limit = DefaultMaxBytes
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("logging: create %s: %w", filepath.Dir(path), err)
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("logging: create %s: %w", path, err)
	}

	return &RunLog{
		file:   file,
		path:   path,
		limit:  limit,
		writer: &cappedWriter{file: file, limit: limit},
	}, nil
}

// Path is where the log is being written.
func (r *RunLog) Path() string { return r.path }

// Write appends one record.
//
// JSON lines rather than the terminal's format: a file that has to be read
// back should not need a parser for a layout designed for a person watching it
// scroll, and one record per line survives being opened in an editor.
func (r *RunLog) Write(record Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nil
	}

	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = r.writer.Write(append(encoded, '\n'))
	return err
}

// Close finishes the file and releases it.
//
// Safe to call twice, and safe to call while something is still writing: a run
// ending and its last stage finishing are not ordered with respect to each
// other, and a log that panics on the way out is worse than a short one.
func (r *RunLog) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nil
	}
	r.closed = true

	// A note when the cap was reached. Without it, a log that simply stops
	// looks like a run that simply stopped — and the reader concludes the
	// process died rather than that the file filled.
	if r.writer.truncated {
		note, _ := json.Marshal(Record{
			Time:  time.Now().UTC(),
			Level: "warn",
			Msg: fmt.Sprintf("此日志已达上限（%d MB），后续输出未写入。"+
				"要完整记录请调大 retention.log_max_mb，或把日志级别调回 info。",
				r.limit>>20),
		})
		_, _ = r.file.Write(append(note, '\n'))
	}

	return r.file.Close()
}

// FileHandler writes records to a run log.
type FileHandler struct {
	log   *RunLog
	attrs []slog.Attr
}

// NewFileHandler wraps a run log as a slog handler.
func NewFileHandler(log *RunLog) *FileHandler {
	return &FileHandler{log: log}
}

func (h *FileHandler) Enabled(context.Context, slog.Level) bool {
	// Everything goes to the file. The level is the terminal's problem: a run
	// log that dropped debug records would be missing exactly the detail
	// someone opens it for.
	return true
}

func (h *FileHandler) Handle(_ context.Context, record slog.Record) error {
	return h.log.Write(recordToEntry(h.attrs, record))
}

func (h *FileHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &FileHandler{
		log:   h.log,
		attrs: append(append([]slog.Attr(nil), h.attrs...), attrs...),
	}
}

func (h *FileHandler) WithGroup(string) slog.Handler {
	// Groups are not flattened into the file. The buffer does not flatten them
	// either, and a record that carried a group in one place and not the other
	// would be a difference with no explanation.
	return h
}

// Multi fans every record out to several handlers.
//
// slog has no fan-out of its own, and the run logger needs one: the terminal
// and the in-memory buffer are what show a run while it happens, and the file
// is what is left afterwards. A record must reach all three.
type Multi struct {
	handlers []slog.Handler
}

// NewMulti builds a handler that writes to each of the given handlers.
func NewMulti(handlers ...slog.Handler) *Multi {
	return &Multi{handlers: handlers}
}

func (m *Multi) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range m.handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m *Multi) Handle(ctx context.Context, record slog.Record) error {
	// One handler's failure must not stop the others: a full disk should not
	// also silence the terminal, which is where someone would see why.
	var first error
	for _, handler := range m.handlers {
		if err := handler.Handle(ctx, record); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (m *Multi) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, 0, len(m.handlers))
	for _, handler := range m.handlers {
		next = append(next, handler.WithAttrs(attrs))
	}
	return &Multi{handlers: next}
}

func (m *Multi) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, 0, len(m.handlers))
	for _, handler := range m.handlers {
		next = append(next, handler.WithGroup(name))
	}
	return &Multi{handlers: next}
}

// cappedWriter stops writing at a limit, without complaining.
//
// Silently, because every caller of a log write is on a path where an error
// has nowhere to go — a logging failure that propagates is a failure that
// takes down the thing being logged about.
type cappedWriter struct {
	file      *os.File
	limit     int64
	written   int64
	truncated bool
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if w.truncated {
		return len(p), nil
	}

	if w.written+int64(len(p)) > w.limit {
		w.truncated = true
		return len(p), nil
	}

	written, err := w.file.Write(p)
	w.written += int64(written)
	return written, err
}

// PruneLogs deletes log files older than maxAge, returning how many went.
//
// Keyed on modification time rather than on the name: a run's log is written
// while the run is happening, so the file's mtime is when the run ended —
// which is the fact the retention period is about. Parsing the job id out of
// the name to find a start time would be a second source of truth for
// something the filesystem already records.
func PruneLogs(dir string, maxAge time.Duration) (int, error) {
	if maxAge <= 0 {
		return 0, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("logging: read %s: %w", dir, err)
	}

	cutoff := time.Now().Add(-maxAge)
	removed := 0

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}

		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
			return removed, fmt.Errorf("logging: remove %s: %w", entry.Name(), err)
		}
		removed++
	}
	return removed, nil
}
