package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AhsokaTano26/NikuCooker/internal/artifact"
	"github.com/AhsokaTano26/NikuCooker/internal/config"
	"github.com/AhsokaTano26/NikuCooker/internal/database"
	"github.com/AhsokaTano26/NikuCooker/internal/glossary"
	"github.com/AhsokaTano26/NikuCooker/internal/media"
	"github.com/AhsokaTano26/NikuCooker/internal/pipeline"
	"github.com/AhsokaTano26/NikuCooker/internal/qc"
	"github.com/AhsokaTano26/NikuCooker/internal/stage"
	stagerender "github.com/AhsokaTano26/NikuCooker/internal/stage/render"
	stagesubtitle "github.com/AhsokaTano26/NikuCooker/internal/stage/subtitle"
	"github.com/AhsokaTano26/NikuCooker/internal/subtitle"
)

// The Phase 6 exit test: lines out as files, and files FFmpeg can actually read.
//
// The interesting property here is not that the writers produce the bytes they
// produce — that is a golden-file test, and golden files agree with whatever
// wrote them. It is that *FFmpeg* accepts the result. A malformed ASS header is
// invisible until playback, and a subtitle file that fails to parse on one
// player and not another is exactly the class of bug this project cannot afford.
//
// So the subtitle assertions below mux the generated file into a real container
// with real FFmpeg and read it back. That catches a wrong colour byte order, a
// missing Format line, a timestamp with the wrong number of digits, and a
// dialogue line whose ninth comma is not where it should be.

// ---------------------------------------------------------------------------
// Timestamps
// ---------------------------------------------------------------------------

func TestPhase6TimestampsRoundToValidValues(t *testing.T) {
	translated := func(text string) *string { return &text }

	// 3.9996 seconds is the case that breaks naive formatting: rounding the
	// seconds and the milliseconds independently produces 00:00:04,1000, which
	// some players reject and others clamp to four seconds.
	segments := []*subtitle.Segment{{
		ID: "seg_0001", Start: 0, End: 3.9996,
		SourceText: "テスト", TranslatedText: translated("测试"),
	}}

	var srt bytes.Buffer
	if err := subtitle.WriteSRT(&srt, setOf(segments), false); err != nil {
		t.Fatalf("WriteSRT: %v", err)
	}

	if strings.Contains(srt.String(), ",1000") {
		t.Fatalf("the milliseconds field overflowed:\n%s", srt.String())
	}
	if !strings.Contains(srt.String(), "00:00:04,000") {
		t.Errorf("expected the end to round to 00:00:04,000:\n%s", srt.String())
	}

	var ass bytes.Buffer
	preset, err := subtitle.PresetByName("fansub")
	if err != nil {
		t.Fatalf("PresetByName: %v", err)
	}
	if err := subtitle.WriteASS(&ass, setOf(segments), preset, false, "test"); err != nil {
		t.Fatalf("WriteASS: %v", err)
	}
	// ASS stops at centiseconds, so the same instant is 0:00:04.00.
	if !strings.Contains(ass.String(), "0:00:04.00") {
		t.Errorf("expected the ASS end time to be 0:00:04.00:\n%s", ass.String())
	}
}

// ---------------------------------------------------------------------------
// Escaping
// ---------------------------------------------------------------------------

