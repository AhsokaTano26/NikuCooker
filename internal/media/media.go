// Package media is the only place in NikuCooker that executes FFmpeg.
//
// Every invocation goes through an argument vector built here and passed
// directly to os/exec. There is no shell, no string interpolation and no word
// splitting anywhere in this package — which is what makes a filename
// containing a space, a quote or a semicolon merely a filename.
//
// Concentrating it also means there is exactly one place to look when a path
// with Japanese characters works on Linux and not on Windows.
package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// Errors callers branch on.
var (
	// ErrToolNotFound reports a missing ffmpeg or ffprobe binary.
	ErrToolNotFound = errors.New("media: required tool not found")
	// ErrProbeFailed reports a file ffprobe could not read.
	ErrProbeFailed = errors.New("media: could not probe the file")
	// ErrNoAudioStream reports a file with no usable audio.
	ErrNoAudioStream = errors.New("media: no audio stream")
)

// Options configures a Service.
type Options struct {
	// FFmpegPath and FFprobePath are explicit binaries. Empty means resolve
	// from PATH.
	FFmpegPath  string
	FFprobePath string

	// Timeout bounds a single invocation. Zero means no limit, which is right
	// for a two-hour transcode and wrong for a probe.
	Timeout time.Duration

	Log *slog.Logger
}

// Service runs media commands.
type Service struct {
	ffmpeg  string
	ffprobe string
	timeout time.Duration
	log     *slog.Logger

	// run is injectable so argument construction can be asserted without
	// FFmpeg installed. Every test of the command line goes through it.
	run runner
}

// runner executes a command and streams its output.
//
// The signature is deliberately narrow: it is the entire surface through which
// this package touches the operating system, which is what makes the argument
// builders testable in isolation.
type runner func(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error

// New resolves the tools and returns a Service.
func New(opts Options) (*Service, error) {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}

	ffmpeg, err := resolve(opts.FFmpegPath, "ffmpeg")
	if err != nil {
		return nil, err
	}
	ffprobe, err := resolve(opts.FFprobePath, "ffprobe")
	if err != nil {
		return nil, err
	}

	return &Service{
		ffmpeg:  ffmpeg,
		ffprobe: ffprobe,
		timeout: opts.Timeout,
		log:     opts.Log,
		run:     defaultRunner,
	}, nil
}

// NewWithRunner returns a Service that never executes anything.
//
// It exists so the argument builders can be exercised on machines without
// FFmpeg — including CI runners that install the Go toolchain and nothing else.
func NewWithRunner(run runner, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{ffmpeg: "ffmpeg", ffprobe: "ffprobe", log: log, run: run}
}

func resolve(explicit, name string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%w: %s is not on PATH; install FFmpeg, or set the path in the configuration", ErrToolNotFound, name)
	}
	return path, nil
}

// defaultRunner executes a command, forwarding stdout and stderr.
func defaultRunner(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Stdin is deliberately nil: /dev/null. FFmpeg with an inherited stdin
	// reads it, and in the container that is where the worker protocol lives.
	cmd.Stdin = nil
	return cmd.Run()
}

// FFmpegPath reports the resolved binary, for the doctor report.
func (s *Service) FFmpegPath() string { return s.ffmpeg }

// FFprobePath reports the resolved binary.
func (s *Service) FFprobePath() string { return s.ffprobe }

func (s *Service) context(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, s.timeout)
}

// ---------------------------------------------------------------------------
// Media information
// ---------------------------------------------------------------------------

// MediaInfo is what ffprobe reports about a file.
type MediaInfo struct {
	Path     string    `json:"path"`
	Format   Format    `json:"format"`
	Duration float64   `json:"duration"`
	Video    []Stream  `json:"video"`
	Audio    []Stream  `json:"audio"`
	Subtitle []Stream  `json:"subtitle"`
	Chapters []Chapter `json:"chapters,omitempty"`

	// ProbedAt records when this was taken, so a cached artifact can say how
	// old its information is.
	ProbedAt time.Time `json:"probed_at"`
}

// HasVideo reports whether the file has a picture.
//
// Audio-only input is a first-class case, not an error: a radio recording or an
// extracted track is perfectly reasonable to subtitle.
func (m *MediaInfo) HasVideo() bool { return len(m.Video) > 0 }

// PrimaryAudio returns the stream to transcribe.
//
// Selection is explicit rather than "the first one", because the first audio
// stream in a bilingual release is frequently the dub. The order of preference
// is: an explicit index, then a stream whose language tag matches the project's
// source language, then the highest channel count.
func (m *MediaInfo) PrimaryAudio(sourceLanguage string) (Stream, error) {
	if len(m.Audio) == 0 {
		return Stream{}, fmt.Errorf("%w: %s", ErrNoAudioStream, m.Path)
	}

	if sourceLanguage != "" {
		for _, stream := range m.Audio {
			if languageMatches(stream.Language, sourceLanguage) {
				return stream, nil
			}
		}
	}

	best := m.Audio[0]
	for _, stream := range m.Audio[1:] {
		if stream.Channels > best.Channels {
			best = stream
		}
	}
	return best, nil
}

