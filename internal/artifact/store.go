package artifact

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/database"
)

// Store persists artifacts for one project.
type Store struct {
	db         *database.DB
	projectID  string
	projectDir string
}

// NewStore binds a store to a project.
func NewStore(db *database.DB, projectID, projectDir string) *Store {
	return &Store{db: db, projectID: projectID, projectDir: projectDir}
}

// ProjectDir reports the project directory this store writes into.
func (s *Store) ProjectDir() string { return s.projectDir }

// ArtifactsDir is where a project's artifacts live, relative to the project
// directory.
const ArtifactsDir = "artifacts"

// tmpPrefix marks a directory being written. Swept at startup.
const tmpPrefix = ".tmp-"

func (s *Store) stageDir(stage string) string {
	return filepath.Join(s.projectDir, ArtifactsDir, stage)
}

// absDir resolves an artifact's stored relative path.
func (s *Store) absDir(relPath string) string {
	return filepath.Join(s.projectDir, filepath.FromSlash(relPath))
}

func (s *Store) relDir(stage, id string) string {
	return ArtifactsDir + "/" + stage + "/" + id
}

// ---------------------------------------------------------------------------
// Lookup
// ---------------------------------------------------------------------------

// Lookup returns the ready artifact for a key, or nil if there is none.
//
// The directory check is not redundant with the row. A ready row whose
// directory is gone — a user cleaned up by hand, an external drive was
// unmounted, a sync tool removed it — is treated as a miss, because disk is
// authoritative and the database is an index. Trusting the row instead would
// produce a stage that reports success and yields a file that is not there,
// which surfaces much later as a confusing downstream failure.
func (s *Store) Lookup(ctx context.Context, key string) (*Artifact, error) {
	const query = `
		SELECT id, project_id, stage, input_hash, config_hash, key, status, path,
		       COALESCE(provider, ''), COALESCE(model, ''), COALESCE(prompt_version, ''),
		       size_bytes, metadata_json, created_at
		FROM artifacts
		WHERE project_id = ? AND key = ? AND status = 'ready'`

	var (
		a            Artifact
		metadataJSON string
		createdAt    string
	)
	err := s.db.Read.QueryRowContext(ctx, query, s.projectID, key).Scan(
		&a.ID, &a.ProjectID, &a.Stage, &a.InputHash, &a.ConfigHash, &a.Key, &a.Status, &a.Path,
		&a.Provider, &a.Model, &a.PromptVersion, &a.SizeBytes, &metadataJSON, &createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("artifact: lookup %s: %w", key, err)
	}

	if info, statErr := os.Stat(s.absDir(a.Path)); statErr != nil || !info.IsDir() {
		// Recorded as ready, but the payload is not on disk.
		return nil, nil
	}

	a.Metadata = decodeMetadata(metadataJSON)
	if ts, parseErr := time.Parse(time.RFC3339Nano, createdAt); parseErr == nil {
		a.CreatedAt = ts
	}
	return &a, nil
}

// Latest returns the newest ready artifact for a stage, or nil.
//
// This is what a downstream stage consumes: it wants "the current output of
// segmentation", not a specific content address.
func (s *Store) Latest(ctx context.Context, stage string) (*Artifact, error) {
	const query = `
		SELECT key FROM artifacts
		WHERE project_id = ? AND stage = ? AND status = 'ready'
		ORDER BY created_at DESC, id DESC
		LIMIT 1`

	var key string
	err := s.db.Read.QueryRowContext(ctx, query, s.projectID, stage).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("artifact: latest for stage %s: %w", stage, err)
	}
	return s.Lookup(ctx, key)
}

// Path returns the absolute path of an artifact's primary payload.
//
// The counterpart to Decode, for stages that hand the file to an external
// process rather than reading it: FFmpeg wants a path, not a decoded struct.
func (s *Store) Path(a *Artifact) (string, error) {
	if a == nil {
		return "", errors.New("artifact: cannot resolve the path of a nil artifact")
	}

	dir := s.absDir(a.Path)
	primary, err := PrimaryOf(dir)
	if err != nil {
		return "", fmt.Errorf("artifact: read manifest of %s: %w", a.ID, err)
	}
	return filepath.Join(dir, primary), nil
}

