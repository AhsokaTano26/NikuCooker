-- Initial schema.
--
-- Forward-only and self-contained: this file is the whole schema at version 1.
-- SQLite has transactional DDL, so a failure here rolls back cleanly rather
-- than leaving a half-applied schema.
--
-- See docs/database.md for the reasoning behind each table and index.

-- Note: schema_migrations is deliberately absent. The migrator creates and
-- owns it before reading this file, because it has to know which migrations
-- have run in order to decide whether to run this one at all. Declaring it here
-- as well would collide on a fresh database.

-- ---------------------------------------------------------------------------
-- Projects
-- ---------------------------------------------------------------------------

CREATE TABLE projects (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    source_path     TEXT NOT NULL,               -- relative to the project dir
    source_language TEXT NOT NULL,               -- BCP-47, e.g. "ja"
    target_language TEXT NOT NULL,               -- BCP-47, e.g. "zh-Hans"
    style           TEXT NOT NULL DEFAULT 'fansub',
    status          TEXT NOT NULL DEFAULT 'active',

    -- Sparse overlay on the global configuration, stored as JSON rather than
    -- exploded into columns: the config surface keeps growing, and
    -- column-per-key would mean a migration per option.
    config_json     TEXT NOT NULL DEFAULT '{}',

    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,

    CHECK (status IN ('active','archived')),
    CHECK (style  IN ('literal','natural','fansub'))
);

CREATE INDEX idx_projects_status ON projects(status, updated_at DESC);

-- ---------------------------------------------------------------------------
-- Jobs
-- ---------------------------------------------------------------------------

CREATE TABLE jobs (
    id           TEXT PRIMARY KEY,
    project_id   TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    kind         TEXT NOT NULL,                  -- full | stage
    target_stage TEXT,                           -- set when kind = 'stage'
    force        INTEGER NOT NULL DEFAULT 0,     -- bool: ignore the artifact cache

    status   TEXT NOT NULL DEFAULT 'pending',
    progress REAL NOT NULL DEFAULT 0,

    error_code    TEXT,
    error_message TEXT,

    created_at   TEXT NOT NULL,
    started_at   TEXT,
    finished_at  TEXT,

    -- Updated periodically while running. Startup reconciliation uses it to
    -- distinguish a job that is genuinely running from one orphaned by a crash.
    heartbeat_at TEXT,

    CHECK (status IN ('pending','running','paused','completed','failed','cancelled')),
    CHECK (kind   IN ('full','stage')),
    CHECK (kind != 'stage' OR target_stage IS NOT NULL),
    CHECK (progress >= 0 AND progress <= 1)
);

-- The scheduler's hot query: "is anything running for this project?"
CREATE INDEX idx_jobs_project_status ON jobs(project_id, status);

-- Startup reconciliation: "find every job left running".
CREATE INDEX idx_jobs_status ON jobs(status) WHERE status IN ('running','paused');

