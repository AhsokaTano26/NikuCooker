package models

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/database"
)

// Status is a model's on-disk state.
type Status string

const (
	StatusMissing     Status = "missing"
	StatusDownloading Status = "downloading"
	StatusReady       Status = "ready"
	StatusError       Status = "error"
)

// Errors callers branch on.
var (
	ErrNotFound    = errors.New("models: not found")
	ErrInUse       = errors.New("models: in use")
	ErrAlreadyBusy = errors.New("models: a download is already running")
	ErrNoDiskSpace = errors.New("models: not enough free space")
	ErrIncomplete  = errors.New("models: the model directory is incomplete")
)

// Record is a model in the inventory.
type Record struct {
	ID       string `json:"id"`
	Kind     Kind   `json:"kind"`
	Name     string `json:"name"`
	Provider string `json:"provider"`

	Path      string `json:"path"`
	Status    Status `json:"status"`
	SizeBytes int64  `json:"size_bytes"`

	Progress     float64 `json:"progress"`
	ErrorMessage string  `json:"error_message,omitempty"`

	InstalledAt *time.Time `json:"installed_at,omitempty"`

	// ApproxBytes is what the catalog expects, so the UI can show what a
	// download will cost before it starts.
	ApproxBytes    int64    `json:"estimated_size_bytes,omitempty"`
	Note           string   `json:"note,omitempty"`
	Recommendation string   `json:"recommendation,omitempty"`
	Accuracy       string   `json:"accuracy,omitempty"`
	Speed          string   `json:"speed,omitempty"`
	Hardware       string   `json:"hardware,omitempty"`
	Language       string   `json:"language,omitempty"`
	Tags           []string `json:"tags,omitempty"`
}

// Service owns the model inventory.
type Service struct {
	db       *database.DB
	modelDir string
	client   *HFClient
	log      *slog.Logger

	mu          sync.Mutex
	downloading map[string]bool
}

// Options configures the service.
type Options struct {
	DB       *database.DB
	ModelDir string
	// Endpoint overrides the Hugging Face host, for users on a network that
	// cannot reach the default one.
	Endpoint string
	Token    string
	Log      *slog.Logger
}

// New builds the service.
func New(opts Options) (*Service, error) {
	if opts.DB == nil {
		return nil, errors.New("models: a database is required")
	}
	if opts.ModelDir == "" {
		return nil, errors.New("models: no model directory configured")
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}

	if err := os.MkdirAll(opts.ModelDir, 0o755); err != nil {
		return nil, fmt.Errorf("models: create %s: %w", opts.ModelDir, err)
	}

	return &Service{
		db:          opts.DB,
		modelDir:    opts.ModelDir,
		client:      NewHFClient(opts.Endpoint, opts.Token),
		log:         opts.Log,
		downloading: map[string]bool{},
	}, nil
}

// Dir reports where a model's files live.
func (s *Service) Dir(kind Kind, name string) string {
	return filepath.Join(s.modelDir, string(kind), name)
}

// Root reports the model directory.
func (s *Service) Root() string { return s.modelDir }

// Resolve returns the on-disk directory of a ready model.
//
// The core owns downloads and the worker owns loading, so the worker must be
// told *where* the model is. Passing a bare name instead would send it to the
// Hugging Face cache, which means the core's inventory and the worker's copy
// disagree — and a user who downloaded a model once would silently download it
// a second time.
// The kind is a string rather than a Kind so that this satisfies
// stage.ModelLocator without the stage package importing this one: a stage
// declaring a dependency on the model inventory would invert the layering the
// pipeline is built on.
func (s *Service) Resolve(kind string, name string) (string, bool) {
	dir := s.Dir(Kind(kind), name)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return "", false
	}
	if _, complete := s.measure(dir); !complete {
		return "", false
	}
	return dir, true
}

// ---------------------------------------------------------------------------
// Inventory
// ---------------------------------------------------------------------------

