"""The worker's request loop.

A worker is serial in data work and concurrent in control work. Data methods run
one at a time on a single thread, because a GPU — and even a CPU running
CTranslate2 — is a single resource; concurrent inference on one process buys
nothing and makes memory unpredictable. Control methods are answered by the
reader thread immediately, which is what makes ``cancel`` work at all: a cancel
that queued behind the transcription it is meant to stop would be useless.

See docs/ipc-protocol.md §6.
"""

from __future__ import annotations

import argparse
import importlib.util
import json
import os
import pathlib
import platform
import queue
import re
import sys
import threading
import time
import traceback
from collections.abc import Callable
from typing import IO, Any

from nikucooker_ai import SPEAKS_PROTOCOL_MAX, SPEAKS_PROTOCOL_MIN, __version__
from nikucooker_ai.models import ModelManager
from nikucooker_ai.protocol import (
    ProtocolDecodeError,
    ProtocolError,
    ProtocolReader,
    ProtocolWriter,
    decode_request,
)
from nikucooker_ai.protocol.errors import ErrorCode, RequestFailed
from nikucooker_ai.protocol.models import (
    ASRCaps,
    ASRResult,
    ASRTranscribeParams,
    ASRTranscribeResult,
    CancelResult,
    Capabilities,
    CapabilitiesResult,
    DeviceInfo,
    HealthResult,
    LoadedModel,
    ModelLoadParams,
    ModelUnloadParams,
    ProgressEvent,
    ProtocolSpan,
    Providers,
    ReadyEvent,
    Request,
    ResultEvent,
    ShutdownResult,
    VADDetectParams,
)
from nikucooker_ai.protocol.schema import schema_digest

#: How many data requests may wait before the worker refuses more.
#:
#: The core's pool keeps at most one data request in flight per worker, so this
#: is a backstop rather than a queue anyone should reach. Reaching it means the
#: pool's accounting is wrong, and failing loudly is the right response.
QUEUE_DEPTH = 8

#: How often a cancellable handler should check its token.
CANCEL_POLL_S = 0.05

#: How long a cancellation waits for its target to actually stop.
CANCEL_SETTLE_TIMEOUT_S = 5.0


class CancelToken:
    """Signals that a request has been cancelled.

    Cooperative rather than preemptive: a handler checks it between units of
    work so that it stops at a point where its own state is consistent.
    """

    def __init__(self) -> None:
        self._event = threading.Event()

    def cancel(self) -> None:
        self._event.set()

    @property
    def cancelled(self) -> bool:
        return self._event.is_set()

    def check(self) -> None:
        """Raises if the request has been cancelled."""
        if self._event.is_set():
            raise RequestFailed(ProtocolError.make(ErrorCode.CANCELLED, "cancelled by the core"))

    def wait(self, timeout: float) -> bool:
        """Sleeps up to timeout, returning early if cancelled.

        Returns True when the wait completed normally and False when it was
        interrupted by a cancellation.
        """
        return not self._event.wait(timeout)


class Inflight:
    """A request that is currently being served.

    The ``finished`` event is what makes the cancellation ordering guarantee
    real: the core treats a cancel acknowledgement as proof that the target has
    stopped, and that is only true if the acknowledgement waits for it.
    """

    def __init__(self) -> None:
        self.token = CancelToken()
        self.finished = threading.Event()


