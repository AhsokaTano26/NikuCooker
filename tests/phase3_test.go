package tests

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AhsokaTano26/NikuCooker/internal/artifact"
	"github.com/AhsokaTano26/NikuCooker/internal/config"
	"github.com/AhsokaTano26/NikuCooker/internal/database"
	"github.com/AhsokaTano26/NikuCooker/internal/media"
	"github.com/AhsokaTano26/NikuCooker/internal/pipeline"
	"github.com/AhsokaTano26/NikuCooker/internal/stage"
	"github.com/AhsokaTano26/NikuCooker/internal/stage/audio"
	"github.com/AhsokaTano26/NikuCooker/internal/stage/probe"
)

// The Phase 3 exit test: video in, canonical WAV out.
//
// The fixture is generated rather than committed. A binary test asset is a file
// nobody can read in a diff, nobody can regenerate when it goes stale, and
// every reviewer has to take on trust. FFmpeg's synthetic sources produce the
// same thing on every platform, deterministically.

// ffmpegPath returns the ffmpeg binary, or skips the test.
func ffmpegPath(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	return path
}

// buildFixture writes a short video with a picture and a tone.
//
// It has both a video and an audio stream because a file with only one of them
// does not exercise the stream-selection logic that the interesting bugs live
// in.
func buildFixture(t *testing.T, path string) {
	t.Helper()

	ffmpeg := ffmpegPath(t)
	args := []string{
		"-hide_banner", "-nostdin", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=10",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "64k",
		"-shortest",
		path,
	}

	if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("could not build the test fixture: %v\n%s", err, out)
	}
}

// mediaHarness is a project wired for the real media stages.
type mediaHarness struct {
	t          *testing.T
	db         *database.DB
	projectID  string
	projectDir string
	sourcePath string
	store      *artifact.Store
	media      *media.Service
	cfg        *config.Config
	registry   *stage.Registry
	log        *slog.Logger
}

