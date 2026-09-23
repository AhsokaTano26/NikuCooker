import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import SubtitleEditorView from './SubtitleEditorView.vue'
import type { Segment } from '@/types/api'

const mocks = vi.hoisted(() => ({
  list: vi.fn(),
  update: vi.fn(),
  review: vi.fn(),
}))

let records: Segment[] = []

vi.mock('@/api/client', () => ({
  API_BASE: '/api/v1',
  ApiError: class ApiError extends Error {},
  api: {
    projects: {
      get: vi.fn().mockResolvedValue({
        id: 'project-1',
        name: '第一话',
        duration: 120,
        segment_count: 2,
        needs_review_count: 2,
      }),
      editorMedia: vi.fn().mockResolvedValue({
        status: 'ready',
        source_url: '/source.mp4',
        direct_playback: true,
        media_kind: 'video',
      }),
      prepareEditorMedia: vi.fn(),
    },
    segments: {
      list: mocks.list,
      get: vi.fn(),
      update: mocks.update,
      review: mocks.review,
      translate: vi.fn(),
      split: vi.fn(),
      merge: vi.fn(),
      bulk: vi.fn(),
    },
  },
}))

function segment(ordinal: number): Segment {
  return {
    id: `segment-${ordinal}`,
    ordinal,
    start: (ordinal - 1) * 3,
    end: ordinal * 3,
    speaker: null,
    source_language: 'ja',
    target_language: 'zh-Hans',
    source_text: `原文 ${ordinal}`,
    translated_text: `译文 ${ordinal}`,
    words: [],
    asr_confidence: null,
    translation_confidence: null,
    cps: 4,
    needs_review: true,
    review_state: 'pending',
    is_edited: false,
    tags: [],
    metadata: {},
    qc: [
      {
        id: `qc-${ordinal}`,
        segment_id: `segment-${ordinal}`,
        segment_ordinal: ordinal,
        stage: 'qc',
        severity: 'warning',
        code: 'fast',
        message: '阅读速度过快',
        suggestion: '缩短译文',
        resolved: false,
        created_at: '2026-09-23T00:00:00Z',
      },
    ],
  }
}

async function renderEditor() {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/projects/:id', component: { template: '<div />' } },
      { path: '/projects/:id/editor', component: { template: '<div />' } },
    ],
  })
  await router.push('/projects/project-1/editor?filter=review')
  await router.isReady()

  const wrapper = mount(SubtitleEditorView, {
    global: { plugins: [router] },
  })
  await flushPromises()
  return wrapper
}

describe('subtitle review workflow', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    records = [segment(1), segment(2)]
    mocks.list.mockReset().mockImplementation(async (_projectId: string, query: { needs_review?: boolean }) => {
      const items = query.needs_review ? records.filter((line) => line.needs_review) : records
      return { items, total: items.length, limit: 200, offset: 0 }
    })
    mocks.update.mockReset().mockImplementation(async (_projectId: string, id: string, body: Record<string, unknown>) => {
      const index = records.findIndex((line) => line.id === id)
      records[index] = { ...records[index]!, ...body, is_edited: true }
      return records[index]
    })
    mocks.review.mockReset().mockImplementation(async (_projectId: string, id: string, state: Segment['review_state']) => {
      const index = records.findIndex((line) => line.id === id)
      records[index] = { ...records[index]!, review_state: state, needs_review: state === 'pending' }
      return records[index]
    })
  })

  it('saves an edited line, approves it and advances to the next review item', async () => {
    const wrapper = await renderEditor()

    expect(wrapper.text()).toContain('选择问题段')
    await wrapper.get('textarea').setValue('修正后的译文')
    await wrapper.get('[data-test="save-and-approve"]').trigger('click')
    await flushPromises()

    expect(mocks.update).toHaveBeenCalledWith('project-1', 'segment-1', expect.objectContaining({
      translated_text: '修正后的译文',
    }))
    expect(mocks.review).toHaveBeenCalledWith('project-1', 'segment-1', 'approved')
    expect(wrapper.get('[data-test="selected-segment"]').text()).toContain('第 2 条')
  })

  it('approves an unchanged line without pretending it was edited', async () => {
    const wrapper = await renderEditor()

    await wrapper.get('[data-test="approve-segment"]').trigger('click')
    await flushPromises()

    expect(mocks.update).not.toHaveBeenCalled()
    expect(mocks.review).toHaveBeenCalledWith('project-1', 'segment-1', 'approved')
    expect(wrapper.text()).not.toContain('打回')
    expect(wrapper.text()).toContain('保留待审')
  })

  it('leads back to final generation when the review queue is empty', async () => {
    records = [segment(1)]
    const wrapper = await renderEditor()

    await wrapper.get('[data-test="approve-segment"]').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('待审校字幕已经全部处理完')
    expect(wrapper.get('[data-test="finish-review"]').attributes('href')).toBe('/projects/project-1')
  })
})
