"""Protocol conformance tests, mirroring pkg/protocol/fixtures_test.go.

Both suites decode the same fixtures and compare against the canonical form of
the original. A field added on one side and not the other fails on the side that
forgot — before a user sees a silently dropped value.

These tests need only pydantic. They never touch a model, a GPU or the network,
which is what keeps them running in CI on all three platforms.
"""

from __future__ import annotations

import io
import json

import pytest
from nikucooker_ai.protocol.codec import (
    MAX_LINE_BYTES,
    ProtocolDecodeError,
    ProtocolLineTooLong,
    ProtocolReader,
    ProtocolWriter,
    canonical_json,
    decode_event,
    decode_request,
    dump_wire,
)
from nikucooker_ai.protocol.errors import ErrorCode, ProtocolError
from nikucooker_ai.protocol.models import (
    ASRResult,
    ASRTranscribeResult,
    ErrorEvent,
    ProgressEvent,
    ReadyEvent,
    Request,
    ResultEvent,
    VADResult,
)
from nikucooker_ai.protocol.schema import MANIFEST_NAME, compute_schema_digest, schema_digest

# Aliased: pytest would otherwise collect a function named test*_ as a test.
from nikucooker_ai.protocol.schema import testdata_dir as fixtures_dir
from pydantic import ValidationError

# Fixtures live in the Go module and are read across the boundary rather than
# duplicated. Absent means the suite is running from an installed wheel, which is
# not a supported way to run tests.
pytestmark = pytest.mark.skipif(
    fixtures_dir() is None,
    reason="protocol fixtures not reachable; run from a source checkout",
)


def fixture(name: str) -> bytes:
    directory = fixtures_dir()
    assert directory is not None
    return (directory / name).read_bytes()


def canonical_of(raw: bytes) -> bytes:
    return canonical_json(json.loads(raw))


#: fixture name -> model it must decode into.
EVENT_FIXTURES: dict[str, type] = {
    "event_progress.json": ProgressEvent,
    "event_error.json": ErrorEvent,
    "event_ready.json": ReadyEvent,
    "result_asr_transcribe_inline.json": ResultEvent,
    "result_asr_transcribe_bypath.json": ResultEvent,
    "result_vad_detect.json": ResultEvent,
}


@pytest.mark.parametrize("name", sorted(EVENT_FIXTURES))
def test_event_fixture_round_trips(name: str) -> None:
    """Every event fixture decodes and re-serialises to the same bytes."""
    raw = fixture(name)

    event = decode_event(raw)
    assert isinstance(event, EVENT_FIXTURES[name]), f"decoded into {type(event).__name__}"

    got = canonical_json(dump_wire(event))
    assert got == canonical_of(raw), "round trip changed the message"


def test_request_fixture_round_trips() -> None:
    raw = fixture("request_asr_transcribe.json")

    request = decode_request(raw)
    got = canonical_json(dump_wire(request))

    assert got == canonical_of(raw), "round trip changed the message"


def test_asr_result_fixture_round_trips() -> None:
    raw = fixture("asr_result_full.json")

    result = ASRResult.model_validate_json(raw)
    got = canonical_json(dump_wire(result))

    assert got == canonical_of(raw), "round trip changed the message"


def test_result_payload_decodes_into_method_type() -> None:
    """The inner payload is checked too, not just the envelope.

    The envelope test only proves ``result`` survives as an opaque dict; without
    this, a method's result type could drift from the fixture describing it.
    """
    inline = decode_event(fixture("result_asr_transcribe_inline.json"))
    assert isinstance(inline, ResultEvent)
    res = ASRTranscribeResult.model_validate(inline.result)

    assert res.result_path is None, "inline result carries result_path"
    assert res.segments, "inline result has no segments"
    assert res.segment_count == len(res.segments)

    # Word timings must fall inside their parent segment, or segmentation
    # downstream produces impossible subtitle boundaries.
    for segment in res.segments:
        for word in segment.words:
            assert segment.start <= word.start, f"word {word.text!r} starts before its segment"
            assert word.end <= segment.end, f"word {word.text!r} ends after its segment"

    by_path = decode_event(fixture("result_asr_transcribe_bypath.json"))
    assert isinstance(by_path, ResultEvent)
    res_path = ASRTranscribeResult.model_validate(by_path.result)

    assert res_path.result_path, "by-path result has no result_path"
    assert not res_path.segments, "by-path result carries inline segments"


def test_vad_regions_are_ordered_and_non_overlapping() -> None:
    event = decode_event(fixture("result_vad_detect.json"))
    assert isinstance(event, ResultEvent)
    result = VADResult.model_validate(event.result)

    previous_end = 0.0
    for region in result.regions:
        assert region.end > region.start, f"non-positive duration: {region}"
        assert region.start >= previous_end, f"region {region} overlaps its predecessor"
        previous_end = region.end


# ---------------------------------------------------------------------------
# Digest agreement
# ---------------------------------------------------------------------------


def test_digest_matches_manifest() -> None:
    """The computed digest equals the published one.

    Go computes the same value from the same files. A mismatch here means the two
    sides would refuse each other's handshake.
    """
    manifest = json.loads(fixture(MANIFEST_NAME))
    assert compute_schema_digest() == manifest["schema_digest"]


def test_digest_is_cached_but_recomputable() -> None:
    assert schema_digest() == compute_schema_digest()


# ---------------------------------------------------------------------------
# Canonical form
# ---------------------------------------------------------------------------


def test_canonical_json_sorts_keys_and_drops_whitespace() -> None:
    a = canonical_json(json.loads('{\n  "b": 1,\n  "a": {"z": 2, "y": 3}\n}'))
    b = canonical_json(json.loads('{"a":{"y":3,"z":2},"b":1}'))
    assert a == b


