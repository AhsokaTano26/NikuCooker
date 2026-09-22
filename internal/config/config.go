// Package config resolves NikuCooker's configuration.
//
// Precedence, lowest to highest (docs/requirements.md FR-CFG-1):
//
//	defaults → config file → environment → project overlay → CLI flags
//
// Layers are applied by unmarshalling a partial document onto the accumulated
// struct, which sets only the keys a layer actually contains. Every applied key
// is recorded with the layer that set it, so the API can answer "why is my
// setting not taking effect?" — a question that is otherwise answered by
// reading four places and guessing.
package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Config is the fully resolved configuration.
type Config struct {
	Server      Server      `yaml:"server"`
	Storage     Storage     `yaml:"storage"`
	Pipeline    Pipeline    `yaml:"pipeline"`
	AI          AI          `yaml:"ai"`
	Audio       Audio       `yaml:"audio"`
	Media       Media       `yaml:"media"`
	VAD         VAD         `yaml:"vad"`
	ASR         ASR         `yaml:"asr"`
	Subtitle    Subtitle    `yaml:"subtitle"`
	Translation Translation `yaml:"translation"`
	QC          QC          `yaml:"qc"`
	Artifact    Artifact    `yaml:"artifact"`
	Worker      Worker      `yaml:"worker"`
	Render      Render      `yaml:"render"`
	Log         Log         `yaml:"log"`
}

