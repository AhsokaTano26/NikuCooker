package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultsValidate(t *testing.T) {
	// A default that fails its own validation is a first run that cannot start,
	// which is the one run that must always work.
	if err := Default().Validate(); err != nil {
		t.Fatalf("defaults are invalid: %v", err)
	}
}

func TestLoadAppliesLayersInOrder(t *testing.T) {
	file := Layer{Source: SourceFile, Data: map[string]any{
		"asr": map[string]any{"model": "small", "beam_size": 3},
		"log": map[string]any{"level": "warn"},
	}}
	env := Layer{Source: SourceEnv, Data: map[string]any{
		"asr": map[string]any{"model": "large-v3"},
	}}

	cfg, prov, err := Load(file, env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// The later layer wins...
	if cfg.ASR.Model != "large-v3" {
		t.Errorf("asr.model = %q, want large-v3 (the environment layer)", cfg.ASR.Model)
	}
	// ...and the earlier layer's other keys survive, which is the whole point of
	// merging partial documents rather than replacing them.
	if cfg.ASR.BeamSize != 3 {
		t.Errorf("asr.beam_size = %d, want 3 (from the file layer)", cfg.ASR.BeamSize)
	}
	if cfg.Log.Level != "warn" {
		t.Errorf("log.level = %q, want warn", cfg.Log.Level)
	}

	if prov["asr.model"] != SourceEnv {
		t.Errorf("provenance for asr.model = %q, want %q", prov["asr.model"], SourceEnv)
	}
	if prov["asr.beam_size"] != SourceFile {
		t.Errorf("provenance for asr.beam_size = %q, want %q", prov["asr.beam_size"], SourceFile)
	}
	if _, recorded := prov["storage.data_dir"]; recorded {
		t.Error("storage.data_dir has provenance but no layer set it")
	}
}

func TestUnknownKeyIsRejected(t *testing.T) {
	// A silently ignored typo is the most common reason a setting "does not
	// work". Rejecting it names the key and the line.
	_, _, err := Load(Layer{Source: SourceFile, Data: map[string]any{
		"asr": map[string]any{"modle": "small"},
	}})
	if err == nil {
		t.Fatal("accepted an unknown key")
	}
	if !strings.Contains(err.Error(), "modle") {
		t.Errorf("error does not name the offending key: %v", err)
	}
}

func TestFileLayer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := `
asr:
  model: small
  device: cpu
translation:
  timeout: 45s
  batch_size: 8
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	layer, err := FileLayer(path)
	if err != nil {
		t.Fatalf("FileLayer: %v", err)
	}
	cfg, _, err := Load(layer)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.ASR.Model != "small" || cfg.ASR.Device != "cpu" {
		t.Errorf("asr = %+v, want model=small device=cpu", cfg.ASR)
	}
	if cfg.Translation.Timeout != 45*time.Second {
		t.Errorf("translation.timeout = %s, want 45s", cfg.Translation.Timeout)
	}
	if cfg.Translation.BatchSize != 8 {
		t.Errorf("translation.batch_size = %d, want 8", cfg.Translation.BatchSize)
	}
	// A key the file does not mention keeps its default.
	if cfg.Translation.MaxRetries != Default().Translation.MaxRetries {
		t.Error("a key absent from the file did not keep its default")
	}
}

func TestMissingFileIsNotAnError(t *testing.T) {
	// Defaults must suffice, or every install needs a config file before it
	// will start.
	layer, err := FileLayer(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("a missing file must not be an error: %v", err)
	}
	if len(layer.Data) != 0 {
		t.Errorf("missing file produced %d keys", len(layer.Data))
	}
}

func TestUnreadableFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("asr: [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Running on defaults while the user believes their file is in effect is
	// worse than refusing to start.
	if _, err := FileLayer(path); err == nil {
		t.Fatal("a malformed file must be an error")
	}
}

func TestEnvLayerCoercion(t *testing.T) {
	layer, err := EnvLayer([]string{
		"NIKUCOOKER_PORT=9000",
		"NIKUCOOKER_DATA_DIR=/srv/niku",
		"NIKUCOOKER_ASR_MODEL=large-v3",
		"NIKUCOOKER_WORKER_POOL_SIZE=2",
		"PATH=/usr/bin",
		"IGNORED=1",
	})
	if err != nil {
		t.Fatalf("EnvLayer: %v", err)
	}

	cfg, _, err := Load(layer)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Server.Port != 9000 {
		t.Errorf("server.port = %d, want 9000 (coerced to int)", cfg.Server.Port)
	}
	if cfg.Storage.DataDir != "/srv/niku" {
		t.Errorf("storage.data_dir = %q", cfg.Storage.DataDir)
	}
	if cfg.ASR.Model != "large-v3" {
		t.Errorf("asr.model = %q", cfg.ASR.Model)
	}
	if cfg.Worker.PoolSize != 2 {
		t.Errorf("worker.pool_size = %d", cfg.Worker.PoolSize)
	}
}

func TestEnvLayerRejectsBadValue(t *testing.T) {
	// The message must name the variable, or the user is left scanning a
	// forty-line environment for the one that is wrong.
	_, err := EnvLayer([]string{"NIKUCOOKER_PORT=not-a-number"})
	if err == nil {
		t.Fatal("accepted a non-numeric port")
	}
	if !strings.Contains(err.Error(), "NIKUCOOKER_PORT") {
		t.Errorf("error does not name the variable: %v", err)
	}
}

func TestValidateNamesEveryProblem(t *testing.T) {
	cfg := Default()
	cfg.Server.Port = 0
	cfg.ASR.Device = "metal"
	cfg.Translation.Style = "shakespearean"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation to fail")
	}
	// Reporting one problem at a time means three restarts to fix three typos.
	for _, want := range []string{"server.port", "asr.device", "translation.style"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%v", want, err)
		}
	}
}

func TestProjectOverlayDoesNotMutateGlobal(t *testing.T) {
	global, _, err := Load(Layer{Source: SourceFile, Data: map[string]any{
		"asr": map[string]any{"model": "medium"},
	}})
	if err != nil {
		t.Fatal(err)
	}

	effective := global.Clone()
	prov, err := effective.Apply(SourceProject, map[string]any{
		"asr": map[string]any{"model": "large-v3"},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if effective.ASR.Model != "large-v3" {
		t.Errorf("overlay did not apply: asr.model = %q", effective.ASR.Model)
	}
	// A leak here would mean one project's settings silently changing another's.
	if global.ASR.Model != "medium" {
		t.Errorf("the overlay mutated the global config: asr.model = %q", global.ASR.Model)
	}
	if prov["asr.model"] != SourceProject {
		t.Errorf("provenance = %q, want %q", prov["asr.model"], SourceProject)
	}
}

func TestCloneIsDeep(t *testing.T) {
	original := Default()
	clone := original.Clone()

	clone.ASR.Model = "tiny"
	clone.Subtitle.Formats[0] = "vtt"

	if original.ASR.Model == "tiny" {
		t.Error("Clone shares the ASR struct")
	}
	// A shallow copy would share the backing array, so mutating one project's
	// output formats would change every other project's.
	if original.Subtitle.Formats[0] == "vtt" {
		t.Error("Clone shares the Subtitle.Formats backing array")
	}
}

func TestConfigPathFromEnvironment(t *testing.T) {
	got := ConfigPath([]string{"PATH=/usr/bin", "NIKUCOOKER_CONFIG=/etc/niku.yaml"})
	if got != "/etc/niku.yaml" {
		t.Errorf("ConfigPath = %q", got)
	}
	if ConfigPath([]string{"PATH=/usr/bin"}) != "" {
		t.Error("ConfigPath returned a value with no NIKUCOOKER_CONFIG set")
	}
}

// A relative directory is resolved once, here, against the working directory
// that wrote it.
//
// It matters because these paths do not stay in this process. They are handed
// to the Python worker, which runs with a working directory of its own, so a
// relative path crossing that boundary resolves against something else and
// every artifact path derived from it points at a file that does not exist.
func TestResolvePathsMakesConfiguredDirectoriesAbsolute(t *testing.T) {
	cfg := Default()
	cfg.Storage.DataDir = filepath.Join(".", "data")
	cfg.Storage.ModelDir = filepath.Join(".", "models")

	if err := cfg.ResolvePaths(); err != nil {
		t.Fatal(err)
	}

	if !filepath.IsAbs(cfg.Storage.DataDir) {
		t.Errorf("data_dir = %q, want an absolute path", cfg.Storage.DataDir)
	}
	if !filepath.IsAbs(cfg.Storage.ModelDir) {
		t.Errorf("model_dir = %q, want an absolute path", cfg.Storage.ModelDir)
	}

	// Resolved against the working directory, not merely prefixed with a
	// separator: the value has to name where the user meant.
	if base := filepath.Base(cfg.Storage.DataDir); base != "data" {
		t.Errorf("data_dir = %q, want it to still end in %q", cfg.Storage.DataDir, "data")
	}
}

// An absolute path is left exactly as written. Rewriting one would be a
// surprise, and on a system with symlinked directories it would be a different
// place.
func TestResolvePathsLeavesAbsolutePathsAlone(t *testing.T) {
	cfg := Default()
	cfg.Storage.DataDir = filepath.Join(string(filepath.Separator), "srv", "nikucooker")

	if err := cfg.ResolvePaths(); err != nil {
		t.Fatal(err)
	}

	if cfg.Storage.DataDir != filepath.Join(string(filepath.Separator), "srv", "nikucooker") {
		t.Errorf("data_dir = %q, want it unchanged", cfg.Storage.DataDir)
	}
}
