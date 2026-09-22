/**
 * Typed client for the NikuCooker REST API.
 *
 * Every non-2xx response carries an error envelope with a stable code
 * (docs/api.md §1.3). This client preserves the code rather than flattening it
 * into a message, because callers branch on codes and never on prose.
 */

import type {
  ApiErrorBody,
  ApiErrorCode,
  Paginated,
  PipelineView,
  Project,
  QCFinding,
  QCSummary,
  Segment,
  SystemOverview,
  TranslationStyle,
} from '@/types/api'

export const API_BASE = '/api/v1'

export class ApiError extends Error {
  readonly code: ApiErrorCode
  readonly status: number
  readonly details: Record<string, unknown>

  constructor(
    code: ApiErrorCode,
    message: string,
    status: number,
    details: Record<string, unknown> = {},
  ) {
    super(message)
    this.name = 'ApiError'
    this.code = code
    this.status = status
    this.details = details
  }
}

function isApiErrorBody(value: unknown): value is ApiErrorBody {
  if (typeof value !== 'object' || value === null) return false
  const candidate = (value as { error?: unknown }).error
  if (typeof candidate !== 'object' || candidate === null) return false
  const { code, message } = candidate as { code?: unknown; message?: unknown }
  return typeof code === 'string' && typeof message === 'string'
}

export interface RequestOptions {
  method?: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE'
  body?: unknown
  query?: Record<string, string | number | boolean | undefined>
  signal?: AbortSignal
}

async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const url = new URL(`${API_BASE}${path}`, globalThis.location?.origin ?? 'http://localhost')

  for (const [key, value] of Object.entries(options.query ?? {})) {
    if (value !== undefined) url.searchParams.set(key, String(value))
  }

  const headers: Record<string, string> = {}
  const init: RequestInit = { method: options.method ?? 'GET', headers }

  if (options.body !== undefined) {
    headers['Content-Type'] = 'application/json'
    init.body = JSON.stringify(options.body)
  }
  if (options.signal !== undefined) {
    init.signal = options.signal
  }

  let response: Response
  try {
    response = await fetch(url, init)
  } catch (cause) {
    // A transport failure is not an API error, and reporting it as one would
    // make "the server said no" indistinguishable from "the server is down" —
    // which are different problems with different remedies.
    throw new ApiError(
      'NETWORK_UNREACHABLE',
      '无法连接到 NikuCooker 服务，请确认后端正在运行。',
      0,
      { cause: String(cause) },
    )
  }

  if (response.status === 204) {
    return undefined as T
  }

  const text = await response.text()
  let payload: unknown
  if (text !== '') {
    try {
      payload = JSON.parse(text)
    } catch {
      payload = undefined
    }
  }

  if (!response.ok) {
    if (isApiErrorBody(payload)) {
      throw new ApiError(
        payload.error.code,
        payload.error.message,
        response.status,
        payload.error.details ?? {},
      )
    }
    throw new ApiError(
      'UNEXPECTED_RESPONSE',
      `服务返回了 ${response.status}，且响应体不是协议规定的错误格式。`,
      response.status,
    )
  }

  return payload as T
}

// ---------------------------------------------------------------------------
// Projects
// ---------------------------------------------------------------------------

export interface ListProjectsQuery {
  status?: 'active' | 'archived' | 'all'
  q?: string
  limit?: number
  offset?: number
}

export interface CreateProjectBody {
  name: string
  source_language: string
  target_language: string
  style: TranslationStyle
  source: { kind: 'upload'; upload_id: string } | { kind: 'path'; path: string }
  config?: Record<string, unknown>
}

/** One file a run produced. */
export interface ProjectOutput {
  name: string
  size_bytes: number
  modified_at: string
  /** A coarse label to group by: subtitle, video, other. */
  kind: 'subtitle' | 'video' | 'other'
}

export interface ProjectOutputs {
  items: ProjectOutput[]
  /** Where the files are on the machine running the core. An empty list with a
   *  directory is a project that has not finished a run. */
  dir: string
}