func newMediaHarness(t *testing.T) *mediaHarness {
	t.Helper()

	ffmpegPath(t) // skip early when FFmpeg is absent

	root := t.TempDir()
	projectDir := filepath.Join(root, "projects", "proj_media")
	sourceDir := filepath.Join(projectDir, "source")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// A source name with a space and Japanese characters, so every invocation
	// in this test exercises the path handling rather than assuming it.
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

	const projectID = "proj_media"
	if _, err := db.Write.ExecContext(context.Background(), `
		INSERT INTO projects (id, name, source_path, source_language, target_language,
		                      style, status, config_json, created_at, updated_at)
		VALUES (?, 'media', 'source/第12回 声優ラジオ.mp4', 'ja', 'zh-Hans',
		        'fansub', 'active', '{}', 'now', 'now')`, projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	svc, err := media.New(media.Options{Log: log})
	if err != nil {
		t.Fatalf("media.New: %v", err)
	}

	registry, err := stage.NewRegistry(probe.New(), audio.New())
	if err != nil {
		t.Fatalf("stage.NewRegistry: %v", err)
	}

	cfg := config.Default()
	cfg.Pipeline.Disabled = nil

	return &mediaHarness{
		t:          t,
		db:         db,
		projectID:  projectID,
		projectDir: projectDir,
		sourcePath: sourcePath,
		store:      artifact.NewStore(db, projectID, projectDir),
		media:      svc,
		cfg:        cfg,
		registry:   registry,
		log:        log,
	}
}

func (h *mediaHarness) options() pipeline.Options {
	return pipeline.Options{
		Registry:          h.registry,
		Config:            h.cfg,
		Store:             h.store,
		ProjectID:         h.projectID,
		JobID:             "job_media",
		CodeRevision:      "test-rev-1",
		SourcePath:        h.sourcePath,
		SourceFingerprint: "v1:size=1:h=fixed",
		Media:             h.media,
		Log:               h.log,
	}
}

func (h *mediaHarness) run(opts pipeline.Options) *pipeline.Plan {
	h.t.Helper()

	ctx := context.Background()
	plan, err := pipeline.Build(ctx, opts)
	if err != nil {
		h.t.Fatalf("pipeline.Build: %v", err)
	}
	if err := pipeline.Run(ctx, plan, opts, pipeline.NopObserver{}); err != nil {
		h.t.Fatalf("pipeline.Run: %v", err)
	}
	return plan
}

// payloadPath resolves an artifact's primary file on disk.
func (h *mediaHarness) payloadPath(t *testing.T, stageName string) string {
	t.Helper()

	a, err := h.store.Latest(context.Background(), stageName)
	if err != nil {
		t.Fatalf("Latest(%s): %v", stageName, err)
	}
	if a == nil {
		t.Fatalf("no artifact for stage %s", stageName)
	}
	dir := filepath.Join(h.projectDir, filepath.FromSlash(a.Path))
	primary, err := artifact.PrimaryOf(dir)
	if err != nil {
		t.Fatalf("PrimaryOf(%s): %v", stageName, err)
	}
	return filepath.Join(dir, primary)
}

// ---------------------------------------------------------------------------
// The pipeline
// ---------------------------------------------------------------------------

func TestPhase3VideoToWav(t *testing.T) {
	h := newMediaHarness(t)

	plan := h.run(h.options())

	expectStates(t, plan, map[string]pipeline.State{
		"probe": pipeline.StateCompleted,
		"audio": pipeline.StateCompleted,
	})

	// The probe artifact describes the file truthfully.
	var info media.MediaInfo
	raw, err := os.ReadFile(h.payloadPath(t, "probe"))
	if err != nil {
		t.Fatalf("read probe.json: %v", err)
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatalf("decode probe.json: %v", err)
	}

	if info.Duration < 1.5 || info.Duration > 2.5 {
		t.Errorf("duration = %v, want ~2s", info.Duration)
	}
	if !info.HasVideo() {
		t.Error("the fixture has a video stream but the probe found none")
	}
	if len(info.Audio) != 1 {
		t.Fatalf("found %d audio streams, want 1", len(info.Audio))
	}
	if info.Audio[0].SampleRate != 44100 && info.Audio[0].SampleRate != 48000 {
		t.Errorf("source sample rate = %d, expected a normal AAC rate", info.Audio[0].SampleRate)
	}

	// And the extracted audio is what the recogniser requires.
	wavPath := h.payloadPath(t, "audio")
	assertWavIsRecogniserReady(t, h, wavPath, info.Duration)
}

// assertWavIsRecogniserReady checks the canonical form.
//
// Every property is asserted against a fresh ffprobe of the produced file
// rather than against the stage's own metadata: a stage that reports what it
// intended rather than what it produced is exactly the failure this catches.
func assertWavIsRecogniserReady(t *testing.T, h *mediaHarness, path string, sourceDuration float64) {
	t.Helper()

	info, err := h.media.Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("probe the extracted audio: %v", err)
	}

	if len(info.Audio) == 0 {
		t.Fatal("the extracted file has no audio stream")
	}
	stream := info.Audio[0]

	// 16 kHz mono is what Whisper consumes. Anything else means the model
	// downsamples it again, or transcribes a stereo file as if it were mono.
	if stream.SampleRate != 16000 {
		t.Errorf("sample rate = %d, want 16000", stream.SampleRate)
	}
	if stream.Channels != 1 {
		t.Errorf("channels = %d, want 1", stream.Channels)
	}
	if !strings.Contains(stream.Codec, "pcm") {
		t.Errorf("codec = %q, want uncompressed PCM", stream.Codec)
	}

	// The audio must span the source, not be a truncated fragment: a stream
	// selection that matches nothing yields a valid header and no samples.
	if info.Duration < sourceDuration*0.5 {
		t.Errorf("extracted duration = %v, source is %v; the extraction was truncated",
			info.Duration, sourceDuration)
	}

	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// 16 kHz mono 16-bit is 32 kB/s; a two-second clip is comfortably above
	// this, and a header-only file is far below it.
	if stat.Size() < 32_000 {
		t.Errorf("the extracted file is %d bytes, too small to hold %v seconds of audio",
			stat.Size(), info.Duration)
	}
}

