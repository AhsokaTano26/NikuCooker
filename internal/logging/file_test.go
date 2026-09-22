package logging

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newRunLog(t *testing.T, limit int64) (*RunLog, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "logs", "job_test.log")

	log, err := CreateRunLog(path, limit)
	if err != nil {
		t.Fatalf("CreateRunLog: %v", err)
	}
	return log, path
}

func readLines(t *testing.T, path string) []Record {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var out []Record
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var record Record
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("line is not a record: %v\n%s", err, line)
		}
		out = append(out, record)
	}
	return out
}

// The directory is created if it is not there.
//
// A project scaffolded before logs were written to disk has the directory, but
// one restored from a backup or created by an older version may not — and a run
// that failed to start because it could not open its log file would be a run
// lost to its own bookkeeping.
func TestCreateRunLogMakesItsDirectory(t *testing.T) {
	log, path := newRunLog(t, 1<<20)

	if err := log.Write(Record{Level: "info", Msg: "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no log file: %v", err)
	}
}

// The cap stops the writing, and the file says so.
//
// Without the note, a log that stops mid-run is indistinguishable from a run
// that stopped mid-log, and the reader concludes the process died.
func TestRunLogStopsAtItsLimitAndSaysSo(t *testing.T) {
	log, path := newRunLog(t, 512)

	// Comfortably more than the cap.
	for i := 0; i < 200; i++ {
		if err := log.Write(Record{Level: "info", Msg: strings.Repeat("x", 40)}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// Within a record's width of the cap, rather than far under it or over it.
	if info.Size() > 512+256 {
		t.Errorf("file is %d bytes, want roughly the 512-byte limit", info.Size())
	}

	lines := readLines(t, path)
	last := lines[len(lines)-1]

	if last.Level != "warn" || !strings.Contains(last.Msg, "上限") {
		t.Errorf("the file does not end with a note about the limit: %+v", last)
	}

	// And the records before it are intact — the cap truncates, it does not
	// corrupt.
	for _, record := range lines[:len(lines)-1] {
		if record.Level != "info" {
			t.Errorf("a record before the note is damaged: %+v", record)
		}
	}
}

// Closing twice is not an error.
//
// A run ending and its last stage finishing are not ordered with respect to
// each other, and a log that panicked on the way out would take the process
// with it.
func TestRunLogCloseIsIdempotent(t *testing.T) {
	log, _ := newRunLog(t, 1<<20)

	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// Writing after the close is ignored rather than a panic.
	if err := log.Write(Record{Msg: "after"}); err != nil {
		t.Fatalf("Write after Close: %v", err)
	}
}

// An error attribute survives the round trip through the file as its message.
//
// Without this the most important line a failed run writes — "stage failed",
// carrying the reason — is stored as `"error":{}`. The failure is invisible
// until someone opens the log to find out what went wrong, which is the one
// moment the file exists for, and an empty object there is worse than no
// attribute at all: it says the reason was recorded and it was not.
func TestRunLogKeepsTheMessageOfAnError(t *testing.T) {
	log, path := newRunLog(t, 1<<20)
	logger := slog.New(NewFileHandler(log))

	logger.Error("stage failed", "error", errors.New("probe: no such file"),
		"stage", "probe")
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	lines := readLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("wrote %d records, want 1", len(lines))
	}

	got, ok := lines[0].Attrs["error"].(string)
	if !ok {
		t.Fatalf("error = %#v, want a string", lines[0].Attrs["error"])
	}
	if got != "probe: no such file" {
		t.Errorf("error = %q, want the message", got)
	}
	if lines[0].Attrs["stage"] != "probe" {
		t.Errorf("stage = %#v, want probe", lines[0].Attrs["stage"])
	}
}

// Pruning is by age, and zero means never.
func TestPruneLogs(t *testing.T) {
	dir := t.TempDir()

	old := filepath.Join(dir, "old.log")
	fresh := filepath.Join(dir, "fresh.log")
	for _, path := range []string{old, fresh} {
		if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Touched rather than mocked: the retention is about what the filesystem
	// records, which is the same thing the sweep reads.
	past := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	removed, err := PruneLogs(dir, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("PruneLogs: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("the old log survived")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("the fresh log was deleted")
	}

	// Zero days means keep everything.
	if err := os.WriteFile(old, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if removed, err := PruneLogs(dir, 0); err != nil || removed != 0 {
		t.Errorf("PruneLogs(0) = %d, %v; want 0, nil", removed, err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Error("a disabled retention still deleted something")
	}
}
