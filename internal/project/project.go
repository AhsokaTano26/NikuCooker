// Package project owns the project aggregate: its identity, its directory, and
// the configuration overlay that applies to it.
//
// Every project is a self-contained directory, so a user can copy it, move it,
// or back it up with a file manager and nothing breaks. That property is what
// requires paths to be stored relative to the project directory rather than
// absolute.
package project

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/AhsokaTano26/NikuCooker/internal/database"
)

// ErrNotFound reports a project that does not exist.
var ErrNotFound = errors.New("project: not found")

// Status is a project's lifecycle state.
type Status string

const (
	StatusActive   Status = "active"
	StatusArchived Status = "archived"
)

// Project is one video and its translation.
type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`

	// SourcePath is relative to the project directory, so the whole data tree
	// can be relocated without rewriting rows.
	SourcePath string `json:"source_path"`

	SourceLanguage string `json:"source_language"` // BCP-47
	TargetLanguage string `json:"target_language"` // BCP-47
	Style          string `json:"style"`           // literal | natural | fansub
	Status         Status `json:"status"`

	// Config is a sparse overlay on the global configuration. Absent keys
	// inherit; present keys win.
	Config map[string]any `json:"config"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Dir returns the project's directory under the data root.
func Dir(dataDir, id string) string {
	return filepath.Join(dataDir, "projects", id)
}

// Service reads and writes projects.
type Service struct {
	db      *database.DB
	dataDir string
}

// NewService binds a service to a data directory.
func NewService(db *database.DB, dataDir string) *Service {
	return &Service{db: db, dataDir: dataDir}
}

// DataDir reports the root under which projects live.
func (s *Service) DataDir() string { return s.dataDir }

// Subdirectories every project has.
const (
	DirSource    = "source"
	DirArtifacts = "artifacts"
	DirOutput    = "output"
	DirLogs      = "logs"
)

// CreateRequest describes a new project.
type CreateRequest struct {
	Name           string
	SourceLanguage string
	TargetLanguage string
	Style          string
	Config         map[string]any

	// SourceName is the original filename, used only to derive an extension.
	// It never participates in a path.
	SourceName string

	// SourceOrigin is where the media is now, so it can be moved into place.
	// When empty, the project is created with no source and the caller supplies
	// one later.
	SourceOrigin string
}

// Create scaffolds a project and its directory.
func (s *Service) Create(ctx context.Context, req CreateRequest) (*Project, error) {
	if err := validateCreate(req); err != nil {
		return nil, err
	}

	id := uuid.Must(uuid.NewV7()).String()
	dir := Dir(s.dataDir, id)

	if err := scaffold(dir); err != nil {
		return nil, err
	}

	sourceRel := ""
	if req.SourceOrigin != "" {
		rel, err := placeSource(dir, req.SourceOrigin, req.SourceName)
		if err != nil {
			// A project whose directory exists but whose row does not is an
			// orphan; cleaning it up here keeps the data tree matching the
			// database.
			_ = os.RemoveAll(dir)
			return nil, err
		}
		sourceRel = rel
	}

	now := time.Now().UTC()
	configJSON := "{}"
	if len(req.Config) > 0 {
		encoded, err := json.Marshal(req.Config)
		if err != nil {
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("project: encode config: %w", err)
		}
		configJSON = string(encoded)
	}

	_, err := s.db.Write.ExecContext(ctx, `
		INSERT INTO projects (id, name, source_path, source_language, target_language,
		                      style, status, config_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 'active', ?, ?, ?)`,
		id, req.Name, sourceRel, req.SourceLanguage, req.TargetLanguage, req.Style,
		configJSON, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("project: insert: %w", err)
	}

	return &Project{
		ID:             id,
		Name:           req.Name,
		SourcePath:     sourceRel,
		SourceLanguage: req.SourceLanguage,
		TargetLanguage: req.TargetLanguage,
		Style:          req.Style,
		Status:         StatusActive,
		Config:         req.Config,
		CreatedAt:      now,
		UpdatedAt:      now,
	}, nil
}