// Server configures the HTTP listener.
type Server struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`

	// AllowPathSource permits creating a project from an arbitrary server-side
	// path. It is off by default: it reads any file the process can reach, which
	// is a meaningful capability to hand out implicitly.
	AllowPathSource bool `yaml:"allow_path_source"`

	// MaxUploadBytes bounds an upload. The limit is enforced while streaming,
	// not after, so a hostile or mistaken upload cannot fill the disk before
	// being rejected.
	MaxUploadBytes int64 `yaml:"max_upload_bytes"`
}

// Storage configures where data lives.
type Storage struct {
	DataDir  string `yaml:"data_dir"`
	ModelDir string `yaml:"model_dir"`
}

// Pipeline selects which stages a run includes.
type Pipeline struct {
	// Disabled names optional stages to leave out.
	//
	// Only optional stages may appear here: disabling a required stage would
	// leave its dependents without an input, and the pipeline reports that as a
	// configuration error rather than running a partial chain.
	Disabled []string `yaml:"disabled"`
}

// Media configures the FFmpeg integration.
type Media struct {
	// FFmpegPath and FFprobePath point at the binaries. Empty means resolve
	// them from PATH, which is right for a normal install and wrong for a
	// bundled build or a machine with several FFmpeg builds on it.
	FFmpegPath  string `yaml:"ffmpeg_path"`
	FFprobePath string `yaml:"ffprobe_path"`

	// Timeout bounds a single FFmpeg invocation, as a duration string.
	//
	// Zero means no limit, which is the default and the deliberate choice: a
	// burn-in of a feature-length film on a CPU encoder is legitimately an hour
	// of work, and a timeout that fires on a slow machine turns a slow render
	// into a failed one. The probe is bounded separately and internally,
	// because a probe that has not answered in a minute is not going to.
	Timeout time.Duration `yaml:"timeout"`
}

// AI configures the Python worker runtime.
type AI struct {
	// Python is an explicit interpreter path, or "auto" to resolve one.
	Python string `yaml:"python"`

	// Dir is the directory containing the nikucooker_ai package. Empty means
	// resolve it: alongside the executable first, then the working directory.
	Dir string `yaml:"dir"`

	// Args are the arguments the worker is launched with. Almost nobody should
	// change this; it exists because a bundled build may need to launch the
	// worker differently from a checkout.
	Args []string `yaml:"args"`
	// PythonVersion is the accepted interpreter range. It is a range rather
	// than a minimum because a too-new Python is as unusable as a too-old one:
	// the AI dependency set has a ceiling set by CTranslate2's wheel lag.
	PythonVersion string `yaml:"python_version"`
}

// Audio configures extraction and preprocessing.
type Audio struct {
	// SampleRate is fixed at 16 kHz because that is what Whisper consumes.
	SampleRate int `yaml:"sample_rate"`

	// HighpassHz removes rumble below the speech band.
	HighpassHz int `yaml:"highpass_hz"`

	// Loudnorm applies EBU R128 loudness normalisation. Off by default: it
	// changes the signal the model sees, and the benefit is material-dependent.
	Loudnorm bool `yaml:"loudnorm"`
}

// VAD configures speech detection.
//
// The thresholds are exposed because the right values differ by material: an
// anime episode with a continuous music bed needs a different silence threshold
// from a talk show recorded in a quiet studio.
type VAD struct {
	Enabled      bool    `yaml:"enabled"`
	Threshold    float64 `yaml:"threshold"`
	MinSpeechMS  int     `yaml:"min_speech_ms"`
	MinSilenceMS int     `yaml:"min_silence_ms"`
	SpeechPadMS  int     `yaml:"speech_pad_ms"`
	MaxSpeechS   float64 `yaml:"max_speech_s"`
}

// ASR configures speech recognition.
type ASR struct {
	Provider    string `yaml:"provider"`
	Model       string `yaml:"model"`
	Device      string `yaml:"device"` // auto | cpu | cuda
	ComputeType string `yaml:"compute_type"`

	// Language is a BCP-47 tag, or "" to detect it.
	Language string `yaml:"language"`

	BeamSize                int     `yaml:"beam_size"`
	Temperature             float64 `yaml:"temperature"`
	ConditionOnPreviousText bool    `yaml:"condition_on_previous_text"`
	WordTimestamps          bool    `yaml:"word_timestamps"`
}

// Subtitle configures segmentation and output.
type Subtitle struct {
	MinDuration float64 `yaml:"min_duration"`
	MaxDuration float64 `yaml:"max_duration"`

	// MaxCPS is the reading-speed ceiling in characters per second. It is
	// checked against the *translated* text once a translation exists, because
	// Chinese line length is what determines readability, and it is not known
	// when the line is first split.
	MaxCPS float64 `yaml:"max_cps"`

	// MaxCharsZH and MaxCharsJA bound line length per target language.
	MaxCharsZH int `yaml:"max_chars_zh"`
	MaxCharsJA int `yaml:"max_chars_ja"`

	// PauseMS splits a line where the gap between words exceeds this.
	PauseMS int `yaml:"pause_ms"`

	// MinGap is the blank interval left between two consecutive lines, in
	// seconds.
	//
	// Zero would read as a single line whose text changed; a couple of frames is
	// enough for the eye to register two. It is also the margin a line may not
	// borrow when it is given more time to be read.
	MinGap float64 `yaml:"min_gap"`

	Bilingual bool     `yaml:"bilingual"`
	Formats   []string `yaml:"formats"`
	Preset    string   `yaml:"preset"`
}

// Translation configures the LLM translation stage.
type Translation struct {
	// Provider names a configured provider entry. BaseURL, APIKey and Model here
	// are a fallback for a single-provider setup that has not been added to the
	// database.
	Provider string `yaml:"provider"`
	BaseURL  string `yaml:"base_url"`
	APIKey   string `yaml:"api_key"`
	Model    string `yaml:"model"`

	Temperature float64       `yaml:"temperature"`
	Timeout     time.Duration `yaml:"timeout"`
	MaxRetries  int           `yaml:"max_retries"`

	// BatchSize is the number of lines per LLM request. Batching matters more
	// than any other single parameter here: one request per line costs an order
	// of magnitude more tokens and loses all cross-line context.
	BatchSize int `yaml:"batch_size"`

	// ContextLines is how many neighbouring lines travel with each batch as
	// read-only context.
	ContextLines int `yaml:"context_lines"`

	// Concurrency bounds simultaneous in-flight batches. It exists to avoid
	// tripping provider rate limits, not to bound local resource use.
	Concurrency int `yaml:"concurrency"`

	Style string `yaml:"style"` // literal | natural | fansub

	// MaxTokensPerJob is a hard ceiling. It stops a job rather than allowing an
	// unbounded spend, which is the only way a user can be sure of the cost.
	MaxTokensPerJob int `yaml:"max_tokens_per_job"`

	CacheMaxEntries int `yaml:"cache_max_entries"`
}

// QC configures quality control.
type QC struct {
	Enabled bool `yaml:"enabled"`

	// LLM enables the semantic pass. Off by default and not implemented in v1:
	// it is probabilistic, costs a full second pass, and duplicates what human
	// review already does. See docs/risks.md §1.3.
	LLM bool `yaml:"llm"`

	MaxCPS          float64 `yaml:"max_cps"`
	MaxDuration     float64 `yaml:"max_duration"`
	MinDuration     float64 `yaml:"min_duration"`
	MaxChars        int     `yaml:"max_chars"`
	DuplicateWindow int     `yaml:"duplicate_window"`
}

// Artifact configures caching and retention.
type Artifact struct {
	// Retention is how many superseded versions of a stage's output to keep.
	// -1 keeps everything.
	Retention int `yaml:"retention"`

	// CacheInDev disables the stage cache in development builds. Without it a
	// developer debugging a stage runs against a stale artifact produced by the
	// previous build, because code_revision is "dev" and never changes.
	CacheInDev bool `yaml:"cache_in_dev"`

	// KeepAudio retains audio.wav after ASR. It dominates the artifact
	// directory — roughly 230 MB for a two-hour video — and re-extracting it
	// costs one FFmpeg pass over the source.
	KeepAudio bool `yaml:"keep_audio"`

	// HashMode is "sampled" (three offsets) or "full". Sampled is
	// collision-resistant for the edits that actually happen; full is for users
	// who want certainty and will pay O(file size) for it.
	HashMode string `yaml:"hash_mode"`
}

// Worker configures the Python worker pool.
type Worker struct {
	// PoolSize is the number of worker processes. Each holds its own copy of a
	// loaded model, so raising this costs memory proportional to model size.
	PoolSize int `yaml:"pool_size"`

	StartupTimeout   time.Duration `yaml:"startup_timeout"`
	ShutdownTimeout  time.Duration `yaml:"shutdown_timeout"`
	ModelLoadTimeout time.Duration `yaml:"model_load_timeout"`

	// StallTimeout fails a request that has produced no progress. Absolute
	// timeouts are wrong for this workload — a three-hour stream on CPU may
	// legitimately take an hour — so liveness is measured by progress instead.
	StallTimeout time.Duration `yaml:"stall_timeout"`
}

// Render configures video output.
type Render struct {
	Mode string `yaml:"mode"` // soft | hard

	// Encoder is chosen from detected capabilities when empty: videotoolbox on
	// Apple Silicon, nvenc with CUDA, libx264 otherwise.
	Encoder      string `yaml:"encoder"`
	CRF          int    `yaml:"crf"`
	Preset       string `yaml:"preset"`
	AudioBitrate string `yaml:"audio_bitrate"`
}

// Log configures logging.
type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"` // text | json
}

