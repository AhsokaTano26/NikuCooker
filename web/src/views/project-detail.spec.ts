import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import ProjectDetailView from './ProjectDetailView.vue'

const mocks = vi.hoisted(() => ({
  getProject: vi.fn(),
  getPipeline: vi.fn(),
  getOutputs: vi.fn(),
  getFiles: vi.fn(),
  startRun: vi.fn(),
}))

vi.mock('@/api/client', () => ({
  API_BASE: '/api/v1',
  ApiError: class ApiError extends Error {},
  api: {
    projects: {
      get: mocks.getProject,
      outputs: mocks.getOutputs,
      files: mocks.getFiles,
      outputURL: (id: string, name: string) => `/api/v1/projects/${id}/outputs/${name}`,
      logURL: (id: string, name: string) => `/api/v1/projects/${id}/logs/${name}`,
      removeFiles: vi.fn(),
    },
    run: {
      pipeline: mocks.getPipeline,
      start: mocks.startRun,
      cancel: vi.fn(),
      runStage: vi.fn(),
    },
  },
}))

const completedPipeline = {
  project_id: 'project-1',
  job: { id: 'job-1', status: 'completed', progress: 1 },
  stages: [
    {
      name: 'asr',
      label: '语音识别',
      ordinal: 1,
      status: 'completed',
      progress: 1,
      artifact_id: 'artifact-1',
    },
  ],
}

function project(needsReview: number) {
  return {
    id: 'project-1',
    name: '第一话',
    source_language: 'ja',
    target_language: 'zh-Hans',
    style: 'fansub',
    status: 'active',
    duration: 120,
    size_bytes: 4096,
    segment_count: 12,
    needs_review_count: needsReview,
    current_job: { id: 'job-1', status: 'completed', progress: 1, current_stage: null },
    created_at: '2026-09-23T00:00:00Z',
    updated_at: '2026-09-23T00:00:00Z',
  }
}

async function renderProject(needsReview: number) {
  mocks.getProject.mockResolvedValue(project(needsReview))
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/projects/:id', component: { template: '<div />' } },
      { path: '/projects/:id/editor', component: { template: '<div />' } },
    ],
  })
  await router.push('/projects/project-1')
  await router.isReady()

  const wrapper = mount(ProjectDetailView, {
    global: { plugins: [router] },
  })
  await flushPromises()
  return wrapper
}

describe('completed project handoff', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mocks.getProject.mockReset()
    mocks.getPipeline.mockReset().mockResolvedValue(completedPipeline)
    mocks.getOutputs.mockReset().mockResolvedValue({
      dir: '/data/projects/project-1/output',
      items: [
        {
          name: 'episode.zh-Hans.srt',
          size_bytes: 2048,
          modified_at: '2026-09-23T00:00:00Z',
          kind: 'subtitle',
          description: '通用字幕文件',
        },
      ],
    })
    mocks.getFiles.mockReset().mockResolvedValue({
      total_bytes: 4096,
      items: [
        { kind: 'output', name: '成品', bytes: 2048, files: 1, removable: true, warning: '可重新生成。' },
      ],
      logs: [],
    })
    mocks.startRun.mockReset().mockResolvedValue(completedPipeline)
  })

  it('puts downloads and the review handoff directly after progress', async () => {
    const wrapper = await renderProject(3)
    const text = wrapper.text()

    const progress = text.indexOf('任务已完成')
    const outputs = text.indexOf('处理结果')
    const review = text.indexOf('字幕审校')
    const stages = text.indexOf('处理阶段')
    const storage = text.indexOf('磁盘占用')

    expect(progress).toBeGreaterThanOrEqual(0)
    expect(progress).toBeLessThan(outputs)
    expect(outputs).toBeLessThan(review)
    expect(review).toBeLessThan(stages)
    expect(stages).toBeLessThan(storage)
    expect(wrapper.get('[data-test="open-review-queue"]').attributes('href')).toContain('filter=review')
    expect(text).toContain('3 条需要人工确认')
    expect(text).toContain('选择问题段')
  })

  it('distinguishes rerunning the workflow from generating reviewed outputs', async () => {
    const wrapper = await renderProject(0)

    expect(wrapper.get('[data-test="rerun-project"]').text()).toBe('重新运行全部流程')
    expect(wrapper.get('[data-test="overall-progress-bar"]').classes()).toContain('bg-status-done')
    expect(wrapper.get('[data-test="generate-final-outputs"]').text()).toBe('生成最终成品')
  })

  it('starts one cached run to generate final outputs after review', async () => {
    const wrapper = await renderProject(0)
    const button = wrapper.find('[data-test="generate-final-outputs"]')

    expect(button.exists()).toBe(true)
    expect(wrapper.text()).toContain('没有待处理的质量问题')
    await button.trigger('click')
    await flushPromises()

    expect(mocks.startRun).toHaveBeenCalledWith('project-1', {})
  })
})
