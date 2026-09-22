// Package translation runs the language model over the subtitle lines.
//
// It is the stage that costs money, and the one whose behaviour a user judges
// the whole system by. The work splits three ways: the line cache decides what
// is asked at all, the translator asks, and this stage fixes the timing that
// results and marks what a human still needs to look at.
package translation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/AhsokaTano26/NikuCooker/internal/analysis"
	"github.com/AhsokaTano26/NikuCooker/internal/config"
	"github.com/AhsokaTano26/NikuCooker/internal/glossary"
	"github.com/AhsokaTano26/NikuCooker/internal/stage"
	"github.com/AhsokaTano26/NikuCooker/internal/subtitle"
	translationcore "github.com/AhsokaTano26/NikuCooker/internal/translation"
	"github.com/AhsokaTano26/NikuCooker/prompts"
)

// PayloadName is the file this stage writes.
const PayloadName = "translated.json"

// Stage translates subtitle lines.
type Stage struct{}

// New builds the stage.
func New() *Stage { return &Stage{} }

// Spec describes the stage.
func (s *Stage) Spec() stage.Spec {
	return stage.Spec{
		Name:    "translation",
		Version: "1",
		// The context document is optional: a project without a configured
		// provider, or a user who disabled the analysis pass, still gets
		// translated subtitles.
		Depends:         []string{"segmentation"},
		OptionalDepends: []string{"context"},
		ConfigKey:       "translation",
	}
}

// ConfigSubtree returns the settings that shape translation.
func (s *Stage) ConfigSubtree(cfg *config.Config) any {
	return struct {
		Translation config.Translation `yaml:"translation"`
		Subtitle    config.Subtitle    `yaml:"subtitle"`
		Prompt      string             `yaml:"prompt"`
	}{cfg.Translation, cfg.Subtitle, translationcore.DefaultPromptName}
}

// Fingerprint records the prompt and the glossary.
//
// Both change the output without changing anything the artifact key already
// covers, and getting this wrong is quiet: the stage would serve the previous
// translations from cache and a user who edited a term or improved a prompt
// would see no effect and conclude the edit was ignored.
func (s *Stage) Fingerprint(ctx context.Context, env *stage.Env) (map[string]string, error) {
	template, err := prompts.Load(translationcore.DefaultPromptName)
	if err != nil {
		return nil, err
	}

	fingerprint := map[string]string{"prompt": template.Version()}

	if env.Services.Glossary != nil {
		entries, err := env.Services.Glossary.List(ctx, env.ProjectID, false)
		if err != nil {
			return nil, err
		}
		digest, err := glossary.Hash(entries)
		if err != nil {
			return nil, err
		}
		fingerprint["glossary"] = digest
	}

	return fingerprint, nil
}

// Run translates the lines.
func (s *Stage) Run(ctx context.Context, env *stage.Env) (*stage.Result, error) {
	var set subtitle.Set
	if err := env.ReadInput("segmentation", &set); err != nil {
		return nil, fmt.Errorf("translation: %w", err)
	}
	if len(set.Segments) == 0 {
		return nil, errors.New("translation: there are no lines to translate")
	}

	client, providerName, err := env.Services.LLM(ctx, env.Config)
	if err != nil {
		return nil, fmt.Errorf("translation: %w", err)
	}
	if env.Services.Translation == nil {
		return nil, errors.New("translation: no line cache is available")
	}

	contextDocument, err := s.contextDocument(env)
	if err != nil {
		return nil, err
	}

	entries, err := s.glossaryEntries(ctx, env)
	if err != nil {
		return nil, err
	}

	translator, err := translationcore.New(client, env.Services.Translation, providerName, env.Log)
	if err != nil {
		return nil, fmt.Errorf("translation: %w", err)
	}

	cfg := env.Config.Translation
	style := env.Project.Style
	if style == "" {
		style = cfg.Style
	}

	env.Log.Info("translating",
		"lines", len(set.Segments),
		"model", client.Model(),
		"style", style,
		"glossary_entries", len(entries),
		"has_context", contextDocument != "")

	outcome, err := translator.Translate(ctx, set.Segments, translationcore.Options{
		SourceLanguage:  set.SourceLanguage,
		TargetLanguage:  set.TargetLanguage,
		Style:           style,
		BatchSize:       cfg.BatchSize,
		ContextLines:    cfg.ContextLines,
		Concurrency:     cfg.Concurrency,
		Temperature:     cfg.Temperature,
		MaxTokensPerJob: cfg.MaxTokensPerJob,
		MaxAttempts:     maxAttempts(cfg),
		ContextDocument: contextDocument,
		Glossary:        entries,
		Progress: func(done, total int) {
			// The last tenth belongs to the timing pass that follows.
			if total > 0 {
				env.ReportProgress(0.9*float64(done)/float64(total), "translating")
			}
		},
	})
	if err != nil {
		// The translations already applied to the segments are real work and
		// are paid for. They are still written, because the stage failing does
		// not unpublish the artifact — the pipeline does — and the caller
		// decides from the outcome whether the partial result is worth keeping.
		if outcome != nil {
			env.Log.Warn("translation stopped early",
				"translated", outcome.Translated, "from_cache", outcome.FromCache,
				"failed", len(outcome.Failed))
		}
		return nil, fmt.Errorf("translation: %w", err)
	}

	s.markFailures(&set, outcome)

	env.ReportProgress(0.9, "adjusting the timing")

	reflow := subtitle.Reflow(set.Segments, reflowOptions(env.Config))
	if len(reflow.Unresolved) > 0 {
		env.Log.Warn("some lines are still too fast to read after borrowing the available time",
			"lines", len(reflow.Unresolved))
	}

	if err := writeSet(env.OutDir, &set); err != nil {
		return nil, err
	}

	env.ReportProgress(1, "done")

	return &stage.Result{
		Primary:       PayloadName,
		Provider:      providerName,
		Model:         client.Model(),
		PromptVersion: outcome.PromptVersion,
		Metadata: map[string]any{
			"line_count":        len(set.Segments),
			"translated":        outcome.Translated,
			"from_cache":        outcome.FromCache,
			"failed":            len(outcome.Failed),
			"needs_review":      len(set.NeedsReview()),
			"reflowed":          reflow.Extended,
			"still_over_cps":    len(reflow.Unresolved),
			"llm_requests":      outcome.LLMRequests,
			"prompt_tokens":     outcome.PromptTokens,
			"completion_tokens": outcome.CompletionTokens,
			"style":             style,
			"glossary_entries":  len(entries),
			"has_context":       contextDocument != "",
			"elapsed_s":         outcome.Elapsed.Seconds(),
		},
	}, nil
}