// Default returns the configuration a fresh install runs with.
//
// These are the values a user gets by changing nothing, so they are chosen for
// a first run succeeding rather than for peak quality. Where the two conflict,
// the comment says which and why.
func Default() *Config {
	return &Config{
		Server: Server{
			Host: "127.0.0.1",
			Port: 8080,
			// Loopback by default: this build has no authentication and reads
			// local files.
			AllowPathSource: false,
			MaxUploadBytes:  20 << 30, // 20 GiB
		},
		Storage: Storage{
			DataDir:  "./data",
			ModelDir: "./models",
		},
		Pipeline: Pipeline{
			// polish is off because it is a second full LLM pass over every
			// line, roughly doubling translation cost for a benefit largely
			// achievable inside the translation prompt itself. It is worth
			// enabling for high-value material a human will review anyway.
			//
			// render stays on: a user who asked for a video expects a video.
			Disabled: []string{"polish"},
		},
		AI: AI{
			Python: "auto",
			// The worker is run as a module, which is what keeps its import
			// path independent of the working directory.
			Args: []string{"-m", "nikucooker_ai"},
			// Verified against real wheels on all four platform targets; the
			// ceiling is CTranslate2's. See docs/dependency-audit.md §6.
			PythonVersion: ">=3.12,<3.15",
		},
		Audio: Audio{
			SampleRate: 16000,
			HighpassHz: 60,
			Loudnorm:   false,
		},
		Media: Media{
			// No timeout by default. See the field's note: a legitimate render
			// of a long film is slower than any fixed limit that would also
			// catch a hang.
			Timeout: 0,
		},
		VAD: VAD{
			Enabled:      true,
			Threshold:    0.5,
			MinSpeechMS:  250,
			MinSilenceMS: 100,
			SpeechPadMS:  30,
			MaxSpeechS:   30,
		},
		ASR: ASR{
			Provider: "faster-whisper",
			// medium rather than large-v3: on CPU — which is what Apple Silicon
			// and any GPU-less machine are — large-v3 runs at roughly 2–3x
			// realtime, and a user who waits 50 minutes for a 24-minute episode
			// concludes the tool is broken. See docs/risks.md R1.
			Model:       "medium",
			Device:      "auto",
			ComputeType: "auto",
			Language:    "",
			BeamSize:    5,
			Temperature: 0,
			// Off by default: conditioning on previous text improves coherence
			// but makes Whisper loop on repetitions, which is common in the
			// music and cheering of live material.
			ConditionOnPreviousText: false,
			WordTimestamps:          true,
		},
		Subtitle: Subtitle{
			MinDuration: 1.0,
			MaxDuration: 7.0,
			MaxCPS:      18,
			MaxCharsZH:  24,
			MaxCharsJA:  32,
			PauseMS:     300,
			// Two frames at 24fps, the smallest gap that reads as two lines
			// rather than one that changed its text.
			MinGap:    0.083,
			Bilingual: false,
			Formats:   []string{"srt", "ass"},
			Preset:    "Fansub",
		},
		Translation: Translation{
			Provider:     "",
			Temperature:  0.2,
			Timeout:      120 * time.Second,
			MaxRetries:   3,
			BatchSize:    20,
			ContextLines: 2,
			Concurrency:  2,
			Style:        "fansub",
			// 0 means unlimited. A default ceiling would be a guess that
			// silently truncates a legitimate long series.
			MaxTokensPerJob: 0,
			CacheMaxEntries: 500_000,
		},
		QC: QC{
			Enabled:         true,
			LLM:             false,
			MaxCPS:          20,
			MaxDuration:     8.0,
			MinDuration:     0.8,
			MaxChars:        28,
			DuplicateWindow: 3,
		},
		Artifact: Artifact{
			Retention:  3,
			CacheInDev: true,
			KeepAudio:  true,
			HashMode:   "sampled",
		},
		Worker: Worker{
			// One worker: a second copy of large-v3 costs ~1.2 GB, and a
			// surprise allocation is worse than waiting.
			PoolSize:         1,
			StartupTimeout:   60 * time.Second,
			ShutdownTimeout:  10 * time.Second,
			ModelLoadTimeout: 600 * time.Second,
			StallTimeout:     120 * time.Second,
		},
		Render: Render{
			Mode:         "soft",
			Encoder:      "",
			CRF:          18,
			Preset:       "medium",
			AudioBitrate: "192k",
		},
		Log: Log{
			Level:  "info",
			Format: "text",
		},
	}
}

