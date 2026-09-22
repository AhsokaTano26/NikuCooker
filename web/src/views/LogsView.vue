<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'

import { ApiError, api, type LogRecord } from '@/api/client'

const records = ref<LogRecord[]>([])
const level = ref<'all' | 'warn' | 'error'>('all')
const following = ref(true)
const error = ref<string | null>(null)

/** The highest sequence seen, which is what the next poll asks for. */
let cursor = 0
let timer: ReturnType<typeof setInterval> | null = null

/**
 * Polled rather than streamed.
 *
 * A busy run logs dozens of lines a second, and pushing each one onto the event
 * stream every tab is already reading would flood it with data the interface
 * shows nothing of. A request a second that returns only what is new is the
 * cheaper and simpler shape, and `after` makes it incremental.
 */
async function poll(): Promise<void> {
  if (!following.value) return

  try {
    const page = await api.logs.list(cursor, 500)
    error.value = null

    if (page.items.length > 0) {
      cursor = page.seq
      records.value = [...records.value, ...page.items].slice(-2000)
    }
  } catch (cause) {
    error.value = cause instanceof ApiError ? cause.message : String(cause)
    // Stopped on failure: a server that is down will not start answering
    // because we kept asking, and one error message is enough.
    following.value = false
  }
}

onMounted(async () => {
  await poll()
  timer = setInterval(poll, 1000)
})

onBeforeUnmount(() => {
  if (timer) clearInterval(timer)
})

const visible = computed(() => {
  if (level.value === 'all') return records.value
  if (level.value === 'warn') {
    return records.value.filter((r) => r.level === 'warn' || r.level === 'error')
  }
  return records.value.filter((r) => r.level === 'error')
})

function clear(): void {
  records.value = []
}

const LEVEL_TONE: Record<LogRecord['level'], string> = {
  debug: 'text-ink-faint',
  info: 'text-ink-muted',
  warn: 'text-status-running',
  error: 'text-status-failed',
}

function timeOf(record: LogRecord): string {
  const date = new Date(record.time)
  return Number.isNaN(date.getTime()) ? '—' : date.toLocaleTimeString()
}

/** Renders a record's attributes as one line of key=value. */
function attrsOf(record: LogRecord): string {
  if (!record.attrs) return ''
  return Object.entries(record.attrs)
    // The component and the timestamp are already on screen; repeating them in
    // every line is noise that pushes the useful fields off the edge.
    .filter(([key]) => key !== 'component' && key !== 'time')
    .map(([key, value]) => `${key}=${String(value)}`)
    .join(' ')
}
</script>

<template>
  <div class="space-y-4">
    <div class="flex flex-wrap items-center gap-3">
      <div class="flex rounded border border-line bg-surface-raised">
        <button
          v-for="option in (['all', 'warn', 'error'] as const)"
          :key="option"
          class="px-3 py-1.5 text-sm transition"
          :class="level === option ? 'bg-accent text-accent-ink' : 'text-ink-muted hover:text-ink'"
          @click="level = option"
        >
          {{ { all: '全部', warn: '警告以上', error: '仅错误' }[option] }}
        </button>
      </div>

      <label class="flex items-center gap-2 text-sm text-ink-muted">
        <input v-model="following" type="checkbox" @change="poll" />
        跟随
      </label>

      <span class="text-xs text-ink-faint">{{ visible.length }} / {{ records.length }} 行</span>

      <button class="ml-auto text-sm text-ink-faint transition hover:text-accent" @click="clear">
        清空显示
      </button>
    </div>

    <p v-if="error" class="rounded border border-status-failed/40 bg-surface-raised p-3 text-sm text-status-failed">
      {{ error }}
    </p>

    <div class="max-h-[70vh] overflow-y-auto rounded border border-line bg-surface-sunken">
      <p v-if="visible.length === 0" class="p-6 text-center text-sm text-ink-muted">
        还没有日志。运行一次 Pipeline 就会有内容。
      </p>

      <ul v-else class="divide-y divide-line/40 font-mono text-xs">
        <li v-for="record in visible" :key="record.seq" class="flex gap-3 px-3 py-1">
          <span class="shrink-0 text-ink-faint">{{ timeOf(record) }}</span>
          <span class="w-10 shrink-0" :class="LEVEL_TONE[record.level]">{{ record.level }}</span>
          <span class="min-w-0 flex-1">
            <span class="text-ink">{{ record.msg }}</span>
            <span v-if="attrsOf(record)" class="ml-2 text-ink-faint">{{ attrsOf(record) }}</span>
          </span>
        </li>
      </ul>
    </div>

    <p class="text-xs text-ink-faint">
      这里显示服务进程最近 {{ 2000 }} 条日志，与终端上的输出是同一份。
    </p>
  </div>
</template>
