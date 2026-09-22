// Package subtitle writes the subtitle files a user actually plays.
//
// Everything before this stage has been about producing the right lines;
// this is where they become files. It writes every configured format rather
// than choosing one, because the choice is not the pipeline's to make: the same
// project is a soft-subtitled MKV for a media server, an SRT for a phone, and
// an ASS for a re-encode, and producing all three costs almost nothing.
package subtitle

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AhsokaTano26/NikuCooker/internal/config"
	"github.com/AhsokaTano26/NikuCooker/internal/stage"
	"github.com/AhsokaTano26/NikuCooker/internal/subtitle"
)

// Fixed payload names.
//
// Deliberately free of the project's name and language: this path is handed to
// FFmpeg's subtitles filter, whose argument parser treats colons, commas and
// brackets as syntax. A user's project name can contain all three, so the file
// the renderer reads is named something that cannot.
const (
	ASSName = "subs.ass"
	SRTName = "subs.srt"

	// ManifestName is the primary payload.
	//
	// Not "manifest.json": that name belongs to the artifact package, which
	// writes one into every artifact directory to describe it. A payload with
	// the same name would be silently replaced at publish time.
	ManifestName = "subtitles.json"
)

// Stage writes the subtitle files.
type Stage struct{}

// New builds the stage.
func New() *Stage { return &Stage{} }

// Spec describes the stage.
func (s *Stage) Spec() stage.Spec {
	return stage.Spec{
		Name:    "subtitle",
		Version: "1",
		Depends: []string{"translation"},
		// When the polish pass ran, its lines supersede the translation's. This
		// is the same relationship the translator has with the context
		// document: an improvement that is used when it exists.
		OptionalDepends: []string{"polish"},
		// Not optional: subtitles are the point of the whole pipeline, and
		// there is no configuration in which producing them is wrong.
		Optional:  false,
		ConfigKey: "subtitle",
	}
}

// ConfigSubtree returns the output settings.
func (s *Stage) ConfigSubtree(cfg *config.Config) any {
	return struct {
		Formats   []string `yaml:"formats"`
		Preset    string   `yaml:"preset"`
		Bilingual bool     `yaml:"bilingual"`
	}{cfg.Subtitle.Formats, cfg.Subtitle.Preset, cfg.Subtitle.Bilingual}
}

// Fingerprint returns nothing: the writer is a pure function of the lines and
// the configuration, and both are already in the key.
func (s *Stage) Fingerprint(context.Context, *stage.Env) (map[string]string, error) {
	return nil, nil
}

// Manifest records what was written.
//
// The renderer reads this rather than assuming a filename. A stage that guessed
// at another stage's output names would be an undeclared interface between
// them, and it would break silently the moment a format was added or renamed.
type Manifest struct {
	SourceLanguage string `json:"source_language"`
	TargetLanguage string `json:"target_language"`

	Bilingual bool   `json:"bilingual"`
	Preset    string `json:"preset"`

	// Polished records whether these lines came from the polish pass. It is
	// provenance the user can act on: a polished file that reads worse than the
	// unpolished one means the pass should be turned off for this project.
	Polished bool `json:"polished"`

	Files []File `json:"files"`

	// NeedsReview is how many lines QC or the translation stage flagged. It is
	// carried here so that whoever renders knows the subtitles have known
	// problems, without having to read the QC artifact.
	NeedsReview int `json:"needs_review"`

	// Untranslated is how many lines will be shown in their source language.
	Untranslated int `json:"untranslated"`
}

// File is one written subtitle file.
type File struct {
	// Format is "srt" or "ass".
	Format string `json:"format"`

	// Name is the file's name within the artifact directory.
	Name string `json:"name"`

	Bytes        int64 `json:"bytes"`
	SegmentCount int   `json:"segment_count"`
}

// FileFor returns the entry for a format.
func (m *Manifest) FileFor(format string) (File, bool) {
	for _, file := range m.Files {
		if file.Format == format {
			return file, true
		}
	}
	return File{}, false
}