// contextDocument reads the optional analysis artifact.
//
// Absence is not an error. The stage declares the dependency as optional
// precisely so that a project without an analysis runs here with an empty
// context and a prompt that says so.
func (s *Stage) contextDocument(env *stage.Env) (string, error) {
	artifact, ok := env.Inputs["context"]
	if !ok || artifact == nil {
		return "", nil
	}

	var document analysis.Document
	if err := env.ReadInput("context", &document); err != nil {
		return "", fmt.Errorf("translation: %w", err)
	}
	return document.Rendered, nil
}

// glossaryEntries reads the terminology in scope.
func (s *Stage) glossaryEntries(ctx context.Context, env *stage.Env) ([]glossary.Entry, error) {
	if env.Services.Glossary == nil {
		return nil, nil
	}
	entries, err := env.Services.Glossary.List(ctx, env.ProjectID, false)
	if err != nil {
		return nil, fmt.Errorf("translation: %w", err)
	}
	return entries, nil
}

// markFailures flags the lines the model never returned text for.
//
// They are not an error and they do not stop the render. A project that is 99%
// translated is worth keeping, and the alternative — failing the whole job
// because one line out of two thousand came back empty — throws away work the
// user has already paid for. The line is shown in the source language and
// carries a tag, so the review queue can find it.
func (s *Stage) markFailures(set *subtitle.Set, outcome *translationcore.Outcome) {
	if len(outcome.Failed) == 0 {
		return
	}

	failed := make(map[string]bool, len(outcome.Failed))
	for _, id := range outcome.Failed {
		failed[id] = true
	}

	for _, segment := range set.Segments {
		if failed[segment.ID] {
			segment.NeedsReview = true
			segment.AddTag(subtitle.TagTranslationFailed)
		}
	}
}

// maxAttempts resolves how many times one batch may be asked for.
func maxAttempts(cfg config.Translation) int {
	// The configured retry count is for transient failures; one more attempt is
	// added because a batch that came back malformed deserves a correction
	// round even when the user has turned retries off.
	if cfg.MaxRetries <= 0 {
		return 3
	}
	return cfg.MaxRetries + 1
}

// reflowOptions maps configuration onto the timing correction.
func reflowOptions(cfg *config.Config) subtitle.ReflowOptions {
	return subtitle.ReflowOptions{
		MaxCPS:      cfg.Subtitle.MaxCPS,
		MaxDuration: cfg.Subtitle.MaxDuration,
		MinGap:      cfg.Subtitle.MinGap,
	}
}

// writeSet writes the artifact payload.
func writeSet(dir string, set *subtitle.Set) error {
	encoded, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return fmt.Errorf("translation: encode the lines: %w", err)
	}

	final := filepath.Join(dir, PayloadName)
	temporary := final + ".tmp"

	if err := os.WriteFile(temporary, encoded, 0o644); err != nil {
		return fmt.Errorf("translation: write the lines: %w", err)
	}
	if err := os.Rename(temporary, final); err != nil {
		return fmt.Errorf("translation: write the lines: %w", err)
	}
	return nil
}
