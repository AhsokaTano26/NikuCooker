// Package context reads the transcript and describes the work.
//
// It is the one place in the pipeline where a model is asked a question rather
// than given a task, and it exists because translation quality is bounded by
// what the translator knows. A model that has seen a summary of the episode, the
// cast list and the show's register translates the same line differently — and
// better — than one that has only the line.
//
// The stage is optional. A project with no provider configured, or a user who
// does not want to pay for the pass, disables it and translation runs without a
// context document. It is not a dependency of anything that cannot do without
// it.
package context

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AhsokaTano26/NikuCooker/internal/analysis"
	"github.com/AhsokaTano26/NikuCooker/internal/config"
	"github.com/AhsokaTano26/NikuCooker/internal/provider"
	"github.com/AhsokaTano26/NikuCooker/internal/stage"
	"github.com/AhsokaTano26/NikuCooker/internal/subtitle"
	"github.com/AhsokaTano26/NikuCooker/prompts"
)

// PayloadName is the file this stage writes.
const PayloadName = "context.json"

// PromptName is the analysis template.
const PromptName = "analysis/ja_zh_v1"

// Sampling limits.
//
// A feature-length transcript is far more than the analysis needs and more than
// a context window will hold. Sampling evenly across the whole work is what
// makes the result describe *the work* rather than its first ten minutes — which
// is exactly the mistake the pass exists to avoid.
const (
	maxSampleLines = 400
	maxSampleChars = 24000
)

// Stage produces the context document.
type Stage struct{}

// New builds the stage.
func New() *Stage { return &Stage{} }

// Spec describes the stage.
func (s *Stage) Spec() stage.Spec {
	return stage.Spec{
		Name:    "context",
		Version: "1",
		Depends: []string{"segmentation"},
		// Optional: the pipeline runs without it, and translation says in its
		// prompt that no context was available.
		Optional:  true,
		ConfigKey: "translation",
	}
}

// ConfigSubtree returns the settings that shape the request.
func (s *Stage) ConfigSubtree(cfg *config.Config) any {
	return struct {
		Translation config.Translation `yaml:"translation"`
		Prompt      string             `yaml:"prompt"`
	}{cfg.Translation, PromptName}
}

// Fingerprint records the prompt, because it is an input that is neither
// configuration nor an upstream artifact.
//
// The prompt's content digest is what makes editing the analysis instructions
// recompute the document. Without it the stage would serve the previous
// document from cache and the edit would appear to do nothing.
func (s *Stage) Fingerprint(context.Context, *stage.Env) (map[string]string, error) {
	template, err := prompts.Load(PromptName)
	if err != nil {
		return nil, err
	}
	return map[string]string{"prompt": template.Version()}, nil
}

