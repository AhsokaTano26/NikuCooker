# NikuCooker

[English](README.md) · **中文**

NikuCooker 是一个面向视频字幕制作的全自动 AI 处理流水线，目标是将未经翻译的“生肉”视频自动处理为带有高质量中文字幕的“熟肉”视频。

**语音识别和媒体处理全部在你自己的机器上完成，音视频不会离开本机。** 只有字幕文本会
发给语言模型，而且只发给你配置的那一家。

## 它能做什么

```
读取容器 → 提取音频 → 语音检测 → 语音识别
→ 切分字幕行 → 通读作品 → LLM 翻译
→ 质量检查 → 生成字幕文件 → 渲染视频
```

每个阶段都是实现好的，能端到端跑通：给它一个日语视频，它会产出翻译好的 `.srt` 和
`.ass` 文件、一份质量报告，以及一个带字幕的视频。目前优先做**日语 → 简体中文**
（动画、声优节目、直播口播、访谈），语言是当作数据处理的，加新语言对是新增而不是重写。

有两条性质对设计的塑造比什么都大：

- **没有一样东西是白算的。** 每个阶段都按内容寻址，翻译按行缓存。改三行再跑一次就
  只重翻三行；改一个 prompt 或一条术语，失效的范围精确到它影响的那部分。
- **你改过的比机器大。** 你修正过的行不会被之后的运行覆盖，重新渲染用的也是你留下的
  那些行。

## 快速开始

```bash
make ai-install                  # 创建 Python 环境
make build                       # 构建网页应用和二进制

./nikucooker doctor              # 检查这台机器能不能跑这条流水线
./nikucooker project create --source /path/to/episode01.mkv
./nikucooker run <项目 id>
./nikucooker serve               # 然后打开 http://localhost:8080
```

在一台新机器上，第一件该做的就是跑 `nikucooker doctor`：它会逐项报告所有前提条件，
缺什么就告诉你怎么补。

翻译需要一个语言模型。可以在网页界面里添加一个 OpenAI 兼容的服务，也可以直接写配置：

```yaml
translation:
  base_url: https://api.example.com/v1
  api_key: sk-...
  model: some-model
```

## 架构简述

三种语言，边界分明：

| | 负责什么 |
|---|---|
| **Go** | 整个系统：API、流水线、任务、产物缓存、数据库、FFmpeg、LLM 调用、Python worker 生命周期 |
| **Python** | 只做本地推理：语音识别、语音活动检测 |
| **Vue 3** | 界面。它是 Go API 的客户端，仅此而已 |

LLM 翻译放在 Go 而不是 Python 里是刻意的：调用一个 HTTP API 需要重试策略、超时、
并发限制、缓存和 provider 抽象，这些是系统层的活。Python 在这里存在，唯一的原因是
`faster-whisper` 是 Python 的。

需求文档和设计文档在本地维护，不发布在这个仓库里。公开发布的是代码和这份文件。

## 环境要求

用发布版二进制的话，唯一的前提是 **FFmpeg** 和 **ffprobe** 在 `PATH` 里；
AI 运行环境会自己装好——见 [INSTALL.zh-CN.md](INSTALL.zh-CN.md)。

从源码构建、或者要改代码的话：

- **Go** 1.26 或更新
- **Python** 3.12–3.14，用 [uv](https://docs.astral.sh/uv/) 管理
- **Node** 24+ 和 **pnpm** 10+（只在构建网页界面时需要）
- **FFmpeg** 和 **ffprobe** 在 `PATH` 里

语音识别在任何平台上都跑 CPU，或者在 CUDA 12.x 的 NVIDIA 显卡上跑 GPU。Apple Silicon
上只能跑 CPU：CTranslate2 没有 Metal 后端，`doctor` 会明说这一点，而不是悄悄降级。

## 构建

```bash
make build          # 先构建网页应用，再构建嵌入了它的 Go 二进制
./nikucooker version
```

`make help` 列出所有目标。`make build-go` 跳过前端，直接把 `web/dist` 里现有的东西
嵌进去。

## Docker

服务器上推荐的路径，也是给不想管 Python 环境的人准备的：

```bash
docker compose up -d                        # CPU
docker compose --profile cuda up -d cuda    # NVIDIA 显卡
```

然后打开 <http://localhost:8080>。

CUDA 镜像是单独构建的，因为它要大出好几个 GB；用 CPU 的人永远不会被迫下载它。两者都
挂载 `./data`、`./models` 和 `./config`，所以在它们之间切换时，所有项目、模型和设置
都还在。

## 安装发布版二进制

每次打 tag 发布时都会产出 Windows、macOS 和 Linux 的二进制，网页界面已经嵌在里面，
不需要 Go、Node 或任何构建工具链。

压缩包里还带了 AI worker 的源码和一份 `uv`。它们需要的 Python 解释器和依赖，是在网页的
「系统」页上点一下按钮装好的——按钮上写着要下载多少、装到哪里。

具体步骤见 **[INSTALL.zh-CN.md](INSTALL.zh-CN.md)**。

## 开发

```bash
make ai-install     # 按 uv.lock 创建 Python 环境
make ai-selfcheck   # 报告那个环境是否可用
make web-install
make test           # Go、Python 和前端三套测试
make lint           # gofmt、go vet、ruff、ESLint、vue-tsc
```

`make web-dev` 会启动 Vite 开发服务器，并把 `/api` 代理到 `127.0.0.1:8080` 上的核心。

## 配置

设置来自五层，优先级从低到高：

```
默认值 → 配置文件 → 环境变量 → 项目覆盖 → 命令行参数
```

默认值本身就是一份完整可用的配置，只有想改什么才需要配置文件。`NIKUCOOKER_CONFIG`
指定要读哪个文件，而各个 `NIKUCOOKER_*` 变量设置单个值——容器镜像就是靠这个在
没有配置文件的情况下配置的。

## 关于 Python 依赖的一个说明

`faster-whisper` 把它的 Silero VAD 模型放在自己的 wheel 里，并通过它本来就依赖的
`onnxruntime` 来跑。所以 `silero-vad` 这个包是**刻意不列为依赖**的：加上它会引入
PyTorch，多出大约 2.5 GB，而能力上没有任何增加。

## 许可证

MIT —— 见 [LICENSE](LICENSE)。
