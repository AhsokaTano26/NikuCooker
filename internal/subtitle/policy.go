package subtitle

import (
	"strings"
	"unicode"

	"golang.org/x/text/language"
)

// Language describes how a language's text is punctuated.
//
// Data rather than code: adding a language means adding a row, and the
// segmentation algorithm never learns a language's name. That is the property
// that keeps Japanese rules from being compromised to serve a generic case.
type Language struct {
	// Code is the BCP-47 tag, lowercased.
	Code string

	// SentenceEnders end a complete thought. A split here is always correct.
	//
	// Closing brackets are included for CJK because they are unambiguous: a
	// word ending 」 or ）has ended its sentence whether or not the terminator
	// survived tokenisation.
	SentenceEnders string

	// ClauseEnders mark a boundary inside a sentence. A split here is not ideal
	// but is far better than splitting between two words that belong together.
	ClauseEnders string

	// Closers end a sentence only when a terminator immediately precedes them,
	// as the ) does in "(...)." — on its own it is far more likely to be an
	// aside, so treating every word ending ) as sentence-final would cut
	// mid-sentence constantly.
	Closers string

	// Particles are sentence-internal markers that must not start a line.
	//
	// A line beginning with は or が reads as a mistake, and a split that
	// leaves one stranded is the most visible failure of naive segmentation.
	Particles string

	// MaxChars is the reading-length ceiling for this language, or 0 to use the
	// configured default. Chinese and Japanese convey more per character than
	// Latin scripts, so the same number means different things.
	MaxChars int
}

// languages is the table of punctuation rules.
var languages = map[string]Language{
	"ja": {
		Code:           "ja",
		SentenceEnders: "。．！？!?…‥」』）】〕》〉",
		ClauseEnders:   "、，,；;：:",
		Closers:        ")\"'”’",
		Particles:      "はがをにでとへもやのねよかわぞぜさ",
		MaxChars:       24,
	},
	"zh": {
		Code:           "zh",
		SentenceEnders: "。！？!?…」』）】》〉",
		ClauseEnders:   "，、；：",
		Closers:        ")\"'”’",
		Particles:      "",
		MaxChars:       24,
	},
	"en": {
		Code:           "en",
		SentenceEnders: ".!?…",
		ClauseEnders:   ",;:—–",
		Closers:        ")]}\"'”’",
		Particles:      "",
		MaxChars:       42,
	},
	"ko": {
		Code:           "ko",
		SentenceEnders: ".!?…",
		ClauseEnders:   ",;:",
		Closers:        ")]}\"'”’",
		Particles:      "",
		MaxChars:       28,
	},
}

// defaultLanguage is used when a tag is unknown.
//
// Latin punctuation, because getting it wrong on an unknown language produces
// slightly odd line breaks, whereas applying Japanese rules would produce
// nonsense.
var defaultLanguage = languages["en"]

// ForLanguage returns the punctuation rules for a BCP-47 tag.
func ForLanguage(tag string) Language {
	if tag == "" {
		return defaultLanguage
	}

	// Regional and script subtags are irrelevant here: "ja-JP" and "ja" are
	// punctuated identically, and requiring an exact match would silently fall
	// back to English for every fully-qualified tag.
	parsed, err := language.Parse(tag)
	if err == nil {
		base, _ := parsed.Base()
		if found, ok := languages[strings.ToLower(base.String())]; ok {
			return found
		}
	}

	if found, ok := languages[strings.ToLower(tag)]; ok {
		return found
	}
	return defaultLanguage
}

// endsSentence reports whether the text ends a sentence.
func (l Language) endsSentence(text string) bool {
	return endsWithAny(text, l.SentenceEnders) || endsWithCloserAfter(text, l.SentenceEnders, l.Closers)
}

// endsClause reports whether the text ends a clause.
func (l Language) endsClause(text string) bool {
	return endsWithAny(text, l.ClauseEnders) || endsWithCloserAfter(text, l.ClauseEnders, l.Closers)
}

// isStrandedParticle reports whether a word is a particle standing alone, and so
// must not begin a line.
//
// The test is the whole word, not its first character. Japanese words routinely
// *start* with a particle character — はい, がっこう, にはん — and matching on the
// first rune alone reattaches those, which silently undoes a boundary the
// splitter chose for a reason.
//
// Trailing punctuation is stripped first, because a word that is は、 or を。 is
// still a lone particle. Longer compounds like には are not covered: they are a
// less jarring break than a bare particle, and widening the rule to catch them
// would start matching real words again.
func (l Language) isStrandedParticle(text string) bool {
	if l.Particles == "" {
		return false
	}

	trimmed := strings.TrimRightFunc(text, func(r rune) bool {
		return strings.ContainsRune(l.ClauseEnders, r) ||
			strings.ContainsRune(l.SentenceEnders, r) ||
			strings.ContainsRune(l.Closers, r) ||
			unicode.IsSpace(r)
	})

	runes := []rune(trimmed)
	if len(runes) != 1 {
		return false
	}
	return strings.ContainsRune(l.Particles, runes[0])
}

func endsWithAny(text, set string) bool {
	if text == "" || set == "" {
		return false
	}
	return strings.ContainsRune(set, lastRuneOf(text))
}

// endsWithCloserAfter handles the 「…。」」 case: the terminator is followed by
// one or more closing marks, which belong with the sentence that just ended.
func endsWithCloserAfter(text, terminators, closers string) bool {
	if text == "" || closers == "" {
		return false
	}

	runes := []rune(text)
	index := len(runes) - 1

	closed := 0
	for index >= 0 && strings.ContainsRune(closers, runes[index]) {
		index--
		closed++
	}
	if closed == 0 || index < 0 {
		return false
	}

	// Latin closers like ')' are also used for asides, so a single one after a
	// terminator is ambiguous. Only treat a run as sentence-ending when the
	// terminator immediately precedes it.
	return strings.ContainsRune(terminators, runes[index])
}
