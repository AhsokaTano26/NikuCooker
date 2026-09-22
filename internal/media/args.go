package media

import (
	"fmt"
	"strconv"
	"strings"
)

// Args is a command line as an argument vector.
//
// Every external command in this package is built as one of these and passed
// straight to os/exec. There is no shell anywhere: no interpolation, no
// quoting, no word splitting. That is what makes a path containing a space, a
// quote, a semicolon or a newline merely a path — the class of bug that turns a
// filename into a command injection simply cannot occur.
//
// Nothing in this file may be changed to build a string that is later parsed.
type Args []string

// Display renders the vector for a log line.
//
// It is for humans only and must never be executed or parsed back. It exists
// because a failed FFmpeg invocation is otherwise very hard to reproduce by
// hand — and reproducing it is exactly what a user needs to do.
func (a Args) Display() string {
	parts := make([]string, 0, len(a))
	for _, arg := range a {
		if arg == "" || strings.ContainsAny(arg, " \t\n\"'\\$`") {
			parts = append(parts, strconv.Quote(arg))
			continue
		}
		parts = append(parts, arg)
	}
	return strings.Join(parts, " ")
}

// ---------------------------------------------------------------------------
// Shared flags
// ---------------------------------------------------------------------------

// baseFlags are prepended to every invocation.
//
// -nostdin matters more than it looks: without it FFmpeg reads the parent's
// standard input, and in a container that is where the Python worker's protocol
// traffic lives. A stray read there is a hang with no obvious cause.
//
// -hide_banner keeps the version and build-configuration block out of the
// stderr that a user sees when something fails.
var baseFlags = Args{"-hide_banner", "-nostdin"}

// progressFlags make FFmpeg report machine-readable progress on stdout.
//
// -nostats suppresses the human-readable line, so stdout carries only the
// key=value stream and parsing is unambiguous.
var progressFlags = Args{"-progress", "pipe:1", "-nostats"}

// ---------------------------------------------------------------------------
// ffprobe
// ---------------------------------------------------------------------------

// ProbeArgs builds the ffprobe invocation.
//
// -print_format json rather than the default key=value output: durations and
// frame rates come back as strings either way, but JSON has a defined shape for
// absent fields, and a missing key is far easier to handle than a missing line.
func ProbeArgs(path string) Args {
	args := Args{"-hide_banner", "-loglevel", "error"}
	args = append(args,
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		// Chapter metadata is occasionally where a release group records the
		// episode title, which is useful context for translation.
		"-show_chapters",
		"-i", path,
	)
	return args
}

// ---------------------------------------------------------------------------
// Audio extraction
// ---------------------------------------------------------------------------

// AudioOptions configures extraction.
type AudioOptions struct {
	// StreamIndex is the stream's index *within the container*, as ffprobe
	// reports it. -1 means the first audio stream.
	//
	// Absolute rather than audio-relative on purpose. FFmpeg's `-map 0:a:N`
	// counts only audio streams, while ffprobe's `index` counts every stream —
	// so a file whose video is stream 0 has its first audio stream at index 1,
	// and `0:a:1` asks for a second audio stream that does not exist. That bug
	// is invisible on audio-only files and on any file where the audio happens
	// to come first, which is exactly the shape that survives to production.
	StreamIndex int

	// SampleRate is what the recogniser consumes. Whisper expects 16 kHz.
	SampleRate int

	// Channels is 1: Whisper is mono, and downmixing once here is cheaper than
	// doing it per chunk inside the model.
	Channels int

	// HighpassHz removes rumble below the speech band. Zero disables it.
	HighpassHz int

	// Loudnorm applies EBU R128 loudness normalisation.
	Loudnorm bool
}

// DefaultAudioOptions returns the settings a first run uses.
func DefaultAudioOptions() AudioOptions {
	return AudioOptions{
		StreamIndex: -1,
		SampleRate:  16000,
		Channels:    1,
		HighpassHz:  60,
		Loudnorm:    false,
	}
}

