<script setup lang="ts">
import { computed, onMounted, watch } from 'vue'

import { api } from '@/api/client'
import AppButton from '@/components/AppButton.vue'
import { formatBytes, useAsync } from '@/composables/useAsync'
import { useEventStore } from '@/stores/events'

const events = useEventStore()
const overview = useAsync(() => api.system.overview())

onMounted(overview.run)
watch(() => events.resyncCount, overview.run)

const worker = computed(() => overview.data.value?.worker)
const stats = computed(() => overview.data.value?.stats)

function percent(value: number | null | undefined): string {
  return value === null || value === undefined ? '—' : `${value.toFixed(0)}%`
}
</script>

<template>
  <div class="space-y-6">
    <p v-if="overview.error.value" class="rounded border border-status-failed/40 bg-surface-raised p-4 text-sm text-status-failed">
      {{ overview.error.value }}
      <AppButton variant="ghost" size="sm" class="ml-2" @click="overview.run">重试</AppButton>
    </p>

    <template v-else>
      <section class="rounded border border-line bg-surface-raised p-4">
        <h2 class="text-sm font-medium text-ink-muted">服务</h2>
        <dl class="mt-3 grid gap-2 text-sm sm:grid-cols-2">
          <div class="flex justify-between gap-4">
            <dt class="text-ink-muted">版本</dt>
            <dd class="font-mono text-xs">{{ overview.data.value?.version ?? '—' }}</dd>
          </div>
          <div class="flex justify-between gap-4">
            <dt class="text-ink-muted">提交</dt>
            <dd class="font-mono text-xs">{{ overview.data.value?.commit ?? '—' }}</dd>
          </div>
          <div class="flex justify-between gap-4">
            <dt class="text-ink-muted">平台</dt>
            <dd class="font-mono text-xs">{{ overview.data.value?.platform ?? '—' }}</dd>
          </div>
          <div class="flex justify-between gap-4">
            <dt class="text-ink-muted">已运行</dt>
            <dd class="tabular-nums">{{ Math.floor((overview.data.value?.uptime_s ?? 0) / 60) }} 分钟</dd>
          </div>
        </dl>
      </section>

      <section class="rounded border border-line bg-surface-raised p-4">
        <h2 class="text-sm font-medium text-ink-muted">AI Worker</h2>
        <dl class="mt-3 space-y-2 text-sm">
          <div class="flex justify-between gap-4">
            <dt class="shrink-0 text-ink-muted">状态</dt>
            <dd>{{ worker?.status ?? '—' }} · {{ worker?.workers ?? 0 }} 个进程</dd>
          </div>
          <div class="flex justify-between gap-4">
            <dt class="shrink-0 text-ink-muted">解释器</dt>
            <dd class="truncate font-mono text-xs" :title="worker?.python">{{ worker?.python || '—' }}</dd>
          </div>
          <div class="flex justify-between gap-4">
            <dt class="shrink-0 text-ink-muted">协议摘要</dt>
            <dd class="truncate font-mono text-xs" :title="worker?.schema_digest">
              {{ worker?.schema_digest || '—' }}
            </dd>
          </div>
        </dl>
        <p class="mt-3 border-t border-line pt-3 text-xs text-ink-faint">
          协议摘要由 Go 与 Python 各自从同一组 fixture 计算得出，握手时逐字比较。
          两边不一致时服务拒绝启动，而不是在中途把消息解释错。
        </p>
      </section>

      <section class="rounded border border-line bg-surface-raised p-4">
        <h2 class="text-sm font-medium text-ink-muted">主机</h2>
        <dl class="mt-3 space-y-2 text-sm">
          <div class="flex justify-between">
            <dt class="text-ink-muted">CPU</dt>
            <dd class="tabular-nums">{{ stats?.cpu.cores ?? '—' }} 核 · {{ percent(stats?.cpu.usage_percent) }}</dd>
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

        <div v-if="stats?.gpu.length" class="mt-3 border-t border-line pt-3">
          <p class="text-xs text-ink-faint">GPU</p>
          <div v-for="gpu in stats.gpu" :key="gpu.index" class="mt-1 flex justify-between text-sm">
            <span>{{ gpu.name }}</span>
            <span class="tabular-nums">
              {{ formatBytes(gpu.memory_used_bytes) }} / {{ formatBytes(gpu.memory_total_bytes) }}
              <span v-if="gpu.driver" class="text-ink-faint"> · {{ gpu.driver }}</span>
            </span>
          </div>
        </div>
      </section>

      <section class="rounded border border-line bg-surface-raised p-4">
        <h2 class="text-sm font-medium text-ink-muted">统计</h2>
        <dl class="mt-3 grid gap-2 text-sm sm:grid-cols-2">
          <div class="flex justify-between gap-4">
            <dt class="text-ink-muted">项目</dt>
            <dd class="tabular-nums">{{ overview.data.value?.counts.projects ?? 0 }}</dd>
          </div>
          <div class="flex justify-between gap-4">
            <dt class="text-ink-muted">字幕行</dt>
            <dd class="tabular-nums">{{ overview.data.value?.counts.segments ?? 0 }}</dd>
          </div>
          <div class="flex justify-between gap-4">
            <dt class="text-ink-muted">运行中</dt>
            <dd class="tabular-nums">{{ overview.data.value?.counts.jobs_running ?? 0 }}</dd>
          </div>
          <div class="flex justify-between gap-4">
            <dt class="text-ink-muted">待审校</dt>
            <dd class="tabular-nums">{{ overview.data.value?.counts.needs_review ?? 0 }}</dd>
          </div>
        </dl>
      </section>
    </template>
  </div>
</template>
