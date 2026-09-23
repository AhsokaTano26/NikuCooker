<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { storeToRefs } from 'pinia'
import { useRouter } from 'vue-router'

import { ApiError, api } from '@/api/client'
import AppButton from '@/components/AppButton.vue'
import AppInput from '@/components/AppInput.vue'
import AppRadio from '@/components/AppRadio.vue'
import AppSelect from '@/components/AppSelect.vue'
import { useAsync, formatBytes } from '@/composables/useAsync'
import type { TranslationStyle } from '@/types/api'
import { useUploadStore } from '@/stores/upload'

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

const uploadSession = useUploadStore()
const { file, uploaded, progress, uploading, error: uploadError } = storeToRefs(uploadSession)
if (uploaded.value && name.value.trim() === '') {
  name.value = uploaded.value.name.replace(/\.[^.]+$/, '')
}
const dragging = ref(false)
const fileInput = ref<HTMLInputElement | null>(null)

async function chooseFiles(files: FileList | null): Promise<void> {
  const picked = files?.[0]
  if (!picked) return
  await beginUpload(picked)
}

async function beginUpload(picked: File): Promise<void> {
  // The project is named after the video unless the user has already said
  // otherwise. Most media filenames are already what someone would have typed.
  if (name.value.trim() === '') {
    name.value = picked.name.replace(/\.[^.]+$/, '')
  }

  await uploadSession.begin(picked, maxUploadBytes.value)
}

function cancelUpload(): void {
  uploadSession.cancel()
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
    uploadSession.consume()
    await router.push(`/projects/${created.id}`)
  } catch (cause) {
    if (cause instanceof ApiError) {
      error.value = cause.message
      // The upload was consumed or expired between choosing it and submitting.
      // Sending the user back to the picker is more useful than leaving them
      // pressing a button that cannot succeed.
      if (cause.code === 'NOT_FOUND' && mode.value === 'upload') {
        uploadSession.consume()
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

/** The two ways media arrives, for the toggle above the source field. */
const SOURCE_MODES = [
  { value: 'upload' as const, label: '上传文件' },
  { value: 'path' as const, label: '服务端路径' },
]
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
          <AppButton
            v-for="option in SOURCE_MODES"
            :key="option.value"
            variant="ghost"
            size="sm"
            :class="mode === option.value ? 'bg-surface-raised text-ink' : ''"
            @click="mode = option.value"
          >
            {{ option.label }}
          </AppButton>
        </div>
      </div>

      <!-- Upload -->
      <div v-if="mode === 'upload'" class="mt-2">
        <input
          id="source-upload"
          ref="fileInput"
          type="file"
          class="hidden"
          :accept="ACCEPT"
          @change="onFileInput"
        />

        <button
          v-if="!uploaded && !uploading"
          type="button"
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
        </button>

        <div v-else-if="uploading" class="space-y-2 rounded border border-line p-4">
          <div class="flex items-center justify-between gap-3 text-sm">
            <span class="truncate">{{ file?.name }}</span>
            <AppButton variant="ghost" size="sm" class="shrink-0" @click="cancelUpload">取消</AppButton>
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
          <AppButton size="sm" class="shrink-0" @click="fileInput?.click()">换一个</AppButton>
        </div>
      </div>

      <!-- Server-side path -->
      <div v-else class="mt-2">
        <AppInput
          id="source"
          v-model="sourcePath"
          mono
          placeholder="/path/to/episode01.mkv"
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
      <div class="mt-1">
        <AppInput id="name" v-model="name" placeholder="第12回 声優ラジオ" />
      </div>
    </div>

    <div class="grid gap-4 sm:grid-cols-2">
      <div>
        <label for="source-language" class="block text-sm text-ink-muted">原语言</label>
        <div class="mt-1">
          <AppSelect id="source-language" v-model="sourceLanguage" :options="LANGUAGES" />
        </div>
      </div>

      <div>
        <label for="target-language" class="block text-sm text-ink-muted">目标语言</label>
        <div class="mt-1">
          <AppSelect id="target-language" v-model="targetLanguage" :options="LANGUAGES" />
        </div>
      </div>
    </div>

    <fieldset>
      <legend class="text-sm text-ink-muted">翻译风格</legend>
      <div class="mt-2 space-y-2">
        <AppRadio
          v-for="option in STYLES"
          :key="option.value"
          v-model="style"
          name="translation-style"
          :value="option.value"
          :label="option.label"
          :hint="option.hint"
        />
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

    <AppButton type="submit" variant="primary" :disabled="!canSubmit">
      {{ submitting ? '创建中…' : '创建项目' }}
    </AppButton>
  </form>
</template>
