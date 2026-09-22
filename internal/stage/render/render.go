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
	"strings"

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

	base, err := outputBase(env.Project.Name, info.HasVideo())
	if err != nil {
		return nil, err
	}

	modes, err := renderModes(env.Config.Render.Modes)
	if err != nil {
		return nil, err
	}

	// Every requested mode is checked before any of them renders.
	//
	// Checked up front rather than as each one comes round, because an artifact
	// is published as a unit: discovering on the second mode that it cannot be
	// done would abort the artifact and discard the first one's output. A user
	// who asked for two versions, one of which is impossible on this machine,
	// would then get neither — and nothing on screen saying which was the
	// problem.
	caps, err := s.capabilities(ctx, env, modes)
	if err != nil {
		return nil, err
	}

	subtitleDir, err := env.Artifacts.Dir(env.Inputs["subtitle"])
	if err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}

	subtitleLines := 0
	outputs := make([]outputRecord, 0, len(modes))
	warnings := []string{}

	for i, mode := range modes {
		outputName := outputName(base, mode)
		outputPath := filepath.Join(env.OutDir, outputName)

		if mode == media.RenderHard && !info.HasVideo() {
			return nil, errors.New(
				"render: the source has no video stream, so there is nothing to burn subtitles into; " +
					"set render.modes to [soft], or disable the render stage")
		}

		subtitleFile, err := pickSubtitle(manifest, mode, outputName)
		if err != nil {
			return nil, err
		}
		subtitlePath := filepath.Join(subtitleDir, subtitleFile)

		options := s.options(env, mode, caps)

		env.Log.Info("rendering",
			"mode", mode, "output", outputName,
			"subtitles", subtitleFile, "encoder", options.Encoder, "duration_s", info.Duration)

		// The progress bar spans every output, so with two of them it reaches
		// halfway after the first rather than filling and starting over — which
		// reads as a stage that finished and then ran again.
		span := 1 / float64(len(modes))
		stageWarnings, err := env.Media.Render(ctx, env.SourcePath, subtitlePath, outputPath, options, info.Duration,
			func(fraction float64, message string) {
				env.ReportProgress(float64(i)*span+fraction*span, message)
			})
		if err != nil {
			return nil, fmt.Errorf("render: %w", err)
		}

		// Warnings are things that worked but not as the user probably intended
		// — styling dropped by the container, a hardware encoder that fell back
		// to software. They are logged because the alternative is the user
		// discovering them on playback.
		for _, warning := range stageWarnings {
			env.Log.Warn(warning)
		}
		warnings = append(warnings, stageWarnings...)

		stat, err := os.Stat(outputPath)
		if err != nil {
			return nil, fmt.Errorf("render: reported success but wrote no output: %w", err)
		}

		if file, ok := manifest.FileFor(fileFormatFor(subtitleFile)); ok {
			subtitleLines = file.SegmentCount
		}

		outputs = append(outputs, outputRecord{
			Name:      outputName,
			Mode:      string(mode),
			Bytes:     stat.Size(),
			Encoder:   options.Encoder,
			Subtitles: subtitleFile,
		})
	}

	env.ReportProgress(1, "done")

	// The first is the primary: it is the one a consumer should open, and the
	// order comes from the configuration rather than from a rule invented here.
	primary := outputs[0]

	return &stage.Result{
		Primary: primary.Name,
		Metadata: map[string]any{
			"mode":           primary.Mode,
			"modes":          modeNames(outputs),
			"encoder":        primary.Encoder,
			"subtitles":      primary.Subtitles,
			"subtitle_lines": subtitleLines,
			"output_bytes":   primary.Bytes,
			"outputs":        outputs,
			"duration_s":     info.Duration,
			"warnings":       warnings,
			"has_video":      info.HasVideo(),
		},
	}, nil
}

// outputRecord is one rendered file, as recorded on the artifact.
type outputRecord struct {
	Name      string `json:"name"`
	Mode      string `json:"mode"`
	Bytes     int64  `json:"bytes"`
	Encoder   string `json:"encoder,omitempty"`
	Subtitles string `json:"subtitles"`
}

func modeNames(outputs []outputRecord) []string {
	names := make([]string, 0, len(outputs))
	for _, output := range outputs {
		names = append(names, output.Mode)
	}
	return names
}

