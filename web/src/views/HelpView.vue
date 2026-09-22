<script setup lang="ts">
/**
 * The manual, in the application.
 *
 * It exists because the long-form documentation lives in the repository, and
 * the people who most need it are the ones running this from a container —
 * who never see that repository at all. So this is written for them: what the
 * stages are, what to do when something goes wrong, and nothing about how any
 * of it is built.
 *
 * Stage names are given with the English label the pipeline shows beside the
 * Chinese, because the two do not match and a help page that names a stage
 * differently from the screen the reader is looking at is worse than no help
 * page.
 */

interface Stage {
  /** What the pipeline displays, verbatim. */
  label: string
  name: string
  description: string
}

const stages: Stage[] = [
  {
    label: 'Reading the container',
    name: '读取容器',
    description: '读出时长、分辨率、音轨。失败通常是文件损坏或不是视频。',
  },
  {
    label: 'Extracting audio',
    name: '提取音频',
    description: '把音轨抽成模型能读的格式，之后所有识别都基于这一步的产物。',
  },
  {
    label: 'Detecting speech',
    name: '语音检测',
    description: '找出哪里有人说话、哪里是静音或纯音乐，交给识别的那部分因此更短也更准。',
  },
  {
    label: 'Transcribing',
    name: '语音识别',
    description: '日语转日语文本。这一步在本机跑，音频不出这台机器。模型越大越准也越慢。',
  },
  {
    label: 'Splitting into lines',
    name: '切分字幕行',
    description: '把连续的文本切成一行一条字幕，并按停顿和标点调整断句。',
  },
  {
    label: 'Analysing the work',
    name: '通读作品',
    description:
      '先看一遍全片，总结剧情、列出人物和专有名词。有了它，翻译才知道「她」是谁、名字该怎么统一。这一步要调用翻译服务。可以不启用。',
  },
  {
    label: 'Translating',
    name: '翻译',
    description: '日译中。按行缓存，改几行只会重翻几行。这是唯一会把文本发到外部服务的一步。',
  },
  {
    label: 'Checking quality',
    name: '质量检查',
    description:
      '自动挑出可疑的译文：过长、过短、没翻、语速超限。结果在项目页的「审校队列」里处理。',
  },
  {
    label: 'Writing subtitles',
    name: '生成字幕文件',
    description: '写出 SRT 和 ASS。ASS 带样式，是渲染硬字幕时用的那份。',
  },
  {
    label: 'Rendering',
    name: '渲染视频',
    description: '把字幕压进画面（硬字幕）或封装成字幕轨（软字幕）。',
  },
]

interface Problem {
  symptom: string
  answer: string
}

const problems: Problem[] = [
  {
    symptom: '「没有配置翻译服务」',
    answer:
      '翻译服务是唯一必须自己配的东西。到「翻译服务」页面填一个 OpenAI 兼容的端点和模型，或者用配置文件设置 translation.base_url / api_key / model。',
  },
  {
    symptom: '「模型没有下载」',
    answer:
      '第一次运行会自动下载识别模型。也可以提前在「模型」页面手动下载。tiny（74 MB）用来试通流程，medium（1.4 GB）是默认，large-v3（2.9 GB）最准也最慢。',
  },
  {
    symptom: '改了设置没生效',
    answer:
      '多半是被更高一层的值盖住了。设置页最下面那张表列出每一项的最终取值和它来自哪一层。优先级从低到高是：默认值 → 配置文件 → 环境变量 → 网页 → 项目覆盖 → 命令行，越靠右越优先，网页上的改动优先于配置文件。',
  },
  {
    symptom: '重跑一次还是很慢',
    answer:
      '看项目页的阶段列表。「= 已缓存」的没有重算，「✓ 完成」的是真的重算了。如果全都重算了，说明有输入变了 —— 检查是不是用了 --force，或者改动触及了上游阶段（改了识别模型，下游全部要重来）。',
  },
  {
    symptom: '渲染出来没字幕',
    answer:
      '软字幕要在播放器里选一下字幕轨道，多数播放器会自动选中。硬字幕要确认 doctor 报告里有 libass，否则这一版 FFmpeg 压不进去。输出成 MP4 会丢样式，但字幕本身还在。',
  },
  {
    symptom: 'Apple Silicon 上很慢',
    answer:
      'CTranslate2 没有 Metal 后端，识别只能走 CPU，doctor 和运行日志都会明说。想要快只能用 NVIDIA 显卡。',
  },
  {
    symptom: '「运行完了却没有输出」',
    answer:
      '看项目页有没有红色的阶段。失败阶段的原因写在运行日志里（项目页的日志文件，或「日志」页面）。修好之后重跑，已经算过的阶段不会重算。',
  },
  {
    symptom: 'Docker 里点了「关闭服务」，容器又起来了',
    answer:
      '这是 compose 的 restart: unless-stopped 在起作用，不是按钮坏了。它重启的是容器，进程确实退出过。要真正停下来用 docker compose stop。',
  },
  {
    symptom: '磁盘占用越来越大',
    answer:
      '项目页的「磁盘占用」列出四类文件各占多少，中间产物通常最大且可以随时删掉重算。「设置 → 清理」里可以配置按天数自动删除旧项目和旧日志。',
  },
]
</script>