func TestPhase3RerunIsCached(t *testing.T) {
	// Re-running must not re-decode the source. On a two-hour video that is the
	// difference between instant and a minute of FFmpeg.
	h := newMediaHarness(t)

	h.run(h.options())

	// The source is removed to prove nothing re-reads it. If the second run
	// tried to extract again it would fail, not merely be slower.
	if err := os.Remove(h.sourcePath); err != nil {
		t.Fatal(err)
	}

	plan := h.run(h.options())

	expectStates(t, plan, map[string]pipeline.State{
		"probe": pipeline.StateCached,
		"audio": pipeline.StateCached,
	})
	if plan.NeedsRun() {
		t.Error("a fully cached plan reports that it needs to run")
	}
}

func TestPhase3ConfigChangeInvalidatesAudioNotProbe(t *testing.T) {
	// Changing the audio settings must re-extract. Re-probing would be wasted
	// work: the file has not changed.
	h := newMediaHarness(t)
	h.run(h.options())

	h.cfg.Audio.HighpassHz = 120

	plan := h.run(h.options())

	expectStates(t, plan, map[string]pipeline.State{
		"probe": pipeline.StateCached,
		"audio": pipeline.StateCompleted,
	})
}

func TestPhase3ProbeIsNotInvalidatedByAnything(t *testing.T) {
	// The probe output depends only on the file. Every other setting belongs to
	// a stage that reads it, and none of them belong here.
	h := newMediaHarness(t)
	h.run(h.options())

	h.cfg.Audio.Loudnorm = true
	h.cfg.ASR.Model = "large-v3"
	h.cfg.Subtitle.MaxCPS = 30
	h.cfg.Render.CRF = 30

	plan := h.run(h.options())

	for _, sp := range plan.Stages {
		if sp.Stage.Spec().Name != "probe" {
			continue
		}
		if sp.State != pipeline.StateCached {
			t.Errorf("probe is %s after unrelated settings changed, want cached", sp.State)
		}
	}
}

func TestPhase3AudioOnlyInputWorks(t *testing.T) {
	// A radio recording with no picture is a first-class case, not an error.
	h := newMediaHarness(t)

	ffmpeg := ffmpegPath(t)
	audioPath := filepath.Join(filepath.Dir(h.sourcePath), "radio.mp3")
	args := []string{
		"-hide_banner", "-nostdin", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:a", "libmp3lame", "-b:a", "64k",
		audioPath,
	}
	if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("could not build the audio fixture: %v\n%s", err, out)
	}

	h.sourcePath = audioPath
	// A different source means a different key, so this runs rather than
	// hitting the cache from the earlier test.
	h.options()

	plan := h.run(h.options())
	expectStates(t, plan, map[string]pipeline.State{
		"probe": pipeline.StateCompleted,
		"audio": pipeline.StateCompleted,
	})

	var info media.MediaInfo
	raw, err := os.ReadFile(h.payloadPath(t, "probe"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatal(err)
	}
	if info.HasVideo() {
		t.Error("an audio-only file reports a video stream")
	}
}

func TestPhase3ProbeSelectsTheSourceLanguageTrack(t *testing.T) {
	// A bilingual release is where "just take the first audio stream" produces
	// a transcript of the dub. The fixture has one track, so this asserts the
	// metadata records which was chosen — the decision itself is unit-tested in
	// the media package against a synthetic two-track probe.
	h := newMediaHarness(t)
	h.run(h.options())

	var info media.MediaInfo
	raw, err := os.ReadFile(h.payloadPath(t, "probe"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatal(err)
	}

	artifact, err := h.store.Latest(context.Background(), "probe")
	if err != nil {
		t.Fatal(err)
	}
	index, ok := artifact.Metadata["selected_audio_index"]
	if !ok {
		t.Fatal("the probe artifact does not record which audio stream was selected")
	}
	// Making the choice auditable is the point: "why did it transcribe the
	// dub?" is answerable from the artifact rather than by re-probing.
	if index == nil {
		t.Error("the selected stream index is nil")
	}
}
