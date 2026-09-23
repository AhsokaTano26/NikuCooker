<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'

import {
  ApiError,
  api,
  type BulkSegmentAction,
  type EditorMedia,
  type ListSegmentsQuery,
} from '@/api/client'
import AppBadge from '@/components/AppBadge.vue'
import AppButton from '@/components/AppButton.vue'
import AppCheckbox from '@/components/AppCheckbox.vue'
import AppInput from '@/components/AppInput.vue'
import AppSelect from '@/components/AppSelect.vue'
import AppTextarea from '@/components/AppTextarea.vue'
import { findActiveSegmentIndex } from '@/composables/workbench'
import { formatDuration, useAsync } from '@/composables/useAsync'
import { useEventStore, type ServerEvent } from '@/stores/events'
import type { QCSeverity, Segment } from '@/types/api'

const PAGE_SIZE = 200
const ROW_HEIGHT = 92
const OVERSCAN = 8

const route = useRoute()
const router = useRouter()
const events = useEventStore()
const projectId = computed(() => String(route.params['id']))

const project = useAsync(() => api.projects.get(projectId.value))
const media = useAsync(() => api.projects.editorMedia(projectId.value))

const rows = ref<Record<number, Segment>>({})
const total = ref(0)
const listLoading = ref(false)
const listError = ref<string | null>(null)
const loadingPages = new Set<string>()
let listGeneration = 0

const search = ref('')
const onlyReview = ref(route.query['filter'] === 'review')
const severity = ref<QCSeverity | ''>('')

const scroller = ref<HTMLElement | null>(null)
const firstVisible = ref(0)
const visibleCount = ref(12)

const startIndex = computed(() => Math.max(0, firstVisible.value - OVERSCAN))
const endIndex = computed(() => Math.min(total.value, firstVisible.value + visibleCount.value + OVERSCAN))
const visibleRows = computed(() => {
  const out: { index: number; line?: Segment }[] = []
  for (let index = startIndex.value; index < endIndex.value; index++) {
    out.push({ index, line: rows.value[index] })
  }
  return out
})
const topSpace = computed(() => startIndex.value * ROW_HEIGHT)
const bottomSpace = computed(() => Math.max(0, (total.value - endIndex.value) * ROW_HEIGHT))

function listQuery(offset: number): ListSegmentsQuery {
  return {
    limit: PAGE_SIZE,
    offset,
    include_qc: true,
    q: search.value.trim() || undefined,
    needs_review: onlyReview.value || undefined,
    qc_severity: severity.value || undefined,
    order: 'ordinal',
  }
}

async function loadPage(page: number): Promise<void> {
  const generation = listGeneration
  const key = `${generation}:${page}`
  if (loadingPages.has(key)) return
  loadingPages.add(key)
  listLoading.value = true
  listError.value = null
  try {
    const response = await api.segments.list(projectId.value, listQuery(page * PAGE_SIZE))
    if (generation !== listGeneration) return
    const next = { ...rows.value }
    response.items.forEach((line, position) => {
      next[page * PAGE_SIZE + position] = line
    })
    rows.value = next
    total.value = response.total
  } catch (cause) {
    listError.value = cause instanceof ApiError ? cause.message : String(cause)
  } finally {
    loadingPages.delete(key)
    listLoading.value = false
  }
}

function ensureWindow(): void {
  const firstPage = Math.floor(startIndex.value / PAGE_SIZE)
  const lastPage = Math.floor(Math.max(startIndex.value, endIndex.value - 1) / PAGE_SIZE)
  for (let page = firstPage; page <= lastPage; page++) void loadPage(page)
}

async function resetList(): Promise<void> {
  listGeneration++
  rows.value = {}
  total.value = 0
  firstVisible.value = 0
  if (scroller.value) scroller.value.scrollTop = 0
  await loadPage(0)
  ensureWindow()
}

