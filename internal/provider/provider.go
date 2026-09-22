// Package provider holds the external services NikuCooker talks to, and the
// HTTP client that talks to them.
//
// Only one kind of provider exists in v1 — an OpenAI-compatible chat endpoint —
// but the record is general because the database has to describe a provider
// before anything knows how to call it, and a user configuring one from the UI
// is not editing code.
package provider

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/AhsokaTano26/NikuCooker/internal/database"
)

// ErrNotFound reports a provider that does not exist.
var ErrNotFound = errors.New("provider: not found")

// Kind is what a provider is used for.
type Kind string

const (
	// KindLLM covers translation and the analysis pass.
	KindLLM Kind = "llm"
	// KindASR covers remote transcription. Nothing implements it in v1; local
	// recognition is the Python worker's job, and a remote ASR provider is a
	// deliberate omission rather than an oversight.
	KindASR Kind = "asr"
)

// Type is the wire protocol a provider speaks.
type Type string

const (
	// TypeOpenAICompatible is the de facto standard: OpenAI, together with
	// DeepSeek, Moonshot, Groq, Together, OpenRouter, llama.cpp, Ollama, vLLM
	// and LM Studio, all accept this request shape.
	TypeOpenAICompatible Type = "openai-compatible"

	// TypeFasterWhisper names the local recogniser, which is reached through the
	// Python worker rather than over HTTP. It has no base URL and no key.
	TypeFasterWhisper Type = "faster-whisper"
)

