<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { RouterLink, useRoute } from 'vue-router'

import { ApiError, api } from '@/api/client'
import { formatDuration, useAsync } from '@/composables/useAsync'
import { useEventStore } from '@/stores/events'
import type { QCFinding, QCSeverity, Segment } from '@/types/api'

const route = useRoute()
const events = useEventStore()
const projectId = computed(() => String(route.params['id']))

const severity = ref<QCSeverity | 'all'>('all')

const findings = useAsync(() =>
  api.qc.list(projectId.value, {
    limit: 300,
    ...(severity.value === 'all' ? {} : { severity: severity.value }),
  }),
)

/**
 * The lines the findings point at, fetched once.
 *
 * A finding carries a message and a code but not the text it is about, and a
 * reviewer looking at "the glossary entry たなか → 田中 does not appear" needs
 * to see the line. Fetching per finding would be a request per row.
 */
const lines = useAsync(() => api.segments.list(projectId.value, { limit: 500 }))

const lineById = computed(() => {
  const map = new Map<string, Segment>()
  for (const line of lines.data.value?.items ?? []) map.set(line.id, line)
  return map
})

const actionError = ref<string | null>(null)

const summary = computed(() => findings.data.value?.summary ?? { error: 0, warning: 0, info: 0 })

const visible = computed(() => findings.data.value?.items ?? [])

function onResync(): void {
  void findings.run()
  void lines.run()
}

const unsubscribers: (() => void)[] = []

onMounted(async () => {
  await Promise.all([findings.run(), lines.run()])

  // A run finishing replaces the whole report, and an edit can resolve a
  // finding by fixing its cause. Both mean the queue is stale.
  unsubscribers.push(
    events.on('segments.replaced', onResync),
    events.on('segment.updated', onResync),
    events.on('resync.required', onResync),
  )
})

onBeforeUnmount(() => {
  for (const unsubscribe of unsubscribers) unsubscribe()
})

async function resolve(finding: QCFinding, resolved: boolean): Promise<void> {
  actionError.value = null
  try {
    await api.qc.resolve(projectId.value, finding.id, resolved)
    // Refetched rather than filtered locally: the summary counts come from the
    // server, and recomputing them here would be a second answer to the same
    // question.
    await findings.run()
  } catch (cause) {
    actionError.value = cause instanceof ApiError ? cause.message : String(cause)
  }
}

const SEVERITY_TONE: Record<QCSeverity, string> = {
  error: 'text-status-failed',
  warning: 'text-status-running',
  info: 'text-ink-faint',
}

const SEVERITY_LABEL: Record<QCSeverity, string> = {
  error: '错误',
  warning: '警告',
  info: '提示',
}
</script>

<template>
  <div class="space-y-4">
    <div class="flex flex-wrap items-center gap-3">
      <div class="flex rounded border border-line bg-surface-raised">
        <button
          v-for="option in (['all', 'error', 'warning', 'info'] as const)"
          :key="option"
          class="px-3 py-1.5 text-sm transition"
          :class="severity === option ? 'bg-accent text-accent-ink' : 'text-ink-muted hover:text-ink'"
          @click="severity = option"
        >
          {{ option === 'all' ? '全部' : SEVERITY_LABEL[option] }}
        </button>
      </div>

      <p class="text-sm text-ink-muted">
        <span class="text-status-failed">{{ summary.error }}</span> 错误 ·
        <span class="text-status-running">{{ summary.warning }}</span> 警告 ·
        <span class="text-ink-faint">{{ summary.info }}</span> 提示
      </p>

      <RouterLink
        :to="`/projects/${projectId}/editor`"
        class="rounded border border-line px-3 py-1.5 text-sm text-ink-muted transition hover:border-accent hover:text-ink"
      >
        打开编辑器
      </RouterLink>
    </div>

    <p v-if="actionError" class="rounded border border-status-failed/40 bg-surface-raised p-3 text-sm text-status-failed">
      {{ actionError }}
    </p>

    <p v-if="findings.error.value" class="rounded border border-status-failed/40 bg-surface-raised p-3 text-sm text-status-failed">
      {{ findings.error.value }}
      <button class="ml-2 text-accent hover:underline" @click="findings.run">重试</button>
    </p>

    <div v-else-if="findings.loading.value && !findings.data.value" class="p-6 text-center text-sm text-ink-muted">
      加载中…
    </div>

    <div
      v-else-if="visible.length === 0"
      class="rounded border border-dashed border-line p-10 text-center text-sm text-ink-muted"
    >
      {{ summary.error + summary.warning + summary.info === 0
        ? '没有未处理的问题。运行一次 Pipeline 会生成质量报告。'
        : '这个筛选条件下没有内容。' }}
    </div>

    <ul v-else class="divide-y divide-line/60 rounded border border-line">
      <li v-for="finding in visible" :key="finding.id" class="px-4 py-3">
        <div class="flex items-start gap-4">
          <span class="w-12 shrink-0 pt-0.5 text-xs" :class="SEVERITY_TONE[finding.severity]">
            {{ SEVERITY_LABEL[finding.severity] }}
          </span>

          <div class="min-w-0 flex-1">
            <p class="text-sm">{{ finding.message }}</p>

            <!-- The line the finding is about. Its absence is itself
                 information: a finding with no line is about the project. -->
            <div
              v-if="finding.segment_id && lineById.get(finding.segment_id)"
              class="mt-1 rounded bg-surface-sunken px-2 py-1 text-xs"
            >
              <div class="flex gap-2 text-ink-faint">
                <span class="tabular-nums">
                  {{ formatDuration(lineById.get(finding.segment_id)?.start) }}
                </span>
                <span class="truncate">{{ lineById.get(finding.segment_id)?.source_text }}</span>
              </div>
              <div class="text-ink-muted">
                {{ lineById.get(finding.segment_id)?.translated_text ?? '（未翻译）' }}
              </div>
            </div>

            <p v-if="finding.suggestion" class="mt-1 text-xs text-ink-faint">
              {{ finding.suggestion }}
            </p>

            <div class="mt-1.5 flex items-center gap-3 text-xs">
              <code class="text-ink-faint">{{ finding.code }}</code>
              <RouterLink
                v-if="finding.segment_id"
                :to="`/projects/${projectId}/editor`"
                class="text-ink-faint hover:text-accent"
              >
                去修改
              </RouterLink>
              <button class="text-ink-faint hover:text-status-done" @click="resolve(finding, true)">
                标记已处理
              </button>
            </div>
          </div>
        </div>
      </li>
    </ul>
  </div>
</template>
