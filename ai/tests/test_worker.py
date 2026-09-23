"""Worker request-loop tests.

These cover what the Go tests cannot observe from outside the process: that the
stdout discipline actually holds, and that the control path really is answered
while a data request is running.

The end-to-end behaviour — crash recovery, stall detection, cancellation
reaching a live worker — is driven from `tests/phase2_test.go`, which spawns a
real process.
"""

from __future__ import annotations

import contextlib
import io
import json
import os
import sys
import threading
import time

import pytest
from nikucooker_ai.__main__ import _claim_stdout
from nikucooker_ai.worker import QUEUE_DEPTH, Worker, is_control_method

# ---------------------------------------------------------------------------
# stdout discipline
# ---------------------------------------------------------------------------


def test_claim_stdout_isolates_the_protocol_stream():
    """The protocol stream must carry protocol messages and nothing else.

    This is the property the whole IPC design rests on, and the one that fails
    intermittently in the field: a library that prints during a request injects
    a line the core cannot parse, and the symptom appears far from the cause.

    Driving it through the real `_claim_stdout` is the point — a test that
    reimplemented the redirect would prove nothing about the shipped code.
    """
    saved_fd1, saved_fd2 = os.dup(1), os.dup(2)
    saved_stdout, saved_stderr = sys.stdout, sys.stderr

    protocol_r, protocol_w = os.pipe()
    noise_r, noise_w = os.pipe()

    try:
        # Emulate a real process: fd 1 and fd 2 are pipes, and sys.stdout and
        # sys.stderr are the objects wrapping them. Replacing only the file
        # descriptors would leave print() writing to whatever pytest installed,
        # and the test would prove nothing.
        os.dup2(protocol_w, 1)
        os.dup2(noise_w, 2)
        sys.stdout = os.fdopen(os.dup(protocol_w), "w")
        sys.stderr = os.fdopen(os.dup(noise_w), "w")

        protocol = _claim_stdout()

        # The three ways a dependency might write.
        print("noisy library")
        sys.stdout.write("also noisy\n")
        sys.stdout.flush()
        os.write(1, b"raw fd write\n")

        protocol.write(b'{"v":1,"id":"req_1","type":"result","result":{}}\n')
        protocol.flush()

        # Every write end closed before the readers look for EOF: the claimed
        # handle, the descriptors they were dup'd onto, the file objects
        # wrapping them, and the pipe ends os.pipe() returned. Leaving any one
        # open is a read that never finishes.
        protocol.close()
        sys.stdout.flush()
        sys.stderr.flush()
        os.close(1)
        os.close(2)
        sys.stdout.close()
        sys.stderr.close()
        os.close(protocol_w)
        os.close(noise_w)

        leaked = _read_all(protocol_r)
        noise = _read_all(noise_r)
    finally:
        os.dup2(saved_fd1, 1)
        os.dup2(saved_fd2, 2)
        os.close(saved_fd1)
        os.close(saved_fd2)
        sys.stdout, sys.stderr = saved_stdout, saved_stderr
        for fd in (protocol_r, protocol_w, noise_r, noise_w):
            # Some are already closed above, and double-closing is harmless but
            # raises.
            with contextlib.suppress(OSError):
                os.close(fd)

    # The protocol handle reaches the original stdout, and only protocol
    # messages travel on it.
    assert leaked == b'{"v":1,"id":"req_1","type":"result","result":{}}\n', (
        f"the protocol stream carried something other than protocol: {leaked!r}"
    )

    # Everything a library wrote went to stderr, including a raw write to fd 1.
    for expected in (b"noisy library", b"also noisy", b"raw fd write"):
        assert expected in noise, f"{expected!r} did not reach stderr: {noise!r}"


