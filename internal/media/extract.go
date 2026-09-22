package media

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ExtractAudio decodes one audio stream to a canonical WAV.
//
// durationSeconds is the source duration, used only to turn FFmpeg's progress
// output into a fraction. Pass 0 when it is unknown; progress is then reported
// as indeterminate rather than invented.
func (s *Service) ExtractAudio(
	ctx context.Context,
	input, output string,
	opts AudioOptions,
	durationSeconds float64,
	onProgress func(float64, string),
) error {
	ctx, cancel := s.context(ctx)
	defer cancel()

	args := ExtractAudioArgs(input, output, opts)

	var stderr bytes.Buffer
	stdout := newProgressWriter(durationSeconds, onProgress)

	if err := s.run(ctx, s.ffmpeg, args, stdout, &stderr); err != nil {
		return fmt.Errorf("media: extract audio from %s: %w\n  ffmpeg: %s",
			input, err, tail(stderr.String(), 20))
	}
	return nil
}

// GeneratePreview renders a short clip for the editor.
func (s *Service) GeneratePreview(
	ctx context.Context,
	input, output string,
	opts PreviewOptions,
	durationSeconds float64,
	onProgress func(float64, string),
) error {
	ctx, cancel := s.context(ctx)
	defer cancel()

	args := PreviewArgs(input, output, opts)

	var stderr bytes.Buffer
	stdout := newProgressWriter(durationSeconds, onProgress)

	if err := s.run(ctx, s.ffmpeg, args, stdout, &stderr); err != nil {
		return fmt.Errorf("media: generate preview from %s: %w\n  ffmpeg: %s",
			input, err, tail(stderr.String(), 20))
	}
	return nil
}

// Capabilities reports what the local FFmpeg can do.
func (s *Service) Capabilities(ctx context.Context) (*Capabilities, error) {
	ctx, cancel := s.context(ctx)
	defer cancel()

	caps := &Capabilities{Path: s.ffmpeg}

	var versionOut, versionErr bytes.Buffer
	if err := s.run(ctx, s.ffmpeg, VersionArgs(), &versionOut, &versionErr); err != nil {
		return nil, fmt.Errorf("media: read ffmpeg version: %w\n  ffmpeg: %s",
			err, tail(versionErr.String(), 10))
	}
	caps.Version = parseVersion(versionOut.String())

	// Encoder availability drives the render defaults: hardware encoders are
	// several times faster than libx264, and which ones exist varies by build.
	var encodersOut, encodersErr bytes.Buffer
	if err := s.run(ctx, s.ffmpeg, EncodersArgs(), &encodersOut, &encodersErr); err != nil {
		// A missing encoder list is not fatal: libx264 is always present and is
		// the fallback.
		s.log.Debug("could not list ffmpeg encoders", "error", err)
		return caps, nil
	}
	caps.Encoders = parseEncoders(encodersOut.String())

	return caps, nil
}

// parseVersion extracts the version from ffmpeg -version's first line.
func parseVersion(out string) string {
	line, _, _ := strings.Cut(out, "\n")
	fields := strings.Fields(line)
	for i, field := range fields {
		if field == "version" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return strings.TrimSpace(line)
}

// parseEncoders reads the encoder names out of ffmpeg -encoders.
//
// The output is a table with flags, a name and a description per line. Only the
// names matter, and only the ones this project would choose between.
func parseEncoders(out string) []string {
	var encoders []string
	wanted := map[string]bool{
		"libx264": true, "libx265": true,
		"h264_videotoolbox": true, "hevc_videotoolbox": true,
		"h264_nvenc": true, "hevc_nvenc": true,
		"h264_vaapi": true, "h264_qsv": true,
		"aac": true, "libopus": true,
	}

	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// Data lines look like " V....D libx264  H.264 ...". The name is the
		// second field when the first is a flag column.
		name := fields[1]
		if wanted[name] {
			encoders = append(encoders, name)
		}
	}
	return encoders
}

// ---------------------------------------------------------------------------
// Progress
// ---------------------------------------------------------------------------

// progressWriter parses FFmpeg's -progress stream.
//
// The format is one key=value per line, terminated by a `progress=` line that
// says whether the run is continuing or finished. FFmpeg writes it to stdout
// while all its human-readable output goes to stderr, which is what makes this
// parseable without noise.
type progressWriter struct {
	totalSeconds float64
	report       func(float64, string)

	buffer   []byte
	lastSent time.Time
	lastPct  float64
}

// progressThrottle bounds how often a fraction reaches the caller.
//
// FFmpeg emits a full progress block several times a second. Forwarding each
// one would flood the event stream with values a user cannot perceive, and the
// stage's own throttling would drop most of them anyway.
const progressThrottle = 250 * time.Millisecond

func newProgressWriter(totalSeconds float64, report func(float64, string)) *progressWriter {
	return &progressWriter{totalSeconds: totalSeconds, report: report}
}

func (p *progressWriter) Write(chunk []byte) (int, error) {
	p.buffer = append(p.buffer, chunk...)

	for {
		index := bytes.IndexByte(p.buffer, '\n')
		if index < 0 {
			break
		}
		line := string(bytes.TrimRight(p.buffer[:index], "\r"))
		p.buffer = p.buffer[index+1:]
		p.parseLine(line)
	}
	return len(chunk), nil
}

func (p *progressWriter) parseLine(line string) {
	key, value, found := strings.Cut(line, "=")
	if !found {
		return
	}

	switch key {
	case "out_time_us", "out_time_ms":
		// Both keys carry microseconds in practice: out_time_ms is misnamed
		// upstream and has been for years. Converting either as microseconds
		// gives the right answer; treating out_time_ms as milliseconds gives a
		// progress bar that finishes a thousand times too early.
		micros, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || micros < 0 {
			return
		}
		p.emit(float64(micros) / 1e6)

	case "progress":
		// "end" is the final block. Reporting 1.0 here rather than relying on
		// the last time value is what makes a finished stage show a finished
		// bar even when FFmpeg's final timestamp is slightly short.
		if strings.TrimSpace(value) == "end" {
			p.send(1.0)
		}
	}
}

func (p *progressWriter) emit(seconds float64) {
	if p.totalSeconds <= 0 {
		// Without a duration there is no honest fraction. Reporting one anyway
		// would be a number the user cannot trust.
		return
	}
	p.send(seconds / p.totalSeconds)
}

func (p *progressWriter) send(fraction float64) {
	if p.report == nil {
		return
	}
	if fraction < 0 {
		fraction = 0
	}
	if fraction > 1 {
		fraction = 1
	}

	// A finished run is always reported, so a bar cannot stop at 99% and look
	// stuck.
	if fraction < 1 && time.Since(p.lastSent) < progressThrottle {
		return
	}
	if fraction < p.lastPct {
		// Progress that goes backwards reads as a bug; clamping keeps a
		// seek-induced rewind from showing one.
		fraction = p.lastPct
	}

	p.lastSent = time.Now()
	p.lastPct = fraction
	p.report(fraction, "processing")
}