export const projects = {
  list: (query: ListProjectsQuery = {}, signal?: AbortSignal): Promise<Paginated<Project>> =>
    request('/projects', { query: { ...query }, signal }),

  get: (id: string, signal?: AbortSignal): Promise<Project> =>
    request(`/projects/${id}`, { signal }),

  create: (body: CreateProjectBody): Promise<Project> =>
    request('/projects', { method: 'POST', body }),

  update: (id: string, body: Partial<CreateProjectBody>): Promise<Project> =>
    request(`/projects/${id}`, { method: 'PATCH', body }),

  outputs: (id: string, signal?: AbortSignal): Promise<ProjectOutputs> =>
    request(`/projects/${id}/outputs`, { signal }),

  /** The URL a browser downloads a published file from. Not a fetch: the point
   *  is the browser's own download, with its progress and its filename. */
  outputURL: (id: string, name: string): string =>
    `${API_BASE}/projects/${id}/outputs/${encodeURIComponent(name)}`,

  remove: (id: string, options: { confirm: boolean; deleteFiles: boolean }): Promise<void> =>
    request(`/projects/${id}`, {
      method: 'DELETE',
      query: { confirm: options.confirm, delete_files: options.deleteFiles },
    }),
}

// ---------------------------------------------------------------------------
// Run control
// ---------------------------------------------------------------------------

export interface RunBody {
  from_stage?: string | null
  to_stage?: string | null
  stages?: string[] | null
  force?: boolean
}

export const run = {
  start: (id: string, body: RunBody = {}): Promise<PipelineView> =>
    request(`/projects/${id}/run`, { method: 'POST', body }),

  pause: (id: string): Promise<void> => request(`/projects/${id}/pause`, { method: 'POST' }),

  resume: (id: string): Promise<void> => request(`/projects/${id}/resume`, { method: 'POST' }),

  cancel: (id: string): Promise<void> => request(`/projects/${id}/cancel`, { method: 'POST' }),

  pipeline: (id: string, signal?: AbortSignal): Promise<PipelineView> =>
    request(`/projects/${id}/pipeline`, { signal }),

  runStage: (id: string, stage: string, force = false): Promise<void> =>
    request(`/projects/${id}/stages/${stage}/run`, { method: 'POST', body: { force } }),
}

// ---------------------------------------------------------------------------
// Segments
// ---------------------------------------------------------------------------

export interface ListSegmentsQuery {
  limit?: number
  offset?: number
  needs_review?: boolean
  qc_severity?: string
  q?: string
  order?: 'ordinal' | 'start' | 'cps' | 'duration'
  include_qc?: boolean
}

export interface UpdateSegmentBody {
  translated_text?: string
  start?: number
  end?: number
  speaker?: string | null
  review_state?: Segment['review_state']
  tags?: string[]
}

export const segments = {
  list: (
    projectId: string,
    query: ListSegmentsQuery = {},
    signal?: AbortSignal,
  ): Promise<Paginated<Segment>> =>
    request(`/projects/${projectId}/segments`, { query: { ...query }, signal }),

  get: (projectId: string, segmentId: string, signal?: AbortSignal): Promise<Segment> =>
    request(`/projects/${projectId}/segments/${segmentId}`, { signal }),

  update: (projectId: string, segmentId: string, body: UpdateSegmentBody): Promise<Segment> =>
    request(`/projects/${projectId}/segments/${segmentId}`, { method: 'PUT', body }),

  /** Both halves come back, because neither is "the" line afterwards. */
  split: (projectId: string, segmentId: string, at: number): Promise<{ first: Segment; second: Segment }> =>
    request(`/projects/${projectId}/segments/${segmentId}/split`, {
      method: 'POST',
      body: { at },
    }),

  merge: (
    projectId: string,
    segmentId: string,
    withNext = true,
  ): Promise<{ merged: Segment; absorbed: Segment }> =>
    request(`/projects/${projectId}/segments/${segmentId}/merge`, {
      method: 'POST',
      body: { with_next: withNext },
    }),

  /**
   * A review decision, as distinct from an edit.
   *
   * Approving a line does not mark it hand-edited, so a later run is still free
   * to improve a translation a reviewer merely accepted.
   */
  review: (
    projectId: string,
    segmentId: string,
    reviewState: Segment['review_state'],
  ): Promise<Segment> =>
    request(`/projects/${projectId}/segments/${segmentId}/review`, {
      method: 'POST',
      body: { review_state: reviewState },
    }),

  /** Bypasses the translation cache by design: the user is asking for a
   *  different answer, and returning the cached one would be indistinguishable
   *  from a bug. */
  translate: (projectId: string, segmentId: string, style?: TranslationStyle): Promise<Segment> =>
    request(`/projects/${projectId}/segments/${segmentId}/translate`, {
      method: 'POST',
      body: { style, use_context: true, glossary: true },
    }),

  transcribe: (projectId: string, segmentId: string): Promise<Segment> =>
    request(`/projects/${projectId}/segments/${segmentId}/transcribe`, { method: 'POST' }),
}

