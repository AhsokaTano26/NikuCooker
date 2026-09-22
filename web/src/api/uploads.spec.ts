/**
 * Tests for the upload client.
 *
 * It is the one request in this application that does not go through the shared
 * `request` helper, because only XMLHttpRequest reports upload progress. That
 * exception is the reason to test it: everything the helper used to guarantee —
 * the field name the server requires, the error envelope, cancellation — has to
 * be re-established by hand here, and a mistake in any of them is invisible
 * until a user is staring at a stalled progress bar.
 */

import { afterEach, describe, expect, it, vi } from 'vitest'

import { ApiError, uploadFile } from './client'

/** A stand-in for XMLHttpRequest that a test drives by hand. */
class FakeXHR {
  static instances: FakeXHR[] = []

  method = ''
  url = ''
  body: FormData | null = null
  aborted = false

  readonly upload = { addEventListener: (_type: string, listener: (event: unknown) => void) => {
    this.progressListener = listener
  } }

  private progressListener: ((event: unknown) => void) | null = null
  private readonly listeners = new Map<string, (event?: unknown) => void>()

  status = 0
  responseText = ''

  constructor() {
    FakeXHR.instances.push(this)
  }

  open(method: string, url: string): void {
    this.method = method
    this.url = url
  }

  addEventListener(type: string, listener: (event?: unknown) => void): void {
    this.listeners.set(type, listener)
  }

  send(body: FormData): void {
    this.body = body
  }

  abort(): void {
    this.aborted = true
    this.listeners.get('abort')?.()
  }

  /** Delivers an upload progress event. */
  reportProgress(loaded: number, total: number): void {
    this.progressListener?.({ lengthComputable: true, loaded, total })
  }

  /** Completes the request with a status and body. */
  finish(status: number, responseText: string): void {
    this.status = status
    this.responseText = responseText
    this.listeners.get('load')?.()
  }
}

function file(name = 'episode01.mkv'): File {
  return new File([new Uint8Array([1, 2, 3])], name, { type: 'video/x-matroska' })
}

afterEach(() => {
  FakeXHR.instances = []
  vi.unstubAllGlobals()
})

function install(): void {
  vi.stubGlobal('XMLHttpRequest', FakeXHR)
}

describe('uploadFile', () => {
  it('posts the file in the field the server looks for', async () => {
    install()
    void uploadFile(file())

    const xhr = FakeXHR.instances[0]!
    expect(xhr.method).toBe('POST')
    expect(xhr.url).toBe('/api/v1/uploads')

    // The field name is part of the contract: the server ignores parts it does
    // not recognise, so a mismatch uploads nothing and reports "no file in the
    // request" only after the bytes have crossed the network.
    const sent = xhr.body!.get('file')
    expect(sent).toBeInstanceOf(File)
    expect((sent as File).name).toBe('episode01.mkv')
  })

  it('reports progress as a fraction', async () => {
    install()
    const fractions: number[] = []
    void uploadFile(file(), (fraction) => fractions.push(fraction))

    const xhr = FakeXHR.instances[0]!
    xhr.reportProgress(0, 200)
    xhr.reportProgress(50, 200)
    xhr.reportProgress(200, 200)

    expect(fractions).toEqual([0, 0.25, 1])
  })

  it('resolves with the staged upload', async () => {
    install()
    const request = uploadFile(file())

    FakeXHR.instances[0]!.finish(
      201,
      JSON.stringify({ id: 'abc', name: 'episode01.mkv', size_bytes: 3, max_bytes: 100 }),
    )

    await expect(request.promise).resolves.toMatchObject({ id: 'abc', size_bytes: 3 })
  })

  it('preserves the error code from the envelope', async () => {
    install()
    const request = uploadFile(file())

    FakeXHR.instances[0]!.finish(
      413,
      JSON.stringify({
        error: { code: 'INVALID_REQUEST', message: 'that file is larger than this server accepts' },
      }),
    )

    // The code, not the prose: the view branches on it to decide whether to
    // offer a retry or explain a limit.
    await expect(request.promise).rejects.toMatchObject({
      code: 'INVALID_REQUEST',
      status: 413,
    })
  })

  it('reports a body that is not an error envelope rather than inventing a message', async () => {
    install()
    const request = uploadFile(file())

    // A proxy or a crash can answer with HTML. Reporting that as the server's
    // message would put a page of markup in front of the user.
    FakeXHR.instances[0]!.finish(502, '<html>bad gateway</html>')

    await expect(request.promise).rejects.toMatchObject({ code: 'UNEXPECTED_RESPONSE' })
  })

  it('cancels with its own code, not a transport failure', async () => {
    install()
    const request = uploadFile(file())

    request.abort()

    await expect(request.promise).rejects.toBeInstanceOf(ApiError)
    await expect(request.promise).rejects.toMatchObject({ code: 'REQUEST_ABORTED' })
    expect(FakeXHR.instances[0]!.aborted).toBe(true)
  })
})