let filterTimer: number | undefined
watch([search, onlyReview, severity], () => {
  window.clearTimeout(filterTimer)
  filterTimer = window.setTimeout(() => void resetList(), 250)
})

function onScroll(): void {
  const element = scroller.value
  if (!element) return
  firstVisible.value = Math.floor(element.scrollTop / ROW_HEIGHT)
  visibleCount.value = Math.ceil(element.clientHeight / ROW_HEIGHT)
  ensureWindow()
}

const selected = ref<Segment | null>(null)
const draftText = ref('')
const draftStart = ref('')
const draftEnd = ref('')
const draftSpeaker = ref('')
const draftTags = ref('')
const saving = ref(false)
const actionError = ref<string | null>(null)
const notice = ref<string | null>(null)

function seedDraft(line: Segment): void {
  selected.value = line
  draftText.value = line.translated_text ?? ''
  draftStart.value = line.start.toFixed(3)
  draftEnd.value = line.end.toFixed(3)
  draftSpeaker.value = line.speaker ?? ''
  draftTags.value = line.tags.join(', ')
  void router.replace({ query: { ...route.query, segment: line.id } })
}

const dirty = computed(() => {
  const line = selected.value
  if (!line) return false
  return (
    draftText.value !== (line.translated_text ?? '') ||
    Number(draftStart.value) !== line.start ||
    Number(draftEnd.value) !== line.end ||
    draftSpeaker.value !== (line.speaker ?? '') ||
    draftTags.value !== line.tags.join(', ')
  )
})

async function selectLine(line: Segment, seek = true): Promise<void> {
  if (dirty.value && selected.value?.id !== line.id) {
    const leave = confirm('当前字幕还有未保存修改。放弃这些修改并切换吗？')
    if (!leave) return
  }
  seedDraft(line)
  if (seek) seekTo(line.start)
}

function resetDraft(): void {
  if (selected.value) seedDraft(selected.value)
}

function patchLine(updated: Segment): void {
  const next = { ...rows.value }
  for (const [index, line] of Object.entries(next)) {
    if (line.id === updated.id) next[Number(index)] = updated
  }
  rows.value = next
  if (selected.value?.id === updated.id) seedDraft(updated)
}

async function save(): Promise<void> {
  const line = selected.value
  if (!line || !dirty.value) return
  const start = Number(draftStart.value)
  const end = Number(draftEnd.value)
  if (!Number.isFinite(start) || !Number.isFinite(end) || start < 0 || end <= start) {
    actionError.value = '时间码无效：结束时间必须大于开始时间。'
    return
  }

  saving.value = true
  actionError.value = null
  try {
    const updated = await api.segments.update(projectId.value, line.id, {
      translated_text: draftText.value,
      start,
      end,
      speaker: draftSpeaker.value || null,
      tags: draftTags.value.split(',').map((tag) => tag.trim()).filter(Boolean),
    })
    patchLine(updated)
    notice.value = `第 ${updated.ordinal} 条已保存`
  } catch (cause) {
    actionError.value = cause instanceof ApiError ? cause.message : String(cause)
  } finally {
    saving.value = false
  }
}

async function lineAction(action: 'translate' | 'split' | 'merge' | 'approve' | 'reject'): Promise<void> {
  const line = selected.value
  if (!line) return
  actionError.value = null
  try {
    if (action === 'translate') patchLine(await api.segments.translate(projectId.value, line.id))
    else if (action === 'split') {
      await api.segments.split(projectId.value, line.id, line.start + (line.end - line.start) / 2)
      await resetList()
    } else if (action === 'merge') {
      await api.segments.merge(projectId.value, line.id, true)
      await resetList()
    } else {
      patchLine(await api.segments.review(
        projectId.value,
        line.id,
        action === 'approve' ? 'approved' : 'rejected',
      ))
    }
  } catch (cause) {
    actionError.value = cause instanceof ApiError ? cause.message : String(cause)
  }
}

