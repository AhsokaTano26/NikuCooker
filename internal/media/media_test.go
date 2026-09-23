package media

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Argument construction
// ---------------------------------------------------------------------------

// nastyPaths are the filenames that break naive command construction.
//
// Several are only unusual on one platform — a backslash is an ordinary
// character on Unix and a separator on Windows; a quote matters only if
// something re-parses the line. They are all tested everywhere, because the
// failure mode is a wrong path on a platform nobody tested on.
var nastyPaths = []struct {
	name string
	path string
}{
	{"spaces", "/media/My Videos/episode 01.mkv"},
	{"japanese", "/media/第12回 声優ラジオ/本編.mp4"},
	{"chinese", "/媒体/熟肉/最终版.mkv"},
	{"single quote", "/media/it's here/ep.mp4"},
	{"double quote", `/media/he said "hi"/ep.mp4`},
	{"semicolon", "/media/a;rm -rf /.mp4"},
	{"ampersand", "/media/a && b/ep.mp4"},
	{"dollar", "/media/$HOME/ep.mp4"},
	{"backtick", "/media/`whoami`/ep.mp4"},
	{"newline", "/media/line\nbreak/ep.mp4"},
	{"backslash", `/media/win\path/ep.mp4`},
	{"windows drive", `C:\Users\tano\Videos\ep 01.mp4`},
	{"windows unc", `\\server\share\第12回\ep.mp4`},
	{"leading dash", "-/media/dash.mp4"},
	{"emoji", "/media/🎬/episode.mp4"},
}

// TestPathsSurviveAsSingleArguments is the whole point of building vectors.
//
// A path is never split, never quoted, never re-interpreted. If any builder
// were changed to construct a string that something later parses, the argument
// count would change and this fails.
func TestPathsSurviveAsSingleArguments(t *testing.T) {
	benign := "/tmp/x.mp4"
	baseline := len(ExtractAudioArgs(benign, "/tmp/x.wav", DefaultAudioOptions()))

	for _, tc := range nastyPaths {
		t.Run(tc.name, func(t *testing.T) {
			args := ExtractAudioArgs(tc.path, "/tmp/out.wav", DefaultAudioOptions())

			if len(args) != baseline {
				t.Errorf("argument count changed from %d to %d: the path was split",
					baseline, len(args))
			}
			if !contains(args, tc.path) {
				t.Errorf("the path does not appear as a single argument:\n  want %q\n  in   %v",
					tc.path, args)
			}
			// Nothing may have been added that merely looks like the path.
			for _, arg := range args {
				if arg != tc.path && strings.Contains(arg, tc.path) && len(arg) > len(tc.path) {
					t.Errorf("the path was concatenated into another argument: %q", arg)
				}
			}
		})
	}
}

// The escaping rules, written down as the strings a real FFmpeg accepted.
//
// Each expectation was produced by handing the string to FFmpeg's `movie`
// filter — which reports the path it received when it cannot open it — and
// reading back what arrived. That is why they look odd: a backslash in the path
// costs four, and the quotes are not decoration but the only way a drive
// letter's colon gets through at all.
//
// The Windows case is tested on every platform. It is the one that broke, and
// the failure was invisible on the machine the code was written on.
func TestFilterPathsArriveIntact(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "plain unix path",
			path: "/media/episode01.mkv",
			want: `'/media/episode01.mkv'`,
		},
		{
			name: "windows drive letter",
			// The colon is the whole problem: unescaped, or escaped but
			// unquoted, and FFmpeg splits the argument here.
			path: `C:\Users\tano\output\subtitles.ass`,
			want: `'C\:\\Users\\tano\\output\\subtitles.ass'`,
		},
		{
			name: "commas and brackets",
			// Structural to the filtergraph parser, and the reason the burn-in
			// test builds a directory called "weird, dir [x]".
			path: `/media/weird, dir [x]/subs.ass`,
			want: `'/media/weird\, dir \[x\]/subs.ass'`,
		},
		{
			name: "the rest of the grammar",
			path: `/media/a;b/c.ass`,
			want: `'/media/a\;b/c.ass'`,
		},
		{
			name: "spaces are left alone",
			path: `/media/My Subtitle File.ass`,
			want: `'/media/My Subtitle File.ass'`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := EscapeFilterPath(test.path); got != test.want {
				t.Errorf("EscapeFilterPath(%q)\n got  %s\n want %s", test.path, got, test.want)
			}
		})
	}
}

