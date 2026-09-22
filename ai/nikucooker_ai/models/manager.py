"""Model residency.

The core owns model *metadata*: what exists, what is downloaded, how large it
is. This owns model *residency*: what is loaded in this process right now, and
on which device.

The split matters because the two fail differently. A missing model is a
download problem the user fixes once; a model that will not fit in memory is a
runtime problem tied to this machine's current state.
"""

from __future__ import annotations

from nikucooker_ai.asr import ASRRequest
from nikucooker_ai.asr import available as available_asr
from nikucooker_ai.asr import create as create_asr
from nikucooker_ai.asr.base import ASRProvider, CancelFn, ProgressFn
from nikucooker_ai.protocol.errors import ErrorCode, ProtocolError, RequestFailed
from nikucooker_ai.protocol.models import (
    ASRResult,
    LoadedModel,
    ModelListLoadedResult,
    ModelLoadResult,
    ModelUnloadResult,
    VADRegion,
    VADResult,
)
from nikucooker_ai.vad import SileroVADProvider, VADOptions

#: The kind names the protocol uses.
KIND_ASR = "asr"
KIND_VAD = "vad"


class ModelManager:
    """Holds what is resident in this worker."""

    def __init__(self) -> None:
        self._asr_providers: dict[str, ASRProvider] = {}
        self._vad = SileroVADProvider()
        #: The provider a request uses when it does not name one.
        self._default_asr = _default_provider_name()

    # -- residency ---------------------------------------------------------

    def load(
        self,
        kind: str,
        name: str,
        device: str = "auto",
        compute_type: str | None = None,
        model_path: str | None = None,
        provider: str | None = None,
    ) -> ModelLoadResult:
        if kind == KIND_VAD:
            # The VAD model is bundled and has nothing to load; reporting
            # success without touching the network is the correct answer.
            return ModelLoadResult(
                loaded=True,
                device="cpu",
                compute_type="int8",
                load_ms=0,
            )

        if kind != KIND_ASR:
            raise RequestFailed(
                ProtocolError.make(
                    ErrorCode.INVALID_PARAMS,
                    f"unknown model kind {kind!r}; expected {KIND_ASR!r} or {KIND_VAD!r}",
                )
            )

        asr = self._provider(provider)
        choice = asr.load(name, device, compute_type, model_path)

        return ModelLoadResult(
            loaded=True,
            device=choice.device,
            compute_type=choice.compute_type,
            load_ms=0,
            fell_back=choice.fell_back,
            fallback_reason=choice.reason or None,
        )

    def unload(self, kind: str, name: str, provider: str | None = None) -> ModelUnloadResult:
        if kind == KIND_VAD:
            return ModelUnloadResult(unloaded=True)
        if kind != KIND_ASR:
            raise RequestFailed(
                ProtocolError.make(ErrorCode.INVALID_PARAMS, f"unknown model kind {kind!r}")
            )

        asr = self._provider(provider)
        freed = asr.unload(name)
        return ModelUnloadResult(unloaded=True, freed_mb=freed)

    def list_loaded(self) -> ModelListLoadedResult:
        models: list[LoadedModel] = []
        for provider in self._asr_providers.values():
            for name, choice in provider.loaded():
                models.append(
                    LoadedModel(
                        kind=KIND_ASR,
                        name=name,
                        device=choice.device,
                        compute_type=choice.compute_type,
                    )
                )
        return ModelListLoadedResult(models=models)

    def _provider(self, name: str | None) -> ASRProvider:
        """Returns the named provider, creating it on first use.

        Providers are cached per name so a second request reuses the resident
        model rather than loading a second copy — which for large-v3 is over a
        gigabyte of memory nobody would notice until the machine swapped.
        """
        resolved = name or self._default_asr
        if resolved not in self._asr_providers:
            self._asr_providers[resolved] = create_asr(resolved)
        return self._asr_providers[resolved]

    # -- inference ---------------------------------------------------------

    def transcribe(
        self,
        audio_path: str,
        model: str,
        language: str | None = None,
        device: str = "auto",
        compute_type: str | None = None,
        beam_size: int = 5,
        temperature: float | None = None,
        condition_on_previous_text: bool | None = None,
        word_timestamps: bool = True,
        initial_prompt: str | None = None,
        vad_regions: list[VADRegion] | None = None,
        model_path: str | None = None,
        provider: str | None = None,
        progress: ProgressFn | None = None,
        check_cancelled: CancelFn | None = None,
    ) -> ASRResult:
        request = ASRRequest(
            audio_path=audio_path,
            model=model,
            language=language,
            device=device,
            compute_type=compute_type,
            beam_size=beam_size,
            temperature=temperature,
            condition_on_previous_text=condition_on_previous_text,
            word_timestamps=word_timestamps,
            initial_prompt=initial_prompt,
            vad_regions=vad_regions or [],
            model_path=model_path,
        )
        return self._provider(provider).transcribe(request, progress, check_cancelled)

    def detect_speech(
        self,
        audio_path: str,
        threshold: float = 0.5,
        min_speech_ms: int = 250,
        min_silence_ms: int = 100,
        speech_pad_ms: int = 30,
        max_speech_s: float = 30.0,
        progress: ProgressFn | None = None,
        check_cancelled: CancelFn | None = None,
    ) -> VADResult:
        options = VADOptions(
            threshold=threshold,
            min_speech_ms=min_speech_ms,
            min_silence_ms=min_silence_ms,
            speech_pad_ms=speech_pad_ms,
            max_speech_s=max_speech_s,
        )
        return self._vad.detect(audio_path, options, progress, check_cancelled)


def _default_provider_name() -> str:
    """Chooses the backend a request uses when it names none."""
    names = available_asr()
    if not names:
        return "faster-whisper"
    # faster-whisper is the portable default; an MLX backend, when it exists,
    # is chosen explicitly by configuration rather than by detection, because
    # its model artifacts differ and are not interchangeable.
    return "faster-whisper" if "faster-whisper" in names else names[0]
