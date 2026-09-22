// Package segmentation turns recogniser output into subtitle lines.
//
// It is the cheapest stage in the pipeline and among the most visible: no model
// runs here, no network is touched, and yet everything a viewer notices about a
// fansub's readability is decided in this step. A transcript is not a subtitle,
// and the gap between them is this package.
package segmentation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/AhsokaTano26/NikuCooker/internal/config"
	"github.com/AhsokaTano26/NikuCooker/internal/stage"
	"github.com/AhsokaTano26/NikuCooker/internal/subtitle"
	"github.com/AhsokaTano26/NikuCooker/pkg/protocol"
)

// PayloadName is the file this stage writes.
const PayloadName = "segments.json"

// Stage builds subtitle lines.
type Stage struct{}

// New builds the stage.
func New() *Stage { return &Stage{} }

// Spec describes the stage.
func (s *Stage) Spec() stage.Spec {
	return stage.Spec{
		Name:    "segmentation",
		Version: "1",
		// Only the transcript. The audio is of no use here — the word timings
		// carry everything this stage needs — and declaring a dependency that is
		// never read would only widen the cache key.
		Depends:   []string{"asr"},
		ConfigKey: "subtitle",
	}
}

// ConfigSubtree returns the line-breaking settings.
func (s *Stage) ConfigSubtree(cfg *config.Config) any { return cfg.Subtitle }

// Fingerprint returns nothing.
//
// Splitting is a pure function of the transcript and the configuration, and both
// are already in the key: the transcript through the artifact dependency, the
// configuration through ConfigSubtree. Adding a fingerprint here would put the
// same information in twice and risk the two disagreeing.
func (s *Stage) Fingerprint(context.Context, *stage.Env) (map[string]string, error) {
	return nil, nil
}

// Run splits the transcript.
func (s *Stage) Run(ctx context.Context, env *stage.Env) (*stage.Result, error) {
	var transcript protocol.ASRResult
	if err := env.ReadInput("asr", &transcript); err != nil {
		return nil, fmt.Errorf("segmentation: %w", err)
	}

	// The project says what language the work is in; the recogniser reports what
	// it actually heard. Where they disagree the recogniser is right, because
	// the line-breaking rules have to match the text in hand. A project left on
	// "auto" is the normal case, and this is where its answer comes from.
	sourceLanguage := env.Project.SourceLanguage
	if transcript.Language != "" && transcript.Language != "auto" {
		if sourceLanguage != "" && sourceLanguage != "auto" &&
			base(sourceLanguage) != base(transcript.Language) {
			env.Log.Warn("the transcript is in a different language than the project declares",
				"project", sourceLanguage, "recognised", transcript.Language)
		}
		sourceLanguage = transcript.Language
	}
	if sourceLanguage == "" || sourceLanguage == "auto" {
		return nil, fmt.Errorf(
			"segmentation: the source language is unknown; set it on the project or configure recognition to detect it")
	}

	targetLanguage := env.Project.TargetLanguage
	if targetLanguage == "" {
		return nil, fmt.Errorf("segmentation: the project has no target language")
	}

	options := buildOptions(env.Config, sourceLanguage)
	env.Log.Info("splitting the transcript",
		"segments", len(transcript.Segments), "language", sourceLanguage)

	lines, err := subtitle.Build(transcript.Segments, sourceLanguage, targetLanguage, options)
	if err != nil {
		return nil, fmt.Errorf("segmentation: %w", err)
	}

	if len(lines) == 0 {
		// Not an error. A file with no speech produces no subtitles, and saying
		// so here is far more useful than a render that silently emits nothing.
		env.Log.Warn("the transcript yielded no subtitle lines")
	}

	set := &subtitle.Set{
		SourceLanguage: sourceLanguage,
		TargetLanguage: targetLanguage,
		Segments:       lines,
		Source: subtitle.SourceProvenance{
			ASRModel:     env.Config.ASR.Model,
			SegmentCount: len(transcript.Segments),
		},
	}
	if asrArtifact, ok := env.Inputs["asr"]; ok && asrArtifact != nil {
		set.Source.ASRArtifactID = asrArtifact.ID
	}

	if err := writeSet(env.OutDir, set); err != nil {
		return nil, err
	}

	env.ReportProgress(1, "done")

	return &stage.Result{
		Primary: PayloadName,
		Model:   env.Config.ASR.Model,
		Metadata: map[string]any{
			"line_count":       len(set.Segments),
			"asr_segmentation": len(transcript.Segments),
			// How much the splitter had to intervene. A ratio near 1 means the
			// recogniser was already producing subtitle-sized segments; much
			// above it means the transcript arrived in long blocks, which is
			// worth knowing when the lines look wrong.
			"split_ratio": ratio(len(set.Segments), len(transcript.Segments)),
			"max_cps":     set.MaxCPS(),
		},
	}, nil
}

// buildOptions maps configuration onto the splitter's limits.
//
// The line-length ceiling is chosen by the *source* language, because that is
// the text being split. A Japanese line and a Chinese line of the same character
// count carry different amounts of speech, and the recogniser that produced them
// punctuates differently.
func buildOptions(cfg *config.Config, sourceLanguage string) subtitle.Options {
	options := subtitle.Options{
		MinDuration: cfg.Subtitle.MinDuration,
		MaxDuration: cfg.Subtitle.MaxDuration,
		MaxCPS:      cfg.Subtitle.MaxCPS,
		PauseMS:     cfg.Subtitle.PauseMS,
	}

	switch base(sourceLanguage) {
	case "ja":
		options.MaxChars = cfg.Subtitle.MaxCharsJA
	case "zh":
		options.MaxChars = cfg.Subtitle.MaxCharsZH
	}

	return options
}

// base reduces a BCP-47 tag to its language subtag.
func base(tag string) string {
	for i := 0; i < len(tag); i++ {
		switch tag[i] {
		case '-', '_':
			return tag[:i]
		}
	}
	return tag
}

// ratio reports one count against another, guarding the division.
func ratio(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}

// writeSet writes the artifact payload.
//
// Through a temporary file and a rename, so that a crash part-way through cannot
// leave a truncated payload in a directory the pipeline is about to publish.
func writeSet(dir string, set *subtitle.Set) error {
	encoded, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return fmt.Errorf("segmentation: encode the lines: %w", err)
	}

	final := filepath.Join(dir, PayloadName)
	temporary := final + ".tmp"

	if err := os.WriteFile(temporary, encoded, 0o644); err != nil {
		return fmt.Errorf("segmentation: write the lines: %w", err)
	}
	if err := os.Rename(temporary, final); err != nil {
		return fmt.Errorf("segmentation: write the lines: %w", err)
	}
	return nil
}