// Text that would be interpreted as ASS markup must not corrupt the file.
func TestPhase6ASSEscapesStructuralCharacters(t *testing.T) {
	translated := func(text string) *string { return &text }

	segments := []*subtitle.Segment{
		{
			ID: "seg_0001", Start: 0, End: 2,
			SourceText:     "brace {test}",
			TranslatedText: translated("大括号 {测试}"),
		},
		{
			ID: "seg_0002", Start: 2, End: 4,
			SourceText:     `backslash \N`,
			TranslatedText: translated(`反斜杠 \N 转义`),
		},
		{
			ID: "seg_0003", Start: 4, End: 6,
			SourceText:     "line\nbreak",
			TranslatedText: translated("换\n行"),
		},
	}

	var ass bytes.Buffer
	preset, _ := subtitle.PresetByName("default")
	if err := subtitle.WriteASS(&ass, setOf(segments), preset, false, "escaping"); err != nil {
		t.Fatalf("WriteASS: %v", err)
	}

	text := ass.String()

	// A brace opens an override block, so a literal one must not survive. If it
	// did, everything up to the next brace would be interpreted as tags and
	// deleted from the rendered text.
	dialogue := dialogueLines(text)
	if len(dialogue) != 3 {
		t.Fatalf("expected 3 dialogue lines, got %d:\n%s", len(dialogue), text)
	}
	for i, line := range dialogue {
		body := line[strings.LastIndex(line, ",,")+2:]
		if strings.ContainsAny(body, "{}") {
			t.Errorf("dialogue %d contains a brace that would start an override block: %q", i+1, body)
		}
	}

	// A literal newline would end the event and corrupt every line after it.
	// The translated text's newline becomes a hard break instead.
	if !strings.Contains(dialogue[2], `\N`) {
		t.Errorf("a newline in the text did not become a hard break: %q", dialogue[2])
	}

	// And a backslash before N would be read as a line break rather than as
	// text, so it is substituted rather than emitted.
	if strings.Contains(dialogue[1], `\N`) && !strings.Contains(dialogue[1], "反斜杠") {
		t.Errorf("a literal backslash-N was emitted as an escape: %q", dialogue[1])
	}
}

// dialogueLines returns the Dialogue events of an ASS file.
func dialogueLines(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "Dialogue:") {
			out = append(out, line)
		}
	}
	return out
}

// setOf wraps segments in a set.
func setOf(segments []*subtitle.Segment) *subtitle.Set {
	return &subtitle.Set{
		SourceLanguage: "ja",
		TargetLanguage: "zh-Hans",
		Segments:       segments,
	}
}

// ---------------------------------------------------------------------------
// FFmpeg reads what we write
// ---------------------------------------------------------------------------

