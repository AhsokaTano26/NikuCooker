package protocol

// Method names.
//
// Control methods are answered immediately, even while a data request is in
// flight. Data methods are serialised per worker: a GPU — and even a CPU
// running CTranslate2 — is a single resource, and concurrent inference on one
// process buys nothing while making memory unpredictable.
const (
	// Control methods.

	// MethodEcho round-trips its params unchanged. It exists so that framing,
	// buffering and flushing can be tested without a model.
	MethodEcho = "echo"
	// MethodHealth reports liveness and resident state.
	MethodHealth = "health"
	// MethodCapabilities reports what this host can actually run.
	MethodCapabilities = "capabilities"
	// MethodCancel stops an in-flight request.
	MethodCancel = "cancel"
	// MethodShutdown releases models and exits.
	MethodShutdown = "shutdown"

	// Data methods.

	// MethodModelLoad makes a model resident and reports the device it landed
	// on. Loading is explicit rather than implicit inside transcription so that
	// a multi-minute load is not hidden inside a progress bar, and so that
	// "which device did it use" is answerable before the work starts.
	MethodModelLoad = "model.load"
	// MethodModelUnload releases a resident model.
	MethodModelUnload = "model.unload"
	// MethodModelListLoaded lists resident models.
	MethodModelListLoaded = "model.list_loaded"

	// MethodVADDetect finds speech regions in an audio file.
	MethodVADDetect = "vad.detect"
	// MethodASRTranscribe transcribes an audio file.
	MethodASRTranscribe = "asr.transcribe"
)