// ResolvePaths makes the configured directories absolute.
//
// A relative path is a perfectly reasonable thing to write — `./data` next to
// the binary — but it is only meaningful next to the working directory it was
// written for. That directory belongs to this process, and the paths do not
// stay here: they are handed to the Python worker, which runs with its own
// working directory, and to FFmpeg. A relative path that crosses either
// boundary resolves against something else and lands on a file that is not
// there.
//
// Resolved once, at load, so that everything downstream — artifacts, models,
// the database, the upload staging area — derives an absolute path without
// having to remember to.
func (c *Config) ResolvePaths() error {
	absolute := func(key, value string) (string, error) {
		if value == "" || filepath.IsAbs(value) {
			return value, nil
		}

		resolved, err := filepath.Abs(value)
		if err != nil {
			// Only reachable when the working directory cannot be read, which
			// is worth saying plainly: the fix is a different cwd rather than a
			// different configuration.
			return "", fmt.Errorf("%s: cannot resolve %q against the working directory: %w", key, value, err)
		}
		return resolved, nil
	}

	dataDir, err := absolute("storage.data_dir", c.Storage.DataDir)
	if err != nil {
		return err
	}
	c.Storage.DataDir = dataDir

	// Model paths are passed to the worker by name, which the worker resolves
	// under the directory it is given — so this one crosses the same boundary
	// and needs the same treatment.
	modelDir, err := absolute("storage.model_dir", c.Storage.ModelDir)
	if err != nil {
		return err
	}
	c.Storage.ModelDir = modelDir

	return nil
}

