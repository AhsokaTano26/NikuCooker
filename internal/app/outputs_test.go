package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/AhsokaTano26/NikuCooker/internal/artifact"
	"github.com/AhsokaTano26/NikuCooker/internal/project"
)

// outputsHarness is an application with one project and a way to give it
// artifacts without running anything.
func outputsHarness(t *testing.T) (*App, string) {
	t.Helper()

	dataDir := t.TempDir()
	application, err := New(context.Background(), Options{
		Flags: map[string]any{
			"storage": map[string]any{
				"data_dir":  dataDir,
				"model_dir": filepath.Join(dataDir, "models"),
			},
		},
		Environ:      []string{},
		CodeRevision: "test-rev-1",
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { _ = application.Close() })

	prj, err := application.Projects.Create(context.Background(), project.CreateRequest{
		Name:           "outputs",
		SourceLanguage: "ja",
		TargetLanguage: "zh-Hans",
		Style:          "fansub",
	})
	if err != nil {
		t.Fatalf("Projects.Create: %v", err)
	}
	return application, prj.ID
}

// giveArtifact publishes an artifact for a stage, containing the named files.
//
// One artifact per stage, because that is what a stage produces: a second
// artifact for the same stage is a different version of its output, not an
// addition to it — and only the latest is the project's current one.
func giveArtifact(t *testing.T, application *App, projectID, stage string, files map[string]string) {
	t.Helper()

	ctx := context.Background()
	store := application.Artifacts.For(projectID)

	writer, err := store.Begin(artifact.KeyInputs{
		Stage:        stage,
		StageVersion: "1",
		CodeRevision: "test-rev-1",
		Config:       map[string]any{"stage": stage},
	})
	if err != nil {
		t.Fatalf("Begin(%s): %v", stage, err)
	}

	// Deterministic primary, rather than whichever name the map happens to
	// yield first.
	primary := ""
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(writer.Dir(), name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if primary == "" || name < primary {
			primary = name
		}
	}

	if _, err := writer.Commit(ctx, artifact.Result{Primary: primary}); err != nil {
		t.Fatalf("Commit(%s): %v", stage, err)
	}
}

func publishedNames(t *testing.T, application *App, projectID string) []string {
	t.Helper()

	files, err := application.OutputFiles(projectID)
	if err != nil {
		t.Fatal(err)
	}

	out := make([]string, 0, len(files))
	for _, file := range files {
		out = append(out, file.Name)
	}
	sort.Strings(out)
	return out
}

// The published set is the project's current files, not the ones one run
// happened to touch.
//
// The failure this catches is silent and destructive: `--only render`
// recomputes a single stage and takes the others from the cache, so a list
// built from the run's plan contains a video and nothing else — and the output
// directory, which is replaced rather than added to, loses the subtitle files
// that are still perfectly current.
func TestDeliverablesCoverEveryStageNotJustTheOneThatRan(t *testing.T) {
	application, projectID := outputsHarness(t)
	ctx := context.Background()

	giveArtifact(t, application, projectID, "subtitle", map[string]string{
		"subs.srt": "1\n00:00:00,000 --> 00:00:01,000\nこんにちは\n",
		"subs.ass": "[Script Info]\n",
		// The stage's own record of what it wrote, which is not an output.
		"subtitles.json": "{}",
	})
	giveArtifact(t, application, projectID, "render", map[string]string{
		"episode.nikucooker.mkv": "not really a video",
	})

	// An intermediate the user did not ask for, which must stay out of the
	// listing even though its stage is just as current.
	giveArtifact(t, application, projectID, "probe", map[string]string{"probe.json": "{}"})

	files := application.deliverables(ctx, projectID)

	names := make([]string, 0, len(files))
	for _, file := range files {
		names = append(names, file.Name)
	}
	sort.Strings(names)

	want := []string{"episode.nikucooker.mkv", "subs.ass", "subs.srt"}
	if len(names) != len(want) {
		t.Fatalf("deliverables = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("deliverables = %v, want %v", names, want)
		}
	}
}

// Publishing twice with the same inputs changes nothing.
//
// Worth asserting because the second call goes through the remove-and-rename
// swap, and a swap that dropped a file it had just written would be invisible
// until a user noticed one of their outputs had gone.
func TestPublishingIsIdempotent(t *testing.T) {
	application, projectID := outputsHarness(t)
	ctx := context.Background()

	giveArtifact(t, application, projectID, "subtitle", map[string]string{"subs.srt": "first"})
	giveArtifact(t, application, projectID, "render", map[string]string{"episode.mkv": "video"})

	application.publishOutputs(ctx, projectID)
	first := publishedNames(t, application, projectID)

	application.publishOutputs(ctx, projectID)
	second := publishedNames(t, application, projectID)

	if len(first) != 2 {
		t.Fatalf("published = %v, want both files", first)
	}
	if len(second) != len(first) {
		t.Fatalf("published = %v after a second run, want %v", second, first)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("published = %v after a second run, want %v", second, first)
		}
	}
}