const checked = ref<Set<string>>(new Set())
function toggleChecked(id: string, value: boolean): void {
  const next = new Set(checked.value)
  if (value) next.add(id)
  else next.delete(id)
  checked.value = next
}

async function bulk(action: BulkSegmentAction): Promise<void> {
  const ids = [...checked.value]
  if (ids.length === 0) return
  actionError.value = null
  try {
    const result = await api.segments.bulk(projectId.value, ids, action)
    checked.value = new Set()
    await resetList()
    notice.value = `已处理 ${result.updated} 条${result.failed.length ? `，${result.failed.length} 条失败` : ''}`
    if (result.failed.length) actionError.value = result.failed.map((item) => item.message ?? item.code).join('\n')
  } catch (cause) {
    actionError.value = cause instanceof ApiError ? cause.message : String(cause)
  }
}

interface MediaControl {
  currentTime: number
  paused: boolean
  play(): Promise<void>
  pause(): void
}

const mediaElement = ref<MediaControl | null>(null)
const currentTime = ref(0)
const followPlayback = ref(true)
const preparingPreview = ref(false)

const mediaSource = computed(() => {
  const value = media.data.value
  if (!value) return ''
  if (value.status === 'ready' && value.preview_url) return value.preview_url
  return value.direct_playback ? value.source_url : ''
})

async function preparePreview(): Promise<void> {
  if (preparingPreview.value || media.data.value?.status === 'running') return
  preparingPreview.value = true
  actionError.value = null
  try {
    media.data.value = await api.projects.prepareEditorMedia(projectId.value)
  } catch (cause) {
    actionError.value = cause instanceof ApiError ? cause.message : String(cause)
  } finally {
    preparingPreview.value = false
  }
}

function onMediaError(): void {
  if (media.data.value?.status !== 'ready') void preparePreview()
}

function seekTo(seconds: number): void {
  const player = mediaElement.value
  if (!player) return
  player.currentTime = Math.max(0, seconds)
  currentTime.value = player.currentTime
}

function loadedLines(): Segment[] {
  return Object.values(rows.value).sort((a, b) => a.start - b.start)
}

const timelineLines = computed(() => loadedLines().slice(0, 500))
const timelineDuration = computed(() => {
  const projectDuration = project.data.value?.duration ?? 0
  const loadedEnd = timelineLines.value.at(-1)?.end ?? 0
  return Math.max(projectDuration, loadedEnd, 1)
})

function timelineStyle(line: Segment): { left: string; width: string } {
  const duration = timelineDuration.value
  return {
    left: `${(line.start / duration) * 100}%`,
    width: `${Math.max(0.18, ((line.end - line.start) / duration) * 100)}%`,
  }
}

function onTimeUpdate(): void {
  const player = mediaElement.value
  if (!player) return
  currentTime.value = player.currentTime
  const lines = loadedLines()
  const index = findActiveSegmentIndex(lines, player.currentTime)
  const active = index >= 0 ? lines[index] : undefined
  if (!active || !followPlayback.value || active.id === selected.value?.id || dirty.value) return
  seedDraft(active)
  const absoluteIndex = Object.entries(rows.value).find(([, line]) => line.id === active.id)?.[0]
  if (absoluteIndex !== undefined && scroller.value) {
    scroller.value.scrollTo({ top: Math.max(0, Number(absoluteIndex) * ROW_HEIGHT - ROW_HEIGHT * 2) })
  }
}

function togglePlayback(): void {
  const player = mediaElement.value
  if (!player) return
  if (player.paused) void player.play()
  else player.pause()
}

