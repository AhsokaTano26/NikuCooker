package subtitle

import (
	"fmt"
	"strings"

	"github.com/AhsokaTano26/NikuCooker/pkg/protocol"
)

// Options are the constraints segmentation enforces.
type Options struct {
	// MinDuration stops a line flashing on screen too briefly to read.
	MinDuration float64

	// MaxDuration bounds how long a line stays up. Settled empirically around
	// seven seconds: beyond that a viewer reads the line twice and starts
	// waiting.
	MaxDuration float64

	// MaxCPS is the reading-speed ceiling, used to decide whether a line is too
	// long *for its duration* rather than in absolute terms.
	MaxCPS float64

	// MaxChars is the absolute line-length ceiling. Zero uses the language's
	// own value.
	MaxChars int

	// PauseMS is the silence that marks a boundary even without punctuation.
	// Whisper's punctuation is unreliable on spontaneous speech, so a long
	// pause is often the only signal that a sentence ended.
	PauseMS int
}

// DefaultOptions returns the settings a first run uses.
func DefaultOptions() Options {
	return Options{
		MinDuration: 1.0,
		MaxDuration: 7.0,
		MaxCPS:      18,
		MaxChars:    0,
		PauseMS:     300,
	}
}

// idWidth is how many digits segment IDs are padded to.
const idWidth = 4

// Build turns recognition output into subtitle lines.
//
// The input is the recogniser's raw segments; the output is lines that can be
// displayed. They are not the same thing: a Whisper segment is several seconds
// of continuous speech, frequently containing more than one sentence. Splitting
// them is what makes the result watchable.
func Build(
	asr []protocol.ASRSegment,
	sourceLanguage, targetLanguage string,
	opts Options,
) ([]*Segment, error) {
	policy := ForLanguage(sourceLanguage)

	maxChars := opts.MaxChars
	if maxChars <= 0 {
		maxChars = policy.MaxChars
	}
	if maxChars <= 0 {
		maxChars = 24
	}

	words := flatten(asr)
	if len(words) == 0 {
		return nil, nil
	}

	lines := split(words, policy, opts, maxChars)

	segments := make([]*Segment, 0, len(lines))
	for i, line := range lines {
		segment := &Segment{
			ID:             fmt.Sprintf("seg_%0*d", idWidth, i+1),
			Start:          line.start,
			End:            line.end,
			SourceLanguage: sourceLanguage,
			TargetLanguage: targetLanguage,
			SourceText:     TextOf(line.words),
			Words:          line.words,
			ASRConfidence:  line.confidence,
			Tags:           []string{},
			Metadata: map[string]any{
				// Diagnostics, not interface: nothing downstream may branch on
				// these, and they are deliberately outside the cache key.
				"source_ordinal": line.sourceOrdinal,
				"split_reason":   line.reason,
				"word_count":     len(line.words),
			},
		}
		segment.RecomputeCPS()

		if err := segment.Validate(); err != nil {
			return nil, err
		}
		segments = append(segments, segment)
	}

	if err := ValidateSet(segments); err != nil {
		return nil, err
	}
	return segments, nil
}

// ---------------------------------------------------------------------------
// Flattening
// ---------------------------------------------------------------------------

// flatWord is one word in a single stream, with the context a split needs.
type flatWord struct {
	protocol.WordTimestamp

	// sourceOrdinal is which ASR segment this word came from, so a line can
	// report its provenance.
	sourceOrdinal int

	// confidence is the parent ASR segment's average log probability. The
	// recogniser only reports it per segment, not per word.
	confidence *float64
}

// flatten turns ASR segments into one word stream.
//
// A segment with no word timings falls back to being one "word" spanning the
// whole thing. That keeps the pipeline working on a model or a language where
// word timings are unavailable, at the cost of coarser lines — which is much
// better than producing nothing.
func flatten(asr []protocol.ASRSegment) []flatWord {
	var words []flatWord

	for i, segment := range asr {
		if len(segment.Words) == 0 {
			text := strings.TrimSpace(segment.Text)
			if text == "" {
				continue
			}
			words = append(words, flatWord{
				WordTimestamp: protocol.WordTimestamp{
					Start: segment.Start,
					End:   segment.End,
					Text:  text,
				},
				sourceOrdinal: i,
				confidence:    segment.AvgLogprob,
			})
			continue
		}

		for _, word := range segment.Words {
			if strings.TrimSpace(word.Text) == "" {
				// Whisper emits empty words around some boundaries; they carry
				// a timing but no text, and keeping them produces a line whose
				// words say nothing.
				continue
			}
			words = append(words, flatWord{
				WordTimestamp: word,
				sourceOrdinal: i,
				confidence:    segment.AvgLogprob,
			})
		}
	}

	return words
}

// ---------------------------------------------------------------------------
// Splitting
// ---------------------------------------------------------------------------

// line is an accumulating subtitle line.
type line struct {
	words         []protocol.WordTimestamp
	start, end    float64
	confidence    *float64
	sourceOrdinal int
	reason        string
}

