<script setup lang="ts">
import { computed, onMounted, watch } from 'vue'
import { RouterLink } from 'vue-router'

import { api } from '@/api/client'
import AppButton from '@/components/AppButton.vue'
import { formatBytes, useAsync } from '@/composables/useAsync'
import { WORKER_LABEL, needsAttention } from '@/composables/workerStatus'
import { useEventStore } from '@/stores/events'

const events = useEventStore()
const overview = useAsync(() => api.system.overview())

onMounted(overview.run)

/**
 * Host statistics are pushed, not polled, so the figures move while a job runs
 * without the page asking for them.
 */
watch(() => events.resyncCount, overview.run)

const counts = computed(() => overview.data.value?.counts)
const stats = computed(() => overview.data.value?.stats)
const worker = computed(() => overview.data.value?.worker)

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
        <h2 class="text-sm font-medium text-ink-muted">AI Worker</h2>
        <dl class="mt-3 space-y-2 text-sm">
          <div class="flex justify-between gap-4">
            <dt class="shrink-0 text-ink-muted">状态</dt>
            <!--
              Reported by the server, never inferred. A UI that decided "the
              worker is idle" from the absence of an event would be wrong every
              time the stream dropped.
            -->
            <dd
              :class="needsAttention(worker?.status) ? 'text-status-failed' : ''"
            >
              {{ worker ? WORKER_LABEL[worker.status] : '—' }}
            </dd>
          </div>
          <div class="flex justify-between gap-4">
            <dt class="shrink-0 text-ink-muted">解释器</dt>
            <dd class="truncate font-mono text-xs" :title="worker?.python">{{ worker?.python || '—' }}</dd>
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
