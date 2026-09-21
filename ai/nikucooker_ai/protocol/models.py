"""Wire models mirroring pkg/protocol.

Field names, optionality and the pointer-vs-value distinction all match the Go
declarations deliberately. An optional numeric is ``float | None`` for the same
reason Go uses ``*float64``: a confidence of 0.0 and "no confidence reported"
are different facts, and collapsing them loses information QC and the review UI
both need.
"""

from __future__ import annotations

from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field

from nikucooker_ai.protocol.errors import ProtocolError

#: Protocol version carried in the "v" field of every message, both directions.
PROTOCOL_VERSION = 1

#: Identifies this worker implementation in the ready handshake.
WORKER_NAME = "nikucooker-ai"


class _Wire(BaseModel):
    """Base for every message.

    ``extra="forbid"`` is the point of this base class. Go ignores unknown JSON
    fields, so a field renamed on one side and not the other would be silently
    dropped there; here it is a hard failure, on the side that is wrong.
    """

    model_config = ConfigDict(extra="forbid")


# ---------------------------------------------------------------------------
# Shared data models
# ---------------------------------------------------------------------------


class WordTimestamp(_Wire):
    """One word with its timing."""

    start: float
    end: float
    text: str
    probability: float | None = None


class ASRSegment(_Wire):
    """One recognition segment as the model produced it.

    These are inputs to segmentation, never final subtitle lines.
    """

    start: float
    end: float
    text: str
    avg_logprob: float | None = None
    no_speech_prob: float | None = None
    words: list[WordTimestamp] = Field(default_factory=list)


class ASRResult(_Wire):
    """Full transcription payload.

    Written to ``result_path`` when the request supplied one, or returned inline
    in the result event when it did not.
    """

    language: str
    language_probability: float | None = None
    duration: float
    segments: list[ASRSegment] = Field(default_factory=list)


class VADRegion(_Wire):
    """One detected speech interval, satisfying ``start < end``."""

    start: float
    end: float


class VADResult(_Wire):
    """Speech detection output."""

    regions: list[VADRegion] = Field(default_factory=list)
    total_speech_s: float
    total_audio_s: float


class LoadedModel(_Wire):
    """A model resident in the worker, with the device it actually landed on."""

    kind: str
    name: str
    device: str
    compute_type: str | None = None
    memory_mb: int = 0


# ---------------------------------------------------------------------------
# Requests (Go -> worker)
# ---------------------------------------------------------------------------


class Request(_Wire):
    """A request. Identified on the wire by carrying ``method``."""

    v: int = PROTOCOL_VERSION
    id: str
    method: str
    params: dict[str, Any] = Field(default_factory=dict)


class EchoParams(_Wire):
    """Echo returns its params verbatim, for framing tests."""

    model_config = ConfigDict(extra="allow")


class CancelParams(_Wire):
    target_id: str


class ModelLoadParams(_Wire):
    kind: Literal["asr", "vad"]
    name: str
    device: str = "auto"
    compute_type: str | None = None
    model_path: str | None = None


class ModelUnloadParams(_Wire):
    kind: Literal["asr", "vad"]
    name: str


class VADDetectParams(_Wire):
    audio_path: str
    threshold: float = 0.5
    min_speech_ms: int = 250
    min_silence_ms: int = 100
    speech_pad_ms: int = 30
    max_speech_s: float = 30.0


class ASRTranscribeParams(_Wire):
    """Configures transcription.

    ``result_path`` is the escape hatch for large results: when set, the worker
    writes the full :class:`ASRResult` there and returns only a summary, keeping
    pipe traffic bounded regardless of media length.
    """

    audio_path: str
    model: str
    language: str | None = None
    device: str = "auto"

    beam_size: int = 5
    temperature: float | None = None
    condition_on_previous_text: bool | None = None
    word_timestamps: bool | None = None
    initial_prompt: str | None = None

    vad_regions: list[VADRegion] | None = None
    result_path: str | None = None


