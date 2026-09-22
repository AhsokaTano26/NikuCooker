package translation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/artifact"
	"github.com/AhsokaTano26/NikuCooker/internal/database"
	"github.com/AhsokaTano26/NikuCooker/internal/llmjson"
)

// KeyInputs are everything that determines a translation.
//
// The list is deliberately exhaustive. Every field left out is a way for a
// translation produced under one set of conditions to be served under another,
// and the symptom — a line that was correct before an edit and wrong after,
// with no request in the log — is close to undiagnosable.
type KeyInputs struct {
	// Source is the line being translated, after normalisation.
	Source string

	Style string

	// Provider and Model distinguish two services offering the same model name.
	Provider string
	Model    string

	// PromptVersion is the content digest of the template and style guidance.
	PromptVersion string

	// ContextHash digests the analysis document. Two episodes of one series
	// share it, which is what makes the cross-episode hit rate real.
	ContextHash string

	// GlossaryHash digests the terms that applied to *this line*.
	GlossaryHash string
}

// KeyPrefix identifies a translation cache key.
const KeyPrefix = "trc_"

// DeriveKey computes the cache key.
//
// Canonical JSON before hashing, so that the key depends on the values and not
// on Go's field order or a struct that gained a field. It also makes the key
// reproducible from another process, which matters the day something other than
// this package wants to compute one.
func DeriveKey(in KeyInputs) (string, error) {
	digest, err := artifact.FingerprintBytes("translation key", in)
	if err != nil {
		return "", fmt.Errorf("translation: derive cache key: %w", err)
	}
	// Truncated, not for security but for legibility: these keys are stored and
	// occasionally read by a person. 128 bits is far past the point where a
	// collision is reachable.
	return KeyPrefix + digest[:32], nil
}

// hashString digests an arbitrary value for use as a key component.
func hashString(value string) (string, error) {
	digest, err := artifact.FingerprintBytes("context", value)
	if err != nil {
		return "", fmt.Errorf("translation: digest the context document: %w", err)
	}
	return digest, nil
}

// CacheEntry is one stored translation.
type CacheEntry struct {
	Key        string
	SourceText string
	Translated string
	Provider   string
	Model      string
	PromptVers string
	Style      string
	TokenIn    int
	TokenOut   int
}

// Cache stores translations across runs and across projects.
//
// It is global rather than per project on purpose: the same line from the same
// show in the same style should cost once, and a user working through a series
// gets real benefit from hitting lines that a previous episode already paid for.
// The key — which carries the context and glossary digests — is what keeps that
// from being wrong.
type Cache struct {
	db *database.DB
}

// NewCache binds a cache to a database.
func NewCache(db *database.DB) *Cache { return &Cache{db: db} }

// lookupChunk bounds how many keys go into one query.
//
// SQLite's default limit on bound variables is 999. Staying well below it keeps
// a large batch from failing as a whole when a couple of hundred keys are looked
// up at once.
const lookupChunk = 200

// Get returns the translations stored for the given keys.
//
// Keys that are absent are simply missing from the result rather than an error:
// a cache miss is the normal case, not a failure.
func (c *Cache) Get(ctx context.Context, keys []string) (map[string]string, error) {
	found := make(map[string]string, len(keys))
	if len(keys) == 0 {
		return found, nil
	}

	for start := 0; start < len(keys); start += lookupChunk {
		end := min(start+lookupChunk, len(keys))
		chunk := keys[start:end]

		query := `SELECT key, translated FROM translation_cache WHERE key IN (` +
			strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",") + `)`

		args := make([]any, 0, len(chunk))
		for _, key := range chunk {
			args = append(args, key)
		}

		rows, err := c.db.Read.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("translation: read cache: %w", err)
		}

		for rows.Next() {
			var key, translated string
			if err := rows.Scan(&key, &translated); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("translation: read cache entry: %w", err)
			}
			found[key] = translated
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("translation: read cache: %w", err)
		}
		_ = rows.Close()
	}

	if len(found) > 0 {
		// Recording the hit is best-effort. A failure here costs a statistic,
		// and failing a translation run because a counter could not be bumped
		// would be the tail wagging the dog.
		_ = c.touch(ctx, keysOf(found))
	}

	return found, nil
}

// touch records cache hits.
func (c *Cache) touch(ctx context.Context, keys []string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)

	for start := 0; start < len(keys); start += lookupChunk {
		end := min(start+lookupChunk, len(keys))
		chunk := keys[start:end]

		query := `UPDATE translation_cache SET hits = hits + 1, last_used_at = ? WHERE key IN (` +
			strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",") + `)`

		args := make([]any, 0, len(chunk)+1)
		args = append(args, now)
		for _, key := range chunk {
			args = append(args, key)
		}

		if _, err := c.db.Write.ExecContext(ctx, query, args...); err != nil {
			return err
		}
	}
	return nil
}

// Put stores translations.
//
// An existing key is left alone rather than overwritten. A cached translation
// was paid for, and re-inserting an identical one — which happens whenever a job
// is re-run after a partial failure — would reset its hit count and its age for
// no gain.
func (c *Cache) Put(ctx context.Context, entries []CacheEntry) error {
	if len(entries) == 0 {
		return nil
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)

	tx, err := c.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("translation: store translations: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	statement, err := tx.PrepareContext(ctx, `
		INSERT INTO translation_cache (
			key, source_text, translated, provider, model, prompt_version, style,
			token_in, token_out, hits, created_at, last_used_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)
		ON CONFLICT(key) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("translation: prepare cache insert: %w", err)
	}
	defer func() { _ = statement.Close() }()

	for _, entry := range entries {
		_, err := statement.ExecContext(ctx,
			entry.Key, entry.SourceText, entry.Translated, entry.Provider,
			entry.Model, entry.PromptVers, entry.Style,
			entry.TokenIn, entry.TokenOut, now, now)
		if err != nil {
			return fmt.Errorf("translation: store translation for %q: %w",
				llmjson.Truncate(entry.SourceText, 40), err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("translation: store translations: %w", err)
	}
	return nil
}

// Prune drops the least recently used entries above a limit.
//
// Unbounded is the wrong default for a table that grows with every line every
// user ever translates. Recency rather than age, because a series is translated
// in bursts and the entries worth keeping are the ones a current project keeps
// hitting.
func (c *Cache) Prune(ctx context.Context, maxEntries int) (int, error) {
	if maxEntries <= 0 {
		return 0, nil
	}

	var total int
	if err := c.db.Read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM translation_cache`).Scan(&total); err != nil {
		return 0, fmt.Errorf("translation: count cache: %w", err)
	}
	if total <= maxEntries {
		return 0, nil
	}

	excess := total - maxEntries
	result, err := c.db.Write.ExecContext(ctx, `
		DELETE FROM translation_cache WHERE key IN (
			SELECT key FROM translation_cache ORDER BY last_used_at ASC LIMIT ?)`, excess)
	if err != nil {
		return 0, fmt.Errorf("translation: prune cache: %w", err)
	}

	removed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("translation: prune cache: %w", err)
	}
	return int(removed), nil
}

// Stats reports how many translations are cached and how many have been reused.
func (c *Cache) Stats(ctx context.Context) (entries, hits int, err error) {
	row := c.db.Read.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(hits), 0) FROM translation_cache`)
	if err := row.Scan(&entries, &hits); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("translation: cache stats: %w", err)
	}
	return entries, hits, nil
}

func keysOf(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}
