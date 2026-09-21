"""NikuCooker AI worker.

A stateless inference process spawned and supervised by the Go core. It receives
file paths and configuration over a newline-delimited JSON protocol on stdin and
returns structured results on stdout. It holds no project state, opens no
database, and has no opinion about what a job is.

See docs/ipc-protocol.md for the protocol and ARCHITECTURE.md for the boundary.
"""

__version__ = "0.1.0"

# SPEAKS_PROTOCOL is the range of protocol versions this build understands. The
# core compares it against its own during the ready handshake; a build that
# overlaps nothing is refused rather than guessed at.
SPEAKS_PROTOCOL_MIN = 1
SPEAKS_PROTOCOL_MAX = 1
