<script setup lang="ts">
import { computed, onMounted, onUnmounted, watch } from 'vue'
import { RouterLink, useRouter } from 'vue-router'

import { api } from '@/api/client'
import AppButton from '@/components/AppButton.vue'
import { formatBytes, useAsync } from '@/composables/useAsync'
import { WORKER_LABEL, attentionClass, installStepLabel } from '@/composables/environment'
import { useEventStore } from '@/stores/events'

const events = useEventStore()
const router = useRouter()
const overview = useAsync(() => api.system.overview())

let unsubscribers: (() => void)[] = []

onMounted(() => {
  overview.run()

  // The environment check runs after the server starts listening, so a page
  // loaded quickly sees `starting` and has to be told when that changes. The
  // stream carries it; without this the warning would only appear after a
  // reload, which is the one moment a user is not going to do.
  unsubscribers.push(
    events.on('worker.status', () => void overview.run()),
    events.on('resync.required', () => void overview.run()),
  )
})

onUnmounted(() => {
  for (const off of unsubscribers) off()
  unsubscribers = []
})

/**
 * Host statistics are pushed, not polled, so the figures move while a job runs
 * without the page asking for them.
 */
watch(() => events.resyncCount, overview.run)

const counts = computed(() => overview.data.value?.counts)
const stats = computed(() => overview.data.value?.stats)
const worker = computed(() => overview.data.value?.worker)
const runtime = computed(() => overview.data.value?.runtime)

/**
 * The environment's state, as one thing.
 *
 * An install in progress is a state of the environment, and the check's verdict
 * does not move until it finishes — so reading only `worker.status` would report
 * "尚未安装" over a download that is halfway done, at exactly the moment someone
 * is looking at this page to see how it is going.
 */
const environment = computed(() => {
  if (runtime.value?.status === 'running') {
    return {
      label: '安装中…',
      step: installStepLabel(runtime.value.phase),
      tone: 'text-status-running',
    }
  }

  const status = worker.value?.status
  return {
    label: status ? WORKER_LABEL[status] : '—',
    step: '',
    tone: attentionClass(status),
  }
})

/**
 * What to tell a user whose machine cannot transcribe.
 *
 * Two states call for an install — nothing was found, or something was found
 * and it does not work — and they need different sentences, because the second
 * one has an error to show and the first does not. `starting` deliberately
 * produces nothing: an answer that has not arrived is not a problem to report.
 */
const environmentProblem = computed(() => {
  // An install in progress is not a problem to report: the page says it is
  // installing instead, and a "去安装" button over a running download is how a
  // user starts a second one.
  if (runtime.value?.status === 'running') return null

  const status = worker.value?.status
  if (status !== 'missing' && status !== 'failed') return null

  // A button is offered only when pressing it can work.
  //
  // Not every broken environment can be fixed by installing one: a binary that
  // has no ai/ directory beside it has nothing to install *into*, and the
  // System page says so. Sending someone there with a button that promised the
  // opposite is how a page teaches people to distrust it — so in that case the
  // card explains, and offers nothing.
  if (!runtime.value?.available) {
    return {
      title: 'AI 运行环境不可用',
      body: runtime.value?.reason ?? '这个安装无法自行准备运行环境。',
      action: '',
    }
  }

  if (status === 'missing') {
    return {
      title: '还没有安装 AI 运行环境',
      body: '语音识别需要它。安装会下载一个 Python 解释器和识别依赖，约 300 MB，只下载这一次。' +
        '在此之前，项目、字幕编辑和翻译都能正常使用。',
      action: '去安装',
    }
  }
  return {
    title: 'AI 运行环境有问题',
    body: '找到了解释器，但它加载不了识别组件。重新安装一次通常就能解决。',
    action: '重新安装',
  }
})

const cards = computed(() => {
  const value = counts.value
  return [
    { label: '项目', value: value?.projects ?? 0, to: '/projects' },
    { label: '运行中', value: value?.jobs_running ?? 0, to: '/projects' },
    { label: '待审校', value: value?.needs_review ?? 0, to: '/projects' },
    { label: '字幕行', value: value?.segments ?? 0, to: '/projects' },
  ]
})

function percent(value: number | null | undefined): string {
  return value === null || value === undefined ? '—' : `${value.toFixed(0)}%`
}
</script>

