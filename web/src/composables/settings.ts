import type { SettingDescriptor } from '@/api/client'

function matchingOption(setting: SettingDescriptor, value: unknown) {
  const text = String(value ?? '')
  return setting.options?.find(
    (option) =>
      option.value === text ||
      (setting.kind === 'enum' && option.value.toLocaleLowerCase() === text.toLocaleLowerCase()),
  )
}

/** Turn an effective server value into the value held by a form control. */
export function editableSettingValue(setting: SettingDescriptor, value: unknown): unknown {
  if (setting.kind === 'list') {
    return Array.isArray(value) ? value.join(',') : String(value ?? '')
  }
  if (value === null || value === undefined) return ''

  // Old configuration files used display names such as "Fansub" while the
  // current catalog uses stable lowercase identifiers. Preserve compatibility
  // instead of leaving the select on its "please choose" placeholder.
  if (setting.kind === 'enum') return matchingOption(setting, value)?.value ?? value
  return value
}

/** Format a current/default value using the catalog's human-facing label. */
export function settingDisplayValue(setting: SettingDescriptor, value: unknown): string {
  if (String(value ?? '') === '***') return '已设置（内容不显示）'
  if (Array.isArray(value)) return value.length ? value.join('、') : '空列表'
  if (value === true) return '开启'
  if (value === false) return '关闭'
  if (value === '' || value === null || value === undefined) return '未设置'
  const option = matchingOption(setting, value)
  return option?.label ?? `${String(value)}${setting.unit ? ` ${setting.unit}` : ''}`
}

/** Compare a form value with the effective value using the same normalization. */
export function settingValueChanged(setting: SettingDescriptor, draftValue: unknown): boolean {
  if (draftValue === undefined) return false
  const currentValue = editableSettingValue(setting, setting.value)

  if (setting.kind === 'list') return String(draftValue) !== String(currentValue)
  if (setting.kind === 'int' || setting.kind === 'float' || setting.kind === 'bytes') {
    const draftNumber = Number(draftValue)
    const currentNumber = Number(currentValue)
    if (Number.isNaN(draftNumber) && Number.isNaN(currentNumber)) return false
    return draftNumber !== currentNumber
  }
  return draftValue !== currentValue
}