// Run writes the files.
func (s *Stage) Run(ctx context.Context, env *stage.Env) (*stage.Result, error) {
	set, polished, err := lines(env)
	if err != nil {
		return nil, err
	}
	if len(set.Segments) == 0 {
		return nil, fmt.Errorf("subtitle: there are no lines to write")
	}

	cfg := env.Config.Subtitle
	formats := normaliseFormats(cfg.Formats)
	if len(formats) == 0 {
		return nil, fmt.Errorf(
			"subtitle: no output formats are configured; use srt, ass or both")
	}

	preset, err := subtitle.PresetByName(cfg.Preset)
	if err != nil {
		return nil, fmt.Errorf("subtitle: %w", err)
	}

	manifest := &Manifest{
		SourceLanguage: set.SourceLanguage,
		TargetLanguage: set.TargetLanguage,
		Bilingual:      cfg.Bilingual,
		Preset:         preset.Name,
		Polished:       polished,
		Files:          []File{},
		NeedsReview:    len(set.NeedsReview()),
	}
	manifest.Untranslated = len(set.Segments) - set.CountTranslated()

	title := env.Project.Name
	if title == "" {
		title = env.ProjectID
	}

	for _, format := range formats {
		file, err := writeFormat(env.OutDir, format, set, preset, cfg.Bilingual, title)
		if err != nil {
			return nil, err
		}
		manifest.Files = append(manifest.Files, file)
		env.Log.Info("wrote subtitles",
			"format", format, "file", file.Name, "bytes", file.Bytes)
	}

	if err := writeManifest(env.OutDir, manifest); err != nil {
		return nil, err
	}

	env.ReportProgress(1, "done")

	return &stage.Result{
		Primary: ManifestName,
		Metadata: map[string]any{
			"formats":         formats,
			"line_count":      len(set.Segments),
			"untranslated":    manifest.Untranslated,
			"needs_review":    manifest.NeedsReview,
			"bilingual":       cfg.Bilingual,
			"preset":          preset.Name,
			"polished":        polished,
			"source_language": set.SourceLanguage,
			"target_language": set.TargetLanguage,
		},
	}, nil
}

// lines returns the lines to write, and whether they were polished.
//
// The polished set is preferred when the polish stage ran. Both stages produce
// the full set with the same ids, so preferring one is a straight substitution
// rather than a merge — and a merge would be wrong, because a line the polish
// pass declined to change is a deliberate "this is already good", not a gap.
func lines(env *stage.Env) (*subtitle.Set, bool, error) {
	var polished subtitle.Set
	ok, err := env.ReadOptional("polish", &polished)
	if err != nil {
		return nil, false, fmt.Errorf("subtitle: %w", err)
	}
	if ok && len(polished.Segments) > 0 {
		return &polished, true, nil
	}

	var set subtitle.Set
	if err := env.ReadInput("translation", &set); err != nil {
		return nil, false, fmt.Errorf("subtitle: %w", err)
	}
	return &set, false, nil
}

// writeFormat writes one subtitle file.
func writeFormat(
	dir, format string,
	set *subtitle.Set,
	preset subtitle.Preset,
	bilingual bool,
	title string,
) (File, error) {
	name := SRTName
	if format == "ass" {
		name = ASSName
	}
	path := filepath.Join(dir, name)

	file, err := os.Create(path)
	if err != nil {
		return File{}, fmt.Errorf("subtitle: create %s: %w", name, err)
	}

	// Written through a writer that reports its own errors rather than through
	// a buffer: a two-hour film is a few hundred kilobytes, and holding it in
	// memory to then fail on the write wastes the work.
	var writeErr error
	switch format {
	case "srt":
		writeErr = subtitle.WriteSRT(file, set, bilingual)
	case "ass":
		writeErr = subtitle.WriteASS(file, set, preset, bilingual, title)
	}

	closeErr := file.Close()
	if writeErr != nil {
		return File{}, fmt.Errorf("subtitle: %w", writeErr)
	}
	if closeErr != nil {
		return File{}, fmt.Errorf("subtitle: write %s: %w", name, closeErr)
	}

	info, err := os.Stat(path)
	if err != nil {
		return File{}, fmt.Errorf("subtitle: stat %s: %w", name, err)
	}
	if info.Size() == 0 {
		// A zero-byte subtitle file is worse than none: a player that finds it
		// reports "no subtitles" as though the track were empty, and the user
		// has no way to tell that from a file that failed to write.
		return File{}, fmt.Errorf("subtitle: %s came out empty", name)
	}

	return File{
		Format:       format,
		Name:         name,
		Bytes:        info.Size(),
		SegmentCount: len(set.Segments),
	}, nil
}

// writeManifest writes the primary payload.
func writeManifest(dir string, manifest *Manifest) error {
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("subtitle: encode the manifest: %w", err)
	}

	final := filepath.Join(dir, ManifestName)
	if err := os.WriteFile(final+".tmp", encoded, 0o644); err != nil {
		return fmt.Errorf("subtitle: write the manifest: %w", err)
	}
	if err := os.Rename(final+".tmp", final); err != nil {
		return fmt.Errorf("subtitle: write the manifest: %w", err)
	}
	return nil
}

// normaliseFormats lowercases, deduplicates and validates the format list.
func normaliseFormats(formats []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(formats))

	for _, format := range formats {
		key := strings.ToLower(strings.TrimSpace(format))
		switch key {
		case "srt", "ass":
			if !seen[key] {
				seen[key] = true
				out = append(out, key)
			}
		case "":
			// An empty entry is a configuration artefact, not a request.
		default:
			// Ignored rather than rejected: a formats list naming one
			// unsupported format should still produce the ones it can, and the
			// stage's metadata records what was actually written.
		}
	}
	return out
}
