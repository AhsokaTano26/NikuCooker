import type { RuntimePhase, WorkerStatus } from '@/types/api'

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
