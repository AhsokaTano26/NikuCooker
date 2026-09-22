package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/AhsokaTano26/NikuCooker/internal/project"
)

// deliverableExtensions are the file types a run exists to produce.
//
// An allowlist by extension rather than a list of stage names, because the
// question being asked is "what would a person take away from this?", and that
// is a question about the file rather than about which stage wrote it. It also
// means a stage that starts producing a subtitle file is published without
// anyone remembering to add it here.
//
// Everything else a stage writes stays in the cache where it belongs:
// probe.json, audio.wav, vad.json and the rest are working material. Publishing
// them would bury the two files that matter among ten that do not.
var deliverableExtensions = map[string]bool{
	".srt": true,
	".ass": true,
	".vtt": true,

	".mkv":  true,
	".mp4":  true,
	".webm": true,
}

// publishOutputs copies a run's results into the project's output directory.
//
// Called after every run, successful or not: a run that failed at the render
// stage still produced subtitles, and withholding them would mean a user
// cannot get at work that is finished and correct because a later step of the
// same run was not. Stages that failed have no artifact and are simply not
// considered.
func (a *App) publishOutputs(ctx context.Context, projectID string) {
	files := a.deliverables(ctx, projectID)
	if len(files) == 0 {
		return
	}

	if err := project.PublishOutputs(a.dataDir, projectID, files); err != nil {
		// Logged rather than returned: the run succeeded, and its results are
		// in the cache and readable through the API either way. Failing here
		// would report a successful run as a failed one over the placement of
		// its output.
		a.log.Error("could not publish the run's output files",
			"project_id", projectID, "error", err)
		return
	}

	a.log.Info("published output files", "project_id", projectID, "count", len(files))
}

// deliverables collects the project's finished files.
//
// Every stage's *current* artifact, rather than the ones this particular run
// happened to touch. The difference matters for a partial run: `--only render`
// recomputes one stage and finds the subtitles already sitting in the cache, so
// a list built from the run would contain a video and nothing else — and the
// output directory, which is replaced rather than added to, would lose the
// subtitle files that are still perfectly current.
func (a *App) deliverables(ctx context.Context, projectID string) []project.Published {
	projectDir := project.Dir(a.dataDir, projectID)
	store := a.Artifacts.For(projectID)

	// Taken by name as well as by file, so that two stages publishing the same
	// filename is a decision made here rather than an error raised later.
	taken := map[string]bool{}
	files := []project.Published{}

	for _, stage := range a.registry.Ordered() {
		name := stage.Spec().Name

		current, err := store.Latest(ctx, name)
		if err != nil {
			a.log.Warn("could not look up a stage's current artifact", "stage", name, "error", err)
			continue
		}
		if current == nil {
			continue
		}

		dir := filepath.Join(projectDir, filepath.FromSlash(current.Path))

		entries, err := os.ReadDir(dir)
		if err != nil {
			a.log.Warn("could not read a stage's artifact directory", "stage", name, "error", err)
			continue
		}

		for _, entry := range entries {
			if entry.IsDir() || !deliverableExtensions[strings.ToLower(filepath.Ext(entry.Name()))] {
				continue
			}

			published := entry.Name()
			if taken[published] {
				// Qualified by the stage that produced it, because dropping one
				// silently would be worse than a longer name.
				published = name + "-" + published
			}
			if taken[published] {
				continue
			}
			taken[published] = true

			files = append(files, project.Published{
				Name:   published,
				Source: filepath.Join(dir, entry.Name()),
			})
		}
	}

	return files
}

// OutputFiles lists a project's published results.
func (a *App) OutputFiles(projectID string) ([]project.OutputFile, error) {
	return project.ListOutputs(a.dataDir, projectID)
}

// OutputPath resolves one published result for reading.
func (a *App) OutputPath(projectID, name string) (string, error) {
	return project.ResolveOutput(a.dataDir, projectID, name)
}
