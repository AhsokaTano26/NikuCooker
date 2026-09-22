package subtitle

import (
	"fmt"
	"io"
	"strings"
)

// WriteSRT writes a SubRip file.
//
// SubRip is the lowest common denominator: every player reads it, it carries no
// styling, and its timestamps are millisecond-precision. It is the format that
// works when the user's goal is "put this in Plex", and the one to fall back to
// when an ASS file renders differently on every player.
//
// bilingual writes the source line above the translation. That order is the
// fansub convention — the viewer who reads Japanese glances up, the one who
// reads Chinese reads the second line — and it is why the source is not simply
// dropped.
func WriteSRT(w io.Writer, set *Set, bilingual bool) error {
	if set == nil {
		return fmt.Errorf("subtitle: no subtitle set to write")
	}

	var b strings.Builder
	for i, segment := range set.Segments {
		// SubRip cues are numbered from one, and the number is positional:
		// gaps or repeats make some players skip or reorder cues.
		fmt.Fprintf(&b, "%d\n", i+1)
		fmt.Fprintf(&b, "%s --> %s\n", srtTimestamp(segment.Start), srtTimestamp(segment.End))
		b.WriteString(srtText(segment, bilingual))
		b.WriteString("\n\n")
	}

	_, err := io.WriteString(w, b.String())
	if err != nil {
		return fmt.Errorf("subtitle: write srt: %w", err)
	}
	return nil
}

// srtText renders one cue's text.
func srtText(segment *Segment, bilingual bool) string {
	translated := ""
	if segment.TranslatedText != nil {
		translated = strings.TrimSpace(*segment.TranslatedText)
	}

	// An untranslated line falls back to the source. Emitting an empty cue
	// would leave a blank frame where dialogue should be, which reads as a
	// broken file rather than as a missing translation — and the missing
	// translation is already recorded as a QC finding.
	if translated == "" {
		return strings.TrimSpace(segment.SourceText) + "\n"
	}
	if !bilingual {
		return translated + "\n"
	}
	return strings.TrimSpace(segment.SourceText) + "\n" + translated + "\n"
}

// srtTimestamp renders a time as SubRip expects it.
func srtTimestamp(seconds float64) string {
	hours, minutes, secs, millis := splitTime(seconds)
	return fmt.Sprintf("%02d:%02d:%02d,%03d", hours, minutes, secs, millis)
}

// splitTime breaks seconds into the components both formats need.
//
// Rounding happens once, on the total, and is carried through the components
// rather than applied to each independently. Rounding the seconds and the
// milliseconds separately produces 00:00:04,1000 for 3.9996 — an invalid
// timestamp that some players reject and others silently clamp.
func splitTime(seconds float64) (hours, minutes, secs, millis int) {
	if seconds < 0 {
		seconds = 0
	}

	total := int64(seconds*1000 + 0.5)

	millis = int(total % 1000)
	total /= 1000
	secs = int(total % 60)
	total /= 60
	minutes = int(total % 60)
	hours = int(total / 60)

	return hours, minutes, secs, millis
}
