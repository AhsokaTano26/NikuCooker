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
import type { RuntimeStatus, SystemOverview, WorkerStatus } from '@/types/api'

// The page fetches through the API client; only the worker status varies here.
const overview = vi.fn()
vi.mock('@/api/client', () => ({
  api: { system: { overview: () => overview() } },
  ApiError: class extends Error {},
}))

/** A dashboard payload whose only interesting fields are the environment ones. */
function payload(
  status: WorkerStatus,
  detail?: string,
  install: { status: RuntimeStatus; phase?: string } = { status: 'idle' },
): Partial<SystemOverview> {
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
      available: status === 'missing' && install.status !== 'running',
      status: install.status,
      phase: install.phase,
      provisioned: status === 'ready',
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

async function dashboard(
  status: WorkerStatus,
  detail?: string,
  install?: { status: RuntimeStatus; phase?: string },
) {
  overview.mockResolvedValue(payload(status, detail, install))

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

  // The state the page shows while an install is running.
  //
  // The check's verdict does not move until the install finishes, so the worker
  // status is still "missing" throughout — and a page that read only that would
  // say "尚未安装" over a download that is halfway done, at the one moment
  // someone is looking at it to see how it is going.
  it('reports an install in progress, not the stale state behind it', async () => {
    const wrapper = await dashboard('missing', undefined, {
      status: 'running',
      phase: 'dependencies',
    })
    const text = wrapper.text()

    expect(text).toContain('安装中…')
    expect(text).toContain('安装识别依赖')
    // And offers no button for starting a second one.
    expect(text).not.toContain('去安装')
  })

  it('shows the interpreter once the environment works', async () => {
    const wrapper = await dashboard('ready')
    const text = wrapper.text()

    expect(text).toContain('可正常使用')
    expect(text).toContain('/data/runtime/venv/bin/python')
    expect(text).toContain('/data/runtime')
  })

  // The half that keeps the warning worth reading.
  it.each<WorkerStatus>(['ready', 'starting'])('says nothing when %s', async (status) => {
    const wrapper = await dashboard(status)
    const text = wrapper.text()

    expect(text).not.toContain('去安装')
    expect(text).not.toContain('重新安装')
  })
})
