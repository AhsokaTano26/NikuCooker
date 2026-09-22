<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'

import { ApiError, api, type SettingDescriptor } from '@/api/client'
import AppBadge from '@/components/AppBadge.vue'
import AppButton from '@/components/AppButton.vue'
import AppCheckbox from '@/components/AppCheckbox.vue'
import AppInput from '@/components/AppInput.vue'
import AppSelect from '@/components/AppSelect.vue'
import { useAsync } from '@/composables/useAsync'
import { useEventStore } from '@/stores/events'

const events = useEventStore()
const settings = useAsync(() => api.settings.get())

/**
 * What the form holds.
 *
 * Seeded from the catalog and compared against it, rather than posted
 * wholesale: a save that sent every field would store a row for all forty of
 * them, and the reset control would become meaningless — every key would be
 * overridden from the moment the page was first saved.
 */
const draft = ref<Record<string, unknown>>({})
const saving = ref(false)
const notice = ref<string | null>(null)
const actionError = ref<string | null>(null)
const showAdvanced = ref(false)

/** The filter, for finding a setting by name when the page is long. */
const filter = ref('')

const catalog = computed(() => settings.data.value?.catalog ?? [])

/** Seeds the draft from the server's values. */
function reseed(): void {
  const next: Record<string, unknown> = {}
  for (const setting of catalog.value) {
    next[setting.key] = editableOf(setting, setting.value)
  }
  draft.value = next
}

onMounted(async () => {
  await settings.run()
  reseed()

  unsubscribers.push(
    // A change made in another tab, or from the CLI, means this form is
    // showing values that are no longer in effect.
    events.on('settings.changed', async () => {
      await settings.run()
      reseed()
    }),
  )
})

const unsubscribers: (() => void)[] = []
onBeforeUnmount(() => {
  for (const unsubscribe of unsubscribers) unsubscribe()
})

/** Turns a stored value into what a control holds: a list becomes text. */
function editableOf(setting: SettingDescriptor, value: unknown): unknown {
  if (setting.kind === 'list') {
    return Array.isArray(value) ? value.join(',') : String(value ?? '')
  }
  if (value === null || value === undefined) return ''
  return value
}

/** Whether a fixed list currently contains a member. */
function listHas(setting: SettingDescriptor, value: string): boolean {
  return String(draft.value[setting.key] ?? '')
    .split(',')
    .map((part) => part.trim())
    .includes(value)
}

/** Adds or removes one member of a fixed list. */
function toggleList(setting: SettingDescriptor, value: string): void {
  const current = String(draft.value[setting.key] ?? '')
    .split(',')
    .map((part) => part.trim())
    .filter((part) => part !== '')

  const next = current.includes(value)
    ? current.filter((part) => part !== value)
    : [...current, value]

  draft.value = { ...draft.value, [setting.key]: next.join(',') }
}

/** Turns a control's value back into what the server expects. */
function payloadOf(setting: SettingDescriptor, value: unknown): unknown {
  switch (setting.kind) {
    case 'int':
    case 'bytes':
      return Math.round(Number(value))
    case 'float':
      return Number(value)
    case 'list':
      return String(value ?? '')
        .split(',')
        .map((part) => part.trim())
        .filter((part) => part !== '')
    default:
      return value
  }
}

/** Whether a control's value differs from what is in effect. */
function isChanged(setting: SettingDescriptor): boolean {
  const draftValue = draft.value[setting.key]
  if (draftValue === undefined) return false

  if (setting.kind === 'list') {
    const current = Array.isArray(setting.value) ? setting.value.map(String).join(',') : ''
    return String(draftValue) !== current
  }
  if (setting.kind === 'int' || setting.kind === 'float' || setting.kind === 'bytes') {
    // The control holds text; the server holds a number. Comparing the two
    // without this would mark every numeric field as changed on load.
    const a = Number(draftValue)
    const b = Number(setting.value)
    if (Number.isNaN(a) && Number.isNaN(b)) return false
    return a !== b
  }
  return draftValue !== setting.value
}

