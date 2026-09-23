import { flushPromises, mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import HelpView from './HelpView.vue'

vi.mock('@/api/client', () => ({
  api: {
    system: { overview: vi.fn().mockResolvedValue({ worker: { status: 'ready' }, counts: { segments: 12 } }) },
    models: { list: vi.fn().mockResolvedValue({ items: [{ kind: 'asr', status: 'ready' }] }) },
    providers: { list: vi.fn().mockResolvedValue({ items: [{ enabled: true, has_key: true }] }) },
    projects: {
      list: vi.fn().mockResolvedValue({
        items: [{ segment_count: 12, needs_review_count: 0 }],
        total: 1,
      }),
    },
  },
}))

describe('new user help', () => {
  it('presents the complete workflow as actionable steps with live readiness', async () => {
    const wrapper = mount(HelpView, {
      global: {
        stubs: { RouterLink: { props: ['to'], template: '<a :data-to="to"><slot /></a>' } },
      },
    })
    await flushPromises()

    const text = wrapper.text()
    for (const title of ['检查运行环境', '配置翻译服务', '准备识别模型', '创建第一个项目', '运行处理任务', '审校并导出']) {
      expect(text).toContain(title)
    }
    expect(text).toContain('已完成')
    expect(wrapper.findAll('[data-test="starter-step-done"]')).toHaveLength(6)
    expect(wrapper.findAll('a[data-to]').length).toBeGreaterThanOrEqual(6)
  })
})
