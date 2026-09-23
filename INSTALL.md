# Installing NikuCooker

[← Back to README](README.md) · **English** · [中文](INSTALL.zh-CN.md)

How to install a **release binary** on Windows, macOS or Linux. Building from
source, running it on a server, and what the program actually does are all in the
README.

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

**Keep the whole extracted folder together.** The archive contains a `nikucooker`
binary, an `ai/` directory with the worker's source, a copy of `uv`, and a
`pkg/` directory of protocol fixtures the worker reads. The
binary looks for those beside itself, and the environment it builds later
records the absolute path it was built from — so moving the folder afterwards
means installing again.

## The AI environment

Speech recognition runs in a Python worker, and the archive deliberately does
not carry Python itself or the AI dependencies: that would add 400 MB–3 GB to
every download, including for people who never run a job. What the archive
carries is everything needed to install them.

So there are two steps, and the second one is a button:

1. Run the binary. The server starts immediately and the interface works —
   everything except recognition.
2. Open **System** (`http://localhost:8080/system`) and click
   **安装 AI 运行环境** / *Install the AI runtime*.

That downloads a Python interpreter and the dependencies — about 300 MB — into
the data directory, checks what it installed, and reports each step as it goes.
It happens once. Nothing is downloaded before you click, and the size and the
destination are both shown first.

If it fails, the page shows `uv`'s own error, and for the failures people
actually hit — a corporate proxy, a full disk, a TLS-inspecting firewall — a
line saying what to do about it. The install can be cancelled, and a cancelled
install is cleaned up so retrying works.

Once it is done, `nikucooker doctor` reports the worker as available, and the
pipeline runs.

### If you would rather install it yourself

Setting `ai.python` to an interpreter disables the button: an interpreter you
chose is the one that runs. In that case build the environment the ordinary way:

```bash
git clone https://github.com/AhsokaTano26/NikuCooker
cd NikuCooker/ai && uv sync
```

Then point the binary at it — either put the binary in that checkout's root,
beside `ai/`, where it will be found on its own:

```bash
cp /path/to/nikucooker /path/to/NikuCooker/
```

(Extracting the archive *into* the checkout instead would overwrite the
repository's own README and LICENSE — copy the binary, not the archive.)

or name the directory in the configuration file:

```yaml
ai:
  dir: C:\path\to\NikuCooker\ai
```

`nikucooker config path` says which file that is; `nikucooker config init`
writes a commented one if there is not one already.

### FFmpeg is still not bundled

**FFmpeg and `ffprobe`** must be on `PATH` — they are a system package on every
platform and are not worth duplicating. Hard subtitles additionally need an
FFmpeg built with `libass`, and a CJK font installed; without either, burned-in
subtitles render as empty boxes. `doctor` names both if they are missing, and
says how to fix them.

Run `nikucooker doctor` first. It checks all of the above and prints what to do
about anything missing, rather than failing later in the middle of a job.

### A proxy, or a firewall that inspects TLS

`HTTPS_PROXY` and `SSL_CERT_FILE` are passed through to the installer, so the
usual settings work. One thing is worth knowing: `uv` uses its own TLS stack
rather than the operating system's trust store, so a network that re-signs
certificates — common on managed laptops — fails with `invalid peer certificate:
UnknownIssuer` even though every browser on the machine is happy. Pointing
`SSL_CERT_FILE` at your organisation's CA bundle fixes it, or `UV_NATIVE_TLS=1`
makes it use the system trust store. The install page recognises this failure
and says so rather than leaving you with the raw error.

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
  `%LOCALAPPDATA%\NikuCooker\projects\<uuid>\artifacts\...`, and the AI
  environment makes it worse — a path through `runtime\venv\Lib\site-packages\`
  is already long before a project exists. **Set `--data-dir` somewhere short
  such as `C:\niku` before installing the environment**, or enable long paths in
  the registry (`LongPathsEnabled`). Moving the data directory afterwards means
  installing again.
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
published. The copy of `uv` it carries does need a glibc of 2.28 or newer, which
every distribution still receiving updates has.

On a server this is the only path: there is no container image. FFmpeg is still
yours to install, and the AI environment installs per machine — see
[running on a server](README.md#running-on-a-server).

## Where it puts your files

| | macOS | Windows |
|---|---|---|
| Data and config | `~/Library/Application Support/NikuCooker` | `%LOCALAPPDATA%\NikuCooker` |
| Models | `<data>/models` | `<data>/models` |
| AI environment | `<data>/runtime` | `<data>/runtime` |

On Linux, data and config follow the XDG directories
(`$XDG_DATA_HOME/nikucooker`, `$XDG_CONFIG_HOME/nikucooker`).

`--data-dir` or `NIKUCOOKER_DATA_DIR` overrides the first. Models live inside
the data directory rather than a cache directory, because a downloaded
`large-v3` is several gigabytes of user-visible state that a cleanup tool should
not silently evict. `doctor` prints every path, so "where did it put my files"
is answered before it is asked.

`<data>/runtime` is the AI environment — the interpreter, the dependencies and
the download cache, about 700 MB. Deleting it reclaims all of that and costs
nothing but installing again.

**Choose the data directory before installing the environment.** A Python
environment records absolute paths, so moving it afterwards leaves one that
exists and does not run. `doctor` says so in as many words if that happens.
