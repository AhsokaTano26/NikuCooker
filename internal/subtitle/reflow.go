package subtitle

import (
	"math"
	"sort"
)

// ReflowOptions are the limits timing correction respects.
type ReflowOptions struct {
	// MaxCPS is the reading speed a corrected line must come down to.
	MaxCPS float64

	// MaxDuration bounds how long a line may stay on screen, which is also what
	// stops a correction from holding one line for the whole silence after it.
	MaxDuration float64

	// MinGap is the blank interval left between two consecutive lines.
	//
	// Zero would read as one continuous line that changed its text; a couple of
	// frames is enough to register as two.
	MinGap float64
}

// DefaultReflowOptions returns the correction limits for a first run.
func DefaultReflowOptions() ReflowOptions {
	return ReflowOptions{
		MaxCPS:      18,
		MaxDuration: 7.0,
		MinGap:      0.083, // two frames at 24fps
	}
}

// ReflowReport describes what a correction pass did.
type ReflowReport struct {
	// Extended is how many lines were given more time.
	Extended int

	// Unresolved lists lines that are still too fast to read after borrowing
	// every available millisecond. These need a human: the fix is to split the
	// line or shorten the translation, and neither is a decision this pass can
	// make.
	Unresolved []string
}

// Reflow slows down lines that are on screen for less time than they take to
// read.
//
// The correction is timing, not text. Giving a line the silence around it is
// both the standard subtitling answer and the only one available here: the
// alternative — re-splitting the line — would invalidate its translation, and
// re-translating costs money for a problem that is usually pure timing. A line
// that is simply too long for any window it could occupy is not silently
// accepted; it is flagged, because quietly shipping it means the viewer gets a
// line they cannot finish.
//
// Segments must be ordered and non-overlapping on entry, which ValidateSet
// enforces upstream.
func Reflow(segments []*Segment, opts ReflowOptions) ReflowReport {
	var report ReflowReport
	if opts.MaxCPS <= 0 || len(segments) == 0 {
		return report
	}
	if opts.MaxDuration <= 0 {
		opts.MaxDuration = DefaultReflowOptions().MaxDuration
	}

	for i, segment := range segments {
		chars := RuneCount(segment.EffectiveText())
		if chars == 0 {
			continue
		}

		// Already readable, or readable within the duration it already has.
		if segment.Duration() >= float64(chars)/opts.MaxCPS {
			continue
		}

		// Unbounded on each side where there is nothing in the way. The
		// duration limit below is what actually bounds them, and using
		// infinity here rather than a large number keeps that single.
		roomAfter := math.Inf(1)
		if i+1 < len(segments) {
			roomAfter = segments[i+1].Start - opts.MinGap - segment.End
		}

		// Behind the first line there is not infinity but the start of the
		// file. Treating it as unbounded lets the line begin at a negative
		// time, which is not a subtitle that can be written.
		roomBefore := segment.Start
		if i > 0 {
			roomBefore = segment.Start - segments[i-1].End - opts.MinGap
		}

		before := segment.End

		// Forward first. A line that stays up longer reads as a line that is
		// still being spoken, whereas one that appears earlier than the speech
		// reads as being out of sync.
		if roomAfter > 0 {
			segment.End = math.Min(segment.End+roomAfter, segment.Start+opts.MaxDuration)
		}

		// Then back, if that was not enough, but never more than the window the
		// line may occupy.
		if segment.Duration() < float64(chars)/opts.MaxCPS && roomBefore > 0 {
			earliest := math.Max(segment.Start-roomBefore, segment.End-opts.MaxDuration)
			segment.Start = earliest
		}

		segment.End = roundMillis(segment.End)
		segment.Start = roundMillis(segment.Start)
		segment.RecomputeCPS()

		if segment.End > before {
			report.Extended++
		}

		if segment.CPS != nil && *segment.CPS > opts.MaxCPS {
			// Left for a human, and marked so. The line is still shown — a line
			// that is too fast is better than a line that is missing — but it
			// carries a flag into the review queue.
			segment.NeedsReview = true
			segment.AddTag(TagCPSExceeded)
			report.Unresolved = append(report.Unresolved, segment.ID)
		}
	}

	sort.Strings(report.Unresolved)
	return report
}

// TagCPSExceeded marks a line that is still too fast to read.
const TagCPSExceeded = "cps_exceeded"

// TagUntranslated marks a line the translator never returned text for.
const TagUntranslated = "untranslated"

// TagTranslationFailed marks a line whose translation was attempted and failed.
const TagTranslationFailed = "translation_failed"

// AddTag records a tag once.
func (s *Segment) AddTag(tag string) {
	for _, existing := range s.Tags {
		if existing == tag {
			return
		}
	}
	s.Tags = append(s.Tags, tag)
}

// RemoveTag clears a tag.
func (s *Segment) RemoveTag(tag string) {
	kept := s.Tags[:0]
	for _, existing := range s.Tags {
		if existing != tag {
			kept = append(kept, existing)
		}
	}
	s.Tags = kept
}

// roundMillis snaps a time to the millisecond.
//
// Subtitle formats store milliseconds, and repeated float arithmetic leaves
// values like 3.0999999999999996 that round-trip inconsistently between formats.
func roundMillis(v float64) float64 {
	return math.Round(v*1000) / 1000
}
