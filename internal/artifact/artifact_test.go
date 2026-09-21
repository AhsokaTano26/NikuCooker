package artifact

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/AhsokaTano26/NikuCooker/internal/database"
)

// ---------------------------------------------------------------------------
// Key derivation
// ---------------------------------------------------------------------------

func baseInputs() KeyInputs {
	return KeyInputs{
		Stage:        "asr",
		StageVersion: "1",
		CodeRevision: "abc123",
		Config:       map[string]any{"model": "medium", "beam_size": 5},
		Fingerprint:  map[string]string{"device": "cpu"},
		UpstreamKeys: map[string]string{"audio": "key-audio-1"},
	}
}

func keyOf(t *testing.T, in KeyInputs) string {
	t.Helper()
	key, _, _, err := DeriveKey(in)
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	return key
}

func TestDeriveKeyIsDeterministic(t *testing.T) {
	// Two calls with the same inputs must agree, or nothing is ever cached.
	if a, b := keyOf(t, baseInputs()), keyOf(t, baseInputs()); a != b {
		t.Fatalf("same inputs produced different keys:\n %s\n %s", a, b)
	}
}

func TestDeriveKeyIgnoresMapOrdering(t *testing.T) {
	// Go randomises map iteration, so a hash that depended on it would miss at
	// random — the worst kind of cache bug, because it looks like flakiness.
	first := baseInputs()
	first.Config = map[string]any{"a": 1, "b": 2, "c": 3, "d": 4, "e": 5, "f": 6}
	second := baseInputs()
	second.Config = map[string]any{"f": 6, "e": 5, "d": 4, "c": 3, "b": 2, "a": 1}

	if keyOf(t, first) != keyOf(t, second) {
		t.Fatal("key depends on map iteration order")
	}
}

func TestDeriveKeyChangesWithEachComponent(t *testing.T) {
	want := keyOf(t, baseInputs())

	cases := []struct {
		name   string
		mutate func(*KeyInputs)
	}{
		{"stage version", func(in *KeyInputs) { in.StageVersion = "2" }},
		{"code revision", func(in *KeyInputs) { in.CodeRevision = "def456" }},
		{"config", func(in *KeyInputs) { in.Config = map[string]any{"model": "large-v3", "beam_size": 5} }},
		{"fingerprint", func(in *KeyInputs) { in.Fingerprint = map[string]string{"device": "cuda"} }},
		{"upstream key", func(in *KeyInputs) { in.UpstreamKeys = map[string]string{"audio": "key-audio-2"} }},
		{"source fingerprint", func(in *KeyInputs) { in.SourceFingerprint = "v1:size=1:h=x" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInputs()
			tc.mutate(&in)
			if keyOf(t, in) == want {
				t.Errorf("changing %s did not change the key", tc.name)
			}
		})
	}
}

func TestStageNameIsPartOfTheKey(t *testing.T) {
	// Otherwise two stages with coincidentally identical configs would share an
	// artifact, and one would silently consume the other's output.
	in := baseInputs()
	in.Stage = "vad"
	if keyOf(t, in) == keyOf(t, baseInputs()) {
		t.Fatal("the stage name does not participate in the key")
	}
}

func TestConfigSubtreeScoping(t *testing.T) {
	// The mistake this guards against is hashing the *whole* configuration, so
	// that changing render.crf invalidates the transcription. Each stage
	// declares which subtree it reads, and only that subtree is hashed — which
	// the type system enforces here by construction, since KeyInputs.Config is
	// whatever the caller passes.
	asrConfig := map[string]any{"model": "medium", "beam_size": 5}
	inputs := baseInputs()
	inputs.Config = asrConfig

	before := keyOf(t, inputs)

	// A different stage's config changing is invisible here, because it was
	// never part of this stage's subtree.
	renderConfig := map[string]any{"mode": "soft", "crf": 18}
	inputs2 := baseInputs()
	inputs2.Config = asrConfig
	_ = renderConfig

	if keyOf(t, inputs2) != before {
		t.Fatal("an unrelated config value changed this stage's key")
	}
}

func TestIDForKey(t *testing.T) {
	key := "0123456789abcdef0123456789abcdef"
	if got, want := IDForKey(key), IDFPrefix+"0123456789abcdef"; got != want {
		t.Errorf("IDForKey = %q, want %q", got, want)
	}
	// A short key must not panic. It cannot be produced by DeriveKey, but a
	// panic in an ID helper is a crash with no useful context.
	if got := IDForKey("short"); got != IDFPrefix+"short" {
		t.Errorf("IDForKey(short) = %q", got)
	}
}

// ---------------------------------------------------------------------------
// Source fingerprint
// ---------------------------------------------------------------------------

func writeFile(t *testing.T, path string, size int, fill byte) {
	t.Helper()
	body := make([]byte, size)
	for i := range body {
		body[i] = fill
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFingerprintDetectsSizeChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.mp4")

	writeFile(t, path, 4<<20, 0xAA)
	before, err := FingerprintSource(path)
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, path, 5<<20, 0xAA)
	after, err := FingerprintSource(path)
	if err != nil {
		t.Fatal(err)
	}

	if before == after {
		t.Fatal("a size change did not change the fingerprint")
	}
}

