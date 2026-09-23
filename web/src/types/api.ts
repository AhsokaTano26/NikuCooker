/**
 * Types mirroring the REST API in docs/api.md.
 *
 * Hand-written rather than generated. The API surface is small, the Go types
 * carry doc comments that matter, and a generator would add a build step and a
 * dependency to keep in step with a contract that changes rarely.
 *
 * Everything here is what the server sends. The client never infers pipeline
 * state, so there are no derived or optimistic fields.
 */

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

/** Stable error codes. Clients branch on these, never on a message. */
export type ApiErrorCode = string

export interface ApiErrorBody {
  error: {
    code: ApiErrorCode
    message: string
    details?: Record<string, unknown>
  }
}

export interface Paginated<T> {
  items: T[]
  total: number
  limit: number
  offset: number
}

// ---------------------------------------------------------------------------
// Projects
// ---------------------------------------------------------------------------

export type ProjectStatus = 'active' | 'archived'
export type TranslationStyle = 'literal' | 'natural' | 'fansub'

export interface JobSummary {
  id: string
  status: JobStatus
  progress: number
  current_stage: string | null
}

export interface Project {
  id: string
  name: string
  source_language: string
  target_language: string
  style: TranslationStyle
  status: ProjectStatus
  duration: number | null
  /** What the project occupies on disk, measured when the response was built. */
  size_bytes: number

  segment_count: number
  needs_review_count: number
  current_job: JobSummary | null
  created_at: string
  updated_at: string
}

// ---------------------------------------------------------------------------
// Jobs and pipeline
// ---------------------------------------------------------------------------

export type JobStatus =
  | 'pending'
  | 'running'
  | 'paused'
  | 'completed'
  | 'failed'
  | 'cancelled'

/**
 * Stage states.
 *
 * `cached` is distinct from `completed` on purpose: it means a valid artifact
 * already existed and the stage did not run. Collapsing the two would hide
 * whether a re-run actually recomputed anything.
 */
export type StageStatus =
  | 'pending'
  | 'running'
  | 'cached'
  | 'completed'
  | 'failed'
  | 'skipped'
  | 'cancelled'

export interface StageView {
  name: string
  /** Localised server-side. The client renders this verbatim and never maps a
   *  stage name to a label itself, so an unknown stage still displays. */
  label: string
  ordinal: number
  status: StageStatus
  progress: number
  artifact_id: string | null
  duration_ms?: number
  attempt?: number

  /** Why a stage did not run. Set when the stage was skipped, and empty
   *  otherwise — a stage that completed has nothing to explain. */
  reason?: string

  error_code?: string
  error_message?: string
  metadata?: Record<string, unknown>
}

export interface PipelineView {
  project_id: string
  job: { id: string; status: JobStatus; progress: number } | null
  stages: StageView[]
}

// ---------------------------------------------------------------------------
// Segments
// ---------------------------------------------------------------------------

export type ReviewState = 'none' | 'pending' | 'approved' | 'rejected' | 'edited'

export interface WordTimestamp {
  start: number
  end: number
  text: string
  probability?: number
}

export interface Segment {
  id: string
  ordinal: number
  start: number
  end: number
  speaker: string | null
  source_language: string
  target_language: string
  source_text: string
  translated_text: string | null
  words: WordTimestamp[]
  /** Whisper's avg_logprob: a log probability, so negative, and higher is
   *  better. Not a 0–1 score. */
  asr_confidence: number | null
  translation_confidence: number | null
  cps: number | null
  needs_review: boolean
  review_state: ReviewState
  is_edited: boolean
  tags: string[]
  metadata: Record<string, unknown>
  qc?: QCFinding[]
}

// ---------------------------------------------------------------------------
// QC
// ---------------------------------------------------------------------------

export type QCSeverity = 'info' | 'warning' | 'error'

export interface QCFinding {
  id: string
  segment_id: string | null
  segment_ordinal?: number
  stage: string
  severity: QCSeverity
  code: string
  message: string
  suggestion: string | null
  resolved: boolean
  created_at: string
}

export interface QCSummary {
  error: number
  warning: number
  info: number
}

// ---------------------------------------------------------------------------
// System
// ---------------------------------------------------------------------------

/**
 * What the startup check found about the AI environment.
 *
 * `missing` and `failed` are the two that call for an install: nothing
 * resolved, or something resolved and could not import the worker package.
 * `ready` means this installation can transcribe, whether or not an
 * environment was ever provisioned — a checkout with ai/.venv has not been,
 * and needs nothing.
 *
 * `busy` and `crashed` describe a worker process, and are not produced by the
 * check.
 */
