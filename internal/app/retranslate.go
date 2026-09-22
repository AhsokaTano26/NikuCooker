package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/AhsokaTano26/NikuCooker/internal/config"
	"github.com/AhsokaTano26/NikuCooker/internal/glossary"
	"github.com/AhsokaTano26/NikuCooker/internal/segments"
	"github.com/AhsokaTano26/NikuCooker/internal/subtitle"
	translationcore "github.com/AhsokaTano26/NikuCooker/internal/translation"
)

// RetranslateLine asks the model for a new translation of one line.
//
// It bypasses the line cache by design. The cache exists to avoid paying twice
// for the same work, but a user pressing "retranslate" is asking for a
// *different* answer — and returning the cached one would be indistinguishable
// from a button that does nothing. The new result is not written back to the
// cache either, for the same reason: it would then be served to every other
// project containing the line.
func (a *App) RetranslateLine(ctx context.Context, projectID, segmentID, style string) (*segments.Record, error) {
	record, err := a.Segments.Get(ctx, projectID, segmentID)
	if err != nil {
		return nil, err
	}

	prj, err := a.Projects.Get(ctx, projectID)
	if err != nil {
		return nil, err
	}

	client, providerName, err := a.stageServices().LLM(ctx, a.cfg)
	if err != nil {
		return nil, fmt.Errorf("retranslate: %w", err)
	}

	if style == "" {
		style = prj.Style
	}
	if style == "" {
		style = a.cfg.Translation.Style
	}

	// The line's neighbours travel with it. A line translated in isolation
	// reads differently from one translated as part of a conversation, and a
	// user retranslating a line is usually fixing exactly that.
	contextSet, err := a.Segments.CurrentLines(ctx, projectID)
	if err != nil {
		return nil, err
	}

	entries, err := a.glossaryFor(ctx, projectID, record.SourceText)
	if err != nil {
		return nil, err
	}

	document, err := a.contextDocumentFor(ctx, projectID)
	if err != nil {
		return nil, err
	}

	segment := record.ToSegment()
	before, after := neighbours(contextSet, record.Ordinal)

	translator, err := translationcore.New(client, a.Cache, providerName, a.log)
	if err != nil {
		return nil, fmt.Errorf("retranslate: %w", err)
	}

	cfg := a.cfg.Translation
	outcome, err := translator.Translate(ctx, []*subtitle.Segment{segment}, translationcore.Options{
		SourceLanguage:  record.SourceLanguage,
		TargetLanguage:  record.TargetLanguage,
		Style:           style,
		BatchSize:       1,
		ContextLines:    0,
		Concurrency:     1,
		Temperature:     cfg.Temperature,
		MaxAttempts:     maxAttempts(cfg),
		ContextDocument: document,
		Glossary:        entries,

		// The point of the whole operation: the ordinary path skips a line that
		// already has a translation, and this is a request to replace one.
		Revise: true,

		// Reported through the surrounding text rather than through the
		// translator's own context mechanism, which needs a batch to attach to.
		Neighbourhood: &translationcore.Neighbourhood{Before: before, After: after},
	})
	if err != nil {
		return nil, fmt.Errorf("retranslate: %w", err)
	}

	if segment.TranslatedText == nil || strings.TrimSpace(*segment.TranslatedText) == "" {
		// The model returned nothing usable. Reported rather than silently
		// leaving the old translation in place, which would look like the
		// button had worked.
		return nil, fmt.Errorf("retranslate: the model returned no translation for this line")
	}
	_ = outcome

	updated, err := a.Segments.Apply(ctx, projectID, segmentID, segments.Edit{
		TranslatedText: segment.TranslatedText,
	})
	if err != nil {
		return nil, err
	}

	return updated, nil
}

// glossaryFor loads the entries that apply to a line.
func (a *App) glossaryFor(ctx context.Context, projectID, text string) ([]glossary.Entry, error) {
	if a.Glossary == nil {
		return nil, nil
	}

	entries, err := a.Glossary.List(ctx, projectID, false)
	if err != nil {
		return nil, err
	}
	return glossary.Match(entries, []string{text}), nil
}

// contextDocumentFor reads a project's analysis, if one exists.
func (a *App) contextDocumentFor(ctx context.Context, projectID string) (string, error) {
	artifact, err := a.Artifacts.For(projectID).Latest(ctx, "context")
	if err != nil {
		return "", err
	}
	if artifact == nil {
		// No analysis pass has run. Not an error: the translation works without
		// one, and the prompt says so.
		return "", nil
	}

	var document analysisDocument
	if err := a.Artifacts.For(projectID).Decode(artifact, &document); err != nil {
		a.log.Warn("could not read the context document", "error", err)
		return "", nil
	}
	return document.Rendered, nil
}

// neighbours returns the lines around an ordinal, as plain text.
func neighbours(set *subtitle.Set, ordinal int) (before, after []string) {
	if set == nil {
		return nil, nil
	}

	const window = 2

	for i := ordinal - window; i < ordinal-1; i++ {
		if i >= 0 && i < len(set.Segments) {
			before = append(before, set.Segments[i].SourceText)
		}
	}
	for i := ordinal; i < ordinal+window; i++ {
		if i >= 0 && i < len(set.Segments) {
			after = append(after, set.Segments[i].SourceText)
		}
	}
	return before, after
}

// analysisDocument is the subset of the analysis payload this package needs.
//
// Declared here rather than imported so that the app does not depend on the
// analysis package's full shape to read one field.
type analysisDocument struct {
	Rendered string `json:"rendered"`
}

// maxAttempts mirrors the translation stage's rule: one more attempt than the
// configured retries, because a malformed reply deserves a correction round
// even when retries are off.
func maxAttempts(cfg config.Translation) int {
	if cfg.MaxRetries <= 0 {
		return 3
	}
	return cfg.MaxRetries + 1
}
