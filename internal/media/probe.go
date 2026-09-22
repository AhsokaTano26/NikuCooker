package media

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Probe reads a file's structure with ffprobe.
func (s *Service) Probe(ctx context.Context, path string) (*MediaInfo, error) {
	ctx, cancel := s.context(ctx)
	defer cancel()

	var stdout, stderr bytes.Buffer
	args := ProbeArgs(path)

	if err := s.run(ctx, s.ffprobe, args, &stdout, &stderr); err != nil {
		// The stderr tail is the whole diagnosis: "could not probe" alone sends
		// a user looking at the wrong thing, while "Invalid data found when
		// processing input" tells them the file is broken.
		return nil, fmt.Errorf("%w: %s\n  ffprobe: %s\n  command: ffprobe %s",
			ErrProbeFailed, path, tail(stderr.String(), 20), args.Display())
	}

	var raw probeOutput
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		return nil, fmt.Errorf("%w: %s: ffprobe returned unparseable JSON: %w",
			ErrProbeFailed, path, err)
	}

	info := raw.toMediaInfo(path)
	if info.Duration <= 0 && info.Format.Duration > 0 {
		info.Duration = info.Format.Duration
	}
	if info.Duration <= 0 && len(info.Audio) > 0 {
		// Some containers omit the format duration but carry it per stream.
		info.Duration = info.Audio[0].Duration
	}
	if info.Duration <= 0 {
		return nil, fmt.Errorf("%w: %s: no usable duration; the file may be truncated or still being written",
			ErrProbeFailed, path)
	}

	info.ProbedAt = time.Now().UTC()
	return info, nil
}

// ---------------------------------------------------------------------------
// ffprobe's JSON shape
// ---------------------------------------------------------------------------

// The raw types exist because ffprobe's output does not match its own
// documentation: numeric fields arrive as strings in some builds and numbers in
// others, "N/A" appears where a field is unknown, and an absent key means the
// same as a zero. Decoding straight into the domain types would fail on files
// that are perfectly readable.

type probeOutput struct {
	Format   *probeFormat   `json:"format"`
	Streams  []probeStream  `json:"streams"`
	Chapters []probeChapter `json:"chapters"`
}

type probeFormat struct {
	FormatName string            `json:"format_name"`
	LongName   string            `json:"long_name"`
	Duration   flexFloat         `json:"duration"`
	Size       flexInt           `json:"size"`
	BitRate    flexInt           `json:"bit_rate"`
	NbStreams  flexInt           `json:"nb_streams"`
	Tags       map[string]string `json:"tags"`
}

type probeStream struct {
	Index         int               `json:"index"`
	CodecType     string            `json:"codec_type"`
	CodecName     string            `json:"codec_name"`
	CodecLong     string            `json:"codec_long_name"`
	Profile       string            `json:"profile"`
	Width         flexInt           `json:"width"`
	Height        flexInt           `json:"height"`
	RFrameRate    string            `json:"r_frame_rate"`
	AvgFrameRate  string            `json:"avg_frame_rate"`
	PixFmt        string            `json:"pix_fmt"`
	SampleRate    flexInt           `json:"sample_rate"`
	Channels      flexInt           `json:"channels"`
	ChannelLayout string            `json:"channel_layout"`
	Duration      flexFloat         `json:"duration"`
	BitRate       flexInt           `json:"bit_rate"`
	Tags          map[string]string `json:"tags"`
	Disposition   struct {
		Default flexInt `json:"default"`
		Forced  flexInt `json:"forced"`
	} `json:"disposition"`
}

type probeChapter struct {
	StartTime flexFloat         `json:"start_time"`
	EndTime   flexFloat         `json:"end_time"`
	Tags      map[string]string `json:"tags"`
}