const changedKeys = computed(() => catalog.value.filter(isChanged).map((setting) => setting.key))

/** The catalog arranged for display, with the filter and the advanced toggle applied. */
const groups = computed(() => {
  const needle = filter.value.trim().toLowerCase()
  const out: { group: string; settings: SettingDescriptor[] }[] = []

  for (const setting of catalog.value) {
    if (setting.advanced && !showAdvanced.value) continue
    if (
      needle !== '' &&
      !setting.key.toLowerCase().includes(needle) &&
      !setting.name.toLowerCase().includes(needle)
    ) {
      continue
    }

    let bucket = out.find((entry) => entry.group === setting.group)
    if (bucket === undefined) {
      bucket = { group: setting.group, settings: [] }
      out.push(bucket)
    }
    bucket.settings.push(setting)
  }
  return out
})

const advancedCount = computed(() => catalog.value.filter((setting) => setting.advanced).length)

async function save(): Promise<void> {
  if (changedKeys.value.length === 0) return

  saving.value = true
  notice.value = null
  actionError.value = null

  const values: Record<string, unknown> = {}
  for (const setting of catalog.value) {
    if (isChanged(setting)) values[setting.key] = payloadOf(setting, draft.value[setting.key])
  }

  try {
    const updated = await api.settings.update(values)
    settings.data.value = updated
    reseed()
    notice.value = `已保存 ${Object.keys(values).length} 项`
  } catch (cause) {
    // The server's refusal names the setting and what is wrong with it, which
    // is more useful than anything the form can work out on its own.
    actionError.value = cause instanceof ApiError ? cause.message : String(cause)
  } finally {
    saving.value = false
  }
}

async function reset(setting: SettingDescriptor): Promise<void> {
  actionError.value = null
  notice.value = null

  try {
    await api.settings.reset(setting.key)
    await settings.run()
    reseed()
    notice.value = `「${setting.name}」已恢复默认`
  } catch (cause) {
    actionError.value = cause instanceof ApiError ? cause.message : String(cause)
  }
}

const SOURCE_LABEL: Record<string, string> = {
  default: '默认',
  config_file: '配置文件',
  environment: '环境变量',
  database: '网页',
  project: '项目覆盖',
  cli: '命令行',
}

/** A value the server withheld. */
function isSecret(value: string): boolean {
  return value === '***'
}

// ---------------------------------------------------------------------------
// The read-only table underneath
// ---------------------------------------------------------------------------

const tableFilter = ref('')

/**
 * Every leaf of the configuration, flattened to dotted paths.
 *
 * Kept alongside the form because the two answer different questions: the form
 * answers "how do I change this", the table answers "why is it this". The
 * second is what you want when a setting did not do what you expected.
 */
const rows = computed(() => {
  const config = settings.data.value?.config ?? {}
  const provenance = settings.data.value?.provenance ?? {}

  const out: { path: string; value: string; source: string }[] = []

  const walk = (value: unknown, prefix: string): void => {
    if (value !== null && typeof value === 'object' && !Array.isArray(value)) {
      for (const [key, nested] of Object.entries(value as Record<string, unknown>)) {
        walk(nested, prefix === '' ? key : `${prefix}.${key}`)
      }
      return
    }

    out.push({
      path: prefix,
      value: Array.isArray(value) ? value.join(', ') : String(value ?? ''),
      source: provenance[prefix] ?? 'default',
    })
  }

  walk(config, '')

  const needle = tableFilter.value.trim().toLowerCase()
  const filtered =
    needle === '' ? out : out.filter((row) => row.path.toLowerCase().includes(needle))

  return filtered.sort((a, b) => a.path.localeCompare(b.path))
})

watch(settings.data, (value) => {
  if (value !== null && Object.keys(draft.value).length === 0) reseed()
})
</script>

