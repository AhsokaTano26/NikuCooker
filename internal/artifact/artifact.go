package artifact

import (
	"encoding/json"
	"time"
)

// Status is an artifact's lifecycle state.
type Status string

const (
	// StatusBuilding means the payload is being written. A row in this state
	// after a crash belongs to a run that never finished.
	StatusBuilding Status = "building"
	// StatusReady means the payload is complete and published.
	StatusReady Status = "ready"
	// StatusFailed means the stage errored. The row is kept so the UI can show
	// "translation failed three minutes ago" rather than an absence that is
	// indistinguishable from "never run".
	StatusFailed Status = "failed"
)

// Artifact is one stage's cached output.
type Artifact struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Stage     string `json:"stage"`

	InputHash  string `json:"input_hash"`
	ConfigHash string `json:"config_hash"`
	Key        string `json:"key"`

	Status Status `json:"status"`

	// Path is relative to the project directory, so the whole data tree is
	// relocatable.
	Path string `json:"path"`

	Provider      string `json:"provider,omitempty"`
	Model         string `json:"model,omitempty"`
	PromptVersion string `json:"prompt_version,omitempty"`

	SizeBytes int64          `json:"size_bytes"`
	Metadata  map[string]any `json:"metadata,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// Manifest is written into every artifact directory.
//
// It makes an artifact self-describing on disk: a user who copies a project
// folder, or who loses the database, can still tell what each artifact is,
// what produced it, and what it was derived from. The database is an index; the
// directory is the record.
type Manifest struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Stage     string `json:"stage"`

	InputHash  string `json:"input_hash"`
	ConfigHash string `json:"config_hash"`
	Key        string `json:"key"`

	Provider      string `json:"provider,omitempty"`
	Model         string `json:"model,omitempty"`
	PromptVersion string `json:"prompt_version,omitempty"`

	// Primary names the payload file a consumer should open first.
	Primary string `json:"primary"`

	// Metadata carries stage-specific facts: device actually used, token usage,
	// counts, timings. Nothing in the pipeline may *depend* on a metadata key —
	// that would be an undeclared interface between stages.
	Metadata map[string]any `json:"metadata,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// PrimaryOf reads the primary payload filename from an artifact's manifest.
func PrimaryOf(dir string) (string, error) {
	raw, err := readManifest(dir)
	if err != nil {
		return "", err
	}
	return raw.Primary, nil
}

func readManifest(dir string) (*Manifest, error) {
	raw, err := readFile(manifestPath(dir))
	if err != nil {
		return nil, err
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}
