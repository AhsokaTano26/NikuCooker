import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import ModelsView from './ModelsView.vue'

const mocks = vi.hoisted(() => ({
  list: vi.fn(),
  getSettings: vi.fn(),
  updateSettings: vi.fn(),
}))

function settings(endpoint = 'https://huggingface.co') {
  return {
    config: { models: { endpoint } },
    provenance: {},
    data_dir: '/tmp/data',
    config_path: '/tmp/config.yaml',
    config_file_exists: false,
    catalog: [
      {
        key: 'models.endpoint',
        name: '模型下载源',
        help: 'Hugging Face 兼容服务的基础地址。',
        group: '模型下载',
        kind: 'string',
        advanced: false,
        value: endpoint,
        default_value: 'https://huggingface.co',
        source: endpoint === 'https://huggingface.co' ? 'default' : 'database',
        overridden: endpoint !== 'https://huggingface.co',
      },
    ],
  }
}

vi.mock('@/api/client', () => ({
  ApiError: class ApiError extends Error {},
  api: {
    models: {
      list: mocks.list,
      download: vi.fn(),
      remove: vi.fn(),
    },
    settings: {
      get: mocks.getSettings,
      update: mocks.updateSettings,
    },
  },
}))

describe('model download source', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mocks.list.mockReset().mockResolvedValue({ items: [] })
    mocks.getSettings.mockReset().mockResolvedValue(settings())
    mocks.updateSettings.mockReset().mockImplementation(async (values: Record<string, unknown>) =>
      settings(String(values['models.endpoint'])),
    )
  })

  it('shows the effective endpoint and saves a compatible mirror', async () => {
    const wrapper = mount(ModelsView)
    await flushPromises()

    expect(wrapper.get('label[for="model-endpoint-input"]').text()).toBe('基础地址')
    const input = wrapper.get('[data-test="model-endpoint"] input')
    expect((input.element as HTMLInputElement).value).toBe('https://huggingface.co')

    await input.setValue('https://mirror.example.com')
    await wrapper.get('[data-test="save-model-endpoint"]').trigger('click')
    await flushPromises()

    expect(mocks.updateSettings).toHaveBeenCalledWith({
      'models.endpoint': 'https://mirror.example.com',
    })
    expect(wrapper.text()).toContain('当前生效：https://mirror.example.com')
    expect(wrapper.get('[role="status"]').text()).toContain('下一次下载立即使用新地址')
  })

  it('restores the official endpoint with one action', async () => {
    mocks.getSettings.mockResolvedValue(settings('https://mirror.example.com'))

    const wrapper = mount(ModelsView)
    await flushPromises()
    await wrapper.get('[data-test="use-official-model-endpoint"]').trigger('click')
    await flushPromises()

    expect(mocks.updateSettings).toHaveBeenCalledWith({
      'models.endpoint': 'https://huggingface.co',
    })
  })
})
