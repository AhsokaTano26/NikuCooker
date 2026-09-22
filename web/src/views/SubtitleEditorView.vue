<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'

import { ApiError, api, API_BASE } from '@/api/client'
import AppButton from '@/components/AppButton.vue'
import { formatDuration, useAsync } from '@/composables/useAsync'
import { useEventStore, type ServerEvent } from '@/stores/events'
import type { Segment } from '@/types/api'

const route = useRoute()
const events = useEventStore()
const projectId = computed(() => String(route.params['id']))

const lines = useAsync(() => api.segments.list(projectId.value, { limit: 500, include_qc: true }))

/** Which line is open for editing. */
const editingId = ref<string | null>(null)
const draft = ref('')
const saving = ref(false)
const actionError = ref<string | null>(null)

/** Filters that change what the list shows rather than what the server returns. */
const onlyReview = ref(false)
const search = ref('')

const visible = computed(() => {
  const items = lines.data.value?.items ?? []
  const needle = search.value.trim()

  return items.filter((line) => {
    if (onlyReview.value && !line.needs_review) return false
    if (needle === '') return true
    return (
      line.source_text.includes(needle) ||
      (line.translated_text ?? '').includes(needle)
    )
  })
})

const reviewCount = computed(
  () => (lines.data.value?.items ?? []).filter((line) => line.needs_review).length,
)

/**
 * The server's copy is the only copy.
 *
 * `segment.updated` carries the whole record, and it is applied verbatim rather
 * than merged into the local one. A merge would let a field the client never
 * touched drift, which is how two tabs end up disagreeing about the same line.
 */
function onSegmentUpdated(event: ServerEvent): void {
  const updated = (event.data as { segment?: Segment }).segment
  if (!updated || !lines.data.value) return

  const items = lines.data.value.items.map((line) =>
    line.id === updated.id ? updated : line,
  )
  lines.data.value = { ...lines.data.value, items }
}

function onSegmentsReplaced(event: ServerEvent): void {
  // Splitting or merging renumbers everything after the change, so the list is
  // refetched rather than patched.
  if (event.project_id !== projectId.value) return
  void lines.run()
}

const unsubscribers: (() => void)[] = []

onMounted(async () => {
  await lines.run()
  unsubscribers.push(
    events.on('segment.updated', onSegmentUpdated),
    events.on('segments.replaced', onSegmentsReplaced),
    events.on('resync.required', () => void lines.run()),
  )
})

onBeforeUnmount(() => {
  for (const unsubscribe of unsubscribers) unsubscribe()
})

function beginEdit(line: Segment): void {
  editingId.value = line.id
  draft.value = line.translated_text ?? ''

  // Focused on the next tick, because the textarea does not exist until the
  // render triggered by editingId has happened.
  void nextTick(() => {
    const element = document.getElementById(`editor-${line.id}`)
    if (element instanceof HTMLTextAreaElement) {
      element.focus()
      element.select()
    }
  })
}

async function commit(): Promise<void> {
  const id = editingId.value
  if (!id) return

  saving.value = true
  actionError.value = null
  try {
    await api.segments.update(projectId.value, id, { translated_text: draft.value })
    editingId.value = null
  } catch (cause) {
    actionError.value = cause instanceof ApiError ? cause.message : String(cause)
  } finally {
    saving.value = false
  }
}

function cancel(): void {
  editingId.value = null
  draft.value = ''
}

async function translate(line: Segment): Promise<void> {
  actionError.value = null
  try {
    await api.segments.translate(projectId.value, line.id)
  } catch (cause) {
    actionError.value = cause instanceof ApiError ? cause.message : String(cause)
  }
}

async function decide(line: Segment, state: Segment['review_state']): Promise<void> {
  actionError.value = null
  try {
    await api.segments.review(projectId.value, line.id, state)
  } catch (cause) {
    actionError.value = cause instanceof ApiError ? cause.message : String(cause)
  }
}

async function split(line: Segment): Promise<void> {
  actionError.value = null

  // Split at the midpoint of the line rather than asking for a time. The user
  // then drags the boundary; asking for a number they have no way to know
  // would be a worse starting point than a plausible one.
  const midpoint = line.start + (line.end - line.start) / 2
  try {
    await api.segments.split(projectId.value, line.id, midpoint)
  } catch (cause) {
    actionError.value = cause instanceof ApiError ? cause.message : String(cause)
  }
}

async function merge(line: Segment): Promise<void> {
  actionError.value = null
  try {
    await api.segments.merge(projectId.value, line.id, true)
  } catch (cause) {
    actionError.value = cause instanceof ApiError ? cause.message : String(cause)
  }
}

/** Reading speed, coloured by how far over the limit it is. */
function cpsTone(line: Segment): string {
  if (line.cps === null) return 'text-ink-faint'
  if (line.cps > 18) return 'text-status-failed'
  if (line.cps > 12) return 'text-status-running'
  return 'text-ink-faint'
}
</script>