// ---------------------------------------------------------------------------
// QC
// ---------------------------------------------------------------------------

export interface ListQCQuery {
  severity?: string
  code?: string
  resolved?: boolean
  limit?: number
  offset?: number
}

export interface QCListResponse extends Paginated<QCFinding> {
  summary: QCSummary
}

export const qc = {
  list: (projectId: string, query: ListQCQuery = {}, signal?: AbortSignal): Promise<QCListResponse> =>
    request(`/projects/${projectId}/qc`, { query: { ...query }, signal }),

  resolve: (projectId: string, findingId: string, resolved = true): Promise<void> =>
    request(`/projects/${projectId}/qc/${findingId}`, {
      method: 'PATCH',
      body: { resolved },
    }),
}

// ---------------------------------------------------------------------------
// System
// ---------------------------------------------------------------------------

export const system = {
  overview: (signal?: AbortSignal): Promise<SystemOverview> => request('/system', { signal }),
  health: (signal?: AbortSignal): Promise<{ status: string }> => request('/system/health', { signal }),
}


// ---------------------------------------------------------------------------
// Models
// ---------------------------------------------------------------------------

export type ModelStatus = 'missing' | 'downloading' | 'ready' | 'failed'

export interface ModelRecord {
  id: string
  name: string
  kind: string
  status: ModelStatus
  size_bytes: number
  estimated_size_bytes?: number
  progress: number
  note?: string
  error_message?: string
  installed_at?: string
}

export const models = {
  list: (signal?: AbortSignal): Promise<{ items: ModelRecord[] }> =>
    request('/models', { signal }),

  /**
   * Starts a download and returns immediately.
   *
   * Progress arrives over the event stream as `model.progress`. Holding the
   * request open for several gigabytes would be killed by any proxy in front of
   * it, and would give the view nothing to show while it waited.
   */
  download: (id: string): Promise<void> => request(`/models/${id}/download`, { method: 'POST' }),

  remove: (id: string): Promise<void> => request(`/models/${id}`, { method: 'DELETE' }),
}

// ---------------------------------------------------------------------------
// Providers
// ---------------------------------------------------------------------------

export interface Provider {
  id: string
  name: string
  kind: 'llm' | 'asr'
  type: string
  base_url?: string
  model?: string
  enabled: boolean
  /** Whether a key is stored. The key itself is never sent. */
  has_key: boolean
  created_at: string
  updated_at: string
}

export interface ProviderInput {
  name: string
  kind: 'llm' | 'asr'
  type: string
  base_url: string
  /** Omitted or empty leaves a stored key alone. */
  api_key?: string
  model: string
  enabled?: boolean
}

export interface ProviderTestResult {
  ok: boolean
  message: string
  model?: string
  /** True of the provider but not a failure — a reasoning model using the
   *  whole check budget, for instance. Rendered apart from `message` so a
   *  working provider is not shown in the same colour as a broken one. */
  warning?: string
}

export const providers = {
  list: (signal?: AbortSignal): Promise<{ items: Provider[] }> =>
    request('/providers', { signal }),

  create: (body: ProviderInput): Promise<Provider> =>
    request('/providers', { method: 'POST', body }),

  update: (id: string, body: Partial<ProviderInput>): Promise<Provider> =>
    request(`/providers/${id}`, { method: 'PATCH', body }),

  remove: (id: string): Promise<void> => request(`/providers/${id}`, { method: 'DELETE' }),

  /** Asks the endpoint whether it answers. Catches a bad key before a job does. */
  test: (id: string): Promise<ProviderTestResult> =>
    request(`/providers/${id}/test`, { method: 'POST' }),
}

// ---------------------------------------------------------------------------
// Settings and logs
// ---------------------------------------------------------------------------

/** One editable setting, as the server describes it. */
export interface SettingDescriptor {
  key: string
  name: string
  help: string
  group: string

  kind: 'bool' | 'int' | 'float' | 'string' | 'enum' | 'list' | 'bytes'
  options?: { value: string; label: string }[]

  unit?: string
  min?: number
  max?: number
  step?: number

  advanced: boolean

  /** The value in effect right now, after every layer. */
  value: unknown

  /** The layer that set it: default, config_file, environment, database,
   *  project or cli. */
  source: string

  /** Whether the database holds a value, which is what makes reset meaningful.
   *  Not derivable from source being "database": a key set to the value the
   *  default already had still has a row. */
  overridden: boolean
}

