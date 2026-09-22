// Package qc finds the mistakes a machine can find.
//
// It exists because the failures that make a fansub look broken are almost all
// mechanical: a line that flashes past too fast to read, a Whisper loop that
// produced the same sentence eleven times, a character whose name is spelled
// three ways in one file. A human reviewer catches those eventually; catching
// them before the render is the difference between a review pass that takes ten
// minutes and one that takes an evening.
//
// What it deliberately does not do is judge the translation. Whether a line
// reads well is not a question a rule can answer, and a checker that guesses at
// it produces noise that trains the reviewer to ignore findings — which costs
// more than the rule was worth. The LLM pass that would attempt it is off by
// default and not implemented (docs/risks.md §1.3).
package qc

import (
	"fmt"
	"sort"
	"strings"

	"github.com/AhsokaTano26/NikuCooker/internal/glossary"
	"github.com/AhsokaTano26/NikuCooker/internal/subtitle"
)

// Severity ranks a finding.
type Severity string

const (
	// SeverityInfo is worth knowing and never blocks.
	SeverityInfo Severity = "info"

	// SeverityWarning is what a reviewer should look at.
	SeverityWarning Severity = "warning"

	// SeverityError means the output is broken: a missing line, an impossible
	// timing, a file that cannot be rendered.
	SeverityError Severity = "error"
)

// Finding is one problem with one line, or with the project.
type Finding struct {
	// SegmentID is empty for a project-level finding.
	SegmentID string `json:"segment_id,omitempty"`

	// Stage names what produced the finding — "qc" for these rules, and
	// "translation" or "asr" for findings another stage reports through the
	// same structure.
	Stage string `json:"stage"`

	Severity Severity `json:"severity"`
	Code     string   `json:"code"`
	Message  string   `json:"message"`

	// Suggestion is what to do about it, when there is a specific answer. It is
	// empty far more often than not, and filling it with advice the reviewer
	// already knows is how a UI becomes noise.
	Suggestion string `json:"suggestion,omitempty"`
}

// Report is the result of a check.
type Report struct {
	SourceLanguage string `json:"source_language"`
	TargetLanguage string `json:"target_language"`

	Findings []Finding `json:"findings"`

	CheckedSegments int `json:"checked_segments"`

	// Counts is by severity, for a summary line without walking the findings.
	Counts map[Severity]int `json:"counts"`
}

// HasErrors reports whether anything blocks a render.
func (r *Report) HasErrors() bool { return r.Counts[SeverityError] > 0 }

// BySeverity returns the findings at or above a severity, in segment order.
func (r *Report) BySeverity(min Severity) []Finding {
	rank := map[Severity]int{SeverityInfo: 0, SeverityWarning: 1, SeverityError: 2}
	want := rank[min]

	out := make([]Finding, 0, len(r.Findings))
	for _, finding := range r.Findings {
		if rank[finding.Severity] >= want {
			out = append(out, finding)
		}
	}
	return out
}

// Summary renders a one-line description, for a log line or a CLI footer.
func (r *Report) Summary() string {
	if len(r.Findings) == 0 {
		return fmt.Sprintf("no problems in %d lines", r.CheckedSegments)
	}
	return fmt.Sprintf("%d error(s), %d warning(s), %d note(s) across %d lines",
		r.Counts[SeverityError], r.Counts[SeverityWarning],
		r.Counts[SeverityInfo], r.CheckedSegments)
}

// Options are the limits the rules check against.
type Options struct {
	MaxCPS      float64
	MaxDuration float64
	MinDuration float64

	// MaxChars bounds a translated line's length. Zero uses a language default.
	MaxChars int

	// DuplicateWindow is how many preceding lines to compare against when
	// looking for a repeated line. Zero uses a default. A window rather than
	// the whole file, because a phrase legitimately repeated an hour later is
	// not the failure this catches.
	DuplicateWindow int

	// Glossary is the terminology in scope. Only entries marked enabled are
	// checked, and only those whose source occurs in the line.
	Glossary []glossary.Entry
}

func (o Options) withDefaults() Options {
	if o.MaxCPS <= 0 {
		o.MaxCPS = 18
	}
	if o.MaxDuration <= 0 {
		o.MaxDuration = 7
	}
	if o.MinDuration <= 0 {
		o.MinDuration = 0.8
	}
	if o.MaxChars <= 0 {
		o.MaxChars = 42
	}
	if o.DuplicateWindow <= 0 {
		o.DuplicateWindow = 20
	}
	return o
}

// ---------------------------------------------------------------------------
// Rules
// ---------------------------------------------------------------------------

// rule is one check.
//
// A table rather than a chain of ifs so that the rules read as a list — which is
// what makes it obvious that a category of problem is missing, rather than
// requiring the reader to hold the whole function in their head.
type rule struct {
	code     string
	severity Severity
	check    func(*subtitle.Set, Options, *collector)
}