class Worker:
    """Serves requests from stdin and writes events to a private stdout."""

    def __init__(self, out: IO[bytes]) -> None:
        self._out = ProtocolWriter(out)
        self._started = time.monotonic()

        self._loaded: list[LoadedModel] = []
        self._inflight: dict[str, Inflight] = {}
        self._lock = threading.Lock()

        # The model manager is created eagerly: it holds no models until one is
        # loaded, and deferring it would mean the first request racing its own
        # initialisation.
        self.models = ModelManager()

        self._queue: queue.Queue[Request | None] = queue.Queue(maxsize=QUEUE_DEPTH)
        self._should_stop = threading.Event()
        self._exited = threading.Event()

        self._data_thread = threading.Thread(
            target=self._data_loop, name="nikucooker-data", daemon=True
        )

    # -- lifecycle ---------------------------------------------------------

    def run(self, stream: IO[bytes]) -> int:
        """Reads requests until stdin closes or a shutdown is served."""
        self._data_thread.start()
        self._announce_ready()

        reader = ProtocolReader(stream)
        while not self._should_stop.is_set():
            try:
                line = reader.read_line()
            except ProtocolDecodeError as exc:
                # An oversized or unreadable frame desynchronises the stream,
                # so there is nothing safe to continue with.
                self._fatal(
                    ProtocolError.make(
                        ErrorCode.INTERNAL, f"protocol framing error: {exc}", retryable=False
                    )
                )
                break

            if line is None:
                break

            try:
                request = decode_request(line)
            except ProtocolDecodeError as exc:
                # A single bad request is not a reason to die. The line ended at
                # a newline, so framing is intact and the next request is still
                # readable — only an oversized line desynchronises the stream,
                # and that is handled above.
                request_id = _peek_id(line)
                if request_id is None:
                    # No id means nothing to report against; the core's request,
                    # if there was one, will fail on its own timeout. Still not a
                    # reason to take down the process.
                    print(
                        f"nikucooker-ai: discarding an unidentifiable request: {exc}",
                        file=sys.stderr,
                    )
                    continue
                self._out.write_error(
                    request_id, ProtocolError.make(ErrorCode.INVALID_PARAMS, str(exc))
                )
                continue

            self._dispatch(request)

        self._should_stop.set()
        self._queue.put(None)
        self._exited.wait(timeout=5)
        return 0

    def _announce_ready(self) -> None:
        digest = ""
        try:
            digest = schema_digest()
        except Exception as exc:
            print(f"nikucooker-ai: schema digest unavailable: {exc}", file=sys.stderr)

        capabilities = _capabilities()
        self._out.write(
            ReadyEvent(
                worker_version=__version__,
                protocol=ProtocolSpan(min=SPEAKS_PROTOCOL_MIN, max=SPEAKS_PROTOCOL_MAX),
                schema_digest=digest,
                python=platform.python_version(),
                platform=f"{sys.platform}-{platform.machine()}",
                capabilities=capabilities,
            )
        )

    def _fatal(self, error: ProtocolError) -> None:
        self._out.write_fatal(error)
        self._should_stop.set()

    # -- dispatch ----------------------------------------------------------

    def _dispatch(self, request: Request) -> None:
        """Routes a request to the control path or the data queue."""
        if not is_control_method(request.method):
            try:
                self._queue.put_nowait(request)
            except queue.Full:
                self._out.write_error(
                    request.id,
                    ProtocolError.make(
                        ErrorCode.INTERNAL,
                        "the worker's request queue is full; the core sent more "
                        "concurrent work than one worker accepts",
                        details={"queue_depth": QUEUE_DEPTH},
                    ),
                )
            return

        # Control methods are served here, on the reader thread, so they are
        # answered even while the data thread is busy.
        result: dict[str, Any] | None = None
        try:
            result = self._run_control(request)
        except RequestFailed as exc:
            self._out.write_error(request.id, exc.error)
            return
        except Exception as exc:
            self._out.write_error(
                request.id,
                ProtocolError.make(
                    ErrorCode.INTERNAL,
                    f"{type(exc).__name__}: {exc}",
                    details={"traceback": traceback.format_exc()},
                ),
            )
            return

        if result is not None:
            self._out.write(ResultEvent(id=request.id, result=result))

    def _run_control(self, request: Request) -> dict[str, Any] | None:
        match request.method:
            case "echo":
                return dict(request.params)

            case "health":
                with self._lock:
                    loaded = list(self._loaded)
                    inflight = sorted(self._inflight)
                return HealthResult(
                    alive=True,
                    uptime_s=time.monotonic() - self._started,
                    loaded_models=loaded,
                    inflight=inflight,
                    rss_mb=_rss_mb(),
                ).model_dump(mode="json", exclude_none=True)

            case "capabilities":
                return _capabilities_result().model_dump(mode="json", exclude_none=True)

            case "cancel":
                target = str(request.params.get("target_id", ""))
                return CancelResult(cancelled=self._cancel(target)).model_dump(mode="json")

            case "shutdown":
                # Answered, then the loop stops. The reply must reach the core
                # before the process exits, which is why the ordering is
                # explicit rather than left to interpreter teardown.
                self._should_stop.set()
                return ShutdownResult(ok=True).model_dump(mode="json")

        raise RequestFailed(
            ProtocolError.make(ErrorCode.UNSUPPORTED_METHOD, f"unknown method {request.method!r}")
        )

    def _cancel(self, target_id: str) -> bool:
        """Cancels a request, waiting for it to actually stop.

        False means it had already finished. Losing that race is not a failure:
        cancellation is racy by nature, and reporting it as an error would make
        a normal outcome look like a fault.

        The wait is what upholds the protocol's ordering guarantee — the
        target's terminal event is written before the cancel's own result — and
        the core relies on it, because it treats the acknowledgement as proof
        that nothing is still holding a model or a file.

        Waiting happens on the reader thread, which briefly stops new requests
        from being read. That is the deliberate trade: a cancel is rare, it is
        what the core is waiting for, and a handler notices within one poll
        interval.
        """
        with self._lock:
            entry = self._inflight.get(target_id)
        if entry is None:
            return False

        entry.token.cancel()

        # Bounded, because a handler that ignores its token must not wedge the
        # reader thread forever. Past this point the core's own stall detection
        # is the backstop.
        if not entry.finished.wait(timeout=CANCEL_SETTLE_TIMEOUT_S):
            print(
                f"nikucooker-ai: {target_id} did not stop within "
                f"{CANCEL_SETTLE_TIMEOUT_S}s of being cancelled",
                file=sys.stderr,
            )
        return True

    # -- data path ---------------------------------------------------------

    def _data_loop(self) -> None:
        while True:
            request = self._queue.get()
            if request is None:
                self._exited.set()
                return
            self._serve_data(request)

    def _serve_data(self, request: Request) -> None:
        entry = Inflight()
        with self._lock:
            self._inflight[request.id] = entry

        try:
            handler = DATA_METHODS.get(request.method)
            if handler is None:
                raise RequestFailed(
                    ProtocolError.make(
                        ErrorCode.UNSUPPORTED_METHOD,
                        f"{request.method!r} is not implemented in this build",
                    )
                )

            result = handler(self, request, entry.token)
            if result is not None:
                self._out.write(ResultEvent(id=request.id, result=result))

        except RequestFailed as exc:
            self._out.write_error(request.id, exc.error)
        except Exception as exc:
            # An unhandled exception fails one request, not the worker. The
            # traceback is the only way a Python bug becomes debuggable from the
            # Go log without reproducing it.
            self._out.write_error(
                request.id,
                ProtocolError.make(
                    ErrorCode.INTERNAL,
                    f"{type(exc).__name__}: {exc}",
                    details={"traceback": traceback.format_exc()},
                ),
            )
        finally:
            with self._lock:
                self._inflight.pop(request.id, None)
            # Set last, and after the terminal event has been written, so a
            # cancel that is waiting here cannot return before the core has been
            # told the request is over.
            entry.finished.set()

    def emit_progress(self, request_id: str, fraction: float, message: str = "") -> None:
        """Reports progress, which is also the core's liveness signal."""
        self._out.write(
            ProgressEvent(id=request_id, progress=max(0.0, min(1.0, fraction)), message=message)
        )