# ---------------------------------------------------------------------------
# Events (worker -> Go)
# ---------------------------------------------------------------------------


class ProgressEvent(_Wire):
    """Partial completion of a request.

    Progress is monotonically non-decreasing within a request and is the liveness
    signal the core's stall detection relies on.
    """

    v: int = PROTOCOL_VERSION
    id: str
    type: Literal["progress"] = "progress"
    progress: float
    message: str | None = None
    data: dict[str, Any] | None = None


class ResultEvent(_Wire):
    """Terminal success for a request."""

    v: int = PROTOCOL_VERSION
    id: str
    type: Literal["result"] = "result"
    result: dict[str, Any] = Field(default_factory=dict)
    elapsed_ms: int | None = None


class ErrorEvent(_Wire):
    """Terminal failure for a request."""

    v: int = PROTOCOL_VERSION
    id: str
    type: Literal["error"] = "error"
    error: ProtocolError


class ProtocolSpan(_Wire):
    """Inclusive range of protocol versions a worker supports."""

    min: int
    max: int

    def supports(self, version: int) -> bool:
        return self.min <= version <= self.max


class Providers(_Wire):
    """Implementation names available for one capability."""

    providers: list[str] = Field(default_factory=list)


class Capabilities(_Wire):
    """What the worker can do on this host.

    ``notes`` carries the reason for any degraded capability, so a fallback is
    visible rather than something the user infers from a slow run.
    """

    asr: Providers
    vad: Providers
    devices: list[str] = Field(default_factory=list)
    notes: list[str] = Field(default_factory=list)


class ReadyEvent(_Wire):
    """Startup handshake, emitted once before any request is served."""

    v: int = PROTOCOL_VERSION
    type: Literal["ready"] = "ready"
    worker: str = WORKER_NAME
    worker_version: str
    protocol: ProtocolSpan
    schema_digest: str
    python: str
    platform: str
    capabilities: Capabilities


class FatalEvent(_Wire):
    """Precedes an unrecoverable worker exit. Not tied to any request."""

    v: int = PROTOCOL_VERSION
    type: Literal["fatal"] = "fatal"
    error: ProtocolError


class DeviceInfo(_Wire):
    """The selected compute device and what else is present."""

    selected: str
    cuda: bool
    mps: bool
    cpu_count: int


class ASRCaps(_Wire):
    providers: list[str] = Field(default_factory=list)
    compute_types: list[str] = Field(default_factory=list)


class CapabilitiesResult(_Wire):
    device: DeviceInfo
    asr: ASRCaps
    vad: Providers
    notes: list[str] = Field(default_factory=list)


class HealthResult(_Wire):
    alive: bool
    uptime_s: float
    loaded_models: list[LoadedModel] = Field(default_factory=list)
    inflight: list[str] = Field(default_factory=list)
    rss_mb: float | None = None


class CancelResult(_Wire):
    cancelled: bool


class ShutdownResult(_Wire):
    ok: bool


class ModelLoadResult(_Wire):
    """Where the model actually landed.

    ``fell_back`` and ``fallback_reason`` are the honesty mechanism: a silent
    fallback to CPU makes a job twenty times slower without saying so.
    """

    loaded: bool
    device: str
    compute_type: str | None = None
    load_ms: int
    fell_back: bool = False
    fallback_reason: str | None = None


class ModelUnloadResult(_Wire):
    unloaded: bool
    freed_mb: int = 0


class ModelListLoadedResult(_Wire):
    models: list[LoadedModel] = Field(default_factory=list)


class ASRTranscribeResult(_Wire):
    """Summary of a transcription.

    ``segments`` is populated only when the request omitted ``result_path``. The
    two forms are mutually exclusive, and a caller that reads neither is a caller
    that silently produces an empty transcript.
    """

    result_path: str | None = None
    segments: list[ASRSegment] | None = None
    language: str
    language_probability: float | None = None
    duration: float
    segment_count: int
    word_count: int
    device: str
    compute_type: str | None = None
    model: str