def _read_all(fd: int) -> bytes:
    """Reads a pipe to EOF.

    Every write end is closed before this is called, so EOF is what ends the
    loop. There is no select() here, and that is not a simplification: select
    on Windows takes sockets and refuses a pipe, with WinError 10038 — "an
    operation was attempted on something that is not a socket" — which is how
    this test failed there while passing everywhere else.
    """
    chunks: list[bytes] = []
    while True:
        chunk = os.read(fd, 65536)
        if not chunk:
            break
        chunks.append(chunk)
    return b"".join(chunks)


# ---------------------------------------------------------------------------
# Request handling
# ---------------------------------------------------------------------------


def run_worker(requests: list[dict], *, timeout: float = 10.0) -> list[dict]:
    """Feeds requests to a worker and returns the events it emitted.

    The whole request list is available up front, so this exercises dispatch
    order rather than the transport. Anything that depends on timing between two
    requests is driven from the Go suite instead.
    """
    body = b"".join(json.dumps(r).encode() + b"\n" for r in requests)
    out = io.BytesIO()

    worker = Worker(out)

    thread = threading.Thread(target=lambda: worker.run(io.BytesIO(body)), daemon=True)
    thread.start()
    thread.join(timeout=timeout)
    assert not thread.is_alive(), "the worker did not finish within the timeout"

    return [json.loads(line) for line in out.getvalue().splitlines() if line.strip()]


def events_of(events: list[dict], etype: str) -> list[dict]:
    return [e for e in events if e.get("type") == etype]


def test_ready_is_announced_first():
    events = run_worker([{"v": 1, "id": "req_1", "method": "shutdown"}])

    assert events, "the worker emitted nothing"
    assert events[0]["type"] == "ready"
    assert events[0]["protocol"] == {"min": 1, "max": 1}
    assert events[0]["capabilities"]["devices"], "no devices reported"


def test_echo_returns_its_params_unchanged():
    events = run_worker(
        [
            {"v": 1, "id": "req_1", "method": "echo", "params": {"a": 1, "b": "こんにちは"}},
            {"v": 1, "id": "req_2", "method": "shutdown"},
        ]
    )

    results = events_of(events, "result")
    echo = next(r for r in results if r["id"] == "req_1")
    assert echo["result"] == {"a": 1, "b": "こんにちは"}


def test_unknown_method_is_reported_not_fatal():
    events = run_worker(
        [
            # A method that does not exist, rather than one that does not exist
            # yet: naming a real-but-unimplemented method makes this test expire
            # the moment that method lands, which it did once.
            {"v": 1, "id": "req_1", "method": "definitely.not.a.method", "params": {}},
            {"v": 1, "id": "req_2", "method": "echo", "params": {}},
            {"v": 1, "id": "req_3", "method": "shutdown"},
        ]
    )

    errors = events_of(events, "error")
    assert [e["id"] for e in errors] == ["req_1"]
    assert errors[0]["error"]["code"] == "UNSUPPORTED_METHOD"

    # The worker kept serving: one unknown method must not take down the
    # process, because the core would then have to restart it mid-job.
    assert "req_2" in {r["id"] for r in events_of(events, "result")}


def test_malformed_request_is_answered_and_the_worker_survives():
    """A bad request costs that request, not the worker.

    The id has to be recovered from the raw line: without it there is nothing to
    report the failure against, and the only remaining option would be to kill
    the process.
    """
    body = b"".join(
        [
            b'{"v":1,"id":"req_bad","method":"echo","params":{"x":}}\n',
            b'{"v":1,"id":"req_ok","method":"echo","params":{}}\n',
            b'{"v":1,"id":"req_stop","method":"shutdown"}\n',
        ]
    )
    out = io.BytesIO()
    Worker(out).run(io.BytesIO(body))

    events = [json.loads(line) for line in out.getvalue().splitlines() if line.strip()]
    errors = events_of(events, "error")
    assert [e["id"] for e in errors] == ["req_bad"]
    assert errors[0]["error"]["code"] == "INVALID_PARAMS"

    assert "req_ok" in {r["id"] for r in events_of(events, "result")}


