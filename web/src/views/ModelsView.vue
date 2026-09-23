<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'

import { ApiError, api } from '@/api/client'
import AppButton from '@/components/AppButton.vue'
import AppBadge from '@/components/AppBadge.vue'
import AppInput from '@/components/AppInput.vue'
import { formatBytes, useAsync } from '@/composables/useAsync'
import { useEventStore, type ServerEvent } from '@/stores/events'
import type { ModelRecord, ModelStatus } from '@/api/client'

const events = useEventStore()
const models = useAsync(() => api.models.list())
const source = useAsync(() => api.settings.get())

const endpointDraft = ref('')
const sourceSaving = ref(false)
const sourceNotice = ref<string | null>(null)
const sourceError = ref<string | null>(null)

const endpointSetting = computed(() =>
  source.data.value?.catalog.find((setting) => setting.key === 'models.endpoint'),
)
const effectiveEndpoint = computed(() => String(endpointSetting.value?.value ?? ''))
const officialEndpoint = computed(() =>
  String(endpointSetting.value?.default_value ?? 'https://huggingface.co'),
)

const SOURCE_LABEL: Record<string, string> = {
  default: '默认',
  config_file: '配置文件',
  environment: '环境变量',
  database: '网页',
  project: '项目覆盖',
  cli: '命令行',
}

function reseedEndpoint(): void {
  endpointDraft.value = effectiveEndpoint.value
}

async function refreshSource(): Promise<void> {
  await source.run()
  reseedEndpoint()
}

async function saveEndpoint(endpoint = endpointDraft.value): Promise<void> {
  sourceSaving.value = true
  sourceNotice.value = null
  sourceError.value = null

  try {
    const updated = await api.settings.update({ 'models.endpoint': endpoint.trim() })
    source.data.value = updated
    reseedEndpoint()
    sourceNotice.value = '模型下载源已更新，下一次下载立即使用新地址。'
  } catch (cause) {
    sourceError.value = cause instanceof ApiError ? cause.message : String(cause)
  } finally {
    sourceSaving.value = false
  }
}

