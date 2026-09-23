# NikuCooker

**English** · [中文](README.zh-CN.md)

**NikuCooker is a local-first AI fansubbing pipeline.** Speech recognition and
media processing run entirely on your machine; audio and video never leave it.
Only subtitle text is sent to an LLM, and only to the provider you configure.

## What it does

```
media probe → audio extract → voice detection → speech recognition
→ subtitle segmentation → context analysis → LLM translation
→ quality control → subtitle generation → video render
```

Every stage is implemented and runs end to end: point it at a Japanese video and
it produces translated `.srt` and `.ass` files, a quality report, and a
subtitled video. Targeting **Japanese → Simplified Chinese** first (anime,
seiyuu programmes, live MC, interviews), with language treated as data so other
pairs are additions rather than rewrites.

Two properties shape the design more than any other:

- **Nothing is recomputed for free.** Every stage is content-addressed, and
  translation is cached per line. Editing three lines and re-running
  re-translates three lines; changing a prompt or a glossary entry invalidates
  exactly what it affects.
- **Your edits outrank the machine.** A line you corrected is never overwritten
  by a later run, and a re-render uses the lines as you left them.

## Quick start

```bash
make ai-install                  # create the Python environment
make build                       # build the web application and the binary
./nikucooker doctor              # check that this machine can run the pipeline

./nikucooker project create --source /path/to/episode01.mkv
./nikucooker run <project-id>
./nikucooker serve               # then open http://localhost:8080
```

`nikucooker doctor` is the first thing to run on a new machine: it reports every
prerequisite and, when something is missing, what to do about it.

Translation needs a language model. Add an OpenAI-compatible provider through the
web interface, or set it directly:

```yaml
translation:
  base_url: https://api.example.com/v1
  api_key: sk-...
  model: some-model
```

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

For a release binary, the only prerequisite is **FFmpeg** and **ffprobe** on
`PATH`; the AI environment installs itself — see [INSTALL.md](INSTALL.md).

For a checkout, which is to say for building or working on the code:

- **Go** 1.26 or newer
- **Python** 3.12–3.14, managed with [uv](https://docs.astral.sh/uv/)
- **Node** 24+ and **pnpm** 10+ (only to build the web interface)
- **FFmpeg** and **ffprobe** on `PATH`

Speech recognition runs on CPU everywhere, or on an NVIDIA GPU. The CUDA
libraries come from the install, so the machine itself needs only a driver.
Apple Silicon runs on CPU: CTranslate2 has no Metal backend, and `doctor` says so
rather than silently falling back.

## Building

```bash
make build          # web application, then the Go binary with it embedded
./nikucooker version
```

`make help` lists every target. `make build-go` skips the frontend and embeds
whatever is already in `web/dist`.

## Running on a server

There is no container image. The release binary is the deployment: it carries
the web interface, the worker's source and the installer, and it builds its own
Python environment on the machine it runs on — which is why nothing here needs
a build toolchain or a Python toolchain.

```bash
./nikucooker serve --host 127.0.0.1 --port 8080
```

It binds to loopback by default, and **this build has no authentication**.
Binding it to a public interface puts a program that reads your files and runs
jobs on the network, so reach it through a reverse proxy that authenticates, or
over an SSH tunnel:

```bash
ssh -L 8080:127.0.0.1:8080 the-server
```

Run it under whatever supervises services on that machine — systemd, a launchd
agent, or a Windows service. It stops cleanly on `SIGTERM`, and the interface
can stop it as well.

## Installing a release binary

Every tagged release publishes a binary for Windows, macOS and Linux, with the
web interface already embedded in it — no Go, Node or build toolchain needed.

The archive also carries the AI worker's source and a copy of `uv`. The Python
interpreter and the AI dependencies they need are installed once, from the
System page of the interface, by clicking a button that says how much it will
download and where it will put it. On a machine with an NVIDIA card the same
page offers the GPU set beside the default — about 700 MB more, and the option
is withheld with a reason where it could not work.

See **[INSTALL.md](INSTALL.md)** for the platform steps.

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

## Configuration

Settings come from five layers, lowest precedence first:

```
defaults → config file → environment → project overlay → CLI flags
```

The defaults are a complete, working configuration; a config file is only needed
to change something. `NIKUCOOKER_CONFIG` names the file to read, and the
individual `NIKUCOOKER_*` variables set single values — which is how a
deployment sets one without writing a file.

## A note on the Python dependency set

`faster-whisper` ships its Silero VAD model inside its own wheel and runs it
through `onnxruntime`, which it already depends on. The `silero-vad` package is
therefore deliberately **not** a dependency: adding it would pull in PyTorch and
add roughly 2.5 GB for no capability gain.

## License

MIT — see [LICENSE](LICENSE).
