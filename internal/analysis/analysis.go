// Package analysis describes a work to the translator.
//
// A translator who has watched an episode knows who the characters are, what the
// show's register is, and which words are names. A model asked to translate line
// 412 in isolation knows none of that, and the result reads like it: pronouns
// resolved wrongly, a name rendered three different ways, a joke flattened
// because its setup was four lines earlier and had already been translated
// without it.
//
// So the transcript is read once, up front, and turned into a document that
// travels with every translation request. It is a projection of the structured
// analysis rather than the model's prose, so that the same analysis always
// produces the same document — which is what lets the translation cache key on
// it without the key changing every run.
package analysis

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/AhsokaTano26/NikuCooker/internal/artifact"
	"github.com/AhsokaTano26/NikuCooker/internal/llmjson"
)

// Document is what a translator needs to know about the work.
type Document struct {
	Summary string `json:"summary"`
	Setting string `json:"setting"`
	Tone    string `json:"tone"`

	Characters []Character `json:"characters"`
	Terms      []Term      `json:"terms"`
	Notes      []string    `json:"notes"`

	// Rendered is the document as it appears in a translation prompt. It is
	// stored rather than recomputed so that the text a translation was produced
	// against can be read back exactly.
	Rendered string `json:"rendered,omitempty"`

	// Model names what produced the analysis, for provenance.
	Model string `json:"model,omitempty"`
}

// Character is a person in the work.
type Character struct {
	// NameJA is the name as it appears in the transcript.
	NameJA string `json:"name_ja"`

	// NameZH is the Chinese rendering to use consistently.
	NameZH string `json:"name_zh"`

	// Role is who they are, in a few words.
	Role string `json:"role,omitempty"`
}

// Term is a recurring name, place or title whose rendering must not drift.
type Term struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Type   string `json:"type,omitempty"`
	Note   string `json:"note,omitempty"`
}

// Parse decodes a model's analysis reply.
//
// Tolerant of a fenced or prose-wrapped reply, and of the model omitting fields
// it found nothing to say about. It is not tolerant of a reply with no JSON in
// it, because that means the analysis did not happen and inventing an empty
// document would silently translate a whole episode with no context at all.
func Parse(reply string) (*Document, error) {
	raw, err := llmjson.Extract(reply)
	if err != nil {
		return nil, fmt.Errorf("analysis: %w", err)
	}

	var document Document
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("analysis: the model's reply was not an analysis document: %w", err)
	}

	document.normalise()
	if document.Summary == "" && document.Setting == "" && document.Tone == "" &&
		len(document.Characters) == 0 && len(document.Terms) == 0 && len(document.Notes) == 0 {
		return nil, fmt.Errorf("analysis: the model returned an empty analysis")
	}

	rendered, err := document.Render()
	if err != nil {
		return nil, err
	}
	document.Rendered = rendered

	return &document, nil
}

// normalise tidies the model's output into a canonical form.
//
// Sorting is not cosmetic. The rendered document is what the translation cache
// keys on, and a model that returns the same six characters in a different order
// on a re-run would otherwise invalidate every cached translation in the
// project for no change in meaning.
func (d *Document) normalise() {
	kept := make([]Character, 0, len(d.Characters))
	for _, character := range d.Characters {
		character.NameJA = strings.TrimSpace(character.NameJA)
		character.NameZH = strings.TrimSpace(character.NameZH)
		character.Role = strings.TrimSpace(character.Role)

		// A character with no name is not one; a character with no Chinese
		// rendering tells the translator nothing it could not have guessed.
		if character.NameJA == "" || character.NameZH == "" {
			continue
		}
		kept = append(kept, character)
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].NameJA < kept[j].NameJA })
	d.Characters = kept

	terms := make([]Term, 0, len(d.Terms))
	seen := make(map[string]bool, len(d.Terms))
	for _, term := range d.Terms {
		term.Source = strings.TrimSpace(term.Source)
		term.Target = strings.TrimSpace(term.Target)
		term.Note = strings.TrimSpace(term.Note)
		term.Type = strings.TrimSpace(term.Type)

		if term.Source == "" || term.Target == "" || term.Source == term.Target {
			continue
		}
		// The model sometimes lists a term twice under different types.
		if seen[term.Source] {
			continue
		}
		seen[term.Source] = true
		terms = append(terms, term)
	}
	sort.Slice(terms, func(i, j int) bool { return terms[i].Source < terms[j].Source })
	d.Terms = terms

	notes := make([]string, 0, len(d.Notes))
	for _, note := range d.Notes {
		if trimmed := strings.TrimSpace(note); trimmed != "" {
			notes = append(notes, trimmed)
		}
	}
	sort.Strings(notes)
	d.Notes = notes

	d.Summary = strings.TrimSpace(d.Summary)
	d.Setting = strings.TrimSpace(d.Setting)
	d.Tone = strings.TrimSpace(d.Tone)
}

// Render produces the document as it appears in a translation prompt.
//
// The output is a function of the structured fields alone — no timestamps, no
// model name, no map iteration — so that re-running the analysis against an
// unchanged transcript produces an identical string and the translation cache
// survives it.
func (d *Document) Render() (string, error) {
	var b strings.Builder

	writeSection := func(heading, body string) {
		body = strings.TrimSpace(body)
		if body == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "%s\n%s", heading, body)
	}

	writeSection("Summary:", d.Summary)
	writeSection("Setting:", d.Setting)
	writeSection("Tone:", d.Tone)

	if len(d.Characters) > 0 {
		var lines []string
		for _, character := range d.Characters {
			line := fmt.Sprintf("- %s (%s)", character.NameZH, character.NameJA)
			if character.Role != "" {
				line += ": " + character.Role
			}
			lines = append(lines, line)
		}
		writeSection("Characters:", strings.Join(lines, "\n"))
	}

	if len(d.Terms) > 0 {
		var lines []string
		for _, term := range d.Terms {
			line := fmt.Sprintf("- %s → %s", term.Source, term.Target)
			if term.Type != "" {
				line += fmt.Sprintf(" (%s)", term.Type)
			}
			if term.Note != "" {
				line += " — " + term.Note
			}
			lines = append(lines, line)
		}
		writeSection("Terms:", strings.Join(lines, "\n"))
	}

	if len(d.Notes) > 0 {
		var lines []string
		for _, note := range d.Notes {
			lines = append(lines, "- "+note)
		}
		writeSection("Notes:", strings.Join(lines, "\n"))
	}

	rendered := strings.TrimSpace(b.String())
	if rendered == "" {
		return "", fmt.Errorf("analysis: the document has nothing to say")
	}
	return rendered, nil
}

// Proposals returns the glossary entries the analysis suggests.
//
// They are suggestions, not decisions. A name the analyst misheard would
// propagate into every line of every later episode if it were applied
// automatically, so the terms are offered to the user and take effect when
// accepted. Until then the document still carries them, so a single run is
// consistent either way.
func (d *Document) Proposals() []Term {
	out := make([]Term, 0, len(d.Characters)+len(d.Terms))

	for _, character := range d.Characters {
		out = append(out, Term{
			Source: character.NameJA,
			Target: character.NameZH,
			Type:   "character",
			Note:   character.Role,
		})
	}
	out = append(out, d.Terms...)

	return out
}

// Hash digests the document for use as a cache key component.
//
// The rendered text, not the struct: it is what the model actually saw, so a
// change to the rendering that alters the prompt invalidates the cache even
// though the parsed fields are identical.
func (d *Document) Hash() (string, error) {
	if d == nil {
		return "", nil
	}
	return artifact.FingerprintBytes("analysis document", d.Rendered)
}
