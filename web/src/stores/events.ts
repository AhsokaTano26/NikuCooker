/**
 * The single server-sent-events stream, and the only source of live state.
 *
 * One stream per tab, held here, with components subscribing in memory. That is
 * what keeps the design inside the browser's six-connection-per-origin limit
 * under HTTP/1.1, and it means there is exactly one place that knows about
 * reconnection and replay.
 *
 * See docs/events.md for the protocol this implements.
 */

import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

import { API_BASE } from '@/api/client'

export interface ServerEvent {
  /** Monotonic per server process. The replay contract depends on it. */
  seq: number
  type: string
  ts: string
  project_id?: string
  job_id?: string
  stage?: string
  data: Record<string, unknown>
}

/**
 * The subset of EventSource this store uses.
 *
 * Narrowed to an interface so tests can drive the store without a real
 * connection, and so the store never depends on browser-only globals at import
 * time.
 */
export interface EventSourceLike {
  addEventListener(type: string, listener: (event: MessageEvent<string>) => void): void
  close(): void
  onerror?: ((event: Event) => void) | null
}

export type EventSourceFactory = (url: string) => EventSourceLike

export type ConnectionState = 'idle' | 'connecting' | 'live' | 'reconnecting' | 'resyncing'

export type EventListener = (event: ServerEvent) => void

/**
 * Every event type the server emits, plus two synthetic ones this store raises
 * locally. Registering listeners explicitly rather than using `onmessage` is
 * what lets the server mirror the type into both the `event:` frame and the
 * envelope.
 */
export const EVENT_TYPES = [
  'hello',
  'resync.required',
  'job.status',
  'job.progress',
  'stage.status',
  'stage.progress',
  'project.updated',
  'project.deleted',
  'segment.updated',
  'segments.replaced',
  'qc.updated',
  'model.download.progress',
  'model.status',
  'worker.status',
  'system.stats',
  'log',
  'log.dropped',
] as const

/** Raised locally, never received. `seq` is the last server sequence seen. */
export const LOCAL_RESYNC = 'resync'
export const LOCAL_CONNECTION = 'connection'

const BACKOFF_START_MS = 1_000
const BACKOFF_MAX_MS = 30_000

