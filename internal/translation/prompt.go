package translation

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/AhsokaTano26/NikuCooker/internal/subtitle"
)

// promptData fills the translation template.
type promptData struct {
	Context       string
	Glossary      string
	StyleGuidance string
	ContextBefore string
	ContextAfter  string
	Lines         string
}

// linePayload is one line as the model sees it.
//
// A JSON object rather than a delimited list. A model reproducing an id from
// `seg_0001: こんにちは` will sometimes drop the id; a model reproducing an id
// from a JSON field returns the field. The format is part of the prompt
// contract, and the prompt version covers it.
type linePayload struct {
	ID         string `json:"id"`
	SourceText string `json:"source_text"`
}

// batchPayload is the document handed to the model as the work to do.
type batchPayload struct {
	SourceLanguage string        `json:"source_language"`
	TargetLanguage string        `json:"target_language"`
	Lines          []linePayload `json:"lines"`
}

// renderLines encodes the batch the model must translate.
func renderLines(lines []*line, sourceLanguage, targetLanguage string) (string, error) {
	payload := batchPayload{
		SourceLanguage: sourceLanguage,
		TargetLanguage: targetLanguage,
		Lines:          make([]linePayload, 0, len(lines)),
	}
	for _, item := range lines {
		payload.Lines = append(payload.Lines, linePayload{
			ID:         item.segment.ID,
			SourceText: item.segment.SourceText,
		})
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("translation: encode the batch: %w", err)
	}
	return string(raw), nil
}

// renderContextLines formats neighbouring lines as read-only context.
//
// Plain numbered text rather than JSON: this is background, not work, and the
// less it resembles the payload the model must transform, the less likely it is
// to be translated and returned.
func renderContextLines(segments []*subtitle.Segment) string {
	if len(segments) == 0 {
		return ""
	}

	var b strings.Builder
	for _, segment := range segments {
		fmt.Fprintf(&b, "%s: %s\n", segment.ID, segment.SourceText)
	}
	return strings.TrimRight(b.String(), "\n")
}

// correctionNote describes what was wrong with a previous reply.
//
// Included in the retry so the model is given the chance to fix a specific
// omission rather than being asked the same question again, which tends to
// produce the same answer.
func correctionNote(validation BatchValidation) string {
	var b strings.Builder
	b.WriteString("\n\n## Correction\n\nYour previous reply was not usable. ")

	switch {
	case len(validation.Missing) > 0 && len(validation.Empty) > 0:
		fmt.Fprintf(&b, "It omitted these ids entirely: %s. It returned empty text for: %s. ",
			strings.Join(validation.Missing, ", "), strings.Join(validation.Empty, ", "))
	case len(validation.Missing) > 0:
		fmt.Fprintf(&b, "It omitted these ids entirely: %s. ",
			strings.Join(validation.Missing, ", "))
	case len(validation.Empty) > 0:
		fmt.Fprintf(&b, "It returned empty text for these ids: %s. ",
			strings.Join(validation.Empty, ", "))
	}
	if len(validation.Extra) > 0 {
		fmt.Fprintf(&b, "It also returned ids that were not asked for: %s. ",
			strings.Join(validation.Extra, ", "))
	}

	b.WriteString("Reply again with JSON only, covering every remaining line exactly once.")
	return b.String()
}
