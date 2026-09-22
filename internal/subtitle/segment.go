// Package subtitle holds the system's central data model and the rules that
// turn recognition output into something a person can read.
//
// A Whisper segment is not a subtitle line. It is several seconds of continuous
// speech, often with several sentences in it, and printing it verbatim produces
// text that is technically correct and unpleasant to watch. The work of this
// package is the difference between the two.
package subtitle

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/AhsokaTano26/NikuCooker/pkg/protocol"
)

// WordTimestamp is one word with its timing.
//
// An alias rather than a parallel type: the recogniser produces this shape and
// everything downstream consumes it, so a second structure with the same fields
// would be a second place to update and the one that gets forgotten.
type WordTimestamp = protocol.WordTimestamp

// Segment is one subtitle line.
//
// This is the single most important shared type in the system. It is defined
// once here in Go and once in the Python protocol models, and the two are held
// compatible by the fixtures both suites read.
type Segment struct {
	ID string `json:"id"`

	// Start and End are seconds from the beginning of the media.
	Start float64 `json:"start"`
	End   float64 `json:"end"`

	// Speaker is nil unless diarization ran, which it does not in v1.
	Speaker *string `json:"speaker,omitempty"`

	SourceLanguage string `json:"source_language"`
	TargetLanguage string `json:"target_language"`

	SourceText string `json:"source_text"`

	// TranslatedText is a pointer because "not translated" and "translated to
	// the empty string" are different facts, and QC treats them differently.
	TranslatedText *string `json:"translated_text,omitempty"`

	// Words are the timings for this line's text. They survive segmentation:
	// each line carries the subset of words that fall within it, which is what
	// lets a later stage re-split without going back to the recogniser.
	Words []WordTimestamp `json:"words"`

	// ASRConfidence is Whisper's average log probability for the segment: a log
	// probability, so negative, and higher is better. It is not a 0–1 score.
	ASRConfidence *float64 `json:"asr_confidence,omitempty"`

	TranslationConfidence *float64 `json:"translation_confidence,omitempty"`

	// CPS is characters per second, computed against the translation when there
	// is one and the source text otherwise.
	CPS *float64 `json:"cps,omitempty"`

	NeedsReview bool     `json:"needs_review"`
	Tags        []string `json:"tags"`

	// Metadata carries diagnostics — which ASR segment this came from, which
	// rule split it. Nothing in the pipeline may *depend* on a metadata key:
	// that would be an undeclared interface between stages.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Duration is the line's on-screen time in seconds.
func (s *Segment) Duration() float64 { return s.End - s.Start }

// EffectiveText is what the viewer reads.
//
// The translation when there is one, because that is what determines whether a
// line is readable on screen; the source otherwise, so that CPS and length
// checks work before translation runs.
func (s *Segment) EffectiveText() string {
	if s.TranslatedText != nil {
		return *s.TranslatedText
	}
	return s.SourceText
}

// RecomputeCPS refreshes the reading-speed figure.
//
// Characters per second, which for Japanese and Chinese is roughly what a
// viewer can track. The target is around 9 and the ceiling around 18; above
// that a line is on screen for less time than it takes to read.
func (s *Segment) RecomputeCPS() {
	duration := s.Duration()
	if duration <= 0 {
		s.CPS = nil
		return
	}

	// Counted in runes, not bytes: a Japanese line is three bytes per character
	// in UTF-8, and a byte count would report a reading speed three times the
	// real one.
	text := strings.TrimSpace(s.EffectiveText())
	count := 0
	for _, r := range text {
		// Whitespace is not read.
		if !unicode.IsSpace(r) {
			count++
		}
	}

	cps := float64(count) / duration
	s.CPS = &cps
}

// SetTranslation records a translation and refreshes the derived figures.
func (s *Segment) SetTranslation(text string) {
	s.TranslatedText = &text
	s.RecomputeCPS()
}

// Clone returns a deep copy.
//
// Segmentation and translation both need to pass segments around without one
// caller's edit reaching another's copy, and the pointers in this struct make
// a shallow copy a trap.
func (s *Segment) Clone() *Segment {
	out := *s

	if s.Speaker != nil {
		speaker := *s.Speaker
		out.Speaker = &speaker
	}
	if s.TranslatedText != nil {
		text := *s.TranslatedText
		out.TranslatedText = &text
	}
	if s.ASRConfidence != nil {
		v := *s.ASRConfidence
		out.ASRConfidence = &v
	}
	if s.TranslationConfidence != nil {
		v := *s.TranslationConfidence
		out.TranslationConfidence = &v
	}
	if s.CPS != nil {
		v := *s.CPS
		out.CPS = &v
	}

	out.Words = append([]WordTimestamp(nil), s.Words...)
	out.Tags = append([]string(nil), s.Tags...)
	if s.Metadata != nil {
		out.Metadata = make(map[string]any, len(s.Metadata))
		for k, v := range s.Metadata {
			out.Metadata[k] = v
		}
	}

	return &out
}

// Validate checks the invariants the rest of the system relies on.
func (s *Segment) Validate() error {
	if s.ID == "" {
		return fmt.Errorf("subtitle: segment has no id")
	}
	if s.End <= s.Start {
		return fmt.Errorf("subtitle: segment %s has a non-positive duration [%v, %v]",
			s.ID, s.Start, s.End)
	}
	if s.Start < 0 {
		return fmt.Errorf("subtitle: segment %s starts before zero", s.ID)
	}
	if strings.TrimSpace(s.SourceText) == "" && s.TranslatedText == nil {
		return fmt.Errorf("subtitle: segment %s is empty", s.ID)
	}

	previousEnd := s.Start
	for i, word := range s.Words {
		if word.End < word.Start {
			return fmt.Errorf("subtitle: segment %s word %d has an inverted duration", s.ID, i)
		}
		// Word timings must nest inside their line. A word outside it becomes
		// an impossible subtitle boundary later, where the cause is invisible.
		if word.Start < s.Start-0.05 || word.End > s.End+0.05 {
			return fmt.Errorf("subtitle: segment %s word %d [%v, %v] falls outside [%v, %v]",
				s.ID, i, word.Start, word.End, s.Start, s.End)
		}
		if word.Start < previousEnd-0.05 {
			return fmt.Errorf("subtitle: segment %s word %d overlaps its predecessor", s.ID, i)
		}
		previousEnd = word.End
	}
	return nil
}

// ValidateSet checks a whole segment list, including the ordering rules that
// only make sense across lines.
func ValidateSet(segments []*Segment) error {
	previousEnd := 0.0
	for i, segment := range segments {
		if err := segment.Validate(); err != nil {
			return err
		}
		if segment.Start < previousEnd-0.001 {
			return fmt.Errorf(
				"subtitle: segment %d (%s) starts at %v, before the previous line ends at %v",
				i, segment.ID, segment.Start, previousEnd)
		}
		previousEnd = segment.End
	}
	return nil
}

// TextOf joins a word list back into a string.
//
// Used by segmentation to rebuild a line's text from its words, which keeps the
// text and the timings from drifting apart — a line whose words say one thing
// and whose text says another is a bug that is very hard to see.
func TextOf(words []WordTimestamp) string {
	var b strings.Builder
	for i, word := range words {
		// A space between Latin words, none between Japanese ones. Getting this
		// wrong produces either "hello world" split into "helloworld" or a
		// Japanese line full of spurious spaces.
		if i > 0 && needsSpace(words[i-1].Text, word.Text) {
			b.WriteString(" ")
		}
		b.WriteString(word.Text)
	}
	return strings.TrimSpace(b.String())
}

func needsSpace(previous, current string) bool {
	last := lastRuneOf(previous)
	first := firstRuneOf(current)
	if last == 0 || first == 0 {
		return false
	}
	// A space goes between two words only when both are written in a script
	// that uses them. Japanese and Chinese are not, which is why joining
	// Whisper's words for those languages must not insert anything.
	return UsesSpaces(last) && UsesSpaces(first)
}

// UsesSpaces reports whether a script separates words with spaces.
//
// Exported because joining two subtitle lines needs the same answer as joining
// two words: a space between two Latin texts, nothing between two CJK ones.
func UsesSpaces(r rune) bool {
	switch {
	case unicode.Is(unicode.Han, r),
		unicode.Is(unicode.Hiragana, r),
		unicode.Is(unicode.Katakana, r),
		unicode.Is(unicode.Hangul, r):
		return false
	}
	// Punctuation and symbols are space-neutral: the spacing decision belongs
	// to the words on either side of them.
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func lastRuneOf(s string) rune {
	runes := []rune(s)
	if len(runes) == 0 {
		return 0
	}
	return runes[len(runes)-1]
}

func firstRuneOf(s string) rune {
	runes := []rune(s)
	if len(runes) == 0 {
		return 0
	}
	return runes[0]
}

// RuneCount counts the characters a viewer reads.
func RuneCount(text string) int {
	count := 0
	for _, r := range text {
		if !unicode.IsSpace(r) {
			count++
		}
	}
	return count
}

// Seconds formats a duration for logs and diagnostics.
func Seconds(v float64) string { return time.Duration(v * float64(time.Second)).String() }
