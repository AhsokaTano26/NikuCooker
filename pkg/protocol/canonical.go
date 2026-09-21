package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Canonical JSON.
//
// Two documents that mean the same thing must hash the same, or a cache keyed
// on them misses for no reason. That requirement shows up in three places: the
// artifact config hash (docs/artifact-cache.md §2.1), the schema digest below,
// and the fixture comparison both language test suites perform.
//
// The canonical form is: object keys sorted, no insignificant whitespace, and
// numbers preserved exactly as written. Numbers are preserved rather than
// reformatted because reformatting is where a float 0.1 becomes 0.100000000001
// on one side and not the other.

// CanonicalizeJSON rewrites a JSON document into canonical form.
func CanonicalizeJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	// UseNumber keeps numeric literals as written instead of routing them
	// through float64, which would lose precision for large integers and
	// reformat everything else.
	dec.UseNumber()

	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("protocol: canonicalise: %w", err)
	}
	// Trailing content means this is not a single document. Accepting it would
	// let a truncated concatenation of two documents pass as the first one.
	if dec.More() {
		return nil, errors.New("protocol: trailing data after JSON document")
	}

	// An Encoder rather than json.Marshal: Marshal escapes <, > and & to <
	// and friends for safe embedding in HTML, and Python's json.dumps does not.
	// Left alone, that difference alone would make the two sides compute
	// different schema digests from identical input.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("protocol: canonicalise: %w", err)
	}

	// Encode appends a newline; the canonical form is a single line.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// encoderDivergentRunes are characters the Go and Python JSON writers cannot be
// made to agree on.
//
// SetEscapeHTML(false) closes the gap for <, > and &, but encoding/json escapes
// U+2028 and U+2029 unconditionally for JSONP safety, and Python does not.
// Rather than carry a hand-written encoder on both sides to normalise two
// characters that have no business in subtitle text, the fixtures are required
// to avoid them — checked by a test, not left to a comment.
var encoderDivergentRunes = []rune{'<', '>', '&', '\u2028', '\u2029'}

// CanonicalJSON marshals v and returns it in canonical form.
//
// Marshalling through a struct loses field order relative to a map, which is
// the point: a Go struct and a Python dict declaring the same fields must
// produce byte-identical canonical output.
func CanonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("protocol: marshal: %w", err)
	}
	return CanonicalizeJSON(raw)
}
