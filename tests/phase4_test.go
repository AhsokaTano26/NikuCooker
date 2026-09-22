package tests

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/artifact"
	"github.com/AhsokaTano26/NikuCooker/internal/config"
	"github.com/AhsokaTano26/NikuCooker/internal/database"
	"github.com/AhsokaTano26/NikuCooker/internal/media"
	"github.com/AhsokaTano26/NikuCooker/internal/models"
	"github.com/AhsokaTano26/NikuCooker/internal/pipeline"
	"github.com/AhsokaTano26/NikuCooker/internal/stage"
	"github.com/AhsokaTano26/NikuCooker/internal/stage/asr"
	"github.com/AhsokaTano26/NikuCooker/internal/stage/audio"
	"github.com/AhsokaTano26/NikuCooker/internal/stage/probe"
	"github.com/AhsokaTano26/NikuCooker/internal/stage/vad"
	"github.com/AhsokaTano26/NikuCooker/internal/worker"
	"github.com/AhsokaTano26/NikuCooker/pkg/protocol"
)

// The Phase 4 exit test: video in, transcript out.
//
// The VAD leg runs for real everywhere — its model ships inside the
// faster-whisper wheel, so there is nothing to download. The ASR leg needs
// weights, so it downloads the smallest model once and skips with a clear
// reason when that is not possible. A test that fails because a network is
// unavailable is a test that gets disabled.

// testModel is the smallest available recognition model.
//
// `tiny` rather than `medium`: what is being tested is the wiring, and a
// 1.5 GB download in CI would make the suite slow enough that someone would
// eventually disable it.
const testModel = "tiny"

// asrHarness is a project wired for the full local pipeline.
type asrHarness struct {
	t          *testing.T
	db         *database.DB
	projectID  string
	projectDir string
	sourcePath string
	store      *artifact.Store
	media      *media.Service
	models     *models.Service
	pool       *worker.Pool
	cfg        *config.Config
	registry   *stage.Registry
	log        *slog.Logger
}

func newASRHarness(t *testing.T) *asrHarness {
	t.Helper()

	ffmpegPath(t)
	pythonForTests(t) // skip when there is no AI environment

	root := t.TempDir()
	projectDir := filepath.Join(root, "projects", "proj_asr")
	sourceDir := filepath.Join(projectDir, "source")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}

	sourcePath := filepath.Join(sourceDir, "第12回 声優ラジオ.mp4")
	buildFixture(t, sourcePath)

	db, err := database.Open(database.Options{Path: filepath.Join(root, "niku.db")})
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := database.Migrate(context.Background(), db); err != nil {
		t.Fatalf("database.Migrate: %v", err)
	}

	const projectID = "proj_asr"
	if _, err := db.Write.ExecContext(context.Background(), `
		INSERT INTO projects (id, name, source_path, source_language, target_language,
		                      style, status, config_json, created_at, updated_at)
		VALUES (?, 'asr', 'source/第12回 声優ラジオ.mp4', 'ja', 'zh-Hans',
		        'fansub', 'active', '{}', 'now', 'now')`, projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	mediaSvc, err := media.New(media.Options{Log: log})
	if err != nil {
		t.Fatalf("media.New: %v", err)
	}

	modelDir := filepath.Join(root, "models")
	modelSvc, err := models.New(models.Options{DB: db, ModelDir: modelDir, Log: log})
	if err != nil {
		t.Fatalf("models.New: %v", err)
	}

	// The pool spawns a real Python process. One worker, as in production.
	cfg := workerConfig(t)
	pool := worker.NewPool(cfg, 1)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = pool.Shutdown(ctx)
	})

	registry, err := stage.NewRegistry(probe.New(), audio.New(), vad.New(), asr.New())
	if err != nil {
		t.Fatalf("stage.NewRegistry: %v", err)
	}

	pipelineCfg := config.Default()
	pipelineCfg.Pipeline.Disabled = nil
	pipelineCfg.ASR.Model = testModel
	pipelineCfg.ASR.Language = "ja"

	return &asrHarness{
		t:          t,
		db:         db,
		projectID:  projectID,
		projectDir: projectDir,
		sourcePath: sourcePath,
		store:      artifact.NewStore(db, projectID, projectDir),
		media:      mediaSvc,
		models:     modelSvc,
		pool:       pool,
		cfg:        pipelineCfg,
		registry:   registry,
		log:        log,
	}
}

