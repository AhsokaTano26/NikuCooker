<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'

import { api } from '@/api/client'
import { useAsync } from '@/composables/useAsync'

const settings = useAsync(() => api.settings.get())
const filter = ref('')

onMounted(settings.run)

/**
 * Every leaf of the configuration, flattened to dotted paths.
 *
 * Flattened rather than rendered as a nested tree because the question a user
 * arrives with is "where does `asr.model` come from", and a dotted list answers
 * it in one scan.
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

    // A key whose layer is unrecorded was never set by anything, so it is
    // showing its default. Saying so is more useful than a blank column.
    out.push({
      path: prefix,
      value: Array.isArray(value) ? value.join(', ') : String(value ?? ''),
      source: provenance[prefix] ?? 'default',
    })
  }

  walk(config, '')

  const needle = filter.value.trim().toLowerCase()
  const filtered = needle === ''
    ? out
    : out.filter((row) => row.path.toLowerCase().includes(needle))

  return filtered.sort((a, b) => a.path.localeCompare(b.path))
})

const SOURCE_LABEL: Record<string, string> = {
  default: '默认',
  config_file: '配置文件',
  environment: '环境变量',
  project: '项目覆盖',
  cli: '命令行',
}

/** A value the server withheld. */
function isSecret(value: string): boolean {
  return value === '***'
}
</script>

<template>
  <div class="space-y-4">
    <p class="text-sm text-ink-muted">
      配置按 <code class="rounded bg-surface-sunken px-1.5 py-0.5 font-mono text-xs">默认 → 配置文件 → 环境变量 → 项目覆盖 → 命令行</code>
      依次覆盖。下表列出每一项的最终取值，以及是谁设的——「改了设置却没生效」通常就是被更高一层的值盖住了。
    </p>

    <p v-if="settings.error.value" class="rounded border border-status-failed/40 bg-surface-raised p-3 text-sm text-status-failed">
      {{ settings.error.value }}
      <button class="ml-2 text-accent hover:underline" @click="settings.run">重试</button>
    </p>

    <template v-else>
      <div class="flex flex-wrap items-center gap-3">
        <input
          v-model="filter"
          type="search"
          placeholder="筛选，例如 asr 或 translation.model"
          class="min-w-64 flex-1 rounded border border-line bg-surface-raised px-3 py-1.5 font-mono text-sm outline-none placeholder:text-ink-faint focus:border-accent"
        />
        <span class="text-xs text-ink-faint">{{ rows.length }} 项</span>
      </div>

      <p v-if="settings.data.value" class="text-xs text-ink-faint">
        数据目录 <code class="font-mono">{{ settings.data.value.data_dir }}</code>
      </p>

      <div class="max-h-[65vh] overflow-y-auto rounded border border-line">
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

      <!-- Editing here would need a rule for precedence between three writable
           sources, which the configuration file already records key by key. -->
      <p class="text-xs text-ink-faint">
        此页为只读。配置写在配置文件或环境变量里，改动后重启生效。
      </p>
    </template>
  </div>
</template>
