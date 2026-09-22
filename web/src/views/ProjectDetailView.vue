<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'

import { ApiError, api } from '@/api/client'
import { formatDuration, useAsync } from '@/composables/useAsync'
import { useEventStore, type ServerEvent } from '@/stores/events'
import type { PipelineView, StageStatus, StageView } from '@/types/api'

const route = useRoute()
const events = useEventStore()
const projectId = computed(() => String(route.params['id']))

const project = useAsync(() => api.projects.get(projectId.value))
const pipeline = useAsync(() => api.run.pipeline(projectId.value))

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
  if (data.status !== undefined) void project.run()
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
  await Promise.all([project.run(), pipeline.run()])

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
})

const running = computed(() => current.value?.job?.status === 'running')

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

/** Status colours, shared so "green means done" is learned once. */
const STATUS_TONE: Record<StageStatus, string> = {
  pending: 'bg-status-pending',
  running: 'bg-status-running',
  cached: 'bg-status-done/60',
  completed: 'bg-status-done',
  failed: 'bg-status-failed',
  skipped: 'bg-status-pending/60',
  cancelled: 'bg-status-pending',
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
  <div class="space-y-6">
    <div v-if="project.error.value" class="rounded border border-status-failed/40 bg-surface-raised p-4 text-sm">
      <p class="text-status-failed">{{ project.error.value }}</p>
      <button class="mt-2 text-accent hover:underline" @click="project.run">重试</button>
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

        <div class="flex gap-2">
          <button
            v-if="!running"
            :disabled="acting"
            class="rounded bg-accent px-3 py-1.5 text-sm font-medium text-accent-ink transition hover:opacity-90 disabled:opacity-40"
            @click="start"
          >
            运行
          </button>
          <button
            v-else
            :disabled="acting"
            class="rounded border border-status-failed px-3 py-1.5 text-sm text-status-failed transition hover:bg-status-failed/10 disabled:opacity-40"
            @click="cancel"
          >
            取消
          </button>
        </div>
      </header>

      <p v-if="actionError" class="rounded border border-status-failed/40 bg-surface-raised p-3 text-sm text-status-failed">
        {{ actionError }}
      </p>

      <section class="rounded border border-line bg-surface-raised">
        <header class="flex items-center justify-between border-b border-line px-4 py-2">
          <h2 class="text-sm font-medium text-ink-muted">Pipeline</h2>
          <span v-if="current?.job" class="text-xs text-ink-faint">
            {{ current.job.status }} · {{ (current.job.progress * 100).toFixed(0) }}%
          </span>
        </header>

        <ol class="divide-y divide-line/60">
          <li v-for="stage in current?.stages ?? []" :key="stage.name" class="px-4 py-2.5">
            <div class="flex items-center gap-3">
              <span class="h-2 w-2 shrink-0 rounded-full" :class="STATUS_TONE[stage.status]" />
              <!-- The label comes from the server, so a stage added after this
                   UI shipped still displays something meaningful. -->
              <span class="w-40 shrink-0 text-sm">{{ stage.label }}</span>
              <span class="w-16 shrink-0 text-xs text-ink-faint">{{ STATUS_LABEL[stage.status] }}</span>

              <div class="h-1 flex-1 overflow-hidden rounded bg-surface-sunken">
                <div
                  class="h-full bg-status-running transition-[width] duration-300"
                  :style="{ width: `${Math.round((stage.progress ?? 0) * 100)}%` }"
                />
              </div>

              <span v-if="stage.duration_ms" class="w-16 shrink-0 text-right text-xs tabular-nums text-ink-faint">
                {{ (stage.duration_ms / 1000).toFixed(1) }}s
              </span>
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

      <p class="text-sm text-ink-faint">
        字幕编辑与审校队列在 Phase 8 实现。
      </p>
    </template>
  </div>
</template>