<template>
  <div class="space-y-6">
    <div v-if="overview.error.value" class="rounded border border-status-failed/40 bg-surface-raised p-4">
      <p class="text-sm text-status-failed">{{ overview.error.value }}</p>
      <AppButton variant="ghost" size="sm" class="mt-2" @click="overview.run">重试</AppButton>
    </div>

    <!--
      The environment check's verdict, when it is one the user has to act on.

      On the dashboard rather than only on the System page, because the
      dashboard is where someone lands. A machine that cannot transcribe should
      say so at the first thing the user looks at, rather than on a page they
      have to already know to open — which is the same reasoning as the check
      itself running at startup rather than on demand.
    -->
    <div
      v-if="environmentProblem"
      class="rounded border border-status-warn/50 bg-surface-raised p-4"
    >
      <div class="flex items-start justify-between gap-4">
        <div class="min-w-0">
          <p class="text-sm font-medium text-status-warn">{{ environmentProblem.title }}</p>
          <p class="mt-1 text-sm text-ink-muted">{{ environmentProblem.body }}</p>
          <p
            v-if="worker?.detail"
            class="mt-2 font-mono text-xs break-words text-ink-faint"
          >
            {{ worker.detail }}
          </p>
        </div>
        <AppButton
          v-if="environmentProblem.action"
          variant="primary"
          size="sm"
          @click="router.push('/system')"
        >
          {{ environmentProblem.action }}
        </AppButton>
      </div>
    </div>

    <div class="grid grid-cols-2 gap-4 lg:grid-cols-4">
      <RouterLink
        v-for="card in cards"
        :key="card.label"
        :to="card.to"
        class="rounded border border-line bg-surface-raised p-4 transition hover:border-accent/60"
      >
        <p class="text-sm text-ink-muted">{{ card.label }}</p>
        <p class="mt-1 text-2xl font-semibold tabular-nums">
          {{ overview.loading.value ? '…' : card.value }}
        </p>
      </RouterLink>
    </div>

    <div class="grid gap-4 lg:grid-cols-2">
      <section class="rounded border border-line bg-surface-raised p-4">
        <h2 class="text-sm font-medium text-ink-muted">主机</h2>
        <dl class="mt-3 space-y-2 text-sm">
          <div class="flex justify-between">
            <dt class="text-ink-muted">CPU</dt>
            <dd class="tabular-nums">
              {{ stats?.cpu.cores ?? '—' }} 核 · {{ percent(stats?.cpu.usage_percent) }}
            </dd>
          </div>
          <div class="flex justify-between">
            <dt class="text-ink-muted">内存</dt>
            <dd class="tabular-nums">
              {{ formatBytes(stats?.memory.used_bytes) }} / {{ formatBytes(stats?.memory.total_bytes) }}
            </dd>
          </div>
          <div class="flex justify-between">
            <dt class="text-ink-muted">数据盘可用</dt>
            <dd class="tabular-nums">{{ formatBytes(stats?.disk.data_free_bytes) }}</dd>
          </div>
          <div class="flex justify-between">
            <dt class="text-ink-muted">模型盘可用</dt>
            <dd class="tabular-nums">{{ formatBytes(stats?.disk.models_free_bytes) }}</dd>
          </div>
        </dl>

        <!--
          No GPU section when there is none. An empty box labelled "GPU" reads as
          a failure to detect one; its absence reads as a machine that has none,
          which is the truth.
        -->
        <div v-if="stats?.gpu.length" class="mt-3 border-t border-line pt-3">
          <p class="text-xs text-ink-faint">GPU</p>
          <div v-for="gpu in stats.gpu" :key="gpu.index" class="mt-1 flex justify-between text-sm">
            <span>{{ gpu.name }}</span>
            <span class="tabular-nums">
              {{ formatBytes(gpu.memory_used_bytes) }} / {{ formatBytes(gpu.memory_total_bytes) }}
            </span>
          </div>
        </div>
      </section>

      <section class="rounded border border-line bg-surface-raised p-4">
        <h2 class="text-sm font-medium text-ink-muted">AI 运行环境</h2>
        <dl class="mt-3 space-y-2 text-sm">
          <div class="flex justify-between gap-4">
            <dt class="shrink-0 text-ink-muted">状态</dt>
            <!--
              Reported by the server, never inferred. A UI that decided "the
              worker is idle" from the absence of an event would be wrong every
              time the stream dropped.
            -->
            <dd :class="environment.tone">{{ environment.label }}</dd>
          </div>
          <div v-if="environment.step" class="flex justify-between gap-4">
            <dt class="shrink-0 text-ink-muted">进行到</dt>
            <dd class="text-status-running">{{ environment.step }}</dd>
          </div>
          <div class="flex justify-between gap-4">
            <dt class="shrink-0 text-ink-muted">解释器</dt>
            <dd class="truncate font-mono text-xs" :title="worker?.python">{{ worker?.python || '—' }}</dd>
          </div>
          <div class="flex justify-between gap-4">
            <dt class="shrink-0 text-ink-muted">位置</dt>
            <dd
              class="truncate font-mono text-xs"
              :title="runtime?.runtime_dir"
            >
              {{ runtime?.runtime_dir || '—' }}
            </dd>
          </div>
          <div class="flex justify-between gap-4">
            <dt class="shrink-0 text-ink-muted">协议摘要</dt>
            <dd class="truncate font-mono text-xs" :title="worker?.schema_digest">
              {{ worker?.schema_digest?.slice(0, 24) ?? '—' }}
            </dd>
          </div>
        </dl>

        <p class="mt-3 border-t border-line pt-3 text-xs text-ink-faint">
          Worker 在第一次需要时启动。这里只报告状态，不会为了显示状态而启动一个进程。
        </p>
      </section>
    </div>

    <section v-if="overview.data.value" class="rounded border border-line bg-surface-raised p-4">
      <h2 class="text-sm font-medium text-ink-muted">关于</h2>
      <p class="mt-2 font-mono text-xs text-ink-muted">
        NikuCooker {{ overview.data.value.version }} ({{ overview.data.value.commit }}) ·
        {{ overview.data.value.platform }}
      </p>
    </section>
  </div>
</template>