func (raw probeOutput) toMediaInfo(path string) *MediaInfo {
	info := &MediaInfo{Path: path}

	if raw.Format != nil {
		info.Format = Format{
			Name:       firstNonEmpty(raw.Format.FormatName, "unknown"),
			LongName:   raw.Format.LongName,
			FormatName: raw.Format.FormatName,
			Duration:   float64(raw.Format.Duration),
			SizeBytes:  int64(raw.Format.Size),
			BitRate:    int64(raw.Format.BitRate),
			NbStreams:  int(raw.Format.NbStreams),
			Tags:       raw.Format.Tags,
		}
		info.Duration = info.Format.Duration
	}

	for _, stream := range raw.Streams {
		converted := stream.toStream()
		switch stream.CodecType {
		case "video":
			// Cover art is a video stream with one frame and no duration. It is
			// not a picture the user wants subtitled, and treating it as one
			// makes an audio-only file look like a video.
			if converted.Duration == 0 && converted.FrameRate == 0 {
				continue
			}
			info.Video = append(info.Video, converted)
		case "audio":
			info.Audio = append(info.Audio, converted)
		case "subtitle":
			info.Subtitle = append(info.Subtitle, converted)
		}
	}

	for _, chapter := range raw.Chapters {
		info.Chapters = append(info.Chapters, Chapter{
			Start: float64(chapter.StartTime),
			End:   float64(chapter.EndTime),
			Title: chapter.Tags["title"],
			Tags:  chapter.Tags,
		})
	}

	return info
}

func (raw probeStream) toStream() Stream {
	stream := Stream{
		Index:         raw.Index,
		Type:          raw.CodecType,
		Codec:         raw.CodecName,
		CodecLong:     raw.CodecLong,
		Profile:       raw.Profile,
		Width:         int(raw.Width),
		Height:        int(raw.Height),
		FrameRate:     parseFrameRate(raw.RFrameRate),
		PixelFormat:   raw.PixFmt,
		SampleRate:    int(raw.SampleRate),
		Channels:      int(raw.Channels),
		ChannelLayout: raw.ChannelLayout,
		Duration:      float64(raw.Duration),
		BitRate:       int64(raw.BitRate),
		Default:       raw.Disposition.Default != 0,
		Forced:        raw.Disposition.Forced != 0,
	}

	if stream.FrameRate == 0 {
		stream.FrameRate = parseFrameRate(raw.AvgFrameRate)
	}
	if raw.Tags != nil {
		stream.Language = raw.Tags["language"]
		stream.Title = firstNonEmpty(raw.Tags["title"], raw.Tags["handler_name"])
	}
	return stream
}

// parseFrameRate reads ffprobe's "num/den" frame rate.
//
// A zero denominator appears on some streams, and dividing by it panics.
func parseFrameRate(raw string) float64 {
	if raw == "" || raw == "N/A" || raw == "0/0" {
		return 0
	}
	numerator, denominator, found := strings.Cut(raw, "/")
	if !found {
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return 0
		}
		return value
	}

	num, err := strconv.ParseFloat(numerator, 64)
	if err != nil {
		return 0
	}
	den, err := strconv.ParseFloat(denominator, 64)
	if err != nil || den == 0 {
		return 0
	}
	return num / den
}

// ---------------------------------------------------------------------------
// Flexible scalars
// ---------------------------------------------------------------------------

// flexInt accepts a JSON number or a numeric string.
type flexInt int64

func (f *flexInt) UnmarshalJSON(data []byte) error {
	text := strings.Trim(string(data), `"`)
	if text == "" || text == "null" || text == "N/A" {
		*f = 0
		return nil
	}
	// ffprobe emits fractional values where integers are documented, so a
	// parse as float and a truncation is more forgiving than failing the file.
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		*f = 0
		return nil
	}
	*f = flexInt(value)
	return nil
}

// flexFloat accepts a JSON number or a numeric string.
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(data []byte) error {
	text := strings.Trim(string(data), `"`)
	if text == "" || text == "null" || text == "N/A" {
		*f = 0
		return nil
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		*f = 0
		return nil
	}
	*f = flexFloat(value)
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// tail returns the last n lines of a command's stderr.
//
// The last lines matter: FFmpeg prints its real complaint at the end, after
// pages of stream mapping.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