var rules = []rule{
	{code: "NO_SEGMENTS", severity: SeverityError, check: checkNoSegments},
	{code: "UNTRANSLATED", severity: SeverityWarning, check: checkUntranslated},
	{code: "OVERLAP", severity: SeverityError, check: checkOverlap},
	{code: "CPS_EXCEEDED", severity: SeverityWarning, check: checkCPS},
	{code: "TOO_SHORT", severity: SeverityWarning, check: checkDuration},
	{code: "TOO_LONG", severity: SeverityWarning, check: checkDuration},
	{code: "TOO_MANY_CHARS", severity: SeverityWarning, check: checkLength},
	{code: "DUPLICATE", severity: SeverityWarning, check: checkDuplicates},
	{code: "GLOSSARY_VIOLATION", severity: SeverityWarning, check: checkGlossary},
	{code: "UNBALANCED_BRACKET", severity: SeverityInfo, check: checkBrackets},
	{code: "SOURCE_UNCHANGED", severity: SeverityInfo, check: checkUntouched},
}

// Check runs every rule over a subtitle set.
func Check(set *subtitle.Set, opts Options) *Report {
	report := &Report{
		Findings: []Finding{},
		Counts:   map[Severity]int{},
	}
	if set == nil {
		report.Findings = append(report.Findings, Finding{
			Stage:    "qc",
			Severity: SeverityError,
			Code:     "NO_SEGMENTS",
			Message:  "there are no subtitle lines to check",
		})
		report.Counts[SeverityError] = 1
		return report
	}

	opts = opts.withDefaults()
	report.SourceLanguage = set.SourceLanguage
	report.TargetLanguage = set.TargetLanguage
	report.CheckedSegments = len(set.Segments)

	collector := &collector{report: report}
	for _, r := range rules {
		collector.code = r.code
		collector.severity = r.severity
		r.check(set, opts, collector)
	}

	// Segment order, then severity. A reviewer works through a file front to
	// back; triaging by severity first would send them up and down it.
	sort.SliceStable(report.Findings, func(i, j int) bool {
		if report.Findings[i].SegmentID != report.Findings[j].SegmentID {
			return report.Findings[i].SegmentID < report.Findings[j].SegmentID
		}
		return rankOf(report.Findings[i].Severity) > rankOf(report.Findings[j].Severity)
	})

	for _, finding := range report.Findings {
		report.Counts[finding.Severity]++
	}

	return report
}

func rankOf(severity Severity) int {
	switch severity {
	case SeverityError:
		return 2
	case SeverityWarning:
		return 1
	default:
		return 0
	}
}

// collector accumulates findings for the rule currently running.
type collector struct {
	report   *Report
	code     string
	severity Severity
}

func (c *collector) add(segment *subtitle.Segment, message string, suggestion string) {
	finding := Finding{
		Stage:      "qc",
		Severity:   c.severity,
		Code:       c.code,
		Message:    message,
		Suggestion: suggestion,
	}
	if segment != nil {
		finding.SegmentID = segment.ID
	}
	c.report.Findings = append(c.report.Findings, finding)
}

// ---------------------------------------------------------------------------
// Rule implementations
// ---------------------------------------------------------------------------

func checkNoSegments(set *subtitle.Set, _ Options, c *collector) {
	if len(set.Segments) == 0 {
		c.add(nil, "the subtitle set contains no lines",
			"check that the source media contains speech, and that the transcript is not empty")
	}
}

func checkUntranslated(set *subtitle.Set, _ Options, c *collector) {
	for _, segment := range set.Segments {
		if segment.TranslatedText == nil {
			c.add(segment, "the line has no translation",
				"re-run translation; the line will be shown in its original language")
			continue
		}
		if strings.TrimSpace(*segment.TranslatedText) == "" {
			c.add(segment, "the translation is empty",
				"re-run translation for this line")
		}
	}
}

func checkOverlap(set *subtitle.Set, _ Options, c *collector) {
	// Compared against the previous line's end rather than against the
	// validator, because this rule runs on data that may have come from the
	// database or from a user's edit and may never have been validated.
	previousEnd := 0.0
	var previous *subtitle.Segment

	for _, segment := range set.Segments {
		if segment.Start < previousEnd-0.001 {
			message := fmt.Sprintf("the line starts at %.3fs, before the previous line ends at %.3fs",
				segment.Start, previousEnd)
			if previous != nil {
				message = fmt.Sprintf("%s starts at %.3fs, before %s ends at %.3fs",
					segment.ID, segment.Start, previous.ID, previousEnd)
			}
			c.add(segment, message,
				"the overlap is a segmentation or an editing artefact; one of the two lines must be retimed")
		}
		if segment.End > previousEnd {
			previousEnd = segment.End
			previous = segment
		}
	}
}

func checkCPS(set *subtitle.Set, opts Options, c *collector) {
	for _, segment := range set.Segments {
		// Recomputed rather than read from the stored value: a line may have
		// been edited since, and the stored figure would then describe text
		// that no longer exists.
		chars := subtitle.RuneCount(segment.EffectiveText())
		duration := segment.Duration()
		if duration <= 0 || chars == 0 {
			continue
		}

		cps := float64(chars) / duration
		if cps > opts.MaxCPS {
			c.add(segment, fmt.Sprintf(
				"%.1f characters per second over a %.2fs window, above the %.1f limit",
				cps, duration, opts.MaxCPS),
				"shorten the line, or give it more time if the surrounding silence allows")
		}
	}
}

