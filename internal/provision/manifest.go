package provision

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/platform"
)

// Manifest records what was installed, and from where.
//
// It exists for one failure in particular. A virtual environment stores
// absolute paths — the interpreter it was built from, the directory of the
// package it installed in editable mode — so moving the extracted archive or
// the data directory afterwards leaves an environment that still exists and no
// longer works, with nothing on screen to say why. This is what turns that into
// a sentence naming the path that went away.
type Manifest struct {
	// V is the manifest format. A file from the future is not read.
	V int `json:"v"`

	AIDir         string `json:"ai_dir"`
	UV            string `json:"uv"`
	Python        string `json:"python"`
	PythonRequest string `json:"python_request,omitempty"`
	Extra         string `json:"extra,omitempty"`

	BinaryVersion string `json:"binary_version,omitempty"`
	CodeRevision  string `json:"code_revision,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// ManifestVersion is the format this build writes.
const ManifestVersion = 1

// writeManifest records the run, atomically.
//
// Temp file then rename, so that a manifest is either the previous one or the
// new one — a half-written file would be worse than none, because it would be
// read as fact.
func writeManifest(req Request) error {
	manifest := Manifest{
		V:             ManifestVersion,
		AIDir:         req.AIDir,
		UV:            req.UV,
		Python:        platform.RuntimeVenvPython(req.RuntimeDir),
		PythonRequest: req.Python,
		Extra:         req.Extra,
		BinaryVersion: req.BinaryVersion,
		CodeRevision:  req.CodeRevision,
		CreatedAt:     time.Now().UTC(),
	}

	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("provision: encode the manifest: %w", err)
	}

	final := platform.RuntimeManifestPath(req.RuntimeDir)
	temporary := final + ".tmp"

	if err := os.WriteFile(temporary, encoded, 0o644); err != nil {
		return fmt.Errorf("provision: write the manifest: %w", err)
	}
	if err := os.Rename(temporary, final); err != nil {
		return fmt.Errorf("provision: write the manifest: %w", err)
	}
	return nil
}

// ReadManifest reads the record of a previous run.
//
// A missing or unreadable manifest is reported as an error rather than an empty
// value: the caller's question is always "what is this environment and where
// did it come from", and answering "nothing" to that when the file exists but
// is corrupt would be a fabrication.
func ReadManifest(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("provision: read the manifest: %w", err)
	}
	if manifest.V > ManifestVersion {
		return nil, fmt.Errorf("provision: the manifest was written by a newer version (%d)", manifest.V)
	}
	return &manifest, nil
}