-- History listing.
CREATE INDEX idx_jobs_recent ON jobs(project_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- Stage runs
-- ---------------------------------------------------------------------------

CREATE TABLE stages (
    job_id     TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    ordinal    INTEGER NOT NULL,                 -- position in the pipeline

    status   TEXT NOT NULL DEFAULT 'pending',
    progress REAL NOT NULL DEFAULT 0,
    attempt  INTEGER NOT NULL DEFAULT 1,

    artifact_id TEXT REFERENCES artifacts(id) ON DELETE SET NULL,

    error_code    TEXT,
    error_message TEXT,

    started_at  TEXT,
    finished_at TEXT,

    -- The composite key is the database enforcing "stage status is per
    -- attempt": one row per stage per job, with no surrogate key to drift.
    PRIMARY KEY (job_id, name),
    CHECK (status IN ('pending','running','cached','completed','failed','skipped','cancelled')),
    CHECK (progress >= 0 AND progress <= 1)
);

CREATE INDEX idx_stages_job ON stages(job_id, ordinal);
CREATE INDEX idx_stages_project ON stages(project_id, name, finished_at DESC);

-- ---------------------------------------------------------------------------
-- Artifacts
-- ---------------------------------------------------------------------------

CREATE TABLE artifacts (
    id         TEXT PRIMARY KEY,                 -- "art_" + 16 hex
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    stage      TEXT NOT NULL,

    input_hash  TEXT NOT NULL,
    config_hash TEXT NOT NULL,
    key         TEXT NOT NULL,                   -- sha256(input_hash || config_hash)

    status TEXT NOT NULL DEFAULT 'building',
    path   TEXT NOT NULL,                        -- relative to the project dir

    provider       TEXT,
    model          TEXT,
    prompt_version TEXT,

    size_bytes    INTEGER NOT NULL DEFAULT 0,
    metadata_json TEXT NOT NULL DEFAULT '{}',

    created_at TEXT NOT NULL,

    CHECK (status IN ('building','ready','failed'))
);

-- The cache lookup: one row per (project, key). It is also what makes artifact
-- creation idempotent — two jobs racing to produce the same artifact cannot
-- both insert, and the loser reads the winner's row.
CREATE UNIQUE INDEX idx_artifacts_key ON artifacts(project_id, key);

-- "Newest ready artifact for this stage", run on every stage plan.
CREATE INDEX idx_artifacts_lookup
    ON artifacts(project_id, stage, created_at DESC)
    WHERE status = 'ready';

-- Retention sweep, oldest first.
CREATE INDEX idx_artifacts_gc ON artifacts(project_id, stage, created_at);

-- ---------------------------------------------------------------------------
-- Segments
-- ---------------------------------------------------------------------------

CREATE TABLE segments (
    id         TEXT PRIMARY KEY,                 -- "seg_" + 10 hex
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    ordinal    INTEGER NOT NULL,

    start REAL NOT NULL,
    end   REAL NOT NULL,
    speaker TEXT,

    source_language TEXT NOT NULL,
    target_language TEXT NOT NULL,

    source_text     TEXT NOT NULL,
    translated_text TEXT,

    words_json TEXT NOT NULL DEFAULT '[]',       -- []WordTimestamp

    asr_confidence         REAL,
    translation_confidence REAL,
    cps                    REAL,

    needs_review INTEGER NOT NULL DEFAULT 0,
    review_state TEXT NOT NULL DEFAULT 'none',
    is_edited    INTEGER NOT NULL DEFAULT 0,

    tags_json     TEXT NOT NULL DEFAULT '[]',
    metadata_json TEXT NOT NULL DEFAULT '{}',

    seg_artifact_id   TEXT REFERENCES artifacts(id) ON DELETE SET NULL,
    trans_artifact_id TEXT REFERENCES artifacts(id) ON DELETE SET NULL,

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    -- The database refuses to store a zero-length or inverted line, so a
    -- segmentation bug surfaces as a failed insert rather than as a broken
    -- subtitle file a user discovers on playback.
    CHECK (end > start),
    CHECK (start >= 0),
    CHECK (review_state IN ('none','pending','approved','rejected','edited'))
);

CREATE UNIQUE INDEX idx_segments_ordinal ON segments(project_id, ordinal);

-- The editor's ordered fetch.
CREATE INDEX idx_segments_project_time ON segments(project_id, start);

-- The review queue's fetch and the dashboard count. Partial, because these are
-- the minority of rows and the ones queried most.
CREATE INDEX idx_segments_review ON segments(project_id, needs_review)
    WHERE needs_review = 1;

-- ---------------------------------------------------------------------------
-- QC findings
-- ---------------------------------------------------------------------------

CREATE TABLE qc_results (
    id         TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- Nullable with CASCADE: a finding with no segment is a project-level
    -- finding, and a finding dies with the segment it describes rather than
    -- dangling.
    segment_id TEXT REFERENCES segments(id) ON DELETE CASCADE,

    stage      TEXT NOT NULL,                    -- qc | translation | asr
    severity   TEXT NOT NULL,                    -- info | warning | error
    code       TEXT NOT NULL,                    -- EMPTY_TRANSLATION, CPS_EXCEEDED, ...
    message    TEXT NOT NULL,
    suggestion TEXT,

    resolved    INTEGER NOT NULL DEFAULT 0,
    resolved_at TEXT,

    artifact_id TEXT REFERENCES artifacts(id) ON DELETE SET NULL,
    created_at  TEXT NOT NULL,

    CHECK (severity IN ('info','warning','error'))
);

CREATE INDEX idx_qc_segment ON qc_results(segment_id, resolved);
CREATE INDEX idx_qc_open    ON qc_results(project_id, severity) WHERE resolved = 0;

-- ---------------------------------------------------------------------------
-- Glossaries
-- ---------------------------------------------------------------------------

CREATE TABLE glossaries (
    id         TEXT PRIMARY KEY,
    project_id TEXT REFERENCES projects(id) ON DELETE CASCADE,  -- NULL = global

    source TEXT NOT NULL,
    target TEXT NOT NULL,
    type   TEXT NOT NULL DEFAULT 'term',         -- term|character|place|org|work|honorific
    note   TEXT,

    priority INTEGER NOT NULL DEFAULT 100,       -- lower wins on conflict
    enabled  INTEGER NOT NULL DEFAULT 1,
    origin   TEXT NOT NULL DEFAULT 'manual',     -- manual | auto

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    CHECK (origin IN ('manual','auto'))
);

-- Two partial unique indexes rather than one composite: SQLite treats NULL as
-- distinct in a unique index, so UNIQUE(project_id, source) would allow
-- unlimited duplicate *global* entries — precisely the opposite of what a
-- glossary needs.
CREATE UNIQUE INDEX idx_glossary_project
    ON glossaries(project_id, source) WHERE project_id IS NOT NULL;

CREATE UNIQUE INDEX idx_glossary_global
    ON glossaries(source) WHERE project_id IS NULL;

-- The lookup that builds the glossary block for a translation prompt.
CREATE INDEX idx_glossary_active ON glossaries(project_id, enabled, priority);

-- ---------------------------------------------------------------------------
-- Translation cache
-- ---------------------------------------------------------------------------

-- Global rather than per project: the same line from the same show in the same
-- style should cost once, and a user running a twelve-episode series gets real
-- benefit from the cross-episode hit rate. The risk of a translation that is
-- correct in one project's context being reused in another's is exactly what
-- context_hash and glossary_hash in the key prevent.
CREATE TABLE translation_cache (
    key         TEXT PRIMARY KEY,

    source_text TEXT NOT NULL,
    translated  TEXT NOT NULL,

    provider       TEXT NOT NULL,
    model          TEXT NOT NULL,
    prompt_version TEXT NOT NULL,
    style          TEXT NOT NULL,

    token_in  INTEGER NOT NULL DEFAULT 0,
    token_out INTEGER NOT NULL DEFAULT 0,

    hits         INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL,
    last_used_at TEXT NOT NULL
);

CREATE INDEX idx_transcache_lru    ON translation_cache(last_used_at);
CREATE INDEX idx_transcache_source ON translation_cache(source_text);

-- ---------------------------------------------------------------------------
-- Providers
-- ---------------------------------------------------------------------------

CREATE TABLE providers (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    kind       TEXT NOT NULL,                    -- llm | asr
    type       TEXT NOT NULL,                    -- openai-compatible | faster-whisper | ...
    base_url   TEXT,

    -- Read and written, never serialised out of the core. The Go field carries
    -- `json:"-"` so that leaking it through a handler requires writing an
    -- explicit conversion rather than forgetting a struct tag.
    api_key    TEXT,

    model      TEXT,
    enabled    INTEGER NOT NULL DEFAULT 1,
    extra_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    CHECK (kind IN ('llm','asr'))
);

CREATE INDEX idx_providers_kind ON providers(kind, enabled);

-- ---------------------------------------------------------------------------
-- Models
-- ---------------------------------------------------------------------------

CREATE TABLE model_records (
    id       TEXT PRIMARY KEY,                   -- "asr:large-v3"
    kind     TEXT NOT NULL,                      -- asr | vad
    name     TEXT NOT NULL,
    provider TEXT NOT NULL,

    path       TEXT NOT NULL,
    status     TEXT NOT NULL DEFAULT 'missing',
    size_bytes INTEGER NOT NULL DEFAULT 0,
    sha256     TEXT,
    source_url TEXT,

    progress      REAL NOT NULL DEFAULT 0,
    error_message TEXT,

    installed_at TEXT,
    -- When the filesystem was last scanned. Status is derived on startup rather
    -- than trusted from the row, so a hand-deleted model directory reports as
    -- missing rather than lying.
    checked_at   TEXT NOT NULL,

    CHECK (status IN ('missing','downloading','ready','error')),
    CHECK (kind IN ('asr','vad'))
);

CREATE UNIQUE INDEX idx_models_kind_name ON model_records(kind, name);

-- ---------------------------------------------------------------------------
-- Settings
-- ---------------------------------------------------------------------------

-- Only what the web UI changes at runtime and what must survive a restart.
-- Configuration a user edits by hand lives in the config file, and the file
-- wins for anything both can express — otherwise a user edits YAML, sees no
-- effect, and has no way to discover that a database row is overriding them.
CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value_json TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
