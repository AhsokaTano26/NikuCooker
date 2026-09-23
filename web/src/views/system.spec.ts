import { flushPromises, mount } from '@vue/test-utils'
import type { VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import SystemView from './SystemView.vue'
import type { CudaView, SystemOverview } from '@/types/api'

// The page reads two endpoints and writes a third; the event stream is not
// exercised here, so the store is left to its real implementation over an empty
// connection.
const overview = vi.fn()
const provisionRuntime = vi.fn()
vi.mock('@/api/client', () => ({
  api: {
    system: {
      overview: () => overview(),
      provisionRuntime: (cuda: boolean) => provisionRuntime(cuda),
      cancelProvision: () => Promise.resolve(),
    },
  },
  ApiError: class extends Error {},
}))

/** A system payload whose only interesting part is the GPU option. */
function payload(cuda: Partial<CudaView>): Partial<SystemOverview> {
  return {
    version: 'test',
    commit: 'test',
    platform: 'linux-amd64',
    uptime_s: 0,
    counts: { projects: 0, jobs_running: 0, jobs_pending: 0, segments: 0, needs_review: 0 },
    worker: {
      status: 'missing',
      workers: 0,
      python: '',
      worker_version: '',
      schema_digest: '',
      loaded_models: [],
    },
    runtime: {
      available: true,
      status: 'idle',
      provisioned: false,
      runtime_dir: '/data/runtime',
      cuda: {
        available: false,
        gpus: [],
        extra_bytes: 700 << 20,
        ...cuda,
      },
    },
    features: { path_source: false, max_upload_bytes: 0, config_path: '', config_file_exists: false },
    stats: {
      cpu: { cores: 1, usage_percent: null },
      memory: { total_bytes: 0, used_bytes: 0 },
      disk: { data_free_bytes: 0, models_free_bytes: 0 },
      gpu: [],
    },
  } as unknown as Partial<SystemOverview>
}

async function system(cuda: Partial<CudaView>) {
  overview.mockResolvedValue(payload(cuda))

  const wrapper = mount(SystemView)
  await flushPromises()

  return wrapper
}

/** The nth radio, or a failure naming what is missing. */
function radio(wrapper: VueWrapper, index: number) {
  const input = wrapper.findAll('input[type="radio"]').at(index)
  if (!input) throw new Error(`radio ${index} is not on the page`)
  return input
}

/** The install button, which the page shows only when there is something to do. */
async function clickInstall(wrapper: VueWrapper) {
  const button = wrapper.findAll('button').find((candidate) => candidate.text().includes('安装 AI 运行环境'))
  if (!button) throw new Error('the install button is not on the page')

  await button.trigger('click')
  await flushPromises()
}

describe('the GPU choice on the system page', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    overview.mockReset()
    provisionRuntime.mockReset()
    provisionRuntime.mockResolvedValue(undefined)
  })

  // A card in the machine is not the question being answered: the option is
  // offered from what the server says the install can use, and where it cannot,
  // the reason takes the option's place.
  it('explains itself instead of offering the GPU set', async () => {
    const wrapper = await system({ available: false, reason_code: 'no_gpu' })
    const text = wrapper.text()

    expect(text).toContain('没有检测到 NVIDIA 显卡')
    // The default is still there, and is what the page would install.
    expect(text).toContain('CPU')
    // And the option that cannot be used cannot be picked either — the server
    // would refuse the request it would produce.
    expect(radio(wrapper, 1).attributes('disabled')).toBeDefined()
  })

  it('names the card it found and what the option costs', async () => {
    const wrapper = await system({ available: true, gpus: ['NVIDIA GeForce RTX 4090'] })
    const text = wrapper.text()

    expect(text).toContain('NVIDIA GeForce RTX 4090')
    expect(text).toContain('700.0 MB')
  })

  it('asks for the default set when nothing is chosen', async () => {
    const wrapper = await system({ available: true, gpus: ['NVIDIA GeForce RTX 4090'] })

    await clickInstall(wrapper)

    expect(provisionRuntime).toHaveBeenCalledWith(false)
  })

  it('asks for CUDA once it is chosen', async () => {
    const wrapper = await system({ available: true, gpus: ['NVIDIA GeForce RTX 4090'] })

    await radio(wrapper, 1).setValue()
    await clickInstall(wrapper)

    expect(provisionRuntime).toHaveBeenCalledWith(true)
  })
})