// A stored setting reaches the resolved configuration, with its type intact.
//
// The value makes two trips — JSON into the database and back, then YAML into
// the config struct — and each one is a place a whole number can become a
// float and an int field can then refuse it. Asserting on the config rather
// than on the stored row is what catches that.
func TestSettingsReachTheConfiguration(t *testing.T) {
	application, _ := outputsHarness(t)
	ctx := context.Background()

	if err := application.UpdateSettings(ctx, map[string]any{
		"asr.model":        "large-v3",
		"subtitle.max_cps": 22,
		"subtitle.formats": []any{"srt"},
		"vad.enabled":      false,
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	cfg := application.Config()
	if cfg.ASR.Model != "large-v3" {
		t.Errorf("asr.model = %q", cfg.ASR.Model)
	}
	if cfg.Subtitle.MaxCPS != 22 {
		t.Errorf("subtitle.max_cps = %v, want 22", cfg.Subtitle.MaxCPS)
	}
	if len(cfg.Subtitle.Formats) != 1 || cfg.Subtitle.Formats[0] != "srt" {
		t.Errorf("subtitle.formats = %v", cfg.Subtitle.Formats)
	}
	if cfg.VAD.Enabled {
		t.Error("vad.enabled was ignored")
	}

	// The provenance says where it came from, which is what the settings page
	// shows beside each field.
	if source := application.Provenance()["asr.model"]; source != "database" {
		t.Errorf("asr.model came from %q, want database", source)
	}
}

// A value the configuration rejects is not stored.
//
// The failure this prevents is a permanent one: the database is read at
// startup, so a value stored there that the configuration refuses is a server
// that will not start — and the settings page that would clear it needs the
// server.
func TestAnInvalidSettingIsNotStored(t *testing.T) {
	application, _ := outputsHarness(t)
	ctx := context.Background()

	// Empty passes the catalog's own checks and fails the configuration's.
	if err := application.UpdateSettings(ctx, map[string]any{"render.modes": []any{}}); err == nil {
		t.Fatal("an empty render.modes was accepted")
	}

	stored, err := application.Settings.All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Fatalf("stored = %#v, want nothing", stored)
	}

	// And the running configuration is untouched.
	if len(application.Config().Render.Modes) == 0 {
		t.Error("the live configuration was left with no render modes")
	}
}

// Clearing a setting puts the lower layers back in charge.
func TestClearingASettingRestoresTheDefault(t *testing.T) {
	application, _ := outputsHarness(t)
	ctx := context.Background()

	before := application.Config().Subtitle.MaxCPS

	if err := application.UpdateSettings(ctx, map[string]any{"subtitle.max_cps": 33}); err != nil {
		t.Fatal(err)
	}
	if application.Config().Subtitle.MaxCPS != 33 {
		t.Fatal("the setting did not take effect")
	}

	if err := application.ClearSetting(ctx, "subtitle.max_cps"); err != nil {
		t.Fatal(err)
	}
	if got := application.Config().Subtitle.MaxCPS; got != before {
		t.Errorf("subtitle.max_cps = %v after the reset, want %v", got, before)
	}
}