export const useEventStore = defineStore('events', () => {
  const connection = ref<ConnectionState>('idle')
  const lastSeq = ref(0)
  const lastError = ref<string | null>(null)
  /** Increments on every resync. Views watch it to know they must refetch. */
  const resyncCount = ref(0)

  const isLive = computed(() => connection.value === 'live')

  const listeners = new Map<string, Set<EventListener>>()

  let source: EventSourceLike | null = null
  let reconnectTimer: ReturnType<typeof setTimeout> | null = null
  let backoffMs = BACKOFF_START_MS
  let currentProjectId: string | undefined
  let factory: EventSourceFactory = defaultFactory

  function defaultFactory(url: string): EventSourceLike {
    if (typeof EventSource === 'undefined') {
      throw new Error('EventSource is unavailable in this environment')
    }
    return new EventSource(url)
  }

  /** Replaces the transport. For tests; production never calls this. */
  function useFactory(next: EventSourceFactory): void {
    factory = next
  }

  function on(type: string, listener: EventListener): () => void {
    let set = listeners.get(type)
    if (set === undefined) {
      set = new Set()
      listeners.set(type, set)
    }
    set.add(listener)

    return () => {
      set.delete(listener)
    }
  }

  function dispatch(event: ServerEvent): void {
    for (const listener of listeners.get(event.type) ?? []) {
      listener(event)
    }
    for (const listener of listeners.get('*') ?? []) {
      listener(event)
    }
  }

  /** Raises a locally-generated event. Never advances `lastSeq`. */
  function dispatchLocal(type: string): void {
    dispatch({ seq: lastSeq.value, type, ts: new Date().toISOString(), data: {} })
  }

  function buildUrl(): string {
    const origin = globalThis.location?.origin ?? 'http://localhost'
    const url = new URL(`${API_BASE}/events`, origin)

    if (currentProjectId !== undefined) {
      url.searchParams.set('project_id', currentProjectId)
    }
    // Resume from the last event actually applied, not the last received.
    if (lastSeq.value > 0) {
      url.searchParams.set('since', String(lastSeq.value + 1))
    }
    return url.toString()
  }

  function handleMessage(type: string, message: MessageEvent<string>): void {
    let event: ServerEvent
    try {
      event = JSON.parse(message.data) as ServerEvent
    } catch {
      // A frame we cannot parse means the stream is not what we think it is.
      // Refetching is the only honest recovery.
      triggerResync('unparseable event frame')
      return
    }

    try {
      dispatch(event)
    } catch (cause) {
      // The sequence is deliberately not advanced: a reconnect then replays
      // from the last event that was applied cleanly, rather than skipping the
      // one that broke and carrying on with a hole in the state.
      lastError.value = `处理事件 ${event.type} 时出错：${String(cause)}`
      triggerResync('event handler failed')
      return
    }

    // Captured before this event's sequence is applied. Comparing against the
    // *post-assignment* value would make every first connection look like a
    // lost stream, since the hello's own sequence is greater than zero.
    const previousSeq = lastSeq.value
    lastSeq.value = event.seq

    if (type === 'hello') {
      // hello is emitted before any replayed events, so it must not clear a
      // pending resync decision.
      const replay = event.data.replay === true
      if (!replay && previousSeq > 0) {
        // The requested `since` was outside the server's buffer, or the server
        // restarted and every number we hold is from the previous process.
        triggerResync('server could not replay from the requested sequence')
        return
      }
      connection.value = 'live'
      backoffMs = BACKOFF_START_MS
      lastError.value = null
      dispatchLocal(LOCAL_CONNECTION)
    } else if (type === 'resync.required') {
      triggerResync(String(event.data.reason ?? 'server requested a resync'))
    }
  }

  function triggerResync(reason: string): void {
    connection.value = 'resyncing'
    lastError.value = reason
    resyncCount.value += 1
    dispatchLocal(LOCAL_RESYNC)
    scheduleReconnect()
  }

  function scheduleReconnect(): void {
    if (reconnectTimer !== null) return

    if (connection.value !== 'resyncing') {
      connection.value = 'reconnecting'
      dispatchLocal(LOCAL_CONNECTION)
    }

    const delay = backoffMs
    backoffMs = Math.min(backoffMs * 2, BACKOFF_MAX_MS)

    reconnectTimer = setTimeout(() => {
      reconnectTimer = null
      open()
    }, delay)
  }

  function open(): void {
    detach()
    if (connection.value !== 'resyncing') {
      connection.value = lastSeq.value > 0 ? 'reconnecting' : 'connecting'
    }

    let next: EventSourceLike
    try {
      next = factory(buildUrl())
    } catch (cause) {
      lastError.value = String(cause)
      scheduleReconnect()
      return
    }
    source = next

    for (const type of EVENT_TYPES) {
      next.addEventListener(type, (message) => handleMessage(type, message))
    }

    next.onerror = () => {
      // EventSource reconnects on its own; we do it instead so that the backoff
      // is ours and the connection state is truthful. Without a truthful state,
      // "the stream is down" and "the pipeline is paused" look identical.
      scheduleReconnect()
    }
  }

  function detach(): void {
    if (source !== null) {
      source.close()
      source = null
    }
  }

  function connect(projectId?: string): void {
    currentProjectId = projectId
    open()
  }

  function disconnect(): void {
    if (reconnectTimer !== null) {
      clearTimeout(reconnectTimer)
      reconnectTimer = null
    }
    detach()
    connection.value = 'idle'
    dispatchLocal(LOCAL_CONNECTION)
  }

  /** Forces a resync. Exposed so a view can recover from a state it knows is stale. */
  function requestResync(reason = 'requested by the application'): void {
    triggerResync(reason)
  }

  /** Test seam: forget everything, including the sequence position. */
  function reset(): void {
    disconnect()
    lastSeq.value = 0
    lastError.value = null
    resyncCount.value = 0
    backoffMs = BACKOFF_START_MS
  }

  return {
    connection,
    lastSeq,
    lastError,
    resyncCount,
    isLive,
    connect,
    disconnect,
    requestResync,
    on,
    useFactory,
    reset,
  }
})