# ---------------------------------------------------------------------------
# Methods
# ---------------------------------------------------------------------------

CONTROL_METHODS = frozenset({"echo", "health", "capabilities", "cancel", "shutdown"})


def is_control_method(method: str) -> bool:
    """Reports whether a method bypasses the data queue."""
    return method in CONTROL_METHODS


def _handle_debug_delay(worker: Worker, request: Request, token: CancelToken) -> dict[str, Any]:
    """Sleeps for a while, in cancellable slices.

    A diagnostic, not a feature: it is what ``nikucooker doctor`` uses to verify
    that cancellation reaches a running worker and is honoured, which is
    otherwise only observable during a twenty-minute transcription.
    """
    seconds = float(request.params.get("seconds", 1.0))
    if seconds < 0 or seconds > 600:
        raise RequestFailed(
            ProtocolError.make(ErrorCode.INVALID_PARAMS, "seconds must be between 0 and 600")
        )

    # ``silent`` reproduces a worker that is running but not reporting. It is
    # how the core's stall detection is verified: a request producing no
    # progress must be killed rather than waited on, and that path is otherwise
    # only reachable by catching a real inference hang.
    silent = bool(request.params.get("silent", False))

    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        token.check()
        if not silent:
            worker.emit_progress(
                request.id,
                min(1.0, 1 - (deadline - time.monotonic()) / seconds) if seconds else 1.0,
                "waiting",
            )
        token.wait(CANCEL_POLL_S)

    return {"slept_s": seconds, "silent": silent}


