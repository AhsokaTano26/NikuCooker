// Package render produces the video a user watches.
//
// The last stage, and the only one that writes something large. It is optional
// because a user who only wanted subtitles should not pay for a full re-encode,
// and because burning subtitles in is destructive: it is the one operation here
// that cannot be undone by re-running a cheaper stage.
package render

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/AhsokaTano26/NikuCooker/internal/config"
	"github.com/AhsokaTano26/NikuCooker/internal/media"
	"github.com/AhsokaTano26/NikuCooker/internal/stage"
	stagesubtitle "github.com/AhsokaTano26/NikuCooker/internal/stage/subtitle"
)

// PayloadName is the file this stage writes.
const PayloadName = "render.json"

// Stage renders the final video.
type Stage struct{}

// New builds the stage.
func New() *Stage { return &Stage{} }

// Spec describes the stage.
func (s *Stage) Spec() stage.Spec {
	return stage.Spec{
		Name:    "render",
		Version: "1",
		// The probe result says whether there is a picture to subtitle; the
		// subtitle manifest says which files to burn in.
		Depends:   []string{"probe", "subtitle"},
		Optional:  true,
		ConfigKey: "render",
	}
}

// ConfigSubtree returns the encoding settings.
func (s *Stage) ConfigSubtree(cfg *config.Config) any { return cfg.Render }

// Fingerprint returns nothing.
//
// Render reads the media file directly rather than an upstream artifact, so the
// source has to be in its cache key — and it is: the pipeline folds the source
// fingerprint into any stage that depends on probe, which this one does. Doing
// it again here would put the same value in twice and risk the two disagreeing.
func (s *Stage) Fingerprint(context.Context, *stage.Env) (map[string]string, error) {
	return nil, nil
}

// Run renders the video.
func (s *Stage) Run(ctx context.Context, env *stage.Env) (*stage.Result, error) {
	if env.Media == nil {
		return nil, errors.New("render: no media service is available")
	}
	if env.SourcePath == "" {
		return nil, errors.New("render: the project has no source media")
	}

	var info media.MediaInfo
	if err := env.ReadInput("probe", &info); err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}

	var manifest stagesubtitle.Manifest
	if err := env.ReadInput("subtitle", &manifest); err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}

	outputName, err := outputName(env.SourcePath, info.HasVideo())
	if err != nil {
		return nil, err
	}
	outputPath := filepath.Join(env.OutDir, outputName)

	mode := media.RenderMode(env.Config.Render.Mode)
	if mode != media.RenderSoft && mode != media.RenderHard {
		return nil, fmt.Errorf("render: %q is not a render mode; use soft or hard", env.Config.Render.Mode)
	}

	if mode == media.RenderHard && !info.HasVideo() {
		return nil, errors.New(
			"render: the source has no video stream, so there is nothing to burn subtitles into; " +
				"set render.mode to soft, or disable the render stage")
	}

	subtitleDir, err := env.Artifacts.Dir(env.Inputs["subtitle"])
	if err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}

	subtitleFile, err := pickSubtitle(manifest, mode, outputName)
	if err != nil {
		return nil, err
	}
	subtitlePath := filepath.Join(subtitleDir, subtitleFile)

	options, err := s.options(ctx, env, mode)
	if err != nil {
		return nil, err
	}

	env.Log.Info("rendering",
		"mode", mode, "output", outputName,
		"subtitles", subtitleFile, "encoder", options.Encoder, "duration_s", info.Duration)

	warnings, err := env.Media.Render(ctx, env.SourcePath, subtitlePath, outputPath, options, info.Duration,
		func(fraction float64, message string) {
			env.ReportProgress(fraction, message)
		})
	if err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}

	// Warnings are things that worked but not as the user probably intended —
	// styling dropped by the container, a hardware encoder that fell back to
	// software. They are logged because the alternative is the user discovering
	// them on playback.
	for _, warning := range warnings {
		env.Log.Warn(warning)
	}

	stat, err := os.Stat(outputPath)
	if err != nil {
		return nil, fmt.Errorf("render: reported success but wrote no output: %w", err)
	}

	env.ReportProgress(1, "done")

	subtitleLines := 0
	if file, ok := manifest.FileFor(fileFormatFor(subtitleFile)); ok {
		subtitleLines = file.SegmentCount
	}

	return &stage.Result{
		Primary: outputName,
		Metadata: map[string]any{
			"mode":           string(mode),
			"encoder":        options.Encoder,
			"crf":            options.CRF,
			"preset":         options.Preset,
			"subtitles":      subtitleFile,
			"subtitle_lines": subtitleLines,
			"output_bytes":   stat.Size(),
			"duration_s":     info.Duration,
			"warnings":       warnings,
			"has_video":      info.HasVideo(),
		},
	}, nil
}