<template>
  <div class="space-y-6">
    <section class="rounded border border-line bg-surface-raised p-4">
      <h2 class="text-sm font-medium text-ink-muted">五分钟跑通</h2>
      <ol class="mt-3 space-y-2 text-sm text-ink-muted">
        <li>
          <span class="text-ink">1. 配好翻译服务。</span>
          这是唯一必须自己配的东西。到「翻译服务」页面填一个 OpenAI 兼容的端点。
        </li>
        <li>
          <span class="text-ink">2. 建项目。</span>
          在「新建项目」页把视频拖进去。文件会复制进项目目录，所以之后移动或删除原文件都不影响它。
        </li>
        <li>
          <span class="text-ink">3. 运行。</span>
          项目页点运行，跑完整条流水线。中途可以随时取消，已经算完的阶段会留在缓存里。
        </li>
        <li>
          <span class="text-ink">4. 看结果。</span>
          跑完在项目页顶部列出成品：SRT、ASS、带字幕的视频，都能直接下载。
        </li>
        <li>
          <span class="text-ink">5. 改不满意的行。</span>
          在字幕编辑页改过的行不会被之后的运行覆盖。改完重跑，只会重翻你动过的那几行。
        </li>
      </ol>
      <p class="mt-3 border-t border-line pt-3 text-xs text-ink-faint">
        还没确认这台机器能不能干这活？命令行跑一次 <span class="font-mono">nikucooker doctor</span>，
        它会逐项报告缺什么、怎么补。
      </p>
      <p class="mt-2 text-xs text-ink-faint">
        <span class="font-mono">nikucooker serve</span> 启动成功后会自己打开这个页面。
        不想让它打开就加 <span class="font-mono">--open=false</span>；
        在没有桌面环境的机器上（比如容器里）它本来就不会打开，也不会报错。
      </p>
    </section>

    <section class="rounded border border-line bg-surface-raised p-4">
      <h2 class="text-sm font-medium text-ink-muted">流水线的各个阶段</h2>
      <dl class="mt-3 space-y-3 text-sm">
        <div v-for="stage in stages" :key="stage.name">
          <dt class="flex flex-wrap items-baseline gap-x-2">
            <span class="text-ink">{{ stage.name }}</span>
            <span class="font-mono text-xs text-ink-faint">{{ stage.label }}</span>
          </dt>
          <dd class="mt-0.5 text-ink-muted">{{ stage.description }}</dd>
        </div>
      </dl>
    </section>

    <section class="rounded border border-line bg-surface-raised p-4">
      <h2 class="text-sm font-medium text-ink-muted">几个说法</h2>
      <dl class="mt-3 space-y-3 text-sm">
        <div>
          <dt class="text-ink">中间产物 / 成品</dt>
          <dd class="mt-0.5 text-ink-muted">
            中间产物是音频、转录、切分结果这些半成品，全部可以重算，删了不丢东西。
            成品是字幕文件和视频，在项目的 <span class="font-mono">output/</span> 目录下。
          </dd>
        </div>
        <div>
          <dt class="text-ink">缓存</dt>
          <dd class="mt-0.5 text-ink-muted">
            每个阶段都按「输入 + 配置 + 代码版本」算一个指纹。指纹没变就跳过，
            所以重跑通常很快。改配置、改 prompt、换模型都会让相关阶段重算，
            换了新版本的程序也一样 —— 这是故意的，避免用新代码跑旧结果。
          </dd>
        </div>
        <div>
          <dt class="text-ink">人工编辑优先</dt>
          <dd class="mt-0.5 text-ink-muted">
            你改过的字幕行不会被之后的运行覆盖。机器只在你不说话的地方说话。
          </dd>
        </div>
        <div>
          <dt class="text-ink">术语表</dt>
          <dd class="mt-0.5 text-ink-muted">
            固定专有名词的译法，让它在全片保持一致。只有出现在待翻译行里的条目才会被送进
            prompt，不会拿一堆用不上的术语干扰模型。
          </dd>
        </div>
      </dl>
    </section>

    <section class="rounded border border-line bg-surface-raised p-4">
      <h2 class="text-sm font-medium text-ink-muted">常见报错</h2>
      <dl class="mt-3 space-y-3 text-sm">
        <div v-for="problem in problems" :key="problem.symptom">
          <dt class="text-ink">{{ problem.symptom }}</dt>
          <dd class="mt-0.5 text-ink-muted">{{ problem.answer }}</dd>
        </div>
      </dl>
    </section>

    <p class="text-xs text-ink-faint">
      这份说明只讲怎么用，不讲它内部怎么实现。更多细节在仓库的
      <span class="font-mono">docs/usage.md</span> 里。
    </p>
  </div>
</template>