// Validate rejects a configuration that would fail later and less clearly.
//
// Every message names the offending key and the expected form: a startup error
// is the cheapest place to fix a mistake, and only if it says what to fix.
func (c *Config) Validate() error {
	var problems []string

	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if c.Server.Port < 1 || c.Server.Port > 65535 {
		add("server.port: %d is not a valid port (expected 1–65535)", c.Server.Port)
	}
	if c.Server.MaxUploadBytes <= 0 {
		add("server.max_upload_bytes: must be positive, got %d", c.Server.MaxUploadBytes)
	}
	if c.Storage.DataDir == "" {
		add("storage.data_dir: must not be empty")
	}
	if c.Storage.ModelDir == "" {
		add("storage.model_dir: must not be empty")
	}

	switch c.AI.Python {
	case "", "auto":
	default:
		// An explicit path is validated when it is used, not here: the file may
		// live on a volume that mounts after the core starts.
	}

	switch c.ASR.Device {
	case "auto", "cpu", "cuda":
	default:
		add("asr.device: %q is not one of auto, cpu, cuda", c.ASR.Device)
	}
	if c.ASR.BeamSize < 1 {
		add("asr.beam_size: must be at least 1, got %d", c.ASR.BeamSize)
	}

	switch c.Translation.Style {
	case "literal", "natural", "fansub":
	default:
		add("translation.style: %q is not one of literal, natural, fansub", c.Translation.Style)
	}
	if c.Translation.BatchSize < 1 {
		add("translation.batch_size: must be at least 1, got %d", c.Translation.BatchSize)
	}
	if c.Translation.Concurrency < 1 {
		add("translation.concurrency: must be at least 1, got %d", c.Translation.Concurrency)
	}
	if c.Translation.Timeout <= 0 {
		add("translation.timeout: must be positive, got %s", c.Translation.Timeout)
	}
	if c.Translation.MaxRetries < 0 {
		add("translation.max_retries: must not be negative, got %d", c.Translation.MaxRetries)
	}

	if c.Subtitle.MinDuration <= 0 {
		add("subtitle.min_duration: must be positive, got %v", c.Subtitle.MinDuration)
	}
	if c.Subtitle.MaxDuration <= c.Subtitle.MinDuration {
		add("subtitle.max_duration: %v must exceed min_duration %v",
			c.Subtitle.MaxDuration, c.Subtitle.MinDuration)
	}
	if c.Subtitle.MaxCPS <= 0 {
		add("subtitle.max_cps: must be positive, got %v", c.Subtitle.MaxCPS)
	}
	for _, f := range c.Subtitle.Formats {
		switch f {
		case "srt", "ass", "vtt":
		default:
			add("subtitle.formats: %q is not one of srt, ass, vtt", f)
		}
	}

	switch c.Artifact.HashMode {
	case "sampled", "full":
	default:
		add("artifact.hash_mode: %q is not one of sampled, full", c.Artifact.HashMode)
	}
	if c.Artifact.Retention < -1 {
		add("artifact.retention: must be -1 (keep all) or non-negative, got %d", c.Artifact.Retention)
	}

	if c.Worker.PoolSize < 1 {
		add("worker.pool_size: must be at least 1, got %d", c.Worker.PoolSize)
	}

	switch c.Render.Mode {
	case "soft", "hard":
	default:
		add("render.mode: %q is not one of soft, hard", c.Render.Mode)
	}
	if c.Render.CRF < 0 || c.Render.CRF > 51 {
		add("render.crf: %d is outside 0–51", c.Render.CRF)
	}

	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		add("log.level: %q is not one of debug, info, warn, error", c.Log.Level)
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		add("log.format: %q is not one of text, json", c.Log.Format)
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
}
