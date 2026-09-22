package main

import (
	"io"
	"strings"
)

// table renders aligned columns.
//
// Written rather than taken from text/tabwriter, which counts runes. Every name
// this program prints is likely to be Japanese or Chinese, where a character
// occupies two terminal columns, so a rune count misaligns every column after a
// CJK name — which is to say, all of them.
type table struct {
	headers []string
	rows    [][]string
}

func newTable(headers ...string) *table {
	return &table{headers: headers}
}

func (t *table) add(cells ...string) {
	t.rows = append(t.rows, cells)
}

// render writes the table.
func (t *table) render(w io.Writer) error {
	widths := make([]int, len(t.headers))
	for i, header := range t.headers {
		widths[i] = displayWidth(header)
	}
	for _, row := range t.rows {
		for i, cell := range row {
			if i < len(widths) && displayWidth(cell) > widths[i] {
				widths[i] = displayWidth(cell)
			}
		}
	}

	var b strings.Builder
	writeRow := func(cells []string) {
		for i, cell := range cells {
			if i == len(cells)-1 {
				// The last column is not padded: trailing whitespace is
				// invisible on a terminal and shows up in a diff or a paste.
				b.WriteString(cell)
				break
			}
			b.WriteString(cell)
			b.WriteString(strings.Repeat(" ", widths[i]-displayWidth(cell)+2))
		}
		b.WriteString("\n")
	}

	writeRow(t.headers)
	for _, row := range t.rows {
		writeRow(row)
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// displayWidth counts the terminal columns a string occupies.
//
// An approximation of the Unicode East Asian Width property, covering the ranges
// that appear in this project's output. A full implementation needs the Unicode
// data tables; what is here is correct for CJK text and for ASCII, and a
// character it gets wrong produces a misaligned column rather than a wrong
// answer.
func displayWidth(text string) int {
	width := 0
	for _, r := range text {
		switch {
		case r == 0:
			// A combining mark or a zero-width joiner takes no column. Only the
			// zero rune is treated this way, because the control characters are
			// not something this program prints.
			continue
		case isWide(r):
			width += 2
		default:
			width++
		}
	}
	return width
}

// isWide reports whether a rune occupies two terminal columns.
func isWide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F, // Hangul Jamo
		r >= 0x2E80 && r <= 0x303E,   // CJK radicals, Kangxi
		r >= 0x3041 && r <= 0x33FF,   // Hiragana, Katakana, Bopomofo, CJK compat
		r >= 0x3400 && r <= 0x4DBF,   // CJK unified ideographs extension A
		r >= 0x4E00 && r <= 0x9FFF,   // CJK unified ideographs
		r >= 0xA000 && r <= 0xA4CF,   // Yi
		r >= 0xAC00 && r <= 0xD7A3,   // Hangul syllables
		r >= 0xF900 && r <= 0xFAFF,   // CJK compatibility ideographs
		r >= 0xFE30 && r <= 0xFE6F,   // CJK compatibility forms
		r >= 0xFF00 && r <= 0xFF60,   // Fullwidth forms
		r >= 0xFFE0 && r <= 0xFFE6,   // Fullwidth signs
		r >= 0x1F300 && r <= 0x1F64F, // emoji
		r >= 0x1F900 && r <= 0x1F9FF,
		r >= 0x20000 && r <= 0x3FFFD: // CJK extensions B onward
		return true
	}
	return false
}

// pad pads a string to a display width.
func pad(text string, width int) string {
	gap := width - displayWidth(text)
	if gap <= 0 {
		return text
	}
	return text + strings.Repeat(" ", gap)
}
