"""Speech recognition through faster-whisper.

The default backend, and the only one on Linux, Windows and Intel macOS. Apple
Silicon runs it on CPU too: CTranslate2 has no Metal backend, which is a real
limitation rather than a configuration mistake.

See docs/dependency-audit.md §4.
"""

from __future__ import annotations

import os
from typing import Any

from nikucooker_ai.asr.base import (
    ASRProvider,
    ASRRequest,
    CancelFn,
    DeviceChoice,
    ProgressFn,
    register,
)
from nikucooker_ai.protocol.errors import ErrorCode, ProtocolError, RequestFailed
from nikucooker_ai.protocol.models import ASRResult, ASRSegment, WordTimestamp

#: Compute types that work well on each device, best first.
#:
#: float16 needs a GPU; int8 is the CPU default because it is roughly twice as
#: fast as float32 for a difference that is not perceptible in subtitle text.
_COMPUTE_TYPES = {
    "cuda": ["float16", "int8_float16", "int8", "float32"],
    "cpu": ["int8", "float32"],
}


@register
class FasterWhisperProvider(ASRProvider):
    """Runs Whisper through CTranslate2."""

    name = "faster-whisper"

    def __init__(self) -> None:
        # Model name -> (model handle, device choice). Holding the handle is
        # what makes a second transcription cheap.
        self._models: dict[str, tuple[Any, DeviceChoice]] = {}

    # -- residency ---------------------------------------------------------

    def load(
        self,
        model: str,
        device: str,
        compute_type: str | None,
        model_path: str | None,
    ) -> DeviceChoice:
        from faster_whisper import WhisperModel

        resolved_device, resolved_compute, fell_back, reason = _resolve_device(device, compute_type)

        source = model_path or model
        try:
            handle = WhisperModel(
                source,
                device=resolved_device,
                compute_type=resolved_compute,
            )
        except Exception as exc:
            # A requested GPU that cannot be used is worth retrying on CPU
            # rather than failing the job: the work is the same, only slower.
            if resolved_device != "cpu":
                try:
                    handle = WhisperModel(source, device="cpu", compute_type="int8")
                except Exception as cpu_exc:
                    raise RequestFailed(
                        ProtocolError.make(
                            ErrorCode.MODEL_LOAD_FAILED,
                            f"could not load {model!r} on {resolved_device} ({exc}) "
                            f"or on cpu ({cpu_exc})",
                            details={"model": model, "path": str(source)},
                        )
                    ) from cpu_exc

                choice = DeviceChoice(
                    device="cpu",
                    compute_type="int8",
                    fell_back=True,
                    reason=f"{resolved_device} could not be initialised: {exc}",
                )
                self._models[model] = (handle, choice)
                return choice

            raise RequestFailed(
                ProtocolError.make(
                    ErrorCode.MODEL_LOAD_FAILED,
                    f"could not load {model!r}: {exc}",
                    details={"model": model, "path": str(source)},
                )
            ) from exc

        choice = DeviceChoice(
            device=resolved_device,
            compute_type=resolved_compute,
            fell_back=fell_back,
            reason=reason,
        )
        self._models[model] = (handle, choice)
        return choice

    def unload(self, model: str) -> int:
        entry = self._models.pop(model, None)
        if entry is None:
            return 0

        handle, _ = entry
        # CTranslate2 releases its buffers when the object is collected; there
        # is no explicit free. Deliberately not calling gc.collect() here: the
        # interpreter's refcounting already handles it, and forcing a full
        # collection costs more than it saves.
        del handle
        return 0

    def loaded(self) -> list[tuple[str, DeviceChoice]]:
        return [(name, choice) for name, (_, choice) in self._models.items()]

    def is_loaded(self, model: str) -> bool:
        return model in self._models

    # -- inference ---------------------------------------------------------

    def transcribe(
        self,
        request: ASRRequest,
        progress: ProgressFn | None = None,
        check_cancelled: CancelFn | None = None,
    ) -> ASRResult:
        handle = self._ensure_loaded(request)

        if not os.path.isfile(request.audio_path):
            raise RequestFailed(
                ProtocolError.make(
                    ErrorCode.AUDIO_READ_FAILED,
                    f"audio file not found: {request.audio_path}",
                    details={"path": request.audio_path},
                )
            )

        options: dict[str, Any] = {
            "beam_size": request.beam_size,
            "word_timestamps": request.word_timestamps,
            "vad_filter": False,
        }
        if request.language:
            options["language"] = request.language
        if request.temperature is not None:
            options["temperature"] = request.temperature
        if request.condition_on_previous_text is not None:
            options["condition_on_previous_text"] = request.condition_on_previous_text
        if request.initial_prompt:
            options["initial_prompt"] = request.initial_prompt

        try:
            segments, info = handle.transcribe(request.audio_path, **options)
        except Exception as exc:
            raise RequestFailed(
                ProtocolError.make(
                    ErrorCode.INFERENCE_FAILED,
                    f"{type(exc).__name__}: {exc}",
                    details={"model": request.model, "path": request.audio_path},
                )
            ) from exc

        # `segments` is a generator: nothing is recognised until it is
        # iterated, which is what makes progress reporting and cancellation
        # possible at all.
        duration = float(getattr(info, "duration", 0.0) or 0.0)
        result_segments: list[ASRSegment] = []
        word_count = 0

        for segment in segments:
            if check_cancelled is not None:
                check_cancelled()

            converted = _to_segment(segment)
            result_segments.append(converted)
            word_count += len(converted.words)

            if progress is not None:
                # Progress is also the core's liveness signal: a long
                # transcription that stopped reporting would be killed as
                # stalled, so this fires per segment rather than per file.
                progress(
                    min(1.0, segment.end / duration) if duration > 0 else 0.0,
                    f"transcribed {len(result_segments)} segments",
                )

        if progress is not None:
            progress(1.0, f"transcribed {len(result_segments)} segments")

        return ASRResult(
            language=request.language or str(getattr(info, "language", "")),
            language_probability=getattr(info, "language_probability", None),
            duration=duration,
            segments=result_segments,
        )

    def _ensure_loaded(self, request: ASRRequest) -> Any:
        """Returns the resident model, loading it if the core did not."""
        entry = self._models.get(request.model)
        if entry is not None:
            return entry[0]

        # The core normally loads first so the device is reported before work
        # starts, but a direct call must not fail for want of that step.
        self.load(
            request.model,
            request.device,
            request.compute_type,
            request.model_path,
        )
        return self._models[request.model][0]


