<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { RouterLink, useRoute } from 'vue-router'

import { ApiError, api, type FileGroupKind, type ProjectOutput } from '@/api/client'
import AppBadge from '@/components/AppBadge.vue'
import AppButton from '@/components/AppButton.vue'
import { formatBytes, formatDuration, formatTime, useAsync } from '@/composables/useAsync'
import { pipelineProgress } from '@/composables/workbench'
import { useEventStore, type ServerEvent } from '@/stores/events'
import type { PipelineView, StageStatus, StageView } from '@/types/api'

const route = useRoute()
const events = useEventStore()
const projectId = computed(() => String(route.params['id']))

const project = useAsync(() => api.projects.get(projectId.value))
const pipeline = useAsync(() => api.run.pipeline(projectId.value))

/**
 * What the last run produced.
 *
 * Refetched whenever the pipeline settles, because the files are published at
 * the end of a run: a list fetched on mount would be empty for the whole of the
 * first run and then stay empty until the page was reloaded.
 */
const outputs = useAsync(() => api.projects.outputs(projectId.value))

/**
 * What the project occupies on disk.
 *
 * Refetched with the outputs, because they are the same subject seen twice —
 * one lists the finished files, the other counts everything — and a run changes
 * both.
 */
const files = useAsync(() => api.projects.files(projectId.value))

const filesError = ref<string | null>(null)
const removingKind = ref<FileGroupKind | null>(null)

async function removeGroup(kind: FileGroupKind, name: string, warning: string): Promise<void> {
  // The warning is the confirmation. These four categories look identical as
  // buttons and differ enormously in what losing them costs, so the question
  // repeats what the row already said rather than asking "are you sure".
  if (!confirm(`删除「${name}」？\n\n${warning}`)) return

  removingKind.value = kind
  filesError.value = null

  try {
    const result = await api.projects.removeFiles(projectId.value, kind)
    await Promise.all([files.run(), outputs.run()])
    void result
  } catch (cause) {
    filesError.value = cause instanceof ApiError ? cause.message : String(cause)
  } finally {
    removingKind.value = null
  }
}

/**
 * Live pipeline state, patched from the event stream.
 *
 * The server is the only thing that decides what a stage's state is. This
 * view applies the transitions it is told about and refetches on a resync; it
 * never infers that a stage finished from the arrival of another stage's event,
 * which is exactly the kind of guess that leaves a pipeline view permanently
 * wrong after one dropped message.
 */
const live = ref<PipelineView | null>(null)
const current = computed(() => live.value ?? pipeline.data.value)

/** stage.progress messages, newest last. */
const recentMessages = ref<{ stage: string; message: string; at: number }[]>([])
const actionError = ref<string | null>(null)
const acting = ref(false)

function applyStage(name: string, status: StageStatus, progress: number, patch: Partial<StageView>): void {
  const view = live.value ?? pipeline.data.value
  if (!view) return

  const stages = view.stages.map((stage) =>
    stage.name === name ? { ...stage, status, progress, ...patch } : stage,
  )
  live.value = { ...view, stages }
}

function onStageStatus(event: ServerEvent): void {
  const name = event.stage
  if (!name) return

  const data = event.data as Partial<StageView> & { status?: StageStatus; progress?: number }
  applyStage(name, (data.status ?? 'pending') as StageStatus, data.progress ?? 0, {
    duration_ms: data.duration_ms,
    error_code: data.error_code,
    error_message: data.error_message,
    artifact_id: data.artifact_id,
    reason: data.reason,
    metadata: data.metadata,
  })

  if (data.status === 'failed') {
    actionError.value = data.error_message ?? `${name} 阶段失败`
  }
  // A settled stage is when the job's own record changes, so the project header
  // is refreshed rather than guessed at from the stage event alone.
  if (data.status === 'completed' || data.status === 'failed' || data.status === 'cached') {
    void project.run()
  }
}

function onStageProgress(event: ServerEvent): void {
  const name = event.stage
  if (!name) return

  const data = event.data as { progress?: number; message?: string }
  applyStage(name, 'running', data.progress ?? 0, {})

  if (data.message) {
    recentMessages.value = [
      ...recentMessages.value.slice(-49),
      { stage: name, message: data.message, at: Date.now() },
    ]
  }
}

