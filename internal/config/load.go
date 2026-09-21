package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Source identifies the layer that supplied a value.
type Source string

const (
	SourceDefault Source = "default"
	SourceFile    Source = "config_file"
	SourceEnv     Source = "environment"
	SourceProject Source = "project"
	SourceCLI     Source = "cli"
)

// Provenance records which layer last set each configuration key.
//
// Keys are dotted paths ("asr.model"). It exists because "I changed the setting
// and nothing happened" is otherwise answered by reading the config file, the
// environment, the project overlay and the command line, and guessing which
// won.
type Provenance map[string]Source

// Layer is one source of configuration.
type Layer struct {
	Source Source
	Data   map[string]any
}

// Load resolves a configuration from the supplied layers, lowest precedence
// first.
func Load(layers ...Layer) (*Config, Provenance, error) {
	cfg := Default()
	prov := Provenance{}

	for _, layer := range layers {
		if len(layer.Data) == 0 {
			continue
		}
		applied, err := cfg.apply(layer)
		if err != nil {
			return nil, nil, err
		}
		for key, source := range applied {
			prov[key] = source
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, nil, err
	}
	return cfg, prov, nil
}

// Apply overlays one layer onto an existing config.
//
// It is what a project overlay uses: the global configuration is resolved once
// at startup, and each project layers its own sparse overrides on a copy. The
// returned provenance covers only the keys this layer set.
func (c *Config) Apply(source Source, data map[string]any) (Provenance, error) {
	if len(data) == 0 {
		return Provenance{}, nil
	}
	return c.apply(Layer{Source: source, Data: data})
}

func (c *Config) apply(layer Layer) (Provenance, error) {
	raw, err := yaml.Marshal(layer.Data)
	if err != nil {
		return nil, fmt.Errorf("config: encoding %s layer: %w", layer.Source, err)
	}

	// A partial document only sets the keys it contains, which is what makes
	// layering work: unmarshalling onto the accumulated struct leaves everything
	// else alone.
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	// Unknown keys are rejected rather than ignored. A silently ignored typo is
	// the single most common cause of a setting appearing not to work, and this
	// turns it into an error that names the key.
	dec.KnownFields(true)

	if err := dec.Decode(c); err != nil {
		var typeErr *yaml.TypeError
		if errors.As(err, &typeErr) {
			return nil, fmt.Errorf("config: invalid %s layer:\n  - %s",
				layer.Source, strings.Join(typeErr.Errors, "\n  - "))
		}
		return nil, fmt.Errorf("config: invalid %s layer: %w", layer.Source, err)
	}

	prov := Provenance{}
	for _, key := range flatten(layer.Data, "") {
		prov[key] = layer.Source
	}
	return prov, nil
}

// Clone returns a deep copy.
//
// A YAML round-trip rather than a hand-written copy: a hand-written one is a
// list of fields to keep in step with the struct, and the failure mode when it
// drifts is a per-project override leaking into the global config.
func (c *Config) Clone() *Config {
	raw, err := yaml.Marshal(c)
	if err != nil {
		// A Config marshals by construction; a failure means the type changed in
		// a way that broke YAML entirely.
		panic(fmt.Sprintf("config: clone: %v", err))
	}
	out := &Config{}
	if err := yaml.Unmarshal(raw, out); err != nil {
		panic(fmt.Sprintf("config: clone: %v", err))
	}
	return out
}

// ---------------------------------------------------------------------------
// File layer
// ---------------------------------------------------------------------------

// FileLayer reads a YAML configuration file.
//
// A missing file is not an error: the whole point of layered configuration is
// that defaults suffice. A file that exists but cannot be read or parsed is an
// error, because silently running on defaults when the user believes their
// configuration is in effect is worse than not starting.
func FileLayer(path string) (Layer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Layer{Source: SourceFile}, nil
		}
		return Layer{}, fmt.Errorf("config: reading %s: %w", path, err)
	}

	data := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&data); err != nil {
			return Layer{}, fmt.Errorf("config: parsing %s: %w", path, err)
		}
	}
	return Layer{Source: SourceFile, Data: data}, nil
}

// ---------------------------------------------------------------------------
// Environment layer
// ---------------------------------------------------------------------------