export interface Settings {
  config: Record<string, unknown>
  /** Which layer set each key. The answer to "I changed it and nothing happened". */
  provenance: Record<string, string>
  data_dir: string
  /** The configuration file this process reads, and whether it is there. A
   *  missing file is not an error, but it does mean a change has nowhere to go. */
  config_path: string
  config_file_exists: boolean
  /** Every setting the interface may change. A key absent from it is readable
   *  here and editable only in the configuration file. */
  catalog: SettingDescriptor[]
}

export interface LogRecord {
  seq: number
  time: string
  level: 'debug' | 'info' | 'warn' | 'error'
  msg: string
  attrs?: Record<string, unknown>
}

export const settings = {
  get: (signal?: AbortSignal): Promise<Settings> => request('/settings', { signal }),

  /** Stores values and returns the settings as they now stand.
   *
   *  The whole response rather than nothing, because a save re-resolves the
   *  configuration: the values that come back are the ones in effect, which is
   *  what the form should show rather than what was sent. */
  update: (values: Record<string, unknown>): Promise<Settings> =>
    request('/settings', { method: 'PATCH', body: { values } }),

  /** Drops one override, returning the key to the layers underneath. */
  reset: (key: string): Promise<void> =>
    request(`/settings/${encodeURIComponent(key)}`, { method: 'DELETE' }),
}

// ---------------------------------------------------------------------------
// Uploads
// ---------------------------------------------------------------------------

export const uploads = { create: uploadFile, discard: discardUpload }

export interface Upload {
  id: string
  name: string
  size_bytes: number
  max_bytes: number
}

/**
 * An upload in flight.
 *
 * Returned as a handle rather than a bare promise so the caller can cancel.
 * Uploading a multi-gigabyte file is long enough that "I picked the wrong one"
 * is certain to happen, and without this the only remedy is to wait it out or
 * reload the page.
 */
export interface UploadHandle {
  promise: Promise<Upload>
  abort: () => void
}

/**
 * Uploads a file, reporting progress.
 *
 * XMLHttpRequest rather than fetch, which is the one thing this client does not
 * use the shared `request` helper for. Fetch cannot report upload progress —
 * request bodies are not streams in any browser that matters yet — and a
 * progress bar is the entire reason this is a separate request from creating
 * the project.
 */
export function uploadFile(
  file: File,
  onProgress?: (fraction: number) => void,
): UploadHandle {
  const xhr = new XMLHttpRequest()

  const promise = new Promise<Upload>((resolve, reject) => {
    xhr.open('POST', `${API_BASE}/uploads`)

    xhr.upload.addEventListener('progress', (event) => {
      // lengthComputable is false for a chunked request, in which case the
      // total is unknown and a fraction would be a guess.
      if (event.lengthComputable && onProgress) {
        onProgress(event.loaded / event.total)
      }
    })

    xhr.addEventListener('load', () => {
      const payload = parseBody(xhr.responseText)

      if (xhr.status >= 200 && xhr.status < 300) {
        resolve(payload as Upload)
        return
      }
      if (isApiErrorBody(payload)) {
        reject(
          new ApiError(payload.error.code, payload.error.message, xhr.status, payload.error.details ?? {}),
        )
        return
      }
      reject(
        new ApiError('UNEXPECTED_RESPONSE', `服务返回了 ${xhr.status}，且响应体不是协议规定的错误格式。`, xhr.status),
      )
    })

    xhr.addEventListener('error', () => {
      reject(new ApiError('NETWORK_UNREACHABLE', '上传中断，无法连接到 NikuCooker 服务。', 0))
    })

    xhr.addEventListener('abort', () => {
      reject(new ApiError('REQUEST_ABORTED', '上传已取消。', 0))
    })

    // The Content-Type is left to the browser: it is the one that knows the
    // multipart boundary it generated.
    const form = new FormData()
    form.append('file', file, file.name)
    xhr.send(form)
  })

  return { promise, abort: () => xhr.abort() }
}

/** Discards a staged upload. */
export function discardUpload(id: string): Promise<void> {
  return request(`/uploads/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

function parseBody(text: string): unknown {
  if (text === '') return undefined
  try {
    return JSON.parse(text)
  } catch {
    return undefined
  }
}

export const logs = {
  /** `after` selects records newer than a sequence number, for following the log. */
  list: (after = 0, limit = 500, signal?: AbortSignal): Promise<{ items: LogRecord[]; seq: number }> =>
    request('/logs', { query: { after, limit }, signal }),
}

export const api = { projects, run, segments, qc, system, models, providers, settings, logs, uploads }