function onJobStatus(event: ServerEvent): void {
  const data = event.data as { status?: string }
  if (data.status === undefined) return

  void project.run()

  // The files are published just before this event is emitted, so this is the
  // moment the list becomes complete. Refetching on the stage events instead
  // would ask too early — the last stage settling is not the run being over.
  void outputs.run()
  void files.run()
}

/**
 * Only events for this project. The stream is multiplexed across every project
 * the server is running, so a view that applied all of them would show one
 * project's progress on another's pipeline.
 */
function scoped(handler: (event: ServerEvent) => void) {
  return (event: ServerEvent): void => {
    if (event.project_id && event.project_id !== projectId.value) return
    handler(event)
  }
}

const unsubscribers: (() => void)[] = []

onMounted(async () => {
  await Promise.all([project.run(), pipeline.run(), outputs.run(), files.run()])

  unsubscribers.push(
    events.on('stage.status', scoped(onStageStatus)),
    events.on('stage.progress', scoped(onStageProgress)),
    events.on('job.status', scoped(onJobStatus)),
  )

  // A resync means this view's copy may be stale, and the honest response is to
  // throw it away and ask again rather than to keep patching a base that might
  // be wrong.
  unsubscribers.push(
    events.on('resync.required', () => {
      live.value = null
      void project.run()
      void pipeline.run()
      void outputs.run()
      void files.run()
    }),
  )
})

onBeforeUnmount(() => {
  for (const unsubscribe of unsubscribers) unsubscribe()
})

watch(projectId, () => {
  live.value = null
  recentMessages.value = []
  void project.run()
  void pipeline.run()
  void outputs.run()
  void files.run()
})

const running = computed(() => current.value?.job?.status === 'running')
const progressSummary = computed(() => pipelineProgress(current.value))
const activeStage = computed(() => current.value?.stages.find((stage) => stage.status === 'running'))
const runButtonLabel = computed(() => current.value?.job ? '重新运行全部流程' : '开始处理')
const progressTone = computed(() => {
  if (current.value?.job?.status === 'completed') return 'bg-status-done'
  if (current.value?.job?.status === 'failed') return 'bg-status-failed'
  return 'bg-status-running'
})

async function rerunStage(stage: string): Promise<void> {
  acting.value = true
  actionError.value = null
  try {
    await api.run.runStage(projectId.value, stage, true)
    await pipeline.run()
  } catch (cause) {
    actionError.value = cause instanceof ApiError ? cause.message : String(cause)
  } finally {
    acting.value = false
  }
}

async function start(): Promise<void> {
  acting.value = true
  actionError.value = null
  try {
    const result = await api.run.start(projectId.value, {})
    live.value = result
    await project.run()
  } catch (cause) {
    actionError.value = cause instanceof ApiError ? cause.message : String(cause)
  } finally {
    acting.value = false
  }
}

async function cancel(): Promise<void> {
  acting.value = true
  actionError.value = null
  try {
    await api.run.cancel(projectId.value)
    await project.run()
    await pipeline.run()
  } catch (cause) {
    actionError.value = cause instanceof ApiError ? cause.message : String(cause)
  } finally {
    acting.value = false
  }
}

/** Stage indicators; the adjacent text label remains the authoritative cue. */
const STATUS_TONE: Record<StageStatus, string> = {
  pending: 'bg-status-pending',
  running: 'bg-status-running',
  cached: 'bg-status-done/60',
  completed: 'bg-status-done',
  failed: 'bg-status-failed',
  skipped: 'bg-status-pending/60',
  cancelled: 'bg-status-pending',
}

const KIND_LABEL: Record<ProjectOutput['kind'], string> = {
  subtitle: '字幕',
  video: '视频',
  log: '日志',
  other: '其他',
}

const STATUS_LABEL: Record<StageStatus, string> = {
  pending: '等待',
  running: '进行中',
  cached: '已缓存',
  completed: '完成',
  failed: '失败',
  skipped: '已跳过',
  cancelled: '已取消',
}
</script>