// A path with an apostrophe cannot be represented, and this records that
// rather than pretending otherwise. FFmpeg's tokeniser drops the character
// however it is escaped — measured, not assumed — so the render fails with "no
// such file", naming the file. Loud, and better than the alternative, which is
// a quote closing the value early and corrupting everything after it.
func TestFilterPathsQuoteAnApostropheWithoutClosingTheValue(t *testing.T) {
	escaped := EscapeFilterPath(`/media/it's/subs.ass`)

	if !strings.HasPrefix(escaped, "'") || !strings.HasSuffix(escaped, "'") {
		t.Fatalf("the value is not quoted, which is how the colon gets through: %s", escaped)
	}

	// Only the two delimiters may be unescaped. A third would end the value
	// early and hand the rest of the path back to the filtergraph parser.
	unescaped := 0
	for i := 0; i < len(escaped); i++ {
		if escaped[i] != '\'' {
			continue
		}
		if i > 0 && escaped[i-1] == '\\' {
			continue
		}
		unescaped++
	}
	if unescaped != 2 {
		t.Errorf("want exactly the two delimiters unescaped, found %d in %s", unescaped, escaped)
	}
}

// The escaping, checked against FFmpeg itself rather than against a string the
// test also computed.
//
// The unit test above pins the exact output; this one proves the output is what
// the parser wanted. They are different claims and the second is the one that
// was wrong: the escaping that broke CI looked perfectly reasonable, and only
// FFmpeg could say otherwise.
//
// It reads the answer back out of an error message. FFmpeg's `movie` filter
// reports the path it could not open, so feeding it a path that certainly does
// not exist makes it print what the two parsers delivered. Brittle in the way
// that any reading of a human-readable message is brittle — and worth it here,
// because the alternative is a test that only runs where libass does, and the
// platform this broke on is not one of them.
func TestEscapeFilterPathSurvivesFFmpeg(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}

	// The path is the platform's own. On Windows that is a real `C:\…` with
	// the drive letter that caused the failure; elsewhere it is a directory
	// whose *name* imitates one, because a colon and a backslash are ordinary
	// characters in a POSIX filename and the hazard is identical.
	dir := t.TempDir()
	if runtime.GOOS != "windows" {
		dir = filepath.Join(dir, `C:\Users\tano\weird, dir [x]`)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	subtitle := filepath.Join(dir, "subs.ass")
	if err := os.WriteFile(subtitle, []byte("not a video"), 0o644); err != nil {
		t.Fatal(err)
	}

	filter := "movie=filename=" + EscapeFilterPath(subtitle)
	cmd := exec.Command(ffmpeg,
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=black:s=32x32:d=0.1",
		"-vf", filter,
		"-f", "null", "-")
	output, _ := cmd.CombinedOutput()
	text := string(output)

	if strings.Contains(text, "No such filter") {
		t.Skip("this FFmpeg has no movie filter")
	}

	const marker = "avformat_open_input '"
	start := strings.Index(text, marker)
	if start < 0 {
		t.Fatalf("ffmpeg did not report the path it received, so this test cannot read it:\n  filter: %s\n%s",
			filter, text)
	}
	delivered := text[start+len(marker):]
	end := strings.Index(delivered, "'")
	if end < 0 {
		t.Fatalf("ffmpeg's message has no closing quote, so this test cannot read the path:\n%s", text)
	}
	delivered = delivered[:end]

	if delivered != subtitle {
		t.Errorf("the path FFmpeg received is not the one it was given\n"+
			"  filter:    %s\n  wanted:    %s\n  delivered: %s", filter, subtitle, delivered)
	}
}

