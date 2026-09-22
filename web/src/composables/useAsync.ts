import { ref, shallowRef, type Ref } from 'vue'

import { ApiError } from '@/api/client'

/**
 * State for one asynchronous load.
 *
 * Every view needs the same four things: the data, whether it is loading,
 * whether it failed, and a way to try again. Writing them out per view is how
 * one of them ends up without an error path, and the view that is missing it is
 * the one that shows a blank screen when the server is down.
 */
export interface AsyncState<T> {
  data: Ref<T | null>
  error: Ref<string | null>
  /** The error code, for callers that branch on it rather than on the prose. */
  errorCode: Ref<string | null>
  loading: Ref<boolean>
  run: () => Promise<void>
}

/**
 * Wraps a loader in loading and error state.
 *
 * The Error is preserved as a code rather than flattened into a message,
 * because the UI's remedies differ: a NOT_FOUND project should navigate away, a
 * NETWORK_UNREACHABLE should offer a retry. Deciding that from prose would mean
 * matching on text that is free to change.
 */
export function useAsync<T>(loader: () => Promise<T>): AsyncState<T> {
  const data = shallowRef<T | null>(null)
  const error = ref<string | null>(null)
  const errorCode = ref<string | null>(null)
  const loading = ref(false)

  async function run(): Promise<void> {
    loading.value = true
    error.value = null
    errorCode.value = null

    try {
      data.value = await loader()
    } catch (cause) {
      if (cause instanceof ApiError) {
        error.value = cause.message
        errorCode.value = cause.code
      } else {
        // Not an API error means something threw before the request — a bug in
        // the view. Reported with its own words rather than hidden behind a
        // generic message, because it is the message nobody can reproduce.
        error.value = cause instanceof Error ? cause.message : String(cause)
        errorCode.value = 'UNEXPECTED'
      }
    } finally {
      loading.value = false
    }
  }

  return { data, error, errorCode, loading, run }
}

/** Renders a duration in seconds as a compact string. */
export function formatDuration(seconds: number | null | undefined): string {
  if (seconds === null || seconds === undefined || seconds <= 0) return '—'

  const total = Math.round(seconds)
  const hours = Math.floor(total / 3600)
  const minutes = Math.floor((total % 3600) / 60)
  const secs = total % 60

  if (hours > 0) {
    return `${hours}:${String(minutes).padStart(2, '0')}:${String(secs).padStart(2, '0')}`
  }
  return `${minutes}:${String(secs).padStart(2, '0')}`
}

/** Renders a timestamp as a local date and time. */
export function formatTime(value: string | null | undefined): string {
  if (!value) return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return '—'
  return date.toLocaleString()
}

/** Renders a byte count for a human. */
export function formatBytes(bytes: number | null | undefined): string {
  if (bytes === null || bytes === undefined) return '—'
  if (bytes < 1024) return `${bytes} B`

  let value = bytes
  for (const unit of ['KB', 'MB', 'GB', 'TB']) {
    value /= 1024
    if (value < 1024) return `${value.toFixed(1)} ${unit}`
  }
  return `${(value / 1024).toFixed(1)} PB`
}