// Scan reconciles the inventory with the filesystem and returns it.
//
// The filesystem wins. A user who deletes a model directory by hand should see
// `missing`, not a stale `ready` that makes a job fail with a confusing loader
// error later. The database row is a cache of this scan, not the truth.
func (s *Service) Scan(ctx context.Context) ([]Record, error) {
	var records []Record

	for _, entry := range Catalog {
		record := s.inspect(entry)
		if err := s.persist(ctx, record); err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	// The VAD ships inside the faster-whisper wheel, so it is present whenever
	// the Python environment is. Recording it keeps the UI honest rather than
	// showing an empty list next to a working VAD.
	records = append(records, Record{
		ID:             ID(KindVAD, "silero"),
		Kind:           KindVAD,
		Name:           "silero",
		Provider:       "silero-onnx",
		Status:         StatusReady,
		Note:           "随 faster-whisper 一起安装，用于找出音频中有人说话的区间，无需单独下载。",
		Recommendation: "自动使用，无需选择", Speed: "很快",
		Hardware: "CPU 即可", Language: "与语言无关", Tags: []string{"内置"},
	})

	return records, nil
}

// inspect checks one catalog entry against the filesystem.
func (s *Service) inspect(entry Entry) Record {
	record := Record{
		ID:             ID(entry.Kind, entry.Name),
		Kind:           entry.Kind,
		Name:           entry.Name,
		Provider:       entry.Provider,
		Path:           s.Dir(entry.Kind, entry.Name),
		ApproxBytes:    entry.ApproxBytes,
		Note:           entry.Note,
		Recommendation: entry.Recommendation,
		Accuracy:       entry.Accuracy,
		Speed:          entry.Speed,
		Hardware:       entry.Hardware,
		Language:       entry.Language,
		Tags:           append([]string(nil), entry.Tags...),
	}

	info, err := os.Stat(record.Path)
	if err != nil || !info.IsDir() {
		record.Status = StatusMissing
		return record
	}

	size, complete := s.measure(record.Path)
	record.SizeBytes = size

	if !complete {
		// A directory with files but no weights is what an interrupted
		// download leaves behind. Reporting it as ready would let a job start
		// and fail inside the loader, far from the cause.
		record.Status = StatusError
		record.ErrorMessage = "the model directory is incomplete; download it again"
		return record
	}

	record.Status = StatusReady
	installed := info.ModTime().UTC()
	record.InstalledAt = &installed
	record.Progress = 1
	return record
}

// measure sums a directory and reports whether it holds usable weights.
func (s *Service) measure(dir string) (int64, bool) {
	var total int64
	hasWeights := false

	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()

		switch filepath.Base(path) {
		case "model.bin", "model.safetensors":
			// The presence of weights is what makes a directory loadable;
			// config and tokenizer files alone are not.
			if info.Size() > 0 {
				hasWeights = true
			}
		}
		return nil
	})

	return total, hasWeights
}

// List returns the inventory.
func (s *Service) List(ctx context.Context) ([]Record, error) {
	records, err := s.Scan(ctx)
	if err != nil {
		return nil, err
	}

	// In-flight downloads are not visible on disk yet, and the scan would
	// report them as missing.
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range records {
		if s.downloading[records[i].ID] {
			records[i].Status = StatusDownloading
		}
	}
	return records, nil
}

// Get returns one record.
func (s *Service) Get(ctx context.Context, id string) (Record, error) {
	records, err := s.List(ctx)
	if err != nil {
		return Record{}, err
	}
	for _, record := range records {
		if record.ID == id {
			return record, nil
		}
	}
	return Record{}, fmt.Errorf("%w: %s", ErrNotFound, id)
}

// ---------------------------------------------------------------------------
// Download
// ---------------------------------------------------------------------------

// Download fetches a model.
//
// onProgress is called with the running total; it may be nil.
func (s *Service) Download(ctx context.Context, id string, onProgress func(written, total int64)) error {
	kind, name, ok := SplitID(id)
	if !ok {
		return fmt.Errorf("models: %q is not a model id; expected kind:name", id)
	}

	entry, ok := Lookup(kind, name)
	if !ok {
		// The VAD is bundled, so "downloading" it is a no-op rather than an
		// error: the user asked for it to be available, and it is.
		if kind == KindVAD {
			return nil
		}
		var known []string
		for _, e := range Catalog {
			known = append(known, ID(e.Kind, e.Name))
		}
		return fmt.Errorf("%w: %s (known models: %s)", ErrNotFound, id, strings.Join(known, ", "))
	}

	s.mu.Lock()
	if s.downloading[id] {
		s.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrAlreadyBusy, id)
	}
	s.downloading[id] = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.downloading, id)
		s.mu.Unlock()
	}()

	if err := s.checkSpace(entry); err != nil {
		return err
	}

	s.log.Info("downloading a model", "id", id, "repo", entry.Repo)

	files, err := s.client.ListFiles(ctx, entry.Repo)
	if err != nil {
		return err
	}

	destination := s.Dir(entry.Kind, entry.Name)
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return fmt.Errorf("models: create %s: %w", destination, err)
	}

	for _, file := range files {
		// Directories in the listing are not files to fetch.
		if strings.HasSuffix(file.Name, "/") {
			continue
		}

		target := filepath.Join(destination, filepath.FromSlash(file.Name))
		written, err := s.client.DownloadFile(ctx, entry.Repo, file.Name, target, onProgress)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			return fmt.Errorf("models: download %s: %w", id, err)
		}
		s.log.Debug("fetched", "id", id, "file", file.Name, "bytes", written)
	}

	// A model without weights is not a model, and saying so here is far cheaper
	// than discovering it when a job fails to load.
	if _, complete := s.measure(destination); !complete {
		return fmt.Errorf("%w: %s has no model weights after downloading", ErrIncomplete, id)
	}

	if _, err := s.Scan(ctx); err != nil {
		return err
	}

	s.log.Info("model ready", "id", id)
	return nil
}

