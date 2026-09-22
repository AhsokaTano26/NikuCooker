<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRouter } from 'vue-router'

import { ApiError, api } from '@/api/client'
import type { TranslationStyle } from '@/types/api'

const router = useRouter()

const name = ref('')
const sourcePath = ref('')
const sourceLanguage = ref('ja')
const targetLanguage = ref('zh-Hans')
const style = ref<TranslationStyle>('fansub')

const submitting = ref(false)
const error = ref<string | null>(null)
const errorCode = ref<string | null>(null)

const STYLES: { value: TranslationStyle; label: string; hint: string }[] = [
  { value: 'literal', label: '直译', hint: '贴近日语结构与敬语，适合需要对照原文的场合。' },
  { value: 'natural', label: '意译', hint: '按中文语序重写，读起来像中文写的。' },
  { value: 'fansub', label: '字幕组', hint: '保留敬称与圈内惯用语，简洁优先。' },
]

const LANGUAGES = [
  { value: 'ja', label: '日语' },
  { value: 'zh-Hans', label: '简体中文' },
  { value: 'zh-Hant', label: '繁体中文' },
  { value: 'en', label: '英语' },
  { value: 'ko', label: '韩语' },
]

const canSubmit = computed(
  () => name.value.trim() !== '' && sourcePath.value.trim() !== '' && !submitting.value,
)

async function submit(): Promise<void> {
  if (!canSubmit.value) return

  submitting.value = true
  error.value = null
  errorCode.value = null

  try {
    const created = await api.projects.create({
      name: name.value.trim(),
      source_language: sourceLanguage.value,
      target_language: targetLanguage.value,
      style: style.value,
      // The only source kind this build accepts. Uploading from the browser
      // arrives in a later phase, and the API says so rather than pretending to
      // accept it.
      source: { kind: 'path', path: sourcePath.value.trim() },
    })
    await router.push(`/projects/${created.id}`)
  } catch (cause) {
    if (cause instanceof ApiError) {
      error.value = cause.message
      errorCode.value = cause.code
    } else {
      error.value = cause instanceof Error ? cause.message : String(cause)
      errorCode.value = 'UNEXPECTED'
    }
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <form class="max-w-2xl space-y-5" @submit.prevent="submit">
    <div>
      <label for="name" class="block text-sm text-ink-muted">项目名称</label>
      <input
        id="name"
        v-model="name"
        type="text"
        placeholder="第12回 声優ラジオ"
        class="mt-1 w-full rounded border border-line bg-surface-raised px-3 py-2 text-sm outline-none placeholder:text-ink-faint focus:border-accent"
      />
    </div>

    <div>
      <label for="source" class="block text-sm text-ink-muted">视频文件路径</label>
      <input
        id="source"
        v-model="sourcePath"
        type="text"
        placeholder="/path/to/episode01.mkv"
        class="mt-1 w-full rounded border border-line bg-surface-raised px-3 py-2 font-mono text-sm outline-none placeholder:text-ink-faint focus:border-accent"
      />
      <!--
        The path is read by the server, not the browser, and it is off by
        default. Saying so here is better than a form that always fails with a
        403 the user has to go and look up.
      -->
      <p class="mt-1 text-xs text-ink-faint">
        这个路径由服务端读取，需要先在配置中打开 <code class="font-mono">server.allow_path_source</code>。
        文件会被复制进项目目录，之后移动或删除原文件都不影响项目。
      </p>
    </div>

    <div class="grid gap-4 sm:grid-cols-2">
      <div>
        <label for="source-language" class="block text-sm text-ink-muted">原语言</label>
        <select
          id="source-language"
          v-model="sourceLanguage"
          class="mt-1 w-full rounded border border-line bg-surface-raised px-3 py-2 text-sm outline-none focus:border-accent"
        >
          <option v-for="language in LANGUAGES" :key="language.value" :value="language.value">
            {{ language.label }}
          </option>
        </select>
      </div>

      <div>
        <label for="target-language" class="block text-sm text-ink-muted">目标语言</label>
        <select
          id="target-language"
          v-model="targetLanguage"
          class="mt-1 w-full rounded border border-line bg-surface-raised px-3 py-2 text-sm outline-none focus:border-accent"
        >
          <option v-for="language in LANGUAGES" :key="language.value" :value="language.value">
            {{ language.label }}
          </option>
        </select>
      </div>
    </div>

    <fieldset>
      <legend class="text-sm text-ink-muted">翻译风格</legend>
      <div class="mt-2 space-y-2">
        <label
          v-for="option in STYLES"
          :key="option.value"
          class="flex cursor-pointer gap-3 rounded border p-3 transition"
          :class="style === option.value ? 'border-accent bg-surface-raised' : 'border-line hover:border-ink-faint'"
        >
          <input v-model="style" type="radio" :value="option.value" class="mt-1" />
          <span>
            <span class="block text-sm">{{ option.label }}</span>
            <span class="mt-0.5 block text-xs text-ink-faint">{{ option.hint }}</span>
          </span>
        </label>
      </div>
    </fieldset>

    <p v-if="error" class="rounded border border-status-failed/40 bg-surface-raised p-3 text-sm text-status-failed">
      {{ error }}
      <span v-if="errorCode === 'INVALID_REQUEST'" class="mt-1 block text-xs text-ink-faint">
        如果提示路径来源被禁用，请在配置文件中设置 <code class="font-mono">server.allow_path_source: true</code> 后重启。
      </span>
    </p>

    <button
      type="submit"
      :disabled="!canSubmit"
      class="rounded bg-accent px-4 py-2 text-sm font-medium text-accent-ink transition hover:opacity-90 disabled:cursor-not-allowed disabled:opacity-40"
    >
      {{ submitting ? '创建中…' : '创建项目' }}
    </button>
  </form>
</template>