// Provider is a configured external service.
type Provider struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind Kind   `json:"kind"`
	Type Type   `json:"type"`

	BaseURL string `json:"base_url,omitempty"`

	// APIKey is never serialised out of the core.
	//
	// The tag is the enforcement. Leaking a key through a handler then requires
	// writing an explicit conversion, which is a visible thing to do in review,
	// rather than forgetting a field, which is not.
	APIKey string `json:"-"`

	Model   string `json:"model,omitempty"`
	Enabled bool   `json:"enabled"`

	// Extra carries provider-specific settings — an organisation header, a
	// custom token-limit field name — without a schema change for each one.
	Extra map[string]any `json:"extra,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Validate checks a provider before it is stored.
func (p *Provider) Validate() error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return errors.New("provider: a provider needs a name")
	}
	if p.Kind == "" {
		p.Kind = KindLLM
	}
	if p.Kind != KindLLM && p.Kind != KindASR {
		return fmt.Errorf("provider: %q is not a known provider kind", p.Kind)
	}
	if p.Type == "" {
		p.Type = TypeOpenAICompatible
	}

	switch p.Type {
	case TypeOpenAICompatible:
		// Checked here rather than at request time, so a malformed URL is
		// reported while the user is looking at the form that produced it.
		if _, err := chatEndpoint(p.BaseURL); err != nil {
			return err
		}
	case TypeFasterWhisper:
		if p.BaseURL != "" {
			return errors.New("provider: a local recogniser has no base URL")
		}
	default:
		return fmt.Errorf("provider: %q is not a known provider type", p.Type)
	}

	if p.Model == "" && p.Type == TypeOpenAICompatible {
		return errors.New("provider: an OpenAI-compatible provider needs a model name")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Storage
// ---------------------------------------------------------------------------

// Service reads and writes provider records.
type Service struct {
	db *database.DB
}

// NewService binds a service to a database.
func NewService(db *database.DB) *Service { return &Service{db: db} }

const providerColumns = `id, name, kind, type, base_url, api_key, model,
	enabled, extra_json, created_at, updated_at`

// List returns the configured providers.
func (s *Service) List(ctx context.Context, kind Kind) ([]Provider, error) {
	query := `SELECT ` + providerColumns + ` FROM providers`
	var args []any
	if kind != "" {
		query += ` WHERE kind = ?`
		args = append(args, string(kind))
	}
	query += ` ORDER BY name ASC`

	rows, err := s.db.Read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("provider: list: %w", err)
	}
	defer rows.Close()

	providers := []Provider{}
	for rows.Next() {
		record, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		providers = append(providers, *record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("provider: list: %w", err)
	}
	return providers, nil
}

// Get returns one provider by id.
func (s *Service) Get(ctx context.Context, id string) (*Provider, error) {
	row := s.db.Read.QueryRowContext(ctx,
		`SELECT `+providerColumns+` FROM providers WHERE id = ?`, id)

	record, err := scanProvider(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return record, err
}

// GetByName returns one provider by its display name.
func (s *Service) GetByName(ctx context.Context, name string, kind Kind) (*Provider, error) {
	row := s.db.Read.QueryRowContext(ctx,
		`SELECT `+providerColumns+` FROM providers WHERE name = ? AND kind = ?`,
		name, string(kind))

	record, err := scanProvider(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return record, err
}

// Default returns the enabled provider to use for a kind when the caller did not
// name one.
//
// A single enabled provider is unambiguous and is chosen without being asked.
// Several is not a choice this function can make on the user's behalf, so it
// declines rather than picking the first alphabetically and quietly using a
// service the user did not mean.
func (s *Service) Default(ctx context.Context, kind Kind) (*Provider, error) {
	providers, err := s.List(ctx, kind)
	if err != nil {
		return nil, err
	}

	var enabled []Provider
	for _, record := range providers {
		if record.Enabled {
			enabled = append(enabled, record)
		}
	}

	switch len(enabled) {
	case 0:
		return nil, fmt.Errorf("provider: no enabled %s provider is configured", kind)
	case 1:
		return &enabled[0], nil
	default:
		names := make([]string, 0, len(enabled))
		for _, record := range enabled {
			names = append(names, record.Name)
		}
		return nil, fmt.Errorf(
			"provider: several %s providers are enabled (%s); name the one to use",
			kind, strings.Join(names, ", "))
	}
}

// Save inserts or updates a provider.
//
// An empty APIKey on an existing provider leaves the stored key alone. The key
// never leaves the core, so the UI cannot round-trip it, and treating an empty
// value as "clear it" would erase a working configuration every time a user
// renamed the provider.
func (s *Service) Save(ctx context.Context, record *Provider) error {
	if err := record.Validate(); err != nil {
		return err
	}

	now := time.Now().UTC()
	if record.ID == "" {
		record.ID = "prv_" + uuid.Must(uuid.NewV7()).String()
		record.CreatedAt = now
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}
	record.UpdatedAt = now

	extra, err := marshalExtra(record.Extra)
	if err != nil {
		return err
	}

	_, err = s.db.Write.ExecContext(ctx, `
		INSERT INTO providers (
			id, name, kind, type, base_url, api_key, model,
			enabled, extra_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name, kind = excluded.kind, type = excluded.type,
			base_url = excluded.base_url,
			api_key = CASE WHEN excluded.api_key = '' THEN providers.api_key
			               ELSE excluded.api_key END,
			model = excluded.model, enabled = excluded.enabled,
			extra_json = excluded.extra_json, updated_at = excluded.updated_at`,
		record.ID, record.Name, string(record.Kind), string(record.Type),
		record.BaseURL, record.APIKey, record.Model,
		boolToInt(record.Enabled), extra,
		record.CreatedAt.Format(time.RFC3339Nano), record.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("provider: save %q: %w", record.Name, err)
	}
	return nil
}

// Delete removes a provider.
func (s *Service) Delete(ctx context.Context, id string) error {
	result, err := s.db.Write.ExecContext(ctx, `DELETE FROM providers WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("provider: delete %s: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("provider: delete %s: %w", id, err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scanning
// ---------------------------------------------------------------------------

type scanner interface {
	Scan(dest ...any) error
}

func scanProvider(row scanner) (*Provider, error) {
	var (
		record    Provider
		kind      string
		typ       string
		baseURL   sql.NullString
		apiKey    sql.NullString
		model     sql.NullString
		enabled   int
		extraJSON string
		created   string
		updated   string
	)

	err := row.Scan(&record.ID, &record.Name, &kind, &typ, &baseURL, &apiKey,
		&model, &enabled, &extraJSON, &created, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("provider: read record: %w", err)
	}

	record.Kind = Kind(kind)
	record.Type = Type(typ)
	record.BaseURL = baseURL.String
	record.APIKey = apiKey.String
	record.Model = model.String
	record.Enabled = enabled != 0
	record.CreatedAt = parseTime(created)
	record.UpdatedAt = parseTime(updated)

	extra, err := unmarshalExtra(extraJSON)
	if err != nil {
		return nil, err
	}
	record.Extra = extra

	return &record, nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func parseTime(value string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return t
}
