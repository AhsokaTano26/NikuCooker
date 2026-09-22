// Package probe reads the source file's structure.
//
// It is the first stage of the pipeline and the only one that reads the source
// directly. Everything downstream consumes its artifact, so a container that is
// expensive to open is opened exactly once.
package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/AhsokaTano26/NikuCooker/internal/config"
	"github.com/AhsokaTano26/NikuCooker/internal/media"
	"github.com/AhsokaTano26/NikuCooker/internal/stage"
)

// PayloadName is the file this stage writes.
const PayloadName = "probe.json"

// Stage probes the source media.
type Stage struct{}

// New builds the stage.
func New() *Stage { return &Stage{} }

// Spec describes the stage.
func (s *Stage) Spec() stage.Spec {
	return stage.Spec{
		Name: "probe",
		// Bump when the shape of probe.json changes, so downstream stages are
		// not handed a payload they cannot read.
		Version: "1",
		// No dependencies: this is the one stage that reads the source directly.
		ConfigKey: "probe",
	}
}

// ConfigSubtree returns nothing, because probing has no configuration.
//
// That is the correct answer rather than an omission: the output depends only
// on the file. Changing any setting must not re-probe, and returning a subtree
// here would make it do exactly that.
func (s *Stage) ConfigSubtree(*config.Config) any { return nil }

// Fingerprint returns nothing for the same reason.
func (s *Stage) Fingerprint(context.Context, *stage.Env) (map[string]string, error) {
	return nil, nil
}

// Run probes the source.
func (s *Stage) Run(ctx context.Context, env *stage.Env) (*stage.Result, error) {
	if env.SourcePath == "" {
		return nil, fmt.Errorf("probe: no source file configured for this project")
	}
	if env.Media == nil {
		return nil, fmt.Errorf("probe: no media service available")
	}

	env.ReportProgress(0, "reading container")

	info, err := env.Media.Probe(ctx, env.SourcePath)
	if err != nil {
		return nil, err
	}

	// A file that probes cleanly but has no audio fails here rather than three
	// stages later, when the reason is no longer visible.
	audio, err := info.PrimaryAudio(env.Config.ASR.Language)
	if err != nil {
		return nil, fmt.Errorf("probe: %w: %s", err, env.SourcePath)
	}

	env.ReportProgress(0.8, "writing metadata")

	raw, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("probe: encode metadata: %w", err)
	}
	raw = append(raw, '\n')

	if err := os.WriteFile(filepath.Join(env.OutDir, PayloadName), raw, 0o644); err != nil {
		return nil, fmt.Errorf("probe: write %s: %w", PayloadName, err)
	}

	env.ReportProgress(1, "done")

	return &stage.Result{
		Primary: PayloadName,
		Metadata: map[string]any{
			"container":      info.Format.Name,
			"duration_s":     info.Duration,
			"has_video":      info.HasVideo(),
			"video_streams":  len(info.Video),
			"audio_streams":  len(info.Audio),
			"subtitle_count": len(info.Subtitle),
			// Recording the selection makes "why did it transcribe the dub"
			// answerable without re-probing.
			"selected_audio_index":    audio.Index,
			"selected_audio_codec":    audio.Codec,
			"selected_audio_channels": audio.Channels,
			"selected_audio_language": audio.Language,
		},
	}, nil
}

// Info is the payload this stage writes.
//
// An alias for the media package's type rather than a copy: a second struct
// with the same fields is a second place to update, and the one that is
// forgotten is the one a consumer depends on.
type Info = media.MediaInfo
