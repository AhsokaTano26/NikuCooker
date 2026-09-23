package render

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/AhsokaTano26/NikuCooker/internal/artifact"
	"github.com/AhsokaTano26/NikuCooker/internal/config"
	"github.com/AhsokaTano26/NikuCooker/internal/media"
	"github.com/AhsokaTano26/NikuCooker/internal/stage"
	stagesubtitle "github.com/AhsokaTano26/NikuCooker/internal/stage/subtitle"
)

// Removing the soft fallback, or checking hard-subtitle support before
// preserving supported modes, must make this test fail. A missing optional
// output must not discard the usable output requested in the same run.
func TestRunKeepsSoftOutputWhenFFmpegCannotBurnSubtitles(t *testing.T) {
	subtitleDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(subtitleDir, stagesubtitle.SRTName), []byte("subtitle"), 0o600); err != nil {
		t.Fatal(err)
	}

	mediaService := media.NewWithRunner(func(
		_ context.Context,
		_ string,
		args []string,
		stdout io.Writer,
		_ io.Writer,
	) error {
		switch {
		case slices.Contains(args, "-version"):
			_, _ = fmt.Fprintln(stdout, "ffmpeg version test")
		case slices.Contains(args, "-encoders"):
			_, _ = fmt.Fprintln(stdout, " V..... libx264 test encoder")
		case slices.Contains(args, "-filters"):
			// Deliberately no subtitles or ass filter: this is the common
			// Homebrew/distribution FFmpeg build that triggered the regression.
			_, _ = fmt.Fprintln(stdout, " ... scale V->V Scale the input video")
		default:
			return os.WriteFile(args[len(args)-1], []byte("rendered"), 0o600)
		}
		return nil
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	cfg := config.Default()
	cfg.Render.Modes = []string{"soft", "hard"}
	outDir := t.TempDir()
	env := &stage.Env{
		Project:    stage.ProjectInfo{Name: "episode", TargetLanguage: "zh-Hans"},
		SourcePath: "source.mp4",
		Media:      mediaService,
		Config:     cfg,
		Inputs: map[string]*artifact.Artifact{
			"probe":    {Stage: "probe"},
			"subtitle": {Stage: "subtitle"},
		},
		Artifacts: renderInputs{subtitleDir: subtitleDir},
		OutDir:    outDir,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	result, err := New().Run(context.Background(), env)
	if err != nil {
		t.Fatalf("Run returned an error instead of the soft output: %v", err)
	}
	if result.Primary != "episode.nikucooker.mkv" {
		t.Errorf("primary = %q, want the soft-subtitle output", result.Primary)
	}
	if _, err := os.Stat(filepath.Join(outDir, result.Primary)); err != nil {
		t.Fatalf("soft-subtitle output was not written: %v", err)
	}

	modes, ok := result.Metadata["modes"].([]string)
	if !ok || len(modes) != 1 || modes[0] != "soft" {
		t.Errorf("rendered modes = %#v, want [soft]", result.Metadata["modes"])
	}
	warnings, ok := result.Metadata["warnings"].([]string)
	if !ok || len(warnings) == 0 {
		t.Fatalf("warnings = %#v, want the skipped hard-subtitle explanation", result.Metadata["warnings"])
	}
}

type renderInputs struct {
	subtitleDir string
}

func (r renderInputs) Decode(a *artifact.Artifact, destination any) error {
	switch a.Stage {
	case "probe":
		info, ok := destination.(*media.MediaInfo)
		if !ok {
			return fmt.Errorf("probe destination is %T", destination)
		}
		*info = media.MediaInfo{
			Duration: 1,
			Video:    []media.Stream{{Index: 0, Type: "video", Codec: "h264"}},
		}
		return nil
	case "subtitle":
		manifest, ok := destination.(*stagesubtitle.Manifest)
		if !ok {
			return fmt.Errorf("subtitle destination is %T", destination)
		}
		*manifest = stagesubtitle.Manifest{Files: []stagesubtitle.File{{
			Format: "srt", Name: stagesubtitle.SRTName, Bytes: 8, SegmentCount: 1,
		}}}
		return nil
	default:
		return fmt.Errorf("unexpected artifact stage %q", a.Stage)
	}
}

func (r renderInputs) Path(*artifact.Artifact) (string, error) {
	return "", fmt.Errorf("render test does not resolve primary paths")
}

func (r renderInputs) Dir(*artifact.Artifact) (string, error) {
	return r.subtitleDir, nil
}