# ---------------------------------------------------------------------------
# Conversion
# ---------------------------------------------------------------------------


def _to_segment(segment: Any) -> ASRSegment:
    """Converts a faster-whisper segment into the wire model."""
    words: list[WordTimestamp] = []
    for word in getattr(segment, "words", None) or []:
        words.append(
            WordTimestamp(
                start=float(word.start),
                end=float(word.end),
                # faster-whisper names the field `word`; the wire format calls
                # it `text` to match the Go model.
                text=str(getattr(word, "word", "")),
                probability=getattr(word, "probability", None),
            )
        )

    return ASRSegment(
        start=float(segment.start),
        end=float(segment.end),
        text=str(segment.text).strip(),
        avg_logprob=getattr(segment, "avg_logprob", None),
        no_speech_prob=getattr(segment, "no_speech_prob", None),
        words=words,
    )


# ---------------------------------------------------------------------------
# Device resolution
# ---------------------------------------------------------------------------


def _resolve_device(requested: str, compute_type: str | None) -> tuple[str, str, bool, str]:
    """Chooses a device and compute type.

    Returns (device, compute_type, fell_back, reason). The reason is what the
    core records on the artifact, so "why is this so slow" is answerable without
    re-running anything.
    """
    if requested == "cpu":
        return "cpu", compute_type or "int8", False, ""

    available_cuda = 0
    try:
        import ctranslate2

        available_cuda = ctranslate2.get_cuda_device_count()
    except Exception:
        available_cuda = 0

    if requested == "cuda":
        if available_cuda == 0:
            return (
                "cpu",
                compute_type or "int8",
                True,
                "cuda was requested but no CUDA device is available",
            )
        return "cuda", compute_type or "float16", False, ""

    # "auto": prefer the accelerator, and say so when it is not there.
    if available_cuda > 0:
        return "cuda", compute_type or "float16", False, ""

    reason = "no CUDA device is available"
    try:
        import sys

        if sys.platform == "darwin":
            reason += "; Apple Silicon has no CTranslate2 Metal backend, so recognition runs on CPU"
    except Exception:
        pass

    return "cpu", compute_type or "int8", True, reason
