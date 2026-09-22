package media

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// RenderMode selects how subtitles reach the output.
type RenderMode string

const (
	// RenderSoft muxes the subtitles in as a track. The viewer can turn them off
	// and switch between them, and the video is not re-encoded, so the render
	// takes seconds and loses nothing.
	RenderSoft RenderMode = "soft"

	// RenderHard draws the subtitles into the picture. It is what a phone, a
	// television or a social platform needs, because none of them will look at
	// the subtitle track — and it is destructive and slow, because every frame
	// is re-encoded.
	RenderHard RenderMode = "hard"
)

// RenderOptions configures a render.
type RenderOptions struct {
	Mode RenderMode

	// Encoder is the video encoder. Empty resolves from capabilities.
	Encoder string

	CRF          int
	Preset       string
	AudioBitrate string

	// SubtitleLanguage tags the subtitle track, as an ISO 639-2 code. Tagging
	// it is what lets a player choose the Chinese track automatically instead
	// of showing whatever happens to be first.
	SubtitleLanguage string

	// SubtitleTitle is the track's display name.
	SubtitleTitle string

	// FontsDir is where the burn-in pass should look for fonts.
	//
	// The subtitles filter renders through libass, which is a separate library
	// from FFmpeg with its own font database. On a machine where the font is
	// installed but fontconfig cannot see it — a container, a portable build —
	// the text renders in a fallback font or not at all.
	FontsDir string
}

// DefaultRenderOptions returns the settings a first run uses.
func DefaultRenderOptions() RenderOptions {
	return RenderOptions{
		Mode:         RenderSoft,
		CRF:          20,
		Preset:       "medium",
		AudioBitrate: "192k",
	}
}

// Render produces a video with subtitles.
//
// The two modes are different enough to be separate functions, and this
// dispatches rather than trying to share an argument vector: a soft render
// copies every stream and a hard render re-encodes the picture, and a builder
// that tried to do both would be a list of conditionals around which flags
// apply.
func (s *Service) Render(
	ctx context.Context,
	input, subtitlePath, output string,
	opts RenderOptions,
	durationSeconds float64,
	onProgress func(float64, string),
) ([]string, error) {
	ctx, cancel := s.context(ctx)
	defer cancel()

	if opts.Mode == RenderHard {
		return s.renderHard(ctx, input, subtitlePath, output, opts, durationSeconds, onProgress)
	}
	return s.renderSoft(ctx, input, subtitlePath, output, opts, durationSeconds, onProgress)
}

// renderSoft muxes the subtitles in without touching the video stream.
//
// It returns the warnings worth surfacing, because one of them is important and
// invisible: an MP4 container cannot carry ASS styling, so a soft render into
// MP4 silently produces plain text where the MKV would have had a styled
// outline. The user asked for styled subtitles and would otherwise find out on
// playback.
func (s *Service) renderSoft(
	ctx context.Context,
	input, subtitlePath, output string,
	opts RenderOptions,
	durationSeconds float64,
	onProgress func(float64, string),
) ([]string, error) {
	args := RenderSoftArgs(input, subtitlePath, output, opts)

	var stderr bytes.Buffer
	stdout := newProgressWriter(durationSeconds, onProgress)

	if err := s.run(ctx, s.ffmpeg, args, stdout, &stderr); err != nil {
		return nil, fmt.Errorf("media: mux subtitles into %s: %w\n  ffmpeg: %s",
			output, err, tail(stderr.String(), 20))
	}

	var warnings []string
	if codec, _ := softSubtitleCodec(output); codec == "mov_text" {
		warnings = append(warnings, fmt.Sprintf(
			"%s cannot carry subtitle styling; the output has plain text. Write an .mkv to keep the styling, or use hard subtitles.",
			strings.ToUpper(filepath.Ext(output))))
	}
	return warnings, nil
}

