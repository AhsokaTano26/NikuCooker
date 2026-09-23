# Installing NikuCooker

**English** · [中文](INSTALL.zh-CN.md)

How to install a **release binary** on Windows, macOS or Linux. For Docker, for
building from source, and for what the program actually does, see the
[README](README.md).

Every tagged release publishes a binary with the web interface already embedded
in it, so there is no Go, Node or build toolchain to install. Take the archive
for your platform from [Releases](https://github.com/AhsokaTano26/NikuCooker/releases):

| Platform | Asset |
|----------|-------|
| macOS, Apple Silicon | `nikucooker_<version>_darwin_arm64.tar.gz` |
| macOS, Intel | `nikucooker_<version>_darwin_amd64.tar.gz` |
| Windows 10/11, x86_64 | `nikucooker_<version>_windows_amd64.zip` |
| Linux, x86_64 | `nikucooker_<version>_linux_amd64.tar.gz` |
| Linux, arm64 | `nikucooker_<version>_linux_arm64.tar.gz` |

There is no Windows arm64 build: the Python packages the worker needs have no
arm64 Windows stack in practice, and a binary that cannot find a worker is a
promise we could not keep.

## Two things are not bundled

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

## macOS

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

## Windows

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

## Linux

```bash
tar xzf nikucooker_<version>_linux_amd64.tar.gz
./nikucooker doctor
./nikucooker serve
```

The binary is statically linked and built without cgo, so it does not depend on
the host's libc and runs on any distribution. Both `amd64` and `arm64` are
published.

On a server, [Docker](README.md#docker) remains the recommended path: it brings
FFmpeg and the Python worker with it, and neither is bundled here.

## Where it puts your files

| | macOS | Windows |
|---|---|---|
| Data and config | `~/Library/Application Support/NikuCooker` | `%LOCALAPPDATA%\NikuCooker` |
| Models | `<data>/models` | `<data>/models` |

On Linux, data and config follow the XDG directories
(`$XDG_DATA_HOME/nikucooker`, `$XDG_CONFIG_HOME/nikucooker`).

`--data-dir` or `NIKUCOOKER_DATA_DIR` overrides the first. Models live inside
the data directory rather than a cache directory, because a downloaded
`large-v3` is several gigabytes of user-visible state that a cleanup tool should
not silently evict. `doctor` prints every path, so "where did it put my files"
is answered before it is asked.
