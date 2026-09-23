import type { WorkerStatus } from '@/types/api'

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
