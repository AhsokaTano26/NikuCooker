// Package audio turns the source into the canonical WAV the recogniser wants.
//
// One FFmpeg pass does everything: select the stream, high-pass, resample and
// downmix. Splitting extraction from preprocessing would decode a
// multi-gigabyte source twice and write an intermediate file, for no benefit —
// the two always run together, and changing either invalidates the other
// anyway.
package audio

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/AhsokaTano26/NikuCooker/internal/config"
	"github.com/AhsokaTano26/NikuCooker/internal/media"
	"github.com/AhsokaTano26/NikuCooker/internal/stage"
)

// PayloadName is the file this stage writes.
const PayloadName = "audio.wav"

// Stage extracts and normalises the audio.
type Stage struct{}

// New builds the stage.
func New() *Stage { return &Stage{} }

// Spec describes the stage.
func (s *Stage) Spec() stage.Spec {
	return stage.Spec{
		Name:    "audio",
		Version: "1",
		// The probe artifact carries the stream index and the duration, so this
		// stage never re-opens the source to find them.
		Depends:   []string{"probe"},
		ConfigKey: "audio",
	}
}

// ConfigSubtree returns the audio settings.
func (s *Stage) ConfigSubtree(cfg *config.Config) any { return cfg.Audio }

// Fingerprint returns nothing: this stage's only inputs are its configuration
// and the probe artifact, and both are already in the key.
func (s *Stage) Fingerprint(context.Context, *stage.Env) (map[string]string, error) {
	return nil, nil
}

// Run extracts the audio.
func (s *Stage) Run(ctx context.Context, env *stage.Env) (*stage.Result, error) {
	if env.Media == nil {
		return nil, fmt.Errorf("audio: no media service available")
	}

	var info media.MediaInfo
	if err := env.ReadInput("probe", &info); err != nil {
		return nil, fmt.Errorf("audio: %w", err)
	}

	// The stream was chosen during probing, so the same file always yields the
	// same audio. Re-deciding here would let a probe re-run silently change
	// which track is transcribed.
	stream, err := info.PrimaryAudio(env.Config.ASR.Language)
	if err != nil {
		return nil, fmt.Errorf("audio: %w", err)
	}

	opts := media.AudioOptions{
		StreamIndex: stream.Index,
		SampleRate:  env.Config.Audio.SampleRate,
		Channels:    1,
		HighpassHz:  env.Config.Audio.HighpassHz,
		Loudnorm:    env.Config.Audio.Loudnorm,
	}

	output := filepath.Join(env.OutDir, PayloadName)

	env.ReportProgress(0, "extracting audio")
	err = env.Media.ExtractAudio(ctx, env.SourcePath, output, opts, info.Duration,
		func(fraction float64, message string) {
			env.ReportProgress(fraction, message)
		})
	if err != nil {
		return nil, err
	}

	// FFmpeg exits zero having written a header and nothing else when a stream
	// selection matches nothing, so the file is checked rather than assumed.
	stat, err := os.Stat(output)
	if err != nil {
		return nil, fmt.Errorf("audio: ffmpeg reported success but wrote no output: %w", err)
	}
	// A 44-byte WAV is a header with no samples.
	if stat.Size() <= 1024 {
		return nil, fmt.Errorf(
			"audio: ffmpeg produced an empty file (%d bytes); the selected stream may carry no audio",
			stat.Size())
	}

	env.ReportProgress(1, "done")

	return &stage.Result{
		Primary: PayloadName,
		Metadata: map[string]any{
			// The container-absolute index, matching what probe.json records.
			"stream_index":    stream.Index,
			"source_codec":    stream.Codec,
			"source_channels": stream.Channels,
			"sample_rate":     env.Config.Audio.SampleRate,
			"channels":        1,
			"highpass_hz":     env.Config.Audio.HighpassHz,
			"loudnorm":        env.Config.Audio.Loudnorm,
			"size_bytes":      stat.Size(),
			"duration_s":      info.Duration,
		},
	}, nil
}
