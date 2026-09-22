// Package vad detects where speech is.
//
// Recognition is expensive and most of a video is not speech — music, silence,
// applause, a title card. Transcribing only the speech regions is the single
// largest saving available in the pipeline, and it also improves quality: the
// model hallucinates freely when handed long stretches of non-speech.
package vad

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/AhsokaTano26/NikuCooker/internal/config"
	"github.com/AhsokaTano26/NikuCooker/internal/media"
	"github.com/AhsokaTano26/NikuCooker/internal/stage"
	"github.com/AhsokaTano26/NikuCooker/pkg/protocol"
)

// PayloadName is the file this stage writes.
const PayloadName = "vad.json"

// Stage detects speech regions.
type Stage struct{}

// New builds the stage.
func New() *Stage { return &Stage{} }

// Spec describes the stage.
func (s *Stage) Spec() stage.Spec {
	return stage.Spec{
		Name:      "vad",
		Version:   "1",
		Depends:   []string{"audio"},
		ConfigKey: "vad",
	}
}

// ConfigSubtree returns the VAD settings.
func (s *Stage) ConfigSubtree(cfg *config.Config) any { return cfg.VAD }

// Fingerprint returns nothing: the thresholds are configuration and the audio
// is an upstream artifact, so both are already in the key.
func (s *Stage) Fingerprint(context.Context, *stage.Env) (map[string]string, error) {
	return nil, nil
}

// Run detects speech.
func (s *Stage) Run(ctx context.Context, env *stage.Env) (*stage.Result, error) {
	if env.Worker == nil {
		return nil, fmt.Errorf("vad: no AI worker is available")
	}
	if !env.Config.VAD.Enabled {
		// Disabled is not the same as failed: the pipeline still needs an
		// artifact for the next stage, and one covering the whole file is the
		// correct answer for "do not use VAD".
		return s.writeWholeFile(ctx, env)
	}

	audioPath, err := audioPathOf(env)
	if err != nil {
		return nil, err
	}

	params := protocol.VADDetectParams{
		AudioPath:    audioPath,
		Threshold:    env.Config.VAD.Threshold,
		MinSpeechMS:  env.Config.VAD.MinSpeechMS,
		MinSilenceMS: env.Config.VAD.MinSilenceMS,
		SpeechPadMS:  env.Config.VAD.SpeechPadMS,
		MaxSpeechS:   env.Config.VAD.MaxSpeechS,
	}

	raw, err := env.Worker.Call(ctx, protocol.MethodVADDetect, params,
		func(event protocol.ProgressEvent) {
			env.ReportProgress(event.Progress, event.Message)
		})
	if err != nil {
		return nil, fmt.Errorf("vad: %w", err)
	}

	var result protocol.VADResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("vad: decode the worker's result: %w", err)
	}

	// The worker sanitises its own output, and this checks it anyway: a
	// violated invariant here becomes impossible subtitle timings much later,
	// where the cause is no longer visible, and rejecting it now costs a
	// comparison.
	if err := validate(result.Regions, result.TotalAudioS); err != nil {
		return nil, fmt.Errorf("vad: the worker returned invalid regions: %w", err)
	}

	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("vad: encode: %w", err)
	}
	payload = append(payload, '\n')

	if err := os.WriteFile(filepath.Join(env.OutDir, PayloadName), payload, 0o644); err != nil {
		return nil, fmt.Errorf("vad: write %s: %w", PayloadName, err)
	}

	speechRatio := 0.0
	if result.TotalAudioS > 0 {
		speechRatio = result.TotalSpeechS / result.TotalAudioS
	}

	env.ReportProgress(1, "done")

	return &stage.Result{
		Primary: PayloadName,
		Metadata: map[string]any{
			"region_count":   len(result.Regions),
			"total_speech_s": result.TotalSpeechS,
			"total_audio_s":  result.TotalAudioS,
			// The ratio is what makes "why is this job taking so long"
			// answerable: a talk show is mostly speech, an anime episode is not.
			"speech_ratio": speechRatio,
			"threshold":    env.Config.VAD.Threshold,
		},
	}, nil
}

// writeWholeFile produces a single region covering everything.
func (s *Stage) writeWholeFile(_ context.Context, env *stage.Env) (*stage.Result, error) {
	var info media.MediaInfo
	if err := env.ReadInput("audio", &info); err != nil {
		// The audio artifact is a WAV whose own duration is what matters here,
		// and the probe artifact has it if this one does not.
		var probe media.MediaInfo
		if probeErr := env.ReadInput("probe", &probe); probeErr == nil {
			info = probe
		}
	}

	result := protocol.VADResult{
		Regions:      []protocol.VADRegion{{Start: 0, End: info.Duration}},
		TotalSpeechS: info.Duration,
		TotalAudioS:  info.Duration,
	}
	if info.Duration <= 0 {
		result.Regions = nil
	}

	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("vad: encode: %w", err)
	}
	payload = append(payload, '\n')

	if err := os.WriteFile(filepath.Join(env.OutDir, PayloadName), payload, 0o644); err != nil {
		return nil, fmt.Errorf("vad: write %s: %w", PayloadName, err)
	}

	return &stage.Result{
		Primary: PayloadName,
		Metadata: map[string]any{
			"region_count": len(result.Regions),
			"disabled":     true,
		},
	}, nil
}

// audioPathOf resolves the audio artifact's file on disk.
func audioPathOf(env *stage.Env) (string, error) {
	path, err := env.InputPath("audio")
	if err != nil {
		return "", fmt.Errorf("vad: %w", err)
	}
	return path, nil
}

// validate enforces the invariants the rest of the pipeline relies on.
func validate(regions []protocol.VADRegion, total float64) error {
	previousEnd := 0.0
	for i, region := range regions {
		if region.End <= region.Start {
			return fmt.Errorf("region %d has a non-positive duration: [%v, %v]",
				i, region.Start, region.End)
		}
		if region.Start < previousEnd {
			return fmt.Errorf("region %d starts at %v, before the previous end %v",
				i, region.Start, previousEnd)
		}
		if total > 0 && region.End > total+1 {
			return fmt.Errorf("region %d ends at %v, past the audio's %v",
				i, region.End, total)
		}
		previousEnd = region.End
	}
	return nil
}