// options builds the render settings.
func (s *Stage) options(ctx context.Context, env *stage.Env, mode media.RenderMode) (media.RenderOptions, error) {
	cfg := env.Config.Render

	options := media.DefaultRenderOptions()
	options.Mode = mode
	options.CRF = cfg.CRF
	options.Preset = cfg.Preset
	options.AudioBitrate = cfg.AudioBitrate
	options.SubtitleLanguage = iso639_2(env.Project.TargetLanguage)
	options.SubtitleTitle = "NikuCooker"

	if mode == media.RenderSoft {
		// The encoder is irrelevant to a stream copy. Leaving it empty is more
		// honest than reporting one that was never used.
		return options, nil
	}

	// Checked before anything is encoded, because the failure it prevents is a
	// burn that runs to completion and then reports a missing filter — having
	// already spent however long it takes to re-encode the whole film.
	//
	// Not every FFmpeg build has libass, and a user whose does not deserves to
	// be told that rather than shown "No such filter: 'subtitles'", which reads
	// as a bug in this program.
	caps, err := env.Media.Capabilities(ctx)
	if err != nil {
		// The probe failed, so the answer is unknown rather than no. Attempting
		// the burn is the right call: if the filter is missing, the error from
		// FFmpeg still names it.
		env.Log.Warn("could not detect the available filters; attempting the burn anyway",
			"error", err)
		options.Encoder = cfg.Encoder
		return options, nil
	}
	if !caps.CanBurnSubtitles() {
		return media.RenderOptions{}, errors.New(
			"render: this FFmpeg was built without libass, so it cannot draw subtitles into video; " +
				"set render.mode to soft, or install an FFmpeg built with --enable-libass")
	}

	options.Encoder = cfg.Encoder
	if options.Encoder != "" {
		return options, nil
	}

	// Detected rather than assumed: which hardware encoders exist varies by
	// machine and by FFmpeg build, and libx264 is always there.
	options.Encoder = caps.RecommendedVideoEncoder()

	return options, nil
}

// outputName builds the rendered file's name.
//
// The suffix is derived from the mode, so that a project rendered both ways
// keeps both files rather than the second silently replacing the first.
func outputName(sourcePath string, hasVideo bool) (string, error) {
	if !hasVideo {
		// An audio-only source cannot be rendered as video, and the caller has
		// already refused. This exists so that the name is never built from an
		// empty base.
		return "", errors.New("render: the source has no video stream")
	}

	base := trimExtension(filepath.Base(sourcePath))
	if base == "" {
		return "", fmt.Errorf("render: cannot derive an output name from %q", sourcePath)
	}

	// Matroska, because it is the container that carries ASS styling natively
	// and accepts any codec. Writing MP4 would silently drop the styling.
	return base + ".nikucooker.mkv", nil
}

// pickSubtitle chooses which subtitle file to use.
//
// A hard render draws the subtitles through libass, which understands ASS and
// can therefore honour the styling. A soft render into a container that cannot
// carry ASS gets the SRT instead, so that the styling is not silently dropped
// twice — once by the conversion and again by the container.
func pickSubtitle(manifest stagesubtitle.Manifest, mode media.RenderMode, outputName string) (string, error) {
	if len(manifest.Files) == 0 {
		return "", errors.New("render: the subtitle stage wrote no files")
	}

	wanted := fileFormatFor(media.SubtitleExtensionFor(outputName))
	if mode == media.RenderHard {
		wanted = "ass"
	}

	if file, ok := manifest.FileFor(wanted); ok {
		return file.Name, nil
	}

	// Fall back to whatever was written rather than failing. The formats list
	// is the user's, and refusing to render because it named only SRT would be
	// a worse outcome than rendering without the ideal format.
	fallback := manifest.Files[0]
	return fallback.Name, nil
}

// fileFormatFor maps a file extension to the format name the manifest uses.
func fileFormatFor(extension string) string {
	switch extension {
	case stagesubtitle.ASSName, ".ass":
		return "ass"
	default:
		return "srt"
	}
}

// trimExtension removes the final extension from a filename.
func trimExtension(name string) string {
	return name[:len(name)-len(filepath.Ext(name))]
}

// iso639_2 maps a BCP-47 tag to the two-letter code Matroska expects.
//
// Matroska's language element is defined as ISO 639-2, and while players
// overwhelmingly accept the 639-1 form, a track tagged "zh-Hans" is not a code
// at all — it is a tag, and a player that matches strictly will not find it.
func iso639_2(tag string) string {
	switch {
	case tag == "":
		return "und"
	case len(tag) >= 2 && (tag[:2] == "zh"):
		return "chi"
	case len(tag) >= 2 && (tag[:2] == "ja"):
		return "jpn"
	case len(tag) >= 2 && (tag[:2] == "ko"):
		return "kor"
	case len(tag) >= 2 && (tag[:2] == "en"):
		return "eng"
	default:
		// Passed through unchanged. A wrong tag is a track a player will not
		// auto-select, which is a mild failure; guessing one would be worse.
		return tag
	}
}
