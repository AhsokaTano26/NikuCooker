/**
 * Tests for the event stream store.
 *
 * These target the replay contract specifically, because its failure mode is
 * invisible: a client that silently misses events, or that advances its
 * sequence past an event it failed to apply, looks connected and shows stale
 * state indefinitely.
 */

import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { LOCAL_RESYNC, useEventStore, type EventSourceLike, type ServerEvent } from './events'

/** A stand-in for EventSource that records what was registered and lets a test
 *  deliver frames by hand. */
class FakeEventSource implements EventSourceLike {
  static instances: FakeEventSource[] = []

  readonly url: string
  closed = false
  onerror: ((event: Event) => void) | null = null
  private readonly listeners = new Map<string, Set<(event: MessageEvent<string>) => void>>()

  constructor(url: string) {
    this.url = url
    FakeEventSource.instances.push(this)
  }

  addEventListener(type: string, listener: (event: MessageEvent<string>) => void): void {
    let set = this.listeners.get(type)
    if (set === undefined) {
      set = new Set()
      this.listeners.set(type, set)
    }
    set.add(listener)
  }

  close(): void {
    this.closed = true
  }

  /** Delivers a frame to whoever registered for that type. */
  deliver(event: Partial<ServerEvent> & { type: string }): void {
    const payload = {
      ts: '2026-09-22T10:14:03.221Z',
      data: {},
      ...event,
    } as ServerEvent
    const message = { data: JSON.stringify(payload) } as MessageEvent<string>
    for (const listener of this.listeners.get(event.type) ?? []) {
      listener(message)
    }
  }

  /** Delivers a frame that is not valid JSON. */
  deliverRaw(raw: string): void {
    const message = { data: raw } as MessageEvent<string>
    for (const listener of this.listeners.get('hello') ?? []) {
      listener(message)
    }
  }

  fail(): void {
    this.onerror?.(new Event('error'))
  }
}

function latest(): FakeEventSource {
  const source = FakeEventSource.instances.at(-1)
  if (source === undefined) throw new Error('no EventSource was created')
  return source
}

