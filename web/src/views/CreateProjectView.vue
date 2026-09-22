<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'

import { ApiError, api, type Upload, type UploadHandle } from '@/api/client'
import { useAsync, formatBytes } from '@/composables/useAsync'
import type { TranslationStyle } from '@/types/api'

const router = useRouter()

const name = ref('')
const sourcePath = ref('')
const sourceLanguage = ref('ja')
const targetLanguage = ref('zh-Hans')
const style = ref<TranslationStyle>('fansub')

/**
 * Which way the media arrives.
 *
 * Uploading is the default because it is the one that works everywhere,
 * including a server that has deliberately not been granted the right to read
 * the filesystem. The path option appears only when that right exists.
 */
const mode = ref<'upload' | 'path'>('upload')

const overview = useAsync(() => api.system.overview())
const features = computed(() => overview.data.value?.features ?? null)
const pathSourceAllowed = computed(() => features.value?.path_source === true)
const maxUploadBytes = computed(() => features.value?.max_upload_bytes ?? 0)

onMounted(() => {
  void overview.run()
})

// ---------------------------------------------------------------------------
// Upload
// ---------------------------------------------------------------------------

const file = ref<File | null>(null)
const uploaded = ref<Upload | null>(null)
const progress = ref(0)
const uploading = ref(false)
const uploadError = ref<string | null>(null)
const dragging = ref(false)
const fileInput = ref<HTMLInputElement | null>(null)

/** The in-flight request, so it can be aborted rather than waited out. */
let handle: UploadHandle | null = null

async function chooseFiles(files: FileList | null): Promise<void> {
  const picked = files?.[0]
  if (!picked) return
  await beginUpload(picked)
}

async function beginUpload(picked: File): Promise<void> {
  uploadError.value = null

  // Checked here so a file the server would refuse is refused before twenty
  // gigabytes cross the network. The server checks it again — this is a
  // courtesy, not the limit.
  const limit = maxUploadBytes.value
  if (limit > 0 && picked.size > limit) {
    uploadError.value = `「${picked.name}」有 ${formatBytes(picked.size)}，超过服务端上限 ${formatBytes(limit)}。`
    return
  }

  await discardCurrent()

  file.value = picked
  uploaded.value = null
  progress.value = 0
  uploading.value = true

  // The project is named after the video unless the user has already said
  // otherwise. Most media filenames are already what someone would have typed.
  if (name.value.trim() === '') {
    name.value = picked.name.replace(/\.[^.]+$/, '')
  }

  const request = api.uploads.create(picked, (fraction) => {
    progress.value = fraction
  })
  handle = request

  try {
    uploaded.value = await request.promise
  } catch (cause) {
    // A cancelled upload is not an error to report: the user asked for it.
    if (!(cause instanceof ApiError && cause.code === 'REQUEST_ABORTED')) {
      uploadError.value = cause instanceof Error ? cause.message : String(cause)
    }
    file.value = null
  } finally {
    uploading.value = false
    handle = null
  }
}

function cancelUpload(): void {
  handle?.abort()
}

/** Throws away whatever is currently staged, on the server as well as here. */
async function discardCurrent(): Promise<void> {
  const current = uploaded.value
  uploaded.value = null
  file.value = null
  progress.value = 0

  if (current) {
    // A failure here leaves a file that the sweep will collect. It is not worth
    // an error message: the user is replacing it, and the replacement is what
    // they care about.
    await api.uploads.discard(current.id).catch(() => {})
  }
}

function onDrop(event: DragEvent): void {
  dragging.value = false
  void chooseFiles(event.dataTransfer?.files ?? null)
}

function onFileInput(event: Event): void {
  const input = event.target as HTMLInputElement
  void chooseFiles(input.files)
  // Cleared so that re-picking the same file fires a change event, which is
  // what a user does after a fixable failure.
  input.value = ''
}

onBeforeUnmount(() => {
  // Only the transfer is cancelled. A completed upload is left on the server:
  // navigating away and coming back is a normal way to use this page, and
  // discarding the file would make the user send it again.
  handle?.abort()
})

// ---------------------------------------------------------------------------
// Submit
// ---------------------------------------------------------------------------

const submitting = ref(false)
const error = ref<string | null>(null)

const canSubmit = computed(() => {
  if (submitting.value) return false
  if (name.value.trim() === '') return false

  if (mode.value === 'upload') return uploaded.value !== null
  return sourcePath.value.trim() !== ''
})

async function submit(): Promise<void> {
  if (!canSubmit.value) return

  submitting.value = true
  error.value = null

  try {
    const staged = uploaded.value
    const created = await api.projects.create({
      name: name.value.trim(),
      source_language: sourceLanguage.value,
      target_language: targetLanguage.value,
      style: style.value,
      source:
        mode.value === 'upload' && staged
          ? { kind: 'upload', upload_id: staged.id }
          : { kind: 'path', path: sourcePath.value.trim() },
    })
    // The staged file has been moved into the project, so there is nothing left
    // to discard.
    uploaded.value = null
    await router.push(`/projects/${created.id}`)
  } catch (cause) {
    if (cause instanceof ApiError) {
      error.value = cause.message
      // The upload was consumed or expired between choosing it and submitting.
      // Sending the user back to the picker is more useful than leaving them
      // pressing a button that cannot succeed.
      if (cause.code === 'NOT_FOUND' && mode.value === 'upload') {
        uploaded.value = null
      }
    } else {
      error.value = cause instanceof Error ? cause.message : String(cause)
    }
  } finally {
    submitting.value = false
  }
}

