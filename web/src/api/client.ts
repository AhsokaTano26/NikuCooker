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

export const projects = {
  list: (query: ListProjectsQuery = {}, signal?: AbortSignal): Promise<Paginated<Project>> =>
    request('/projects', { query: { ...query }, signal }),

  get: (id: string, signal?: AbortSignal): Promise<Project> =>
    request(`/projects/${id}`, { signal }),

  create: (body: CreateProjectBody): Promise<Project> =>
    request('/projects', { method: 'POST', body }),

  update: (id: string, body: Partial<CreateProjectBody>): Promise<Project> =>
    request(`/projects/${id}`, { method: 'PATCH', body }),

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

  split: (projectId: string, segmentId: string, at: number): Promise<Segment> =>
    request(`/projects/${projectId}/segments/${segmentId}/split`, {
      method: 'POST',
      body: { at },
    }),

  merge: (projectId: string, segmentId: string, withNext = true): Promise<Segment> =>
    request(`/projects/${projectId}/segments/${segmentId}/merge`, {
      method: 'POST',
      body: { with_next: withNext },
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

export const api = { projects, run, segments, qc, system }
