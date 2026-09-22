// Package llmjson reads structured data out of a language model's reply.
//
// A model asked to reply with JSON only will usually oblige, and will sometimes
// wrap it in a code fence, preface it with "Here is the result:", or append a
// closing remark. All of those are recoverable, and failing on them would mean
// discarding a paid-for response over punctuation.
//
// It is a package rather than a helper inside one caller because both the
// analysis pass and the translation pass need it, and two copies of a scanner
// this fiddly would drift.
package llmjson

import (
	"errors"
	"fmt"
	"strings"
)

// ErrNoJSON reports a reply with no JSON value in it at all.
var ErrNoJSON = errors.New("no JSON value was found in the model's reply")

// Extract pulls the first complete JSON object or array out of a reply.
//
// The scan is depth-aware and string-aware, so a brace inside a translated line
// does not end the value early. That is not hypothetical: translated dialogue
// contains 「」, quoted speech contains braces, and a naive search for the last
// `}` in the reply truncates the value at the wrong place and reports a parse
// error whose cause is nowhere near its symptom.
func Extract(text string) ([]byte, error) {
	trimmed := StripFence(strings.TrimSpace(text))
	if trimmed == "" {
		return nil, ErrNoJSON
	}

	start := strings.IndexAny(trimmed, "{[")
	if start < 0 {
		return nil, ErrNoJSON
	}

	opener := trimmed[start]
	closer := byte('}')
	if opener == '[' {
		closer = ']'
	}

	depth := 0
	inString := false
	escaped := false

	for i := start; i < len(trimmed); i++ {
		ch := trimmed[i]

		if inString {
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inString = false
			}
			continue
		}

		switch ch {
		case '"':
			inString = true
		case opener:
			depth++
		case closer:
			depth--
			if depth == 0 {
				return []byte(trimmed[start : i+1]), nil
			}
		}
	}

	return nil, fmt.Errorf("the model's reply has an unterminated JSON value: %s",
		Truncate(trimmed, 120))
}

// StripFence removes a markdown code fence wrapping the whole reply.
//
// Only a fence around the entire reply is removed. A fence in the middle is left
// alone, because the JSON scan finds the value inside it anyway, and stripping
// it would risk cutting into the value.
func StripFence(text string) string {
	if !strings.HasPrefix(text, "```") {
		return text
	}

	// Drop the opening fence and any language tag on the same line.
	newline := strings.IndexByte(text, '\n')
	if newline < 0 {
		return text
	}
	text = text[newline+1:]

	if end := strings.LastIndex(text, "```"); end >= 0 {
		text = text[:end]
	}
	return strings.TrimSpace(text)
}

// Truncate shortens text for an error message, on a rune boundary.
func Truncate(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}
