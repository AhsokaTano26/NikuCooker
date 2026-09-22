<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'

import { ApiError, api, type Provider, type ProviderInput } from '@/api/client'
import { useAsync } from '@/composables/useAsync'
import { useEventStore } from '@/stores/events'

const events = useEventStore()
const providers = useAsync(() => api.providers.list())

const actionError = ref<string | null>(null)
const notice = ref<string | null>(null)
const busy = ref<string | null>(null)

/** The form, which doubles as the editor for an existing provider. */
const editingID = ref<string | null>(null)
const form = reactive<ProviderInput>({
  name: '',
  kind: 'llm',
  type: 'openai-compatible',
  base_url: '',
  api_key: '',
  model: '',
  enabled: true,
})

/** Whether the key field should be sent. Empty means "leave the stored one". */
const replacingKey = ref(false)

const isEditing = computed(() => editingID.value !== null)

const unsubscribers: (() => void)[] = []

onMounted(async () => {
  await providers.run()

  // A provider added or changed in another tab, or from the CLI, means this
  // list is stale. The event carries no project scope, so there is nothing to
  // filter on.
  unsubscribers.push(events.on('settings.changed', () => void providers.run()))
})

onBeforeUnmount(() => {
  for (const unsubscribe of unsubscribers) unsubscribe()
})

function reset(): void {
  editingID.value = null
  replacingKey.value = false
  Object.assign(form, {
    name: '', kind: 'llm', type: 'openai-compatible',
    base_url: '', api_key: '', model: '', enabled: true,
  })
}

function edit(provider: Provider): void {
  editingID.value = provider.id
  replacingKey.value = false
  Object.assign(form, {
    name: provider.name,
    kind: provider.kind,
    type: provider.type,
    base_url: provider.base_url ?? '',
    api_key: '',
    model: provider.model ?? '',
    enabled: provider.enabled,
  })
}

async function submit(): Promise<void> {
  actionError.value = null
  notice.value = null

  // An empty key on an existing provider means "keep the stored one", so the
  // field is omitted rather than sent blank — which the server would read as a
  // deliberate clear.
  const body: ProviderInput = { ...form }
  if (isEditing.value && !replacingKey.value) delete body.api_key

  try {
    if (editingID.value) {
      await api.providers.update(editingID.value, body)
      notice.value = `已保存「${body.name}」`
    } else {
      await api.providers.create(body)
      notice.value = `已添加「${body.name}」`
    }
    reset()
    await providers.run()
  } catch (cause) {
    actionError.value = cause instanceof ApiError ? cause.message : String(cause)
  }
}

async function toggle(provider: Provider): Promise<void> {
  actionError.value = null
  try {
    await api.providers.update(provider.id, { enabled: !provider.enabled })
    await providers.run()
  } catch (cause) {
    actionError.value = cause instanceof ApiError ? cause.message : String(cause)
  }
}

async function test(provider: Provider): Promise<void> {
  busy.value = provider.id
  actionError.value = null
  notice.value = null
  try {
    const result = await api.providers.test(provider.id)
    if (result.ok) {
      notice.value = `「${provider.name}」可用${result.model ? ` · ${result.model}` : ''}`
    } else {
      // Reported as a notice rather than an error: the request succeeded and
      // its answer is that the provider is misconfigured, which is a result.
      actionError.value = `「${provider.name}」测试失败：${result.message}`
    }
  } catch (cause) {
    actionError.value = cause instanceof ApiError ? cause.message : String(cause)
  } finally {
    busy.value = null
  }
}

async function remove(provider: Provider): Promise<void> {
  if (!confirm(`删除翻译服务「${provider.name}」？`)) return
  actionError.value = null
  try {
    await api.providers.remove(provider.id)
    if (editingID.value === provider.id) reset()
    await providers.run()
  } catch (cause) {
    actionError.value = cause instanceof ApiError ? cause.message : String(cause)
  }
}

const items = computed(() => providers.data.value?.items ?? [])
const canSubmit = computed(() => form.name.trim() !== '' && form.base_url.trim() !== '')
</script>

