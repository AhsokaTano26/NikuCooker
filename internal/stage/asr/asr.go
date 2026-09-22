// Package asr transcribes speech.
//
// The most expensive stage in the pipeline by a wide margin, and the one whose
// artifact everything downstream depends on. It transcribes only the regions
// the VAD found, which is what makes it tractable at all: a 24-minute episode
// is rarely more than 15 minutes of speech.
package asr

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/AhsokaTano26/NikuCooker/internal/config"
	"github.com/AhsokaTano26/NikuCooker/internal/stage"
	"github.com/AhsokaTano26/NikuCooker/pkg/protocol"
)

// PayloadName is the file this stage writes.
//
// It is the recogniser's own result shape rather than a stage-specific wrapper:
// segmentation consumes exactly this, and a wrapper would be one more thing to
// keep in step with the protocol.
const PayloadName = "asr.json"

// Stage transcribes speech.
type Stage struct{}

// New builds the stage.
func New() *Stage { return &Stage{} }

// Spec describes the stage.
func (s *Stage) Spec() stage.Spec {
	return stage.Spec{
		Name:    "asr",
		Version: "1",
		// The audio artifact gives the file to read; the VAD artifact says
		// which parts of it contain speech.
		Depends:   []string{"audio", "vad"},
		ConfigKey: "asr",
	}
}

// ConfigSubtree returns the recognition settings.
func (s *Stage) ConfigSubtree(cfg *config.Config) any { return cfg.ASR }

// Fingerprint returns nothing, deliberately.
//
// The design note for this stage suggested folding the *resolved device* into
// the cache key. It is not here, and the reason is worth recording: including
// it means that plugging in a GPU silently invalidates every transcript already
// on disk, and re-running would recompute work whose output is, at most,
// marginally different in its timings. The device belongs in the artifact's
// provenance — where it is recorded — not in its identity.
//
// What *does* belong in the key is reaching the stage through ConfigSubtree
// (model, beam size, device choice as configured) and through UpstreamKeys (the
// audio and the speech regions).
func (s *Stage) Fingerprint(context.Context, *stage.Env) (map[string]string, error) {
	return nil, nil
}

// Run transcribes the audio.
func (s *Stage) Run(ctx context.Context, env *stage.Env) (*stage.Result, error) {
	if env.Worker == nil {
		return nil, fmt.Errorf("asr: no AI worker is available")
	}

	audioPath, err := env.InputPath("audio")
	if err != nil {
		return nil, fmt.Errorf("asr: %w", err)
	}

	var regions protocol.VADResult
	if err := env.ReadInput("vad", &regions); err != nil {
		return nil, fmt.Errorf("asr: %w", err)
	}

	cfg := env.Config.ASR

	// The path is resolved from the core's inventory when the model is there,
	// so the worker loads what was downloaded rather than fetching its own copy
	// into a different cache.
	modelPath := ""
	if env.Models != nil {
		if dir, ok := env.Models.Resolve("asr", cfg.Model); ok {
			modelPath = dir
		}
	}

	// Loaded explicitly rather than left to the transcription request. A
	// multi-minute load should not be hidden inside a progress bar, and the
	// answer to "which device is this actually on" must be known before the
	// work starts and before a cache entry is created based on it.
	env.ReportProgress(0, "loading the model")

	loadRaw, err := env.Worker.Call(ctx, protocol.MethodModelLoad, protocol.ModelLoadParams{
		Kind:      "asr",
		Name:      cfg.Model,
		Device:    cfg.Device,
		ModelPath: modelPath,
	}, func(event protocol.ProgressEvent) {
		env.ReportProgress(event.Progress*0.2, event.Message)
	})
	if err != nil {
		return nil, fmt.Errorf("asr: %w", err)
	}

	var loaded protocol.ModelLoadResult
	if err := json.Unmarshal(loadRaw, &loaded); err != nil {
		return nil, fmt.Errorf("asr: decode the model load result: %w", err)
	}

	// A silent fallback to CPU makes a job twenty times slower without saying
	// so, which is a support problem rather than a performance one.
	if loaded.FellBack {
		env.Log.Warn("the model did not load on the requested device",
			"device", loaded.Device, "reason", loaded.FallbackReason)
	}

	// The transcript is written by the worker straight into the artifact's
	// directory, so a two-hour result never travels through the pipe. The
	// summary that comes back is what the metadata is built from.
	resultPath := filepath.Join(env.OutDir, PayloadName)

	env.ReportProgress(0.2, "transcribing")

	raw, err := env.Worker.Call(ctx, protocol.MethodASRTranscribe, protocol.ASRTranscribeParams{
		AudioPath:               audioPath,
		Model:                   cfg.Model,
		Language:                cfg.Language,
		Device:                  cfg.Device,
		BeamSize:                cfg.BeamSize,
		Temperature:             &cfg.Temperature,
		ConditionOnPreviousText: &cfg.ConditionOnPreviousText,
		WordTimestamps:          &cfg.WordTimestamps,
		VADRegions:              regions.Regions,
		ResultPath:              resultPath,
	}, func(event protocol.ProgressEvent) {
		// The worker's progress spans the transcription; the load already
		// consumed the first fifth of the bar.
		env.ReportProgress(0.2+event.Progress*0.8, event.Message)
	})
	if err != nil {
		return nil, fmt.Errorf("asr: %w", err)
	}

	var summary protocol.ASRTranscribeResult
	if err := json.Unmarshal(raw, &summary); err != nil {
		return nil, fmt.Errorf("asr: decode the transcription summary: %w", err)
	}

	// The worker claims the file exists; this checks. A stage that reports
	// success and yields a file that is not there fails much later, somewhere
	// that cannot explain why.
	stat, err := os.Stat(resultPath)
	if err != nil {
		return nil, fmt.Errorf("asr: the worker reported success but wrote no transcript: %w", err)
	}
	if stat.Size() == 0 {
		return nil, fmt.Errorf("asr: the worker wrote an empty transcript")
	}

	if summary.SegmentCount == 0 {
		// Not an error: a file can legitimately contain no speech. It is worth
		// saying so loudly, because every later stage will produce nothing and
		// the user needs to know why now rather than after a render.
		env.Log.Warn("transcription produced no segments; the audio may contain no speech")
	}

	env.ReportProgress(1, "done")

	return &stage.Result{
		Primary:  PayloadName,
		Provider: cfg.Provider,
		Model:    summary.Model,
		Metadata: map[string]any{
			// The device that was actually used, not the one requested. This is
			// the provenance the cache key deliberately omits.
			"device":          summary.Device,
			"compute_type":    summary.ComputeType,
			"fell_back":       loaded.FellBack,
			"fallback_reason": loaded.FallbackReason,

			"language":             summary.Language,
			"language_probability": summary.LanguageProbability,
			"duration_s":           summary.Duration,
			"segment_count":        summary.SegmentCount,
			"word_count":           summary.WordCount,

			"vad_regions": len(regions.Regions),
			"speech_s":    regions.TotalSpeechS,
			"size_bytes":  stat.Size(),
		},
	}, nil
}