<template>
  <div class="space-y-4">
    <div class="flex flex-wrap items-center gap-3">
      <AppCheckbox v-model="onlyReview" :label="`只看待审校${reviewCount > 0 ? ` (${reviewCount})` : ''}`" />

      <div class="min-w-48 flex-1">
        <AppInput v-model="search" type="search" placeholder="搜索原文或译文" />
      </div>

      <a
        v-for="format in ['srt', 'ass']"
        :key="format"
        :href="`${API_BASE}/projects/${projectId}/subtitles.${format}`"
        class="rounded border border-line px-3 py-1.5 text-sm text-ink-muted transition outline-none hover:border-accent hover:text-ink focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent"
      >
        导出 {{ format.toUpperCase() }}
      </a>
    </div>

    <p v-if="actionError" class="rounded border border-status-failed/40 bg-surface-raised p-3 text-sm text-status-failed">
      {{ actionError }}
    </p>

    <p v-if="lines.error.value" class="rounded border border-status-failed/40 bg-surface-raised p-3 text-sm text-status-failed">
      {{ lines.error.value }}
      <AppButton variant="ghost" size="sm" class="ml-2" @click="lines.run">重试</AppButton>
    </p>

    <div v-else-if="lines.loading.value && !lines.data.value" class="p-6 text-center text-sm text-ink-muted">
      加载中…
    </div>

    <div
      v-else-if="visible.length === 0"
      class="rounded border border-dashed border-line p-10 text-center text-sm text-ink-muted"
    >
      {{ lines.data.value?.items.length === 0 ? '这个项目还没有字幕行，先运行一次 Pipeline。' : '没有匹配的行。' }}
    </div>

    <ol v-else class="divide-y divide-line/60 rounded border border-line">
      <li
        v-for="line in visible"
        :key="line.id"
        class="px-4 py-3 transition"
        :class="line.needs_review ? 'bg-surface-raised' : ''"
      >
        <div class="flex gap-4">
          <div class="w-20 shrink-0 pt-0.5 text-xs tabular-nums text-ink-faint">
            <div>{{ formatDuration(line.start) }}</div>
            <div :class="cpsTone(line)">
              {{ line.cps === null ? '—' : line.cps.toFixed(1) }}
            </div>
          </div>

          <div class="min-w-0 flex-1">
            <p class="text-sm text-ink-muted">{{ line.source_text }}</p>

            <AppTextarea
              v-if="editingId === line.id"
              :id="`editor-${line.id}`"
              v-model="draft"
              :rows="2"
              @keydown.enter.exact.prevent="commit"
              @keydown.esc.prevent="cancel"
            />
            <p
              v-else
              class="mt-1 cursor-text text-sm"
              :class="line.translated_text ? '' : 'italic text-ink-faint'"
              @click="beginEdit(line)"
            >
              {{ line.translated_text ?? '（未翻译）' }}
            </p>

            <!-- QC findings are shown inline. A separate panel would make the
                 reviewer hold the line number in their head while they look at
                 it. -->
            <ul v-if="line.qc?.length" class="mt-1 space-y-0.5">
              <li
                v-for="finding in line.qc"
                :key="finding.id"
                class="text-xs"
                :class="finding.severity === 'error' ? 'text-status-failed' : 'text-status-running'"
              >
                {{ finding.message }}
              </li>
            </ul>

            <div class="mt-1.5 flex flex-wrap items-center gap-3 text-xs">
              <template v-if="editingId === line.id">
                <AppButton variant="ghost" size="sm" :disabled="saving" @click="commit">
                  {{ saving ? '保存中…' : '保存' }}
                </AppButton>
                <AppButton variant="ghost" size="sm" @click="cancel">取消</AppButton>
              </template>
              <template v-else>
                <AppButton variant="ghost" size="sm" @click="translate(line)">重译</AppButton>
                <AppButton variant="ghost" size="sm" @click="split(line)">拆分</AppButton>
                <AppButton variant="ghost" size="sm" @click="merge(line)">合并下一行</AppButton>
                <AppButton variant="ghost" size="sm" @click="decide(line, 'approved')">通过</AppButton>
                <AppButton variant="danger" size="sm" @click="decide(line, 'rejected')">打回</AppButton>

                <span v-if="line.is_edited" class="text-ink-faint">已手工修改</span>
                <span v-else-if="line.review_state !== 'none'" class="text-ink-faint">
                  {{ line.review_state }}
                </span>
              </template>
            </div>
          </div>
        </div>
      </li>
    </ol>

    <p v-if="lines.data.value" class="text-xs text-ink-faint">
      共 {{ lines.data.value.total }} 行，显示 {{ visible.length }} 行
    </p>
  </div>
</template>