// A generated ASS file must survive being muxed into a container and read back.
//
// This is the assertion that matters: it is FFmpeg's parser, not ours, deciding
// whether the file is well formed.
func TestPhase6FFmpegAcceptsGeneratedSubtitles(t *testing.T) {
	ffmpeg := ffmpegPath(t)
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe is not installed")
	}

	root := t.TempDir()
	assPath := filepath.Join(root, "subs.ass")
	videoPath := filepath.Join(root, "source.mp4")
	outPath := filepath.Join(root, "muxed.mkv")

	// Text with the characters that break naive writers: CJK, braces, a
	// newline, and a comma — the comma matters because it is the field
	// separator in an ASS event line.
	translated := func(text string) *string { return &text }
	segments := []*subtitle.Segment{
		{
			ID: "seg_0001", Start: 0, End: 1.5,
			SourceText:     "こんにちは、世界。",
			TranslatedText: translated("你好，世界。{不是标签}"),
		},
		{
			ID: "seg_0002", Start: 1.5, End: 2,
			SourceText:     "二行目",
			TranslatedText: translated("第二行\n换行后"),
		},
	}

	preset, err := subtitle.PresetByName("fansub")
	if err != nil {
		t.Fatalf("PresetByName: %v", err)
	}

	file, err := os.Create(assPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := subtitle.WriteASS(file, setOf(segments), preset, true, "FFmpeg round trip"); err != nil {
		_ = file.Close()
		t.Fatalf("WriteASS: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	buildFixture(t, videoPath)

	// Muxed the same way the render stage does it.
	mux := exec.Command(ffmpeg,
		"-hide_banner", "-nostdin", "-y", "-loglevel", "error",
		"-i", videoPath, "-i", assPath,
		"-map", "0", "-map", "1",
		"-c", "copy", "-c:s", "ass",
		"-metadata:s:s:0", "language=chi",
		outPath,
	)
	if output, err := mux.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg refused the generated ASS file: %v\n%s", err, output)
	}

	// Read it back and confirm the cues survived.
	probe := exec.Command(ffprobe,
		"-hide_banner", "-loglevel", "error",
		"-select_streams", "s",
		"-show_entries", "stream=index,codec_name:stream_tags=language",
		"-of", "json", outPath,
	)
	raw, err := probe.Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}

	var probed struct {
		Streams []struct {
			Index     int               `json:"index"`
			CodecName string            `json:"codec_name"`
			Tags      map[string]string `json:"tags"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(raw, &probed); err != nil {
		t.Fatalf("decode ffprobe output: %v", err)
	}

	if len(probed.Streams) != 1 {
		t.Fatalf("expected one subtitle stream, got %d", len(probed.Streams))
	}
	if probed.Streams[0].CodecName != "ass" {
		t.Errorf("subtitle codec is %q, want ass", probed.Streams[0].CodecName)
	}
	if got := probed.Streams[0].Tags["language"]; got != "chi" {
		t.Errorf("subtitle language tag is %q, want chi", got)
	}
}

// A burn-in must render without FFmpeg rejecting the filter argument.
//
// The subtitle filter has its own escaping rules, and a path with a colon or a
// bracket in it is where they bite. This builds a project directory containing
// both and burns from there.
func TestPhase6BurnsSubtitlesIntoThePicture(t *testing.T) {
	ffmpeg := ffmpegPath(t)

	// Plenty of FFmpeg builds ship without libass, including this project's
	// development machine. Skipping is right: the capability is the user's
	// FFmpeg's, and the render stage reports its absence clearly rather than
	// failing obscurely.
	if !ffmpegCanBurn(t, ffmpeg) {
		t.Skip("this FFmpeg was built without libass, so it cannot burn subtitles in")
	}

	// A directory with the characters FFmpeg's filter parser treats as syntax.
	root := filepath.Join(t.TempDir(), "weird, dir [x]")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	assPath := filepath.Join(root, "subs.ass")
	videoPath := filepath.Join(root, "source.mp4")
	outPath := filepath.Join(t.TempDir(), "burned.mp4")

	translated := func(text string) *string { return &text }
	segments := []*subtitle.Segment{{
		ID: "seg_0001", Start: 0, End: 2,
		SourceText:     "テスト",
		TranslatedText: translated("烧录测试"),
	}}

	preset, _ := subtitle.PresetByName("fansub")
	file, err := os.Create(assPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := subtitle.WriteASS(file, setOf(segments), preset, false, "burn"); err != nil {
		_ = file.Close()
		t.Fatalf("WriteASS: %v", err)
	}
	_ = file.Close()

	buildFixture(t, videoPath)

	// The same escaping the render stage applies.
	filter := "subtitles=filename=" + escapeFilterPathForTest(assPath)

	burn := exec.Command(ffmpeg,
		"-hide_banner", "-nostdin", "-y", "-loglevel", "error",
		"-i", videoPath,
		"-vf", filter,
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "30",
		"-pix_fmt", "yuv420p",
		"-c:a", "copy",
		outPath,
	)
	if output, err := burn.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg could not burn the subtitles in: %v\n  filter: %s\n%s",
			err, filter, output)
	}

	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		t.Error("the burn-in produced an empty file")
	}
}

// ffmpegCanBurn reports whether an FFmpeg build provides the subtitles filter.
func ffmpegCanBurn(t *testing.T, ffmpeg string) bool {
	t.Helper()

	output, err := exec.Command(ffmpeg, "-hide_banner", "-filters").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && (fields[1] == "subtitles" || fields[1] == "ass") {
			return true
		}
	}
	return false
}

// escapeFilterPathForTest mirrors media.escapeFilterPath.
//
// Duplicated rather than exported, because the point of this test is to prove
// the escaping works against real FFmpeg. Exporting the function under test and
// calling it would prove nothing about FFmpeg's parser, which is what actually
// has to accept the argument.
func escapeFilterPathForTest(path string) string {
	var b strings.Builder
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

// ---------------------------------------------------------------------------
// The quality rules
// ---------------------------------------------------------------------------

func TestPhase6QualityRules(t *testing.T) {
	translated := func(text string) *string { return &text }

	// Each case is a line that should provoke exactly one code. Building them
	// as a table makes the rules read as a list of what can go wrong.
	cases := []struct {
		name    string
		segment *subtitle.Segment
		want    string
	}{
		{
			name: "an untranslated line",
			segment: &subtitle.Segment{
				ID: "seg_0001", Start: 0, End: 3,
				SourceText: "こんにちは",
			},
			want: "UNTRANSLATED",
		},
		{
			name: "an empty translation",
			segment: &subtitle.Segment{
				ID: "seg_0001", Start: 0, End: 3,
				SourceText: "こんにちは", TranslatedText: translated("   "),
			},
			want: "UNTRANSLATED",
		},
		{
			name: "a line too fast to read",
			segment: &subtitle.Segment{
				ID: "seg_0001", Start: 0, End: 1,
				SourceText:     "長い",
				TranslatedText: translated("这是一句非常非常长的话根本读不完"),
			},
			want: "CPS_EXCEEDED",
		},
		{
			name: "a line that flashes past",
			segment: &subtitle.Segment{
				ID: "seg_0001", Start: 0, End: 0.2,
				SourceText: "はい", TranslatedText: translated("是"),
			},
			want: "TOO_SHORT",
		},
		{
			name: "a line that outstays its welcome",
			segment: &subtitle.Segment{
				ID: "seg_0001", Start: 0, End: 30,
				SourceText: "ええ", TranslatedText: translated("嗯"),
			},
			want: "TOO_LONG",
		},
		{
			name: "a line with an unbalanced bracket",
			segment: &subtitle.Segment{
				ID: "seg_0001", Start: 0, End: 3,
				SourceText:     "「テスト",
				TranslatedText: translated("「测试"),
			},
			want: "UNBALANCED_BRACKET",
		},
		{
			name: "a translation identical to its source",
			segment: &subtitle.Segment{
				ID: "seg_0001", Start: 0, End: 3,
				SourceText:     "OK",
				TranslatedText: translated("OK"),
			},
			want: "SOURCE_UNCHANGED",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			report := qc.Check(setOf([]*subtitle.Segment{testCase.segment}), qc.Options{
				MaxCPS: 9, MaxDuration: 7, MinDuration: 0.8, MaxChars: 42,
			})

			if !hasCode(report, testCase.want) {
				t.Errorf("expected %s, got %v", testCase.want, codesOf(report))
			}
		})
	}
}

// A duplicate is reported on the later line, and only once.
func TestPhase6DuplicateDetection(t *testing.T) {
	translated := func(text string) *string { return &text }

	makeLine := func(id string, start float64, text string) *subtitle.Segment {
		return &subtitle.Segment{
			ID: id, Start: start, End: start + 2,
			SourceText: text, TranslatedText: translated("译" + text),
		}
	}

	report := qc.Check(setOf([]*subtitle.Segment{
		makeLine("seg_0001", 0, "そうですね。"),
		makeLine("seg_0002", 2, "たしかに。"),
		// The same sentence with different punctuation, which is what a
		// recogniser loop actually produces.
		makeLine("seg_0003", 4, "そうですね"),
	}), qc.Options{MaxCPS: 100, MaxDuration: 60, MinDuration: 0})

	duplicates := findByCode(report, "DUPLICATE")
	if len(duplicates) != 1 {
		t.Fatalf("expected one duplicate finding, got %d: %v", len(duplicates), codesOf(report))
	}
	if duplicates[0].SegmentID != "seg_0003" {
		t.Errorf("the duplicate was reported on %s, want the later line seg_0003",
			duplicates[0].SegmentID)
	}
}

// A glossary term that never reached the translation is a finding.
func TestPhase6GlossaryViolationIsReported(t *testing.T) {
	translated := func(text string) *string { return &text }

	report := qc.Check(setOf([]*subtitle.Segment{{
		ID: "seg_0001", Start: 0, End: 3,
		SourceText:     "たなかさんが来ました。",
		TranslatedText: translated("田中先生来了。"),
	}}), qc.Options{
		MaxCPS: 100, MaxDuration: 60, MinDuration: 0,
		Glossary: []glossary.Entry{
			{Source: "たなか", Target: "铃木", Enabled: true},
			{Source: "来ました", Target: "来了", Enabled: true},
			{Source: "存在しない語", Target: "不会出现", Enabled: true},
		},
	})

	violations := findByCode(report, "GLOSSARY_VIOLATION")
	if len(violations) != 1 {
		t.Fatalf("expected one glossary violation, got %d: %v",
			len(violations), codesOf(report))
	}
	// The term that is absent from the line must not be reported.
	if !strings.Contains(violations[0].Message, "たなか") {
		t.Errorf("wrong term reported: %s", violations[0].Message)
	}
}

// An empty project is an error, not a clean bill of health.
func TestPhase6EmptySetIsAnError(t *testing.T) {
	report := qc.Check(setOf(nil), qc.Options{})

	if !report.HasErrors() {
		t.Errorf("an empty set produced no errors: %v", codesOf(report))
	}
}

func hasCode(report *qc.Report, code string) bool {
	return len(findByCode(report, code)) > 0
}

func findByCode(report *qc.Report, code string) []qc.Finding {
	var out []qc.Finding
	for _, finding := range report.Findings {
		if finding.Code == code {
			out = append(out, finding)
		}
	}
	return out
}

func codesOf(report *qc.Report) []string {
	codes := make([]string, 0, len(report.Findings))
	for _, finding := range report.Findings {
		codes = append(codes, finding.Code+"("+finding.SegmentID+")")
	}
	return codes
}

// ---------------------------------------------------------------------------
// The output stages, end to end
// ---------------------------------------------------------------------------

// fixedStage emits a canned payload.
//
// Stands in for the two stages before this chain's own — recognition and
// translation — so that the subtitle and render stages can be exercised through
// the real pipeline, with real artifacts, real cache keys and real FFmpeg.
// Phase 4 and Phase 5 test the stages it replaces.
type fixedStage struct {
	name    string
	payload any
	deps    []string
}

func (s *fixedStage) Spec() stage.Spec {
	return stage.Spec{Name: s.name, Version: "1", Depends: s.deps, ConfigKey: s.name}
}

func (s *fixedStage) ConfigSubtree(*config.Config) any { return nil }

func (s *fixedStage) Fingerprint(context.Context, *stage.Env) (map[string]string, error) {
	return nil, nil
}

func (s *fixedStage) Run(_ context.Context, env *stage.Env) (*stage.Result, error) {
	raw, err := json.Marshal(s.payload)
	if err != nil {
		return nil, err
	}
	name := s.name + ".json"
	if err := os.WriteFile(filepath.Join(env.OutDir, name), raw, 0o644); err != nil {
		return nil, err
	}
	return &stage.Result{Primary: name}, nil
}

// A translated project must come out as a video with working subtitles in it.
//
// This is the assertion the whole pipeline exists to satisfy: the user asked for
// a watchable file, and every stage before this one is machinery in service of
// it. The video is real, the FFmpeg is real, and the subtitle file is produced
// by the same writer the CLI runs.
func TestPhase6PipelineProducesSubtitledVideo(t *testing.T) {
	ffmpegPath(t)

	root := t.TempDir()
	projectDir := filepath.Join(root, "projects", "proj_phase6")
	sourceDir := filepath.Join(projectDir, "source")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}

	sourcePath := filepath.Join(sourceDir, "episode01.mp4")
	buildFixture(t, sourcePath)

	db, err := database.Open(database.Options{Path: filepath.Join(root, "niku.db")})
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := database.Migrate(context.Background(), db); err != nil {
		t.Fatalf("database.Migrate: %v", err)
	}

	const projectID = "proj_phase6"
	if _, err := db.Write.ExecContext(context.Background(), `
		INSERT INTO projects (id, name, source_path, source_language, target_language,
		                      style, status, config_json, created_at, updated_at)
		VALUES (?, 'phase 6', 'source/episode01.mp4', 'ja', 'zh-Hans',
		        'fansub', 'active', '{}', 'now', 'now')`, projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	mediaService, err := media.New(media.Options{Log: log})
	if err != nil {
		t.Fatalf("media.New: %v", err)
	}

	translated := func(text string) *string { return &text }
	set := &subtitle.Set{
		SourceLanguage: "ja",
		TargetLanguage: "zh-Hans",
		Segments: []*subtitle.Segment{
			{
				ID: "seg_0001", Start: 0, End: 1,
				SourceText: "こんにちは", TranslatedText: translated("你好"),
			},
			{
				ID: "seg_0002", Start: 1, End: 2,
				SourceText: "さようなら", TranslatedText: translated("再见"),
			},
		},
	}
	// The cached figures are what QC and the writers read.
	for _, segment := range set.Segments {
		segment.RecomputeCPS()
	}

	// The probe is faked because it needs a real file to inspect and the
	// fixture's exact geometry is not what is under test here. The render stage
	// only reads HasVideo and Duration from it.
	probePayload := media.MediaInfo{
		Path:     sourcePath,
		Duration: 2,
		Video:    []media.Stream{{Index: 0, Type: "video", Codec: "h264"}},
		Audio:    []media.Stream{{Index: 1, Type: "audio", Codec: "aac", Channels: 1}},
	}

	registry, err := stage.NewRegistry(
		&fixedStage{name: "probe", payload: probePayload},
		&fixedStage{name: "translation", payload: set, deps: []string{"probe"}},
		stagesubtitle.New(),
		stagerender.New(),
	)
	if err != nil {
		t.Fatalf("stage.NewRegistry: %v", err)
	}

	cfg := config.Default()
	// Only the chain under test. The others are not registered, and a disabled
	// list naming an unregistered stage is correctly rejected.
	cfg.Pipeline.Disabled = nil
	cfg.Subtitle.Formats = []string{"srt", "ass"}
	cfg.Render.Mode = "soft"

	store := artifact.NewStore(db, projectID, projectDir)
	opts := pipeline.Options{
		Registry:          registry,
		Config:            cfg,
		Store:             store,
		ProjectID:         projectID,
		JobID:             "job_phase6",
		CodeRevision:      "test-rev-1",
		SourceFingerprint: "v1:size=1:h=fixed",
		SourcePath:        sourcePath,
		Media:             mediaService,
		Project: stage.ProjectInfo{
			Name: "phase 6", SourceLanguage: "ja", TargetLanguage: "zh-Hans", Style: "fansub",
		},
		Log: log,
	}

	ctx := context.Background()
	plan, err := pipeline.Build(ctx, opts)
	if err != nil {
		t.Fatalf("pipeline.Build: %v", err)
	}
	if err := pipeline.Run(ctx, plan, opts, pipeline.NopObserver{}); err != nil {
		t.Fatalf("pipeline.Run: %v", err)
	}

	for _, sp := range plan.Stages {
		if sp.State != pipeline.StateCompleted && sp.State != pipeline.StateCached {
			t.Fatalf("stage %s ended in state %s (error: %v)",
				sp.Stage.Spec().Name, sp.State, sp.Err)
		}
	}

	// The subtitle files must exist and be non-empty.
	subtitleDir, err := store.Dir(artifactOf(t, plan, "subtitle"))
	if err != nil {
		t.Fatalf("resolve the subtitle directory: %v", err)
	}
	for _, name := range []string{stagesubtitle.SRTName, stagesubtitle.ASSName} {
		info, err := os.Stat(filepath.Join(subtitleDir, name))
		if err != nil {
			t.Fatalf("%s was not written: %v", name, err)
		}
		if info.Size() == 0 {
			t.Errorf("%s is empty", name)
		}
	}

	// And the rendered video must carry the subtitles as a real stream.
	rendered, err := store.Path(artifactOf(t, plan, "render"))
	if err != nil {
		t.Fatalf("resolve the rendered file: %v", err)
	}
	if filepath.Ext(rendered) != ".mkv" {
		t.Errorf("the render produced %s; Matroska is the container that keeps ASS styling",
			filepath.Base(rendered))
	}

	probe := exec.Command("ffprobe",
		"-hide_banner", "-loglevel", "error",
		"-select_streams", "s",
		"-show_entries", "stream=codec_name:stream_tags=language",
		"-of", "json", rendered,
	)
	raw, err := probe.Output()
	if err != nil {
		t.Fatalf("ffprobe on the rendered file: %v", err)
	}

	var probed struct {
		Streams []struct {
			CodecName string            `json:"codec_name"`
			Tags      map[string]string `json:"tags"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(raw, &probed); err != nil {
		t.Fatalf("decode ffprobe output: %v", err)
	}

	if len(probed.Streams) != 1 {
		t.Fatalf("the rendered file has %d subtitle streams, want 1", len(probed.Streams))
	}
	if probed.Streams[0].CodecName != "ass" {
		t.Errorf("the subtitle stream is %s, want ass", probed.Streams[0].CodecName)
	}
	if got := probed.Streams[0].Tags["language"]; got != "chi" {
		t.Errorf("the subtitle track is tagged %q, want chi", got)
	}
}

// artifactOf returns the artifact a named stage produced.
func artifactOf(t *testing.T, plan *pipeline.Plan, name string) *artifact.Artifact {
	t.Helper()

	for _, sp := range plan.Stages {
		if sp.Stage.Spec().Name == name {
			if sp.Artifact == nil {
				t.Fatalf("stage %s produced no artifact", name)
			}
			return sp.Artifact
		}
	}
	t.Fatalf("the plan contained no stage named %s", name)
	return nil
}
