import { describe, expect, it } from 'vitest'

import { findActiveSegmentIndex, pipelineProgress } from './workbench'
import type { PipelineView, Segment } from '@/types/api'

const segment = (start: number, end: number): Segment => ({
  id: `${start}`,
  ordinal: start + 1,
  start,
  end,
  speaker: null,
  source_language: 'ja',
  target_language: 'zh-Hans',
  source_text: '',
  translated_text: '',
  words: [],
  asr_confidence: null,
  translation_confidence: null,
  cps: null,
  needs_review: false,
  review_state: 'none',
  is_edited: false,
  tags: [],
  metadata: {},
})

describe('workbench timeline helpers', () => {
  it('finds the line containing the current playback time', () => {
    const lines = [segment(0, 2), segment(3, 5), segment(8, 10)]
    expect(findActiveSegmentIndex(lines, 4)).toBe(1)
    expect(findActiveSegmentIndex(lines, 6)).toBe(-1)
  })

  it('uses completed stages plus only the running stage real fraction', () => {
    const view = {
      project_id: 'p',
      job: { id: 'j', status: 'running', progress: 0 },
      stages: [
        { name: 'a', label: 'A', ordinal: 1, status: 'completed', progress: 1, artifact_id: null },
        { name: 'b', label: 'B', ordinal: 2, status: 'running', progress: 0.5, artifact_id: null },
        { name: 'c', label: 'C', ordinal: 3, status: 'pending', progress: 0, artifact_id: null },
      ],
    } satisfies PipelineView

    expect(pipelineProgress(view)).toEqual({ fraction: 0.5, completed: 1, current: 2, total: 3 })
  })

  it('treats a completed job as fully settled even when an optional stage stayed pending', () => {
    const view = {
      project_id: 'p',
      job: { id: 'j', status: 'completed', progress: 1 },
      stages: [
        { name: 'a', label: 'A', ordinal: 1, status: 'completed', progress: 1, artifact_id: null },
        { name: 'optional', label: '可选阶段', ordinal: 2, status: 'pending', progress: 0, artifact_id: null },
      ],
    } satisfies PipelineView

    expect(pipelineProgress(view)).toEqual({ fraction: 1, completed: 2, current: 2, total: 2 })
  })
})