// IsControlMethod reports whether a method bypasses the data queue.
//
// Cancel is the reason this distinction exists: a cancel that queued behind the
// transcription it is meant to stop would be useless.
func IsControlMethod(method string) bool {
	switch method {
	case MethodEcho, MethodHealth, MethodCapabilities, MethodCancel, MethodShutdown:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// echo
// ---------------------------------------------------------------------------

// EchoParams is returned verbatim. Its fields are deliberately unconstrained so
// that framing tests can push arbitrary payloads through.
type EchoParams map[string]any

// ---------------------------------------------------------------------------
// health
// ---------------------------------------------------------------------------

// HealthResult reports worker liveness and resident state.
type HealthResult struct {
	Alive        bool          `json:"alive"`
	UptimeS      float64       `json:"uptime_s"`
	LoadedModels []LoadedModel `json:"loaded_models"`
	Inflight     []string      `json:"inflight"`
	RSSMB        float64       `json:"rss_mb,omitempty"`
}

// ---------------------------------------------------------------------------
// capabilities
// ---------------------------------------------------------------------------

// CapabilitiesResult is the worker's view of what this host can run.
//
// The core merges it with its own host probe for the /system/capabilities
// endpoint. It is the worker, not the core, that knows whether CTranslate2 can
// actually open a CUDA device.
type CapabilitiesResult struct {
	Device DeviceInfo `json:"device"`
	ASR    ASRCaps    `json:"asr"`
	VAD    Providers  `json:"vad"`
	Notes  []string   `json:"notes,omitempty"`
}

// DeviceInfo describes the selected compute device and what else is present.
type DeviceInfo struct {
	Selected string `json:"selected"`
	CUDA     bool   `json:"cuda"`
	MPS      bool   `json:"mps"`
	CPUCount int    `json:"cpu_count"`
}

// ASRCaps lists the available recognition backends and compute types.
type ASRCaps struct {
	Providers    []string `json:"providers"`
	ComputeTypes []string `json:"compute_types"`
}

// ---------------------------------------------------------------------------
// cancel
// ---------------------------------------------------------------------------

// CancelParams names the request to stop.
type CancelParams struct {
	TargetID string `json:"target_id"`
}

// CancelResult acknowledges a cancellation request.
//
// Cancelled is false when the target had already finished. That is not an
// error: cancellation is racy by nature, and losing the race is a normal
// outcome rather than a failure.
type CancelResult struct {
	Cancelled bool `json:"cancelled"`
}

// ---------------------------------------------------------------------------
// shutdown
// ---------------------------------------------------------------------------

// ShutdownResult acknowledges a shutdown request.
type ShutdownResult struct {
	OK bool `json:"ok"`
}

// ---------------------------------------------------------------------------
// model.load / model.unload / model.list_loaded
// ---------------------------------------------------------------------------

// ModelLoadParams requests that a model be made resident.
//
// Device accepts "auto", "cpu" or "cuda". "auto" prefers an accelerator and
// falls back, recording why in the result.
type ModelLoadParams struct {
	Kind        string `json:"kind"` // "asr" or "vad"
	Name        string `json:"name"` // e.g. "large-v3"
	Device      string `json:"device,omitempty"`
	ComputeType string `json:"compute_type,omitempty"`
	ModelPath   string `json:"model_path,omitempty"`
}

// ModelLoadResult reports where the model actually landed.
//
// FellBack and FallbackReason are the honesty mechanism: a silent fallback to
// CPU makes a job twenty times slower without saying so, which is a support
// problem rather than a performance one.
type ModelLoadResult struct {
	Loaded         bool   `json:"loaded"`
	Device         string `json:"device"`
	ComputeType    string `json:"compute_type,omitempty"`
	LoadMS         int64  `json:"load_ms"`
	FellBack       bool   `json:"fell_back,omitempty"`
	FallbackReason string `json:"fallback_reason,omitempty"`
}

// ModelUnloadParams names the model to release.
type ModelUnloadParams struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// ModelUnloadResult reports what was released.
type ModelUnloadResult struct {
	Unloaded bool  `json:"unloaded"`
	FreedMB  int64 `json:"freed_mb,omitempty"`
}

// ModelListLoadedResult lists resident models.
type ModelListLoadedResult struct {
	Models []LoadedModel `json:"models"`
}

// ---------------------------------------------------------------------------
// vad.detect
// ---------------------------------------------------------------------------

// VADDetectParams configures speech detection.
//
// The thresholds are exposed because the right values differ by material:
// an anime episode with a music bed needs a different silence threshold from a
// talk show recorded in a quiet studio.
type VADDetectParams struct {
	AudioPath    string  `json:"audio_path"`
	Threshold    float64 `json:"threshold,omitempty"`
	MinSpeechMS  int     `json:"min_speech_ms,omitempty"`
	MinSilenceMS int     `json:"min_silence_ms,omitempty"`
	SpeechPadMS  int     `json:"speech_pad_ms,omitempty"`
	MaxSpeechS   float64 `json:"max_speech_s,omitempty"`
}

// ---------------------------------------------------------------------------
// asr.transcribe
// ---------------------------------------------------------------------------

// ASRTranscribeParams configures transcription.
//
// ResultPath is the escape hatch for large results: when set, the worker writes
// the full ASRResult to that file and returns only a summary. This keeps pipe
// traffic bounded regardless of media length. When empty, the full payload is
// returned inline — which is what tests and short clips use.
type ASRTranscribeParams struct {
	AudioPath string `json:"audio_path"`
	Model     string `json:"model"`
	Language  string `json:"language,omitempty"`
	Device    string `json:"device,omitempty"`

	BeamSize                int      `json:"beam_size,omitempty"`
	Temperature             *float64 `json:"temperature,omitempty"`
	ConditionOnPreviousText *bool    `json:"condition_on_previous_text,omitempty"`
	WordTimestamps          *bool    `json:"word_timestamps,omitempty"`
	InitialPrompt           *string  `json:"initial_prompt,omitempty"`

	// VADRegions restricts recognition to detected speech. When absent the
	// worker transcribes the whole file.
	VADRegions []VADRegion `json:"vad_regions,omitempty"`

	ResultPath string `json:"result_path,omitempty"`
}

// ASRTranscribeResult summarises a transcription.
//
// SegmentsInline is populated only when the request omitted ResultPath. The two
// forms are mutually exclusive, and a caller that reads neither is a caller
// that silently produces an empty transcript.
type ASRTranscribeResult struct {
	ResultPath          string       `json:"result_path,omitempty"`
	SegmentsInline      []ASRSegment `json:"segments,omitempty"`
	Language            string       `json:"language"`
	LanguageProbability *float64     `json:"language_probability,omitempty"`
	Duration            float64      `json:"duration"`
	SegmentCount        int          `json:"segment_count"`
	WordCount           int          `json:"word_count"`
	Device              string       `json:"device"`
	ComputeType         string       `json:"compute_type,omitempty"`
	Model               string       `json:"model"`
}
