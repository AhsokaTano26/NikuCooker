import type { PipelineView, Segment } from '@/types/api'

/** Finds the segment containing a playback time in a start-sorted window. */
export function findActiveSegmentIndex(lines: Segment[], time: number): number {
  let low = 0
  let high = lines.length - 1

  while (low <= high) {
    const middle = Math.floor((low + high) / 2)
    const line = lines[middle]
    if (line === undefined) return -1
    if (time < line.start) high = middle - 1
    else if (time >= line.end) low = middle + 1
    else return middle
  }
  return -1
}

/** Honest overall progress when stages have equal weight. */
export function pipelineProgress(view: PipelineView | null | undefined): {
  fraction: number
  completed: number
  current: number
  total: number
} {
  const stages = view?.stages ?? []
  if (stages.length === 0) return { fraction: 0, completed: 0, current: 0, total: 0 }

  const completed = stages.filter((stage) =>
    ['completed', 'cached', 'skipped'].includes(stage.status),
  ).length
  const runningIndex = stages.findIndex((stage) => stage.status === 'running')
  const runningProgress = runningIndex >= 0 ? Math.max(0, Math.min(1, stages[runningIndex]?.progress ?? 0)) : 0

  return {
    fraction: (completed + runningProgress) / stages.length,
    completed,
    current: runningIndex >= 0 ? runningIndex + 1 : Math.min(completed, stages.length),
    total: stages.length,
  }
}