def test_control_methods_bypass_the_data_queue():
    """`health` is answered while a data request is still running.

    A cancel that queued behind the transcription it is meant to stop would be
    useless, and this is the cheapest place to prove the split exists.
    """
    events = run_worker(
        [
            {"v": 1, "id": "req_slow", "method": "debug.delay", "params": {"seconds": 0.4}},
            {"v": 1, "id": "req_health", "method": "health"},
            {"v": 1, "id": "req_stop", "method": "shutdown"},
        ]
    )

    order = [e["id"] for e in events if e.get("type") == "result"]
    assert order.index("req_health") < order.index("req_slow"), (
        f"health waited for the data request: {order}"
    )


def test_cancel_reaches_a_running_request():
    """A cancellation is honoured, and reported as CANCELLED rather than as a
    generic failure: a user who cancels should not see the job as broken."""
    out = io.BytesIO()
    worker = Worker(out)

    # A pipe rather than a pre-loaded buffer: the cancel must arrive while the
    # delay is genuinely running, which is the whole point of the control path.
    read_fd, write_fd = os.pipe()
    stream = os.fdopen(read_fd, "rb")

    thread = threading.Thread(target=lambda: worker.run(stream), daemon=True)
    thread.start()

    def send(payload: dict) -> None:
        os.write(write_fd, json.dumps(payload).encode() + b"\n")

    send({"v": 1, "id": "req_slow", "method": "debug.delay", "params": {"seconds": 30}})
    # Let the data thread pick the request up before the cancel arrives.
    time.sleep(0.3)

    send({"v": 1, "id": "req_cancel", "method": "cancel", "params": {"target_id": "req_slow"}})
    time.sleep(0.3)
    send({"v": 1, "id": "req_stop", "method": "shutdown"})

    thread.join(timeout=10)
    os.close(write_fd)
    assert not thread.is_alive(), "the worker did not stop"

    events = [json.loads(line) for line in out.getvalue().splitlines() if line.strip()]
    errors = events_of(events, "error")
    assert [e["id"] for e in errors] == ["req_slow"]
    assert errors[0]["error"]["code"] == "CANCELLED"
    assert errors[0]["error"]["retryable"] is False

    # The cancel's acknowledgement follows the target's terminal event, so a
    # returned ack means the request is genuinely finished.
    order = [e["id"] for e in events if e.get("type") in ("result", "error")]
    assert order.index("req_slow") < order.index("req_cancel")


def test_cancelling_an_unknown_request_is_not_an_error():
    events = run_worker(
        [
            {"v": 1, "id": "req_1", "method": "cancel", "params": {"target_id": "nope"}},
            {"v": 1, "id": "req_2", "method": "shutdown"},
        ]
    )

    result = next(r for r in events_of(events, "result") if r["id"] == "req_1")
    assert result["result"] == {"cancelled": False}
    assert not events_of(events, "error")


def test_debug_delay_rejects_an_out_of_range_duration():
    events = run_worker(
        [
            {"v": 1, "id": "req_1", "method": "debug.delay", "params": {"seconds": 100000}},
            {"v": 1, "id": "req_2", "method": "shutdown"},
        ]
    )
    errors = events_of(events, "error")
    assert errors[0]["error"]["code"] == "INVALID_PARAMS"


# ---------------------------------------------------------------------------
# Classification
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("method", "control"),
    [
        ("echo", True),
        ("health", True),
        ("capabilities", True),
        ("cancel", True),
        ("shutdown", True),
        ("asr.transcribe", False),
        ("vad.detect", False),
        ("model.load", False),
        ("definitely.not.a.method", False),
    ],
)
def test_method_classification(method: str, control: bool):
    """Which path a method takes is the difference between a working cancel and
    a useless one, so it is asserted rather than assumed."""
    assert is_control_method(method) is control


def test_queue_depth_is_a_backstop_not_a_buffer():
    # The core keeps one data request in flight per worker; reaching the queue
    # limit means the core's accounting is wrong, and failing loudly is right.
    assert QUEUE_DEPTH >= 2
