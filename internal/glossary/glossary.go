// Package glossary owns the terminology a translation must respect.
//
// It exists because the failure it prevents is the most damaging one a fansub
// can have: a character whose name drifts between episodes, a technique that is
// translated three different ways in one file. The translator cannot know any of
// this, so the terms travel with the request.
//
// Two kinds of entry share one table. A *global* entry belongs to the user —
// a series they watch regularly — and applies everywhere. A *project* entry
// belongs to one video. Where the two disagree the project wins, because that is
// the more specific statement of intent.
package glossary

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/AhsokaTano26/NikuCooker/internal/artifact"
	"github.com/AhsokaTano26/NikuCooker/internal/database"
)

// ErrNotFound reports an entry that does not exist.
var ErrNotFound = errors.New("glossary: entry not found")

// Type classifies an entry, which changes how the translator is told to treat
// it: a character name is transliterated consistently, an honorific is usually
// kept, an organisation is often kept in its original form.
type Type string

const (
	TypeTerm      Type = "term"
	TypeCharacter Type = "character"
	TypePlace     Type = "place"
	TypeOrg       Type = "org"
	TypeWork      Type = "work"
	TypeHonorific Type = "honorific"
)

var validTypes = map[Type]bool{
	TypeTerm: true, TypeCharacter: true, TypePlace: true,
	TypeOrg: true, TypeWork: true, TypeHonorific: true,
}

// Origin records how an entry got there.
//
// It matters because auto-extracted entries are guesses. The UI shows them
// differently and QC can treat a violation of a manual entry as more serious
// than one of an inferred entry.
type Origin string

const (
	OriginManual Origin = "manual"
	OriginAuto   Origin = "auto"
)

