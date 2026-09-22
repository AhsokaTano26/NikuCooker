<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'

import { ApiError, api, type Provider, type ProviderInput } from '@/api/client'
import AppButton from '@/components/AppButton.vue'
import AppCheckbox from '@/components/AppCheckbox.vue'
import AppField from '@/components/AppField.vue'
import AppInput from '@/components/AppInput.vue'
import { useAsync } from '@/composables/useAsync'
import { useEventStore } from '@/stores/events'

const events = useEventStore()
const providers = useAsync(() => api.providers.list())

const actionError = ref<string | null>(null)
const notice = ref<string | null>(null)

/**
 * Something true about the provider that is not a failure.
 *
 * Kept apart from the success notice so it can be styled as a caution: a
 * working provider whose reply used the whole check budget is not broken, and
 * showing it in the failure colour would send the user to fix it.
 */
const caution = ref<string | null>(null)
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
      <p v-if="caution" class="rounded border border-status-warn/40 bg-surface-raised p-3 text-xs text-status-warn">
        {{ caution }}
      </p>
      <p v-if="actionError" class="rounded border border-status-failed/40 bg-surface-raised p-3 text-sm text-status-failed">
        {{ actionError }}
      </p>

      <p v-if="providers.error.value" class="rounded border border-status-failed/40 bg-surface-raised p-3 text-sm text-status-failed">
        {{ providers.error.value }}
        <AppButton variant="ghost" size="sm" class="ml-2" @click="providers.run">重试</AppButton>
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
                <AppButton variant="ghost" size="sm" @click="edit(provider)">编辑</AppButton>
                <AppButton variant="ghost" size="sm" @click="toggle(provider)">
                  {{ provider.enabled ? '停用' : '启用' }}
                </AppButton>
                <AppButton
                  variant="ghost"
                  size="sm"
                  :disabled="busy === provider.id"
                  @click="test(provider)"
                >
                  {{ busy === provider.id ? '测试中…' : '测试' }}
                </AppButton>
                <AppButton variant="danger" size="sm" @click="remove(provider)">删除</AppButton>
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
        <AppField label="名称" for-id="p-name">
          <AppInput id="p-name" v-model="form.name" placeholder="openai" />
        </AppField>

        <AppField
          label="接口地址"
          for-id="p-url"
          help="不带 /chat/completions 也可以，会自动补全。"
        >
          <AppInput
            id="p-url"
            v-model="form.base_url"
            mono
            placeholder="https://api.example.com/v1"
          />
        </AppField>

        <AppField label="模型" for-id="p-model">
          <AppInput id="p-model" v-model="form.model" mono placeholder="gpt-4o-mini" />
        </AppField>

        <AppField label="API 密钥" for-id="p-key">
          <AppInput
            id="p-key"
            v-model="form.api_key"
            type="password"
            mono
            :disabled="isEditing && !replacingKey"
            :placeholder="isEditing ? '留空则保持原密钥' : 'sk-...'"
          />
          <!-- The key never leaves the server, so it cannot be shown back. The
               checkbox is what makes "replace it" an explicit act rather than
               something that happens by typing in a field. -->
          <div v-if="isEditing" class="mt-1.5">
            <AppCheckbox v-model="replacingKey" label="更换密钥" />
          </div>
        </AppField>

        <div class="flex gap-2 pt-1">
          <AppButton type="submit" variant="primary" :disabled="!canSubmit">
            {{ isEditing ? '保存' : '添加' }}
          </AppButton>
          <AppButton v-if="isEditing" @click="reset">取消</AppButton>
        </div>
      </form>
    </section>
  </div>
</template>
