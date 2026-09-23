<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'

import { ApiError, api } from '@/api/client'
import AppButton from '@/components/AppButton.vue'
import AppRadio from '@/components/AppRadio.vue'
import { formatBytes, useAsync } from '@/composables/useAsync'
import { useEventStore } from '@/stores/events'
import {
  ACCELERATOR_LABEL,
  INSTALL_STEPS,
  WORKER_LABEL,
  cudaReason,
  needsAttention,
} from '@/composables/environment'
import type { RuntimePhase } from '@/types/api'

const events = useEventStore()
const overview = useAsync(() => api.system.overview())

const shutdownError = ref<string | null>(null)
const installError = ref<string | null>(null)

onMounted(overview.run)
watch(() => events.resyncCount, overview.run)

const worker = computed(() => overview.data.value?.worker)
const stats = computed(() => overview.data.value?.stats)
const runtime = computed(() => overview.data.value?.runtime)

/**
 * Whether the environment card has anything useful to say.
 *
 * Two cases, and nothing else. There is something to install, so the server
 * says it can be installed; or there is nothing to install and the worker is
 * not usable, in which case the reason is the useful part and hiding the card
 * would leave the problem unexplained.
 */
const showRuntime = computed(() => {
  const state = runtime.value
  if (state === undefined) return false
  if (state.available) return true

  const status = worker.value?.status
  return status === 'missing' || status === 'failed'
})

// A list and not a bar. The installer narrates through uv, which writes a
// terminal animation rather than a number this could render, and a progress bar
// over an invented denominator moves backwards — which reads as a bug in the
// thing that is working. The steps themselves come from the shared vocabulary.

type StepState = 'done' | 'current' | 'pending' | 'failed'

function stepState(phase: RuntimePhase): StepState {
  const state = runtime.value
  if (state === undefined) return 'pending'

  const current = INSTALL_STEPS.findIndex((step) => step.phase === state.phase)
  const index = INSTALL_STEPS.findIndex((step) => step.phase === phase)

  if (state.status === 'failed' && index === current) return 'failed'
  if (index < current) return 'done'
  if (index === current) return state.status === 'running' ? 'current' : 'done'
  return 'pending'
}

function stepMark(phase: RuntimePhase): string {
  switch (stepState(phase)) {
    case 'done':
      return '✓'
    case 'current':
      return '→'
    case 'failed':
      return '✗'
    default:
      return '·'
  }
}

/**
 * Which dependency set to install.
 *
 * 'cuda' is only ever selected where the server said it could be used, so the
 * request cannot be one the server refuses; the watcher is what keeps that true
 * across a reload, since the answer comes from the machine rather than from the
 * choice.
 */
const device = ref<'cpu' | 'cuda'>('cpu')
watch(
  () => runtime.value?.cuda.available,
  (available) => {
    if (!available) device.value = 'cpu'
  },
)

/**
 * Installs the AI environment.
 *
 * Nothing happens without this click: the download is several hundred megabytes
 * and may be on a metered connection, so it is offered rather than taken. The
 * figure and the destination are both stated, because "it is about to download
 * something" is not consent. Choosing the GPU set states the extra it costs in
 * the same place.
 */
async function install(): Promise<void> {
  installError.value = null
  try {
    await api.system.provisionRuntime(device.value === 'cuda')
    overview.run()
  } catch (cause) {
    installError.value = cause instanceof ApiError ? cause.message : String(cause)
  }
}

async function cancelInstall(): Promise<void> {
  installError.value = null
  try {
    await api.system.cancelProvision()
    overview.run()
  } catch (cause) {
    installError.value = cause instanceof ApiError ? cause.message : String(cause)
  }
}

// The stream carries the phases; this is what turns the last one into a
// refreshed page — the interpreter path, and the worker status beside it.
onMounted(() => {
  events.on('runtime.provision', (event) => {
    const data = event.data as { status?: string }
    if (data.status !== 'running') {
      overview.run()
    }
  })
})

function percent(value: number | null | undefined): string {
  return value === null || value === undefined ? '—' : `${value.toFixed(0)}%`
}

/**
 * Stops the server.
 *
 * The confirmation says what actually happens. Nothing is hidden behind it:
 * the process really does end, the page really does lose its connection, and
 * restarting means running the command again.
 */
