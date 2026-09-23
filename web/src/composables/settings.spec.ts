import { describe, expect, it } from 'vitest'

import type { SettingDescriptor } from '@/api/client'

import { editableSettingValue, settingDisplayValue, settingValueChanged } from './settings'

const preset: SettingDescriptor = {
  key: 'subtitle.preset',
  name: '字幕样式',
  help: '',
  group: '字幕',
  kind: 'enum',
  options: [
    { value: 'fansub', label: '字幕组' },
    { value: 'default', label: '默认' },
  ],
  advanced: false,
  value: 'Fansub',
  default_value: 'Fansub',
  source: 'default',
  overridden: false,
}

describe('settings value presentation', () => {
  it('normalizes a legacy enum value to the matching option', () => {
    expect(editableSettingValue(preset, 'Fansub')).toBe('fansub')
  })

  it('uses the matching option label for a legacy enum value', () => {
    expect(settingDisplayValue(preset, 'Fansub')).toBe('字幕组')
  })

  it('does not mark a normalized legacy value as an unsaved edit', () => {
    expect(settingValueChanged(preset, 'fansub')).toBe(false)
  })
})