// Entry is one terminology rule.
type Entry struct {
	ID string `json:"id"`

	// ProjectID is nil for a global entry.
	ProjectID *string `json:"project_id,omitempty"`

	Source string `json:"source"`
	Target string `json:"target"`
	Type   Type   `json:"type"`
	Note   string `json:"note,omitempty"`

	// Priority breaks ties: the lower value wins. It is what lets a user say
	// "this term, not that one" without deleting the loser.
	Priority int `json:"priority"`

	Enabled bool   `json:"enabled"`
	Origin  Origin `json:"origin"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Validate checks an entry before it is stored.
func (e *Entry) Validate() error {
	e.Source = strings.TrimSpace(e.Source)
	e.Target = strings.TrimSpace(e.Target)

	if e.Source == "" {
		return errors.New("glossary: an entry needs a source term")
	}
	if e.Target == "" {
		return errors.New("glossary: an entry needs a target term")
	}
	if e.Type == "" {
		e.Type = TypeTerm
	}
	if !validTypes[e.Type] {
		return fmt.Errorf("glossary: %q is not a known entry type", e.Type)
	}
	if e.Origin == "" {
		e.Origin = OriginManual
	}
	if e.Priority == 0 {
		e.Priority = 100
	}
	if len([]rune(e.Source)) > 200 || len([]rune(e.Target)) > 200 {
		return errors.New("glossary: a term is too long; entries are terms, not sentences")
	}
	// A term that appears in its own translation is a no-op at best and, more
	// often, a sign the entry was added the wrong way round.
	if e.Source == e.Target {
		return fmt.Errorf("glossary: %q maps to itself", e.Source)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Storage
// ---------------------------------------------------------------------------

// Service reads and writes glossary entries.
type Service struct {
	db *database.DB
}

// NewService binds a service to a database.
func NewService(db *database.DB) *Service { return &Service{db: db} }

const entryColumns = `id, project_id, source, target, type, note, priority,
	enabled, origin, created_at, updated_at`

// List returns the entries in scope, project-specific and global, ordered by
// priority.
//
// Both are returned together and in one order because every caller wants the
// same merged view, and merging is where the project-wins-over-global rule
// lives.
func (s *Service) List(ctx context.Context, projectID string, includeDisabled bool) ([]Entry, error) {
	query := `SELECT ` + entryColumns + ` FROM glossaries WHERE (project_id = ? OR project_id IS NULL)`
	if !includeDisabled {
		query += ` AND enabled = 1`
	}
	query += ` ORDER BY priority ASC, source ASC`

	rows, err := s.db.Read.QueryContext(ctx, query, nullableID(projectID))
	if err != nil {
		return nil, fmt.Errorf("glossary: list entries: %w", err)
	}
	defer rows.Close()

	return scanEntries(rows)
}

// Get returns one entry.
func (s *Service) Get(ctx context.Context, id string) (*Entry, error) {
	row := s.db.Read.QueryRowContext(ctx,
		`SELECT `+entryColumns+` FROM glossaries WHERE id = ?`, id)

	entry, err := scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return entry, err
}

// Save inserts or updates an entry.
func (s *Service) Save(ctx context.Context, entry *Entry) error {
	if err := entry.Validate(); err != nil {
		return err
	}

	now := time.Now().UTC()
	if entry.ID == "" {
		entry.ID = "gls_" + uuid.Must(uuid.NewV7()).String()
		entry.CreatedAt = now
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = now
	}
	entry.UpdatedAt = now

	// An upsert rather than an insert, because the unique indexes make "the same
	// term twice" an update in every case a user would describe that way.
	_, err := s.db.Write.ExecContext(ctx, `
		INSERT INTO glossaries (
			id, project_id, source, target, type, note, priority,
			enabled, origin, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			source = excluded.source, target = excluded.target,
			type = excluded.type, note = excluded.note,
			priority = excluded.priority, enabled = excluded.enabled,
			updated_at = excluded.updated_at`,
		entry.ID, nullableID(deref(entry.ProjectID)), entry.Source, entry.Target,
		string(entry.Type), entry.Note, entry.Priority,
		boolToInt(entry.Enabled), string(entry.Origin),
		entry.CreatedAt.Format(time.RFC3339Nano), entry.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return wrapConstraint(err, entry)
	}
	return nil
}

// Delete removes an entry.
func (s *Service) Delete(ctx context.Context, id string) error {
	result, err := s.db.Write.ExecContext(ctx, `DELETE FROM glossaries WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("glossary: delete %s: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("glossary: delete %s: %w", id, err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// ForText returns the enabled entries that actually occur in the given text.
//
// Filtering is the point: a series glossary runs to hundreds of entries, and
// sending all of them with every batch would cost more tokens than the lines
// being translated, while burying the handful that matter. Only terms present in
// this batch travel with it.
func (s *Service) ForText(ctx context.Context, projectID string, texts []string) ([]Entry, error) {
	entries, err := s.List(ctx, projectID, false)
	if err != nil {
		return nil, err
	}
	return Match(entries, texts), nil
}

// Match filters entries to those occurring in texts and resolves duplicates.
func Match(entries []Entry, texts []string) []Entry {
	joined := strings.Join(texts, "\n")

	// source → the entry that won for that source.
	winners := make(map[string]Entry, len(entries))

	for _, entry := range entries {
		if !entry.Enabled || !Occurs(entry.Source, joined) {
			continue
		}

		existing, seen := winners[entry.Source]
		if !seen || moreSpecific(entry, existing) {
			winners[entry.Source] = entry
		}
	}

	out := make([]Entry, 0, len(winners))
	for _, entry := range winners {
		out = append(out, entry)
	}

	// Priority first, then source. The order is part of the prompt, and a
	// prompt that varies between identical requests produces cache misses and
	// translations that differ for no reason a user can see.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].Source < out[j].Source
	})
	return out
}

// moreSpecific reports whether a should beat b when both define the same
// source term.
func moreSpecific(a, b Entry) bool {
	aProject := a.ProjectID != nil
	bProject := b.ProjectID != nil
	if aProject != bProject {
		// A project entry is a statement about this video; a global entry is a
		// default. The specific one wins regardless of priority.
		return aProject
	}
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	// A manual entry beats an auto-extracted one at equal priority.
	if a.Origin != b.Origin {
		return a.Origin == OriginManual
	}
	return a.UpdatedAt.After(b.UpdatedAt)
}

// Occurs reports whether a term appears in text.
//
// The distinction between the two scripts is not cosmetic. Japanese has no
// spaces, so a substring test is the only option and it is correct. Applying the
// same test to Latin text matches "Art" inside "Arthur" and injects a term the
// translator was never meant to see.
func Occurs(term, text string) bool {
	if term == "" || text == "" {
		return false
	}
	if !usesWordBoundaries(term) {
		return strings.Contains(text, term)
	}

	index := 0
	for {
		found := strings.Index(text[index:], term)
		if found < 0 {
			return false
		}
		start := index + found
		end := start + len(term)

		if !isWordByte(byteAt(text, start-1)) && !isWordByte(byteAt(text, end)) {
			return true
		}
		index = start + 1
		if index >= len(text) {
			return false
		}
	}
}

// usesWordBoundaries reports whether a term is written in a script that
// separates words, and so needs boundary-checked matching.
func usesWordBoundaries(term string) bool {
	for _, r := range term {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			return false
		}
	}
	return true
}

