// Package artifact implements the stage cache.
//
// A stage's output is identified by the hash of its inputs combined with the
// hash of its configuration, so identical work is never repeated and a change
// invalidates its own stage and everything downstream of it, and nothing
// upstream. See docs/artifact-cache.md.
package artifact

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/AhsokaTano26/NikuCooker/pkg/protocol"
)

// IDFPrefix is prepended to an artifact's content address to form its ID.
const IDFPrefix = "art_"

// IDFLength is how many hex characters of the key an artifact ID carries.
//
// Sixteen hex characters is 64 bits. The full key is still stored and indexed;
// the short form exists so a directory name stays readable and a log line stays
// scannable, and the collision probability across a single user's projects is
// not a practical concern.
const IDFLength = 16

// separator delimits the components fed into a hash.
//
// A byte that cannot appear in a JSON document, a stage name, or a hex digest,
// so two different component lists cannot concatenate into the same byte
// string.
const separator = "\x1f"

// KeyInputs is everything that determines a stage's output identity.
type KeyInputs struct {
	// Stage is the stage name, e.g. "translation".
	Stage string

	// StageVersion is the stage implementation's own version, bumped by hand
	// when its algorithm changes. This is the intentional invalidation lever.
	StageVersion string

	// CodeRevision is the build's VCS revision. It is the safety net for
	// forgetting to bump StageVersion, turning that mistake into a slow rebuild
	// rather than into wrong output.
	CodeRevision string

	// Config is the stage's configuration subtree — not the whole config.
	// Changing render.crf must not invalidate the ASR artifact, so each stage
	// declares which subtree it reads and only that subtree is hashed.
	Config any

	// Fingerprint captures stage-specific external inputs that are neither
	// configuration nor an upstream artifact: the context document and glossary
	// for translation, the QC rule set version, and so on.
	Fingerprint map[string]string

	// UpstreamKeys maps a producer stage name to the content key of the
	// artifact this stage consumes.
	//
	// Upstream *keys* rather than upstream *contents*: hashing a 200 MB
	// audio.wav would cost more than re-running the stage, and the upstream key
	// already captures that content transitively.
	UpstreamKeys map[string]string

	// SourceFingerprint identifies the source media, for stages that read it.
	// Empty for stages that do not.
	SourceFingerprint string
}

// write adds parts to a hash, in order.
//
// The return is dropped here and nowhere else. hash.Hash cannot fail to accept
// bytes — its embedded io.Writer is documented as always succeeding — and the
// alternative is an ignored error at every one of the twenty call sites below,
// which is how an ignored error stops meaning anything.
func write(h hash.Hash, parts ...string) {
	for _, part := range parts {
		_, _ = io.WriteString(h, part)
	}
}

// DeriveKey computes the artifact's content address.
//
//	ConfigHash = sha256(canonical(config) ‖ stage_version ‖ code_revision ‖ fingerprint)
//	InputHash  = sha256(sorted(producer:key) ‖ source_fingerprint)
//	Key        = sha256(InputHash ‖ ConfigHash)
func DeriveKey(in KeyInputs) (key, inputHash, configHash string, err error) {
	configHash, err = deriveConfigHash(in)
	if err != nil {
		return "", "", "", err
	}
	inputHash = deriveInputHash(in)

	// The order is the one the doc comment above specifies.
	full := sha256.New()
	write(full, inputHash, separator, configHash)

	return hex.EncodeToString(full.Sum(nil)), inputHash, configHash, nil
}

func deriveConfigHash(in KeyInputs) (string, error) {
	// Canonical form matters here beyond tidiness: without it the same
	// configuration serialised two ways hashes differently, and the cache
	// misses at random depending on map iteration order.
	canonical, err := protocol.CanonicalJSON(in.Config)
	if err != nil {
		return "", fmt.Errorf("artifact: canonicalise config for stage %q: %w", in.Stage, err)
	}

	h := sha256.New()
	h.Write(canonical)
	write(h, separator, in.Stage, separator, in.StageVersion, separator,
		in.CodeRevision, separator, fingerprintString(in.Fingerprint))

	return hex.EncodeToString(h.Sum(nil)), nil
}