// checkSpace refuses a download that will not fit.
//
// Refusing up front is much better than failing at 90%: a user who is told
// "needs 3 GB, 1.2 GB free" can act, and one whose download fails at 90%
// cannot tell whether the problem is the disk or the network.
func (s *Service) checkSpace(entry Entry) error {
	if entry.ApproxBytes <= 0 {
		return nil
	}

	free, err := freeSpace(s.modelDir)
	if err != nil {
		// Not knowing is not a reason to refuse.
		return nil
	}

	// A gigabyte of headroom: the estimate is approximate, and a model that
	// just fits leaves nothing for the artifacts of the job that uses it.
	const headroom = uint64(1) << 30
	needed := uint64(max(entry.ApproxBytes, 0)) + headroom

	if free < needed {
		return fmt.Errorf("%w: %s needs about %s, and %s has %s free",
			ErrNoDiskSpace, ID(entry.Kind, entry.Name),
			humanBytes(entry.ApproxBytes), s.modelDir, humanBytes(int64(free)))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Delete
// ---------------------------------------------------------------------------

// Delete removes a model's files.
//
// inUse reports whether a running job references the model; the caller supplies
// it because only the caller knows about jobs.
func (s *Service) Delete(ctx context.Context, id string, inUse bool) error {
	if inUse {
		return fmt.Errorf("%w: %s is used by a running job", ErrInUse, id)
	}

	kind, name, ok := SplitID(id)
	if !ok {
		return fmt.Errorf("models: %q is not a model id", id)
	}
	if kind == KindVAD {
		return fmt.Errorf("models: the VAD model is bundled and cannot be removed")
	}

	dir := s.Dir(kind, name)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}

	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("models: remove %s: %w", dir, err)
	}

	if _, err := s.Scan(ctx); err != nil {
		return err
	}

	s.log.Info("model removed", "id", id)
	return nil
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

// persist records a scan result.
//
// Best effort: the scan's answer is what callers use, and a failure to cache it
// must not turn a working inventory into an error. The row exists so other
// parts of the system — the API, the dashboard — can query without walking the
// filesystem.
func (s *Service) persist(ctx context.Context, record Record) error {
	var installedAt any
	if record.InstalledAt != nil {
		installedAt = record.InstalledAt.Format(time.RFC3339Nano)
	}

	var errorMessage any
	if record.ErrorMessage != "" {
		errorMessage = record.ErrorMessage
	}

	_, err := s.db.Write.ExecContext(ctx, `
		INSERT INTO model_records
			(id, kind, name, provider, path, status, size_bytes, progress,
			 error_message, installed_at, checked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (kind, name) DO UPDATE SET
			provider = excluded.provider,
			path = excluded.path,
			status = excluded.status,
			size_bytes = excluded.size_bytes,
			progress = excluded.progress,
			error_message = excluded.error_message,
			installed_at = excluded.installed_at,
			checked_at = excluded.checked_at`,
		record.ID, string(record.Kind), record.Name, record.Provider,
		record.Path, string(record.Status), record.SizeBytes, record.Progress,
		errorMessage, installedAt, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		s.log.Warn("could not cache the model inventory", "id", record.ID, "error", err)
	}
	return nil
}

// Cached reads the last recorded inventory without touching the filesystem.
//
// For the API's list endpoint, where a full scan on every request would walk
// gigabytes of files to answer a question whose answer rarely changes.
func (s *Service) Cached(ctx context.Context) ([]Record, error) {
	rows, err := s.db.Read.QueryContext(ctx, `
		SELECT id, kind, name, provider, path, status, size_bytes, progress,
		       COALESCE(error_message, ''), installed_at
		FROM model_records ORDER BY kind, name`)
	if err != nil {
		return nil, fmt.Errorf("models: list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var records []Record
	for rows.Next() {
		var (
			record       Record
			kind, status string
			installedAt  sql.NullString
		)
		if err := rows.Scan(&record.ID, &kind, &record.Name, &record.Provider,
			&record.Path, &status, &record.SizeBytes, &record.Progress,
			&record.ErrorMessage, &installedAt); err != nil {
			return nil, fmt.Errorf("models: scan: %w", err)
		}
		record.Kind = Kind(kind)
		record.Status = Status(status)

		if installedAt.Valid {
			if parsed, err := time.Parse(time.RFC3339Nano, installedAt.String); err == nil {
				record.InstalledAt = &parsed
			}
		}
		if entry, ok := Lookup(record.Kind, record.Name); ok {
			record.ApproxBytes = entry.ApproxBytes
			record.Note = entry.Note
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for value := n / unit; value >= unit; value /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
