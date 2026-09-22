package project

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// publishHarness is a project directory with somewhere to publish from.
func publishHarness(t *testing.T) (dataDir, projectID, sourceDir string) {
	t.Helper()

	dataDir = t.TempDir()
	projectID = "01a0c94f-349f-7cd5-87cc-d08fdfbd4e3d"

	if err := scaffold(Dir(dataDir, projectID)); err != nil {
		t.Fatal(err)
	}

	sourceDir = t.TempDir()
	return dataDir, projectID, sourceDir
}

func write(t *testing.T, dir, name, content string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func names(t *testing.T, dataDir, projectID string) []string {
	t.Helper()

	files, err := ListOutputs(dataDir, projectID)
	if err != nil {
		t.Fatal(err)
	}

	out := make([]string, 0, len(files))
	for _, file := range files {
		out = append(out, file.Name)
	}
	return out
}

// A second run replaces the first run's files rather than adding to them.
//
// The failure this prevents is a stale file that nothing produced: turn a
// format off, run again, and a subtitle file from the previous run is still
// sitting in the output directory where whoever picks it up has no way to know
// it does not belong to the release they think they are assembling.
func TestPublishOutputsReplacesThePreviousSet(t *testing.T) {
	dataDir, projectID, sourceDir := publishHarness(t)

	srt := write(t, sourceDir, "subs.srt", "first run")
	ass := write(t, sourceDir, "subs.ass", "first run")

	if err := PublishOutputs(dataDir, projectID, []Published{
		{Name: "subs.srt", Source: srt},
		{Name: "subs.ass", Source: ass},
	}); err != nil {
		t.Fatal(err)
	}

	if got := names(t, dataDir, projectID); len(got) != 2 {
		t.Fatalf("outputs = %v, want two files", got)
	}

	// The second run produces only one of them.
	if err := PublishOutputs(dataDir, projectID, []Published{
		{Name: "subs.srt", Source: write(t, sourceDir, "subs.srt", "second run")},
	}); err != nil {
		t.Fatal(err)
	}

	got := names(t, dataDir, projectID)
	if len(got) != 1 || got[0] != "subs.srt" {
		t.Fatalf("outputs = %v, want only subs.srt", got)
	}

	content, err := os.ReadFile(filepath.Join(OutputDir(dataDir, projectID), "subs.srt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "second run" {
		t.Errorf("content = %q, want the second run's", content)
	}
}

// A run that produced nothing does not wipe what an earlier run published.
//
// Stages that failed have no artifact, so a run in which only some of them
// succeeded still publishes its finished files — and a run in which none did
// leaves the previous set alone rather than emptying the directory.
func TestPublishOutputsWithNothingKeepsThePreviousSet(t *testing.T) {
	dataDir, projectID, sourceDir := publishHarness(t)

	if err := PublishOutputs(dataDir, projectID, []Published{
		{Name: "subs.srt", Source: write(t, sourceDir, "subs.srt", "kept")},
	}); err != nil {
		t.Fatal(err)
	}

	if err := PublishOutputs(dataDir, projectID, nil); err != nil {
		t.Fatal(err)
	}

	if got := names(t, dataDir, projectID); len(got) != 1 {
		t.Fatalf("outputs = %v, want the previous file kept", got)
	}
}

// A name that is not a plain filename is refused rather than joined.
func TestPublishOutputsRefusesAPathAsAName(t *testing.T) {
	dataDir, projectID, sourceDir := publishHarness(t)

	err := PublishOutputs(dataDir, projectID, []Published{
		{Name: "../escaped.srt", Source: write(t, sourceDir, "subs.srt", "x")},
	})
	if err == nil {
		t.Fatal("a name with a path in it was accepted")
	}

	if _, statErr := os.Stat(filepath.Join(Dir(dataDir, projectID), "escaped.srt")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a file was written outside the output directory")
	}
}

func TestResolveOutputRefusesToLeaveTheDirectory(t *testing.T) {
	dataDir, projectID, _ := publishHarness(t)

	for _, name := range []string{
		"../manifest.json",
		"sub/subs.srt",
		"..",
		"",
		// Absolute paths are the same attack written differently.
		string(filepath.Separator) + "etc" + string(filepath.Separator) + "passwd",
		// A NUL is never a filename byte. Handed to the filesystem it produces
		// an invalid argument, which surfaces as a server fault rather than as
		// the not-found the request actually is.
		"subs.srt\x00.txt",
		"subs\nsrt",
	} {
		if _, err := ResolveOutput(dataDir, projectID, name); err == nil {
			t.Errorf("ResolveOutput(%q) was accepted", name)
		}
	}
}

func TestListOutputsOfAProjectThatHasNotRun(t *testing.T) {
	dataDir, projectID, _ := publishHarness(t)

	files, err := ListOutputs(dataDir, projectID)
	if err != nil {
		t.Fatalf("a project that has not run yet reported an error: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("outputs = %v, want none", files)
	}
}

// The published copy is independent of the artifact it came from.
//
// A hard link would be free and would survive the cache being pruned, but it
// would also mean an editor opened on the output file writes through to the
// cached artifact — whose key is derived from its inputs and never re-checked
// against its contents.
func TestPublishOutputsCopiesRatherThanLinks(t *testing.T) {
	dataDir, projectID, sourceDir := publishHarness(t)

	source := write(t, sourceDir, "subs.srt", "original")
	if err := PublishOutputs(dataDir, projectID, []Published{{Name: "subs.srt", Source: source}}); err != nil {
		t.Fatal(err)
	}

	published := filepath.Join(OutputDir(dataDir, projectID), "subs.srt")
	if err := os.WriteFile(source, []byte("edited upstream"), 0o644); err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(published)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "edited") {
		t.Errorf("editing the source changed the published file: %q", content)
	}
}