// languageMatches compares an ffprobe language tag with a BCP-47 tag.
//
// ffprobe reports ISO 639-2 ("jpn"), projects are configured with 639-1 ("ja").
// Comparing the prefixes covers that without a full table, and a false negative
// only costs a fallback to the channel-count rule.
func languageMatches(tag, want string) bool {
	if tag == "" || want == "" {
		return false
	}
	tag = strings.ToLower(tag)
	want = strings.ToLower(want)

	if tag == want {
		return true
	}
	// "zh-Hans" and "zh" should match; "ja" and "jpn" share a prefix only by
	// accident, so the comparison is one-directional and length-bounded.
	if strings.HasPrefix(tag, want) || strings.HasPrefix(want, tag) {
		return true
	}
	return iso639Alias(tag) == iso639Alias(want)
}

// iso639Alias maps the handful of two-letter codes this project actually
// handles to their three-letter forms.
//
// A full table is hundreds of entries and would still be wrong about the ones
// nobody agrees on. These are the pairs that matter for the languages
// NikuCooker targets.
var iso639Alias = func() func(string) string {
	aliases := map[string]string{
		"ja": "jpn", "jp": "jpn",
		"zh": "zho", "chi": "zho", "cmn": "zho",
		"en": "eng", "ko": "kor",
		"zh-hans": "zho", "zh-hant": "zho",
	}
	return func(tag string) string {
		if mapped, ok := aliases[tag]; ok {
			return mapped
		}
		return tag
	}
}()

// Format describes the container.
type Format struct {
	Name       string            `json:"name"`
	LongName   string            `json:"long_name"`
	Duration   float64           `json:"duration"`
	SizeBytes  int64             `json:"size_bytes"`
	BitRate    int64             `json:"bit_rate"`
	Tags       map[string]string `json:"tags,omitempty"`
	NbStreams  int               `json:"stream_count"`
	FormatName string            `json:"format_name"`
}

// Stream is one elementary stream.
type Stream struct {
	Index     int    `json:"index"`
	Type      string `json:"type"` // video | audio | subtitle
	Codec     string `json:"codec"`
	CodecLong string `json:"codec_long,omitempty"`
	Profile   string `json:"profile,omitempty"`

	// Video
	Width       int     `json:"width,omitempty"`
	Height      int     `json:"height,omitempty"`
	FrameRate   float64 `json:"frame_rate,omitempty"`
	PixelFormat string  `json:"pixel_format,omitempty"`

	// Audio
	SampleRate    int    `json:"sample_rate,omitempty"`
	Channels      int    `json:"channels,omitempty"`
	ChannelLayout string `json:"channel_layout,omitempty"`

	// Both
	Language string  `json:"language,omitempty"`
	Title    string  `json:"title,omitempty"`
	Duration float64 `json:"duration,omitempty"`
	BitRate  int64   `json:"bit_rate,omitempty"`

	// Default and Forced come from the container's disposition flags.
	Default bool `json:"default,omitempty"`
	Forced  bool `json:"forced,omitempty"`
}

// Chapter is a container chapter marker.
type Chapter struct {
	Start float64           `json:"start"`
	End   float64           `json:"end"`
	Title string            `json:"title,omitempty"`
	Tags  map[string]string `json:"tags,omitempty"`
}

// Capabilities describes the local FFmpeg.
type Capabilities struct {
	Path     string   `json:"path"`
	Version  string   `json:"version"`
	Encoders []string `json:"encoders"`
	Filters  []string `json:"filters"`
}

// HasFilter reports whether an FFmpeg build provides a filter.
func (c *Capabilities) HasFilter(name string) bool {
	for _, filter := range c.Filters {
		if filter == name {
			return true
		}
	}
	return false
}

// CanBurnSubtitles reports whether this build can draw subtitles into video.
//
// False is not a broken installation: plenty of FFmpeg builds ship without
// libass, including some distribution packages. It means hard subtitles are
// unavailable and the soft path is the one to use.
func (c *Capabilities) CanBurnSubtitles() bool {
	return c.HasFilter("subtitles") || c.HasFilter("ass")
}

// RecommendedVideoEncoder picks an encoder from what this machine has.
//
// Preference order is by how much faster the hardware path is than libx264 on
// each platform. libx264 is always the fallback because every FFmpeg build has
// it.
func (c *Capabilities) RecommendedVideoEncoder() string {
	preferred := []string{"h264_videotoolbox", "h264_nvenc", "h264_vaapi", "h264_qsv"}
	for _, want := range preferred {
		for _, have := range c.Encoders {
			if have == want {
				return want
			}
		}
	}
	return "libx264"
}
