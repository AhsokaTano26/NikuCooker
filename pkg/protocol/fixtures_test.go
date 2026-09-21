package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/manifest.json")

// TestFixturesRoundTrip is the contract this package exists to keep: every
// fixture decodes into the declared types and re-serialises to the same bytes.
//
// A field added on one side of the language boundary and not the other fails
// here, on the side that forgot, rather than surfacing to a user as a silently
// dropped value.
func TestFixturesRoundTrip(t *testing.T) {
	cases := []struct {
		name   string
		decode func(t *testing.T, raw []byte) any
	}{
		{"request_asr_transcribe.json", func(t *testing.T, raw []byte) any {
			return decodeRequest(t, raw)
		}},
		{"event_progress.json", func(t *testing.T, raw []byte) any {
			return decodeProgress(t, raw)
		}},
		{"event_error.json", func(t *testing.T, raw []byte) any {
			return decodeErrorEvent(t, raw)
		}},
		{"event_ready.json", func(t *testing.T, raw []byte) any {
			return decodeReady(t, raw)
		}},
		{"result_asr_transcribe_inline.json", func(t *testing.T, raw []byte) any {
			return decodeResult(t, raw)
		}},
		{"result_asr_transcribe_bypath.json", func(t *testing.T, raw []byte) any {
			return decodeResult(t, raw)
		}},
		{"result_vad_detect.json", func(t *testing.T, raw []byte) any {
			return decodeResult(t, raw)
		}},
		{"asr_result_full.json", func(t *testing.T, raw []byte) any {
			var r ASRResult
			if err := json.Unmarshal(raw, &r); err != nil {
				t.Fatalf("decode ASRResult: %v", err)
			}
			return r
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := mustFixture(t, tc.name)

			want := canonical(t, raw)
			got := canonical(t, marshal(t, tc.decode(t, raw)))

			if !bytes.Equal(want, got) {
				t.Errorf("round trip changed the message\n want: %s\n  got: %s", want, got)
			}
		})
	}
}

// TestResultPayloadDecodesIntoMethodType checks the inner payload as well as the
// envelope.
//
// TestFixturesRoundTrip only proves the envelope survives, because ResultEvent
// keeps Result as raw bytes. Without this, a method's result type could drift
// from the fixture describing it and nothing would notice.
func TestResultPayloadDecodesIntoMethodType(t *testing.T) {
	t.Run("transcribe inline", func(t *testing.T) {
		ev := decodeResult(t, mustFixture(t, "result_asr_transcribe_inline.json"))

		var res ASRTranscribeResult
		if err := json.Unmarshal(ev.Result, &res); err != nil {
			t.Fatalf("decode result: %v", err)
		}
		if res.ResultPath != "" {
			t.Errorf("inline result carries result_path %q", res.ResultPath)
		}
		if len(res.SegmentsInline) == 0 {
			t.Fatal("inline result has no segments")
		}
		if res.SegmentCount != len(res.SegmentsInline) {
			t.Errorf("segment_count %d does not match %d inline segments",
				res.SegmentCount, len(res.SegmentsInline))
		}
		// Word timings must fall inside their parent segment, or segmentation
		// downstream produces impossible subtitle boundaries.
		for _, seg := range res.SegmentsInline {
			for _, w := range seg.Words {
				if w.Start < seg.Start || w.End > seg.End {
					t.Errorf("word %q [%v,%v] outside segment [%v,%v]",
						w.Text, w.Start, w.End, seg.Start, seg.End)
				}
			}
		}
	})

	t.Run("transcribe by path", func(t *testing.T) {
		ev := decodeResult(t, mustFixture(t, "result_asr_transcribe_bypath.json"))

		var res ASRTranscribeResult
		if err := json.Unmarshal(ev.Result, &res); err != nil {
			t.Fatalf("decode result: %v", err)
		}
		if res.ResultPath == "" {
			t.Error("by-path result has no result_path")
		}
		if res.SegmentsInline != nil {
			t.Error("by-path result carries inline segments")
		}
	})

	t.Run("vad detect", func(t *testing.T) {
		ev := decodeResult(t, mustFixture(t, "result_vad_detect.json"))

		var res VADResult
		if err := json.Unmarshal(ev.Result, &res); err != nil {
			t.Fatalf("decode result: %v", err)
		}
		prev := 0.0
		for i, r := range res.Regions {
			if r.End <= r.Start {
				t.Errorf("region %d has non-positive duration: [%v,%v]", i, r.Start, r.End)
			}
			if r.Start < prev {
				t.Errorf("region %d starts at %v, before previous end %v", i, r.Start, prev)
			}
			prev = r.End
		}
	})
}