// renderHard burns the subtitles into the picture.
//
// The encoder is retried once on the software path. Hardware encoders are
// several times faster and fail in ways that are specific to the machine —
// a missing driver, an unsupported profile, a container that cannot hold the
// result — and a render that took four minutes of encoding should not be
// thrown away because the GPU path was unavailable.
func (s *Service) renderHard(
	ctx context.Context,
	input, subtitlePath, output string,
	opts RenderOptions,
	durationSeconds float64,
	onProgress func(float64, string),
) ([]string, error) {
	encoder := opts.Encoder
	if encoder == "" {
		encoder = "libx264"
	}

	var stderr bytes.Buffer
	stdout := newProgressWriter(durationSeconds, onProgress)

	args := RenderHardArgs(input, subtitlePath, output, opts, encoder)
	if err := s.run(ctx, s.ffmpeg, args, stdout, &stderr); err != nil {
		if encoder == "libx264" {
			return nil, fmt.Errorf("media: burn subtitles into %s: %w\n  ffmpeg: %s",
				output, err, tail(stderr.String(), 20))
		}

		// Retried on the software encoder rather than reported, and reported
		// afterwards so the user knows the render is slower than it could be.
		s.log.Warn("the hardware encoder failed; retrying with libx264",
			"encoder", encoder, "error", err)

		fallback := opts.Encoder
		opts.Encoder = "libx264"
		retryArgs := RenderHardArgs(input, subtitlePath, output, opts, "libx264")
		opts.Encoder = fallback

		stderr.Reset()
		// A fresh writer: the first one has already reported progress, and
		// reusing it would suppress the retry's output as "going backwards".
		stdout = newProgressWriter(durationSeconds, onProgress)

		if retryErr := s.run(ctx, s.ffmpeg, retryArgs, stdout, &stderr); retryErr != nil {
			return nil, fmt.Errorf(
				"media: burn subtitles into %s: %w (the %s attempt failed first: %v)\n  ffmpeg: %s",
				output, retryErr, encoder, err, tail(stderr.String(), 20))
		}
		return []string{fmt.Sprintf(
			"the %s encoder failed and the render used libx264 instead, which is several times slower",
			encoder)}, nil
	}

	return nil, nil
}

// ---------------------------------------------------------------------------
// Argument construction
// ---------------------------------------------------------------------------

// RenderSoftArgs builds a stream-copy mux of the source and a subtitle file.
func RenderSoftArgs(input, subtitlePath, output string, opts RenderOptions) Args {
	args := append(Args{}, baseFlags...)
	args = append(args, "-y", "-loglevel", "error")
	args = append(args, "-i", input, "-i", subtitlePath)

	// Every stream from the source, then the subtitles. Without the explicit
	// second map, FFmpeg's default stream selection picks one stream per type
	// and the subtitle file is silently dropped.
	args = append(args, "-map", "0", "-map", "1")

	// Copying is the whole point: a soft render must not re-encode, and any
	// video re-encode here would be a quality loss for no gain.
	args = append(args, "-c", "copy")

	if codec, ok := softSubtitleCodec(output); ok {
		args = append(args, "-c:s", codec)
	}

	// The metadata is what lets a player select this track automatically rather
	// than showing the first one it finds.
	index := "0"
	if opts.SubtitleLanguage != "" {
		args = append(args, "-metadata:s:s:"+index, "language="+opts.SubtitleLanguage)
	}
	if opts.SubtitleTitle != "" {
		args = append(args, "-metadata:s:s:"+index, "title="+opts.SubtitleTitle)
	}
	args = append(args, "-disposition:s:"+index, "default")

	// Only meaningful for MP4-family containers, and ignored elsewhere.
	args = append(args, "-movflags", "+faststart")

	args = append(args, output)
	return args
}

