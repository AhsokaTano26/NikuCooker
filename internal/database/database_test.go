package database

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(Options{Path: filepath.Join(t.TempDir(), "niku.db")})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestMigrateCreatesSchema(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	applied, err := Migrate(ctx, db)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if applied != 1 {
		t.Errorf("applied %d migrations, want 1", applied)
	}

	want := []string{
		"projects", "jobs", "stages", "artifacts", "segments", "qc_results",
		"glossaries", "translation_cache", "providers", "model_records",
		"settings", "schema_migrations",
	}
	for _, table := range want {
		var name string
		err := db.Read.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %s is missing: %v", table, err)
		}
	}

	version, err := CurrentVersion(ctx, db)
	if err != nil {
		t.Fatalf("CurrentVersion: %v", err)
	}
	if version != 1 {
		t.Errorf("schema version = %d, want 1", version)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	if _, err := Migrate(ctx, db); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	// The common case is an up-to-date database, and it must be silent.
	applied, err := Migrate(ctx, db)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if applied != 0 {
		t.Errorf("re-running applied %d migrations, want 0", applied)
	}
}

func TestMigrateRefusesNewerSchema(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	if _, err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	// A database written by a future binary has columns this build does not
	// know about; reading it anyway is how data gets corrupted.
	_, err := db.Write.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (9999, 'from_the_future', '2100-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Migrate(ctx, db); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("got %v, want ErrSchemaTooNew", err)
	}
}

func TestSchemaEnforcesTimingInvariant(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	projectID := seedProject(t, db)

	// A zero-length or inverted line must not be storable. Catching it here
	// means a segmentation bug fails loudly at insert rather than producing a
	// subtitle file a user discovers is broken on playback.
	cases := []struct {
		name       string
		start, end float64
	}{
		{"inverted", 5.0, 3.0},
		{"zero length", 5.0, 5.0},
		{"negative start", -1.0, 2.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.Write.ExecContext(ctx, `
				INSERT INTO segments
					(id, project_id, ordinal, start, end, source_language, target_language,
					 source_text, created_at, updated_at)
				VALUES ('seg_x', ?, 1, ?, ?, 'ja', 'zh-Hans', 'text', 'now', 'now')`,
				projectID, tc.start, tc.end)
			if err == nil {
				t.Fatal("the database accepted an impossible segment timing")
			}
		})
	}
}

func TestSchemaRejectsUnknownStageStatus(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	projectID := seedProject(t, db)

	if _, err := db.Write.ExecContext(ctx, `
		INSERT INTO jobs (id, project_id, kind, status, progress, created_at)
		VALUES ('job_1', ?, 'full', 'finished', 0, 'now')`, projectID); err == nil {
		t.Fatal("the database accepted an unknown job status")
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	// SQLite disables foreign keys by default, so the pragma in Open is what
	// makes every REFERENCES clause in the schema mean anything at all.
	if _, err := db.Write.ExecContext(ctx, `
		INSERT INTO jobs (id, project_id, kind, status, progress, created_at)
		VALUES ('job_1', 'no-such-project', 'full', 'pending', 0, 'now')`); err == nil {
		t.Fatal("a job referencing a nonexistent project was accepted")
	}
}

func TestGlossaryScopesAreSeparatelyUnique(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	projectID := seedProject(t, db)

	insert := func(id string, project any, source string) error {
		_, err := db.Write.ExecContext(ctx, `
			INSERT INTO glossaries (id, project_id, source, target, created_at, updated_at)
			VALUES (?, ?, ?, 'x', 'now', 'now')`, id, project, source)
		return err
	}

	if err := insert("g1", nil, "エビ中"); err != nil {
		t.Fatalf("first global entry: %v", err)
	}
	// Global entries must be unique among themselves. A single composite
	// UNIQUE(project_id, source) would not catch this, because SQLite treats
	// NULL as distinct — which is exactly why there are two partial indexes.
	if err := insert("g2", nil, "エビ中"); err == nil {
		t.Error("a duplicate global glossary entry was accepted")
	}
	// ...and a project entry for the same source is a different scope.
	if err := insert("g3", projectID, "エビ中"); err != nil {
		t.Errorf("a project entry shadowing a global one was rejected: %v", err)
	}
	if err := insert("g4", projectID, "エビ中"); err == nil {
		t.Error("a duplicate project glossary entry was accepted")
	}
}

func TestArtifactKeyIsUniquePerProject(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	projectID := seedProject(t, db)

	insert := func(id string) error {
		_, err := db.Write.ExecContext(ctx, `
			INSERT INTO artifacts (id, project_id, stage, input_hash, config_hash, key, path, created_at)
			VALUES (?, ?, 'asr', 'in', 'cfg', 'the-key', 'artifacts/asr/' || ?, 'now')`, id, projectID, id)
		return err
	}

	// This constraint is what makes artifact creation idempotent: two jobs
	// racing to produce the same artifact cannot both insert.
	if err := insert("art_1"); err != nil {
		t.Fatalf("first artifact: %v", err)
	}
	if err := insert("art_2"); err == nil {
		t.Error("two artifacts with the same content key were accepted")
	}
}

func TestIntegrityCheck(t *testing.T) {
	db := openTestDB(t)
	if _, err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}

	result, err := db.IntegrityCheck()
	if err != nil {
		t.Fatalf("IntegrityCheck: %v", err)
	}
	if result != "ok" {
		t.Errorf("integrity check reported %q", result)
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open(Options{}); err == nil {
		t.Fatal("Open accepted an empty path")
	}
}

func TestOpenFailsOnUnwritableDirectory(t *testing.T) {
	// The error must name the path: "unable to open database file" alone sends
	// a user looking in the wrong place.
	_, err := Open(Options{Path: filepath.Join(t.TempDir(), "missing", "nested", "niku.db")})
	if err == nil {
		t.Fatal("Open succeeded in a directory that does not exist")
	}
	if !strings.Contains(err.Error(), "niku.db") {
		t.Errorf("error does not name the path: %v", err)
	}
}

// seedProject inserts the minimum row other tables' foreign keys need.
func seedProject(t *testing.T, db *DB) string {
	t.Helper()
	const id = "proj_test"
	_, err := db.Write.ExecContext(context.Background(), `
		INSERT INTO projects (id, name, source_path, source_language, target_language,
		                      style, status, config_json, created_at, updated_at)
		VALUES (?, 'test', 'source/source.mp4', 'ja', 'zh-Hans',
		        'fansub', 'active', '{}', 'now', 'now')`, id)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return id
}
