"""Environment self-check.

Run as ``python -m nikucooker_ai --selfcheck``, which prints one JSON line and
exits. The core runs it before spawning a worker for real work, so that a broken
environment is reported as a named missing dependency with a remediation rather
than as a stream of confusing failures later.

Nothing here loads a model.
"""

from __future__ import annotations

import importlib.util
import platform
import shutil
import sys
from dataclasses import dataclass, field
from importlib.metadata import PackageNotFoundError
from importlib.metadata import version as package_version
from pathlib import Path
from typing import Any

from nikucooker_ai import SPEAKS_PROTOCOL_MAX, SPEAKS_PROTOCOL_MIN, __version__
from nikucooker_ai.protocol import PROTOCOL_VERSION

#: Distribution name -> import name. The two differ often enough that conflating
#: them produces a check that reports "installed" for the wrong package.
REQUIRED_PACKAGES: tuple[tuple[str, str], ...] = (
    ("faster-whisper", "faster_whisper"),
    ("ctranslate2", "ctranslate2"),
    ("onnxruntime", "onnxruntime"),
    ("pydantic", "pydantic"),
)

#: Bundled inside the faster-whisper wheel. Depending on the silero-vad package
#: instead would pull in torch and torchaudio; see docs/dependency-audit.md §3.
VAD_MODEL_RELATIVE = Path("assets") / "silero_vad_v6.onnx"


@dataclass
class Check:
    """One named check with its outcome."""

    name: str
    ok: bool
    value: str | None = None
    error: str | None = None

    def as_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {"name": self.name, "ok": self.ok}
        if self.value is not None:
            out["version" if self.name.startswith("import:") else "value"] = self.value
        if self.error is not None:
            out["error"] = self.error
        return out


@dataclass
class DeviceReport:
    """What this host can run, and the reason for anything it cannot."""

    selected: str = "cpu"
    cuda: bool = False
    mps: bool = False
    compute_types: list[str] = field(default_factory=lambda: ["int8", "float32"])
    notes: list[str] = field(default_factory=list)


def run_selfcheck() -> dict[str, Any]:
    """Runs every check and returns the payload printed to stdout."""
    checks: list[Check] = [_check_package(dist, module) for dist, module in REQUIRED_PACKAGES]
    checks.append(_check_vad_model())
    checks.append(_check_ffmpeg())

    device = detect_device()
    digest, digest_error = _schema_digest_safe()

    return {
        "v": PROTOCOL_VERSION,
        "type": "selfcheck",
        "ok": all(c.ok for c in checks),
        "worker": "nikucooker-ai",
        "worker_version": __version__,
        "python": platform.python_version(),
        "platform": f"{sys.platform}-{platform.machine()}",
        "protocol": {"min": SPEAKS_PROTOCOL_MIN, "max": SPEAKS_PROTOCOL_MAX},
        "schema_digest": digest,
        "schema_digest_error": digest_error,
        "checks": [c.as_dict() for c in checks],
        "capabilities": {
            "asr": ["faster-whisper"] if _has("faster_whisper") else [],
            "vad": ["silero-onnx"] if _has("onnxruntime") and _vad_model_path() else [],
            "devices": ["cpu", "cuda"] if device.cuda else ["cpu"],
            "notes": device.notes,
        },
    }


def detect_device() -> DeviceReport:
    """Probes for accelerators, degrading to CPU with a recorded reason.

    A silent fallback to CPU makes a job twenty times slower without saying so,
    which is a support problem rather than a performance one.
    """
    report = DeviceReport()

    if not _has("ctranslate2"):
        report.notes.append("ctranslate2 is not importable; ASR cannot run")
        return report

    try:
        import ctranslate2

        count = ctranslate2.get_cuda_device_count()
    except Exception as exc:
        report.notes.append(f"CUDA probe failed: {exc}")
        count = 0

    if count > 0:
        report.cuda = True
        report.selected = "cuda"
        report.compute_types = ["float16", "int8_float16", "int8", "float32"]
        return report

    # Apple Silicon has Metal, but CTranslate2 has no Metal backend: its device
    # enum is exactly {CPU, CUDA} and the macOS arm64 wheel links only
    # Accelerate.framework, a CPU BLAS. Reported so the capability endpoint can
    # tell the truth rather than implying an accelerator exists.
    if sys.platform == "darwin" and platform.machine() == "arm64":
        report.mps = True
        report.notes.append(
            "Apple Silicon: CTranslate2 has no Metal backend; ASR runs on CPU. "
            "An MLX Whisper backend is the only GPU path on this hardware."
        )

    return report


def _has(module: str) -> bool:
    """Reports whether a module is importable without importing it.

    ``find_spec`` imports parent packages but not the module itself, so this is
    cheap for top-level modules — which matters because importing ctranslate2
    costs hundreds of milliseconds and the self-check runs on every start.
    """
    try:
        return importlib.util.find_spec(module) is not None
    except (ImportError, ValueError):
        # A module can be present but raise during parent import; treat that as
        # unavailable rather than letting the check itself crash.
        return False


def _check_package(distribution: str, module: str) -> Check:
    if not _has(module):
        return Check(f"import:{module}", ok=False, error=f"No module named {module!r}")
    try:
        return Check(f"import:{module}", ok=True, value=package_version(distribution))
    except PackageNotFoundError:
        # Importable but with no distribution metadata: a source checkout on
        # PYTHONPATH, most likely. Not a failure.
        return Check(f"import:{module}", ok=True, value="unknown")


def _vad_model_path() -> Path | None:
    """Locates the Silero VAD ONNX model shipped inside faster-whisper."""
    spec = None
    try:
        spec = importlib.util.find_spec("faster_whisper")
    except (ImportError, ValueError):
        return None
    if spec is None or not spec.origin:
        return None

    candidate = Path(spec.origin).parent / VAD_MODEL_RELATIVE
    return candidate if candidate.is_file() else None


def _check_vad_model() -> Check:
    if not _has("faster_whisper"):
        return Check("vad_model", ok=False, error="faster-whisper is not importable")

    path = _vad_model_path()
    if path is None:
        return Check(
            "vad_model",
            ok=False,
            error=f"{VAD_MODEL_RELATIVE} not found inside the faster-whisper package",
        )
    return Check("vad_model", ok=True, value=str(path))


def _check_ffmpeg() -> Check:
    path = shutil.which("ffmpeg")
    if path is None:
        return Check(
            "ffmpeg",
            ok=False,
            error="ffmpeg not found on PATH; install it, or set ai.ffmpeg in the configuration",
        )
    return Check("ffmpeg", ok=True, value=path)


def _schema_digest_safe() -> tuple[str | None, str | None]:
    """Computes the schema digest, reporting failure rather than raising.

    A missing digest is a real problem but not a reason to refuse to report
    everything else, and the core can still start with a warning.
    """
    try:
        from nikucooker_ai.protocol.schema import schema_digest

        return schema_digest(), None
    except Exception as exc:
        return None, str(exc)