func checkDuration(set *subtitle.Set, opts Options, c *collector) {
	for _, segment := range set.Segments {
		duration := segment.Duration()
		switch {
		case duration < opts.MinDuration:
			c.add(segment, fmt.Sprintf("the line is on screen for only %.2fs", duration),
				"a line that flashes past is worse than a slightly long one; merge it with its neighbour")
		case duration > opts.MaxDuration:
			c.add(segment, fmt.Sprintf("the line stays on screen for %.2fs", duration),
				"split the line, or tighten its end to the next line's start")
		}
	}
}

func checkLength(set *subtitle.Set, opts Options, c *collector) {
	for _, segment := range set.Segments {
		text := segment.EffectiveText()
		// Counted without spaces: a Latin line's spaces are not read, and
		// counting them would flag a long English line that fits.
		if count := subtitle.RuneCount(text); count > opts.MaxChars {
			c.add(segment, fmt.Sprintf(
				"the line is %d characters, over the %d limit", count, opts.MaxChars),
				"a subtitle line that needs more than two rows is hard to read; split it")
		}
	}
}

func checkDuplicates(set *subtitle.Set, opts Options, c *collector) {
	window := opts.DuplicateWindow

	for i, segment := range set.Segments {
		text := normaliseForComparison(segment.SourceText)
		if text == "" {
			continue
		}

		low := i - window
		if low < 0 {
			low = 0
		}
		for j := i - 1; j >= low; j-- {
			if normaliseForComparison(set.Segments[j].SourceText) != text {
				continue
			}
			// Reported on the later line, which is the one a reviewer would
			// delete. Reporting both would double the finding count for a
			// single mistake.
			c.add(segment, fmt.Sprintf(
				"the line repeats %s, %d lines earlier", set.Segments[j].ID, i-j),
				"repeated lines between speech are usually a recogniser loop; check the audio before deleting")
			break
		}
	}
}

func checkGlossary(set *subtitle.Set, opts Options, c *collector) {
	if len(opts.Glossary) == 0 {
		return
	}

	for _, segment := range set.Segments {
		if segment.TranslatedText == nil {
			continue
		}
		translated := *segment.TranslatedText
		if strings.TrimSpace(translated) == "" {
			continue
		}

		for _, entry := range opts.Glossary {
			if !entry.Enabled {
				continue
			}
			// Only terms the source actually contains. A term that is not in
			// the line cannot have been mistranslated in it.
			if !glossary.Occurs(entry.Source, segment.SourceText) {
				continue
			}
			if strings.Contains(translated, entry.Target) {
				continue
			}
			c.add(segment, fmt.Sprintf(
				"the glossary entry %q → %q does not appear in the translation",
				entry.Source, entry.Target),
				"the model may have inflected or rephrased the term; accept it if the meaning is right")
		}
	}
}

// bracketPairs are the bracket pairs whose imbalance is worth reporting.
//
// Only CJK and full-width forms: an unmatched ） in Japanese dialogue is a real
// error, whereas an unmatched Latin bracket is common in ordinary prose and
// flagging it would be noise.
var bracketPairs = [][2]rune{
	{'「', '」'},
	{'『', '』'},
	{'（', '）'},
	{'【', '】'},
	{'《', '》'},
	{'〈', '〉'},
}

func checkBrackets(set *subtitle.Set, _ Options, c *collector) {
	for _, segment := range set.Segments {
		text := segment.EffectiveText()
		for _, pair := range bracketPairs {
			open := strings.Count(text, string(pair[0]))
			close := strings.Count(text, string(pair[1]))
			if open == close {
				continue
			}
			c.add(segment, fmt.Sprintf(
				"%d %c but %d %c in the text", open, pair[0], close, pair[1]),
				"an unbalanced bracket is usually a line that was split mid-quote")
		}
	}
}

func checkUntouched(set *subtitle.Set, opts Options, c *collector) {
	for _, segment := range set.Segments {
		if segment.TranslatedText == nil {
			continue
		}
		translated := strings.TrimSpace(*segment.TranslatedText)
		source := strings.TrimSpace(segment.SourceText)
		if translated == "" || translated != source {
			continue
		}
		// Identical text is correct when the line is already in the target
		// language — a title card, a song lyric, a phrase the show leaves
		// untranslated. It is only worth a note, never a warning.
		c.add(segment, "the translation is identical to the source",
			"correct for text that is already in the target language; otherwise the model may have echoed its input")
	}
}

// normaliseForComparison reduces a line to what makes two lines "the same".
//
// Punctuation and whitespace are dropped because a recogniser loop rarely
// reproduces them identically, and a duplicate rule that misses the duplicate
// because of a comma is not doing its job.
func normaliseForComparison(text string) string {
	var b strings.Builder
	for _, r := range text {
		switch r {
		case ' ', '\t', '\n', '\r',
			'。', '、', '．', '.', ',', '，', '！', '!', '？', '?', '…':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