function onKeyboard(event: KeyboardEvent): void {
  const target = event.target as HTMLElement | null
  const typing = target?.matches('input, textarea, [contenteditable="true"]') ?? false
  if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 's') {
    event.preventDefault()
    void save()
    return
  }
  if (typing) return
  if (event.key === ' ') {
    event.preventDefault()
    togglePlayback()
  } else if (event.key === 'ArrowLeft') {
    event.preventDefault()
    seekTo(currentTime.value - 2)
  } else if (event.key === 'ArrowRight') {
    event.preventDefault()
    seekTo(currentTime.value + 2)
  }
}

function onSegmentUpdated(event: ServerEvent): void {
  const updated = (event.data as { segment?: Segment }).segment
  if (!updated || event.project_id !== projectId.value) return
  if (dirty.value && selected.value?.id === updated.id) return
  patchLine(updated)
}

async function refreshMedia(): Promise<void> {
  await media.run()
  if (media.data.value && !media.data.value.direct_playback && media.data.value.status === 'missing') {
    await preparePreview()
  }
}

const unsubscribers: (() => void)[] = []
let mediaPoll: number | undefined

onMounted(async () => {
  await Promise.all([project.run(), resetList(), refreshMedia()])
  const requested = typeof route.query['segment'] === 'string' ? route.query['segment'] : ''
  if (requested) {
    try {
      seedDraft(await api.segments.get(projectId.value, requested))
    } catch {
      // A stale deep link falls back to the first available line.
    }
  }
  if (!selected.value) {
    const first = rows.value[0]
    if (first) seedDraft(first)
  }

  unsubscribers.push(
    events.on('segment.updated', onSegmentUpdated),
    events.on('segments.replaced', () => void resetList()),
    events.on('media.preview', () => void refreshMedia()),
    events.on('resync.required', () => {
      void resetList()
      void refreshMedia()
    }),
  )
  window.addEventListener('keydown', onKeyboard)
  mediaPoll = window.setInterval(() => {
    if (media.data.value?.status === 'running') void refreshMedia()
  }, 1500)
  await nextTick()
  onScroll()
})

onBeforeUnmount(() => {
  for (const unsubscribe of unsubscribers) unsubscribe()
  window.removeEventListener('keydown', onKeyboard)
  window.clearTimeout(filterTimer)
  window.clearInterval(mediaPoll)
})

const FILTER_OPTIONS = [
  { value: '', label: '全部质量级别' },
  { value: 'error', label: '只看错误' },
  { value: 'warning', label: '只看警告' },
  { value: 'info', label: '只看提示' },
]

function reviewLabel(line: Segment): string {
  if (line.review_state === 'approved') return '已通过'
  if (line.review_state === 'rejected') return '已打回'
  if (line.is_edited) return '已修改'
  if (line.needs_review) return '待审校'
  return '未处理'
}

function cpsTone(line: Segment): string {
  if (line.cps === null) return 'text-ink-faint'
  if (line.cps > 18) return 'text-status-failed'
  if (line.cps > 12) return 'text-status-running'
  return 'text-ink-faint'
}

function previewLabel(value: EditorMedia | null): string {
  if (!value) return '正在读取媒体信息…'
  if (value.status === 'running') return `正在生成网页预览${value.progress ? ` ${Math.round(value.progress * 100)}%` : '…'}`
  if (value.status === 'failed') return value.error_message ?? '网页预览生成失败'
  return '正在准备播放器…'
}
</script>