def test_canonical_json_preserves_non_ascii() -> None:
    # ensure_ascii=True, the json.dumps default, would escape this and produce a
    # different digest from Go on identical input.
    assert canonical_json({"text": "こんにちは"}) == '{"text":"こんにちは"}'.encode()


def test_fixtures_avoid_encoder_divergent_characters() -> None:
    """Keeps fixtures inside the JSON subset Go and Python encode identically.

    encoding/json escapes U+2028 and U+2029 unconditionally for JSONP safety and
    Python does not, so a fixture containing one would yield two different
    digests from one file set.
    """
    divergent = ("<", ">", "&", "\u2028", "\u2029")

    directory = fixtures_dir()
    assert directory is not None
    for path in sorted(directory.glob("*.json")):
        text = path.read_text(encoding="utf-8")
        for char in divergent:
            assert char not in text, (
                f"{path.name} contains {char!r} (U+{ord(char):04X}), "
                "which Go and Python encoders escape differently"
            )


# ---------------------------------------------------------------------------
# Framing
# ---------------------------------------------------------------------------


def test_reader_skips_blank_lines_and_strips_crlf() -> None:
    stream = io.BytesIO(b'\n\n{"a":1}\r\n\n{"b":2}\n')
    reader = ProtocolReader(stream)

    assert reader.read_line() == b'{"a":1}'
    assert reader.read_line() == b'{"b":2}'
    assert reader.read_line() is None


def test_reader_returns_none_at_end_of_stream() -> None:
    assert ProtocolReader(io.BytesIO(b"")).read_line() is None


def test_reader_rejects_oversized_line() -> None:
    # Streamed rather than built in memory: the point is that the reader stops,
    # and it must do so without buffering the whole thing first.
    stream = io.BytesIO(b"x" * (MAX_LINE_BYTES + 2))
    with pytest.raises(ProtocolLineTooLong):
        ProtocolReader(stream).read_line()


def test_reader_handles_a_line_larger_than_a_read_chunk() -> None:
    payload = json.dumps({"text": "あ" * 100_000}).encode()
    line = ProtocolReader(io.BytesIO(payload + b"\n")).read_line()
    assert line == payload


def test_writer_emits_one_line_per_message() -> None:
    stream = io.BytesIO()
    writer = ProtocolWriter(stream)

    writer.write(Request(id="req_1", method="echo", params={"text": "a\nb\r\nc"}))
    writer.write(ProgressEvent(id="req_1", progress=0.5))

    lines = stream.getvalue().split(b"\n")
    assert lines[-1] == b"", "output must end with a terminator"
    assert len(lines) == 3, f"wrote {len(lines) - 1} lines for 2 messages"

    for line in lines[:-1]:
        json.loads(line)  # escapes the newlines; framing stays intact


def test_writer_refuses_non_events() -> None:
    writer = ProtocolWriter(io.BytesIO())
    with pytest.raises(ValueError, match="not an event"):
        writer.write_event(Request(id="req_1", method="echo"))


def test_writer_round_trips_through_reader() -> None:
    stream = io.BytesIO()
    ProtocolWriter(stream).write_error(
        "req_1", ProtocolError.make(ErrorCode.CANCELLED, "cancelled by the core")
    )

    stream.seek(0)
    event = decode_event(stream.readline())
    assert isinstance(event, ErrorEvent)
    assert event.id == "req_1"
    assert event.error.code is ErrorCode.CANCELLED
    assert event.error.retryable is False, "CANCELLED must not be retryable"


# ---------------------------------------------------------------------------
# Decode rejection
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("line", "match"),
    [
        (b'{"v":2,"id":"r","type":"progress","progress":0.1}', "protocol version"),
        (b'{"v":1,"id":"r","type":"telemetry"}', "unknown event type"),
        (b'{"v":1,"id":', "malformed"),
        (b'{"v":1,"id":"r","type":"progress","progress":"half"}', "malformed"),
        (b"[1,2,3]", "not a JSON object"),
    ],
)
def test_decode_event_rejects_bad_input(line: bytes, match: str) -> None:
    with pytest.raises(ProtocolDecodeError, match=match):
        decode_event(line)


def test_decode_request_rejects_bad_input() -> None:
    with pytest.raises(ProtocolDecodeError, match="protocol version"):
        decode_request(b'{"v":99,"id":"r","method":"echo"}')

    with pytest.raises(ProtocolDecodeError, match="malformed"):
        decode_request(b'{"v":1,"method":"echo"}')


def test_decode_rejects_unknown_fields() -> None:
    """extra="forbid" is what stops a renamed field from being silently dropped.

    Go ignores unknown JSON fields, so a rename on one side and not the other
    would be invisible there and fail here — on the side that is wrong.
    """
    with pytest.raises(ProtocolDecodeError):
        decode_request(b'{"v":1,"id":"r","method":"echo","methdo":"typo"}')


def test_error_retryable_defaults_from_code() -> None:
    # Deriving the default from the code means a call site cannot mark a
    # transient failure permanent by omitting the argument.
    assert ProtocolError.make(ErrorCode.OUT_OF_MEMORY, "oom").retryable is True
    assert ProtocolError.make(ErrorCode.INVALID_PARAMS, "bad").retryable is False
    # ...and an explicit value still wins.
    assert ProtocolError.make(ErrorCode.INVALID_PARAMS, "bad", retryable=True).retryable is True


def test_extra_forbid_is_inherited_by_wire_models() -> None:
    with pytest.raises(ValidationError):
        ProgressEvent.model_validate({"id": "r", "progress": 0.5, "unexpected": 1})
