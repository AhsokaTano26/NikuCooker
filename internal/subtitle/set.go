package subtitle

import (
	"fmt"

	"github.com/AhsokaTano26/NikuCooker/pkg/protocol"
)

// Set is a project's subtitle lines with the languages they are in.
//
// It is the artifact segmentation produces and every later stage reads, and it
// exists so that the language pair travels with the lines. The alternative —
// every consumer re-reading the project configuration — would mean two stages
// could disagree about which language they are working in, and the disagreement
// would look like a translation bug.
type Set struct {
	SourceLanguage string `json:"source_language"`
	TargetLanguage string `json:"target_language"`

	Segments []*Segment `json:"segments"`

	// Source is the recognition artifact this set was built from. It is
	// provenance, and it is what makes "which transcript produced these lines"
	// answerable after a re-run.
	Source SourceProvenance `json:"source"`
}

// SourceProvenance records where the lines came from.
type SourceProvenance struct {
	ASRArtifactID string `json:"asr_artifact_id,omitempty"`
	ASRModel      string `json:"asr_model,omitempty"`
	SegmentCount  int    `json:"asr_segment_count,omitempty"`
	WordCount     int    `json:"asr_word_count,omitempty"`
}

// Validate checks the whole set.
func (s *Set) Validate() error {
	if s.SourceLanguage == "" || s.TargetLanguage == "" {
		return fmt.Errorf("subtitle: the set does not say which languages it is in")
	}
	return ValidateSet(s.Segments)
}

// Clone returns a deep copy.
func (s *Set) Clone() *Set {
	out := *s
	out.Segments = make([]*Segment, 0, len(s.Segments))
	for _, segment := range s.Segments {
		out.Segments = append(out.Segments, segment.Clone())
	}
	return &out
}

// BuildFromASR builds a set from recognition output.
func BuildFromASR(
	asr []protocol.ASRSegment,
	sourceLanguage, targetLanguage string,
	opts Options,
) (*Set, error) {
	segments, err := Build(asr, sourceLanguage, targetLanguage, opts)
	if err != nil {
		return nil, err
	}
	return &Set{
		SourceLanguage: sourceLanguage,
		TargetLanguage: targetLanguage,
		Segments:       segments,
		Source:         SourceProvenance{SegmentCount: len(asr)},
	}, nil
}

// MaxCPS returns the highest reading speed in the set, or zero when it is empty.
func (s *Set) MaxCPS() float64 {
	highest := 0.0
	for _, segment := range s.Segments {
		if segment.CPS != nil && *segment.CPS > highest {
			highest = *segment.CPS
		}
	}
	return highest
}

// CountTranslated reports how many lines carry a translation.
func (s *Set) CountTranslated() int {
	count := 0
	for _, segment := range s.Segments {
		if segment.TranslatedText != nil && *segment.TranslatedText != "" {
			count++
		}
	}
	return count
}

// NeedsReview returns the ids of lines flagged for a human to look at.
func (s *Set) NeedsReview() []string {
	var ids []string
	for _, segment := range s.Segments {
		if segment.NeedsReview {
			ids = append(ids, segment.ID)
		}
	}
	return ids
}
