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

// IdleSince returns the projects nothing has touched for a period.
//
// "Touched" means created, edited or run, and the third is the one that needs
// care: running a project does not write its row, so `updated_at` alone would
// call a project idle that was run this morning because it was created last
// year. The most recent run is read from the jobs table, which has an index for
// exactly this lookup.
//
// A project whose newest run is still going is not filtered out here — this
// asks a question about timestamps, and whether something is running right now
// is a different question, asked by the caller that knows about the scheduler.
func (s *Service) IdleSince(ctx context.Context, idle time.Duration) ([]*Project, error) {
	const query = `
		SELECT p.id, p.name, p.source_path, p.source_language, p.target_language,
		       p.style, p.status, p.config_json, p.created_at, p.updated_at
		FROM projects p
		LEFT JOIN (
			SELECT project_id, MAX(created_at) AS last_run FROM jobs GROUP BY project_id
		) j ON j.project_id = p.id
		WHERE MAX(p.updated_at, COALESCE(j.last_run, '')) < ?
		ORDER BY p.updated_at`

	cutoff := time.Now().Add(-idle).UTC().Format(time.RFC3339Nano)

	rows, err := s.db.Read.QueryContext(ctx, query, cutoff)
	if err != nil {
		return nil, fmt.Errorf("project: list idle: %w", err)
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
			&p.TargetLanguage, &p.Style, &p.Status, &configJSON,
			&createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("project: scan idle: %w", err)
		}

		p.Config = decodeConfig(configJSON)
		p.CreatedAt = parseTime(createdAt)
		p.UpdatedAt = parseTime(updatedAt)
		out = append(out, &p)
	}

	return out, rows.Err()
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
// validateID reports whether a string could be a project identifier.
//
// An identifier that is not one cannot name a project that exists, so callers
// report it as not-found rather than as a malformed request. The distinction
// matters at the edge: a URL path segment holding a deleted project's id and one
// holding a typo are the same situation to a client, and answering one with a
// 400 and the other with a 404 gives it two cases to handle where it has one.
func validateID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: no id was given", ErrNotFound)
	}
	if _, err := uuid.Parse(id); err != nil {
		return fmt.Errorf("%w: %q is not a project id", ErrNotFound, id)
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
	return validateFields(req.Name, req.SourceLanguage, req.TargetLanguage, req.Style)
}

// validateFields checks the project fields that are validated identically
// whether a project is being created or changed.
//
// Shared rather than duplicated, because an update that skipped a rule a create
// enforces is a way to reach a state the rest of the system assumes is
// impossible — and it is exactly the rule that would be forgotten.
func validateFields(name, sourceLanguage, targetLanguage, style string) error {
	var problems []string

	if strings.TrimSpace(name) == "" {
		problems = append(problems, "name must not be empty")
	}
	if sourceLanguage == "" {
		problems = append(problems, "source_language must not be empty")
	}
	if targetLanguage == "" {
		problems = append(problems, "target_language must not be empty")
	}
	// Translating a language into itself is almost always a mistake, and
	// catching it here is far cheaper than after a transcription.
	if sourceLanguage != "" && sourceLanguage == targetLanguage {
		problems = append(problems,
			fmt.Sprintf("source and target language are both %q", sourceLanguage))
	}
	switch style {
	case "literal", "natural", "fansub":
	default:
		problems = append(problems,
			fmt.Sprintf("style %q is not one of literal, natural, fansub", style))
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

// UpdateRequest describes a change to a project. A nil field is left alone.
type UpdateRequest struct {
	Name           *string
	SourceLanguage *string
	TargetLanguage *string
	Style          *string
	Config         map[string]any
}

// Update applies a change to a project.
//
// A nil field is untouched rather than cleared. A PATCH carries only what the
// caller meant to change, and treating an absent field as "set it to empty"
// would make every partial update destroy the rest of the record.
func (s *Service) Update(ctx context.Context, id string, req UpdateRequest) (*Project, error) {
	p, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	if req.Name != nil {
		p.Name = strings.TrimSpace(*req.Name)
		if p.Name == "" {
			return nil, fmt.Errorf("project: a project needs a name")
		}
	}
	if req.SourceLanguage != nil {
		p.SourceLanguage = *req.SourceLanguage
	}
	if req.TargetLanguage != nil {
		p.TargetLanguage = *req.TargetLanguage
	}
	if req.Style != nil {
		p.Style = *req.Style
	}
	if req.Config != nil {
		p.Config = req.Config
	}

	if err := validateFields(p.Name, p.SourceLanguage, p.TargetLanguage, p.Style); err != nil {
		return nil, err
	}

	configJSON := "{}"
	if len(p.Config) > 0 {
		encoded, err := json.Marshal(p.Config)
		if err != nil {
			return nil, fmt.Errorf("project: encode config: %w", err)
		}
		configJSON = string(encoded)
	}

	now := time.Now().UTC()
	_, err = s.db.Write.ExecContext(ctx, `
		UPDATE projects
		SET name = ?, source_language = ?, target_language = ?, style = ?,
		    config_json = ?, updated_at = ?
		WHERE id = ?`,
		p.Name, p.SourceLanguage, p.TargetLanguage, p.Style,
		configJSON, now.Format(time.RFC3339Nano), id)
	if err != nil {
		return nil, fmt.Errorf("project: update %s: %w", id, err)
	}

	p.UpdatedAt = now
	return p, nil
}