<template>
  <div class="grid gap-6 lg:grid-cols-[1fr_22rem]">
    <section class="space-y-4">
      <p v-if="notice" class="rounded border border-status-done/40 bg-surface-raised p-3 text-sm text-status-done">
        {{ notice }}
      </p>
      <p v-if="actionError" class="rounded border border-status-failed/40 bg-surface-raised p-3 text-sm text-status-failed">
        {{ actionError }}
      </p>

      <p v-if="providers.error.value" class="rounded border border-status-failed/40 bg-surface-raised p-3 text-sm text-status-failed">
        {{ providers.error.value }}
        <button class="ml-2 text-accent hover:underline" @click="providers.run">重试</button>
      </p>

      <div v-else-if="items.length === 0" class="rounded border border-dashed border-line p-10 text-center text-sm text-ink-muted">
        还没有配置翻译服务。翻译需要一个 OpenAI 兼容的接口。
      </div>

      <ul v-else class="divide-y divide-line/60 rounded border border-line">
        <li v-for="provider in items" :key="provider.id" class="px-4 py-3">
          <div class="flex items-start gap-4">
            <div class="min-w-0 flex-1">
              <div class="flex flex-wrap items-baseline gap-2">
                <span class="text-sm">{{ provider.name }}</span>
                <span
                  class="rounded-full border px-2 py-0.5 text-xs"
                  :class="provider.enabled
                    ? 'border-status-done/40 text-status-done'
                    : 'border-line text-ink-faint'"
                >
                  {{ provider.enabled ? '启用' : '停用' }}
                </span>
                <!-- Whether a key is stored, without showing it. A blank field
                     would read as "not configured" for a provider that is. -->
                <span v-if="provider.has_key" class="text-xs text-ink-faint">密钥已配置</span>
              </div>
              <p class="mt-1 truncate font-mono text-xs text-ink-faint" :title="provider.base_url">
                {{ provider.base_url }} · {{ provider.model }}
              </p>

              <div class="mt-1.5 flex flex-wrap gap-3 text-xs">
                <button class="text-ink-faint hover:text-accent" @click="edit(provider)">编辑</button>
                <button class="text-ink-faint hover:text-accent" @click="toggle(provider)">
                  {{ provider.enabled ? '停用' : '启用' }}
                </button>
                <button
                  :disabled="busy === provider.id"
                  class="text-ink-faint hover:text-accent disabled:opacity-40"
                  @click="test(provider)"
                >
                  {{ busy === provider.id ? '测试中…' : '测试' }}
                </button>
                <button class="text-ink-faint hover:text-status-failed" @click="remove(provider)">删除</button>
              </div>
            </div>
          </div>
        </li>
      </ul>
    </section>

    <section class="rounded border border-line bg-surface-raised p-4">
      <h2 class="text-sm font-medium text-ink-muted">
        {{ isEditing ? '编辑翻译服务' : '添加翻译服务' }}
      </h2>

      <form class="mt-3 space-y-3" @submit.prevent="submit">
        <div>
          <label for="p-name" class="block text-xs text-ink-muted">名称</label>
          <input
            id="p-name"
            v-model="form.name"
            type="text"
            placeholder="openai"
            class="mt-1 w-full rounded border border-line bg-surface px-2 py-1.5 text-sm outline-none focus:border-accent"
          />
        </div>

        <div>
          <label for="p-url" class="block text-xs text-ink-muted">接口地址</label>
          <input
            id="p-url"
            v-model="form.base_url"
            type="text"
            placeholder="https://api.example.com/v1"
            class="mt-1 w-full rounded border border-line bg-surface px-2 py-1.5 font-mono text-xs outline-none focus:border-accent"
          />
          <p class="mt-1 text-xs text-ink-faint">
            不带 <code class="font-mono">/chat/completions</code> 也可以，会自动补全。
          </p>
        </div>

        <div>
          <label for="p-model" class="block text-xs text-ink-muted">模型</label>
          <input
            id="p-model"
            v-model="form.model"
            type="text"
            placeholder="gpt-4o-mini"
            class="mt-1 w-full rounded border border-line bg-surface px-2 py-1.5 font-mono text-xs outline-none focus:border-accent"
          />
        </div>

        <div>
          <label for="p-key" class="block text-xs text-ink-muted">API 密钥</label>
          <input
            id="p-key"
            v-model="form.api_key"
            type="password"
            :disabled="isEditing && !replacingKey"
            :placeholder="isEditing ? '留空则保持原密钥' : 'sk-...'"
            class="mt-1 w-full rounded border border-line bg-surface px-2 py-1.5 font-mono text-xs outline-none focus:border-accent disabled:opacity-50"
          />
          <!-- The key never leaves the server, so it cannot be shown back. The
               checkbox is what makes "replace it" an explicit act rather than
               something that happens by typing in a field. -->
          <label v-if="isEditing" class="mt-1 flex items-center gap-2 text-xs text-ink-faint">
            <input v-model="replacingKey" type="checkbox" />
            更换密钥
          </label>
        </div>

        <div class="flex gap-2 pt-1">
          <button
            type="submit"
            :disabled="!canSubmit"
            class="rounded bg-accent px-3 py-1.5 text-sm font-medium text-accent-ink transition hover:opacity-90 disabled:opacity-40"
          >
            {{ isEditing ? '保存' : '添加' }}
          </button>
          <button
            v-if="isEditing"
            type="button"
            class="rounded border border-line px-3 py-1.5 text-sm text-ink-muted transition hover:text-ink"
            @click="reset"
          >
            取消
          </button>
        </div>
      </form>
    </section>
  </div>
</template>
