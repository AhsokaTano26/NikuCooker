"""Protocol types mirrored from the Go side.

Go:     pkg/protocol/
Python: ai/nikucooker_ai/protocol/

The two are held together by the golden fixtures in pkg/protocol/testdata/,
which both test suites decode and re-serialise. See docs/ipc-protocol.md §8.
"""

from nikucooker_ai.protocol.codec import (
    PROTOCOL_VERSION,
    ProtocolReader,
    ProtocolWriter,
    canonical_json,
    decode_event,
    decode_request,
)
from nikucooker_ai.protocol.errors import ErrorCode, ProtocolError

__all__ = [
    "PROTOCOL_VERSION",
    "ErrorCode",
    "ProtocolError",
    "ProtocolReader",
    "ProtocolWriter",
    "canonical_json",
    "decode_event",
    "decode_request",
]