// Get loads a project by ID.
func (s *Service) Get(ctx context.Context, id string) (*Project, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}

	const query = `
		SELECT id, name, source_path, source_language, target_language, style,
		       status, config_json, created_at, updated_at
		FROM projects WHERE id = ?`

	var (
		p          Project
		configJSON string
		createdAt  string
		updatedAt  string
	)
	err := s.db.Read.QueryRowContext(ctx, query, id).Scan(
		&p.ID, &p.Name, &p.SourcePath, &p.SourceLanguage, &p.TargetLanguage, &p.Style,
		&p.Status, &configJSON, &createdAt, &updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("project: get %s: %w", id, err)
	}

	p.Config = decodeConfig(configJSON)
	p.CreatedAt = parseTime(createdAt)
	p.UpdatedAt = parseTime(updatedAt)
	return &p, nil
}

// SourceAbs resolves a project's source file path on disk.
func (s *Service) SourceAbs(p *Project) string {
	if p.SourcePath == "" {
		return ""
	}
	return filepath.Join(Dir(s.dataDir, p.ID), filepath.FromSlash(p.SourcePath))
}

// ListQuery filters List.
type ListQuery struct {
	Status Status
	Search string
	Limit  int
	Offset int
}

// List returns projects, newest first.
func (s *Service) List(ctx context.Context, q ListQuery) ([]*Project, int, error) {
	if q.Limit <= 0 {
		q.Limit = 50
	}

	where := []string{"1 = 1"}
	args := []any{}

	if q.Status != "" && q.Status != "all" {
		where = append(where, "status = ?")
		args = append(args, string(q.Status))
	}
	if q.Search != "" {
		where = append(where, "name LIKE ?")
		args = append(args, "%"+q.Search+"%")
	}
	clause := strings.Join(where, " AND ")

	var total int
	if err := s.db.Read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM projects WHERE "+clause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("project: count: %w", err)
	}

	query := `
		SELECT id, name, source_path, source_language, target_language, style,
		       status, config_json, created_at, updated_at
		FROM projects WHERE ` + clause + `
		ORDER BY created_at DESC, id DESC
		LIMIT ? OFFSET ?`

	rows, err := s.db.Read.QueryContext(ctx, query, append(args, q.Limit, q.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("project: list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Project
	for rows.Next() {
		var (
			p          Project
			configJSON string
			createdAt  string
			updatedAt  string
		)
		if err := rows.Scan(&p.ID, &p.Name, &p.SourcePath, &p.SourceLanguage,
			&p.TargetLanguage, &p.Style, &p.Status, &configJSON, &createdAt, &updatedAt); err != nil {
			return nil, 0, fmt.Errorf("project: scan: %w", err)
		}
		p.Config = decodeConfig(configJSON)
		p.CreatedAt = parseTime(createdAt)
		p.UpdatedAt = parseTime(updatedAt)
		out = append(out, &p)
	}
	return out, total, rows.Err()
}

// Delete removes a project.
//
// Files are removed only when the caller explicitly asks. The safe default is
// to drop the rows and leave the media on disk, because a delete that silently
// destroys a user's source video is not recoverable.
func (s *Service) Delete(ctx context.Context, id string, deleteFiles bool) error {
	p, err := s.Get(ctx, id)
	if err != nil {
		return err
	}

	if _, err := s.db.Write.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id); err != nil {
		return fmt.Errorf("project: delete %s: %w", id, err)
	}

	if deleteFiles {
		if err := os.RemoveAll(Dir(s.dataDir, p.ID)); err != nil {
			return fmt.Errorf("project: remove directory for %s: %w", id, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Path safety
// ---------------------------------------------------------------------------

// validateID rejects anything that could escape the projects directory.
//
// Project IDs are server-generated UUIDs, so a malformed one is either a bug or
// an attack. Both warrant refusal rather than sanitisation: sanitising a
// traversal attempt produces a path that exists and is wrong.
func validateID(id string) error {
	if id == "" {
		return fmt.Errorf("project: empty id")
	}
	if _, err := uuid.Parse(id); err != nil {
		return fmt.Errorf("project: %q is not a valid project id", id)
	}
	return nil
}

// sourceExtensions is the allowlist of media extensions.
//
// An allowlist rather than a denylist: the extension reaches the filesystem,
// and "everything except the dangerous ones" is a list that is always one entry
// out of date.
var sourceExtensions = map[string]bool{
	".mp4": true, ".mkv": true, ".mov": true, ".avi": true, ".webm": true,
	".flv": true, ".ts": true, ".m2ts": true, ".mpg": true, ".mpeg": true,
	".wmv": true, ".m4v": true,
	".mp3": true, ".m4a": true, ".aac": true, ".flac": true, ".wav": true,
	".ogg": true, ".opus": true, ".wma": true,
	".srt": true, ".ass": true, ".vtt": true,
}

// safeExtension extracts an extension from a user-supplied filename.
//
// The filename itself never reaches the filesystem. Only its extension does,
// and only after being checked against the allowlist — which is what makes a
// name like "../../etc/passwd" merely an unknown extension rather than a path.
func safeExtension(filename string) string {
	// filepath.Base first, so a path separator in the name cannot survive into
	// the extension lookup.
	base := filepath.Base(filepath.FromSlash(filename))
	ext := strings.ToLower(filepath.Ext(base))

	if ext == "" || len(ext) > 10 {
		return ""
	}
	if !sourceExtensions[ext] {
		return ""
	}
	return ext
}

// placeSource moves or copies the media into the project directory under a
// generated name.
func placeSource(projectDir, origin, originalName string) (string, error) {
	info, err := os.Stat(origin)
	if err != nil {
		return "", fmt.Errorf("project: source %s: %w", origin, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("project: source %s is a directory", origin)
	}

	ext := safeExtension(originalName)
	if ext == "" {
		ext = safeExtension(origin)
	}
	if ext == "" {
		return "", fmt.Errorf("project: cannot determine a supported media extension for %q", originalName)
	}

	sourceDir := filepath.Join(projectDir, DirSource)
	dest := filepath.Join(sourceDir, "source"+ext)

	if err := os.Rename(origin, dest); err != nil {
		// A cross-device rename fails; fall back to a copy.
		if err := copyFile(origin, dest); err != nil {
			return "", fmt.Errorf("project: move source into place: %w", err)
		}
		_ = os.Remove(origin)
	}

	return DirSource + "/source" + ext, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// scaffold creates a project's directory tree.
func scaffold(dir string) error {
	for _, sub := range []string{DirSource, DirArtifacts, DirOutput, DirLogs} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return fmt.Errorf("project: create %s: %w", filepath.Join(dir, sub), err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func validateCreate(req CreateRequest) error {
	var problems []string

	if strings.TrimSpace(req.Name) == "" {
		problems = append(problems, "name must not be empty")
	}
	if req.SourceLanguage == "" {
		problems = append(problems, "source_language must not be empty")
	}
	if req.TargetLanguage == "" {
		problems = append(problems, "target_language must not be empty")
	}
	// Translating a language into itself is almost always a mistake, and
	// catching it here is far cheaper than after a transcription.
	if req.SourceLanguage != "" && req.SourceLanguage == req.TargetLanguage {
		problems = append(problems,
			fmt.Sprintf("source and target language are both %q", req.SourceLanguage))
	}
	switch req.Style {
	case "literal", "natural", "fansub":
	default:
		problems = append(problems,
			fmt.Sprintf("style %q is not one of literal, natural, fansub", req.Style))
	}

	if len(problems) > 0 {
		return fmt.Errorf("project: %s", strings.Join(problems, "; "))
	}
	return nil
}

func decodeConfig(raw string) map[string]any {
	if raw == "" || raw == "{}" {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func parseTime(raw string) time.Time {
	ts, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return ts
}
