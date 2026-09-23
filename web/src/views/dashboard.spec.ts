/**
 * Tests for the dashboard's environment warning.
 *
 * This is the one thing on the page a user has to act on, and its failure mode
 * is silence: a machine that cannot transcribe shows a dashboard that looks
 * entirely normal, and the user finds out when a job fails instead. So what is
 * asserted is which states produce a warning and which do not — the second half
 * matters as much, because a banner that appears on a working installation is
 * one people learn to ignore.
 */

import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import DashboardView from './DashboardView.vue'
import type { SystemOverview, WorkerStatus } from '@/types/api'

// The page fetches through the API client; only the worker status varies here.
const overview = vi.fn()
vi.mock('@/api/client', () => ({
  api: { system: { overview: () => overview() } },
  ApiError: class extends Error {},
}))

/** A dashboard payload whose only interesting field is the worker status. */
function payload(status: WorkerStatus, detail?: string): Partial<SystemOverview> {
  return {
    version: 'test',
    commit: 'test',
    platform: 'test',
    uptime_s: 0,
    counts: { projects: 0, jobs_running: 0, jobs_pending: 0, segments: 0, needs_review: 0 },
    worker: {
      status,
      workers: 0,
      python: status === 'ready' ? '/data/runtime/venv/bin/python' : '',
      detail,
      worker_version: '',
      schema_digest: '',
      loaded_models: [],
    },
    runtime: {
      available: status === 'missing',
      status: 'idle',
      provisioned: false,
      runtime_dir: '/data/runtime',
    },
    stats: {
      cpu: { cores: 1, usage_percent: null },
      memory: { total_bytes: 0, used_bytes: 0 },
      disk: { data_free_bytes: 0, models_free_bytes: 0 },
      gpu: [],
    },
  } as unknown as Partial<SystemOverview>
}

async function dashboard(status: WorkerStatus, detail?: string) {
  overview.mockResolvedValue(payload(status, detail))

  const wrapper = mount(DashboardView, {
    global: { stubs: { RouterLink: { template: '<a><slot /></a>' } } },
  })
  await flushPromises()

  return wrapper
}

describe('the dashboard environment warning', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    overview.mockReset()
  })

  it('offers an install when nothing is installed', async () => {
    const wrapper = await dashboard('missing', 'no Python interpreter was found')
    const text = wrapper.text()

    expect(text).toContain('还没有安装 AI 运行环境')
    expect(text).toContain('去安装')
    // The search's own words, so the user has something to search for.
    expect(text).toContain('no Python interpreter was found')
  })

  it('offers to reinstall when the environment is broken', async () => {
    const wrapper = await dashboard('failed', 'No module named nikucooker_ai')
    const text = wrapper.text()

    expect(text).toContain('AI 运行环境有问题')
    expect(text).toContain('重新安装')
  })

  // The half that keeps the warning worth reading.
  it.each<WorkerStatus>(['ready', 'starting'])('says nothing when %s', async (status) => {
    const wrapper = await dashboard(status)
    const text = wrapper.text()

    expect(text).not.toContain('去安装')
    expect(text).not.toContain('重新安装')
  })
})
