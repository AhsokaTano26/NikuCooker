"""Voice activity detection through the Silero model.

The model ships inside the faster-whisper wheel and runs on onnxruntime, which
faster-whisper already depends on. The `silero-vad` package is deliberately not
used: it requires torch and torchaudio with no torch-free path, and would add
roughly 2.5 GB and pin `torch==2.9.1`. See docs/dependency-audit.md §3.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

from nikucooker_ai.asr.base import CancelFn, ProgressFn
from nikucooker_ai.protocol.errors import ErrorCode, ProtocolError, RequestFailed
from nikucooker_ai.protocol.models import VADRegion, VADResult

#: The rate the Silero model expects. Whisper's is the same, which is why the
#: audio stage produces it once.
SAMPLE_RATE = 16000


@dataclass
class VADOptions:
    """Detection thresholds.

    Exposed rather than fixed because the right values differ by material: an
    anime episode with a continuous music bed needs a different silence
    threshold from a talk show recorded in a quiet studio.
    """

    threshold: float = 0.5
    min_speech_ms: int = 250
    min_silence_ms: int = 100
    speech_pad_ms: int = 30
    max_speech_s: float = 30.0

    def to_silero(self) -> Any:
        from faster_whisper.vad import VadOptions as SileroOptions

        return SileroOptions(
            threshold=self.threshold,
            min_speech_duration_ms=self.min_speech_ms,
            min_silence_duration_ms=self.min_silence_ms,
            speech_pad_ms=self.speech_pad_ms,
            max_speech_duration_s=self.max_speech_s,
        )


class SileroVADProvider:
    """Detects speech regions in an audio file."""

    name = "silero-onnx"

    def detect(
        self,
        audio_path: str,
        options: VADOptions | None = None,
        progress: ProgressFn | None = None,
        check_cancelled: CancelFn | None = None,
    ) -> VADResult:
        if options is None:
            options = VADOptions()

        try:
            from faster_whisper.audio import decode_audio
            from faster_whisper.vad import get_speech_timestamps
        except ImportError as exc:
            raise RequestFailed(
                ProtocolError.make(
                    ErrorCode.DEPENDENCY_MISSING,
                    f"the VAD needs faster-whisper: {exc}",
                    details={"remediation": "run `cd ai && uv sync`"},
                )
            ) from exc

        if progress is not None:
            progress(0.0, "decoding audio")

        try:
            # decode_audio goes through PyAV, so no ffmpeg binary is needed —
            # unlike mlx-whisper, which shells out to one.
            audio = decode_audio(audio_path, sampling_rate=SAMPLE_RATE)
        except Exception as exc:
            raise RequestFailed(
                ProtocolError.make(
                    ErrorCode.AUDIO_DECODE_FAILED,
                    f"could not decode {audio_path}: {exc}",
                    details={"path": audio_path},
                )
            ) from exc

        # decode_audio's annotation admits a 2-tuple, which it returns only when
        # split_stereo is set — an option this code never passes. Narrowing
        # explicitly rather than suppressing the warning: if a future version
        # changed the default, this fails loudly instead of feeding a tuple to
        # the model and producing nonsense.
        if isinstance(audio, tuple):
            raise RequestFailed(
                ProtocolError.make(
                    ErrorCode.AUDIO_DECODE_FAILED,
                    "the decoder returned separate channels; mono audio is required",
                    details={"path": audio_path},
                )
            )

        total_seconds = len(audio) / SAMPLE_RATE

        if progress is not None:
            progress(0.1, "detecting speech")

        # Not cancellable inside: the model runs over the whole array in one
        # call, and there is no safe point to stop at. It is fast enough that
        # this does not matter in practice — an order of magnitude quicker than
        # the transcription that follows it.
        try:
            timestamps = get_speech_timestamps(
                audio,
                options.to_silero(),
                sampling_rate=SAMPLE_RATE,
            )
        except Exception as exc:
            raise RequestFailed(
                ProtocolError.make(
                    ErrorCode.INFERENCE_FAILED,
                    f"voice activity detection failed: {exc}",
                    details={"path": audio_path},
                )
            ) from exc

        if check_cancelled is not None:
            # The one point where stopping is safe, and where a job cancelled
            # during a long decode is noticed.
            check_cancelled()

        regions = [
            VADRegion(
                start=round(entry["start"] / SAMPLE_RATE, 3),
                end=round(entry["end"] / SAMPLE_RATE, 3),
            )
            for entry in timestamps
        ]
        # Invariant the core relies on: ordered, non-overlapping, each non-empty.
        regions = _sanitise(regions, total_seconds)

        speech_seconds = sum(region.end - region.start for region in regions)

        if progress is not None:
            progress(1.0, f"{len(regions)} speech regions")

        return VADResult(
            regions=regions,
            total_speech_s=round(speech_seconds, 3),
            total_audio_s=round(total_seconds, 3),
        )


def _sanitise(regions: list[VADRegion], total_seconds: float) -> list[VADRegion]:
    """Enforces the invariants the core validates on receipt.

    The model's output is trustworthy in practice, but a violated invariant
    here propagates into impossible subtitle timings much later, where the cause
    is no longer visible. Clamping at the source keeps the failure local.
    """
    cleaned: list[VADRegion] = []
    previous_end = 0.0

    for region in sorted(regions, key=lambda r: r.start):
        start = max(previous_end, max(0.0, region.start))
        end = min(total_seconds, region.end)
        if end <= start:
            # Zero-length or inverted after clamping: dropped rather than
            # passed on.
            continue
        cleaned.append(VADRegion(start=start, end=end))
        previous_end = end

    return cleaned
