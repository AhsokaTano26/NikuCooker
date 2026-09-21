"""Newline-delimited JSON framing, mirroring pkg/protocol/codec.go.

One message is exactly one line of UTF-8 JSON. Go writes requests on the
worker's stdin; the worker writes events on a private handle to the real stdout
(see ``nikucooker_ai.__main__``), never through ``sys.stdout``.
"""

from __future__ import annotations

import json
from typing import Any, BinaryIO

from pydantic import BaseModel, ValidationError

from nikucooker_ai.protocol.errors import ProtocolError
from nikucooker_ai.protocol.models import (
    PROTOCOL_VERSION,
    ErrorEvent,
    FatalEvent,
    ProgressEvent,
    ReadyEvent,
    Request,
    ResultEvent,
)

#: Bounds a single message, matching the Go reader.
MAX_LINE_BYTES = 64 << 20  # 64 MiB

#: Event type -> model, used to dispatch an incoming event.
_EVENT_MODELS: dict[str, type[BaseModel]] = {
    "progress": ProgressEvent,
    "result": ResultEvent,
    "error": ErrorEvent,
    "ready": ReadyEvent,
    "fatal": FatalEvent,
}


class ProtocolDecodeError(Exception):
    """A message could not be decoded.

    Always a protocol violation rather than bad user input, so it is reported and
    never retried.
    """


class ProtocolLineTooLong(ProtocolDecodeError):
    """A message exceeded :data:`MAX_LINE_BYTES`.

    Kept separate from a generic decode error because the consequence differs: an
    oversized line desynchronises the stream, so it is fatal rather than
    skippable.
    """


class ProtocolReader:
    """Reads newline-delimited JSON messages from a binary stream."""

    def __init__(self, stream: BinaryIO, max_bytes: int = MAX_LINE_BYTES) -> None:
        self._stream = stream
        self._max = max_bytes

    def read_line(self) -> bytes | None:
        """Returns the next non-blank message, or ``None`` at end of stream.

        Blank lines are skipped rather than reported: a stray newline is a common
        artifact of a shell pipeline and carries no meaning. A trailing carriage
        return is stripped so a Windows writer behaves identically.
        """
        while True:
            raw = self._stream.readline(self._max + 1)
            if not raw:
                return None
            if len(raw) > self._max:
                raise ProtocolLineTooLong(
                    f"message exceeds {self._max} bytes; the stream cannot be resynchronised"
                )
            raw = raw.rstrip(b"\r\n")
            if not raw.strip():
                continue
            return raw

    def read_request(self) -> Request | None:
        """Reads and validates the next request."""
        line = self.read_line()
        if line is None:
            return None
        return decode_request(line)


class ProtocolWriter:
    """Writes newline-delimited JSON messages to a binary stream.

    Every write is flushed. A buffered message that has not reached the pipe is
    indistinguishable from a hang to the other side, and this protocol has no
    other liveness signal.
    """

    def __init__(self, stream: BinaryIO) -> None:
        self._stream = stream

    def write(self, message: BaseModel | dict[str, Any]) -> None:
        payload = message if isinstance(message, dict) else dump_wire(message)
        line = canonical_json(payload)
        self._stream.write(line + b"\n")
        self._stream.flush()

    def write_event(self, event: BaseModel) -> None:
        """Writes an event, refusing anything that is not one.

        The guard exists because the failure it prevents is silent: a request
        written to the event stream would be read by the core as an unknown
        message type, and the symptom would appear far from the cause.
        """
        etype = getattr(event, "type", None)
        if etype not in _EVENT_MODELS:
            raise ValueError(f"not an event: {type(event).__name__}")
        self.write(event)

    def write_error(self, request_id: str, error: ProtocolError) -> None:
        self.write(ErrorEvent(id=request_id, error=error))

    def write_fatal(self, error: ProtocolError) -> None:
        self.write(FatalEvent(error=error))


def decode_request(line: bytes) -> Request:
    """Decodes and validates a request line."""
    try:
        payload = json.loads(line)
    except json.JSONDecodeError as exc:
        raise ProtocolDecodeError(f"malformed request: {exc}") from exc

    if not isinstance(payload, dict):
        raise ProtocolDecodeError("request is not a JSON object")

    version = payload.get("v")
    if version != PROTOCOL_VERSION:
        raise ProtocolDecodeError(
            f"unsupported protocol version: got {version!r}, want {PROTOCOL_VERSION}"
        )

    try:
        return Request.model_validate(payload)
    except ValidationError as exc:
        raise ProtocolDecodeError(f"malformed request: {exc}") from exc


def decode_event(line: bytes) -> BaseModel:
    """Decodes an event line into its concrete model."""
    try:
        payload = json.loads(line)
    except json.JSONDecodeError as exc:
        raise ProtocolDecodeError(f"malformed event: {exc}") from exc

    if not isinstance(payload, dict):
        raise ProtocolDecodeError("event is not a JSON object")

    version = payload.get("v")
    if version != PROTOCOL_VERSION:
        raise ProtocolDecodeError(
            f"unsupported protocol version: got {version!r}, want {PROTOCOL_VERSION}"
        )

    etype = payload.get("type")
    model = _EVENT_MODELS.get(etype) if isinstance(etype, str) else None
    if model is None:
        raise ProtocolDecodeError(f"unknown event type: {etype!r}")

    try:
        return model.model_validate(payload)
    except ValidationError as exc:
        raise ProtocolDecodeError(f"malformed {etype} event: {exc}") from exc


def dump_wire(model: BaseModel) -> dict[str, Any]:
    """Serialises a model into its wire form.

    ``exclude_none`` mirrors Go's ``omitempty`` on pointers and optionals. It does
    not mirror ``omitempty`` on a plain Go string, which also drops ``""``: this
    side would emit ``"message": ""`` where Go emits nothing. The difference is
    benign — Go decodes both to the empty string — and closing it would mean
    dropping any empty string field, including required ones that Go does emit.
    """
    return model.model_dump(mode="json", exclude_none=True)


def canonical_json(value: Any) -> bytes:
    """Renders a value in canonical form.

    Object keys sorted, no insignificant whitespace, non-ASCII preserved.

    ``ensure_ascii=False`` is required for agreement with Go: the default escapes
    every non-ASCII character, so Japanese subtitle text would hash differently on
    the two sides. Go's equivalent switch is ``SetEscapeHTML(false)`` — see
    pkg/protocol/canonical.go.
    """
    return json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
    ).encode("utf-8")