async function useOfficialEndpoint(): Promise<void> {
  endpointDraft.value = officialEndpoint.value
  await saveEndpoint(officialEndpoint.value)
}

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
  await Promise.all([models.run(), refreshSource()])
  unsubscribers.push(
    events.on('model.progress', onModelProgress),
    events.on('resync.required', () => void models.run()),
    events.on('settings.changed', () => void refreshSource()),
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
  <div class="mx-auto max-w-5xl space-y-4">
    <section class="rounded border border-accent/40 bg-surface-raised p-4">
      <h2 class="text-sm font-medium">不知道选哪个？先用 medium</h2>
      <p class="mt-1 text-sm text-ink-muted">
        medium 是日语字幕的日常推荐，准确度和速度比较均衡。tiny 只用来快速试通流程；
        large-v3 适合追求最终精度且愿意等待的任务；distil-large-v3 偏英语，不建议日语项目使用。
      </p>
    </section>

    <section class="rounded border border-line bg-surface-raised p-4">
      <div class="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 class="text-sm font-medium">模型下载源</h2>
          <p class="mt-1 text-xs leading-5 text-ink-muted">
            填写 Hugging Face 兼容服务的基础地址。镜像需要支持
            <code class="font-mono">/api/models/…</code> 和
            <code class="font-mono">/resolve/main/…</code> 接口。
          </p>
        </div>
        <span v-if="endpointSetting" class="text-xs text-ink-faint">
          来源：{{ SOURCE_LABEL[endpointSetting.source] ?? endpointSetting.source }}
        </span>
      </div>

      <div class="mt-3" data-test="model-endpoint">
        <label for="model-endpoint-input" class="text-xs font-medium text-ink-muted">基础地址</label>
        <div class="mt-1 grid items-stretch gap-2 lg:grid-cols-[minmax(0,1fr)_auto_auto]">
          <AppInput
            id="model-endpoint-input"
            v-model="endpointDraft"
            mono
            placeholder="https://huggingface.co"
            :disabled="source.loading.value || sourceSaving"
          />
          <AppButton
            data-test="save-model-endpoint"
            class="whitespace-nowrap"
            variant="primary"
            size="sm"
            :disabled="source.loading.value || sourceSaving || endpointDraft.trim() === effectiveEndpoint"
            @click="saveEndpoint()"
          >
            {{ sourceSaving ? '保存中…' : '保存模型源' }}
          </AppButton>
          <AppButton
            data-test="use-official-model-endpoint"
            class="whitespace-nowrap"
            variant="secondary"
            size="sm"
            :disabled="source.loading.value || sourceSaving || effectiveEndpoint === officialEndpoint"
            @click="useOfficialEndpoint"
          >
            使用官方源
          </AppButton>
        </div>
      </div>

      <div v-if="effectiveEndpoint" class="mt-3 flex flex-wrap items-baseline gap-x-2 gap-y-1 text-xs">
        <span class="text-ink-faint">当前生效：</span>
        <code class="break-all font-mono text-ink-muted">{{ effectiveEndpoint }}</code>
      </div>
      <p v-if="sourceNotice" role="status" class="mt-2 text-xs text-status-done">{{ sourceNotice }}</p>
      <p v-if="source.error.value || sourceError" role="alert" class="mt-2 text-xs text-status-failed">
        {{ sourceError ?? source.error.value }}
      </p>
    </section>

    <div class="flex flex-wrap items-center justify-between gap-3">
      <p class="text-sm text-ink-muted">
        已安装模型占用 <span class="tabular-nums text-ink">{{ formatBytes(usedBytes) }}</span>
      </p>
      <AppButton variant="ghost" size="sm" @click="models.run">
        刷新
      </AppButton>
    </div>

    <p v-if="actionError" class="rounded border border-status-failed/40 bg-surface-raised p-3 text-sm text-status-failed">
      {{ actionError }}
    </p>

    <p v-if="models.error.value" class="rounded border border-status-failed/40 bg-surface-raised p-3 text-sm text-status-failed">
      {{ models.error.value }}
      <AppButton variant="ghost" size="sm" class="ml-2" @click="models.run">重试</AppButton>
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
              <AppBadge v-for="tag in model.tags ?? []" :key="tag" :tone="tag.includes('推荐') || tag === '默认' ? 'accent' : 'neutral'">
                {{ tag }}
              </AppBadge>
              <span class="text-xs" :class="STATUS_TONE[model.status]">
                {{ STATUS_LABEL[model.status] }}
              </span>
              <span class="text-xs tabular-nums text-ink-faint">
                {{ model.status === 'ready'
                  ? formatBytes(model.size_bytes)
                  : (model.estimated_size_bytes ? '~' + formatBytes(model.estimated_size_bytes) : '') }}
              </span>
            </div>

            <p v-if="model.note" class="mt-2 text-sm leading-6 text-ink-muted">{{ model.note }}</p>
            <dl v-if="model.kind === 'asr'" class="mt-3 grid grid-cols-2 gap-x-6 gap-y-2 text-xs lg:grid-cols-4">
              <div>
                <dt class="text-ink-faint">适合</dt>
                <dd class="mt-0.5 text-ink-muted">{{ model.recommendation }}</dd>
              </div>
              <div>
                <dt class="text-ink-faint">精度 / 速度</dt>
                <dd class="mt-0.5 text-ink-muted">{{ model.accuracy }} / {{ model.speed }}</dd>
              </div>
              <div>
                <dt class="text-ink-faint">硬件建议</dt>
                <dd class="mt-0.5 text-ink-muted">{{ model.hardware }}</dd>
              </div>
              <div>
                <dt class="text-ink-faint">语言</dt>
                <dd class="mt-0.5 text-ink-muted">{{ model.language }}</dd>
              </div>
            </dl>
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
            <AppButton
              v-if="model.status !== 'ready' && progress[model.name] === undefined"
              variant="ghost"
              size="sm"
              :disabled="busy === model.id"
              @click="download(model)"
            >
              下载
            </AppButton>
            <AppButton
              v-else-if="model.status === 'ready'"
              variant="danger"
              size="sm"
              :disabled="busy === model.id"
              @click="remove(model)"
            >
              删除
            </AppButton>
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