def _handle_model_load(worker: Worker, request: Request, token: CancelToken) -> dict[str, Any]:
    """Makes a model resident and reports where it landed.

    Loading is explicit rather than implicit inside transcription so that a
    multi-minute load is not hidden inside a progress bar, and so that "which
    device did it actually use" is answerable before the work starts.
    """
    params = ModelLoadParams.model_validate(request.params)
    worker.emit_progress(request.id, 0.0, f"loading {params.name}")

    result = worker.models.load(
        kind=params.kind,
        name=params.name,
        device=params.device,
        compute_type=params.compute_type,
        model_path=params.model_path,
    )
    worker.emit_progress(request.id, 1.0, f"{params.name} ready on {result.device}")
    return result.model_dump(mode="json", exclude_none=True)


def _handle_model_unload(worker: Worker, request: Request, token: CancelToken) -> dict[str, Any]:
    params = ModelUnloadParams.model_validate(request.params)
    result = worker.models.unload(kind=params.kind, name=params.name)
    return result.model_dump(mode="json", exclude_none=True)


def _handle_model_list_loaded(
    worker: Worker, request: Request, token: CancelToken
) -> dict[str, Any]:
    return worker.models.list_loaded().model_dump(mode="json", exclude_none=True)


def _handle_vad_detect(worker: Worker, request: Request, token: CancelToken) -> dict[str, Any]:
    params = VADDetectParams.model_validate(request.params)

    result = worker.models.detect_speech(
        audio_path=params.audio_path,
        threshold=params.threshold,
        min_speech_ms=params.min_speech_ms,
        min_silence_ms=params.min_silence_ms,
        speech_pad_ms=params.speech_pad_ms,
        max_speech_s=params.max_speech_s,
        progress=lambda fraction, message: worker.emit_progress(request.id, fraction, message),
        check_cancelled=token.check,
    )
    return result.model_dump(mode="json", exclude_none=True)


def _handle_asr_transcribe(worker: Worker, request: Request, token: CancelToken) -> dict[str, Any]:
    """Transcribes audio, returning segments inline or by path.

    The two forms are mutually exclusive, and the summary always says which was
    used: a caller that reads neither is a caller that silently produces an
    empty transcript.

    Compute type and model path are absent here on purpose. They belong to
    model.load, which the core sends first — so the device decision is made and
    reported once, rather than being re-derived per request and potentially
    disagreeing.
    """
    params = ASRTranscribeParams.model_validate(request.params)

    result = worker.models.transcribe(
        audio_path=params.audio_path,
        model=params.model,
        language=params.language,
        device=params.device,
        beam_size=params.beam_size,
        temperature=params.temperature,
        condition_on_previous_text=params.condition_on_previous_text,
        # Absent means "yes": word timings are what subtitle segmentation is
        # built on, so defaulting them off would produce a transcript the rest
        # of the pipeline cannot use.
        word_timestamps=True if params.word_timestamps is None else params.word_timestamps,
        initial_prompt=params.initial_prompt,
        vad_regions=params.vad_regions or [],
        progress=lambda fraction, message: worker.emit_progress(request.id, fraction, message),
        check_cancelled=token.check,
    )

    word_count = sum(len(segment.words) for segment in result.segments)
    device, compute_type = _resolved_device(worker, params.model)

    summary = ASRTranscribeResult(
        language=result.language,
        language_probability=result.language_probability,
        duration=result.duration,
        segment_count=len(result.segments),
        word_count=word_count,
        device=device,
        compute_type=compute_type,
        model=params.model,
    )

    if params.result_path:
        # The escape hatch for large results: pipe traffic stays bounded
        # regardless of media length, which a two-hour transcript would
        # otherwise exceed by a factor of thirty.
        _write_result(params.result_path, result)
        summary.result_path = params.result_path
    else:
        summary.segments = result.segments

    return summary.model_dump(mode="json", exclude_none=True)


