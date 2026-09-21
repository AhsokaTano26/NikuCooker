// Package database owns the SQLite connection and schema.
//
// SQLite in WAL mode allows many concurrent readers with exactly one writer. A
// single *sql.DB shared between the API and the job runner hits SQLITE_BUSY
// under concurrent load, and raising busy_timeout only turns a fast failure
// into a slow one. Two handles with different pools make the contention a
// queue instead of an error.
package database

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"runtime"

	// Registers the "sqlite" driver. Pure Go, so CGO_ENABLED=0 keeps working
	// and the release matrix needs no C toolchain per target.
	_ "modernc.org/sqlite"
)

// DB holds the write and read handles.
type DB struct {
	// Write is serialised to one connection. Every mutation goes through it.
	Write *sql.DB
	// Read serves concurrent queries.
	Read *sql.DB

	path string
}

// Options configures Open.
type Options struct {
	Path string
}

// Open connects to the database at path, creating it if absent.
func Open(opts Options) (*DB, error) {
	if opts.Path == "" {
		return nil, fmt.Errorf("database: path must not be empty")
	}

	// busy_timeout is a backstop for the rare collision between the writer and
	// the retention sweep; the one-connection write pool is what prevents it.
	//
	// synchronous=NORMAL is crash-safe against process death under WAL. Only an
	// OS-level power loss can lose the last transaction, and FULL would fsync
	// every write — this is a local application, not a ledger.
	const pragmas = "_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)"

	dsn := "file:" + filepath.ToSlash(opts.Path) + "?" + pragmas

	// Both handles are opened read-write on purpose. A read-only connection
	// (mode=ro) cannot read a WAL database unless it can also write the -shm
	// file, so `mode=ro` fails with SQLITE_CANTOPEN on exactly the setup this
	// design uses. The separation between the handles is about connection
	// count, not permissions: one writer, many readers.
	write, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("database: open %s: %w", opts.Path, err)
	}
	write.SetMaxOpenConns(1)
	// No lifetime cap: closing and reopening the single writer would drop the
	// WAL checkpointing cadence for no benefit.
	write.SetConnMaxLifetime(0)
	write.SetConnMaxIdleTime(0)

	read, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = write.Close()
		return nil, fmt.Errorf("database: open read pool %s: %w", opts.Path, err)
	}
	read.SetMaxOpenConns(max(2, runtime.NumCPU()))
	read.SetConnMaxLifetime(0)

	db := &DB{Write: write, Read: read, path: opts.Path}

	if err := db.ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func (db *DB) ping() error {
	if err := db.Write.Ping(); err != nil {
		return fmt.Errorf("database: cannot write to %s: %w", db.path, err)
	}
	var mode string
	if err := db.Write.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		return fmt.Errorf("database: read journal mode: %w", err)
	}
	if mode != "wal" {
		// Not fatal, but it means the two-handle design is not doing what it
		// was built for, so it is worth knowing.
		return fmt.Errorf("database: journal_mode is %q, expected wal; the database may be on a filesystem that cannot support it", mode)
	}
	return nil
}

// Path reports the database file path.
func (db *DB) Path() string { return db.path }

// Close releases both handles.
func (db *DB) Close() error {
	var first error
	if db.Read != nil {
		if err := db.Read.Close(); err != nil {
			first = err
		}
	}
	if db.Write != nil {
		if err := db.Write.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// IntegrityCheck reports the result of SQLite's own consistency check.
//
// Called by doctor and reported rather than auto-repaired: a repair that runs
// silently is a repair nobody knows happened.
func (db *DB) IntegrityCheck() (string, error) {
	var result string
	if err := db.Read.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		return "", fmt.Errorf("database: integrity check: %w", err)
	}
	return result, nil
}
