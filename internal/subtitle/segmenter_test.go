package subtitle

import (
	"fmt"
	"strings"
	"testing"

	"github.com/AhsokaTano26/NikuCooker/pkg/protocol"
)

// word is a shorthand for building a timed word in a test.
func word(start, end float64, text string) protocol.WordTimestamp {
	return protocol.WordTimestamp{Start: start, End: end, Text: text}
}

// asr builds one recogniser segment from a word list.
func asr(start, end float64, text string, words ...protocol.WordTimestamp) protocol.ASRSegment {
	segment := protocol.ASRSegment{Start: start, End: end, Text: text, Words: words}
	return segment
}

func buildOne(t *testing.T, segments ...protocol.ASRSegment) []*Segment {
	t.Helper()
	lines, err := Build(segments, "ja", "zh", DefaultOptions())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return lines
}

// A pause with no punctuation is the only boundary signal Whisper reliably
// gives on spontaneous speech, so it has to produce a split on its own.
func TestSplitsOnPause(t *testing.T) {
	lines := buildOne(t, asr(0, 4, "そうですね たしかに",
		word(0.0, 1.0, "そうですね"),
		word(1.0, 2.0, "たしかに"),
		word(3.0, 4.0, "それは"),
		word(4.0, 5.0, "いいですね"),
	))

	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %s", len(lines), describe(lines))
	}
	if lines[0].Metadata["split_reason"] != "pause" && lines[1].Metadata["split_reason"] != "pause" {
		t.Fatalf("expected a pause split, got %v / %v",
			lines[0].Metadata["split_reason"], lines[1].Metadata["split_reason"])
	}
}

// A sentence end should split, but only once the line is long enough to read.
func TestSplitsOnSentenceEnd(t *testing.T) {
	lines := buildOne(t, asr(0, 8, "こんにちは。ありがとう。",
		word(0.0, 1.0, "こんにちは。"),
		word(1.2, 2.2, "ありがとう。"),
	))

	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %s", len(lines), describe(lines))
	}
}

// Text must be rebuilt from the words that ended up in the line, not carried
// over from the recogniser's own segmentation.
func TestTextMatchesWords(t *testing.T) {
	lines := buildOne(t, asr(0, 4, "今日は いい 天気",
		word(0.0, 1.0, "今日は"),
		word(1.0, 2.0, "いい"),
		word(2.0, 3.0, "天気"),
	))

	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d: %s", len(lines), describe(lines))
	}
	// Japanese gets no spaces inserted between words.
	if lines[0].SourceText != "今日はいい天気" {
		t.Fatalf("unexpected text %q", lines[0].SourceText)
	}
	if got := TextOf(lines[0].Words); got != lines[0].SourceText {
		t.Fatalf("text %q does not match words %q", lines[0].SourceText, got)
	}
}

// Latin text keeps its spaces. The two rules live next to each other and one
// must not break the other.
func TestTextKeepsLatinSpacing(t *testing.T) {
	words := []WordTimestamp{word(0, 1, "Hello"), word(1, 2, "world")}
	if got := TextOf(words); got != "Hello world" {
		t.Fatalf("expected %q, got %q", "Hello world", got)
	}
}

// A line must never begin with a stranded particle: it is the most visible
// failure of naive segmentation.
func TestNeverStartsALineWithAParticle(t *testing.T) {
	lines := buildOne(t, asr(0, 6, "私は それが 好きです",
		word(0.0, 1.1, "私は"),
		word(1.1, 2.2, "それが"), // ends the line; "が" in the next word must not start one
		word(3.0, 4.1, "が"),
		word(4.1, 5.2, "好きです"),
	))

	for _, line := range lines {
		if strings.HasPrefix(line.SourceText, "が") {
			t.Fatalf("line %s starts with a stranded particle: %q", line.ID, line.SourceText)
		}
	}
}

// Long continuous speech has to be cut, and cutting at a clause boundary reads
// better than cutting between two words that belong together.
func TestSplitsLongLinesAtClauseBoundaries(t *testing.T) {
	lines := buildOne(t, asr(0, 20, "",
		word(0.0, 2.0, "まずは、"),
		word(2.0, 4.0, "準備を"), word(4.0, 6.0, "して、"),
		word(6.0, 8.0, "それから"), word(8.0, 10.0, "始めます。"),
	))

	if len(lines) < 2 {
		t.Fatalf("expected the long segment to be split, got %d line(s): %s", len(lines), describe(lines))
	}
	for _, line := range lines {
		if line.Duration() > DefaultOptions().MaxDuration+0.001 {
			t.Fatalf("line %s lasts %.2fs, over the %.2fs ceiling",
				line.ID, line.Duration(), DefaultOptions().MaxDuration)
		}
		if startsWithClauseMark(line.SourceText) {
			t.Fatalf("line %s was cut before a clause mark instead of after it: %q",
				line.ID, line.SourceText)
		}
	}
}