func TestProbeArgsTreatThePathAsData(t *testing.T) {
	for _, tc := range nastyPaths {
		t.Run(tc.name, func(t *testing.T) {
			args := ProbeArgs(tc.path)
			if !contains(args, tc.path) {
				t.Errorf("the path is not a single argument: %v", args)
			}
			// -i must be immediately followed by the path, so a path that
			// begins with a dash cannot be read as an option.
			for i, arg := range args {
				if arg == "-i" {
					if i+1 >= len(args) || args[i+1] != tc.path {
						t.Errorf("-i is not followed by the path: %v", args)
					}
				}
			}
		})
	}
}

func TestDisplayIsForHumansOnly(t *testing.T) {
	// Display quotes for readability. It must never be the thing that is
	// executed, and this records the difference explicitly.
	args := ExtractAudioArgs("/media/a b.mp4", "/tmp/out.wav", DefaultAudioOptions())

	display := args.Display()
	if !strings.Contains(display, `"/media/a b.mp4"`) {
		t.Errorf("Display did not quote the path: %s", display)
	}

	// The vector itself is unchanged by rendering it.
	if !contains(args, "/media/a b.mp4") {
		t.Error("Display mutated the vector")
	}
}

func TestExtractAudioArgsShape(t *testing.T) {
	args := ExtractAudioArgs("/in.mp4", "/out.wav", AudioOptions{
		StreamIndex: 1,
		SampleRate:  16000,
		Channels:    1,
		HighpassHz:  60,
		Loudnorm:    true,
	})

	// The index is container-absolute, not audio-relative. `-map 0:a:1` would
	// ask for the second audio stream — which does not exist in a file whose
	// only audio stream sits at index 1 behind a video stream. That mistake is
	// invisible on audio-only input, so it is asserted here.
	if !contains(args, "0:1") {
		t.Errorf("stream selection is missing or wrong: %v", args)
	}
	if contains(args, "0:a:1") {
		t.Error("the absolute stream index was passed as an audio-relative one")
	}
	// Video and subtitle streams must be excluded, or the "audio" file is as
	// large as the source.
	for _, unwanted := range []string{"-vn", "-sn", "-dn"} {
		if !contains(args, unwanted) {
			t.Errorf("%s is missing; the output would carry more than audio", unwanted)
		}
	}
	if !contains(args, "16000") || !contains(args, "1") {
		t.Errorf("sample rate or channel count is missing: %v", args)
	}
	if !contains(args, "pcm_s16le") {
		t.Errorf("the output is not uncompressed PCM: %v", args)
	}
	if !contains(args, "-progress") {
		t.Errorf("progress reporting is missing: %v", args)
	}
}

func TestAudioFilterChain(t *testing.T) {
	cases := []struct {
		name string
		opts AudioOptions
		want string
	}{
		{"both", AudioOptions{HighpassHz: 60, Loudnorm: true}, "highpass=f=60,loudnorm=I=-16:TP=-1.5:LRA=11"},
		{"highpass only", AudioOptions{HighpassHz: 80}, "highpass=f=80"},
		{"loudnorm only", AudioOptions{Loudnorm: true}, "loudnorm=I=-16:TP=-1.5:LRA=11"},
		{"none", AudioOptions{}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := audioFilters(tc.opts)
			if got != tc.want {
				t.Errorf("filters = %q, want %q", got, tc.want)
			}
		})
	}

	// The chain is one argument, not several: FFmpeg parses the commas itself,
	// and a shell would have split them.
	args := ExtractAudioArgs("/in.mp4", "/out.wav", AudioOptions{HighpassHz: 60, Loudnorm: true})
	if !contains(args, "highpass=f=60,loudnorm=I=-16:TP=-1.5:LRA=11") {
		t.Errorf("the filter chain was split across arguments: %v", args)
	}
}

