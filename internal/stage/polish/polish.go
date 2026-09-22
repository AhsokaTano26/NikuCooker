// Package polish runs a second language-model pass over the finished
// translation.
//
// It is off by default, and the reason is cost rather than quality: it is a
// full extra pass over every line, roughly doubling what a project costs to
// translate. Where a project is worth a human's review anyway — a release, a
// commission, anything with a deadline attached — the improvement is real,
// because a model asked to improve an existing translation has more to work
// with than one asked to produce it cold.
//
// It is a separate stage rather than another option on translation so that it
// can be turned on for one project and not the next, and so that its cost shows
// up as its own line in the run's record.
package polish

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
const PayloadName = "polished.json"

// PromptName is the template this stage uses.
const PromptName = "polish/ja_zh_v1"

// Stage revises the translated lines.
type Stage struct{}

// New builds the stage.
func New() *Stage { return &Stage{} }

// Spec describes the stage.
func (s *Stage) Spec() stage.Spec {
	return stage.Spec{
		Name:    "polish",
		Version: "1",
		Depends: []string{"translation"},
		// Optional, and the only stage the default configuration disables.
		Optional: true,
		// The context document improves a revising pass for the same reason it
		// improves a translating one, and is just as optional here.
		OptionalDepends: []string{"context"},
		ConfigKey:       "polish",
	}
}

// ConfigSubtree returns the settings that shape the requests.
func (s *Stage) ConfigSubtree(cfg *config.Config) any {
	return struct {
		Translation config.Translation `yaml:"translation"`
		Subtitle    config.Subtitle    `yaml:"subtitle"`
		Prompt      string             `yaml:"prompt"`
	}{cfg.Translation, cfg.Subtitle, PromptName}
}

// Fingerprint records the prompt and the glossary, for the same reason
// translation does: both change the output without changing anything the
// artifact key already covers.
func (s *Stage) Fingerprint(ctx context.Context, env *stage.Env) (map[string]string, error) {
	template, err := prompts.Load(PromptName)
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

// Run revises the lines.
func (s *Stage) Run(ctx context.Context, env *stage.Env) (*stage.Result, error) {
	var set subtitle.Set
	if err := env.ReadInput("translation", &set); err != nil {
		return nil, fmt.Errorf("polish: %w", err)
	}
	if len(set.Segments) == 0 {
		return nil, errors.New("polish: there are no lines to revise")
	}

	client, providerName, err := env.Services.LLM(ctx, env.Config)
	if err != nil {
		return nil, fmt.Errorf("polish: %w", err)
	}
	if env.Services.Translation == nil {
		return nil, errors.New("polish: no line cache is available")
	}

	contextDocument := ""
	if artifact, ok := env.Inputs["context"]; ok && artifact != nil {
		var document analysis.Document
		if err := env.ReadInput("context", &document); err != nil {
			return nil, fmt.Errorf("polish: %w", err)
		}
		contextDocument = document.Rendered
	}

	var entries []glossary.Entry
	if env.Services.Glossary != nil {
		entries, err = env.Services.Glossary.List(ctx, env.ProjectID, false)
		if err != nil {
			return nil, fmt.Errorf("polish: %w", err)
		}
	}

	// The revision happens on a copy. The upstream set is an artifact another
	// stage produced, and editing it in place would mutate a decoded value
	// whose lifetime the caller still owns — a change that is invisible until
	// two stages disagree about the same lines.
	revised := set.Clone()

	translator, err := translationcore.New(client, env.Services.Translation, providerName, env.Log)
	if err != nil {
		return nil, fmt.Errorf("polish: %w", err)
	}

	cfg := env.Config.Translation
	style := env.Project.Style
	if style == "" {
		style = cfg.Style
	}

	env.Log.Info("polishing", "lines", len(revised.Segments), "model", client.Model())

	outcome, err := translator.Translate(ctx, revised.Segments, translationcore.Options{
		SourceLanguage:  revised.SourceLanguage,
		TargetLanguage:  revised.TargetLanguage,
		Style:           style,
		PromptName:      PromptName,
		BatchSize:       cfg.BatchSize,
		ContextLines:    cfg.ContextLines,
		Concurrency:     cfg.Concurrency,
		Temperature:     cfg.Temperature,
		MaxTokensPerJob: cfg.MaxTokensPerJob,
		MaxAttempts:     maxAttempts(cfg),
		ContextDocument: contextDocument,
		Glossary:        entries,
		// The whole point of this stage.
		Revise: true,
		Progress: func(done, total int) {
			if total > 0 {
				env.ReportProgress(float64(done)/float64(total), "polishing")
			}
		},
	})
	if err != nil {
		return nil, fmt.Errorf("polish: %w", err)
	}

	if err := writeSet(env.OutDir, revised); err != nil {
		return nil, err
	}

	env.ReportProgress(1, "done")

	return &stage.Result{
		Primary:       PayloadName,
		Provider:      providerName,
		Model:         client.Model(),
		PromptVersion: outcome.PromptVersion,
		Metadata: map[string]any{
			"line_count":        len(revised.Segments),
			"revised":           outcome.Translated,
			"from_cache":        outcome.FromCache,
			"failed":            len(outcome.Failed),
			"llm_requests":      outcome.LLMRequests,
			"prompt_tokens":     outcome.PromptTokens,
			"completion_tokens": outcome.CompletionTokens,
			"has_context":       contextDocument != "",
			"elapsed_s":         outcome.Elapsed.Seconds(),
		},
	}, nil
}

// maxAttempts resolves how many times one batch may be asked for.
func maxAttempts(cfg config.Translation) int {
	if cfg.MaxRetries <= 0 {
		return 3
	}
	return cfg.MaxRetries + 1
}

// writeSet writes the artifact payload.
func writeSet(dir string, set *subtitle.Set) error {
	encoded, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return fmt.Errorf("polish: encode the lines: %w", err)
	}

	final := filepath.Join(dir, PayloadName)
	if err := os.WriteFile(final+".tmp", encoded, 0o644); err != nil {
		return fmt.Errorf("polish: write the lines: %w", err)
	}
	if err := os.Rename(final+".tmp", final); err != nil {
		return fmt.Errorf("polish: write the lines: %w", err)
	}
	return nil
}
