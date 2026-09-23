# 安装 NikuCooker

[← 返回 README](README.zh-CN.md) · [English](INSTALL.md) · **中文**

本文只讲**怎么装发布版二进制**，覆盖 Windows、macOS 和 Linux。用 Docker、从源码构建、
以及这个程序到底能做什么，都在 README 里。

每次打 tag 发布时都会产出一个二进制，网页界面已经嵌在里面，所以不需要装 Go、Node
或任何构建工具链。到 [Releases](https://github.com/AhsokaTano26/NikuCooker/releases)
下载对应平台的压缩包：

| 平台 | 文件名 |
|------|--------|
| macOS，Apple Silicon | `nikucooker_<版本>_darwin_arm64.tar.gz` |
| macOS，Intel | `nikucooker_<版本>_darwin_amd64.tar.gz` |
| Windows 10/11，x86_64 | `nikucooker_<版本>_windows_amd64.zip` |
| Linux，x86_64 | `nikucooker_<版本>_linux_amd64.tar.gz` |
| Linux，arm64 | `nikucooker_<版本>_linux_arm64.tar.gz` |

没有 Windows arm64 版本：worker 依赖的那些 Python 包在 Windows arm64 上实际上没有
可用的构建，而一个找不到 worker 的二进制是我们兑现不了的承诺。

## 有两样东西没有打包进去

**Python。** 语音识别跑在一个 Python worker 里，把它一起打包会让每次下载多出
400 MB 到 3 GB。没有它二进制照样能跑——只有识别用不了。

要装 worker，先把仓库克隆下来并构建它的环境：

```bash
git clone https://github.com/AhsokaTano26/NikuCooker
cd NikuCooker/ai && uv sync
```

然后把这个二进制放到那个 checkout 的根目录、也就是 `ai/` 旁边，它就会自己找到 worker：

```bash
cp /path/to/nikucooker /path/to/NikuCooker/
```

（把压缩包直接**解压到** checkout 里的话，会覆盖掉仓库自己的 README 和 LICENSE——
要复制的是二进制，不是压缩包。）

放在别处的话，就在配置文件里指明这个目录：

```yaml
ai:
  dir: C:\path\to\NikuCooker\ai
```

`nikucooker config path` 会告诉你读的是哪个配置文件；`nikucooker config init`
会在还没有的时候写一份带注释的出来。

**FFmpeg 和 `ffprobe`**，必须能在 `PATH` 里找到。另外，硬字幕还要求这个 FFmpeg
编译时带了 `libass`，并且系统里装了中日韩字体——缺任何一样，压进画面的字幕都会
显示成空白方块。缺的时候 `doctor` 会把这两项都点出来。

先跑一次 `nikucooker doctor`。上面这些它都会检查，缺什么就直接告诉你怎么补，
而不是等到任务跑到一半才失败。

## macOS

```bash
tar xzf nikucooker_<版本>_darwin_arm64.tar.gz
xattr -d com.apple.quarantine ./nikucooker     # 见下
./nikucooker doctor
./nikucooker serve
```

macOS 只信任用付费 Developer ID 签名的二进制，而这些不是。它们能跑——链接器会给
每个 arm64 二进制加上有效的 ad-hoc 签名——但从浏览器下载来的会被打上隔离标记并拒绝
运行，提示 *"cannot be opened because the developer cannot be verified"*。
清掉这个属性就好了，或者在访达里右键这个二进制、选**打开**、确认一次。

Apple Silicon 上识别走 CPU：CTranslate2 没有 Metal 后端，所以这里没有 GPU 加速
路径，`doctor` 会明说而不是悄悄降级。模型选 `small` 或 `medium` 比较合适。

Intel Mac 是受限的那一档——`onnxruntime` 在 1.23.2 之后就不再发布 macOS x86_64 的
wheel 了，这导致 worker 最高只能用 Python 3.13。

## Windows

```powershell
Expand-Archive nikucooker_<版本>_windows_amd64.zip -DestinationPath .
.\nikucooker.exe doctor
.\nikucooker.exe serve
```

想在任意 shell 里直接敲 `nikucooker` 而不是 `.\nikucooker.exe`，就把解压出来的那个
文件夹加到 `PATH` 里。

有两件 Windows 特有的事 `doctor` 会检查：

- **长路径。** 在 `%LOCALAPPDATA%\NikuCooker\projects\<uuid>\artifacts\...`
  这种目录下，260 字符的 `MAX_PATH` 限制很容易超。要么在注册表里打开长路径支持
  （`LongPathsEnabled`），要么把 `--data-dir` 指到一个短路径上，比如 `C:\niku`。
- **NVIDIA 显卡。** `cuda` 那套依赖需要提供 `libcublas` 的 CUDA 12.x 运行时；
  `doctor` 会真的试加载一次而不是想当然。没有的话识别会退回 CPU。

## Linux

```bash
tar xzf nikucooker_<版本>_linux_amd64.tar.gz
./nikucooker doctor
./nikucooker serve
```

这个二进制是静态链接的、构建时没有用 cgo，所以不依赖宿主机的 libc，任何发行版都能跑。
`amd64` 和 `arm64` 都有发布。

在服务器上，[Docker](README.zh-CN.md#docker) 仍然是推荐的路径：FFmpeg 和 Python worker
都在镜像里，而这两样二进制都没有打包。

## 文件放在哪

| | macOS | Windows |
|---|---|---|
| 数据和配置 | `~/Library/Application Support/NikuCooker` | `%LOCALAPPDATA%\NikuCooker` |
| 模型 | `<数据目录>/models` | `<数据目录>/models` |

Linux 上数据和配置遵循 XDG 目录规范（`$XDG_DATA_HOME/nikucooker`、
`$XDG_CONFIG_HOME/nikucooker`）。

`--data-dir` 或 `NIKUCOOKER_DATA_DIR` 可以覆盖前者。模型放在数据目录里而不是缓存
目录里，因为下载下来的 `large-v3` 是几个 GB 的用户可见状态，不该被某个清理工具悄悄
清掉。`doctor` 会把每个路径都打印出来，所以「它到底把我的文件放哪了」这个问题在你
开口问之前就已经有答案了。