// Decode reads an artifact's primary payload into v.
//
// It exists so a stage can consume an upstream artifact without knowing how
// artifacts are laid out on disk. The moment a stage joins paths itself, the
// layout stops being an implementation detail of this package.
func (s *Store) Decode(a *Artifact, v any) error {
	if a == nil {
		return errors.New("artifact: cannot decode a nil artifact")
	}

	dir := s.absDir(a.Path)
	primary, err := PrimaryOf(dir)
	if err != nil {
		return fmt.Errorf("artifact: read manifest of %s: %w", a.ID, err)
	}

	raw, err := readFile(filepath.Join(dir, primary))
	if err != nil {
		return fmt.Errorf("artifact: read %s of %s: %w", primary, a.ID, err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("artifact: decode %s of %s: %w", primary, a.ID, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Writing
// ---------------------------------------------------------------------------

// Result describes what a stage produced.
type Result struct {
	// Primary is the payload filename a consumer should open first.
	Primary string

	Provider      string
	Model         string
	PromptVersion string

	Metadata map[string]any
}

// Writer collects a stage's output before it is visible.
type Writer struct {
	store *Store
	spec  KeyInputs

	key        string
	inputHash  string
	configHash string
	id         string

	tmpDir string
	dir    string
	done   bool
}

// Begin starts writing an artifact.
//
// Nothing is visible until Commit: the payload is written into a temporary
// directory and renamed into place, so a half-written artifact is never
// observable even if the process is killed mid-run.
func (s *Store) Begin(in KeyInputs) (*Writer, error) {
	key, inputHash, configHash, err := DeriveKey(in)
	if err != nil {
		return nil, err
	}

	id := IDForKey(key)
	stageDir := s.stageDir(in.Stage)
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return nil, fmt.Errorf("artifact: create stage directory %s: %w", stageDir, err)
	}

	// MkdirTemp rather than a name derived from the key: a fixed name would be
	// shared by any two writers of the same artifact, and clobbering a live
	// writer's directory is a much worse failure than leaving a stale one
	// behind, which the startup sweep already handles.
	tmpDir, err := os.MkdirTemp(stageDir, tmpPrefix)
	if err != nil {
		return nil, fmt.Errorf("artifact: create temp directory in %s: %w", stageDir, err)
	}

	return &Writer{
		store:      s,
		spec:       in,
		key:        key,
		inputHash:  inputHash,
		configHash: configHash,
		id:         id,
		tmpDir:     tmpDir,
		dir:        filepath.Join(stageDir, id),
	}, nil
}

// Dir is the directory a stage writes its payload into.
func (w *Writer) Dir() string { return w.tmpDir }

// Key is the artifact's content address.
func (w *Writer) Key() string { return w.key }

// ID is the artifact's short identifier.
func (w *Writer) ID() string { return w.id }

// Commit publishes the artifact and records it.
func (w *Writer) Commit(ctx context.Context, result Result) (*Artifact, error) {
	if w.done {
		return nil, errors.New("artifact: writer already finished")
	}
	w.done = true

	if result.Primary == "" {
		return nil, errors.New("artifact: a result must name its primary payload file")
	}
	if _, err := os.Stat(filepath.Join(w.tmpDir, result.Primary)); err != nil {
		return nil, fmt.Errorf("artifact: declared primary %q is not present: %w", result.Primary, err)
	}

	now := time.Now().UTC()
	manifest := Manifest{
		ID:            w.id,
		ProjectID:     w.store.projectID,
		Stage:         w.spec.Stage,
		InputHash:     w.inputHash,
		ConfigHash:    w.configHash,
		Key:           w.key,
		Provider:      result.Provider,
		Model:         result.Model,
		PromptVersion: result.PromptVersion,
		Primary:       result.Primary,
		Metadata:      result.Metadata,
		CreatedAt:     now,
	}
	if err := writeManifest(w.tmpDir, manifest); err != nil {
		return nil, err
	}

	size, err := dirSize(w.tmpDir)
	if err != nil {
		return nil, err
	}

	// The temporary directory is currently inside the stage directory and so is
	// visible; the rename is what makes it authoritative. Renaming within one
	// directory is atomic on POSIX and on Windows.
	if err := os.RemoveAll(w.dir); err != nil {
		return nil, fmt.Errorf("artifact: clear previous %s: %w", w.dir, err)
	}
	if err := os.Rename(w.tmpDir, w.dir); err != nil {
		return nil, fmt.Errorf("artifact: publish %s: %w", w.dir, err)
	}
	if err := syncDir(filepath.Dir(w.dir)); err != nil {
		return nil, err
	}

	metadataJSON, err := json.Marshal(manifest.Metadata)
	if err != nil {
		return nil, fmt.Errorf("artifact: encode metadata: %w", err)
	}

	a := &Artifact{
		ID:            w.id,
		ProjectID:     w.store.projectID,
		Stage:         w.spec.Stage,
		InputHash:     w.inputHash,
		ConfigHash:    w.configHash,
		Key:           w.key,
		Status:        StatusReady,
		Path:          w.store.relDir(w.spec.Stage, w.id),
		Provider:      result.Provider,
		Model:         result.Model,
		PromptVersion: result.PromptVersion,
		SizeBytes:     size,
		Metadata:      result.Metadata,
		CreatedAt:     now,
	}

	if err := w.store.insert(ctx, a, string(metadataJSON)); err != nil {
		return nil, err
	}

	return a, nil
}

// Abort discards the temporary directory and records the failure.
//
// The row is written rather than skipped so a failed attempt is visible in the
// UI and in the artifact list, instead of being an absence.
func (w *Writer) Abort(ctx context.Context) error {
	w.done = true
	_ = os.RemoveAll(w.tmpDir)

	now := time.Now().UTC()
	a := &Artifact{
		ID:         w.id,
		ProjectID:  w.store.projectID,
		Stage:      w.spec.Stage,
		InputHash:  w.inputHash,
		ConfigHash: w.configHash,
		Key:        w.key,
		Status:     StatusFailed,
		Path:       w.store.relDir(w.spec.Stage, w.id),
		CreatedAt:  now,
	}

	return w.store.insert(ctx, a, "{}")
}

func (s *Store) insert(ctx context.Context, a *Artifact, metadataJSON string) error {
	_, err := s.db.Write.ExecContext(ctx, upsertQuery,
		a.ID, a.ProjectID, a.Stage, a.InputHash, a.ConfigHash, a.Key, string(a.Status), a.Path,
		nullable(a.Provider), nullable(a.Model), nullable(a.PromptVersion),
		a.SizeBytes, metadataJSON, a.CreatedAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("artifact: record %s: %w", a.ID, err)
	}
	return nil
}

// upsertQuery records an artifact, replacing any existing row for the same key.
//
// The replace rather than a plain insert is what makes a retry work. A failed
// attempt writes a row with status 'failed'; retrying the same work derives the
// same key, and a plain INSERT would then fail the unique constraint — so the
// retry could never succeed. Since the ID and path are derived from the key,
// both rows describe the same directory, and the newer attempt simply gets to
// say what state it is in.
const upsertQuery = `
	INSERT INTO artifacts
		(id, project_id, stage, input_hash, config_hash, key, status, path,
		 provider, model, prompt_version, size_bytes, metadata_json, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT (project_id, key) DO UPDATE SET
		status = excluded.status,
		provider = excluded.provider,
		model = excluded.model,
		prompt_version = excluded.prompt_version,
		size_bytes = excluded.size_bytes,
		metadata_json = excluded.metadata_json,
		created_at = excluded.created_at`

// ---------------------------------------------------------------------------
// Retention
// ---------------------------------------------------------------------------

// GC prunes superseded versions of a stage's output.
//
// The newest version is never pruned: it is the one downstream stages consume,
// and a retention policy that removed it would delete the working set.
func (s *Store) GC(ctx context.Context, stage string, retention int) (int, error) {
	if retention < 0 {
		return 0, nil
	}

	const query = `
		SELECT id, path FROM artifacts
		WHERE project_id = ? AND stage = ? AND status IN ('ready','failed')
		ORDER BY created_at DESC, id DESC`

	rows, err := s.db.Read.QueryContext(ctx, query, s.projectID, stage)
	if err != nil {
		return 0, fmt.Errorf("artifact: list %s for GC: %w", stage, err)
	}

	type candidate struct{ id, path string }
	var found []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.path); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("artifact: scan %s for GC: %w", stage, err)
		}
		found = append(found, c)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("artifact: list %s for GC: %w", stage, err)
	}

	// Pruning is housekeeping, not correctness. A failure to delete a directory
	// must not fail the job that triggered it.
	removed := 0
	for _, c := range found[min(retention, len(found)):] {
		_, err := s.db.Write.ExecContext(ctx,
			`DELETE FROM artifacts WHERE id = ? AND project_id = ?`, c.id, s.projectID)
		if err != nil {
			return removed, fmt.Errorf("artifact: delete row %s: %w", c.id, err)
		}
		if err := os.RemoveAll(s.absDir(c.path)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return removed, fmt.Errorf("artifact: delete directory %s: %w", c.path, err)
		}
		removed++
	}
	return removed, nil
}

