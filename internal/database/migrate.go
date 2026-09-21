package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AhsokaTano26/NikuCooker/migrations"
)

// ErrSchemaTooNew reports a database written by a newer binary.
//
// Continuing against a schema with unknown columns is how data gets corrupted,
// so this refuses rather than attempts a best effort. There is no down
// migration either: a local-first app has no rollback story better than
// "restore your backup".
var ErrSchemaTooNew = errors.New("database: schema is newer than this binary")

const createMigrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL,
    applied_at TEXT    NOT NULL
);`

// Migration is one embedded schema change.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// Migrate applies every pending migration in ascending order.
//
// It returns the number applied, which is zero on an up-to-date database — the
// common case, and one that should be silent.
func Migrate(ctx context.Context, db *DB) (int, error) {
	if _, err := db.Write.ExecContext(ctx, createMigrationsTable); err != nil {
		return 0, fmt.Errorf("database: create migrations table: %w", err)
	}

	available, err := loadMigrations()
	if err != nil {
		return 0, err
	}
	if len(available) == 0 {
		return 0, errors.New("database: no migrations are embedded in this binary")
	}

	applied, err := appliedVersions(ctx, db)
	if err != nil {
		return 0, err
	}

	// Refuse a database from the future before touching anything. A silent
	// partial read of a schema this build does not understand is worse than not
	// starting.
	highest := available[len(available)-1].Version
	for version := range applied {
		if version > highest {
			return 0, fmt.Errorf("%w: database has version %d, this binary knows up to %d; run the newer binary",
				ErrSchemaTooNew, version, highest)
		}
	}

	count := 0
	for _, migration := range available {
		if applied[migration.Version] {
			continue
		}
		if err := applyOne(ctx, db, migration); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// applyOne runs a migration and records it in a single transaction.
//
// SQLite has transactional DDL, so a failure rolls back completely rather than
// leaving half a schema behind.
func applyOne(ctx context.Context, db *DB, migration Migration) error {
	tx, err := db.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("database: begin migration %04d: %w", migration.Version, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, migration.SQL); err != nil {
		return fmt.Errorf("database: migration %04d (%s): %w", migration.Version, migration.Name, err)
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
		migration.Version, migration.Name, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("database: record migration %04d: %w", migration.Version, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("database: commit migration %04d: %w", migration.Version, err)
	}
	return nil
}

// CurrentVersion reports the highest applied migration, or 0 for an empty
// database.
func CurrentVersion(ctx context.Context, db *DB) (int, error) {
	if _, err := db.Read.ExecContext(ctx, createMigrationsTable); err != nil {
		return 0, fmt.Errorf("database: create migrations table: %w", err)
	}

	var version sql.NullInt64
	err := db.Read.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("database: read schema version: %w", err)
	}
	if !version.Valid {
		return 0, nil
	}
	return int(version.Int64), nil
}

func appliedVersions(ctx context.Context, db *DB) (map[int]bool, error) {
	rows, err := db.Read.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("database: read applied migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	applied := map[int]bool{}
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("database: scan applied migration: %w", err)
		}
		applied[version] = true
	}
	return applied, rows.Err()
}

// loadMigrations reads and sorts the embedded SQL files.
//
// A file that does not parse as NNNN_name.sql is an error rather than skipped:
// a migration silently not running is a schema that silently is not what the
// code expects.
func loadMigrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("database: read embedded migrations: %w", err)
	}

	var out []Migration
	seen := map[int]string{}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || path.Ext(name) != ".sql" {
			continue
		}

		version, err := parseVersion(name)
		if err != nil {
			return nil, err
		}
		if previous, dup := seen[version]; dup {
			return nil, fmt.Errorf("database: two migrations claim version %d: %s and %s",
				version, previous, name)
		}
		seen[version] = name

		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("database: read migration %s: %w", name, err)
		}
		if len(strings.TrimSpace(string(body))) == 0 {
			return nil, fmt.Errorf("database: migration %s is empty", name)
		}

		out = append(out, Migration{
			Version: version,
			Name:    strings.TrimSuffix(name, ".sql"),
			SQL:     string(body),
		})
	}

	if len(out) == 0 {
		return nil, errors.New("database: no migration files found")
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

func parseVersion(filename string) (int, error) {
	base := strings.TrimSuffix(filename, ".sql")
	prefix, _, found := strings.Cut(base, "_")
	if !found {
		return 0, fmt.Errorf("database: migration %s is not named NNNN_description.sql", filename)
	}
	version, err := strconv.Atoi(prefix)
	if err != nil {
		return 0, fmt.Errorf("database: migration %s has a non-numeric version prefix: %w", filename, err)
	}
	if version <= 0 {
		return 0, fmt.Errorf("database: migration %s has version %d; versions start at 1", filename, version)
	}
	return version, nil
}
