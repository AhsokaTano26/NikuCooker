# NikuCooker

NikuCooker 是一个面向视频字幕制作的全自动 AI 处理流水线，目标是将未经翻译的“生肉”视频自动处理为带有高质量中文字幕的“熟肉”视频。

**NikuCooker is a local-first AI fansubbing pipeline.** Speech recognition and
media processing run entirely on your machine; audio and video never leave it.
Only subtitle text is sent to an LLM, and only to the provider you configure.

> ## Status: Phase 0 — not usable yet
>
> This repository currently contains a skeleton: the Go module and CLI shell,
> the cross-language protocol types, the Python worker's entry point and
> self-check, and the Vue application shell. **No stage, no API endpoint and no
> pipeline is implemented.** Running the binary today produces version output
> and nothing else.
>
> The roadmap puts a working `nikucooker run video.mp4` at Phase 6. Until then,
> nothing here will transcribe or translate anything.

## What it will do

```
media probe → audio extract → voice detection → speech recognition
→ subtitle segmentation → context analysis → LLM translation
→ quality control → subtitle generation → video render
```

Targeting **Japanese → Simplified Chinese** first (anime, seiyuu programmes,
live MC, interviews), with language treated as data so other pairs are additions
rather than rewrites.

## Architecture, briefly

Three languages, with strict boundaries:

| | Responsibility |
|---|---|
| **Go** | The system: API, pipeline, jobs, artifact cache, database, FFmpeg, LLM calls, Python worker lifecycle |
| **Python** | Local inference only: speech recognition, voice activity detection |
| **Vue 3** | The interface. A client of the Go API and nothing else |

LLM translation lives in Go rather than Python on purpose: calling an HTTP API
needs retry policies, timeouts, concurrency limits, caching and provider
abstraction, which is systems work. Python exists here solely because
`faster-whisper` is Python.

Requirements and design are maintained locally and are not published in this
repository. What is published is the code and this file.

## Requirements

- **Go** 1.25 or newer
- **Python** 3.12–3.14, managed with [uv](https://docs.astral.sh/uv/)
- **Node** 24+ and **pnpm** 10+ (only to build the web interface)
- **FFmpeg** and **ffprobe** on `PATH`

Speech recognition runs on CPU everywhere, or on an NVIDIA GPU with CUDA 12.x.
Apple Silicon runs on CPU: CTranslate2 has no Metal backend.

## Building

```bash
make build          # web application, then the Go binary with it embedded
./nikucooker version
```

`make help` lists every target. `make build-go` skips the frontend and embeds
whatever is already in `web/dist`.

## Docker

The recommended path for a server, or for anyone who would rather not manage a
Python environment:

```bash
docker compose up -d              # CPU
docker compose --profile cuda up -d   # NVIDIA GPU
```

Then open <http://localhost:8080>.

The CUDA image is a separate build because it is several gigabytes larger; CPU
users are never made to download it.

## Development

```bash
make ai-install     # create the Python environment from uv.lock
make ai-selfcheck   # report whether that environment is usable
make web-install
make test           # Go, Python and frontend suites
make lint           # gofmt, go vet, ruff, ESLint, vue-tsc
```

`make web-dev` runs the Vite dev server and proxies `/api` to a core on
`127.0.0.1:8080`.

## A note on the Python dependency set

`faster-whisper` ships its Silero VAD model inside its own wheel and runs it
through `onnxruntime`, which it already depends on. The `silero-vad` package is
therefore deliberately **not** a dependency: adding it would pull in PyTorch and
add roughly 2.5 GB for no capability gain.

## License

MIT — see [LICENSE](LICENSE).
