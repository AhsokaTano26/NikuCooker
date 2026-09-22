<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'

import { ApiError, api } from '@/api/client'
import { formatBytes, useAsync } from '@/composables/useAsync'
import { useEventStore, type ServerEvent } from '@/stores/events'
import type { ModelRecord, ModelStatus } from '@/api/client'

const events = useEventStore()
const models = useAsync(() => api.models.list())

/**
 * Live download progress, keyed by model name.
 *
 * The list comes from the server and is refetched when a download finishes; the
 * percentage comes from the stream, because polling a multi-gigabyte download
 * once a second is a lot of requests to learn one number.
 */
const progress = ref<Record<string, number>>({})
const actionError = ref<string | null>(null)
const busy = ref<string | null>(null)

function onModelProgress(event: ServerEvent): void {
  const data = event.data as { model?: string; status?: string; progress?: number; error_message?: string }
  if (!data.model) return

  if (data.status === 'failed') {
    actionError.value = `${data.model}: ${data.error_message ?? 'download failed'}`
    delete progress.value[data.model]
    void models.run()
    return
  }

  if (data.status === 'ready') {
    delete progress.value[data.model]
    void models.run()
    return
  }

  progress.value = { ...progress.value, [data.model]: data.progress ?? 0 }
}

const unsubscribers: (() => void)[] = []

onMounted(async () => {
  await models.run()
  unsubscribers.push(
    events.on('model.progress', onModelProgress),
    events.on('resync.required', () => void models.run()),
  )
})

onBeforeUnmount(() => {
  for (const unsubscribe of unsubscribers) unsubscribe()
})

async function download(model: ModelRecord): Promise<void> {
  busy.value = model.id
  actionError.value = null
  try {
    await api.models.download(model.id)
    progress.value = { ...progress.value, [model.name]: 0 }
  } catch (cause) {
    actionError.value = cause instanceof ApiError ? cause.message : String(cause)
  } finally {
    busy.value = null
  }
}

async function remove(model: ModelRecord): Promise<void> {
  if (!confirm(`删除模型「${model.name}」？下次使用时需要重新下载。`)) return

  busy.value = model.id
  actionError.value = null
  try {
    await api.models.remove(model.id)
    await models.run()
  } catch (cause) {
    actionError.value = cause instanceof ApiError ? cause.message : String(cause)
  } finally {
    busy.value = null
  }
}

const items = computed(() => models.data.value?.items ?? [])

/** Disk used by everything that is installed. */
const usedBytes = computed(() =>
  items.value.reduce((total, model) => total + (model.status === 'ready' ? model.size_bytes : 0), 0),
)

const STATUS_LABEL: Record<ModelStatus, string> = {
  missing: '未下载',
  downloading: '下载中',
  ready: '已安装',
  failed: '失败',
}

const STATUS_TONE: Record<ModelStatus, string> = {
  missing: 'text-ink-faint',
  downloading: 'text-status-running',
  ready: 'text-status-done',
  failed: 'text-status-failed',
}
</script>

<template>
  <div class="space-y-4">
    <div class="flex flex-wrap items-center justify-between gap-3">
      <p class="text-sm text-ink-muted">
        已安装模型占用 <span class="tabular-nums text-ink">{{ formatBytes(usedBytes) }}</span>
      </p>
      <button class="text-sm text-ink-faint transition hover:text-accent" @click="models.run">
        刷新
      </button>
    </div>

    <p v-if="actionError" class="rounded border border-status-failed/40 bg-surface-raised p-3 text-sm text-status-failed">
      {{ actionError }}
    </p>

    <p v-if="models.error.value" class="rounded border border-status-failed/40 bg-surface-raised p-3 text-sm text-status-failed">
      {{ models.error.value }}
      <button class="ml-2 text-accent hover:underline" @click="models.run">重试</button>
    </p>

    <div v-else-if="models.loading.value && !models.data.value" class="p-6 text-center text-sm text-ink-muted">
      加载中…
    </div>

    <div v-else-if="items.length === 0" class="rounded border border-dashed border-line p-10 text-center text-sm text-ink-muted">
      模型清单为空。
    </div>

    <ul v-else class="divide-y divide-line/60 rounded border border-line">
      <li v-for="model in items" :key="model.id" class="px-4 py-3">
        <div class="flex items-start gap-4">
          <div class="min-w-0 flex-1">
            <div class="flex flex-wrap items-baseline gap-2">
              <span class="font-mono text-sm">{{ model.name }}</span>
              <span class="text-xs" :class="STATUS_TONE[model.status]">
                {{ STATUS_LABEL[model.status] }}
              </span>
              <span class="text-xs tabular-nums text-ink-faint">
                {{ model.status === 'ready'
                  ? formatBytes(model.size_bytes)
                  : (model.estimated_size_bytes ? '~' + formatBytes(model.estimated_size_bytes) : '') }}
              </span>
            </div>

            <p v-if="model.note" class="mt-1 text-xs text-ink-faint">{{ model.note }}</p>
            <p v-if="model.error_message" class="mt-1 text-xs text-status-failed">
              {{ model.error_message }}
            </p>

            <!-- A download in flight. The percentage is only rendered when the
                 server has reported one; an invented denominator moves
                 backwards. -->
            <div v-if="progress[model.name] !== undefined" class="mt-2 flex items-center gap-3">
              <div class="h-1 flex-1 overflow-hidden rounded bg-surface-sunken">
                <div
                  class="h-full bg-status-running transition-[width] duration-300"
                  :style="{ width: `${Math.round((progress[model.name] ?? 0) * 100)}%` }"
                />
              </div>
              <span class="w-10 text-right text-xs tabular-nums text-ink-faint">
                {{ Math.round((progress[model.name] ?? 0) * 100) }}%
              </span>
            </div>
          </div>

          <div class="flex shrink-0 gap-3 text-xs">
            <button
              v-if="model.status !== 'ready' && progress[model.name] === undefined"
              :disabled="busy === model.id"
              class="text-accent transition hover:underline disabled:opacity-40"
              @click="download(model)"
            >
              下载
            </button>
            <button
              v-else-if="model.status === 'ready'"
              :disabled="busy === model.id"
              class="text-ink-faint transition hover:text-status-failed disabled:opacity-40"
              @click="remove(model)"
            >
              删除
            </button>
          </div>
        </div>
      </li>
    </ul>

    <p class="text-xs text-ink-faint">
      模型按需下载。NikuCooker 不内置权重：最小的 75 MB，最大的超过 1 GB，
      只做日语的项目不该因此下载九十种语言的模型。
    </p>
  </div>
</template>
