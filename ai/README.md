# NikuCooker AI Worker

A stateless inference process, spawned and supervised by the Go core. It
receives file paths and configuration over a newline-delimited JSON protocol on
stdin, returns structured results on stdout, and holds no project state.

It does **not** own a database, a job, an artifact, an FFmpeg call, or an LLM
request. Those live in the Go core. What lives here is PyTorch-shaped work:
speech recognition and voice activity detection.

## Setup

```bash
uv sync --extra dev
```

`uv.lock` is committed, so this reproduces an exact dependency set rather than
resolving fresh. `requires-python` is `>=3.12,<3.15`; `.python-version` pins 3.12
to match what the first-run installer provisions and what CI tests against.

### Why PyTorch is not a dependency

`faster-whisper` ships `silero_vad_v6.onnx` inside its own wheel and runs it
through `onnxruntime`, which it already depends on. The `silero-vad` package is
therefore **deliberately absent**: it declares `torch` and `torchaudio` with no
torch-free path, which would add roughly 2.5 GB and pin `torch==2.9.1` through
its `torchaudio<2.10` constraint.

Adding it back would be a regression, not a feature. See
`docs/dependency-audit.md §3`.

### Why the onnxruntime pin has an environment marker

`onnxruntime` publishes no macOS x86_64 wheel after 1.23.2, and `faster-whisper`
requires it unconditionally — so Intel Macs cap at Python 3.13 and need the older
build. Every other target gets 1.30.0. See `docs/dependency-audit.md §5`.

## Running

```bash
uv run python -m nikucooker_ai --selfcheck   # environment report, one JSON line
uv run python -m nikucooker_ai --version     # build and protocol versions
uv run python -m nikucooker_ai               # serve requests (see Status)
```

`--selfcheck` never loads a model. It reports which dependencies are importable,
whether the bundled VAD model is present, whether FFmpeg is on `PATH`, what
compute devices this host has, and the schema digest the core will compare
against. Exit code is zero whenever a report was produced; whether the
environment is *healthy* is the report's `ok` field, so a caller never has to
distinguish "unhealthy" from "crashed" by exit code.

## The stdout rule

Stdout carries protocol messages and nothing else. That is harder than it
sounds: Hugging Face downloads, `tqdm` bars and CTranslate2 itself all print to
stdout, and one stray line desynchronises the stream.

`__main__._claim_stdout` makes the rule structural rather than a convention.
It duplicates fd 1 to a private handle, points fd 1 at stderr, and reassigns
`sys.stdout`. After that, a library printing to stdout is harmless, and only the
private handle reaches the core.

**Never print to stdout in this package.** Use `sys.stderr`, or the logging
module, which is configured to write there.

## Tests

```bash
uv run pytest
```

The suite reads the golden fixtures from `pkg/protocol/testdata/` in the Go
module and checks that every message decodes into these types and re-serialises
to the same bytes. A field added on the Go side and not here — or the reverse —
fails on the side that forgot.

Tests need only `pydantic`. They never touch a model, a GPU, or the network,
which is what keeps them running on all three CI platforms.

## Status

Phase 0 of the roadmap: project skeleton, protocol types, self-check. The
request loop arrives in Phase 2; until then, serving mode emits a `fatal` event
naming that fact rather than accepting requests it cannot honour.

See `docs/roadmap.md` for what each phase delivers.