// split walks the word stream and produces lines.
func split(words []flatWord, policy Language, opts Options, maxChars int) []line {
	var (
		lines   []line
		current []flatWord
	)

	pause := float64(opts.PauseMS) / 1000

	flush := func(reason string) {
		if len(current) == 0 {
			return
		}
		lines = append(lines, buildLine(current, reason))
		current = nil
	}

	for i := range words {
		word := words[i]

		if len(current) > 0 {
			last := current[len(current)-1]
			gap := word.Start - last.End
			lineStart := current[0].Start

			// The line as it would be if this word joined it.
			projected := word.End - lineStart
			chars := runeCount(current) + RuneCount(word.Text)

			switch {
			case gap >= pause:
				// Sentence-final punctuation is dropped from Whisper's words
				// often enough that a long pause is frequently the only real
				// boundary signal in spontaneous speech.
				flush("pause")

			case policy.endsSentence(last.Text) && last.End-lineStart >= opts.MinDuration:
				// A sentence ended and the line is already long enough to stand
				// on its own.

				flush("sentence")

			case projected > opts.MaxDuration:
				splitAtClause(&lines, &current, policy, opts, "max_duration")

			case chars > maxChars:
				splitAtClause(&lines, &current, policy, opts, "max_chars")

			case exceedsCPS(current, word, opts):
				flush("cps")
			}
		}

		// A particle must not begin a line: it reads as a mistake, and it is
		// the single most visible failure of naive segmentation.
		if len(current) == 0 && len(lines) > 0 && policy.isStrandedParticle(word.Text) {
			previous := &lines[len(lines)-1]
			previous.words = append(previous.words, toWord(word))
			previous.end = word.End
			continue
		}

		current = append(current, word)
	}

	flush("end")

	// A fragment shorter than the minimum is absorbed by its neighbour: a
	// 0.4-second flash of one word is worse than a slightly long line.
	return mergeShort(lines, opts)
}

// splitAtClause ends the current line at a clause boundary when one is usable,
// and at the last word otherwise.
//
// Cutting at a boundary is worth the extra search: a line that ends on 、 reads
// as a deliberate break, while one that ends mid-phrase reads as a mistake,
// even though both are on screen for the same time.
func splitAtClause(lines *[]line, current *[]flatWord, policy Language, opts Options, reason string) {
	words := *current
	if at := lastClauseBoundary(words, policy, opts); at > 0 {
		*lines = append(*lines, buildLine(words[:at], "clause"))
		*current = append([]flatWord(nil), words[at:]...)
		return
	}
	*lines = append(*lines, buildLine(words, reason))
	*current = nil
}

// buildLine converts accumulated words into a line.
func buildLine(words []flatWord, reason string) line {
	out := line{
		reason:        reason,
		start:         words[0].Start,
		end:           words[len(words)-1].End,
		sourceOrdinal: words[0].sourceOrdinal,
		confidence:    words[0].confidence,
	}
	out.words = make([]protocol.WordTimestamp, 0, len(words))
	for _, word := range words {
		out.words = append(out.words, toWord(word))
	}
	return out
}

func toWord(word flatWord) protocol.WordTimestamp { return word.WordTimestamp }

func runeCount(words []flatWord) int {
	total := 0
	for _, word := range words {
		total += RuneCount(word.Text)
	}
	return total
}

// exceedsCPS reports whether adding a word would push the line past the reading
// speed ceiling.
//
// This is a *source-text* estimate. The real check is against the translation,
// which is not known yet — Chinese is usually shorter than Japanese, but not
// always — so this is deliberately conservative: it splits a little too eagerly
// rather than producing a line nobody can read.
func exceedsCPS(current []flatWord, next flatWord, opts Options) bool {
	if opts.MaxCPS <= 0 || len(current) == 0 {
		return false
	}
	duration := next.End - current[0].Start
	if duration <= 0 {
		return false
	}
	chars := runeCount(current) + RuneCount(next.Text)
	return float64(chars)/duration > opts.MaxCPS
}

// lastClauseBoundary finds where to cut when a line must be split.
//
// It returns the index *after* which to cut, or 0 when no clause boundary is
// usable. A boundary is usable when the part before it is long enough to stand
// on its own and the part after it is not left trivially short.
func lastClauseBoundary(current []flatWord, policy Language, opts Options) int {
	if len(current) < 2 {
		return 0
	}

	for i := len(current) - 1; i > 0; i-- {
		if !policy.endsClause(current[i-1].Text) {
			continue
		}

		head := current[i-1].End - current[0].Start
		tail := current[len(current)-1].End - current[i].Start

		// Both halves have to be worth showing. Cutting at a clause boundary
		// that leaves one word on one side is worse than cutting in the middle.
		if head < opts.MinDuration || tail < opts.MinDuration {
			continue
		}
		return i
	}
	return 0
}

// mergeShort absorbs lines too brief to read into a neighbour.
//
// Only the two ends are considered. A short line in the middle is usually a real
// short utterance, and absorbing it would undo a boundary the splitter chose
// deliberately; but a short *opening* or *closing* fragment is nearly always an
// artefact of where the speech started or stopped.
func mergeShort(lines []line, opts Options) []line {
	if len(lines) < 2 || opts.MinDuration <= 0 {
		return lines
	}

	out := make([]line, 0, len(lines))

	// A short head has nothing behind it to merge into, so it waits here and is
	// prepended to the next line instead.
	var head line
	hasHead := false

	for _, current := range lines {
		if hasHead {
			current.words = append(append([]protocol.WordTimestamp(nil), head.words...), current.words...)
			current.start = head.start
			current.reason = "merged_short"
			hasHead = false
		}

		if current.end-current.start < opts.MinDuration {
			if len(out) == 0 {
				// The first line, and there is a later line to carry it.
				head, hasHead = current, true
				continue
			}
			previous := &out[len(out)-1]
			previous.words = append(previous.words, current.words...)
			previous.end = current.end
			previous.reason = "merged_short"
			continue
		}

		out = append(out, current)
	}

	// hasHead cannot still be set: it is only set on the first line, and the
	// guard above means a second line always exists to clear it.
	return out
}