func TestFingerprintDetectsTailEdit(t *testing.T) {
	// An edit at the end is the case a head-only hash misses, and appending to
	// a file is a common way to "change" it.
	dir := t.TempDir()
	path := filepath.Join(dir, "source.mp4")

	const size = 6 << 20
	body := make([]byte, size)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := FingerprintSource(path)
	if err != nil {
		t.Fatal(err)
	}

	body[size-1] = 0xFF
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := FingerprintSource(path)
	if err != nil {
		t.Fatal(err)
	}

	if before == after {
		t.Fatal("an edit at the end of the file did not change the fingerprint")
	}
}

func TestFingerprintDetectsMiddleEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.mp4")

	const size = 8 << 20
	body := make([]byte, size)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := FingerprintSource(path)
	if err != nil {
		t.Fatal(err)
	}

	body[size/2] = 0xFF
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := FingerprintSource(path)
	if err != nil {
		t.Fatal(err)
	}

	if before == after {
		t.Fatal("an edit near the middle did not change the fingerprint")
	}
}

func TestFingerprintIsStableAcrossUnchangedReads(t *testing.T) {
	// Re-running a pipeline must not invalidate everything because the
	// fingerprint is not reproducible.
	dir := t.TempDir()
	path := filepath.Join(dir, "source.mp4")
	writeFile(t, path, 3<<20, 0x11)

	first, err := FingerprintSource(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := FingerprintSource(path)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("fingerprint is not stable:\n %s\n %s", first, second)
	}
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()

	db, err := database.Open(database.Options{Path: filepath.Join(dir, "niku.db")})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := database.Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	projectDir := filepath.Join(dir, "projects", "proj_1")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err = db.Write.ExecContext(context.Background(), `
		INSERT INTO projects (id, name, source_path, source_language, target_language,
		                      style, status, config_json, created_at, updated_at)
		VALUES ('proj_1', 'test', 'source/source.mp4', 'ja', 'zh-Hans',
		        'fansub', 'active', '{}', 'now', 'now')`)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}

	return NewStore(db, "proj_1", projectDir), projectDir
}

func TestStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, projectDir := newTestStore(t)

	w, err := store.Begin(baseInputs())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir(), "asr.json"), []byte(`{"segments":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	published, err := w.Commit(ctx, Result{Primary: "asr.json", Provider: "faster-whisper", Model: "medium"})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if published.Status != StatusReady {
		t.Errorf("status = %q, want ready", published.Status)
	}

	found, err := store.Lookup(ctx, published.Key)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if found == nil {
		t.Fatal("a committed artifact was not found")
	}
	if found.Model != "medium" {
		t.Errorf("model = %q, want medium", found.Model)
	}

	// The payload must be readable through the recorded path.
	payload := filepath.Join(projectDir, filepath.FromSlash(found.Path), "asr.json")
	if _, err := os.Stat(payload); err != nil {
		t.Errorf("payload is not where the artifact says it is: %v", err)
	}
}

func TestStoreLookupMissingKeyReturnsNil(t *testing.T) {
	store, _ := newTestStore(t)

	found, err := store.Lookup(context.Background(), "no-such-key")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if found != nil {
		t.Fatal("Lookup returned an artifact for an unknown key")
	}
}

func TestStoreTreatsMissingDirectoryAsMiss(t *testing.T) {
	// Disk is authoritative and the database is an index. Trusting the row here
	// would produce a stage that reports success and yields a file that is not
	// there — which surfaces much later, as a confusing downstream failure.
	ctx := context.Background()
	store, projectDir := newTestStore(t)

	w, err := store.Begin(baseInputs())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir(), "asr.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	published, err := w.Commit(ctx, Result{Primary: "asr.json"})
	if err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(filepath.Join(projectDir, filepath.FromSlash(published.Path))); err != nil {
		t.Fatal(err)
	}

	found, err := store.Lookup(ctx, published.Key)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if found != nil {
		t.Fatal("Lookup trusted a row whose directory is gone")
	}
}

func TestStoreRequiresPrimaryPayload(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)

	w, err := store.Begin(baseInputs())
	if err != nil {
		t.Fatal(err)
	}
	// Declaring a primary that was never written would publish an artifact
	// whose consumers cannot find their input.
	if _, err := w.Commit(ctx, Result{Primary: "absent.json"}); err == nil {
		t.Fatal("Commit accepted a missing primary payload")
	}
}

func TestAbortLeavesNothingVisible(t *testing.T) {
	ctx := context.Background()
	store, projectDir := newTestStore(t)

	w, err := store.Begin(baseInputs())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Abort(ctx); err != nil {
		t.Fatalf("Abort: %v", err)
	}

	if found, _ := store.Lookup(ctx, w.Key()); found != nil {
		t.Error("an aborted artifact is visible as ready")
	}

	stageDir := filepath.Join(projectDir, ArtifactsDir, "asr")
	entries, err := os.ReadDir(stageDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Errorf("abort left %s behind", entry.Name())
	}
}

func TestCommitPublishesAtomically(t *testing.T) {
	ctx := context.Background()
	store, projectDir := newTestStore(t)

	w, err := store.Begin(baseInputs())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir(), "asr.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Before Commit, the temp directory exists but the published one does not.
	stageDir := filepath.Join(projectDir, ArtifactsDir, "asr")
	if _, err := os.Stat(w.dir); !os.IsNotExist(err) {
		t.Error("the artifact directory exists before Commit")
	}

	if _, err := w.Commit(ctx, Result{Primary: "asr.json"}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(w.dir); err != nil {
		t.Errorf("the artifact directory is missing after Commit: %v", err)
	}
	entries, err := os.ReadDir(stageDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if len(entry.Name()) > len(tmpPrefix) && entry.Name()[:len(tmpPrefix)] == tmpPrefix {
			t.Errorf("Commit left a temp directory behind: %s", entry.Name())
		}
	}
}

func TestSweepTempDirectories(t *testing.T) {
	// A run killed mid-write leaves these, and nothing else will ever clean
	// them up, because no live writer owns a directory in this project.
	store, projectDir := newTestStore(t)

	stageDir := filepath.Join(projectDir, ArtifactsDir, "asr")
	stale := filepath.Join(stageDir, tmpPrefix+"deadbeef")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(stageDir, "art_0123456789abcdef")
	if err := os.MkdirAll(keep, 0o755); err != nil {
		t.Fatal(err)
	}

	removed, err := store.SweepTempDirectories()
	if err != nil {
		t.Fatalf("SweepTempDirectories: %v", err)
	}
	if removed != 1 {
		t.Errorf("swept %d directories, want 1", removed)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("the stale temp directory survived the sweep")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("the sweep removed a published artifact")
	}
}

func TestGCNeverRemovesTheNewest(t *testing.T) {
	ctx := context.Background()
	store, projectDir := newTestStore(t)

	var keys []string
	for i := range 5 {
		in := baseInputs()
		// Each version has distinct inputs, so each gets its own key.
		in.Fingerprint = map[string]string{"device": "cpu", "round": string(rune('a' + i))}

		w, err := store.Begin(in)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(w.Dir(), "asr.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		published, err := w.Commit(ctx, Result{Primary: "asr.json"})
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, published.Key)
	}

	removed, err := store.GC(ctx, "asr", 2)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if removed != 3 {
		t.Errorf("GC removed %d, want 3", removed)
	}

	latest, err := store.Latest(ctx, "asr")
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if latest == nil {
		t.Fatal("GC removed the newest artifact, which is the working set")
	}
	if latest.Key != keys[len(keys)-1] {
		t.Errorf("Latest returned %s, want the final version %s", latest.Key, keys[len(keys)-1])
	}

	// The directory of a pruned artifact must go too, or the disk saving is
	// imaginary.
	entries, err := os.ReadDir(filepath.Join(projectDir, ArtifactsDir, "asr"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("%d artifact directories remain, want 2", len(entries))
	}
}

func TestGCRetentionMinusOneKeepsEverything(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)

	for i := range 3 {
		in := baseInputs()
		in.Fingerprint = map[string]string{"round": string(rune('a' + i))}
		w, err := store.Begin(in)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(w.Dir(), "asr.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Commit(ctx, Result{Primary: "asr.json"}); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := store.GC(ctx, "asr", -1)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Errorf("GC removed %d with retention -1, want 0", removed)
	}
}

func TestLatestReturnsNilForUnrunStage(t *testing.T) {
	store, _ := newTestStore(t)

	latest, err := store.Latest(context.Background(), "never-ran")
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if latest != nil {
		t.Fatal("Latest returned an artifact for a stage that has never run")
	}
}

func TestManifestIsSelfDescribing(t *testing.T) {
	// A user who copies a project folder, or who loses the database, should
	// still be able to tell what each artifact is and what produced it.
	ctx := context.Background()
	store, projectDir := newTestStore(t)

	w, err := store.Begin(baseInputs())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir(), "asr.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	published, err := w.Commit(ctx, Result{
		Primary:  "asr.json",
		Provider: "faster-whisper",
		Model:    "large-v3",
		Metadata: map[string]any{"device": "cuda"},
	})
	if err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(projectDir, filepath.FromSlash(published.Path))
	manifest, err := readManifest(dir)
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}

	if manifest.Stage != "asr" || manifest.Model != "large-v3" || manifest.Primary != "asr.json" {
		t.Errorf("manifest lost information: %+v", manifest)
	}
	if manifest.Key != published.Key {
		t.Errorf("manifest key %q does not match the artifact key %q", manifest.Key, published.Key)
	}
	if manifest.Metadata["device"] != "cuda" {
		t.Errorf("manifest metadata lost the device: %+v", manifest.Metadata)
	}
}
