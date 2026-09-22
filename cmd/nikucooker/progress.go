package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/AhsokaTano26/NikuCooker/internal/pipeline"
)

// progressObserver prints pipeline progress to stderr.
//
// stderr rather than stdout, so that `nikucooker run --json` can be piped into
// something that parses JSON without the progress lines corrupting it. That is
// the same reason the worker protocol owns stdout in the Python process, and the
// same reasoning applies here.
type progressObserver struct {
	out *cobra.Command

	// interactive is whether the output is a terminal. A progress line that
	// rewrites itself is right on a terminal and garbage in a file: redirected
	// output would contain a carriage return and an erase sequence for every
	// update, which is unreadable and makes the log hard to grep. When it is
	// not a terminal, one line per stage is printed instead.
	interactive bool

	mu         sync.Mutex
	lastUpdate map[string]time.Time
	lastLine   int
}

func newProgressObserver(cmd *cobra.Command) *progressObserver {
	return &progressObserver{
		out:         cmd,
		interactive: isTerminal(cmd.ErrOrStderr()),
		lastUpdate:  map[string]time.Time{},
	}
}

// isTerminal reports whether a writer is an interactive terminal.
//
// A character device is the standard test and needs no dependency. It is not
// perfectly accurate — /dev/null is a character device — but the failure mode is
// a progress line that rewrites itself into a file the user sent to /dev/null,
// which costs nothing.
func isTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func (o *progressObserver) StageEntered(_ context.Context, plan *pipeline.StagePlan) {
	o.mu.Lock()
	defer o.mu.Unlock()

	fmt.Fprintf(o.err(), "  → %s\n", plan.Stage.Spec().Name)
}

func (o *progressObserver) StageProgress(_ context.Context, plan *pipeline.StagePlan, fraction float64, message string) {
	name := plan.Stage.Spec().Name

	o.mu.Lock()
	defer o.mu.Unlock()

	// Throttled again here even though the executor already coalesces: a stage
	// that reports progress in a tight loop would otherwise repaint the line
	// hundreds of times a second on a slow terminal.
	if time.Since(o.lastUpdate[name]) < 500*time.Millisecond && fraction < 1 {
		return
	}
	o.lastUpdate[name] = time.Now()

	if !o.interactive {
		return
	}

	fmt.Fprintf(o.err(), "\r  → %s %3.0f%%  %s\033[K",
		pad(name, 14), fraction*100, truncate(message, 40))
	o.lastLine = len(name)
}

func (o *progressObserver) StageSettled(_ context.Context, plan *pipeline.StagePlan) {
	o.mu.Lock()
	defer o.mu.Unlock()

	// The carriage-return progress line has to be cleared before the settled
	// line is written, or the two interleave into something unreadable.
	if !o.interactive {
		fmt.Fprintf(o.err(), "  %s  %s\n", pad(plan.Stage.Spec().Name, 14), stateLabel(plan.State))
		return
	}

	if o.lastLine > 0 {
		fmt.Fprint(o.err(), "\r\033[K")
		o.lastLine = 0
	}
}

// stateLabel renders a stage state for the non-interactive progress line.
func stateLabel(state pipeline.State) string {
	switch state {
	case pipeline.StateCompleted:
		return "done"
	case pipeline.StateCached:
		return "cached"
	case pipeline.StateSkipped:
		return "skipped"
	case pipeline.StateFailed:
		return "failed"
	default:
		return string(state)
	}
}

func (o *progressObserver) err() interface{ Write([]byte) (int, error) } {
	return o.out.ErrOrStderr()
}

// firstLine returns the first line of a message.
//
// Error messages from a stage are wrapped several times and often multi-line —
// an FFmpeg failure appends the command and its stderr. The summary wants the
// sentence, not the transcript.
func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return truncate(line, 120)
}

// truncate shortens text to a rune count, marking that it was cut.
func truncate(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

// progressReporter renders a byte-oriented download progress line.
//
// Separate from progressObserver because a download is measured in bytes rather
// than in pipeline stages, and the two have nothing in common but the carriage
// return.
type progressReporter struct {
	out  *cobra.Command
	name string

	interactive bool

	last  time.Time
	ended bool
}

func newProgressReporter(cmd *cobra.Command, name string) *progressReporter {
	return &progressReporter{
		out:         cmd,
		name:        name,
		interactive: isTerminal(cmd.ErrOrStderr()),
	}
}

// report is called as bytes arrive.
func (r *progressReporter) report(written, total int64) {
	// Throttled: the downloader reports per chunk, which for a gigabyte is
	// thousands of calls, and each one writes a terminal escape sequence.
	if time.Since(r.last) < 200*time.Millisecond && written < total {
		return
	}
	r.last = time.Now()

	if !r.interactive {
		return
	}

	if total <= 0 {
		// An unknown total is reported as a running byte count rather than as a
		// percentage. A progress bar that invents its denominator is worse than
		// no bar, because it moves backwards.
		fmt.Fprintf(r.out.ErrOrStderr(), "\r  %s  %s", r.name, formatBytes(written))
		return
	}

	percent := float64(written) / float64(total) * 100
	fmt.Fprintf(r.out.ErrOrStderr(), "\r  %s  %s / %s  %3.0f%%",
		r.name, formatBytes(written), formatBytes(total), percent)
}

// finish clears the progress line.
func (r *progressReporter) finish() {
	if r.ended {
		return
	}
	r.ended = true
	if r.interactive {
		fmt.Fprint(r.out.ErrOrStderr(), "\r\033[K")
	}
}
