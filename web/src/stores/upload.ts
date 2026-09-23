import { defineStore } from 'pinia'
import { ref } from 'vue'

import { ApiError, api, type Upload, type UploadHandle } from '@/api/client'
import { formatBytes } from '@/composables/useAsync'

/**
 * A staged upload belongs to the application session, not to one route mount.
 * Leaving the creation page therefore no longer aborts a multi-gigabyte upload
 * or hides a completed staged file when the user comes back.
 */
export const useUploadStore = defineStore('upload', () => {
  const file = ref<File | null>(null)
  const uploaded = ref<Upload | null>(null)
  const progress = ref(0)
  const uploading = ref(false)
  const error = ref<string | null>(null)
  let handle: UploadHandle | null = null

  async function begin(picked: File, maxBytes: number): Promise<void> {
    error.value = null
    if (maxBytes > 0 && picked.size > maxBytes) {
      error.value = `「${picked.name}」有 ${formatBytes(picked.size)}，超过服务端上限 ${formatBytes(maxBytes)}。`
      return
    }

    await discard()
    file.value = picked
    uploaded.value = null
    progress.value = 0
    uploading.value = true

    const request = api.uploads.create(picked, (fraction) => {
      progress.value = fraction
    })
    handle = request
    try {
      uploaded.value = await request.promise
    } catch (cause) {
      if (!(cause instanceof ApiError && cause.code === 'REQUEST_ABORTED')) {
        error.value = cause instanceof Error ? cause.message : String(cause)
      }
      file.value = null
    } finally {
      uploading.value = false
      handle = null
    }
  }

  function cancel(): void {
    handle?.abort()
  }

  async function discard(): Promise<void> {
    const current = uploaded.value
    if (uploading.value) handle?.abort()
    uploaded.value = null
    file.value = null
    progress.value = 0
    if (current) await api.uploads.discard(current.id).catch(() => {})
  }

  function consume(): void {
    uploaded.value = null
    file.value = null
    progress.value = 0
  }

  return { file, uploaded, progress, uploading, error, begin, cancel, discard, consume }
})
