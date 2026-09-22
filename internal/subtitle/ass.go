package subtitle

import (
	"fmt"
	"io"
	"strings"
)

// ASSHeader is the part of an ASS file that precedes the dialogue.
//
// It is exposed so a caller can inspect what a render will look like without
// writing a file, which is what the preview in the editor needs.
func ASSHeader(preset Preset, title string) (string, error) {
	if preset.PlayResX <= 0 || preset.PlayResY <= 0 {
		return "", fmt.Errorf("subtitle: preset %q has no canvas size", preset.Name)
	}

	styleName := preset.Name
	if styleName == "" {
		styleName = "Default"
	}

	primary, err := normaliseColour(preset.PrimaryColour)
	if err != nil {
		return "", err
	}
	outline, err := normaliseColour(preset.OutlineColour)
	if err != nil {
		return "", err
	}
	back, err := normaliseColour(preset.BackColour)
	if err != nil {
		return "", err
	}

	bold := 0
	if preset.Bold {
		bold = -1 // ASS uses -1 for true, because it predates agreeing on 1.
	}
	italic := 0
	if preset.Italic {
		italic = -1
	}

	borderStyle := preset.BorderStyle
	if borderStyle == 0 {
		borderStyle = 1
	}
	alignment := preset.Alignment
	if alignment == 0 {
		alignment = 2
	}

	var b strings.Builder
	b.WriteString("[Script Info]\n")
	if title != "" {
		fmt.Fprintf(&b, "Title: %s\n", sanitiseASSHeaderValue(title))
	}
	b.WriteString("; Written by NikuCooker\n")
	// v4.00+ rather than v4.00: the plus form is what every modern renderer
	// expects, and it is what allows the style fields below to be understood.
	b.WriteString("ScriptType: v4.00+\n")
	fmt.Fprintf(&b, "PlayResX: %d\n", preset.PlayResX)
	fmt.Fprintf(&b, "PlayResY: %d\n", preset.PlayResY)
	// 0 lets the renderer wrap to the frame width. The alternative values wrap
	// at a fixed character count, which is wrong the moment the video is not the
	// canvas size it was authored against.
	b.WriteString("WrapStyle: 0\n")
	b.WriteString("ScaledBorderAndShadow: yes\n")
	// Without this, players guess at the colour matrix and white text picks up a
	// colour cast on some sources.
	b.WriteString("YCbCr Matrix: TV.709\n")

	b.WriteString("\n[V4+ Styles]\n")
	b.WriteString("Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, " +
		"OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, " +
		"Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, " +
		"MarginV, Encoding\n")
	fmt.Fprintf(&b, "Style: %s,%s,%d,%s,%s,%s,%s,%d,%d,0,0,100,100,%s,0,%d,%s,%s,%d,%d,%d,%d,1\n",
		styleName,
		sanitiseASSHeaderValue(preset.FontName),
		preset.FontSize,
		primary,
		primary, // secondary, used for karaoke; the same colour is correct here
		outline,
		back,
		bold, italic,
		formatDecimal(preset.Spacing),
		borderStyle,
		formatDecimal(preset.Outline),
		formatDecimal(preset.Shadow),
		alignment,
		preset.MarginL, preset.MarginR, preset.MarginV,
	)

	b.WriteString("\n[Events]\n")
	b.WriteString("Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n")

	return b.String(), nil
}

// WriteASS writes an Advanced SubStation Alpha file.
//
// ASS rather than SubRip whenever the user has a choice: it carries the styling,
// it positions text reliably, and it is what a fansub is expected to ship. The
// cost is that it is only correct if the style block is, which is why the header
// is built from a validated preset rather than assembled at the call site.
func WriteASS(w io.Writer, set *Set, preset Preset, bilingual bool, title string) error {
	if set == nil {
		return fmt.Errorf("subtitle: no subtitle set to write")
	}

	header, err := ASSHeader(preset, title)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, header); err != nil {
		return fmt.Errorf("subtitle: write ass: %w", err)
	}

	styleName := preset.Name
	if styleName == "" {
		styleName = "Default"
	}

	var b strings.Builder
	for _, segment := range set.Segments {
		text := assText(segment, bilingual)
		if text == "" {
			// A cue with no text is a blank line. Some renderers still flash the
			// outline for it, so it is skipped rather than emitted empty.
			continue
		}

		// Layer 0 is the base layer. Dialogue rather than Comment: this is
		// speech.
		fmt.Fprintf(&b, "Dialogue: 0,%s,%s,%s,,0,0,0,,%s\n",
			assTimestamp(segment.Start),
			assTimestamp(segment.End),
			styleName,
			text,
		)
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("subtitle: write ass: %w", err)
	}
	return nil
}