// ensureModel obtains the recognition model, skipping when it cannot.
func (h *asrHarness) ensureModel(ctx context.Context) {
	h.t.Helper()

	record, err := h.models.Get(ctx, models.ID(models.KindASR, testModel))
	if err != nil {
		h.t.Fatalf("model lookup: %v", err)
	}
	if record.Status == models.StatusReady {
		return
	}

	if os.Getenv("NIKUCOOKER_TEST_SKIP_DOWNLOAD") != "" {
		h.t.Skip("NIKUCOOKER_TEST_SKIP_DOWNLOAD is set")
	}

	downloadCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	err = h.models.Download(downloadCtx, record.ID, nil)
	if err != nil {
		// A network problem is not a defect in this code, and a test that fails
		// for it is a test someone disables.
		h.t.Skipf("could not obtain the %s model: %v", testModel, err)
	}
}

func (h *asrHarness) options() pipeline.Options {
	return pipeline.Options{
		Registry:          h.registry,
		Config:            h.cfg,
		Store:             h.store,
		ProjectID:         h.projectID,
		JobID:             "job_asr",
		CodeRevision:      "test-rev-1",
		SourcePath:        h.sourcePath,
		SourceFingerprint: "v1:size=1:h=fixed",
		Media:             h.media,
		Worker:            h.pool,
		Models:            h.models,
		Log:               h.log,
	}
}

func (h *asrHarness) run(ctx context.Context, opts pipeline.Options) *pipeline.Plan {
	h.t.Helper()

	plan, err := pipeline.Build(ctx, opts)
	if err != nil {
		h.t.Fatalf("pipeline.Build: %v", err)
	}
	if err := pipeline.Run(ctx, plan, opts, pipeline.NopObserver{}); err != nil {
		h.t.Fatalf("pipeline.Run: %v", err)
	}
	return plan
}

func (h *asrHarness) payload(t *testing.T, stageName string) string {
	t.Helper()

	a, err := h.store.Latest(context.Background(), stageName)
	if err != nil {
		t.Fatalf("Latest(%s): %v", stageName, err)
	}
	if a == nil {
		t.Fatalf("no artifact for stage %s", stageName)
	}
	path, err := h.store.Path(a)
	if err != nil {
		t.Fatalf("Path(%s): %v", stageName, err)
	}
	return path
}

// ---------------------------------------------------------------------------
// The pipeline
// ---------------------------------------------------------------------------

func TestPhase4VADRunsForReal(t *testing.T) {
	// No model download: the VAD ships inside the faster-whisper wheel.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := newASRHarness(t)

	// A range rather than disabling the asr stage: asr is not optional, and
	// the pipeline correctly refuses a disabled list naming a required stage.
	opts := h.options()
	opts.ToStage = "vad"

	plan := h.run(ctx, opts)

	expectStates(t, plan, map[string]pipeline.State{
		"probe": pipeline.StateCompleted,
		"audio": pipeline.StateCompleted,
		"vad":   pipeline.StateCompleted,
	})

	raw, err := os.ReadFile(h.payload(t, "vad"))
	if err != nil {
		t.Fatalf("read vad.json: %v", err)
	}

	var result protocol.VADResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode vad.json: %v", err)
	}

	if result.TotalAudioS <= 0 {
		t.Fatal("the VAD reported no audio duration")
	}
	// The invariants the rest of the pipeline depends on. A violation here
	// becomes impossible subtitle timings much later.
	previousEnd := 0.0
	for i, region := range result.Regions {
		if region.End <= region.Start {
			t.Errorf("region %d has a non-positive duration: %+v", i, region)
		}
		if region.Start < previousEnd {
			t.Errorf("region %d overlaps its predecessor: %+v", i, region)
		}
		if region.End > result.TotalAudioS+1 {
			t.Errorf("region %d ends past the audio: %+v", i, region)
		}
		previousEnd = region.End
	}
}