// renderModes normalises the configured list.
//
// Deduplicated and ordered, because the artifact key is built from this
// configuration subtree: a list that varied in order or repetition between two
// runs of the same configuration would produce a different key and re-encode
// the film.
func renderModes(configured []string) ([]media.RenderMode, error) {
	if len(configured) == 0 {
		// An empty list is a request for nothing, which is a mistake rather than
		// a preference. Falling back to soft would silently ignore the setting.
		return nil, errors.New(
			"render: render.modes is empty; list the versions to produce, for example [soft] or [soft, hard]")
	}

	seen := map[media.RenderMode]bool{}
	modes := make([]media.RenderMode, 0, len(configured))

	for _, name := range configured {
		mode := media.RenderMode(strings.TrimSpace(name))
		if mode != media.RenderSoft && mode != media.RenderHard {
			return nil, fmt.Errorf("render: %q is not a render mode; use soft or hard", name)
		}
		if seen[mode] {
			continue
		}
		seen[mode] = true
		modes = append(modes, mode)
	}
	return modes, nil
}

// capabilities probes FFmpeg, but only when a mode needs the answer.
//
// A soft render is a stream copy: it does not care which filters or encoders
// exist, and probing for them would spawn three FFmpeg processes to learn
// something nothing is going to use.
func (s *Stage) capabilities(
	ctx context.Context,
	env *stage.Env,
	modes []media.RenderMode,
) (*media.Capabilities, error) {
	needsHard := false
	for _, mode := range modes {
		if mode == media.RenderHard {
			needsHard = true
		}
	}
	if !needsHard {
		return nil, nil
	}

	caps, err := env.Media.Capabilities(ctx)
	if err != nil {
		// The probe failed, so the answer is unknown rather than no. Attempting
		// the burn is the right call: if the filter is missing, FFmpeg names it,
		// and that error is more specific than anything guessable here.
		env.Log.Warn("could not detect the available filters; attempting the burn anyway",
			"error", err)
		return nil, nil
	}

	// Not every FFmpeg build has libass, and a user whose does not deserves to
	// be told which setting to change rather than shown "No such filter:
	// 'subtitles'", which reads as a bug in this program.
	//
	// Refused before anything is encoded: the alternative is a burn that runs to
	// completion and then reports a missing filter, having already spent however
	// long it takes to re-encode the whole film.
	if !caps.CanBurnSubtitles() {
		return nil, errors.New(
			"render: this FFmpeg was built without libass, so it cannot draw subtitles into video; " +
				"drop hard from render.modes, or install an FFmpeg built with --enable-libass")
	}
	return caps, nil
}

// options builds the render settings. `caps` is nil when nothing needed probing.
func (s *Stage) options(env *stage.Env, mode media.RenderMode, caps *media.Capabilities) media.RenderOptions {
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
		return options
	}

	options.Encoder = cfg.Encoder
	if options.Encoder != "" || caps == nil {
		return options
	}

	// Detected rather than assumed: which hardware encoders exist varies by
	// machine and by FFmpeg build, and libx264 is always there.
	options.Encoder = caps.RecommendedVideoEncoder()

	return options
}

// outputName builds the rendered file's name.
//
// Named after the project rather than the file on disk. The source is stored
// under a fixed name — "source.mp4", whatever it was called when it arrived —
// so deriving the output from it would give every project in the data
// directory the same output filename, and a user who exported two of them
// would have two files called source.nikucooker.mkv.
func outputBase(projectName string, hasVideo bool) (string, error) {
	if !hasVideo {
		// An audio-only source cannot be rendered as video, and the caller has
		// already refused. This exists so that the name is never built from an
		// empty base.
		return "", errors.New("render: the source has no video stream")
	}

	base := sanitiseFilename(projectName)
	if base == "" {
		base = "nikucooker"
	}
	return base, nil
}

// outputName builds one output's filename.
//
// The mode is part of it, because two outputs that differ only in whether the
// subtitles are in the picture are indistinguishable once they are sitting in
// the same directory — and picking the wrong one is discovered by watching the
// whole thing.
func outputName(base string, mode media.RenderMode) string {
	// Matroska, because it is the container that carries ASS styling natively
	// and accepts any codec. Writing MP4 would silently drop the styling.
	suffix := ".nikucooker.mkv"
	if mode == media.RenderHard {
		suffix = ".nikucooker.hardsub.mkv"
	}

	return base + suffix
}

// sanitiseFilename removes what cannot appear in a filename.
//
// The project name is free text a user typed, and it reaches a path. Removing
// the separators and the reserved characters is what keeps a project called
// "../../etc/passwd" from being a path rather than a name.
func sanitiseFilename(name string) string {
	replacer := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_",
		"?", "_", "\"", "_", "<", "_", ">", "_", "|", "_",
		"\n", "_", "\r", "_",
	)
	return strings.TrimSpace(replacer.Replace(strings.TrimSpace(name)))
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