// assText renders one cue's text with ASS line breaks.
func assText(segment *Segment, bilingual bool) string {
	source := escapeASSText(strings.TrimSpace(segment.SourceText))

	translated := ""
	if segment.TranslatedText != nil {
		translated = escapeASSText(strings.TrimSpace(*segment.TranslatedText))
	}
	if translated == "" {
		return source
	}
	if !bilingual {
		return translated
	}
	// \N is a hard line break. \n would be a soft one, which the renderer is
	// free to re-wrap — and it would then re-wrap the two languages into each
	// other.
	return source + `\N` + translated
}

// assTimestamp renders a time in ASS's centisecond format.
//
// Note the fractional part is centiseconds, not milliseconds: ASS is a format
// from 1999 and stopped at two digits. Writing three produces a file that
// parses as a different time entirely, because the renderer reads the extra
// digit as part of the seconds field.
func assTimestamp(seconds float64) string {
	hours, minutes, secs, millis := splitTime(seconds)
	return fmt.Sprintf("%d:%02d:%02d.%02d", hours, minutes, secs, millis/10)
}

// escapeASSText makes arbitrary text safe to place in an ASS event.
//
// Three sequences are structural, and ASS has no escape for any of them:
//
//   - `{` opens an override block. Everything to the next `}` is
//     interpreted as tags, so a stray brace silently deletes the text between
//     them and can change the styling of everything after it.
//   - `\` followed by N, n or h is a line break or a non-breaking space;
//     a backslash before anything else renders literally.
//   - A raw newline ends the event, corrupting every line that follows.
//
// There is no way to write a literal `{` in ASS dialogue text — the usual
// advice to write `\{` is not an escape the format defines, it merely happens
// to work in some renderers and not others. So the brace is *substituted*
// rather than escaped. The fullwidth forms are the right substitution for this
// project's languages: a Japanese or Chinese script uses ｛｝ for braces in
// running text anyway, so the substitute is what the source would have
// contained, and the reader sees no difference.
//
// A backslash is left alone unless it introduces one of the three escapes,
// where it is likewise replaced by its fullwidth form. Substituting only the
// ambiguous case keeps every legitimate backslash — a URL, a Windows path —
// byte-identical.
func escapeASSText(text string) string {
	if text == "" {
		return ""
	}

	var b strings.Builder
	b.Grow(len(text))

	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		switch runes[i] {
		case '{':
			b.WriteRune('｛')
		case '}':
			b.WriteRune('｝')
		case '\r':
			// Dropped, so that a CRLF pair does not become two breaks.
		case '\n':
			b.WriteString(`\N`)
		case '\\':
			if i+1 < len(runes) && (runes[i+1] == 'N' || runes[i+1] == 'n' || runes[i+1] == 'h') {
				b.WriteRune('＼')
				continue
			}
			b.WriteRune('\\')
		default:
			b.WriteRune(runes[i])
		}
	}

	return b.String()
}

// sanitiseASSHeaderValue strips the characters that would end a header line
// early or turn one field into two.
func sanitiseASSHeaderValue(value string) string {
	replacer := strings.NewReplacer(
		"\n", " ",
		"\r", " ",
		",", " ",
		":", " ",
	)
	return strings.TrimSpace(replacer.Replace(value))
}

// normaliseColour accepts either an ASS literal or a hex colour.
func normaliseColour(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "&H00FFFFFF", nil
	}
	if strings.HasPrefix(strings.ToUpper(trimmed), "&H") {
		return trimmed, nil
	}
	return assColour(trimmed)
}

// formatDecimal renders a number without a trailing ".0" and without exponent
// notation, both of which some ASS parsers mishandle.
func formatDecimal(value float64) string {
	if value == float64(int64(value)) {
		return fmt.Sprintf("%d", int64(value))
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", value), "0"), ".")
}
