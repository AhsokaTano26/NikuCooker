package protocol

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"sync"
)

// Fixtures holds the golden JSON documents that define the wire format.
//
// Both language test suites read them, so a field added on one side and not the
// other fails a test in the other language's suite rather than surfacing as a
// user-visible deserialisation error.
//
//go:embed testdata/*.json
var Fixtures embed.FS

const (
	fixtureDir = "testdata"
	// manifestName is excluded from the digest it records. Including it would
	// be circular.
	manifestName = "manifest.json"
)

// Manifest records the digest of the fixture set.
type Manifest struct {
	SchemaDigest string   `json:"schema_digest"`
	Files        []string `json:"files"`
}

var (
	digestOnce sync.Once
	digestVal  string
	digestErr  error
)

// SchemaDigest returns the digest of the fixture set, computing it once.
//
// The worker reports this value in its ready handshake and the core compares it
// against its own. A mismatch means the two sides disagree about data shapes,
// and there is no compatibility mode: guessing is worse than stopping.
func SchemaDigest() (string, error) {
	digestOnce.Do(func() { digestVal, digestErr = ComputeSchemaDigest() })
	return digestVal, digestErr
}

// ComputeSchemaDigest derives the digest from the fixtures as embedded.
//
// It is computed rather than read from the manifest so that it cannot drift
// from the files it describes. The manifest is the published copy; this is the
// source.
func ComputeSchemaDigest() (string, error) {
	names, err := fixtureNames()
	if err != nil {
		return "", err
	}

	h := sha256.New()
	for _, name := range names {
		raw, err := Fixtures.ReadFile(path.Join(fixtureDir, name))
		if err != nil {
			return "", fmt.Errorf("protocol: read fixture %s: %w", name, err)
		}
		// Canonicalise first: a fixture reformatted by an editor must not
		// change the digest, or every whitespace-only edit would look like a
		// wire-format change.
		canon, err := CanonicalizeJSON(raw)
		if err != nil {
			return "", fmt.Errorf("protocol: fixture %s: %w", name, err)
		}

		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write(canon)
		h.Write([]byte{0})
	}

	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// fixtureNames lists the fixture base names, sorted, excluding the manifest.
func fixtureNames() ([]string, error) {
	entries, err := fs.ReadDir(Fixtures, fixtureDir)
	if err != nil {
		return nil, fmt.Errorf("protocol: read fixture dir: %w", err)
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() || e.Name() == manifestName {
			continue
		}
		if path.Ext(e.Name()) != ".json" {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("protocol: no fixtures found in %s", fixtureDir)
	}

	sort.Strings(names)
	return names, nil
}

// Fixture reads one fixture by base name.
func Fixture(name string) ([]byte, error) {
	return Fixtures.ReadFile(path.Join(fixtureDir, name))
}