// Run analyses the transcript.
func (s *Stage) Run(ctx context.Context, env *stage.Env) (*stage.Result, error) {
	var set subtitle.Set
	if err := env.ReadInput("segmentation", &set); err != nil {
		return nil, fmt.Errorf("context: %w", err)
	}
	if len(set.Segments) == 0 {
		return nil, errors.New("context: there are no lines to analyse")
	}

	client, providerName, err := env.Services.LLM(ctx, env.Config)
	if err != nil {
		return nil, fmt.Errorf(
			"context: %w; configure a language-model provider or disable the context stage", err)
	}

	template, err := prompts.Load(PromptName)
	if err != nil {
		return nil, err
	}

	sampled := sample(set.Segments)
	env.Log.Info("analysing the transcript",
		"lines", len(set.Segments), "sampled", len(sampled), "model", client.Model())

	env.ReportProgress(0.1, "reading the transcript")

	reply, err := client.Chat(ctx, provider.ChatRequest{
		User: mustRender(template, map[string]string{
			"Transcript": renderTranscript(sampled),
		}),
		// Low but not zero. The analysis is a description, and a little
		// variation costs nothing; a temperature of zero makes some providers
		// slow and others refuse the request outright.
		Temperature: 0.2,

		// Sized for the reasoning, not for the answer.
		//
		// The document this stage asks for is a few hundred tokens. A model
		// that thinks before it answers spends several thousand doing it, and
		// those tokens come out of the same budget — measured on an eleven-line
		// transcript, 2934 completion tokens went out for a description of
		// about 600. A ceiling set to fit the answer alone truncates, and it
		// truncates intermittently, because how long a model deliberates varies
		// between runs of the same input.
		//
		// A ceiling is not a target: an unused one costs nothing.
		MaxTokens: 8192,

		// Asked for explicitly, because the default is not neutral on the
		// models that have this. DeepSeek's, for one, defaults to high, and it
		// is the difference between spending 3446 tokens on this and 2934.
		//
		// That is a real saving and not a fix on its own — the ceiling above is
		// what makes it safe. Low rather than off: some deliberation helps fix a
		// reading of an ambiguous name, and the OpenAI-compatible surface does
		// not offer "off" anyway.
		ReasoningEffort: "low",

		JSON: true,
	})
	if err != nil {
		return nil, fmt.Errorf("context: %w", err)
	}

	env.ReportProgress(0.8, "reading the analysis")

	document, err := analysis.Parse(reply.Content)
	if err != nil {
		return nil, fmt.Errorf("context: %w", err)
	}
	document.Model = reply.Model
	if document.Model == "" {
		document.Model = client.Model()
	}

	if err := writeDocument(env.OutDir, document); err != nil {
		return nil, err
	}

	env.ReportProgress(1, "done")

	proposals := document.Proposals()

	return &stage.Result{
		Primary:       PayloadName,
		Provider:      providerName,
		Model:         document.Model,
		PromptVersion: template.Version(),
		Metadata: map[string]any{
			"lines_analysed":    len(set.Segments),
			"lines_sampled":     len(sampled),
			"characters":        len(document.Characters),
			"terms":             len(document.Terms),
			"proposed_terms":    len(proposals),
			"prompt_tokens":     reply.PromptTokens,
			"completion_tokens": reply.CompletionTokens,
		},
	}, nil
}

// sample picks lines to analyse, evenly across the whole work.
//
// Deterministic on purpose. The rendered document is what the translation cache
// keys on, so a sample that varied between runs would produce a different
// document, a different key, and a full re-translation of a project whose
// transcript had not changed.
func sample(segments []*subtitle.Segment) []*subtitle.Segment {
	if len(segments) == 0 {
		return nil
	}

	limit := maxSampleLines
	if len(segments) < limit {
		limit = len(segments)
	}

	// Evenly spaced by position rather than taken in blocks. The first and last
	// lines are always included, because a work's opening and closing establish
	// more about it than any middle stretch.
	step := float64(len(segments)) / float64(limit)

	picked := make([]*subtitle.Segment, 0, limit)
	chars := 0

	for i := 0; i < limit; i++ {
		index := int(float64(i) * step)
		if index >= len(segments) {
			index = len(segments) - 1
		}
		segment := segments[index]

		// The character budget binds before the line budget on a transcript of
		// unusually long lines, and exceeding the model's context is a hard
		// failure rather than a degradation.
		if chars+len(segment.SourceText) > maxSampleChars && len(picked) > 0 {
			break
		}
		chars += len(segment.SourceText)
		picked = append(picked, segment)
	}

	return picked
}

// renderTranscript formats the sampled lines for the prompt.
func renderTranscript(segments []*subtitle.Segment) string {
	var b strings.Builder
	for _, segment := range segments {
		fmt.Fprintf(&b, "%s\t%s\n", segment.ID, segment.SourceText)
	}
	return strings.TrimRight(b.String(), "\n")
}

// mustRender fills a template that has already been loaded and whose variables
// are known to match.
func mustRender(template *prompts.Template, data map[string]string) string {
	rendered, err := template.Render(data)
	if err != nil {
		// Unreachable: the template is embedded, its variables are fixed, and
		// data is a map with the keys the template names. Reaching here means
		// the template and this call site have diverged, which is a build-time
		// mistake rather than a runtime condition.
		panic(fmt.Sprintf("context: the analysis prompt did not render: %v", err))
	}
	return rendered
}

// writeDocument writes the artifact payload.
func writeDocument(dir string, document *analysis.Document) error {
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("context: encode the analysis: %w", err)
	}

	final := filepath.Join(dir, PayloadName)
	temporary := final + ".tmp"

	if err := os.WriteFile(temporary, encoded, 0o644); err != nil {
		return fmt.Errorf("context: write the analysis: %w", err)
	}
	if err := os.Rename(temporary, final); err != nil {
		return fmt.Errorf("context: write the analysis: %w", err)
	}
	return nil
}
