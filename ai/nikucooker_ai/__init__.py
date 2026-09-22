"""NikuCooker AI worker.

A stateless inference process spawned and supervised by the Go core. It receives
file paths and configuration over a newline-delimited JSON protocol on stdin and
returns structured results on stdout. It holds no project state, opens no
database, and has no opinion about what a job is.

See docs/ipc-protocol.md for the protocol and ARCHITECTURE.md for the boundary.
"""

import os as _os

__version__ = "0.1.0"

# Disable onnxruntime's telemetry before anything imports it.
#
# Two reasons, and the second is the one that forced it. First, this project's
# premise is that nothing leaves the machine unless the user configured a remote
# translator — a runtime that collects usage events contradicts that directly.
# Second, onnxruntime 1.30's telemetry *creates a file literally named
# ":memory:.ses" in the current working directory* on every run, which is junk
# in a user's project folder and in this repository.
#
# Set here rather than in the worker because this module is imported before any
# dependency, so the variable is in place before the library reads it.
_os.environ.setdefault("ORT_DISABLE_TELEMETRY", "1")

# SPEAKS_PROTOCOL is the range of protocol versions this build understands. The
# core compares it against its own during the ready handshake; a build that
# overlaps nothing is refused rather than guessed at.
SPEAKS_PROTOCOL_MIN = 1
SPEAKS_PROTOCOL_MAX = 1