describe('event store', () => {
  beforeEach(() => {
    FakeEventSource.instances = []
    vi.useFakeTimers()
    setActivePinia(createPinia())
  })

  function store() {
    const s = useEventStore()
    s.useFactory((url) => new FakeEventSource(url))
    return s
  }

  it('advances the sequence after applying an event', () => {
    const events = store()
    const seen: number[] = []
    events.on('stage.progress', (e) => seen.push(e.seq))

    events.connect()
    latest().deliver({ type: 'stage.progress', seq: 7 })

    expect(seen).toEqual([7])
    expect(events.lastSeq).toBe(7)
  })

  it('does not advance the sequence when a handler throws', () => {
    // The point: a reconnect then replays from the last event that was applied
    // cleanly, rather than skipping the one that broke.
    const events = store()
    events.on('stage.progress', () => {
      throw new Error('handler bug')
    })

    events.connect()
    latest().deliver({ type: 'stage.progress', seq: 3 })

    expect(events.lastSeq).toBe(0)
    expect(events.connection).toBe('resyncing')
  })

  it('resyncs when hello reports that the requested sequence could not be replayed', () => {
    const events = store()
    const resyncs: string[] = []
    events.on(LOCAL_RESYNC, (e) => resyncs.push(e.type))

    events.connect()
    latest().deliver({ type: 'stage.progress', seq: 4 })
    latest().deliver({ type: 'hello', seq: 5, data: { replay: false, current_seq: 5 } })

    expect(resyncs).toEqual([LOCAL_RESYNC])
    expect(events.connection).toBe('resyncing')
  })

  it('goes live on hello when nothing was lost', () => {
    const events = store()
    events.connect()
    latest().deliver({ type: 'hello', seq: 1, data: { replay: true, current_seq: 1 } })

    expect(events.connection).toBe('live')
  })

  it('does not resync on the first hello of a fresh session', () => {
    // lastSeq is 0, so nothing can have been missed. Treating this as a loss
    // would make every page load trigger a pointless refetch.
    const events = store()
    events.connect()
    latest().deliver({ type: 'hello', seq: 1, data: { replay: false, current_seq: 1 } })

    expect(events.connection).toBe('live')
    expect(events.resyncCount).toBe(0)
  })

  it('resyncs on an explicit resync.required', () => {
    const events = store()
    events.connect()
    latest().deliver({ type: 'resync.required', seq: 9, data: { reason: 'buffer_evicted' } })

    expect(events.connection).toBe('resyncing')
    expect(events.lastError).toContain('buffer_evicted')
  })

  it('resumes from lastSeq + 1', () => {
    const events = store()
    events.connect()
    latest().deliver({ type: 'hello', seq: 1, data: { replay: true } })
    latest().deliver({ type: 'stage.progress', seq: 2 })

    // Simulate a drop and let the backoff timer fire.
    latest().fail()
    vi.advanceTimersByTime(1_000)

    expect(latest().url).toContain('since=3')
  })

  it('backs off exponentially and caps the delay', () => {
    const events = store()
    events.connect()

    const delays: number[] = []
    for (let attempt = 0; attempt < 8; attempt += 1) {
      const source = latest()
      const before = FakeEventSource.instances.length
      source.fail()
      // Advance in small steps so each reconnect is observed individually.
      let elapsed = 0
      while (FakeEventSource.instances.length === before && elapsed < 60_000) {
        vi.advanceTimersByTime(100)
        elapsed += 100
      }
      delays.push(elapsed)
    }

    expect(delays[0]).toBeLessThan(delays[1]!)
    expect(Math.max(...delays)).toBeLessThanOrEqual(30_000)
  })

  it('closes the previous source before opening a new one', () => {
    const events = store()
    events.connect()
    const first = latest()

    events.connect()

    expect(first.closed).toBe(true)
    expect(latest()).not.toBe(first)
  })

  it('stops reconnecting after disconnect', () => {
    const events = store()
    events.connect()
    latest().fail()

    events.disconnect()
    vi.advanceTimersByTime(60_000)

    expect(events.connection).toBe('idle')
  })

  it('delivers to wildcard subscribers', () => {
    const events = store()
    const types: string[] = []
    events.on('*', (e) => types.push(e.type))

    events.connect()
    latest().deliver({ type: 'job.status', seq: 1 })
    latest().deliver({ type: 'qc.updated', seq: 2 })

    expect(types).toEqual(['job.status', 'qc.updated'])
  })

  it('unsubscribes via the returned function', () => {
    const events = store()
    let calls = 0
    const off = events.on('job.status', () => {
      calls += 1
    })

    events.connect()
    latest().deliver({ type: 'job.status', seq: 1 })
    off()
    latest().deliver({ type: 'job.status', seq: 2 })

    expect(calls).toBe(1)
  })

  it('resyncs on an unparseable frame', () => {
    const events = store()
    events.connect()
    latest().deliverRaw('this is not json')

    expect(events.connection).toBe('resyncing')
  })

  // Once the server has been asked to stop, the stream failing is the expected
  // outcome rather than a fault to recover from. Reconnecting would spend the
  // whole backoff schedule against a port with nothing behind it, and the
  // interface would report "重连中" for a server the user deliberately turned off.
  it('stops reconnecting once the server has been shut down', () => {
    const events = store()
    events.connect()

    const before = FakeEventSource.instances.length
    events.markStopped()

    // The stream dying is what actually happens next, and it must not schedule
    // anything.
    latest().onerror?.(new Event('error'))
    vi.advanceTimersByTime(60_000)

    expect(events.connection).toBe('stopped')
    expect(events.isShuttingDown).toBe(true)
    expect(FakeEventSource.instances.length).toBe(before)
  })

  // The other half: an ordinary failure still reconnects. Without this the test
  // above would pass just as well against a store that never reconnects at all.
  it('still reconnects after an ordinary stream failure', () => {
    const events = store()
    events.connect()

    const before = FakeEventSource.instances.length
    latest().onerror?.(new Event('error'))
    vi.advanceTimersByTime(5_000)

    expect(FakeEventSource.instances.length).toBeGreaterThan(before)
  })
})
