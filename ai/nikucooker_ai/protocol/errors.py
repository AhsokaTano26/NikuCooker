"""Error codes and the protocol error payload.

Mirrors pkg/protocol/errors.go. The code set is closed: adding one is a protocol
change and must be made on both sides, which the schema digest will catch but a
reviewer should catch first.
"""

from __future__ import annotations

from enum import StrEnum
from typing import Any

from pydantic import BaseModel, ConfigDict, Field


class ErrorCode(StrEnum):
    """Stable machine-readable error identifiers.

    Callers branch on these; they never branch on the human-readable message.
    """

    INVALID_PARAMS = "INVALID_PARAMS"
    UNSUPPORTED_METHOD = "UNSUPPORTED_METHOD"
    UNSUPPORTED_PROTOCOL_VERSION = "UNSUPPORTED_PROTOCOL_VERSION"
    DEPENDENCY_MISSING = "DEPENDENCY_MISSING"
    MODEL_NOT_FOUND = "MODEL_NOT_FOUND"
    MODEL_LOAD_FAILED = "MODEL_LOAD_FAILED"
    MODEL_ALREADY_LOADED = "MODEL_ALREADY_LOADED"
    DEVICE_UNAVAILABLE = "DEVICE_UNAVAILABLE"
    OUT_OF_MEMORY = "OUT_OF_MEMORY"
    AUDIO_READ_FAILED = "AUDIO_READ_FAILED"
    AUDIO_DECODE_FAILED = "AUDIO_DECODE_FAILED"
    INFERENCE_FAILED = "INFERENCE_FAILED"
    CANCELLED = "CANCELLED"
    TIMEOUT = "TIMEOUT"
    INTERNAL = "INTERNAL"


#: Codes describing a condition that might not recur.
#:
#: Retryability describes the request, not the moment. Whether to actually retry
#: is the core's decision, made against its own budget.
RETRYABLE: frozenset[ErrorCode] = frozenset(
    {
        ErrorCode.MODEL_LOAD_FAILED,
        ErrorCode.DEVICE_UNAVAILABLE,
        ErrorCode.OUT_OF_MEMORY,
        ErrorCode.INFERENCE_FAILED,
        ErrorCode.TIMEOUT,
        ErrorCode.INTERNAL,
    }
)


class ProtocolError(BaseModel):
    """The payload of an error or fatal event.

    ``message`` is written for a human and may change between releases. It may
    contain a filesystem path, which is what makes a failure diagnosable without
    reproducing it. It must never contain a secret.
    """

    model_config = ConfigDict(extra="forbid")

    code: ErrorCode
    message: str
    retryable: bool
    details: dict[str, Any] | None = Field(default=None)

    @classmethod
    def make(
        cls,
        code: ErrorCode,
        message: str,
        *,
        retryable: bool | None = None,
        details: dict[str, Any] | None = None,
    ) -> ProtocolError:
        """Builds an error, defaulting ``retryable`` from the code.

        Deriving the default from the code means a call site cannot accidentally
        mark a transient failure permanent by omitting the argument.
        """
        return cls(
            code=code,
            message=message,
            retryable=code in RETRYABLE if retryable is None else retryable,
            details=details,
        )


class RequestFailed(Exception):
    """Raised inside a handler to fail one request without killing the worker.

    The worker turns this into an error event for the request's id and carries
    on serving, which is what keeps one bad input from taking down the process.
    """

    def __init__(self, error: ProtocolError) -> None:
        super().__init__(f"{error.code}: {error.message}")
        self.error = error