// startsWithClauseMark reports a cut made *before* a clause mark, which strands
// the mark at the head of the next line.
func startsWithClauseMark(text string) bool {
	runes := []rune(text)
	return len(runes) > 0 && strings.ContainsRune("、，,；;：:", runes[0])
}

// A single-word fragment at the very end is merged back rather than left to
// flash on screen for a third of a second.
func TestMergesShortTrailingFragment(t *testing.T) {
	lines := buildOne(t, asr(0, 6, "",
		word(0.0, 2.0, "そうですね"),
		word(2.0, 4.0, "わかりました"),
		word(5.0, 5.3, "なるほど"),
	))

	if len(lines) != 1 {
		t.Fatalf("expected the trailing fragment to be merged, got %d lines: %s", len(lines), describe(lines))
	}
	if !strings.HasSuffix(lines[0].SourceText, "なるほど") {
		t.Fatalf("expected the fragment to join the previous line, got %q", lines[0].SourceText)
	}
}

// The particle rule must match a lone particle, not any word that happens to
// begin with a particle character. はい and がっこう begin with は and が and are
// ordinary words; reattaching them silently undoes a deliberate boundary.
func TestParticleRuleDoesNotMatchRealWords(t *testing.T) {
	policy := ForLanguage("ja")

	for _, lone := range []string{"は", "が", "を", "は、", "を。"} {
		if !policy.isStrandedParticle(lone) {
			t.Fatalf("%q is a lone particle and must not start a line", lone)
		}
	}
	for _, real := range []string{"はい", "がっこう", "にはん", "だけど", "それで"} {
		if policy.isStrandedParticle(real) {
			t.Fatalf("%q is a word, not a stranded particle", real)
		}
	}
}

// Segments must come out ordered and non-overlapping: every later stage assumes
// it, and a violation is invisible until the subtitle file is unusable.
func TestOutputIsOrdered(t *testing.T) {
	lines := buildOne(t, asr(0, 3, "あ", word(0, 1, "あ"), word(1, 2, "い")),
		asr(3, 6, "う", word(3, 4, "う"), word(4, 5, "え")))

	if err := ValidateSet(lines); err != nil {
		t.Fatalf("Build produced an invalid set: %v", err)
	}
	for i, line := range lines {
		want := idFor(i + 1)
		if line.ID != want {
			t.Fatalf("line %d has id %q, want %q", i, line.ID, want)
		}
	}
}

// A recogniser that returns no word timings still has to produce subtitles.
// Coarser lines are much better than none.
func TestFallsBackWhenWordsAreMissing(t *testing.T) {
	lines := buildOne(t, asr(0, 3, "こんにちは"))

	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if lines[0].SourceText != "こんにちは" {
		t.Fatalf("unexpected text %q", lines[0].SourceText)
	}
	if len(lines[0].Words) != 1 {
		t.Fatalf("expected a single spanning word, got %d", len(lines[0].Words))
	}
}

func TestEmptyInputProducesNothing(t *testing.T) {
	lines, err := Build(nil, "ja", "zh", DefaultOptions())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(lines) != 0 {
		t.Fatalf("expected no lines, got %d", len(lines))
	}
}

// Full-width and half-width terminators both end sentences; Whisper emits both
// depending on the language hint.
func TestSentenceEndersAcceptBothWidths(t *testing.T) {
	for _, text := range []string{"です。", "です！", "です?", "です」", "です。」"} {
		if !ForLanguage("ja").endsSentence(text) {
			t.Fatalf("%q should end a sentence", text)
		}
	}
	for _, text := range []string{"です、", "です", "ですね"} {
		if ForLanguage("ja").endsSentence(text) {
			t.Fatalf("%q should not end a sentence", text)
		}
	}
}

// A qualified tag must not silently fall back to English rules.
func TestLanguageLookupIgnoresRegion(t *testing.T) {
	for _, tag := range []string{"ja", "ja-JP", "JA-jp", "ja-Hira"} {
		if got := ForLanguage(tag); got.Code != "ja" {
			t.Fatalf("ForLanguage(%q) gave %q, want ja", tag, got.Code)
		}
	}
	if got := ForLanguage("pt-BR"); got.Code != "en" {
		t.Fatalf("ForLanguage(pt-BR) gave %q, want the en default", got.Code)
	}
}

func idFor(n int) string {
	return fmt.Sprintf("seg_%0*d", idWidth, n)
}

func describe(lines []*Segment) string {
	parts := make([]string, 0, len(lines))
	for _, line := range lines {
		reason, _ := line.Metadata["split_reason"].(string)
		parts = append(parts, "["+reason+"]"+line.SourceText)
	}
	return strings.Join(parts, " | ")
}
