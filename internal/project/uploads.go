package project

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Uploads stages media that arrives over HTTP before it belongs to a project.
//
// It exists because the two are not simultaneous: a browser uploads a
// multi-gigabyte file while the user is still choosing a name and a language
// pair, and the project row should not be created until they commit. So the
// bytes land somewhere neutral first, and creating the project moves them into
// place.
//
// The staging directory sits under the data root on purpose. Moving the file
// into the project is then a rename — atomic, and instant regardless of size —
// with the copy fallback in placeSource covering only the case where someone
// configured them onto different filesystems.
type Uploads struct {
	dir string
}

// NewUploads binds the store to a data root.
func NewUploads(dataDir string) *Uploads {
	return &Uploads{dir: filepath.Join(dataDir, "uploads")}
}

// Dir reports the staging root.
func (u *Uploads) Dir() string { return u.dir }

// ErrUploadNotFound reports an upload id that names nothing on disk.
//
// Its own error because it is expected rather than exceptional: an upload that
// was already consumed by a project, discarded, or swept is exactly what a
// retried request looks like.
var ErrUploadNotFound = errors.New("upload not found")

// ErrUploadTooLarge reports a body that exceeded the configured cap.
var ErrUploadTooLarge = errors.New("upload exceeds the size limit")

// Staged is an uploaded file waiting to become a project's source.
type Staged struct {
	ID   string
	Name string
	Path string
	Size int64
}

// Stage writes r into a new upload directory.
//
// The name is reduced to a basename before anything touches the filesystem.
// Nothing about it can then escape the upload's own directory, which is
// UUID-named and therefore shares no prefix with anywhere else.
func (u *Uploads) Stage(_ context.Context, name string, r io.Reader, maxBytes int64) (*Staged, error) {
	base := safeBase(name)
	if base == "" {
		return nil, fmt.Errorf("the uploaded file has no usable name")
	}

	id := uuid.Must(uuid.NewV7()).String()
	dir := filepath.Join(u.dir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating the upload directory: %w", err)
	}

	dest := filepath.Join(dir, base)

	written, err := writeLimited(dest, r, maxBytes)
	if err != nil {
		// A partial upload is worth nothing and, at these sizes, worth
		// reclaiming immediately rather than at the next sweep.
		_ = os.RemoveAll(dir)
		return nil, err
	}

	return &Staged{ID: id, Name: base, Path: dest, Size: written}, nil
}

// writeLimited streams r to a file, refusing to exceed maxBytes.
//
// The limit is applied while reading rather than to a Content-Length header,
// because a declared length is a claim and the point of a cap is not to trust
// one. The read is bounded at maxBytes+1 so that "exactly at the limit" is
// distinguishable from "over it" without reading the rest of a 40 GB body to
// find out.
func writeLimited(dest string, r io.Reader, maxBytes int64) (int64, error) {
	out, err := os.Create(dest)
	if err != nil {
		return 0, fmt.Errorf("creating %s: %w", filepath.Base(dest), err)
	}

	written, copyErr := io.Copy(out, io.LimitReader(r, maxBytes+1))

	// Closed before the size is judged, so the file on disk is complete
	// whatever the outcome, and so the removal that follows can succeed on
	// Windows.
	closeErr := out.Close()

	if copyErr != nil {
		return written, fmt.Errorf("storing the upload: %w", copyErr)
	}
	if closeErr != nil {
		return written, fmt.Errorf("storing the upload: %w", closeErr)
	}
	if written > maxBytes {
		return written, ErrUploadTooLarge
	}
	return written, nil
}

// Path resolves an upload id to the file it staged.
func (u *Uploads) Path(id string) (string, error) {
	dir, err := u.dirFor(id)
	if err != nil {
		return "", err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %s", ErrUploadNotFound, id)
		}
		return "", fmt.Errorf("reading the upload directory: %w", err)
	}

	// The directory holds exactly one file, whichever name it was given. Any
	// more means something wrote there that did not come through Stage, and
	// guessing which one was meant is worse than refusing.
	var found string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("upload %s contains more than one file", id)
		}
		found = entry.Name()
	}
	if found == "" {
		return "", fmt.Errorf("%w: %s", ErrUploadNotFound, id)
	}

	return filepath.Join(dir, found), nil
}

// Get returns an upload's metadata without reading its contents.
func (u *Uploads) Get(id string) (*Staged, error) {
	file, err := u.Path(id)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(file)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrUploadNotFound, id)
	}

	return &Staged{ID: id, Name: filepath.Base(file), Path: file, Size: info.Size()}, nil
}

// Remove discards an upload.
//
// Called both when a user cancels and after a project has taken the file, since
// the move leaves the directory behind empty. A missing upload is not an error:
// the caller is stating an intent, not asserting a fact.
func (u *Uploads) Remove(id string) error {
	dir, err := u.dirFor(id)
	if err != nil {
		return err
	}

	if err := os.RemoveAll(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("discarding upload %s: %w", id, err)
	}
	return nil
}

// Sweep deletes uploads older than maxAge, returning how many went.
//
// Needed because the common failure is silent: a user uploads a 4 GB file,
// closes the tab before creating the project, and nothing ever references it
// again. Without this, the data directory only grows.
func (u *Uploads) Sweep(maxAge time.Duration) (int, error) {
	entries, err := os.ReadDir(u.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading %s: %w", u.dir, err)
	}

	cutoff := time.Now().Add(-maxAge)
	removed := 0

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}

		if err := os.RemoveAll(filepath.Join(u.dir, entry.Name())); err != nil {
			return removed, fmt.Errorf("sweeping upload %s: %w", entry.Name(), err)
		}
		removed++
	}
	return removed, nil
}

// dirFor validates an id and returns its directory.
//
// The id is parsed as a UUID rather than pattern-matched or cleaned. An upload
// id arrives from a client, and accepting one that is merely "safe after
// cleaning" invites the next person to change the cleaning. Parsing admits
// exactly the strings Stage produces and nothing else.
func (u *Uploads) dirFor(id string) (string, error) {
	if _, err := uuid.Parse(id); err != nil {
		return "", fmt.Errorf("%w: %s", ErrUploadNotFound, id)
	}
	return filepath.Join(u.dir, id), nil
}

// safeBase reduces an uploaded filename to a basename that cannot traverse.
//
// Browsers send the name as a path in some configurations and a client is free
// to send anything at all, so this is a boundary rather than a formality.
func safeBase(name string) string {
	// Both separators, regardless of the host's: a Windows client sending
	// "C:\Users\me\ep01.mkv" should yield "ep01.mkv", and the separator that
	// matters for containment is the one on this machine, which is the one
	// filepath.Base would handle — not necessarily the one that arrived.
	base := path.Base(strings.ReplaceAll(name, `\`, "/"))

	base = strings.TrimSpace(base)
	if base == "" || base == "." || base == ".." || base == "/" {
		return ""
	}

	// Control characters would corrupt logs and terminal output, and a NUL
	// cannot be passed to the filesystem at all.
	base = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '-'
		}
		return r
	}, base)

	// Long enough for any real filename, short enough to leave room for the
	// temporary suffixes a copy may add.
	const maxNameRunes = 128
	if runes := []rune(base); len(runes) > maxNameRunes {
		ext := filepath.Ext(base)
		keep := maxNameRunes - len([]rune(ext))
		if keep < 1 {
			keep = 1
		}
		base = string(runes[:keep]) + ext
	}

	return base
}