func byteAt(s string, i int) byte {
	if i < 0 || i >= len(s) {
		return ' '
	}
	return s[i]
}

func isWordByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b >= 0x80
}

// ---------------------------------------------------------------------------
// Prompt rendering
// ---------------------------------------------------------------------------

// MaxBlockEntries bounds how many terms reach a single request.
//
// Past this point the glossary stops being an instruction and becomes noise the
// model skips over. A long series is handled by the fact that only terms present
// in the batch are sent at all.
const MaxBlockEntries = 60

// Block renders the glossary section of a translation prompt.
//
// The output is plain text rather than markdown or JSON: it is an instruction to
// a model, and a table is the shape that survives being read by a model of any
// size. The format is stable — it is baked into the prompt version, and changing
// it means changing the version so that cached translations are not reused
// against a prompt that no longer exists.
func Block(entries []Entry) string {
	if len(entries) == 0 {
		return ""
	}

	limit := len(entries)
	if limit > MaxBlockEntries {
		limit = MaxBlockEntries
	}

	var b strings.Builder
	for i, entry := range entries[:limit] {
		if i > 0 {
			b.WriteByte('\n')
		}

		// The type is shown because it changes the instruction: keeping a
		// character's name consistent is a different request from honouring an
		// honorific.
		if entry.Type == TypeTerm || entry.Type == "" {
			fmt.Fprintf(&b, "%s → %s", entry.Source, entry.Target)
		} else {
			fmt.Fprintf(&b, "%s → %s (%s)", entry.Source, entry.Target, entry.Type)
		}
		if entry.Note != "" {
			fmt.Fprintf(&b, " — %s", entry.Note)
		}
	}
	return b.String()
}

// Hash digests the entries that reached a prompt.
//
// It is part of the translation cache key, which is what stops a translation
// produced under one glossary from being reused under another.
//
// Only the semantic fields are hashed. An entry's ID and timestamps change when
// a user re-saves it unchanged, and folding those in would throw away every
// cached translation for a series because someone fixed a typo in a note that
// was already correct.
func Hash(entries []Entry) (string, error) {
	type term struct {
		Source string `json:"source"`
		Target string `json:"target"`
		Type   string `json:"type"`
		Note   string `json:"note"`
	}

	projection := make([]term, 0, len(entries))
	for _, entry := range entries {
		projection = append(projection, term{
			Source: entry.Source,
			Target: entry.Target,
			Type:   string(entry.Type),
			Note:   entry.Note,
		})
	}
	return artifact.FingerprintBytes("glossary", projection)
}

// ---------------------------------------------------------------------------
// Scanning
// ---------------------------------------------------------------------------

type scanner interface {
	Scan(dest ...any) error
}

func scanEntries(rows *sql.Rows) ([]Entry, error) {
	entries := []Entry{}
	for rows.Next() {
		entry, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, *entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("glossary: read entries: %w", err)
	}
	return entries, nil
}

func scanEntry(row scanner) (*Entry, error) {
	var (
		entry     Entry
		projectID sql.NullString
		entryType string
		origin    string
		enabled   int
		created   string
		updated   string
	)

	err := row.Scan(&entry.ID, &projectID, &entry.Source, &entry.Target,
		&entryType, &entry.Note, &entry.Priority, &enabled, &origin, &created, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("glossary: read entry: %w", err)
	}

	if projectID.Valid {
		id := projectID.String
		entry.ProjectID = &id
	}
	entry.Type = Type(entryType)
	entry.Origin = Origin(origin)
	entry.Enabled = enabled != 0
	entry.CreatedAt = parseTime(created)
	entry.UpdatedAt = parseTime(updated)

	return &entry, nil
}

func wrapConstraint(err error, entry *Entry) error {
	message := err.Error()
	if strings.Contains(message, "UNIQUE constraint failed") {
		return fmt.Errorf("glossary: %q is already defined in this scope", entry.Source)
	}
	return fmt.Errorf("glossary: save %q: %w", entry.Source, err)
}

func nullableID(id string) any {
	if id == "" {
		return nil
	}
	return id
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
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