// ---------------------------------------------------------------------------
// Options
// ---------------------------------------------------------------------------

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

/**
 * A hint for the file dialog, not a gate.
 *
 * The server decides what it accepts, and its list includes subtitle files that
 * this does not mention — "All files" is always still available, so being
 * incomplete here costs a click rather than a capability.
 */
const ACCEPT = 'video/*,audio/*,.mkv,.ts,.m2ts,.flv,.wmv'
</script>

<template>
  <form class="max-w-2xl space-y-5" @submit.prevent="submit">
    <!-- ------------------------------------------------------------------ -->
    <!-- Source                                                              -->
    <!-- ------------------------------------------------------------------ -->

    <fieldset>
      <div class="flex items-baseline justify-between">
        <legend class="text-sm text-ink-muted">源视频</legend>

        <!-- Only offered when the server permits it. Showing a choice that
             leads to a refusal is worse than not showing it. -->
        <div v-if="pathSourceAllowed" class="flex gap-1 text-xs">
          <button
            type="button"
            class="rounded px-2 py-0.5 transition"
            :class="mode === 'upload' ? 'bg-surface-raised text-ink' : 'text-ink-faint hover:text-ink-muted'"
            @click="mode = 'upload'"
          >
            上传文件
          </button>
          <button
            type="button"
            class="rounded px-2 py-0.5 transition"
            :class="mode === 'path' ? 'bg-surface-raised text-ink' : 'text-ink-faint hover:text-ink-muted'"
            @click="mode = 'path'"
          >
            服务端路径
          </button>
        </div>
      </div>

      <!-- Upload -->
      <div v-if="mode === 'upload'" class="mt-2">
        <input
          ref="fileInput"
          type="file"
          class="hidden"
          :accept="ACCEPT"
          @change="onFileInput"
        />

        <div
          v-if="!uploaded && !uploading"
          class="flex cursor-pointer flex-col items-center gap-2 rounded border border-dashed p-6 text-center transition"
          :class="dragging ? 'border-accent bg-surface-raised' : 'border-line hover:border-ink-faint'"
          @click="fileInput?.click()"
          @dragover.prevent="dragging = true"
          @dragleave.prevent="dragging = false"
          @drop.prevent="onDrop"
        >
          <span class="text-sm">把视频拖到这里，或点击选择</span>
          <span v-if="maxUploadBytes > 0" class="text-xs text-ink-faint">
            上限 {{ formatBytes(maxUploadBytes) }}。文件会被复制进项目目录。
          </span>
        </div>

        <div v-else-if="uploading" class="space-y-2 rounded border border-line p-4">
          <div class="flex items-center justify-between gap-3 text-sm">
            <span class="truncate">{{ file?.name }}</span>
            <button
              type="button"
              class="shrink-0 text-xs text-ink-faint transition hover:text-ink"
              @click="cancelUpload"
            >
              取消
            </button>
          </div>
          <div class="h-1.5 overflow-hidden rounded-full bg-surface-sunken">
            <div
              class="h-full rounded-full bg-accent transition-[width] duration-200"
              :style="{ width: `${Math.round(progress * 100)}%` }"
            />
          </div>
          <p class="text-xs text-ink-faint">
            上传中 {{ Math.round(progress * 100) }}% · {{ formatBytes(file?.size) }}
          </p>
        </div>

        <div v-else class="flex items-center justify-between gap-3 rounded border border-line p-4">
          <div class="min-w-0">
            <p class="truncate text-sm">{{ uploaded?.name }}</p>
            <p class="text-xs text-ink-faint">已上传 · {{ formatBytes(uploaded?.size_bytes) }}</p>
          </div>
          <button
            type="button"
            class="shrink-0 rounded border border-line px-2 py-1 text-xs text-ink-muted transition hover:border-ink-faint hover:text-ink"
            @click="fileInput?.click()"
          >
            换一个
          </button>
        </div>
      </div>

      <!-- Server-side path -->
      <div v-else class="mt-2">
        <input
          id="source"
          v-model="sourcePath"
          type="text"
          placeholder="/path/to/episode01.mkv"
          class="w-full rounded border border-line bg-surface-raised px-3 py-2 font-mono text-sm outline-none placeholder:text-ink-faint focus:border-accent"
        />
        <p class="mt-1 text-xs text-ink-faint">
          这个路径由服务端读取。文件会被复制进项目目录，之后移动或删除原文件都不影响项目。
        </p>
      </div>

      <p v-if="!pathSourceAllowed && features" class="mt-2 text-xs text-ink-faint">
        服务端路径来源未开启（<code class="font-mono">server.allow_path_source</code>），因此只能上传文件。
      </p>

      <p v-if="uploadError" class="mt-2 text-xs text-status-failed">{{ uploadError }}</p>
    </fieldset>

    <!-- ------------------------------------------------------------------ -->
    <!-- Project                                                             -->
    <!-- ------------------------------------------------------------------ -->

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

    <!--
      pre-line because the server's refusals are multi-line on purpose: they
      carry the configuration file's path and the exact lines to add, and
      reflowing that into one paragraph would hide where the file is.
    -->
    <p
      v-if="error"
      class="whitespace-pre-line rounded border border-status-failed/40 bg-surface-raised p-3 text-sm text-status-failed"
    >
      {{ error }}
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