func deriveInputHash(in KeyInputs) string {
	h := sha256.New()

	// Sorted so that map iteration order cannot change the hash. The upstream
	// keys are themselves content-derived, so this transitively captures the
	// content of every input without reading any of it.
	names := make([]string, 0, len(in.UpstreamKeys))
	for name := range in.UpstreamKeys {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		write(h, name, ":", in.UpstreamKeys[name], separator)
	}
	write(h, separator, in.SourceFingerprint)

	return hex.EncodeToString(h.Sum(nil))
}

// fingerprintString renders the stage fingerprint in a stable order.
func fingerprintString(fingerprint map[string]string) string {
	if len(fingerprint) == 0 {
		return ""
	}
	keys := make([]string, 0, len(fingerprint))
	for key := range fingerprint {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, key := range keys {
		b.WriteString(key)
		b.WriteString("=")
		b.WriteString(fingerprint[key])
		b.WriteString(separator)
	}
	return b.String()
}

// IDForKey renders the short identifier an artifact directory is named after.
func IDForKey(key string) string {
	if len(key) < IDFLength {
		return IDFPrefix + key
	}
	return IDFPrefix + key[:IDFLength]
}

// ---------------------------------------------------------------------------
// Source fingerprint
// ---------------------------------------------------------------------------

// sampleSize is how many bytes are read from each sampled offset.
const sampleSize = 1 << 20 // 1 MiB

// FingerprintSource identifies a media file without reading all of it.
//
// Hashing a multi-gigabyte video on every plan would be absurd, but hashing
// only size and mtime breaks when a file is copied or touched. Three sampled
// offsets — first, middle, last — plus the size catch every edit that changes
// the beginning or the end, and edits buried in the middle with high
// probability. Full is available for users who want certainty and will pay
// O(file size) for it.
func FingerprintSource(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("artifact: fingerprint %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("artifact: fingerprint %s: %w", path, err)
	}
	size := info.Size()

	h := sha256.New()

	if size <= 3*sampleSize {
		// Small enough that sampling would read everything anyway, and reading
		// it once is both simpler and stronger.
		if _, err := io.Copy(h, file); err != nil {
			return "", fmt.Errorf("artifact: fingerprint %s: %w", path, err)
		}
	} else {
		offsets := []int64{0, size/2 - sampleSize/2, size - sampleSize}
		buf := make([]byte, sampleSize)
		for _, offset := range offsets {
			if _, err := file.ReadAt(buf, offset); err != nil {
				return "", fmt.Errorf("artifact: fingerprint %s at offset %d: %w", path, offset, err)
			}
			h.Write(buf)
		}
	}

	// The size is mixed in explicitly: two files could otherwise share all
	// three samples and differ only in the unsampled region between them.
	var sizeBytes [8]byte
	binary.LittleEndian.PutUint64(sizeBytes[:], uint64(size))
	h.Write(sizeBytes[:])

	return fmt.Sprintf("v1:size=%d:h=%s", size, hex.EncodeToString(h.Sum(nil))), nil
}

// FingerprintSourceFull hashes the entire file.
//
// Offered as artifact.hash_mode: full for users who want certainty about a
// source that may have been edited in place.
func FingerprintSourceFull(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("artifact: fingerprint %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("artifact: fingerprint %s: %w", path, err)
	}

	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", fmt.Errorf("artifact: fingerprint %s: %w", path, err)
	}
	return fmt.Sprintf("v1:full:size=%d:h=%s", info.Size(), hex.EncodeToString(h.Sum(nil))), nil
}

// FingerprintFile identifies a small file by its full contents.
//
// Used for prompt files, ASS presets and the context document — all small
// enough that full hashing is cheaper than reasoning about sampling.
func FingerprintFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("artifact: fingerprint %s: %w", path, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// FingerprintBytes identifies a value by its canonical JSON encoding.
func FingerprintBytes(label string, value any) (string, error) {
	canonical, err := protocol.CanonicalJSON(value)
	if err != nil {
		return "", fmt.Errorf("artifact: fingerprint %s: %w", label, err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}