<template>
  <div class="-mx-4 -my-3 flex h-[calc(100vh-5.5rem)] min-h-[680px] flex-col overflow-hidden">
    <header class="flex shrink-0 items-center gap-3 border-b border-line px-4 py-2">
      <RouterLink
        :to="`/projects/${projectId}`"
        class="rounded px-2 py-1 text-sm text-ink-muted outline-none hover:text-accent focus-visible:ring-2 focus-visible:ring-accent"
      >
        ← 返回项目
      </RouterLink>
      <div class="min-w-0">
        <h2 class="truncate text-sm font-medium">{{ project.data.value?.name ?? '审校工作台' }}</h2>
        <p class="text-xs text-ink-faint">{{ total }} 条字幕 · 空格播放/暂停 · Ctrl/⌘ + S 保存</p>
      </div>
      <div class="ml-auto flex items-center gap-2">
        <span v-if="dirty" class="text-xs text-status-running">有未保存修改</span>
        <span v-else-if="notice" class="text-xs text-status-done">{{ notice }}</span>
        <AppButton variant="primary" size="sm" :disabled="!dirty || saving" @click="save">
          {{ saving ? '保存中…' : '保存字幕' }}
        </AppButton>
        <a
          :href="`/api/v1/projects/${projectId}/subtitles.srt`"
          class="rounded border border-line px-3 py-1.5 text-xs text-ink-muted hover:border-accent hover:text-ink"
        >导出 SRT</a>
      </div>
    </header>

    <p v-if="actionError" class="m-3 whitespace-pre-line rounded border border-status-failed/40 bg-surface-raised p-3 text-sm text-status-failed">
      {{ actionError }}
    </p>

    <div class="grid min-h-0 flex-1 grid-cols-[minmax(520px,1fr)_340px] gap-3 p-3">
      <div class="flex min-h-0 flex-col gap-3">
        <section class="relative h-[38%] min-h-52 overflow-hidden rounded border border-line bg-black">
          <component
            :is="media.data.value?.media_kind === 'audio' ? 'audio' : 'video'"
            v-if="mediaSource"
            ref="mediaElement"
            :src="mediaSource"
            controls
            preload="metadata"
            class="size-full"
            @timeupdate="onTimeUpdate"
            @error="onMediaError"
          />
          <div v-else class="flex size-full flex-col items-center justify-center gap-3 px-8 text-center">
            <p class="text-sm text-ink-muted">{{ previewLabel(media.data.value) }}</p>
            <div v-if="media.data.value?.status === 'running'" class="h-1.5 w-64 overflow-hidden rounded bg-surface-raised">
              <div
                class="h-full bg-status-running transition-[width]"
                :class="media.data.value.progress ? '' : 'animate-pulse w-1/2'"
                :style="media.data.value.progress ? { width: `${media.data.value.progress * 100}%` } : undefined"
              />
            </div>
            <AppButton
              v-if="media.data.value?.status === 'failed' || media.data.value?.status === 'missing'"
              variant="ghost"
              size="sm"
              :disabled="preparingPreview"
              @click="preparePreview"
            >
              重新生成预览
            </AppButton>
            <p class="text-xs text-ink-faint">预览准备期间仍可继续编辑字幕。</p>
          </div>
          <div class="pointer-events-none absolute left-3 top-3 rounded bg-black/70 px-2 py-1 text-xs tabular-nums text-white">
            {{ formatDuration(currentTime) }}
          </div>
          <div
            v-if="mediaSource && timelineLines.length"
            class="absolute inset-x-3 bottom-12 h-6 rounded border border-white/20 bg-black/70"
            aria-label="字幕时间轴"
          >
            <button
              v-for="line in timelineLines"
              :key="line.id"
              type="button"
              class="absolute top-1 h-4 min-w-px rounded-sm bg-white/45 outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-accent"
              :class="selected?.id === line.id ? 'bg-accent' : ''"
              :style="timelineStyle(line)"
              :aria-label="`定位到第 ${line.ordinal} 条，${formatDuration(line.start)}`"
              @click="selectLine(line)"
            />
            <span
              class="pointer-events-none absolute inset-y-0 w-px bg-status-failed"
              :style="{ left: `${Math.min(100, (currentTime / timelineDuration) * 100)}%` }"
            />
          </div>
        </section>

        <section class="flex min-h-0 flex-1 flex-col rounded border border-line bg-surface-raised">
          <div class="flex shrink-0 items-center gap-2 border-b border-line p-2">
            <div class="min-w-44 flex-1"><AppInput v-model="search" type="search" placeholder="搜索原文或译文" /></div>
            <div class="w-40"><AppSelect id="qc-severity" v-model="severity" aria-label="质量问题级别" :options="FILTER_OPTIONS" /></div>
            <AppCheckbox v-model="onlyReview" label="只看待审校" />
            <AppCheckbox v-model="followPlayback" label="播放跟随" />
          </div>

          <div v-if="checked.size" class="flex shrink-0 items-center gap-2 border-b border-line bg-surface-sunken px-3 py-2">
            <span class="text-xs text-ink-muted">已选 {{ checked.size }} 条</span>
            <AppButton size="sm" variant="ghost" @click="bulk('approve')">批量通过</AppButton>
            <AppButton size="sm" variant="ghost" @click="bulk('pending')">标记待审</AppButton>
            <AppButton size="sm" variant="danger" @click="bulk('reject')">批量打回</AppButton>
            <AppButton size="sm" variant="ghost" @click="bulk('retranslate')">批量重译</AppButton>
          </div>

          <div v-if="listError" class="p-4 text-sm text-status-failed">
            {{ listError }} <AppButton size="sm" variant="ghost" @click="resetList">重试</AppButton>
          </div>
          <div v-else-if="total === 0 && !listLoading" class="flex flex-1 items-center justify-center p-8 text-sm text-ink-muted">
            {{ Object.keys(rows).length === 0 && !search && !onlyReview ? '还没有字幕，请先在项目页运行任务。' : '没有符合条件的字幕。' }}
          </div>
          <div v-else ref="scroller" class="min-h-0 flex-1 overflow-auto" @scroll="onScroll">
            <div :style="{ height: `${topSpace}px` }" />
            <div
              v-for="entry in visibleRows"
              :key="entry.line?.id ?? `placeholder-${entry.index}`"
              class="border-b border-line/60"
              :style="{ height: `${ROW_HEIGHT}px` }"
            >
              <div
                v-if="entry.line"
                class="group grid size-full grid-cols-[32px_minmax(0,1fr)] items-start gap-3 px-3 py-2 transition hover:bg-surface"
                :class="[
                  selected?.id === entry.line.id ? 'bg-surface' : '',
                  currentTime >= entry.line.start && currentTime < entry.line.end ? 'border-l-2 border-l-accent' : '',
                ]"
              >
                <span class="pt-0.5">
                  <input
                    type="checkbox"
                    :checked="checked.has(entry.line.id)"
                    :aria-label="`选择第 ${entry.line.ordinal} 条字幕`"
                    class="size-4 accent-[var(--color-accent)]"
                    @change="toggleChecked(entry.line.id, ($event.target as HTMLInputElement).checked)"
                  />
                </span>
                <button
                  type="button"
                  class="grid size-full grid-cols-[76px_minmax(140px,0.8fr)_minmax(200px,1fr)_72px] items-start gap-3 text-left outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-accent"
                  :aria-label="`审校第 ${entry.line.ordinal} 条字幕`"
                  @click="selectLine(entry.line)"
                >
                  <span class="text-xs tabular-nums text-ink-faint">
                    <span class="block">#{{ entry.line.ordinal }}</span>
                    <span class="block">{{ formatDuration(entry.line.start) }}</span>
                    <span :class="cpsTone(entry.line)">{{ entry.line.cps?.toFixed(1) ?? '—' }} CPS</span>
                  </span>
                  <span class="line-clamp-3 text-xs leading-5 text-ink-muted">{{ entry.line.source_text }}</span>
                  <span class="line-clamp-3 text-sm leading-5" :class="entry.line.translated_text ? '' : 'italic text-ink-faint'">
                    {{ entry.line.translated_text || '（未翻译）' }}
                  </span>
                  <span class="space-y-1 text-right text-xs text-ink-faint">
                    <span class="block">{{ reviewLabel(entry.line) }}</span>
                    <span v-if="entry.line.qc?.length" class="block text-status-running">{{ entry.line.qc.length }} 个问题</span>
                  </span>
                </button>
              </div>
              <div v-else class="flex size-full items-center px-4 text-xs text-ink-faint">正在载入…</div>
            </div>
            <div :style="{ height: `${bottomSpace}px` }" />
          </div>
        </section>
      </div>

      <aside class="flex min-h-0 flex-col overflow-hidden rounded border border-line bg-surface-raised">
        <template v-if="selected">
          <header class="shrink-0 border-b border-line px-4 py-3">
            <div class="flex items-center justify-between gap-2">
              <h3 class="text-sm font-medium">第 {{ selected.ordinal }} 条</h3>
              <AppBadge :tone="selected.needs_review ? 'warn' : 'neutral'">{{ reviewLabel(selected) }}</AppBadge>
            </div>
            <p class="mt-2 text-sm leading-6 text-ink-muted">{{ selected.source_text }}</p>
          </header>

          <div class="min-h-0 flex-1 space-y-4 overflow-auto p-4">
            <label class="block text-xs text-ink-muted">
              <span class="mb-1 block">译文</span>
              <AppTextarea v-model="draftText" :rows="5" placeholder="输入中文字幕" />
            </label>

            <div class="grid grid-cols-2 gap-3">
              <label class="text-xs text-ink-muted">
                <span class="mb-1 block">开始（秒）</span>
                <AppInput v-model="draftStart" type="number" :step="0.01" :min="0" />
              </label>
              <label class="text-xs text-ink-muted">
                <span class="mb-1 block">结束（秒）</span>
                <AppInput v-model="draftEnd" type="number" :step="0.01" :min="0" />
              </label>
            </div>

            <label class="block text-xs text-ink-muted">
              <span class="mb-1 block">说话人</span>
              <AppInput v-model="draftSpeaker" placeholder="可选" />
            </label>
            <label class="block text-xs text-ink-muted">
              <span class="mb-1 block">标签（逗号分隔）</span>
              <AppInput v-model="draftTags" placeholder="例如：人名, 术语" />
            </label>

            <section v-if="selected.qc?.length" class="rounded border border-status-running/40 bg-surface p-3">
              <h4 class="text-xs font-medium text-status-running">质量检查</h4>
              <ul class="mt-2 space-y-2">
                <li v-for="finding in selected.qc" :key="finding.id" class="text-xs leading-5">
                  <p>{{ finding.message }}</p>
                  <p v-if="finding.suggestion" class="text-ink-faint">建议：{{ finding.suggestion }}</p>
                </li>
              </ul>
            </section>

            <div class="grid grid-cols-2 gap-2">
              <AppButton variant="ghost" size="sm" @click="lineAction('translate')">重新翻译</AppButton>
              <AppButton variant="ghost" size="sm" @click="lineAction('split')">从中间拆分</AppButton>
              <AppButton variant="ghost" size="sm" @click="lineAction('merge')">合并下一条</AppButton>
              <AppButton variant="ghost" size="sm" @click="seekTo(selected.start)">定位播放</AppButton>
              <AppButton variant="ghost" size="sm" @click="lineAction('approve')">通过</AppButton>
              <AppButton variant="danger" size="sm" @click="lineAction('reject')">打回</AppButton>
            </div>
          </div>

          <footer class="flex shrink-0 items-center gap-2 border-t border-line p-3">
            <AppButton variant="primary" class="flex-1" :disabled="!dirty || saving" @click="save">
              {{ saving ? '保存中…' : '保存修改' }}
            </AppButton>
            <AppButton variant="ghost" :disabled="!dirty" @click="resetDraft">放弃</AppButton>
          </footer>
        </template>
        <div v-else class="flex flex-1 items-center justify-center p-8 text-center text-sm text-ink-muted">
          从左侧选择一条字幕开始审校。
        </div>
      </aside>
    </div>
  </div>
</template>
