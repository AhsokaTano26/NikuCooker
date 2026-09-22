package api

import "time"

// The API's response shapes.
//
// Distinct from the domain types on purpose. A project row carries a
// source_path and a config overlay the UI has no business reading; a job row
// carries a heartbeat timestamp that is nobody's business. Serving the domain
// types directly would mean every field added to them appears in the API by
// default, and the first one that should not have is discovered in review or
// not at all.
//
// The names and JSON tags match web/src/types/api.ts exactly. That file is the
// contract, and a change here is a change there.

// projectView is one project as the UI sees it.
type projectView struct {
	ID   string `json:"id"`
	Name string `json:"name"`

	SourceLanguage string `json:"source_language"`
	TargetLanguage string `json:"target_language"`
	Style          string `json:"style"`
	Status         string `json:"status"`

	// Duration is the media's length, or null before anything has been read
	// from it. Zero would be indistinguishable from an empty file.
	Duration *float64 `json:"duration"`

	SegmentCount     int `json:"segment_count"`
	NeedsReviewCount int `json:"needs_review_count"`

	// CurrentJob is the run in progress, or null. The UI uses it to decide
	// whether to show progress or a run button.
	CurrentJob *jobView `json:"current_job"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// jobView is a run as the UI sees it.
type jobView struct {
	ID       string  `json:"id"`
	Status   string  `json:"status"`
	Progress float64 `json:"progress"`

	CurrentStage string `json:"current_stage,omitempty"`

	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`

	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// stageView is one stage of a pipeline.
type stageView struct {
	Name string `json:"name"`

	// Label is the display name, localised server-side. The client renders it
	// verbatim, so a stage added after the UI shipped still displays something
	// meaningful.
	Label string `json:"label"`

	Ordinal  int     `json:"ordinal"`
	Status   string  `json:"status"`
	Progress float64 `json:"progress"`

	ArtifactID string `json:"artifact_id,omitempty"`

	DurationMS int64 `json:"duration_ms,omitempty"`
	Attempt    int   `json:"attempt,omitempty"`

	Reason string `json:"reason,omitempty"`

	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`

	Metadata map[string]any `json:"metadata,omitempty"`
}

// pipelineView is a project's whole pipeline.
type pipelineView struct {
	ProjectID string      `json:"project_id"`
	Job       *jobView    `json:"job"`
	Stages    []stageView `json:"stages"`
}

// systemOverview is the dashboard's payload.
type systemOverview struct {
	Version  string  `json:"version"`
	Commit   string  `json:"commit"`
	Platform string  `json:"platform"`
	UptimeS  float64 `json:"uptime_s"`

	Counts systemCounts `json:"counts"`
	Worker workerView   `json:"worker"`
	Stats  systemStats  `json:"stats"`
}

type systemCounts struct {
	Projects    int `json:"projects"`
	JobsRunning int `json:"jobs_running"`
	JobsPending int `json:"jobs_pending"`
	Segments    int `json:"segments"`
	NeedsReview int `json:"needs_review"`
}

type workerView struct {
	Status  string `json:"status"`
	Workers int    `json:"workers"`
	Python  string `json:"python"`

	WorkerVersion string `json:"worker_version"`
	SchemaDigest  string `json:"schema_digest"`

	LoadedModels []loadedModelView `json:"loaded_models"`
}

type loadedModelView struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Device   string `json:"device"`
	MemoryMB int64  `json:"memory_mb,omitempty"`
}

// systemStats is the host's vitals.
//
// Every numeric field is a pointer or explicitly nullable, because "unknown" and
// "zero" are different: a machine with no GPU and a machine whose GPU query
// failed should not render identically.
type systemStats struct {
	CPU    cpuStats    `json:"cpu"`
	Memory memoryStats `json:"memory"`
	Disk   diskStats   `json:"disk"`

	// GPU is empty rather than null when there is none, so the dashboard
	// renders one shape everywhere.
	GPU []gpuStats `json:"gpu"`
}

type cpuStats struct {
	Cores        int      `json:"cores"`
	UsagePercent *float64 `json:"usage_percent"`
}

type memoryStats struct {
	TotalBytes *int64 `json:"total_bytes"`
	UsedBytes  *int64 `json:"used_bytes"`
}

type diskStats struct {
	DataFreeBytes   *int64 `json:"data_free_bytes"`
	ModelsFreeBytes *int64 `json:"models_free_bytes"`
}

type gpuStats struct {
	Index              int    `json:"index"`
	Name               string `json:"name"`
	MemoryTotalBytes   *int64 `json:"memory_total_bytes,omitempty"`
	MemoryUsedBytes    *int64 `json:"memory_used_bytes,omitempty"`
	UtilizationPercent *int   `json:"utilization_percent,omitempty"`
	Driver             string `json:"driver,omitempty"`
	CUDA               string `json:"cuda,omitempty"`
}

// qcSummary counts findings by severity.
type qcSummary struct {
	Error   int `json:"error"`
	Warning int `json:"warning"`
	Info    int `json:"info"`
}

// glossaryView is one terminology entry.
type glossaryView struct {
	ID string `json:"id"`

	// ProjectID is null for an entry that applies to every project.
	ProjectID *string `json:"project_id"`

	Source string `json:"source"`
	Target string `json:"target"`
	Type   string `json:"type"`
	Note   string `json:"note,omitempty"`

	Priority int    `json:"priority"`
	Enabled  bool   `json:"enabled"`
	Origin   string `json:"origin"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// modelView is one entry in the model catalog.
type modelView struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Status string `json:"status"`

	SizeBytes      int64      `json:"size_bytes"`
	EstimatedBytes int64      `json:"estimated_size_bytes,omitempty"`
	Progress       float64    `json:"progress"`
	Note           string     `json:"note,omitempty"`
	ErrorMessage   string     `json:"error_message,omitempty"`
	InstalledAt    *time.Time `json:"installed_at,omitempty"`
}

// providerView is a configured provider as the interface sees it.
//
// The API key is absent, not blanked. A field that is always empty invites the
// interface to render it and a user to wonder what they did wrong; a field that
// does not exist cannot be misused. The client sends one only when setting it.
type providerView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	Type string `json:"type"`

	BaseURL string `json:"base_url,omitempty"`
	Model   string `json:"model,omitempty"`

	Enabled bool `json:"enabled"`

	// HasKey reports whether a key is stored, without disclosing it or its
	// length. It is what lets the interface say "configured" rather than
	// leaving the user to guess.
	HasKey bool `json:"has_key"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// providerTestView is the result of checking a provider.
type providerTestView struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Model   string `json:"model,omitempty"`
}

// settingsView is the resolved configuration and where it came from.
//
// Read-only. Settings are edited in the configuration file or the environment,
// and a UI that wrote them would need to decide precedence between three
// sources — a decision the file already records, key by key.
type settingsView struct {
	// Config is the resolved configuration with secrets removed.
	Config map[string]any `json:"config"`

	// Provenance says which layer set each key. It is the answer to "I changed
	// the setting and nothing happened", which is otherwise found by reading
	// four places and guessing.
	Provenance map[string]string `json:"provenance"`

	DataDir string `json:"data_dir"`
}

// logRecordView is one log line.
type logRecordView struct {
	Seq   int64          `json:"seq"`
	Time  time.Time      `json:"time"`
	Level string         `json:"level"`
	Msg   string         `json:"msg"`
	Attrs map[string]any `json:"attrs,omitempty"`
}
