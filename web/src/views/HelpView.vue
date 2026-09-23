<script setup lang="ts">
import { computed, onMounted } from 'vue'
import { RouterLink } from 'vue-router'

import { api } from '@/api/client'
import AppBadge from '@/components/AppBadge.vue'
import { useAsync } from '@/composables/useAsync'

/**
 * The manual, in the application.
 *
 * It exists because the long-form documentation lives in the repository, and
 * the people who most need it are the ones running this from a release binary —
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
    symptom: '磁盘占用越来越大',
    answer:
      '项目页的「磁盘占用」列出四类文件各占多少，中间产物通常最大且可以随时删掉重算。「设置 → 清理」里可以配置按天数自动删除旧项目和旧日志。',
  },
]

const system = useAsync(() => api.system.overview())
const models = useAsync(() => api.models.list())
const providers = useAsync(() => api.providers.list())
const projects = useAsync(() => api.projects.list({ limit: 1 }))

const readyModel = computed(() => models.data.value?.items.some((model) => model.kind === 'asr' && model.status === 'ready') ?? false)
const readyProvider = computed(() => providers.data.value?.items.some((provider) => provider.enabled && provider.has_key) ?? false)
const reviewComplete = computed(
  () =>
    projects.data.value?.items.some(
      (project) => project.segment_count > 0 && project.needs_review_count === 0,
    ) ?? false,
)

const starterSteps = computed(() => [
  {
    number: 1, title: '检查运行环境',
    description: '确认 FFmpeg、Python 和识别 Worker 可以使用。缺少环境时，系统页会提供安装入口。',
    to: '/system', action: '检查系统', done: system.data.value?.worker.status === 'ready',
  },
  {
    number: 2, title: '配置翻译服务',
    description: '添加一个 OpenAI 兼容端点、模型和密钥，然后先点一次连接测试。',
    to: '/providers', action: '配置翻译服务', done: readyProvider.value,
  },
  {
    number: 3, title: '准备识别模型',
    description: '第一次建议选择 medium；只想确认流程时可先下载 tiny。',
    to: '/models', action: '选择模型', done: readyModel.value,
  },
  {
    number: 4, title: '创建第一个项目',
    description: '上传视频，确认日语到简体中文和翻译风格，然后创建项目。',
    to: '/projects/new', action: '新建项目', done: (projects.data.value?.total ?? 0) > 0,
  },
  {
    number: 5, title: '运行处理任务',
    description: '进入项目后点击“运行”。页面会说明当前阶段、缓存命中和失败原因。',
    to: '/projects', action: '打开项目', done: (system.data.value?.counts.segments ?? 0) > 0,
  },
  {
    number: 6, title: '审校并导出',
    description: '在项目页进入审校工作台，播放原片、修改字幕、处理质量问题，再重新运行生成成品。',
    to: '/projects', action: '去审校', done: reviewComplete.value,
  },
])

onMounted(() => {
  void Promise.all([system.run(), models.run(), providers.run(), projects.run()])
})
</script>

<template>
  <div class="mx-auto max-w-5xl space-y-6">
    <section class="rounded border border-line bg-surface-raised p-4">
      <div class="flex items-start justify-between gap-4">
        <div>
          <h2 class="text-base font-medium">第一次使用：按顺序完成这六步</h2>
          <p class="mt-1 text-sm text-ink-muted">每一步都能直接跳到对应页面；“已完成”根据这台机器的当前状态判断。</p>
        </div>
        <AppBadge tone="accent">新手路线</AppBadge>
      </div>

      <ol class="mt-4 grid gap-3 md:grid-cols-2">
        <li v-for="step in starterSteps" :key="step.number" class="rounded border border-line bg-surface p-4">
          <div class="flex items-center gap-2">
            <span class="flex size-6 items-center justify-center rounded-full bg-surface-sunken text-xs tabular-nums">{{ step.number }}</span>
            <h3 class="text-sm font-medium">{{ step.title }}</h3>
            <AppBadge
              class="ml-auto"
              :data-test="step.done ? 'starter-step-done' : undefined"
              :tone="step.done ? 'done' : 'neutral'"
            >
              {{ step.done ? '已完成' : '待完成' }}
            </AppBadge>
          </div>
          <p class="mt-2 min-h-10 text-xs leading-5 text-ink-muted">{{ step.description }}</p>
          <RouterLink
            :to="step.to"
            class="mt-3 inline-flex rounded border border-line px-3 py-1.5 text-xs text-ink-muted outline-none hover:border-accent hover:text-ink focus-visible:ring-2 focus-visible:ring-accent"
          >
            {{ step.action }} →
          </RouterLink>
        </li>
      </ol>

      <div class="mt-4 rounded border border-line bg-surface-sunken p-3 text-xs leading-5 text-ink-muted">
        <strong class="text-ink">最省心的首次组合：</strong>
        medium 模型 + 默认字幕组风格 + 软字幕。先用一段短视频跑通，确认字幕可用后再处理长片。
      </div>
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
