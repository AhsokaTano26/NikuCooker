"""Entry point for the NikuCooker AI worker.

    python -m nikucooker_ai --selfcheck   report environment health, then exit
    python -m nikucooker_ai --version     report build and protocol versions
    python -m nikucooker_ai               serve requests on stdin/stdout

Exit codes carry one meaning only: zero means a report was produced. Whether the
environment is healthy is in the report's ``ok`` field, so a caller never has to
distinguish "unhealthy" from "crashed" by exit code.
"""

from __future__ import annotations

import argparse
import json
import os
import platform
import sys
from collections.abc import Sequence
from typing import IO

from nikucooker_ai import SPEAKS_PROTOCOL_MAX, SPEAKS_PROTOCOL_MIN, __version__
from nikucooker_ai.protocol import PROTOCOL_VERSION
from nikucooker_ai.protocol.codec import ProtocolWriter
from nikucooker_ai.protocol.errors import ErrorCode, ProtocolError

#: Emitted when a report could not be produced at all.
EXIT_OK = 0
EXIT_UNUSABLE = 2


def _claim_stdout() -> IO[bytes]:
    """Takes private ownership of the real stdout for protocol use.

    Hugging Face downloads, tqdm progress bars and CTranslate2 itself all print
    to stdout, and a single stray line corrupts the message stream. Rather than
    hoping no dependency ever prints, stdout is duplicated to a private handle
    and file descriptor 1 is pointed at stderr — after which a library printing
    to stdout is harmless, and only the returned handle reaches the core.

    This is the only way to make the "stdout carries protocol messages and
    nothing else" rule structural instead of a convention that intermittent,
    hard-to-reproduce corruption punishes.
    """
    try:
        fd = os.dup(1)
    except OSError as exc:  # stdout closed or otherwise unusable
        print(
            f"nikucooker-ai: cannot duplicate stdout ({exc}); "
            "protocol output is unprotected against library writes",
            file=sys.stderr,
        )
        return sys.stdout.buffer

    os.dup2(2, 1)
    sys.stdout = sys.stderr
    return os.fdopen(fd, "wb")


def _emit(stream: IO[bytes], payload: dict[str, object]) -> None:
    """Writes one JSON line and flushes it."""
    line = json.dumps(payload, ensure_ascii=False, separators=(",", ":"), sort_keys=True)
    stream.write(line.encode("utf-8") + b"\n")
    stream.flush()


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="nikucooker-ai",
        description="NikuCooker AI inference worker. Normally spawned by the Go core.",
    )
    parser.add_argument(
        "--selfcheck",
        action="store_true",
        help="report environment health as one JSON line, then exit",
    )
    parser.add_argument(
        "--version",
        action="store_true",
        help="report build and protocol versions as one JSON line, then exit",
    )
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    protocol_out = _claim_stdout()
    args = _build_parser().parse_args(argv)

    if args.version:
        _emit(
            protocol_out,
            {
                "v": PROTOCOL_VERSION,
                "worker": "nikucooker-ai",
                "worker_version": __version__,
                "python": platform.python_version(),
                "platform": f"{sys.platform}-{platform.machine()}",
                "protocol": {"min": SPEAKS_PROTOCOL_MIN, "max": SPEAKS_PROTOCOL_MAX},
            },
        )
        return EXIT_OK

    if args.selfcheck:
        from nikucooker_ai.runtime.selfcheck import run_selfcheck

        report = run_selfcheck()
        _emit(protocol_out, report)
        return EXIT_OK

    # Worker mode. The request loop is the next phase; until it exists, say so
    # explicitly rather than accepting requests and failing on each one.
    ProtocolWriter(protocol_out).write_fatal(
        ProtocolError.make(
            ErrorCode.UNSUPPORTED_METHOD,
            "this build has no request loop; only --selfcheck and --version are implemented",
            retryable=False,
            details={"phase": "phase-0"},
        )
    )
    return EXIT_UNUSABLE


if __name__ == "__main__":
    sys.exit(main())