<template>
  <div class="space-y-4">
    <p class="text-sm text-ink-muted">
      这里的改动立即生效，下一次运行就用新设置。需要写明文件的事情
      <code class="rounded bg-surface-sunken px-1.5 py-0.5 font-mono text-xs">nikucooker config path</code>
      会告诉你 —— 但通常不需要。
    </p>

    <p
      v-if="settings.error.value"
      class="rounded border border-status-failed/40 bg-surface-raised p-3 text-sm text-status-failed"
    >
      {{ settings.error.value }}
      <AppButton variant="ghost" size="sm" class="ml-2" @click="settings.run">重试</AppButton>
    </p>

    <p v-else-if="settings.loading.value && !settings.data.value" class="p-6 text-center text-sm text-ink-muted">
      加载中…
    </p>

    <template v-else>
      <p v-if="notice" class="rounded border border-status-done/40 bg-surface-raised p-3 text-sm text-status-done">
        {{ notice }}
      </p>
      <p
        v-if="actionError"
        class="whitespace-pre-line rounded border border-status-failed/40 bg-surface-raised p-3 text-sm text-status-failed"
      >
        {{ actionError }}
      </p>

      <!-- Filter and the advanced toggle, then Save. Pinned to the top of the
           form because the save control is what everything above it is for,
           and on a long page it would otherwise be a scroll away. -->
      <div class="flex flex-wrap items-center gap-3">
        <div class="min-w-56 flex-1">
          <AppInput v-model="filter" type="search" placeholder="筛选，例如 模型 或 asr" />
        </div>
        <AppCheckbox v-model="showAdvanced" :label="`高级（${advancedCount}）`" />
        <AppButton
          variant="primary"
          :disabled="changedKeys.length === 0 || saving"
          @click="save"
        >
          {{ saving ? '保存中…' : changedKeys.length === 0 ? '没有改动' : `保存 ${changedKeys.length} 项` }}
        </AppButton>
      </div>

      <section
        v-for="entry in groups"
        :key="entry.group"
        class="rounded border border-line bg-surface-raised"
      >
        <header class="border-b border-line px-4 py-2">
          <h2 class="text-sm font-medium text-ink-muted">{{ entry.group }}</h2>
        </header>

        <ul class="divide-y divide-line/60">
          <li v-for="setting in entry.settings" :key="setting.key" class="flex gap-4 px-4 py-3">
            <div class="min-w-0 flex-1">
              <div class="flex flex-wrap items-center gap-2">
                <label :for="setting.key" class="text-sm">{{ setting.name }}</label>

                <!-- Where a value came from matters most when it is somewhere
                     the user has forgotten about. -->
                <AppBadge v-if="setting.source !== 'default'">
                  {{ SOURCE_LABEL[setting.source] ?? setting.source }}
                </AppBadge>
                <AppBadge v-if="isChanged(setting)" tone="warn">已改动</AppBadge>
                <AppButton v-if="setting.overridden" variant="ghost" size="sm" @click="reset(setting)">
                  恢复默认
                </AppButton>
              </div>

              <p class="mt-1 text-xs text-ink-faint">{{ setting.help }}</p>
              <p class="mt-0.5 font-mono text-xs text-ink-faint">{{ setting.key }}</p>
            </div>

            <div class="w-72 shrink-0">
              <!-- On or off. -->
              <AppCheckbox
                v-if="setting.kind === 'bool'"
                v-model="draft[setting.key] as boolean"
                :label="draft[setting.key] ? '开启' : '关闭'"
              />

              <!-- A fixed set of choices. -->
              <AppSelect
                v-else-if="setting.kind === 'enum'"
                :id="setting.key"
                v-model="draft[setting.key] as string"
                :options="setting.options ?? []"
              />

              <!-- A list with fixed members is a set of checkboxes: it is the
                   only control that says what the valid values are. -->
              <div v-else-if="setting.kind === 'list' && (setting.options?.length ?? 0) > 0" class="space-y-1.5">
                <AppCheckbox
                  v-for="option in setting.options ?? []"
                  :key="option.value"
                  :model-value="listHas(setting, option.value)"
                  :label="option.label"
                  @update:model-value="toggleList(setting, option.value)"
                />
              </div>

              <!-- A number, with its unit beside it. -->
              <div
                v-else-if="setting.kind === 'int' || setting.kind === 'float' || setting.kind === 'bytes'"
                class="flex items-center gap-2"
              >
                <AppInput
                  :id="setting.key"
                  v-model="draft[setting.key] as string"
                  type="number"
                  class="tabular-nums"
                  :min="setting.min || undefined"
                  :max="setting.max || undefined"
                  :step="setting.step || (setting.kind === 'int' ? 1 : 'any')"
                />
                <span v-if="setting.unit" class="shrink-0 text-xs text-ink-faint">{{ setting.unit }}</span>
              </div>

              <!-- Everything else, including a list with no fixed members. -->
              <AppInput v-else :id="setting.key" v-model="draft[setting.key] as string" />
            </div>
          </li>
        </ul>
      </section>

      <p v-if="groups.length === 0" class="rounded border border-dashed border-line p-10 text-center text-sm text-ink-muted">
        没有匹配的设置。
      </p>

      <p v-if="!showAdvanced && advancedCount > 0" class="text-xs text-ink-faint">
        还有 {{ advancedCount }} 项高级设置没有显示。它们都能改，只是平时用不到。
      </p>

      <!-- ------------------------------------------------------------------ -->
      <!-- The whole configuration, read-only.                                 -->
      <!-- ------------------------------------------------------------------ -->
      <details class="rounded border border-line bg-surface-raised">
        <summary class="cursor-pointer px-4 py-2 text-sm text-ink-muted">
          全部配置（只读）—— 排查「改了怎么没生效」时看这里
        </summary>

        <div class="space-y-3 border-t border-line p-4">
          <p v-if="settings.data.value" class="space-y-1 text-xs text-ink-faint">
            <span class="block">数据目录 <code class="font-mono">{{ settings.data.value.data_dir }}</code></span>
            <span class="block">
              配置文件 <code class="font-mono">{{ settings.data.value.config_path }}</code>
              <span v-if="!settings.data.value.config_file_exists">
                —— 不存在，这一层没有设置任何东西（这是正常的）
              </span>
            </span>
          </p>

          <div class="flex flex-wrap items-center gap-3">
            <div class="min-w-64 flex-1">
              <AppInput
                v-model="tableFilter"
                type="search"
                mono
                placeholder="筛选，例如 asr 或 translation.model"
              />
            </div>
            <span class="text-xs text-ink-faint">{{ rows.length }} 项</span>
          </div>

          <div class="max-h-[60vh] overflow-auto rounded border border-line">
            <table class="w-full border-collapse text-sm">
              <thead class="sticky top-0 bg-surface-raised">
                <tr class="border-b border-line text-left text-xs text-ink-faint">
                  <th class="px-3 py-2 font-medium">键</th>
                  <th class="px-3 py-2 font-medium">值</th>
                  <th class="px-3 py-2 font-medium">来源</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="row in rows" :key="row.path" class="border-b border-line/40">
                  <td class="px-3 py-1.5 font-mono text-xs">{{ row.path }}</td>
                  <td class="px-3 py-1.5 font-mono text-xs">
                    <span v-if="isSecret(row.value)" class="text-ink-faint">已设置（不显示）</span>
                    <span v-else-if="row.value === ''" class="text-ink-faint">未设置</span>
                    <span v-else>{{ row.value }}</span>
                  </td>
                  <td class="px-3 py-1.5 text-xs text-ink-muted">
                    {{ SOURCE_LABEL[row.source] ?? row.source }}
                  </td>
                </tr>
              </tbody>
            </table>
          </div>

          <p class="text-xs text-ink-faint">
            优先级：默认值 → 配置文件 → 环境变量 → 网页 → 项目覆盖 → 命令行。越靠右越优先。
            翻译服务的地址与密钥在「翻译服务」页，那里不会把密钥发回浏览器。
          </p>
        </div>
      </details>
    </template>
  </div>
</template>
