"""The speech recognition provider interface.

A provider turns an audio file into segments with word timings. It holds no
project state and knows nothing about jobs or artifacts — it is given a path and
configuration and returns a structure.

Adding a backend means adding a class here and registering it; nothing in the
Go core or the pipeline changes. That is the seam that makes MLX Whisper on
Apple Silicon or WhisperX's forced alignment an addition rather than a rewrite.
"""

from __future__ import annotations

from abc import ABC, abstractmethod
from collections.abc import Callable, Iterator
from dataclasses import dataclass, field
from typing import Any

from nikucooker_ai.protocol.models import ASRResult, VADRegion

#: Reports progress as a fraction and a short status.
ProgressFn = Callable[[float, str], None]

#: Raises if the request has been cancelled.
CancelFn = Callable[[], None]


@dataclass
class ASRRequest:
    """Everything a provider needs to transcribe."""

    audio_path: str
    model: str
    language: str | None = None
    device: str = "auto"
    compute_type: str | None = None

    beam_size: int = 5
    temperature: float | None = None
    condition_on_previous_text: bool | None = None
    word_timestamps: bool = True
    initial_prompt: str | None = None

    #: Restricts recognition to detected speech. Empty means the whole file.
    vad_regions: list[VADRegion] = field(default_factory=list)

    #: Where the model lives on disk, when the core has already resolved it.
    model_path: str | None = None


@dataclass
class DeviceChoice:
    """The device a model actually landed on, and why."""

    device: str
    compute_type: str
    fell_back: bool = False
    reason: str = ""


class ASRProvider(ABC):
    """Recognises speech in an audio file."""

    #: Identifies the provider in configuration, artifacts and the API.
    name: str = ""

    @abstractmethod
    def load(
        self,
        model: str,
        device: str,
        compute_type: str | None,
        model_path: str | None,
    ) -> DeviceChoice:
        """Makes a model resident and reports where it landed."""

    @abstractmethod
    def unload(self, model: str) -> int:
        """Releases a model, returning the megabytes freed."""

    @abstractmethod
    def loaded(self) -> list[tuple[str, DeviceChoice]]:
        """Lists resident models."""

    @abstractmethod
    def transcribe(
        self,
        request: ASRRequest,
        progress: ProgressFn | None = None,
        check_cancelled: CancelFn | None = None,
    ) -> ASRResult:
        """Transcribes a file.

        `check_cancelled` is called between chunks rather than on a timer: a
        handler should stop at a point where its own state is consistent, and
        the only such points are between units of work.
        """

    def supports(self, model: str) -> bool:
        """Reports whether this provider can run a model name."""
        return True


# ---------------------------------------------------------------------------
# Registry
# ---------------------------------------------------------------------------

_PROVIDERS: dict[str, type[ASRProvider]] = {}


def register(provider: type[ASRProvider]) -> type[ASRProvider]:
    """Registers a provider class under its `name`."""
    if not provider.name:
        raise ValueError(f"{provider.__name__} has no name")
    _PROVIDERS[provider.name] = provider
    return provider


def create(name: str, **kwargs: Any) -> ASRProvider:
    """Builds a provider by name.

    Raising here rather than returning None: an unknown provider is a
    configuration error, and a caller that has to check for nil will eventually
    forget.
    """
    if name not in _PROVIDERS:
        known = ", ".join(sorted(_PROVIDERS)) or "(none installed)"
        raise ValueError(f"unknown ASR provider {name!r}; available: {known}")
    return _PROVIDERS[name](**kwargs)


def available() -> list[str]:
    """Lists registered provider names."""
    return sorted(_PROVIDERS)


def iter_chunks(regions: list[VADRegion], total_duration: float) -> Iterator[tuple[float, float]]:
    """Yields the spans to transcribe, in order.

    With no regions the whole file is one span. Regions are used as given rather
    than merged: the VAD already padded and coalesced them, and merging again
    would undo that.
    """
    if not regions:
        yield (0.0, total_duration)
        return
    for region in regions:
        yield (region.start, region.end)