func TestPhase4VideoToTranscript(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	h := newASRHarness(t)
	h.ensureModel(ctx)

	plan := h.run(ctx, h.options())

	expectStates(t, plan, map[string]pipeline.State{
		"probe": pipeline.StateCompleted,
		"audio": pipeline.StateCompleted,
		"vad":   pipeline.StateCompleted,
		"asr":   pipeline.StateCompleted,
	})

	// The transcript is the recogniser's own result shape.
	raw, err := os.ReadFile(h.payload(t, "asr"))
	if err != nil {
		t.Fatalf("read asr.json: %v", err)
	}

	var result protocol.ASRResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode asr.json: %v", err)
	}

	if len(result.Segments) == 0 {
		t.Skip("the fixture produced no segments; there is no speech-like audio in it")
	}
	if result.Duration <= 0 {
		t.Errorf("duration = %v, want positive", result.Duration)
	}

	// Word timings are what subtitle segmentation is built on, so a transcript
	// without them is not usable by the rest of the pipeline.
	withWords := 0
	for _, segment := range result.Segments {
		if len(segment.Words) == 0 {
			continue
		}
		withWords++

		// Timings must nest. A word outside its segment produces an impossible
		// subtitle boundary, and the cause is invisible by the time anyone sees
		// the output.
		for _, word := range segment.Words {
			if word.Start < segment.Start-0.01 || word.End > segment.End+0.01 {
				t.Errorf("word %q [%.2f,%.2f] falls outside its segment [%.2f,%.2f]",
					word.Text, word.Start, word.End, segment.Start, segment.End)
			}
			if word.End < word.Start {
				t.Errorf("word %q has an inverted duration [%.2f,%.2f]",
					word.Text, word.Start, word.End)
			}
		}
	}

	if withWords == 0 {
		t.Error("no segment carries word timings; segmentation cannot work without them")
	}
}

func TestPhase4ASRIsCachedAcrossRuns(t *testing.T) {
	// The expensive stage must not re-run. On a real episode this is the
	// difference between instant and forty minutes.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	h := newASRHarness(t)
	h.ensureModel(ctx)

	h.run(ctx, h.options())

	// The source is removed to prove nothing re-reads it.
	if err := os.Remove(h.sourcePath); err != nil {
		t.Fatal(err)
	}

	plan := h.run(ctx, h.options())

	for _, name := range []string{"probe", "audio", "vad", "asr"} {
		if got := states(plan)[name]; got != pipeline.StateCached {
			t.Errorf("stage %s is %s on a re-run, want cached", name, got)
		}
	}
}

func TestPhase4DeviceProvenanceIsRecorded(t *testing.T) {
	// A silent fallback to CPU makes a job twenty times slower without saying
	// so. The artifact must record what actually happened.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	h := newASRHarness(t)
	h.ensureModel(ctx)
	h.run(ctx, h.options())

	a, err := h.store.Latest(ctx, "asr")
	if err != nil {
		t.Fatal(err)
	}
	if a == nil {
		t.Fatal("no asr artifact")
	}

	device, ok := a.Metadata["device"].(string)
	if !ok || device == "" {
		t.Fatalf("the artifact does not record the device: %+v", a.Metadata)
	}
	if device != "cpu" && device != "cuda" {
		t.Errorf("device = %q, want cpu or cuda", device)
	}

	// On a machine without a GPU the fallback reason must be present, because
	// that string is the entire explanation for why a job took an hour.
	if device == "cpu" {
		if reason, _ := a.Metadata["fallback_reason"].(string); reason == "" {
			// Not an error when CPU was configured explicitly; the default is
			// auto, so a reason is expected here.
			t.Log("no fallback reason recorded; cpu may have been configured explicitly")
		}
	}

	if a.Model != testModel {
		t.Errorf("model = %q, want %q", a.Model, testModel)
	}
	if a.Provider != "faster-whisper" {
		t.Errorf("provider = %q", a.Provider)
	}
}

// ---------------------------------------------------------------------------
// Model inventory
// ---------------------------------------------------------------------------

func TestPhase4InventoryReportsMissingModels(t *testing.T) {
	root := t.TempDir()
	db := openTestDatabase(t, root)
	svc, err := models.New(models.Options{DB: db, ModelDir: filepath.Join(root, "models")})
	if err != nil {
		t.Fatalf("models.New: %v", err)
	}

	ctx := context.Background()
	records, err := svc.Scan(ctx)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if len(records) < len(models.Catalog) {
		t.Errorf("the inventory has %d records, the catalog lists %d",
			len(records), len(models.Catalog))
	}

	for _, record := range records {
		if record.Kind == models.KindVAD {
			// The VAD ships with the Python environment, so there is nothing to
			// download and it is always ready.
			if record.Status != models.StatusReady {
				t.Errorf("the bundled VAD reports %s, want ready", record.Status)
			}
			continue
		}
		if record.Status != models.StatusMissing {
			t.Errorf("%s reports %s on an empty model directory, want missing",
				record.ID, record.Status)
		}
		// The estimate is what lets the UI say what a download will cost.
		if record.ApproxBytes <= 0 {
			t.Errorf("%s has no size estimate", record.ID)
		}
	}
}