<template>
  <div class="mx-auto max-w-6xl space-y-6">
    <div v-if="project.error.value" class="rounded border border-status-failed/40 bg-surface-raised p-4 text-sm">
      <p class="text-status-failed">{{ project.error.value }}</p>
      <AppButton variant="ghost" size="sm" class="mt-2" @click="project.run">重试</AppButton>
    </div>

    <template v-else>
      <header class="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 class="text-lg font-medium">{{ project.data.value?.name ?? '…' }}</h1>
          <p class="mt-1 text-sm text-ink-muted">
            {{ project.data.value?.source_language }} → {{ project.data.value?.target_language }} ·
            {{ project.data.value?.style }} ·
            {{ formatDuration(project.data.value?.duration) }} ·
            {{ project.data.value?.segment_count ?? 0 }} 行
          </p>
          <p v-if="project.data.value && project.data.value.needs_review_count > 0" class="mt-1 text-sm text-status-running">
            {{ project.data.value.needs_review_count }} 行待审校
          </p>
        </div>

        <div class="flex flex-wrap gap-2">
          <AppButton
            v-if="!running"
            data-test="rerun-project"
            :variant="current?.job ? 'secondary' : 'primary'"
            :disabled="acting"
            @click="start"
          >
            {{ runButtonLabel }}
          </AppButton>
          <AppButton v-else variant="danger" :disabled="acting" @click="cancel">取消</AppButton>
        </div>
      </header>

      <p v-if="actionError" class="rounded border border-status-failed/40 bg-surface-raised p-3 text-sm text-status-failed">
        {{ actionError }}
      </p>

      <section
        v-if="current?.job"
        class="rounded border border-line bg-surface-raised p-4"
        aria-live="polite"
      >
        <div class="flex items-start justify-between gap-4">
          <div>
            <p class="text-sm font-medium">
              {{ running ? '任务正在运行' : current.job.status === 'completed' ? '任务已完成' : `任务状态：${current.job.status}` }}
            </p>
            <p class="mt-1 text-xs text-ink-muted">
              <template v-if="activeStage">
                当前：{{ activeStage.label }} · 第 {{ progressSummary.current }}/{{ progressSummary.total }} 阶段
              </template>
              <template v-else>
                已完成 {{ progressSummary.completed }}/{{ progressSummary.total }} 个阶段
              </template>
            </p>
          </div>
          <span class="text-lg font-medium tabular-nums">
            {{ Math.round(progressSummary.fraction * 100) }}%
          </span>
        </div>
        <div class="mt-3 h-2 overflow-hidden rounded bg-surface-sunken">
          <div
            data-test="overall-progress-bar"
            class="h-full transition-[width] duration-300"
            :class="progressTone"
            :style="{ width: `${Math.round(progressSummary.fraction * 100)}%` }"
          />
        </div>
        <p v-if="activeStage && (activeStage.progress ?? 0) <= 0" class="mt-2 text-xs text-ink-faint">
          这个阶段暂时无法计算精确百分比，完成后会自动进入下一阶段。
        </p>
      </section>

      <section
        v-if="current?.job"
        data-test="project-outputs"
        class="rounded border border-line bg-surface-raised"
      >
        <header class="flex flex-wrap items-start justify-between gap-3 border-b border-line px-4 py-3">
          <div>
            <h2 class="text-sm font-medium">处理结果</h2>
            <p class="mt-1 text-xs text-ink-faint">
              这里显示最近一次运行产物；审校修改后，请生成最终成品再下载。
            </p>
          </div>
          <span v-if="outputs.data.value?.items.length" class="text-xs text-ink-faint">
            {{ outputs.data.value.items.length }} 个文件
          </span>
        </header>

        <div v-if="outputs.loading.value && !outputs.data.value" class="px-4 py-6 text-sm text-ink-muted">
          正在读取产物…
        </div>
        <ul v-else-if="outputs.data.value?.items.length" class="divide-y divide-line/60">
          <li
            v-for="file in outputs.data.value.items"
            :key="file.name"
            class="flex items-center justify-between gap-4 px-4 py-3"
          >
            <div class="min-w-0">
              <p class="truncate text-sm">{{ file.name }}</p>
              <p class="mt-0.5 text-xs text-ink-faint">
                {{ KIND_LABEL[file.kind] }} · {{ formatBytes(file.size_bytes) }} ·
                {{ formatTime(file.modified_at) }}
              </p>
              <p v-if="file.description" class="mt-1 text-xs text-ink-muted">
                {{ file.description }}
              </p>
            </div>
            <a
              :href="api.projects.outputURL(projectId, file.name)"
              download
              class="shrink-0 rounded border border-line px-3 py-1.5 text-xs text-ink-muted outline-none transition hover:border-accent hover:text-ink focus-visible:ring-2 focus-visible:ring-accent"
            >
              下载
            </a>
          </li>
        </ul>
        <div v-else class="px-4 py-6">
          <p class="text-sm text-ink-muted">
            {{ running ? '任务完成后，可下载的字幕和视频会出现在这里。' : '最近一次运行没有生成可下载文件。' }}
          </p>
          <p v-if="!running" class="mt-1 text-xs text-ink-faint">可查看处理阶段是否被跳过或失败。</p>
        </div>

        <p v-if="outputs.data.value?.items.length" class="border-t border-line px-4 py-2 text-xs text-ink-faint">
          文件在 <code class="font-mono">{{ outputs.data.value.dir }}</code>
        </p>
      </section>

      <section
        v-if="(project.data.value?.segment_count ?? 0) > 0"
        data-test="review-handoff"
        class="rounded border border-accent/40 bg-surface-raised p-4"
      >
        <div class="flex flex-wrap items-start justify-between gap-4">
          <div>
            <h2 class="text-sm font-medium">字幕审校</h2>
            <template v-if="(project.data.value?.needs_review_count ?? 0) > 0">
              <p class="mt-1 text-sm text-ink-muted">
                有 <span class="font-medium text-status-running">{{ project.data.value?.needs_review_count }} 条需要人工确认</span>。
                质量检查会说明每一条为什么进入队列。
              </p>
            </template>
            <p v-else class="mt-1 text-sm text-status-done">没有待处理的质量问题，可以生成最终成品。</p>
          </div>

          <RouterLink
            v-if="(project.data.value?.needs_review_count ?? 0) > 0"
            data-test="open-review-queue"
            :to="`/projects/${projectId}/editor?filter=review`"
            class="shrink-0 rounded bg-accent px-3 py-1.5 text-sm font-medium text-accent-ink outline-none transition hover:brightness-110 focus-visible:ring-2 focus-visible:ring-accent"
          >
            打开待审队列
          </RouterLink>
          <AppButton
            v-else
            data-test="generate-final-outputs"
            variant="primary"
            :disabled="acting || running"
            @click="start"
          >
            {{ acting || running ? '生成中…' : '生成最终成品' }}
          </AppButton>
        </div>

        <ol
          v-if="(project.data.value?.needs_review_count ?? 0) > 0"
          class="mt-4 grid gap-2 text-xs text-ink-muted md:grid-cols-4"
          aria-label="字幕审校步骤"
        >
          <li class="rounded border border-line bg-surface px-3 py-2"><span class="text-accent">1</span> 选择问题段</li>
          <li class="rounded border border-line bg-surface px-3 py-2"><span class="text-accent">2</span> 定位播放核对</li>
          <li class="rounded border border-line bg-surface px-3 py-2"><span class="text-accent">3</span> 修改译文或时间</li>
          <li class="rounded border border-line bg-surface px-3 py-2"><span class="text-accent">4</span> 保存并通过</li>
        </ol>
        <p v-else class="mt-3 text-xs text-ink-faint">
          系统会复用识别和翻译缓存，只重新生成受字幕修改影响的文件。
        </p>
      </section>

      <section class="rounded border border-line bg-surface-raised">
        <header class="flex items-center justify-between border-b border-line px-4 py-2">
          <h2 class="text-sm font-medium text-ink-muted">处理阶段</h2>
          <span class="text-xs text-ink-faint">缓存阶段不会重复计算</span>
        </header>

        <ol class="divide-y divide-line/60">
          <li v-for="stage in current?.stages ?? []" :key="stage.name" class="px-4 py-2.5">
            <div class="flex items-center gap-3">
              <span class="h-2 w-2 shrink-0 rounded-full" :class="STATUS_TONE[stage.status]" />
              <!-- The label comes from the server, so a stage added after this
                   UI shipped still displays something meaningful. -->
              <span class="w-40 shrink-0 text-sm">{{ stage.label }}</span>
              <span class="w-16 shrink-0 text-xs text-ink-faint">{{ STATUS_LABEL[stage.status] }}</span>

              <div v-if="stage.status === 'running'" class="h-1 flex-1 overflow-hidden rounded bg-surface-sunken">
                <div
                  class="h-full bg-status-running transition-[width] duration-300"
                  :class="(stage.progress ?? 0) <= 0 ? 'w-1/2 animate-pulse' : ''"
                  :style="(stage.progress ?? 0) > 0 ? { width: `${Math.round((stage.progress ?? 0) * 100)}%` } : undefined"
                />
              </div>

              <span v-else class="flex-1 text-xs text-ink-faint">
                {{ stage.status === 'cached' ? '直接使用已有结果' : stage.status === 'completed' ? '处理完成' : '' }}
              </span>

              <span v-if="stage.duration_ms" class="w-16 shrink-0 text-right text-xs tabular-nums text-ink-faint">
                {{ (stage.duration_ms / 1000).toFixed(1) }}s
              </span>
              <AppButton
                v-if="stage.status === 'failed'"
                variant="ghost"
                size="sm"
                :disabled="acting || running"
                @click="rerunStage(stage.name)"
              >
                重跑此阶段
              </AppButton>
            </div>

            <p v-if="stage.reason" class="mt-1 pl-5 text-xs text-ink-faint">{{ stage.reason }}</p>
            <p v-if="stage.error_message" class="mt-1 pl-5 text-xs text-status-failed">
              {{ stage.error_message }}
            </p>
          </li>
        </ol>
      </section>

      <section v-if="recentMessages.length" class="rounded border border-line bg-surface-raised">
        <header class="border-b border-line px-4 py-2">
          <h2 class="text-sm font-medium text-ink-muted">进度</h2>
        </header>
        <ul class="max-h-48 overflow-y-auto px-4 py-2 font-mono text-xs">
          <li v-for="(entry, index) in recentMessages" :key="index" class="flex gap-3 py-0.5">
            <span class="w-28 shrink-0 text-ink-faint">{{ entry.stage }}</span>
            <span class="truncate">{{ entry.message }}</span>
          </li>
        </ul>
      </section>

      <section
        v-if="files.data.value && files.data.value.items.length"
        class="rounded border border-line bg-surface-raised"
      >
        <header class="flex items-center justify-between border-b border-line px-4 py-2">
          <h2 class="text-sm font-medium text-ink-muted">磁盘占用</h2>
          <span class="text-xs text-ink-faint">共 {{ formatBytes(files.data.value.total_bytes) }}</span>
        </header>

        <ul class="divide-y divide-line/60">
          <li v-for="group in files.data.value.items" :key="group.kind" class="px-4 py-3">
            <div class="flex items-start justify-between gap-4">
              <div class="min-w-0">
                <div class="flex flex-wrap items-center gap-2">
                  <span class="text-sm">{{ group.name }}</span>
                  <AppBadge>{{ formatBytes(group.bytes) }}</AppBadge>
                  <span v-if="group.files > 0" class="text-xs text-ink-faint">
                    {{ group.files }} 个文件
                  </span>
                </div>
                <p class="mt-1 text-xs text-ink-faint">{{ group.warning }}</p>
              </div>

              <AppButton
                v-if="group.removable && group.bytes > 0"
                :variant="group.kind === 'source' ? 'danger' : 'secondary'"
                size="sm"
                class="shrink-0"
                :disabled="removingKind !== null"
                @click="removeGroup(group.kind, group.name, group.warning)"
              >
                {{ removingKind === group.kind ? '删除中…' : '删除' }}
              </AppButton>
            </div>

            <ul v-if="group.kind === 'logs' && files.data.value.logs.length" class="mt-2 space-y-0.5">
              <li
                v-for="log in files.data.value.logs"
                :key="log.name"
                class="flex items-center justify-between gap-3 text-xs"
              >
                <a
                  :href="api.projects.logURL(projectId, log.name)"
                  target="_blank"
                  rel="noopener"
                  class="truncate font-mono text-ink-muted transition hover:text-accent"
                >
                  {{ log.name }}
                </a>
                <span class="shrink-0 text-ink-faint">
                  {{ formatBytes(log.size_bytes) }} · {{ formatTime(log.modified_at) }}
                </span>
              </li>
            </ul>
          </li>
        </ul>

        <p v-if="filesError" class="whitespace-pre-line border-t border-line px-4 py-2 text-xs text-status-failed">
          {{ filesError }}
        </p>
      </section>
    </template>
  </div>
</template>