async function shutdown(): Promise<void> {
  const confirmed = confirm(
    '关闭 NikuCooker 服务？\n\n' +
      '正在运行的作业会中断，这个页面会失去连接，需要重新启动进程才能继续使用。',
  )
  if (!confirmed) return

  shutdownError.value = null

  try {
    await api.system.shutdown()
    // Before the stream notices. The connection is about to fail because the
    // server is stopping, and telling the store now is what keeps that from
    // being reported as a dropped connection worth retrying.
    events.markStopped()
  } catch (cause) {
    shutdownError.value = cause instanceof ApiError ? cause.message : String(cause)
  }
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
            <dt class="shrink-0 text-ink-muted">加速</dt>
            <dd>
              {{
                runtime?.accelerator
                  ? (ACCELERATOR_LABEL[runtime.accelerator] ?? runtime.accelerator)
                  : '—'
              }}
            </dd>
          </div>
          <div class="flex justify-between gap-4">
            <dt class="shrink-0 text-ink-muted">协议摘要</dt>
            <dd class="truncate font-mono text-xs" :title="worker?.schema_digest">
              {{ worker?.schema_digest || '—' }}
            </dd>
          </div>
        </dl>
        <p
          v-if="worker?.detail"
          class="mt-3 rounded border border-status-warn/40 bg-surface p-3 text-xs text-status-warn"
        >
          {{ worker.detail }}
        </p>

        <p class="mt-3 border-t border-line pt-3 text-xs text-ink-faint">
          协议摘要由 Go 与 Python 各自从同一组 fixture 计算得出，握手时逐字比较。
          两边不一致时服务拒绝启动，而不是在中途把消息解释错。
        </p>

        <!--
          Installing the environment the worker runs from.

          Shown when the startup check found something to fix — which the server
          decides, in `runtime.available`. It is not shown merely because no
          environment has been provisioned: a checkout with a working ai/.venv
          transcribes perfectly and has never been provisioned, and offering it
          a 300 MB download would be answering a question nobody asked.

          When it is not offered but the worker is unusable, the card appears
          anyway with the reason instead of a button. Hiding it then would leave
          a user with a pipeline that cannot run and nothing on screen saying
          why.
        -->
        <div v-if="runtime && showRuntime" class="mt-3 border-t border-line pt-3">
          <p class="text-xs text-ink-faint">运行环境</p>

          <template v-if="runtime.status === 'running'">
            <ul class="mt-2 space-y-1 text-sm">
              <li
                v-for="step in INSTALL_STEPS"
                :key="step.phase"
                class="flex gap-2"
                :class="{
                  'text-ink': stepState(step.phase) === 'current',
                  'text-ink-muted': stepState(step.phase) === 'done',
                  'text-status-failed': stepState(step.phase) === 'failed',
                  'text-ink-faint': stepState(step.phase) === 'pending',
                }"
              >
                <span class="w-3 shrink-0">{{ stepMark(step.phase) }}</span>
                <span>{{ step.label }}</span>
              </li>
            </ul>
            <!-- No bar and no percentage: see the comment on INSTALL_STEPS. -->
            <AppButton class="mt-3" size="sm" variant="ghost" @click="cancelInstall">
              取消安装
            </AppButton>
          </template>

          <template v-else-if="runtime.status === 'failed' || runtime.status === 'cancelled'">
            <p
              v-if="runtime.remediation"
              class="mt-2 rounded border border-status-warn/40 bg-surface p-3 text-sm text-status-warn"
            >
              {{ runtime.remediation }}
            </p>
            <pre
              class="mt-2 max-h-40 overflow-auto rounded border border-line bg-surface-sunken p-2 text-xs whitespace-pre-wrap text-ink-muted"
            >{{ runtime.error_message }}</pre>
            <AppButton class="mt-3" size="sm" variant="primary" @click="install">
              重新安装
            </AppButton>
          </template>

          <template v-else-if="runtime.available">
            <p class="mt-1 text-sm text-ink-muted">
              语音识别需要一个 Python 环境。这个压缩包里带了安装工具和 worker 源码，
              但没有带 Python 本身——首次安装会下载解释器和依赖，约 300 MB，只下载这一次。
            </p>
            <p class="mt-1 text-xs text-ink-faint">
              装在 <span class="font-mono">{{ runtime.runtime_dir }}</span>，
              删掉这个目录即可回收空间。
            </p>

            <!--
              The accelerator, chosen before the download rather than measured
              after it. The GPU option carries its own price, and where there is
              no card to use the reason takes the option's place: a control that
              is present and refuses to move is the thing this avoids, and the
              line is visible on most machines rather than only on failures.
            -->
            <fieldset class="mt-3">
              <legend class="text-xs text-ink-faint">计算设备</legend>
              <div class="mt-2 space-y-2">
                <AppRadio
                  v-model="device"
                  name="runtime-device"
                  value="cpu"
                  label="CPU"
                  hint="任何机器都能跑。识别速度取决于处理器，Apple 芯片也只能走这条路。"
                />
                <AppRadio
                  v-model="device"
                  name="runtime-device"
                  value="cuda"
                  :disabled="!runtime.cuda.available"
                  :label="`NVIDIA 显卡${runtime.cuda.gpus.length > 0 ? `（${runtime.cuda.gpus.join('、')}）` : ''}`"
                  :hint="
                    runtime.cuda.available
                      ? `快得多，代价是多下载约 ${formatBytes(runtime.cuda.extra_bytes)} 的 NVIDIA 计算库。装好后自动启用，不用改配置。`
                      : cudaReason(runtime.cuda.reason_code)
                  "
                />
              </div>
            </fieldset>

            <AppButton class="mt-3" size="sm" variant="primary" @click="install">
              安装 AI 运行环境
            </AppButton>
          </template>

          <p v-else class="mt-1 text-sm text-ink-muted">{{ runtime.reason }}</p>

          <p
            v-if="installError"
            class="mt-2 rounded border border-status-failed/40 bg-surface p-3 text-sm text-status-failed"
          >
            {{ installError }}
          </p>
        </div>
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

      <section class="rounded border border-line bg-surface-raised p-4">
        <h2 class="text-sm font-medium text-ink-muted">关闭服务</h2>

        <p
          v-if="events.isShuttingDown"
          class="mt-3 rounded border border-status-warn/40 bg-surface p-3 text-sm text-status-warn"
        >
          服务正在关闭。这个页面不会再更新，可以关掉它了；要重新使用，请重新启动进程。
        </p>

        <template v-else>
          <p class="mt-3 text-sm text-ink-muted">
            结束服务进程，等同于在终端按 Ctrl-C。正在运行的作业会中断 ——
            但不会丢成果，下次运行会自动接着算。
          </p>
          <p
            v-if="shutdownError"
            class="mt-3 rounded border border-status-failed/40 bg-surface p-3 text-sm text-status-failed"
          >
            {{ shutdownError }}
          </p>

          <AppButton class="mt-3" variant="danger" @click="shutdown">关闭服务</AppButton>
        </template>
      </section>
    </template>
  </div>
</template>