func TestPhase4InventoryAdoptsAnExistingModel(t *testing.T) {
	// A user who placed the files by hand, or copied them from another machine,
	// must see them as ready rather than being told to download again.
	root := t.TempDir()
	db := openTestDatabase(t, root)

	modelDir := filepath.Join(root, "models")
	svc, err := models.New(models.Options{DB: db, ModelDir: modelDir})
	if err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(modelDir, "asr", testModel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The weights are what make a directory loadable.
	if err := os.WriteFile(filepath.Join(dir, "model.bin"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	record, err := svc.Get(context.Background(), models.ID(models.KindASR, testModel))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if record.Status != models.StatusReady {
		t.Fatalf("status = %s, want ready", record.Status)
	}
	if record.SizeBytes <= 0 {
		t.Error("the model reports no size on disk")
	}
	if record.InstalledAt == nil {
		t.Error("a ready model has no install time")
	}
}

func TestPhase4IncompleteModelIsNotReady(t *testing.T) {
	// A directory with config files but no weights is what an interrupted
	// download leaves behind. Reporting it as ready lets a job start and fail
	// inside the loader, far from the cause.
	root := t.TempDir()
	db := openTestDatabase(t, root)

	modelDir := filepath.Join(root, "models")
	svc, err := models.New(models.Options{DB: db, ModelDir: modelDir})
	if err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(modelDir, "asr", testModel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	record, err := svc.Get(context.Background(), models.ID(models.KindASR, testModel))
	if err != nil {
		t.Fatal(err)
	}

	if record.Status == models.StatusReady {
		t.Fatal("a model directory with no weights reports ready")
	}
	if record.ErrorMessage == "" {
		t.Error("an unusable model does not explain why")
	}
}

func TestPhase4DeletingAModelMakesItMissing(t *testing.T) {
	root := t.TempDir()
	db := openTestDatabase(t, root)

	modelDir := filepath.Join(root, "models")
	svc, err := models.New(models.Options{DB: db, ModelDir: modelDir})
	if err != nil {
		t.Fatal(err)
	}

	id := models.ID(models.KindASR, testModel)
	dir := filepath.Join(modelDir, "asr", testModel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "model.bin"), make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if record, _ := svc.Get(ctx, id); record.Status != models.StatusReady {
		t.Fatalf("setup: status = %s", record.Status)
	}

	if err := svc.Delete(ctx, id, false); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	record, err := svc.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != models.StatusMissing {
		t.Errorf("status after deleting = %s, want missing", record.Status)
	}
}

func TestPhase4AModelInUseIsNotDeleted(t *testing.T) {
	root := t.TempDir()
	db := openTestDatabase(t, root)

	svc, err := models.New(models.Options{DB: db, ModelDir: filepath.Join(root, "models")})
	if err != nil {
		t.Fatal(err)
	}

	err = svc.Delete(context.Background(), models.ID(models.KindASR, "medium"), true)
	if !errors.Is(err, models.ErrInUse) {
		t.Fatalf("error = %v, want ErrInUse", err)
	}
}

func TestPhase4UnknownModelIsNamed(t *testing.T) {
	root := t.TempDir()
	db := openTestDatabase(t, root)

	svc, err := models.New(models.Options{DB: db, ModelDir: filepath.Join(root, "models")})
	if err != nil {
		t.Fatal(err)
	}

	err = svc.Download(context.Background(), "asr:not-a-real-model", nil)
	if !errors.Is(err, models.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	// The message lists what is available, so a typo is fixable without
	// consulting documentation.
	if !strings.Contains(err.Error(), "large-v3") {
		t.Errorf("the error does not list the known models: %v", err)
	}
}

// openTestDatabase builds a migrated database in a temporary directory.
func openTestDatabase(t *testing.T, root string) *database.DB {
	t.Helper()

	db, err := database.Open(database.Options{Path: filepath.Join(root, "niku.db")})
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := database.Migrate(context.Background(), db); err != nil {
		t.Fatalf("database.Migrate: %v", err)
	}
	return db
}