// TestManifestMatchesFixtures keeps the published digest in step with the files
// it describes. Regenerate with:
//
//	go test ./pkg/protocol -run TestManifest -update
func TestManifestMatchesFixtures(t *testing.T) {
	digest, err := ComputeSchemaDigest()
	if err != nil {
		t.Fatalf("compute digest: %v", err)
	}
	names, err := fixtureNames()
	if err != nil {
		t.Fatalf("list fixtures: %v", err)
	}

	if *update {
		writeManifest(t, Manifest{SchemaDigest: digest, Files: names})
		t.Logf("manifest updated: %s", digest)
		return
	}

	var m Manifest
	if err := json.Unmarshal(mustFixture(t, manifestName), &m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if m.SchemaDigest != digest {
		t.Fatalf("manifest is stale\n manifest: %s\n fixtures: %s\nregenerate with: go test ./pkg/protocol -run TestManifest -update",
			m.SchemaDigest, digest)
	}
	if strings.Join(m.Files, ",") != strings.Join(names, ",") {
		t.Errorf("manifest file list is stale\nmanifest: %v\nfixtures: %v", m.Files, names)
	}
}

// ---------------------------------------------------------------------------
// Framing
// ---------------------------------------------------------------------------

// TestReaderFraming covers the framing rules that fail silently when wrong.
func TestReaderFraming(t *testing.T) {
	t.Run("ignores blank lines", func(t *testing.T) {
		r := NewReader(strings.NewReader("\n\n{\"a\":1}\n\n{\"b\":2}\n"))
		for _, want := range []string{`{"a":1}`, `{"b":2}`} {
			got, err := r.ReadLine()
			if err != nil {
				t.Fatalf("ReadLine: %v", err)
			}
			if string(got) != want {
				t.Errorf("got %q, want %q", got, want)
			}
		}
	})

	t.Run("strips CRLF", func(t *testing.T) {
		// A worker on Windows writes the same bytes plus a carriage return; the
		// core must not see it, or every string field would carry one.
		r := NewReader(strings.NewReader("{\"a\":1}\r\n"))
		got, err := r.ReadLine()
		if err != nil {
			t.Fatalf("ReadLine: %v", err)
		}
		if string(got) != `{"a":1}` {
			t.Errorf("got %q, want %q", got, `{"a":1}`)
		}
	})

	t.Run("accepts a final line without a terminator", func(t *testing.T) {
		r := NewReader(strings.NewReader(`{"a":1}`))
		got, err := r.ReadLine()
		if err != nil {
			t.Fatalf("ReadLine: %v", err)
		}
		if string(got) != `{"a":1}` {
			t.Errorf("got %q", got)
		}
	})

	t.Run("rejects an oversized line instead of truncating", func(t *testing.T) {
		// The failure this guards against is a default bufio.Scanner, which
		// stops at 64 KiB with no error at all. A silently truncated ASR result
		// is far worse than a refused one.
		var sb strings.Builder
		sb.WriteString(`{"data":"`)
		sb.WriteString(strings.Repeat("x", MaxLineBytes+1))
		sb.WriteString(`"}`)

		r := NewReader(strings.NewReader(sb.String()))
		if _, err := r.ReadLine(); !errors.Is(err, ErrLineTooLong) {
			t.Fatalf("got %v, want ErrLineTooLong", err)
		}
	})

	t.Run("reads a line larger than the internal buffer", func(t *testing.T) {
		// Proves the accumulation path works, not just the rejection path: a
		// payload comfortably over bufio's 64 KiB chunk must arrive intact.
		payload := strings.Repeat("あ", 100_000)
		msg, err := json.Marshal(map[string]string{"text": payload})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		r := NewReader(bytes.NewReader(append(msg, '\n')))
		got, err := r.ReadLine()
		if err != nil {
			t.Fatalf("ReadLine: %v", err)
		}
		if !bytes.Equal(got, msg) {
			t.Fatalf("round trip corrupted a %d-byte message", len(msg))
		}
	})
}

// TestWriterEmitsOneLinePerMessage guards the invariant the reader depends on.
func TestWriterEmitsOneLinePerMessage(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	// A message whose string content contains newlines must still occupy one
	// line: JSON escaping handles it and framing stays intact.
	msgs := []any{
		Request{V: Version, ID: "req_1", Method: MethodEcho,
			Params: map[string]string{"text": "line one\nline two\r\nline three"}},
		ProgressEvent{V: Version, ID: "req_1", Type: TypeProgress, Progress: 0.5},
	}

	for _, m := range msgs {
		if err := w.WriteMessage(m); err != nil {
			t.Fatalf("WriteMessage: %v", err)
		}
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != len(msgs) {
		t.Fatalf("wrote %d lines for %d messages", len(lines), len(msgs))
	}
	for i, line := range lines {
		if !json.Valid([]byte(line)) {
			t.Errorf("line %d is not valid JSON: %s", i, line)
		}
	}
}

// ---------------------------------------------------------------------------
// Decode rejection
// ---------------------------------------------------------------------------

// TestDecodeRejections checks that malformed input fails loudly.
//
// Every case here would otherwise decode into a zero-valued struct, which
// presents to the user as a mysterious empty transcript rather than as a
// protocol error.
func TestDecodeRejections(t *testing.T) {
	cases := []struct {
		name string
		line string
		want error
	}{
		{"version mismatch", `{"v":2,"id":"r","type":"progress","progress":0.1}`, ErrVersionMismatch},
		{"unknown event type", `{"v":1,"id":"r","type":"telemetry"}`, nil},
		{"malformed json", `{"v":1,"id":`, nil},
		{"progress is not a number", `{"v":1,"id":"r","type":"progress","progress":"half"}`, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeEvent([]byte(tc.line))
			if err == nil {
				t.Fatal("expected an error, got none")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
		})
	}

	t.Run("version mismatch on request", func(t *testing.T) {
		if _, err := DecodeRequest([]byte(`{"v":99,"id":"r","method":"echo"}`)); !errors.Is(err, ErrVersionMismatch) {
			t.Errorf("got %v, want ErrVersionMismatch", err)
		}
	})

	t.Run("request without id or method", func(t *testing.T) {
		for _, line := range []string{
			`{"v":1,"method":"echo"}`,
			`{"v":1,"id":"req_1"}`,
		} {
			if _, err := DecodeRequest([]byte(line)); err == nil {
				t.Errorf("accepted %s", line)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Canonical form
// ---------------------------------------------------------------------------

// TestCanonicalizeJSON covers the properties the digest and the artifact cache
// key both depend on.
func TestCanonicalizeJSON(t *testing.T) {
	t.Run("sorts keys and drops whitespace", func(t *testing.T) {
		a := canonical(t, []byte("{\n  \"b\": 1,\n  \"a\": {\"z\": 2, \"y\": 3}\n}"))
		b := canonical(t, []byte(`{"a":{"y":3,"z":2},"b":1}`))
		if !bytes.Equal(a, b) {
			t.Errorf("equivalent documents canonicalised differently:\n%s\n%s", a, b)
		}
	})

	t.Run("preserves number literals", func(t *testing.T) {
		// Routing numbers through float64 would rewrite 1.10 as 1.1 and lose
		// precision on large integers, which would make a cache key unstable.
		got := canonical(t, []byte(`{"a":1.10,"b":9007199254740993}`))
		want := `{"a":1.10,"b":9007199254740993}`
		if string(got) != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})

	t.Run("rejects trailing data", func(t *testing.T) {
		if _, err := CanonicalizeJSON([]byte(`{"a":1}{"b":2}`)); err == nil {
			t.Error("accepted two concatenated documents as one")
		}
	})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func mustFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := Fixture(name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return raw
}

func decodeRequest(t *testing.T, raw []byte) *Request {
	t.Helper()
	req, err := DecodeRequest(raw)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	return req
}

func decodeProgress(t *testing.T, raw []byte) *ProgressEvent {
	t.Helper()
	ev := decodeAny(t, raw)
	p, ok := ev.(*ProgressEvent)
	if !ok {
		t.Fatalf("decoded into %T, want *ProgressEvent", ev)
	}
	return p
}

func decodeResult(t *testing.T, raw []byte) *ResultEvent {
	t.Helper()
	ev := decodeAny(t, raw)
	r, ok := ev.(*ResultEvent)
	if !ok {
		t.Fatalf("decoded into %T, want *ResultEvent", ev)
	}
	return r
}

func decodeErrorEvent(t *testing.T, raw []byte) *ErrorEvent {
	t.Helper()
	ev := decodeAny(t, raw)
	e, ok := ev.(*ErrorEvent)
	if !ok {
		t.Fatalf("decoded into %T, want *ErrorEvent", ev)
	}
	return e
}

func decodeReady(t *testing.T, raw []byte) *ReadyEvent {
	t.Helper()
	ev := decodeAny(t, raw)
	r, ok := ev.(*ReadyEvent)
	if !ok {
		t.Fatalf("decoded into %T, want *ReadyEvent", ev)
	}
	return r
}

func decodeAny(t *testing.T, raw []byte) Event {
	t.Helper()
	ev, err := DecodeEvent(raw)
	if err != nil {
		t.Fatalf("DecodeEvent: %v", err)
	}
	return ev
}

func marshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func canonical(t *testing.T, raw []byte) []byte {
	t.Helper()
	out, err := CanonicalizeJSON(raw)
	if err != nil {
		t.Fatalf("canonicalise %s: %v", raw, err)
	}
	return out
}

func writeManifest(t *testing.T, m Manifest) {
	t.Helper()
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	raw = append(raw, '\n')

	path := filepath.Join("testdata", manifestName)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}