// SweepTempDirectories removes leftovers from runs that were killed mid-write.
//
// Every temp directory at startup is by definition abandoned: a live writer
// owns its own and no other process should be writing into this project.
func (s *Store) SweepTempDirectories() (int, error) {
	root := filepath.Join(s.projectDir, ArtifactsDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("artifact: sweep temp directories: %w", err)
	}

	removed := 0
	for _, stage := range entries {
		if !stage.IsDir() {
			continue
		}
		stagePath := filepath.Join(root, stage.Name())
		children, err := os.ReadDir(stagePath)
		if err != nil {
			return removed, fmt.Errorf("artifact: sweep %s: %w", stagePath, err)
		}
		for _, child := range children {
			if !strings.HasPrefix(child.Name(), tmpPrefix) {
				continue
			}
			if err := os.RemoveAll(filepath.Join(stagePath, child.Name())); err != nil {
				return removed, fmt.Errorf("artifact: sweep %s: %w", child.Name(), err)
			}
			removed++
		}
	}
	return removed, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func manifestPath(dir string) string { return filepath.Join(dir, "manifest.json") }

func readFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("artifact: read %s: %w", path, err)
	}
	return raw, nil
}

func writeManifest(dir string, manifest Manifest) error {
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("artifact: encode manifest: %w", err)
	}
	raw = append(raw, '\n')

	path := manifestPath(dir)
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("artifact: create manifest: %w", err)
	}
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		return fmt.Errorf("artifact: write manifest: %w", err)
	}
	// The manifest must reach disk before the rename that publishes it, or a
	// crash can leave a published artifact with no description of itself.
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("artifact: sync manifest: %w", err)
	}
	return file.Close()
}

// syncDir flushes a directory entry so the rename survives a crash.
//
// Not all platforms permit syncing a directory; where they do not, the rename
// is still atomic but its durability depends on the filesystem's own ordering.
// That is a reasonable trade rather than a silent one.
func syncDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("artifact: open %s for sync: %w", dir, err)
	}
	defer func() { _ = handle.Close() }()

	if err := handle.Sync(); err != nil {
		if errors.Is(err, os.ErrInvalid) || errors.Is(err, fs.ErrPermission) {
			return nil
		}
		return fmt.Errorf("artifact: sync %s: %w", dir, err)
	}
	return nil
}

func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("artifact: measure %s: %w", dir, err)
	}
	return total, nil
}

func decodeMetadata(raw string) map[string]any {
	if raw == "" || raw == "null" {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		// Metadata is diagnostic. A row written by an older build with a
		// different shape must not make the artifact unreadable.
		return nil
	}
	return out
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