export type WorkerStatus =
  | 'starting'
  | 'ready'
  | 'busy'
  | 'crashed'
  | 'failed'
  | 'missing'
  | 'stopped'

export interface LoadedModel {
  name: string
  kind: string
  device: string
  memory_mb?: number
}

export interface SystemOverview {
  version: string
  commit: string
  platform: string
  uptime_s: number
  counts: {
    projects: number
    jobs_running: number
    jobs_pending: number
    segments: number
    needs_review: number
  }
  worker: {
    status: WorkerStatus
    workers: number
    python: string
    /** Why it is not ready, when it is not, in the search's own words. */
    detail?: string
    worker_version: string
    /** Reported by the worker and compared against the core's own. A mismatch
     *  means the two sides disagree about data shapes and the core refuses to
     *  start — the usual cause is an upgraded binary with a stale venv. */
    schema_digest: string
    loaded_models: LoadedModel[]
  }
  runtime: RuntimeView
  stats: SystemStats
  features: SystemFeatures
}

/** The steps an install goes through, in order. */
export type RuntimePhase = 'detect' | 'interpreter' | 'dependencies' | 'verify'

export type RuntimeStatus = 'idle' | 'running' | 'ready' | 'failed' | 'cancelled'

/**
 * The Python environment the worker runs from.
 *
 * `status` and `provisioned` answer different questions and disagree in the
 * cases that matter: `provisioned` is whether an interpreter exists on disk, so
 * it stays true across a restart of the server, while `status` describes what
 * this process has done — which is idle on a server that did not perform the
 * install.
 */
export interface RuntimeView {
  /** Whether this installation can install an environment at all. */
  available: boolean
  /** Why not, when it cannot. Shown instead of a button that would fail. */
  reason?: string

  status: RuntimeStatus
  phase?: RuntimePhase

  provisioned: boolean
  python?: string
  runtime_dir: string
  uv?: string
  /** Which optional dependency set the interpreter was built with — 'cpu' or
   *  'cuda'. Absent when this program has no record of building it, which is a
   *  different answer from the CPU one and is shown as neither. */
  accelerator?: string

  error_code?: string
  error_message?: string
  /** What to do about the failure, when it is one people hit often enough to
   *  be worth recognising. */
  remediation?: string

  cuda: CudaView

  started_at?: string
  finished_at?: string
}

/**
 * Why the GPU option is not available. A stable code, never prose: the wording
 * belongs with the other labels, in `composables/environment.ts`.
 *
 * `platform` — no nvidia-* wheels are published for this system.
 * `no_gpu`   — there is nothing here to accelerate.
 */
export type CudaReasonCode = 'platform' | 'no_gpu'

/**
 * The GPU option.
 *
 * Whether the install can be asked for CUDA acceleration on this machine, and
 * what that costs. Decided by the server from the detected hardware and the
 * platform, because it has to agree with what the install request itself will
 * enforce — a page that offers an option the server refuses is worse than one
 * that never offered it.
 */
export interface CudaView {
  /** Whether the CUDA dependency set can be installed and used here. */
  available: boolean
  /** Why not, when it cannot: a stable code, not prose. The wording lives in
   *  CUDA_REASON, because the interface is the only thing that renders it. */
  reason_code?: CudaReasonCode
  /** The accelerators that were detected, by name, so "yes" is checkable. */
  gpus: string[]
  /** What choosing it adds to the download, in bytes. */
  extra_bytes: number
  /** Whether the environment on disk was built with it. */
  installed: boolean
}

/**
 * What this installation is configured to do.
 *
 * Read before offering an action rather than after it fails: a capability that
 * is off is a refusal waiting to happen, and a form that says so only once it
 * has been filled in wastes the filling in.
 */
export interface SystemFeatures {
  /** Whether a project can be created from a server-side path
   *  (config: `server.allow_path_source`). */
  path_source: boolean
  /** The largest upload this server accepts. A client-side check against it
   *  saves a transfer that the server would refuse at the end. */
  max_upload_bytes: number
  /** The configuration file this process reads. Reported even when it does not
   *  exist, because "no file, and here is where one goes" is what a user needs
   *  in order to change a setting. */
  config_path: string
  config_file_exists: boolean
}

export interface SystemStats {
  cpu: { cores: number; usage_percent: number | null }
  memory: { total_bytes: number | null; used_bytes: number | null }
  disk: { data_free_bytes: number | null; models_free_bytes: number | null }
  /** Empty when there is no GPU — never null, so the dashboard renders the
   *  same shape everywhere. */
  gpu: GPUInfo[]
}

export interface GPUInfo {
  index: number
  name: string
  memory_total_bytes?: number
  memory_used_bytes?: number
  utilization_percent?: number
  driver?: string
  cuda?: string
}