// envKind is how an environment variable's string is converted.
//
// The kind is declared rather than inferred from the value. Inferring it would
// misread an API key of "12345" as a number, and a silently retyped secret is
// an authentication failure with no trace of why.
type envKind int

const (
	envString envKind = iota
	envInt
	envBool
	envDuration
)

// envBindings is the complete set of recognised environment variables.
//
// An explicit table rather than a prefix convention: a convention has to guess
// where the section ends and the key begins, and every guess is wrong for some
// key containing an underscore.
var envBindings = []struct {
	Name string
	Path string
	Kind envKind
}{
	// NIKUCOOKER_CONFIG is absent deliberately: it selects the file to read
	// rather than setting a field, and is handled by ConfigPath.
	{"NIKUCOOKER_DATA_DIR", "storage.data_dir", envString},
	{"NIKUCOOKER_MODEL_DIR", "storage.model_dir", envString},
	{"NIKUCOOKER_HOST", "server.host", envString},
	{"NIKUCOOKER_PORT", "server.port", envInt},
	{"NIKUCOOKER_LOG_LEVEL", "log.level", envString},
	{"NIKUCOOKER_LOG_FORMAT", "log.format", envString},
	{"NIKUCOOKER_PYTHON", "ai.python", envString},
	{"NIKUCOOKER_ASR_MODEL", "asr.model", envString},
	{"NIKUCOOKER_ASR_DEVICE", "asr.device", envString},
	{"NIKUCOOKER_ASR_LANGUAGE", "asr.language", envString},
	{"NIKUCOOKER_TRANSLATION_BASE_URL", "translation.base_url", envString},
	{"NIKUCOOKER_TRANSLATION_API_KEY", "translation.api_key", envString},
	{"NIKUCOOKER_TRANSLATION_MODEL", "translation.model", envString},
	{"NIKUCOOKER_TRANSLATION_STYLE", "translation.style", envString},
	{"NIKUCOOKER_RENDER_MODE", "render.mode", envString},
	{"NIKUCOOKER_WORKER_POOL_SIZE", "worker.pool_size", envInt},
}

// EnvLayer reads configuration from the environment.
func EnvLayer(environ []string) (Layer, error) {
	present := make(map[string]string, len(environ))
	for _, entry := range environ {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			present[name] = value
		}
	}

	data := map[string]any{}
	for _, binding := range envBindings {
		raw, ok := present[binding.Name]
		if !ok {
			continue
		}
		value, err := coerce(raw, binding.Kind)
		if err != nil {
			return Layer{}, fmt.Errorf("config: %s: %w", binding.Name, err)
		}
		setPath(data, binding.Path, value)
	}
	return Layer{Source: SourceEnv, Data: data}, nil
}

func coerce(raw string, kind envKind) (any, error) {
	switch kind {
	case envString:
		return raw, nil
	case envInt:
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("expected an integer, got %q", raw)
		}
		return n, nil
	case envBool:
		b, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("expected a boolean (true/false/1/0), got %q", raw)
		}
		return b, nil
	case envDuration:
		d, err := time.ParseDuration(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("expected a duration such as 120s, got %q", raw)
		}
		return d.String(), nil
	default:
		return raw, nil
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// setPath writes a value into a nested map, creating intermediate maps.
func setPath(data map[string]any, path string, value any) {
	parts := strings.Split(path, ".")
	current := data

	for _, part := range parts[:len(parts)-1] {
		next, ok := current[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			current[part] = next
		}
		current = next
	}
	current[parts[len(parts)-1]] = value
}

// flatten lists the dotted key paths of every leaf in a nested map.
//
// A leaf is any value that is not a map. A null value counts as a leaf: it is
// how a layer says "explicitly unset", and recording it keeps provenance
// honest about which layer last spoke.
func flatten(data map[string]any, prefix string) []string {
	var keys []string
	for key, value := range data {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if nested, ok := value.(map[string]any); ok {
			keys = append(keys, flatten(nested, path)...)
			continue
		}
		keys = append(keys, path)
	}
	return keys
}

// ConfigPath returns the configuration file path to read, honouring
// NIKUCOOKER_CONFIG.
func ConfigPath(environ []string) string {
	for _, entry := range environ {
		if name, value, ok := strings.Cut(entry, "="); ok && name == "NIKUCOOKER_CONFIG" {
			return value
		}
	}
	return ""
}
