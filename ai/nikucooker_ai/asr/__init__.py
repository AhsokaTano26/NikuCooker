"""Speech recognition providers.

Importing this package registers the built-in providers, so a caller asks the
registry for a name rather than importing a class. That is the seam that lets a
new backend be added without touching the pipeline.
"""

from nikucooker_ai.asr.base import (
    ASRProvider,
    ASRRequest,
    DeviceChoice,
    available,
    create,
    register,
)

# Imported for the side effect of registering. The linters would otherwise
# remove them.
from nikucooker_ai.asr.faster_whisper import FasterWhisperProvider

__all__ = [
    "ASRProvider",
    "ASRRequest",
    "DeviceChoice",
    "FasterWhisperProvider",
    "available",
    "create",
    "register",
]
