# 安装 NikuCooker

[← 返回 README](README.zh-CN.md) · [English](INSTALL.md) · **中文**

本文只讲**怎么装发布版二进制**，覆盖 Windows、macOS 和 Linux。从源码构建、在服务器上
运行、以及这个程序到底能做什么，都在 README 里。

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

**解压出来的整个文件夹要放在一起。** 压缩包里有 `nikucooker` 二进制、一个装 worker 源码的
`ai/` 目录、一份 `uv`，以及一个 `pkg/` 目录（worker 要读的协议 fixture）。二进制会在自己
旁边找这些，而且它之后构建出来的环境里记的是**绝对路径**——所以事后再挪文件夹，就得重新
安装一次。

## AI 运行环境

语音识别跑在一个 Python worker 里，而压缩包**刻意没有**带 Python 本身和那些依赖：
带上会让每次下载多出 400 MB 到 3 GB，包括那些根本不会跑任务的人。压缩包带的是
**安装它们所需要的一切**。

所以是两步，第二步是一个按钮：

1. 运行二进制。服务立刻起来，网页界面可以用——只有识别不能。
2. 打开**「系统」**页（`http://localhost:8080/system`），点**「安装 AI 运行环境」**。

它会下载一个 Python 解释器和依赖，约 300 MB，装进数据目录，装完自检一次，全程逐步报告。
**只下载这一次。** 你不点，它一个字节都不下；下载量和装到哪里都先写在页面上。

**有 NVIDIA 显卡的机器**，页面上会比默认选项多一个选择：同一套环境再加上 NVIDIA 的计算库，
多下载约 700 MB，识别速度会快很多。只有真能用上时才会提供——那些库没有 macOS 版本，
没插显卡的机器上这个选项会被换成一句说明。装完不用改任何配置，worker 自己认得，System
页面也会写明这套环境是按哪种方式装的。

失败的话，页面直接显示 `uv` 的原话；对于大家真正会踩的那几类——公司代理、磁盘满、
会拆 TLS 的防火墙——上面还会多一句该怎么办。安装可以取消，取消后会清理干净，
所以重试是能成的。

装完 `nikucooker doctor` 会报告 worker 可用，流水线就能跑了。

### 如果你想自己装环境

把 `ai.python` 指到一个解释器上就会关掉那个按钮：你自己选的解释器才是要跑的那个。
这种情况下按老办法建环境：

```bash
git clone https://github.com/AhsokaTano26/NikuCooker
cd NikuCooker/ai && uv sync
```

然后让二进制找到它——要么把这个二进制放到那个 checkout 的根目录、也就是 `ai/` 旁边，
它会自己找到：

```bash
cp /path/to/nikucooker /path/to/NikuCooker/
```

（把压缩包直接**解压到** checkout 里的话，会覆盖掉仓库自己的 README 和 LICENSE——
要复制的是二进制，不是压缩包。）

要么在配置文件里指明这个目录：

```yaml
ai:
  dir: C:\path\to\NikuCooker\ai
```

`nikucooker config path` 会告诉你读的是哪个配置文件；`nikucooker config init`
会在还没有的时候写一份带注释的出来。

### FFmpeg 仍然没有打包

**FFmpeg 和 `ffprobe`** 必须能在 `PATH` 里找到——它们在每个平台上都是系统包，不值得
再多带一份。另外，硬字幕还要求这个 FFmpeg 编译时带了 `libass`，并且系统里装了中日韩
字体；缺任何一样，压进画面的字幕都会显示成空白方块。缺的时候 `doctor` 会把这两项都
点出来，并告诉你怎么补。

先跑一次 `nikucooker doctor`。上面这些它都会检查，缺什么就直接告诉你怎么补，
而不是等到任务跑到一半才失败。

### 代理，或者会拆 TLS 的防火墙

`HTTPS_PROXY` 和 `SSL_CERT_FILE` 会原样传给安装程序，所以常见的设置都好使。有一点
值得知道：`uv` 用的是它自己的 TLS 栈，而不是操作系统的信任库，所以在会重签证书的网络里
（受管笔记本上很常见）会报 `invalid peer certificate: UnknownIssuer`，尽管这台机器上
每个浏览器都一切正常。把 `SSL_CERT_FILE` 指向你们组织的 CA 证书包就好，或者设
`UV_NATIVE_TLS=1` 让它改用系统信任库。安装页认得这个失败，会直接告诉你，而不是把
原始报错甩给你。

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
  这种目录下，260 字符的 `MAX_PATH` 限制很容易超，而 AI 环境会让它更糟——
  光是在 `runtime\venv\Lib\site-packages\` 下面走一圈就已经很长了，那时还没有任何项目。
  **装环境之前先把 `--data-dir` 指到短路径上**，比如 `C:\niku`，或者在注册表里打开长路径
  支持（`LongPathsEnabled`）。装完再挪数据目录等于重装。
- **NVIDIA 显卡。** `cuda` 那套依赖需要提供 `libcublas` 的 CUDA 12.x 运行时；
  `doctor` 会真的试加载一次而不是想当然。没有的话识别会退回 CPU。

## Linux

```bash
tar xzf nikucooker_<版本>_linux_amd64.tar.gz
./nikucooker doctor
./nikucooker serve
```

这个二进制是静态链接的、构建时没有用 cgo，所以不依赖宿主机的 libc，任何发行版都能跑。
`amd64` 和 `arm64` 都有发布。它带的那份 `uv` 需要 glibc 2.28 或更新——还在收更新的发行版
都满足。

在服务器上这就是唯一的路径：没有容器镜像。FFmpeg 仍然要你自己装，AI 环境则是每台机器
各装一次——见[在服务器上运行](README.zh-CN.md#在服务器上运行)。

## 文件放在哪

| | macOS | Windows |
|---|---|---|
| 数据和配置 | `~/Library/Application Support/NikuCooker` | `%LOCALAPPDATA%\NikuCooker` |
| 模型 | `<数据目录>/models` | `<数据目录>/models` |
| AI 运行环境 | `<数据目录>/runtime` | `<数据目录>/runtime` |

Linux 上数据和配置遵循 XDG 目录规范（`$XDG_DATA_HOME/nikucooker`、
`$XDG_CONFIG_HOME/nikucooker`）。

`--data-dir` 或 `NIKUCOOKER_DATA_DIR` 可以覆盖前者。模型放在数据目录里而不是缓存
目录里，因为下载下来的 `large-v3` 是几个 GB 的用户可见状态，不该被某个清理工具悄悄
清掉。`doctor` 会把每个路径都打印出来，所以「它到底把我的文件放哪了」这个问题在你
开口问之前就已经有答案了。

`<数据目录>/runtime` 就是 AI 运行环境——解释器、依赖和下载缓存，约 700 MB。
删掉它就能把这些空间全收回来，代价只是重装一次。

**装环境之前先把数据目录定下来。** Python 环境里记的是绝对路径，事后挪走会留下一个
存在但跑不起来的环境。真发生了，`doctor` 会明说。
