# Protocol golden fixtures

These files define the Go ↔ Python wire format. They are read by **both** test
suites:

- Go: `pkg/protocol/fixtures_test.go`
- Python: `ai/tests/test_protocol.py`

Each side decodes every fixture into its own types, re-serialises, and compares
the result against the canonical form of the original. A field added on one side
and not the other therefore fails a test in the *other* language's suite, before
a user ever sees a deserialisation error. See `docs/ipc-protocol.md §8`.

## Files

| Fixture | Message |
|---------|---------|
| `request_asr_transcribe.json` | Go → worker, `asr.transcribe` |
| `event_progress.json` | worker → Go, `progress` |
| `event_error.json` | worker → Go, `error` |
| `event_ready.json` | worker → Go, `ready` handshake |
| `result_asr_transcribe_inline.json` | worker → Go, `result` with segments inline |
| `result_asr_transcribe_bypath.json` | worker → Go, `result` referencing a result file |
| `result_vad_detect.json` | worker → Go, `result` for `vad.detect` |
| `asr_result_full.json` | the `ASRResult` payload written to `result_path` |

The envelope fixtures are stored as single lines, because that is what they are:
one message is exactly one line of JSON. `asr_result_full.json` is a file
payload rather than a wire line, so it is indented for readability.

## The manifest

`manifest.json` records the digest of every file here except itself. It is
generated, never hand-edited:

```
make protocol-manifest
```

`TestManifestMatchesFixtures` fails if it drifts, and the same test regenerates
it with `-update`.

## Number literals must be in shortest round-trip form

Write `2.1`, not `2.10`. Write `1423.5`, not `1423.50`.

`TestFixturesRoundTrip` decodes each fixture into typed Go structs and
re-serialises, then compares against the canonical form of the original. A
`float64` marshals back in the shortest form that round-trips, so a literal
written as `12.10` comes back as `12.1` and the comparison fails — even though
the two are the same number.

The rule exists because the alternative is worse. Preserving literals exactly
would mean routing numbers through a decimal type on both sides, and the
canonical form's whole job is to make two documents that *mean* the same thing
hash the same. See `TestCanonicalizeJSON`, which asserts that `1.10` and
`9007199254740993` survive canonicalisation untouched.

This only constrains how a fixture is *written*. Numbers arriving over the wire
are unaffected: the digest is computed over these files, which are static, and
both sides canonicalise the same bytes.

## Placeholder values

Two fields deliberately hold values that are not real, and tests assert only
their shape:

- **`schema_digest`** in `event_ready.json` is a placeholder. The real value is
  derived from these fixtures, so a fixture containing the true digest of itself
  would be circular. The handshake logic that compares digests is tested
  separately, in Go.
- **`python`** and `platform` in the same fixture describe the machine that
  produced them. They are illustrative; no test depends on their values.