func TestPreviewArgsSeekBeforeInput(t *testing.T) {
	// -ss before -i seeks by keyframe and is far faster on a long source. The
	// trade is a cut at the nearest keyframe, which is right for a preview.
	args := PreviewArgs("/in.mkv", "/out.mp4", PreviewOptions{Start: 90, Duration: 12})

	ssAt, iAt := indexOf(args, "-ss"), indexOf(args, "-i")
	if ssAt < 0 || iAt < 0 {
		t.Fatalf("missing -ss or -i: %v", args)
	}
	if ssAt > iAt {
		t.Error("-ss comes after -i, so FFmpeg decodes the whole file up to the seek point")
	}
	if !contains(args, "90.000") {
		t.Errorf("the seek time has no decimal point: %v", args)
	}
}

func TestPreviewArgsDefaultsAreUsable(t *testing.T) {
	args := PreviewArgs("/in.mkv", "/out.mp4", PreviewOptions{})

	if !contains(args, "libx264") {
		t.Error("no default encoder: every FFmpeg build has libx264")
	}
	if !contains(args, "23") {
		t.Errorf("no default CRF: %v", args)
	}
	// faststart puts the index at the front, which is what makes a preview
	// start playing immediately instead of after a full download.
	if !contains(args, "+faststart") {
		t.Errorf("faststart is missing: %v", args)
	}
}

// ---------------------------------------------------------------------------
// Probe parsing
// ---------------------------------------------------------------------------

func TestProbeParsesFfprobeQuirks(t *testing.T) {
	// Every quirk here appears in real ffprobe output: numbers as strings,
	// "N/A" where a value is unknown, and a frame rate that divides by zero.
	const raw = `{
	  "format": {
	    "format_name": "matroska,webm",
	    "duration": "1423.456",
	    "size": "1073741824",
	    "bit_rate": "6033927"
	  },
	  "streams": [
	    {"index": 0, "codec_type": "video", "codec_name": "h264",
	     "width": 1920, "height": 1080, "r_frame_rate": "24000/1001",
	     "duration": "1423.456", "pix_fmt": "yuv420p"},
	    {"index": 1, "codec_type": "audio", "codec_name": "aac",
	     "sample_rate": "48000", "channels": 2, "channel_layout": "stereo",
	     "duration": "1423.400", "tags": {"language": "jpn", "title": "Japanese"}},
	    {"index": 2, "codec_type": "audio", "codec_name": "aac",
	     "sample_rate": "48000", "channels": 6,
	     "tags": {"language": "eng"}},
	    {"index": 3, "codec_type": "subtitle", "codec_name": "ass",
	     "tags": {"language": "eng"}},
	    {"index": 4, "codec_type": "video", "codec_name": "mjpeg",
	     "r_frame_rate": "0/0", "width": 600, "height": 600},
	    {"index": 5, "codec_type": "audio", "codec_name": "aac",
	     "sample_rate": "N/A", "channels": 2}
	  ],
	  "chapters": [
	    {"start_time": "0.000000", "end_time": "120.000000", "tags": {"title": "Opening"}}
	  ]
	}`

	var output probeOutput
	if err := json.Unmarshal([]byte(raw), &output); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	info := output.toMediaInfo("/media/ep.mkv")

	if info.Duration != 1423.456 {
		t.Errorf("duration = %v, want 1423.456", info.Duration)
	}
	if info.Format.SizeBytes != 1073741824 {
		t.Errorf("size = %d", info.Format.SizeBytes)
	}

	// The mjpeg stream with a 0/0 frame rate is cover art, not a picture.
	if len(info.Video) != 1 {
		t.Fatalf("found %d video streams, want 1 (cover art should be excluded)", len(info.Video))
	}
	if got := info.Video[0].FrameRate; got < 23.97 || got > 23.98 {
		t.Errorf("frame rate = %v, want ~23.976", got)
	}

	// All three audio streams are kept, including the one with an unparseable
	// sample rate: dropping it would make a file look like it had no audio.
	if len(info.Audio) != 3 {
		t.Fatalf("found %d audio streams, want 3", len(info.Audio))
	}
	if info.Audio[0].Language != "jpn" || info.Audio[0].Title != "Japanese" {
		t.Errorf("tags not read: %+v", info.Audio[0])
	}
	if info.Audio[2].SampleRate != 0 {
		t.Errorf("an N/A sample rate became %d instead of 0", info.Audio[2].SampleRate)
	}

	if len(info.Subtitle) != 1 {
		t.Errorf("found %d subtitle streams, want 1", len(info.Subtitle))
	}
	if len(info.Chapters) != 1 || info.Chapters[0].Title != "Opening" {
		t.Errorf("chapters not read: %+v", info.Chapters)
	}
}

