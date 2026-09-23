package provision

import (
	"strings"
	"sync"
)

// maxBufferedLines is how much of a command's output is kept.
//
// uv narrates: resolving, downloading, installing, for every one of several
// hundred packages. The whole transcript is tens of thousands of lines, of
// which the only ones ever read are the last few. Bounded so that a long
// install cannot grow this process's memory by however much uv decides to say.
const maxBufferedLines = 40

// maxPartialLine bounds a single line that never ends.
//
// A command writing a progress animation without newlines would otherwise grow
// this buffer without limit — the one way a bounded ring can still be unbounded.
const maxPartialLine = 8 << 10

// lineBuffer collects the tail of a stream as whole lines.
//
// It is a writer rather than a scanner because it is handed to a command, which
// writes in whatever chunk sizes it likes. Both of the command's output streams
// point at the same buffer, so it is locked.
type lineBuffer struct {
	mu      sync.Mutex
	max     int
	lines   []string
	partial strings.Builder
}

func newLineBuffer(max int) *lineBuffer {
	return &lineBuffer{max: max}
}

func (b *lineBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.partial.Write(p)
	if b.partial.Len() > maxPartialLine {
		// Kept, not discarded: an over-long line is usually a message with a
		// very long path in it, and the beginning is the part that says what
		// went wrong.
		text := b.partial.String()[:maxPartialLine]
		b.partial.Reset()
		b.partial.WriteString(text[:len(text)-len("\n")])
	}

	for {
		text := b.partial.String()
		index := strings.IndexByte(text, '\n')
		if index < 0 {
			break
		}

		b.push(strings.TrimRight(text[:index], "\r"))
		b.partial.Reset()
		b.partial.WriteString(text[index+1:])
	}

	return len(p), nil
}

// push appends a line, dropping the oldest when full. Caller holds the lock.
func (b *lineBuffer) push(line string) {
	b.lines = append(b.lines, line)
	if len(b.lines) > b.max {
		b.lines = b.lines[len(b.lines)-b.max:]
	}
}

func (b *lineBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	lines := b.lines
	// A final line with no newline after it is still output.
	if trailing := strings.TrimSpace(b.partial.String()); trailing != "" {
		lines = append(append([]string{}, lines...), trailing)
	}
	return strings.Join(lines, "\n")
}
