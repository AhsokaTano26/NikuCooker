import type { CudaReasonCode, RuntimePhase, WorkerStatus } from '@/types/api'

/**
 * What the environment check's verdict means, in words.
 *
 * The server sends a stable identifier and never prose, so the label lives on
 * this side — in one place, because two copies is how the dashboard and the
 * System page come to call the same state by different names.
 *
 * `missing` in particular is not a word to put in front of someone who has
 * just installed this.
 */
export const WORKER_LABEL: Record<WorkerStatus, string> = {
  starting: '检查中…',
  ready: '可正常使用',
  busy: '运行中',
  crashed: '已崩溃',
  failed: '环境异常',
  missing: '尚未安装',
  stopped: '未启动',
}

/** Whether a state is one the user has to do something about. */
export function needsAttention(status: WorkerStatus | undefined): boolean {
  return status === 'missing' || status === 'failed'
}

/** How a state should read: a problem, work in progress, or nothing to say. */
export function attentionClass(status: WorkerStatus | undefined): string {
  if (needsAttention(status)) return 'text-status-failed'
  if (status === 'ready' || status === 'busy') return 'text-status-done'
  return ''
}

/**
 * The install's steps, in order.
 *
 * Shared because two pages render them now: the System page as the install runs,
 * and the dashboard while it is happening. Two copies is how the same step comes
 * to be called two different things on two screens.
 */
export const INSTALL_STEPS: { phase: RuntimePhase; label: string }[] = [
  { phase: 'detect', label: '检查环境' },
  { phase: 'interpreter', label: '下载 Python 解释器（约 40 MB）' },
  { phase: 'dependencies', label: '安装识别依赖（约 300 MB）' },
  { phase: 'verify', label: '自检' },
]

/** The label for one step, for a display that shows only the current one. */
export function installStepLabel(phase: RuntimePhase | undefined): string {
  return INSTALL_STEPS.find((step) => step.phase === phase)?.label ?? ''
}

/**
 * Why the GPU option is not offered, in words.
 *
 * The server sends a code and never a sentence, following the same rule as
 * WORKER_LABEL — and for a stronger reason here, because this line is not an
 * error message that appears on failures: it sits under the GPU option on every
 * machine that cannot use one, which is most of them. A code with no wording
 * renders as an unexplained disabled control, so the Go test that owns the
 * codes compares them against this map.
 */
/**
 * A record over the union rather than over `string`, so a code the server
 * starts sending and this file does not is a type error rather than a blank
 * line under the option.
 */
export const CUDA_REASON: Record<CudaReasonCode, string> = {
  platform: 'NVIDIA 的加速库没有 macOS 版本，而这台机器上的识别本来就跑在 CPU 上。',
  no_gpu: '没有检测到 NVIDIA 显卡，装了也用不上。',
}

/** The reason for one code, or the code itself if this build has no wording. */
export function cudaReason(code: CudaReasonCode | undefined): string {
  if (!code) return ''
  return CUDA_REASON[code] ?? code
}

/**
 * What the installed environment was built to run on.
 *
 * Named by the dependency set, not by what the machine has: the two differ for
 * anyone whose card is idle because they installed the default set, which is
 * the question this row exists to answer.
 */
export const ACCELERATOR_LABEL: Record<string, string> = {
  cpu: 'CPU',
  cuda: 'NVIDIA CUDA',
}