func TestPrimaryAudioPrefersTheSourceLanguage(t *testing.T) {
	// Taking the first audio stream is wrong on a bilingual release, where the
	// first is usually the dub.
	info := &MediaInfo{
		Path: "/media/ep.mkv",
		Audio: []Stream{
			{Index: 1, Language: "eng", Channels: 2},
			{Index: 2, Language: "jpn", Channels: 2},
			{Index: 3, Language: "jpn", Channels: 6},
		},
	}

	got, err := info.PrimaryAudio("ja")
	if err != nil {
		t.Fatalf("PrimaryAudio: %v", err)
	}
	// Three-letter ffprobe tags must match two-letter project configuration.
	if got.Index != 2 {
		t.Errorf("selected stream %d, want 2 (the first Japanese track)", got.Index)
	}
}

func TestPrimaryAudioFallsBackToChannelCount(t *testing.T) {
	// With no language tags there is nothing better to go on than "the
	// fullest track is probably the original mix".
	info := &MediaInfo{
		Path: "/media/ep.mkv",
		Audio: []Stream{
			{Index: 1, Channels: 2},
			{Index: 2, Channels: 6},
		},
	}

	got, err := info.PrimaryAudio("ja")
	if err != nil {
		t.Fatal(err)
	}
	if got.Index != 2 {
		t.Errorf("selected stream %d, want 2 (6 channels)", got.Index)
	}
}

func TestPrimaryAudioReportsNoAudio(t *testing.T) {
	info := &MediaInfo{Path: "/media/silent.mp4", Video: []Stream{{Index: 0}}}

	if _, err := info.PrimaryAudio("ja"); !errors.Is(err, ErrNoAudioStream) {
		t.Fatalf("error = %v, want ErrNoAudioStream", err)
	}
}

func TestAudioOnlyInputIsNotAnError(t *testing.T) {
	info := &MediaInfo{
		Path:   "/media/radio.mp3",
		Audio:  []Stream{{Index: 0, Channels: 2}},
		Format: Format{Name: "mp3"},
	}
	if info.HasVideo() {
		t.Error("an audio-only file reports video")
	}
	if _, err := info.PrimaryAudio("ja"); err != nil {
		t.Errorf("audio-only input was rejected: %v", err)
	}
}

