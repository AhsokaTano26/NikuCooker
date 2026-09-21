"""Schema digest, mirroring pkg/protocol/schema.go.

The digest is computed from the golden fixtures and reported in the ready
handshake. The core compares it against its own and refuses to start on a
mismatch, which turns the most common support problem in this architecture — a
user upgrades the Go binary and forgets to reinstall the Python environment —
into one precise message.
"""

from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path

from nikucooker_ai.protocol.codec import canonical_json

#: Excluded from the digest it records; including it would be circular.
MANIFEST_NAME = "manifest.json"

_cached: str | None = None


def testdata_dir() -> Path | None:
    """Locates the shared fixture directory, or ``None`` when it is not present.

    The fixtures live in the Go module (pkg/protocol/testdata) and are read across
    the language boundary rather than duplicated. That works from a source
    checkout, which is where the worker is developed and tested; an installed
    wheel has no such directory, which is what the manifest fallback covers.
    """
    override = os.environ.get("NIKUCOOKER_PROTOCOL_TESTDATA")
    if override:
        candidate = Path(override)
        return candidate if candidate.is_dir() else None

    # .../ai/nikucooker_ai/protocol/schema.py -> repository root
    root = Path(__file__).resolve().parents[3]
    candidate = root / "pkg" / "protocol" / "testdata"
    return candidate if candidate.is_dir() else None


def compute_schema_digest() -> str:
    """Derives the digest from the fixtures.

    Computed rather than read from the manifest so it cannot drift from the files
    it describes. The algorithm matches Go exactly:

        sha256( for each fixture, sorted by name, excluding the manifest:
                    name + NUL + canonical_json + NUL )
    """
    directory = testdata_dir()
    if directory is None:
        raise FileNotFoundError(
            "protocol fixtures not found; set NIKUCOOKER_PROTOCOL_TESTDATA "
            "or run from a source checkout"
        )

    names = sorted(
        p.name for p in directory.iterdir() if p.suffix == ".json" and p.name != MANIFEST_NAME
    )
    if not names:
        raise FileNotFoundError(f"no fixtures found in {directory}")

    digest = hashlib.sha256()
    for name in names:
        raw = (directory / name).read_bytes()
        # Canonicalise first: reformatting a fixture must not change the digest,
        # or every whitespace-only edit would look like a wire-format change.
        canonical = canonical_json(json.loads(raw))
        digest.update(name.encode("utf-8"))
        digest.update(b"\x00")
        digest.update(canonical)
        digest.update(b"\x00")

    return "sha256:" + digest.hexdigest()


def schema_digest() -> str:
    """Returns the digest, preferring a computed value over the recorded one.

    Falls back to the manifest's recorded digest when the fixtures are not
    reachable, which is the installed-wheel case. The fallback is strictly weaker
    — it cannot detect a corrupt fixture set — so it is a last resort rather than
    the default path.
    """
    global _cached
    if _cached is not None:
        return _cached

    try:
        _cached = compute_schema_digest()
    except FileNotFoundError:
        _cached = _read_manifest_digest()
    return _cached


def _read_manifest_digest() -> str:
    directory = testdata_dir()
    if directory is None:
        raise FileNotFoundError("protocol fixtures and manifest are both unreachable") from None

    manifest = json.loads((directory / MANIFEST_NAME).read_text(encoding="utf-8"))
    digest = manifest.get("schema_digest")
    if not isinstance(digest, str) or not digest:
        raise ValueError(f"{MANIFEST_NAME} has no schema_digest")
    return digest


def _reset_cache() -> None:
    """Clears the memo. For tests that need a fresh computation."""
    global _cached
    _cached = None
