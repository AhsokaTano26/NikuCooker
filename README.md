# NikuCooker

NikuCooker 是一个面向视频字幕制作的全自动 AI 处理流水线，目标是将未经翻译的“生肉”视频自动处理为带有高质量中文字幕的“熟肉”视频。

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

- **Go** 1.26 or newer
- **Python** 3.12–3.14, managed with [uv](https://docs.astral.sh/uv/)
- **Node** 24+ and **pnpm** 10+ (only to build the web interface)
- **FFmpeg** and **ffprobe** on `PATH`

Speech recognition runs on CPU everywhere, or on an NVIDIA GPU with CUDA 12.x.
Apple Silicon runs on CPU: CTranslate2 has no Metal backend, and `doctor` says so
rather than silently falling back.

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
docker compose up -d                        # CPU
docker compose --profile cuda up -d cuda    # NVIDIA GPU
```

Then open <http://localhost:8080>.

The CUDA image is a separate build because it is several gigabytes larger; CPU
users are never made to download it. Both mount `./data`, `./models` and
`./config`, so switching between them keeps every project, model and setting.

## Windows and macOS

Every tagged release publishes a binary with the web interface already embedded
in it, so there is no Go, Node or build toolchain to install. Take the archive
for your platform from [Releases](https://github.com/AhsokaTano26/NikuCooker/releases):

| Platform | Asset |
|----------|-------|
| macOS, Apple Silicon | `nikucooker_<version>_darwin_arm64.tar.gz` |
| macOS, Intel | `nikucooker_<version>_darwin_amd64.tar.gz` |
| Windows 10/11, x86_64 | `nikucooker_<version>_windows_amd64.zip` |

There is no Windows arm64 build: the Python packages the worker needs have no
arm64 Windows stack in practice, and a binary that cannot find a worker is a
promise we could not keep.

### Two things are not bundled

**Python.** Speech recognition runs in a Python worker, and including it would
add 400 MB–3 GB to every download. The binary runs without it — recognition is
the only thing that does not.

To install the worker, clone the repository and build its environment:

```bash
git clone https://github.com/AhsokaTano26/NikuCooker
cd NikuCooker/ai && uv sync
```

Then put the binary in that checkout's root, beside `ai/`, and it finds the
worker on its own:

```bash
cp /path/to/nikucooker /path/to/NikuCooker/
```

(Extracting the archive *into* the checkout instead would overwrite the
repository's own README and LICENSE — copy the binary, not the archive.)

Anywhere else, name the directory in the configuration file:

```yaml
ai:
  dir: C:\path\to\NikuCooker\ai
```

`nikucooker config path` says which file that is; `nikucooker config init`
writes a commented one if there is not one already.

**FFmpeg and `ffprobe`**, which must be on `PATH`. Hard subtitles additionally
need an FFmpeg built with `libass`, and a CJK font installed — without either,
burned-in subtitles render as empty boxes. `doctor` names both if they are
missing.

Run `nikucooker doctor` first. It checks all of the above and prints what to do
about anything missing, rather than failing later in the middle of a job.

### macOS

```bash
tar xzf nikucooker_<version>_darwin_arm64.tar.gz
xattr -d com.apple.quarantine ./nikucooker     # see below
./nikucooker doctor
./nikucooker serve
```

macOS only trusts binaries signed with a paid Developer ID, and these are not.
They run — the linker gives every arm64 binary a valid ad-hoc signature — but
anything a browser downloaded is quarantined and refused with *"cannot be
opened because the developer cannot be verified"*. Clearing the attribute is the
fix, or right-click the binary in Finder, choose **Open**, and confirm once.

Apple Silicon runs recognition on the CPU: CTranslate2 has no Metal backend, so
there is no GPU path here and `doctor` says so rather than quietly falling back.
`small` or `medium` models are the sensible choice.

Intel Macs are the constrained target — `onnxruntime` stopped publishing macOS
x86_64 wheels after 1.23.2, which caps the worker at Python 3.13.

### Windows

```powershell
Expand-Archive nikucooker_<version>_windows_amd64.zip -DestinationPath .
.\nikucooker.exe doctor
.\nikucooker.exe serve
```

To run it as `nikucooker` from any shell rather than `.\nikucooker.exe`, add the
folder you extracted it into to your `PATH`.

Two Windows-specific things `doctor` checks:

- **Long paths.** The 260-character `MAX_PATH` limit is easy to exceed under
  `%LOCALAPPDATA%\NikuCooker\projects\<uuid>\artifacts\...`. Either enable long
  path support in the registry (`LongPathsEnabled`), or point `--data-dir` at
  somewhere short such as `C:\niku`.
- **NVIDIA GPU.** The `cuda` extra needs a CUDA 12.x runtime providing
  `libcublas`; `doctor` test-loads it rather than assuming. Without one,
  recognition runs on the CPU.

### Where it puts your files

| | macOS | Windows |
|---|---|---|
| Data and config | `~/Library/Application Support/NikuCooker` | `%LOCALAPPDATA%\NikuCooker` |
| Models | `<data>/models` | `<data>/models` |

`--data-dir` or `NIKUCOOKER_DATA_DIR` overrides the first. Models live inside
the data directory rather than a cache directory, because a downloaded
`large-v3` is several gigabytes of user-visible state that a cleanup tool should
not silently evict. `doctor` prints every path, so "where did it put my files"
is answered before it is asked.

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
individual `NIKUCOOKER_*` variables set single values — which is how the
container image is configured without one.

## A note on the Python dependency set

`faster-whisper` ships its Silero VAD model inside its own wheel and runs it
through `onnxruntime`, which it already depends on. The `silero-vad` package is
therefore deliberately **not** a dependency: adding it would pull in PyTorch and
add roughly 2.5 GB for no capability gain.

## License

MIT — see [LICENSE](LICENSE).
