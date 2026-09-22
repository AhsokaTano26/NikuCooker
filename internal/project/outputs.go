package project

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The output directory is where a run's results become files a person can find.
//
// Everything a stage writes lands in the artifact cache, under a path derived
// from its inputs — which is what makes re-running cheap and what makes the
// paths unguessable, change on every invalidation, and hold intermediates no
// one asked for. That is the right shape for a cache and the wrong shape for a
// result, so the results are published here as well: one stable directory, the
// same names every run, nothing in it that is not an answer.
//
// It is a copy rather than a hard link. Linking would be free and would survive
// the cache being pruned, but it would also mean an editor opened on
// output/subs.srt writes through to the cached artifact — whose key was
// derived from its inputs and is never re-checked against its contents. A
// corrupted cache entry is a much worse failure than the disk space.

// OutputDir is where a project's finished files live.
func OutputDir(dataDir, id string) string {
	return filepath.Join(Dir(dataDir, id), DirOutput)
}

// OutputFile is one published result.
type OutputFile struct {
	Name       string    `json:"name"`
	SizeBytes  int64     `json:"size_bytes"`
	ModifiedAt time.Time `json:"modified_at"`
}

// ErrNoOutput reports a published file that is not there.
var ErrNoOutput = errors.New("output not found")

// ListOutputs reads the published files, newest name order.
//
// A missing directory is an empty list rather than an error: it means the
// project has not finished a run yet, which is a state the interface shows
// rather than one it reports as a fault.
func ListOutputs(dataDir, id string) ([]OutputFile, error) {
	dir, err := outputDirFor(dataDir, id)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []OutputFile{}, nil
		}
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}

	files := make([]OutputFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			// Vanished between the readdir and the stat, which is a race with
			// nothing else — but returning a short list is better than failing
			// the whole listing over one entry.
			continue
		}

		files = append(files, OutputFile{
			Name:       entry.Name(),
			SizeBytes:  info.Size(),
			ModifiedAt: info.ModTime().UTC(),
		})
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, nil
}

// ResolveOutput returns the path of one published file.
//
// The name is reduced to a basename before it is joined, and the result is
// checked to be inside the directory afterwards. Either alone would do; both
// together mean a future change to one is not a traversal.
func ResolveOutput(dataDir, id, name string) (string, error) {
	dir, err := outputDirFor(dataDir, id)
	if err != nil {
		return "", err
	}
	return resolveIn(dir, name)
}

// resolveIn returns one file inside a directory, or refuses to.
//
// Shared by the published results and the run logs, because both take a name
// out of a URL and both are one mistake away from reading the whole disk. A
// guard that exists in one place and is copied into another is a guard that
// gets fixed in one place.
func resolveIn(dir, name string) (string, error) {
	if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
		return "", fmt.Errorf("%w: %s", ErrNoOutput, name)
	}

	// Control characters, of which the one that matters is NUL: it is never a
	// valid filename byte, and handing it to the filesystem produces an invalid
	// argument rather than a not-found — which would be reported to the user as
	// the server having a fault, when the request is simply not a filename.
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("%w: %s", ErrNoOutput, name)
		}
	}

	path := filepath.Join(dir, name)

	// Defence in depth. filepath.Join already cleans the result, so this
	// catches only a name that survived the checks above — which is to say, a
	// bug in them.
	relative, err := filepath.Rel(dir, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s", ErrNoOutput, name)
	}

	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%w: %s", ErrNoOutput, name)
		}
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%w: %s", ErrNoOutput, name)
	}

	return path, nil
}

// Published is one file on its way into the output directory.
type Published struct {
	// Name is what it is called there.
	Name string

	// Source is where to read it from, normally inside an artifact directory.
	Source string
}

// PublishOutputs replaces the published set.
//
// Replaced, not added to: a run that produces fewer files than the last one
// must not leave the previous run's leftovers behind, where they would be
// picked up and used by someone who has no way to tell they are stale.
//
// The new set is assembled next to the old one and swapped in at the end, so a
// failure part way through leaves the previous, complete set in place. Copying
// over the live directory would instead leave a truncated video that looks like
// a finished one.
func PublishOutputs(dataDir, id string, files []Published) error {
	if len(files) == 0 {
		return nil
	}

	final, err := outputDirFor(dataDir, id)
	if err != nil {
		return err
	}

	// A sibling of the destination rather than a temp directory elsewhere: the
	// swap is a rename, and a rename is only atomic within one filesystem.
	staging := final + ".staging"

	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("clearing %s: %w", staging, err)
	}
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", staging, err)
	}

	// The names come from stages rather than from a request, but they are still
	// checked here: this function writes into a directory tree, and the check
	// belongs next to the write rather than in the caller that happens to be
	// correct today.
	seen := map[string]bool{}
	for _, file := range files {
		name := filepath.Base(file.Name)
		if name == "" || name == "." || name == ".." || name != file.Name {
			_ = os.RemoveAll(staging)
			return fmt.Errorf("project: %q is not a usable output filename", file.Name)
		}
		if seen[name] {
			_ = os.RemoveAll(staging)
			return fmt.Errorf("project: two stages both want to publish %q", name)
		}
		seen[name] = true

		if err := copyInto(filepath.Join(staging, name), file.Source); err != nil {
			_ = os.RemoveAll(staging)
			return err
		}
	}

	// Runs are serialised per project by the scheduler, so nothing else is
	// writing here.
	if err := os.RemoveAll(final); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("clearing %s: %w", final, err)
	}
	if err := os.Rename(staging, final); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("moving the finished files into place: %w", err)
	}

	return nil
}

// outputDirFor resolves and validates the directory path.
func outputDirFor(dataDir, id string) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}
	return OutputDir(dataDir, id), nil
}

// copyInto copies a file, preserving nothing but its contents and mode.
//
// The artifact it comes from is a cache entry whose timestamps mean nothing to
// a reader, so the copy gets the time it was published — which is what a person
// looking at the folder expects to see.
func copyInto(dest, source string) error {
	in, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("reading %s: %w", filepath.Base(source), err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Base(dest), err)
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copying %s: %w", filepath.Base(source), err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", filepath.Base(dest), err)
	}
	return nil
}