def _resolved_device(worker: Worker, model: str) -> tuple[str, str | None]:
    """Reports the device a model is actually resident on.

    Not what was requested: a request for cuda on a machine without one lands on
    cpu, and the caller needs to know that, because it is the difference between
    a two-minute job and a forty-minute one.
    """
    for entry in worker.models.list_loaded().models:
        if entry.name == model:
            return entry.device, entry.compute_type
    return "unknown", None


def _write_result(path: str, result: ASRResult) -> None:
    """Writes the full transcript to a file for the core to read."""
    target = pathlib.Path(path)
    try:
        target.parent.mkdir(parents=True, exist_ok=True)
        # Written to a temporary name and renamed, so a reader never sees a
        # half-written transcript.
        temporary = target.with_suffix(target.suffix + ".partial")
        temporary.write_text(
            json.dumps(result.model_dump(mode="json", exclude_none=True), ensure_ascii=False),
            encoding="utf-8",
        )
        os.replace(temporary, target)
    except OSError as exc:
        raise RequestFailed(
            ProtocolError.make(
                ErrorCode.INTERNAL,
                f"could not write the transcript to {path}: {exc}",
                details={"path": path},
            )
        ) from exc


#: Data methods this build implements.
DATA_METHODS: dict[str, Callable[[Worker, Request, CancelToken], dict[str, Any] | None]] = {
    "debug.delay": _handle_debug_delay,
    "model.load": _handle_model_load,
    "model.unload": _handle_model_unload,
    "model.list_loaded": _handle_model_list_loaded,
    "vad.detect": _handle_vad_detect,
    "asr.transcribe": _handle_asr_transcribe,
}


# ---------------------------------------------------------------------------
# Host capabilities
# ---------------------------------------------------------------------------


def _capabilities_result() -> CapabilitiesResult:
    from nikucooker_ai.runtime.selfcheck import detect_device

    device = detect_device()
    has_asr = _importable("faster_whisper")
    has_vad = _importable("onnxruntime")

    return CapabilitiesResult(
        device=DeviceInfo(
            selected=device.selected,
            cuda=device.cuda,
            mps=device.mps,
            cpu_count=os.cpu_count() or 1,
        ),
        asr=ASRCaps(
            providers=["faster-whisper"] if has_asr else [],
            compute_types=device.compute_types,
        ),
        vad=Providers(providers=["silero-onnx"] if has_vad else []),
        notes=device.notes,
    )


def _capabilities() -> Capabilities:
    result = _capabilities_result()
    return Capabilities(
        asr=Providers(providers=result.asr.providers),
        vad=result.vad,
        devices=["cpu", "cuda"] if result.device.cuda else ["cpu"],
        notes=result.notes,
    )


def _importable(module: str) -> bool:
    try:
        return importlib.util.find_spec(module) is not None
    except (ImportError, ValueError):
        return False


def _rss_mb() -> float | None:
    """Reports resident memory, where the platform makes it cheap to ask.

    Windows has no ``resource`` module at all, so there is no answer to give
    there — the core reports the worker's footprint itself.
    """
    if sys.platform == "win32":
        return None

    try:
        import resource

        usage = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss
        # Linux reports kilobytes, macOS bytes. Normalising matters less than
        # being in the right order of magnitude, but a factor of 1024 is not
        # something to leave to a reader's guess.
        return usage / 1024 if sys.platform != "darwin" else usage / (1024 * 1024)
    except Exception:
        return None


def _peek_id(line: bytes) -> str | None:
    """Recovers a request id from a line that failed to validate.

    Worth the effort twice over: without an id there is nothing to report the
    failure against, so the core's request would hang until its own timeout, and
    the only other option would be to kill the worker over one bad line.
    """
    try:
        payload = json.loads(line)
    except Exception:
        # The JSON is unparseable, so read the id out of the raw bytes. The
        # pattern is deliberately narrow — a bounded, escape-free string — so it
        # cannot match something that merely looks like an id.
        match = re.search(rb'"id"\s*:\s*"([^"\\]{1,64})"', line)
        if match is None:
            return None
        return match.group(1).decode("utf-8", "replace")

    if isinstance(payload, dict):
        candidate = payload.get("id")
        if isinstance(candidate, str) and candidate:
            return candidate
    return None


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="nikucooker-ai worker")
    parser.parse_args(argv)

    worker = Worker(sys.stdout.buffer)
    return worker.run(sys.stdin.buffer)


if __name__ == "__main__":
    raise SystemExit(main())