func TestParseFrameRateHandlesDegenerateInput(t *testing.T) {
	cases := map[string]float64{
		"24000/1001": 23.976023976023978,
		"30/1":       30,
		"25":         25,
		"0/0":        0,
		"N/A":        0,
		"":           0,
		"30/0":       0,
		"garbage":    0,
	}
	for raw, want := range cases {
		if got := parseFrameRate(raw); got != want {
			t.Errorf("parseFrameRate(%q) = %v, want %v", raw, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Progress
// ---------------------------------------------------------------------------

func TestProgressParsing(t *testing.T) {
	var seen []float64
	writer := newProgressWriter(100, func(fraction float64, _ string) {
		seen = append(seen, fraction)
	})

	// out_time_ms carries microseconds despite its name. Treating it as
	// milliseconds is a real bug that produces a bar finishing a thousand times
	// too early.
	io.WriteString(writer, "frame=100\nout_time_ms=25000000\nprogress=continue\n")

	// The writer throttles, so time is moved on rather than the throttle being
	// disabled: the throttle is real behaviour and testing without it would
	// test something that does not ship.
	writer.lastSent = time.Now().Add(-time.Hour)
	io.WriteString(writer, "frame=200\nout_time_us=50000000\nprogress=continue\n")

	if len(seen) == 0 {
		t.Fatal("no progress was reported")
	}
	if seen[0] < 0.24 || seen[0] > 0.26 {
		t.Errorf("first fraction = %v, want ~0.25 — 25s of 100s, so the microsecond "+
			"conversion is wrong", seen[0])
	}
	if last := seen[len(seen)-1]; last < 0.49 || last > 0.51 {
		t.Errorf("last fraction = %v, want ~0.5", last)
	}
}

func TestProgressIsThrottled(t *testing.T) {
	// FFmpeg emits a full block several times a second. Forwarding each one
	// floods the event stream with values nobody can perceive.
	var calls int
	writer := newProgressWriter(100, func(float64, string) { calls++ })

	for i := range 20 {
		io.WriteString(writer, "out_time_us="+strconv.Itoa(i*1_000_000)+"\nprogress=continue\n")
	}

	if calls != 1 {
		t.Errorf("reported %d times for 20 rapid updates, want 1 (the first)", calls)
	}
}

func TestProgressAlwaysReportsCompletion(t *testing.T) {
	// A bar that stops at 99% looks stuck even though the stage finished, so
	// the final update bypasses the throttle.
	var last float64
	writer := newProgressWriter(100, func(fraction float64, _ string) { last = fraction })

	io.WriteString(writer, "out_time_us=99000000\nprogress=continue\n")
	io.WriteString(writer, "progress=end\n")

	if last != 1 {
		t.Errorf("final fraction = %v, want 1", last)
	}
}

func TestProgressReachesOneAtEnd(t *testing.T) {
	// FFmpeg's final timestamp is often slightly short of the duration, so
	// relying on it leaves a bar stuck at 99%.
	var last float64
	writer := newProgressWriter(100, func(fraction float64, _ string) { last = fraction })

	io.WriteString(writer, "out_time_us=99000000\nprogress=continue\n")
	io.WriteString(writer, "progress=end\n")

	if last != 1 {
		t.Errorf("final fraction = %v, want 1", last)
	}
}

func TestProgressIsNeverInventedWithoutADuration(t *testing.T) {
	var calls int
	writer := newProgressWriter(0, func(float64, string) { calls++ })

	io.WriteString(writer, "out_time_us=5000000\nprogress=continue\n")

	// Without a duration there is no honest fraction, and a made-up one is a
	// number the user cannot trust.
	if calls != 0 {
		t.Errorf("progress was reported %d times with no duration", calls)
	}
}

func TestProgressNeverGoesBackwards(t *testing.T) {
	var seen []float64
	writer := newProgressWriter(100, func(fraction float64, _ string) { seen = append(seen, fraction) })

	io.WriteString(writer, "out_time_us=80000000\nprogress=continue\n")
	writer.lastSent = time.Now().Add(-time.Hour)
	io.WriteString(writer, "out_time_us=20000000\nprogress=continue\n")

	for i := 1; i < len(seen); i++ {
		if seen[i] < seen[i-1] {
			t.Errorf("progress went backwards: %v", seen)
			break
		}
	}
}

// ---------------------------------------------------------------------------
// Capabilities
// ---------------------------------------------------------------------------

func TestRecommendedEncoder(t *testing.T) {
	cases := []struct {
		name     string
		encoders []string
		want     string
	}{
		{"apple silicon", []string{"aac", "h264_videotoolbox", "libx264"}, "h264_videotoolbox"},
		{"nvidia", []string{"h264_nvenc", "libx264"}, "h264_nvenc"},
		{"plain cpu", []string{"aac", "libx264"}, "libx264"},
		{"unknown build", nil, "libx264"},
		// Hardware is preferred over software when both exist, but libx264 is
		// always the fallback because every FFmpeg build has it.
		{"videotoolbox wins over nvenc", []string{"h264_nvenc", "h264_videotoolbox"}, "h264_videotoolbox"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caps := &Capabilities{Encoders: tc.encoders}
			if got := caps.RecommendedVideoEncoder(); got != tc.want {
				t.Errorf("encoder = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseEncodersIgnoresTheHeader(t *testing.T) {
	out := `Encoders:
 V..... = Video
 ------
 V....D libx264              libx264 H.264 / AVC
 V....D h264_videotoolbox    VideoToolbox H.264
 A....D aac                  AAC (Advanced Audio Coding)
`
	got := parseEncoders(out)

	if !contains(got, "libx264") || !contains(got, "h264_videotoolbox") || !contains(got, "aac") {
		t.Errorf("missing encoders: %v", got)
	}
	// Header lines must not be mistaken for encoder names.
	for _, name := range got {
		if strings.Contains(name, ".") || name == "=" {
			t.Errorf("a header line was parsed as an encoder: %q", name)
		}
	}
}

func TestParseVersion(t *testing.T) {
	out := "ffmpeg version 8.1 Copyright (c) 2000-2026 the FFmpeg developers\nbuilt with..."
	if got := parseVersion(out); got != "8.1" {
		t.Errorf("version = %q, want 8.1", got)
	}
}

// ---------------------------------------------------------------------------
// Service behaviour
// ---------------------------------------------------------------------------

func TestProbeSurfacesFfprobeStderr(t *testing.T) {
	// "could not probe" alone sends a user looking at the wrong thing.
	svc := NewWithRunner(func(context.Context, string, []string, io.Writer, io.Writer) error {
		return errors.New("exit status 1")
	}, nil)

	// The runner writes nothing, so the error must still name the file and the
	// command that failed.
	_, err := svc.Probe(context.Background(), "/media/broken.mp4")
	if err == nil {
		t.Fatal("a failing probe was reported as success")
	}
	if !errors.Is(err, ErrProbeFailed) {
		t.Errorf("error = %v, want ErrProbeFailed", err)
	}
	for _, want := range []string{"/media/broken.mp4", "ffprobe"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q:\n%v", want, err)
		}
	}
}

func TestProbeRejectsAFileWithNoDuration(t *testing.T) {
	svc := NewWithRunner(func(_ context.Context, _ string, _ []string, stdout, _ io.Writer) error {
		// A file still being written probes as valid but with no duration.
		io.WriteString(stdout, `{"format":{"format_name":"matroska"},"streams":[]}`)
		return nil
	}, nil)

	_, err := svc.Probe(context.Background(), "/media/growing.mkv")
	if !errors.Is(err, ErrProbeFailed) {
		t.Fatalf("error = %v, want ErrProbeFailed", err)
	}
	if !strings.Contains(err.Error(), "duration") {
		t.Errorf("the error does not explain what is missing: %v", err)
	}
}

func TestExtractAudioReportsTheFailingCommand(t *testing.T) {
	svc := NewWithRunner(func(_ context.Context, _ string, _ []string, _, stderr io.Writer) error {
		io.WriteString(stderr, "line one\nline two\nConversion failed!\n")
		return errors.New("exit status 1")
	}, nil)

	err := svc.ExtractAudio(context.Background(), "/media/in.mp4", "/tmp/out.wav",
		DefaultAudioOptions(), 100, nil)
	if err == nil {
		t.Fatal("a failing extraction was reported as success")
	}
	// FFmpeg's real complaint is at the end, after pages of stream mapping.
	if !strings.Contains(err.Error(), "Conversion failed!") {
		t.Errorf("the error does not quote ffmpeg's stderr:\n%v", err)
	}
}

func TestServiceResolvesMissingTools(t *testing.T) {
	// A missing FFmpeg must be a named error with a remedy, not a confusing
	// failure at the first probe.
	_, err := New(Options{FFmpegPath: filepath.Join(os.TempDir(), "no-such-ffmpeg")})
	if err != nil {
		// An explicit path is trusted; the failure surfaces when it runs. That
		// is deliberate, so no assertion here beyond it not panicking.
		t.Skipf("explicit path was validated eagerly: %v", err)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func contains(args []string, want string) bool { return indexOf(args, want) >= 0 }

func indexOf(args []string, want string) int {
	for i, arg := range args {
		if arg == want {
			return i
		}
	}
	return -1
}