// ExtractAudioArgs builds the extraction invocation.
//
// One pass, one decode: selecting the stream, filtering and resampling all
// happen together. Splitting them across two invocations would decode the
// source twice and write an intermediate file, for no benefit — the two halves
// always run together.
func ExtractAudioArgs(input, output string, opts AudioOptions) Args {
	args := append(Args{}, baseFlags...)
	args = append(args, "-y", "-loglevel", "error")

	if opts.StreamIndex >= 0 {
		args = append(args, "-i", input, "-map", fmt.Sprintf("0:%d", opts.StreamIndex))
	} else {
		args = append(args, "-i", input, "-map", "0:a:0")
	}

	// Nothing but the audio track. Without these FFmpeg copies the video and
	// subtitle streams the -map might still admit, and the "audio" file becomes
	// as large as the source.
	args = append(args, "-vn", "-sn", "-dn")

	if filters := audioFilters(opts); filters != "" {
		args = append(args, "-af", filters)
	}

	args = append(args,
		"-ac", strconv.Itoa(opts.Channels),
		"-ar", strconv.Itoa(opts.SampleRate),
		// Uncompressed PCM: the recogniser decodes it anyway, and a lossy
		// intermediate would add an artefact the model then transcribes.
		"-c:a", "pcm_s16le",
	)

	args = append(args, progressFlags...)
	args = append(args, output)
	return args
}

// audioFilters renders the filter chain.
//
// Returned as a single argument, not split on commas: FFmpeg parses the chain
// itself, and a shell would have interpreted the commas as separators if this
// were ever built as a string.
func audioFilters(opts AudioOptions) string {
	var filters []string

	if opts.HighpassHz > 0 {
		filters = append(filters, fmt.Sprintf("highpass=f=%d", opts.HighpassHz))
	}
	if opts.Loudnorm {
		// Single-pass loudnorm. Two-pass is more accurate but needs a full
		// analysis run first, and the difference is not worth a second decode
		// of a two-hour source for speech recognition.
		filters = append(filters, "loudnorm=I=-16:TP=-1.5:LRA=11")
	}

	return strings.Join(filters, ",")
}

// ---------------------------------------------------------------------------
// Preview
// ---------------------------------------------------------------------------

// PreviewOptions configures a bounded render for the editor.
type PreviewOptions struct {
	Start    float64
	Duration float64
	// ScaleHeight of 0 keeps the source resolution.
	ScaleHeight int
	CRF         int
	Preset      string
}

// PreviewArgs builds a short render for editor scrubbing.
//
// It re-encodes: the point is a small file that starts instantly, which
// stream-copying a slice of a long video does not give.
func PreviewArgs(input, output string, opts PreviewOptions) Args {
	args := append(Args{}, baseFlags...)
	args = append(args, "-y", "-loglevel", "error")

	// -ss before -i seeks by keyframe and is much faster; the cost is that the
	// cut lands on the nearest keyframe rather than exactly at Start. For a
	// preview that is the right trade.
	if opts.Start > 0 {
		args = append(args, "-ss", formatSeconds(opts.Start))
	}
	args = append(args, "-i", input)

	if opts.Duration > 0 {
		args = append(args, "-t", formatSeconds(opts.Duration))
	}

	if opts.ScaleHeight > 0 {
		// -2 keeps the aspect ratio and rounds to an even number, which H.264
		// requires.
		args = append(args, "-vf", fmt.Sprintf("scale=-2:%d", opts.ScaleHeight))
	}

	crf := opts.CRF
	if crf == 0 {
		crf = 23
	}
	preset := opts.Preset
	if preset == "" {
		preset = "veryfast"
	}

	args = append(args,
		"-c:v", "libx264",
		"-crf", strconv.Itoa(crf),
		"-preset", preset,
		"-c:a", "aac",
		"-b:a", "128k",
		"-movflags", "+faststart",
	)

	args = append(args, progressFlags...)
	args = append(args, output)
	return args
}

// formatSeconds renders a duration the way FFmpeg accepts it.
//
// Always with a decimal point: a bare integer is interpreted as a frame count
// by some FFmpeg options, and "90" seeking to frame 90 rather than 90 seconds
// is a confusing failure.
func formatSeconds(seconds float64) string {
	return strconv.FormatFloat(seconds, 'f', 3, 64)
}

// ---------------------------------------------------------------------------
// Version
// ---------------------------------------------------------------------------

// VersionArgs probes an FFmpeg binary's version.
func VersionArgs() Args {
	return Args{"-hide_banner", "-version"}
}

// EncodersArgs lists available encoders, used to pick render defaults.
func EncodersArgs() Args {
	return Args{"-hide_banner", "-encoders"}
}
