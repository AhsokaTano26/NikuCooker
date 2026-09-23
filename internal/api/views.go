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

	// SizeBytes is what the project occupies on disk. Measured when the view is
	// built rather than stored, because the number that matters is the one now
	// — and a cached size is wrong exactly after the cleanup someone is
	// checking it for.
	SizeBytes int64 `json:"size_bytes"`

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

	Counts  systemCounts `json:"counts"`
	Worker  workerView   `json:"worker"`
	Runtime runtimeView  `json:"runtime"`
	Stats   systemStats  `json:"stats"`

	Features systemFeatures `json:"features"`
}

// runtimeView is the state of the provisioned Python environment.
//
// It is on the system overview rather than behind its own endpoint because the
// System page already loads that and refetches when the stream resyncs, so a
// tab opened halfway through an install finds the state without anyone having
// to poll for it.
type runtimeView struct {
	// Available is whether this installation can install an environment at all.
	//
	// Read before the button is offered. A capability that is off is a refusal
	// waiting to happen, and a button that fails is worse than a sentence
	// saying why it was not offered.
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`

	// Status is idle, running, ready, failed or cancelled.
	Status string `json:"status"`

	// Phase is which step of the install is running or failed.
	Phase string `json:"phase,omitempty"`

	// Provisioned is whether an interpreter exists right now, which is not the
	// same question as whether this process has installed one.
	Provisioned bool   `json:"provisioned"`
	Python      string `json:"python,omitempty"`
	RuntimeDir  string `json:"runtime_dir"`
	UV          string `json:"uv,omitempty"`

	// Accelerator is which optional dependency set that interpreter was built
	// with: "cuda" or "cpu". Empty when there is no record of the install —
	// an environment this program did not build, such as a checkout's own —
	// which is a different answer from "the CPU one" and is rendered as
	// neither rather than as a guess.
	Accelerator string `json:"accelerator,omitempty"`

	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	Remediation  string `json:"remediation,omitempty"`

	// CUDA is whether the install can be asked for GPU acceleration here.
	CUDA cudaView `json:"cuda"`

	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// cudaView is the GPU option, offered or refused before it is chosen.
//
// It hangs off the runtime rather than the host section because it is a
// property of the install: the same question decides which button the System
// page shows, and the answer has to be the same one the provision request
// enforces. The host section reports what the machine has; this reports what
// the environment can be built with.
type cudaView struct {
	// Available is whether the CUDA set can be installed and used here.
	Available bool `json:"available"`

	// ReasonCode says why not, when it cannot: "platform" where the wheels do
	// not exist for this system, "no_gpu" where there is nothing to accelerate.
	//
	// A code rather than a sentence because the interface renders it to someone
	// reading Chinese, and it renders it on most machines rather than only on
	// failures — see provision.CUDAUnavailable.
	ReasonCode string `json:"reason_code,omitempty"`

	// GPUs names the accelerators that were found, so that "yes" is checkable:
	// a person who reads their own card's name back knows the detection worked,
	// and one who does not see it knows why the option is missing.
	GPUs []string `json:"gpus"`

	// ExtraBytes is what choosing it adds to the download, so the cost is
	// stated before the choice rather than discovered by watching it.
	ExtraBytes int64 `json:"extra_bytes"`
}

// systemFeatures reports what this installation is configured to do.
//
// The interface reads it to describe itself honestly before a user commits to a
// course of action. A capability that is off is a refusal waiting to happen, and
// a form that only says so after it has been filled in wastes the filling in —
// which is exactly what the new-project page did while allow_path_source was
// false by default.
type systemFeatures struct {
	// PathSource is whether a project can be created from a server-side path.
	PathSource bool `json:"path_source"`

	// MaxUploadBytes is the largest file this server will accept. Sent so the
	// interface can refuse a file it already knows is too large, rather than
	// streaming twenty gigabytes in order to be told so.
	MaxUploadBytes int64 `json:"max_upload_bytes"`

	// ConfigPath is the configuration file this process reads, and whether it
	// is there. Reported even when absent: "no file, and here is where one
	// goes" is what a user needs in order to change a setting.
	ConfigPath       string `json:"config_path"`
	ConfigFileExists bool   `json:"config_file_exists"`

	// Catalog is every setting the interface may change, each with its value in
	// effect and the layer that set it. A key absent from it is readable here
	// and editable only in the configuration file.
	Catalog []settingView `json:"catalog"`
}

type systemCounts struct {
	Projects    int `json:"projects"`
	JobsRunning int `json:"jobs_running"`
	JobsPending int `json:"jobs_pending"`
	Segments    int `json:"segments"`
	NeedsReview int `json:"needs_review"`
}

type workerView struct {
	// Status is the startup check's answer: starting, ready, missing or failed.
	//
	// It describes whether this installation can transcribe, not whether a
	// process happens to be running — the pool is lazy, so "no worker process"
	// is the normal state and says nothing a user needs.
	Status  string `json:"status"`
	Workers int    `json:"workers"`
	Python  string `json:"python"`

	// Detail is why, when it is not ready, in the search's or the interpreter's
	// own words.
	Detail string `json:"detail,omitempty"`

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

	// SizeBytes is what the project occupies on disk. Measured when the view is
	// built rather than stored, because the number that matters is the one now
	// — and a cached size is wrong exactly after the cleanup someone is
	// checking it for.
	SizeBytes int64 `json:"size_bytes"`

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
	Recommendation string     `json:"recommendation,omitempty"`
	Accuracy       string     `json:"accuracy,omitempty"`
	Speed          string     `json:"speed,omitempty"`
	Hardware       string     `json:"hardware,omitempty"`
	Language       string     `json:"language,omitempty"`
	Tags           []string   `json:"tags,omitempty"`
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

	// SizeBytes is what the project occupies on disk. Measured when the view is
	// built rather than stored, because the number that matters is the one now
	// — and a cached size is wrong exactly after the cleanup someone is
	// checking it for.
	SizeBytes int64 `json:"size_bytes"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// providerTestView is the result of checking a provider.
type providerTestView struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Model   string `json:"model,omitempty"`

	// Warning is something true about the provider that is not a failure.
	// Separate from Message rather than folded into it, because a client that
	// renders the two the same way would show a working provider in the same
	// red as a broken one.
	Warning string `json:"warning,omitempty"`
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

	// DataDir and ConfigPath are the two paths a user needs in order to change
	// something: one to find their work, one to edit a setting. Reported
	// together because "the setting did not take" is answered by the second.
	DataDir string `json:"data_dir"`

	// ConfigPath is the file this process reads, present or not. An absent file
	// is not an error — the defaults are complete — but it does mean there is
	// nowhere to write a change, and the interface has to be able to say so.
	ConfigPath       string `json:"config_path"`
	ConfigFileExists bool   `json:"config_file_exists"`

	// Catalog is every setting the interface may change, each with its value in
	// effect and the layer that set it. A key absent from it is readable here
	// and editable only in the configuration file.
	Catalog []settingView `json:"catalog"`
}

// logRecordView is one log line.
type logRecordView struct {
	Seq   int64          `json:"seq"`
	Time  time.Time      `json:"time"`
	Level string         `json:"level"`
	Msg   string         `json:"msg"`
	Attrs map[string]any `json:"attrs,omitempty"`
}