// RenderHardArgs builds a burn-in render.
func RenderHardArgs(input, subtitlePath, output string, opts RenderOptions, encoder string) Args {
	args := append(Args{}, baseFlags...)
	args = append(args, "-y", "-loglevel", "error")
	args = append(args, "-i", input)

	// The filter's own argument syntax, built here as one vector element and
	// never as a shell string: FFmpeg parses the filtergraph itself, and the
	// commas and colons inside it are its separators, not a shell's.
	args = append(args, "-vf", subtitleFilter(subtitlePath, opts.FontsDir))

	if opts.Encoder == "" {
		opts.Encoder = encoder
	}

	crf := opts.CRF
	if crf == 0 {
		crf = 20
	}
	preset := opts.Preset
	if preset == "" {
		preset = "medium"
	}

	args = append(args, "-c:v", encoder)
	if !isHardwareEncoder(encoder) {
		// CRF is meaningless to the hardware encoders, which take a bitrate or
		// a quality level under a different name; passing it to them is at best
		// ignored and at worst an error.
		args = append(args, "-crf", strconv.Itoa(crf), "-preset", preset)
	}

	// Burning changes the colour handling enough that some sources come out as
	// 4:4:4 or 10-bit, which many players refuse. 4:2:0 8-bit is what every
	// player accepts.
	args = append(args, "-pix_fmt", "yuv420p")

	// Audio is copied, not re-encoded: burning subtitles cannot change it, and
	// a second lossy pass over the audio would be pure loss.
	args = append(args, "-c:a", "copy")

	args = append(args, "-movflags", "+faststart")

	args = append(args, progressFlags...)
	args = append(args, output)
	return args
}

// subtitleFilter renders the subtitles filtergraph.
func subtitleFilter(subtitlePath, fontsDir string) string {
	var b strings.Builder
	b.WriteString("subtitles=filename=")
	b.WriteString(escapeFilterPath(subtitlePath))

	if fontsDir != "" {
		b.WriteString(":fontsdir=")
		b.WriteString(escapeFilterPath(fontsDir))
	}
	return b.String()
}

// escapeFilterPath prepares a path for use inside an FFmpeg filter argument.
//
// This is a third escaping language, distinct from the shell's and from
// FFmpeg's own option parser, and getting it wrong is silent: the filter either
// fails with "No such file" for a file that plainly exists, or — worse on
// Windows — reads the drive-letter colon as an option separator and renders
// without subtitles.
//
// The characters below are the ones the filtergraph parser treats as
// structural. Backslash is handled in the same pass as everything else, so a
// path that already contains one is not double-escaped.
func escapeFilterPath(path string) string {
	var b strings.Builder
	b.Grow(len(path) + 8)

	for _, r := range path {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\'':
			b.WriteString(`\'`)
		case ':':
			b.WriteString(`\:`)
		case ',':
			b.WriteString(`\,`)
		case ';':
			b.WriteString(`\;`)
		case '[':
			b.WriteString(`\[`)
		case ']':
			b.WriteString(`\]`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// softSubtitleCodec picks the subtitle codec a container can carry.
//
// The second return value is false when the container is unknown, in which case
// the caller leaves the codec to FFmpeg — which will copy the stream when it can
// and fail with a clear message when it cannot.
func softSubtitleCodec(output string) (string, bool) {
	switch strings.ToLower(filepath.Ext(output)) {
	case ".mp4", ".m4v", ".mov":
		// The only subtitle codec the MP4 family defines. It carries plain text
		// and position but not styling, which the caller warns about.
		return "mov_text", true
	case ".mkv", ".webm":
		// Matroska carries ASS natively, so the styling survives.
		return "ass", true
	default:
		return "", false
	}
}

// isHardwareEncoder reports whether an encoder ignores CRF and preset.
func isHardwareEncoder(encoder string) bool {
	for _, marker := range []string{"videotoolbox", "nvenc", "vaapi", "qsv", "amf"} {
		if strings.Contains(encoder, marker) {
			return true
		}
	}
	return false
}

// SubtitleExtensionFor returns the subtitle extension a container prefers.
//
// Matroska plays ASS with its styling; the MP4 family reduces it to plain
// text, so writing ASS there produces a file whose styling is silently
// discarded on the way in.
func SubtitleExtensionFor(output string) string {
	switch strings.ToLower(filepath.Ext(output)) {
	case ".mp4", ".m4v", ".mov":
		return ".srt"
	default:
		return ".ass"
	}
}
