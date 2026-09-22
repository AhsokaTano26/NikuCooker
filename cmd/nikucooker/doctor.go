package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/AhsokaTano26/NikuCooker/internal/app"
	"github.com/AhsokaTano26/NikuCooker/internal/media"
	"github.com/AhsokaTano26/NikuCooker/internal/platform"
)

// check is one line of the doctor report.
type check struct {
	Name string

	// Status is "ok", "warn" or "fail".
	Status string

	Detail string

	// Fix is what to do about it. An empty fix on a failing check is a bug in
	// this file: a diagnosis the user cannot act on is not a diagnosis.
	Fix string
}

func (c check) symbol() string {
	switch c.Status {
	case "ok":
		return "✓"
	case "warn":
		return "!"
	default:
		return "✗"
	}
}

func newDoctorCmd(g *globals) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that this machine can run the pipeline",
		Long: `Reports what NikuCooker needs and whether each piece is present.

Every problem it finds is one that would otherwise surface as a confusing failure
in the middle of a job — an FFmpeg that is not on PATH, a Python that cannot
import the AI package, a model that was never downloaded.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			checks := runChecks(cmd, g)

			out := cmd.OutOrStdout()
			if asJSON {
				encoder := json.NewEncoder(out)
				encoder.SetIndent("", "  ")
				return encoder.Encode(checks)
			}

			failed := 0
			for _, c := range checks {
				fmt.Fprintf(out, "%s %-22s %s\n", c.symbol(), c.Name, c.Detail)
				if c.Status != "ok" {
					if c.Fix != "" {
						fmt.Fprintf(out, "  %-22s → %s\n", "", c.Fix)
					}
					if c.Status == "fail" {
						failed++
					}
				}
			}

			fmt.Fprintln(out)
			if failed > 0 {
				fmt.Fprintf(out, "%d problem(s) must be fixed before a run will succeed.\n", failed)
				// A non-zero exit is what lets a script or an installer act on
				// this without parsing the output.
				return fmt.Errorf("%d check(s) failed", failed)
			}
			fmt.Fprintln(out, "Everything needed to run the pipeline is present.")
			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

func runChecks(cmd *cobra.Command, g *globals) []check {
	ctx := cmd.Context()

	var checks []check

	checks = append(checks, checkPlatform())
	application, err := g.openApp(cmd)
	if err != nil {
		// Without the application there is nothing to report about paths,
		// configuration or models, but the checks that do not need it are still
		// worth printing.
		checks = append(checks, check{
			Name: "configuration", Status: "fail",
			Detail: err.Error(),
			Fix:    "fix the configuration, or pass --config with a path that exists",
		})
		return checks
	}
	defer func() { _ = application.Close() }()

	checks = append(checks, checkConfig(application))
	checks = append(checks, checkDirectories(application))
	checks = append(checks, checkFFmpeg(ctx, application))
	checks = append(checks, checkPython(ctx, application))
	checks = append(checks, checkModels(ctx, application))
	checks = append(checks, checkProvider(ctx, application))

	return checks
}

func checkPlatform() check {
	return check{
		Name:   "platform",
		Status: "ok",
		Detail: fmt.Sprintf("%s/%s, %s", runtime.GOOS, runtime.GOARCH, runtime.Version()),
	}
}

func checkConfig(app interface{ DataDir() string }) check {
	return check{
		Name:   "configuration",
		Status: "ok",
		// The data directory is printed because every other path is derived
		// from it, and "which one is it actually using" is the first question
		// when a project seems to have vanished.
		Detail: fmt.Sprintf("valid, data in %s", app.DataDir()),
	}
}

func checkDirectories(app interface{ DataDir() string }) check {
	dir := app.DataDir()
	info, err := os.Stat(dir)
	if err != nil {
		return check{
			Name: "data directory", Status: "fail",
			Detail: dir,
			Fix:    "check that the path is writable, or set storage.data_dir in the configuration",
		}
	}
	if !info.IsDir() {
		return check{
			Name: "data directory", Status: "fail",
			Detail: fmt.Sprintf("%s is not a directory", dir),
			Fix:    "point storage.data_dir at a directory",
		}
	}

	// Written to rather than merely inspected: a directory can exist and still
	// be read-only, and that failure would appear at the first artifact write.
	probe := filepath.Join(dir, ".doctor-probe")
	if err := os.WriteFile(probe, []byte("probe"), 0o600); err != nil {
		return check{
			Name: "data directory", Status: "fail",
			Detail: fmt.Sprintf("%s is not writable", dir),
			Fix:    "fix the permissions, or set storage.data_dir to a writable path",
		}
	}
	_ = os.Remove(probe)

	return check{Name: "data directory", Status: "ok", Detail: dir}
}

func checkFFmpeg(ctx context.Context, app *app.App) check {
	service, err := media.New(media.Options{
		FFmpegPath:  app.Config().Media.FFmpegPath,
		FFprobePath: app.Config().Media.FFprobePath,
		Log:         app.Logger(),
	})
	if err != nil {
		return check{
			Name: "ffmpeg", Status: "fail",
			Detail: err.Error(),
			Fix:    "install FFmpeg, or set media.ffmpeg_path and media.ffprobe_path",
		}
	}

	caps, err := service.Capabilities(ctx)
	if err != nil {
		return check{
			Name: "ffmpeg", Status: "fail",
			Detail: err.Error(),
			Fix:    "check that the FFmpeg binary runs: " + service.FFmpegPath() + " -version",
		}
	}

	// Which encoder will actually be used is worth reporting, because it is the
	// difference between a render taking minutes and taking an hour.
	encoder := caps.RecommendedVideoEncoder()
	status := "ok"
	detail := fmt.Sprintf("%s, encoder %s", caps.Version, encoder)
	if encoder == "libx264" {
		status = "warn"
		detail = fmt.Sprintf("%s, encoder libx264 (software, no hardware encoder found)", caps.Version)
	}

	// Whether hard subtitles are possible at all. A build without libass is
	// perfectly usable for everything except burning, and finding that out here
	// is much better than finding it out after a render.
	if !caps.CanBurnSubtitles() {
		return check{
			Name: "ffmpeg", Status: "warn",
			Detail: detail + "; no libass, so hard subtitles are unavailable",
			Fix:    "soft subtitles still work; for burned-in subtitles install an FFmpeg built with --enable-libass",
		}
	}

	return check{Name: "ffmpeg", Status: status, Detail: detail + ", can burn subtitles"}
}

func checkPython(ctx context.Context, app *app.App) check {
	dir, err := app.AIDir()
	if err != nil {
		return check{
			Name: "python worker", Status: "fail",
			Detail: err.Error(),
			Fix:    "set ai.dir to the directory containing the nikucooker_ai package",
		}
	}

	python, err := platform.ResolvePython(ctx, platform.ResolveOptions{
		Explicit:      app.Config().AI.Python,
		AIDir:         dir,
		RequireImport: true,
	})
	if err != nil {
		return check{
			Name: "python worker", Status: "fail",
			Detail: err.Error(),
			Fix: "run `make ai-install` in a checkout, or set ai.python to an interpreter " +
				"that has the AI dependencies installed",
		}
	}

	version, err := platform.Version(ctx, python)
	if err != nil {
		return check{
			Name: "python worker", Status: "fail",
			Detail: err.Error(),
			Fix:    "check that " + python + " runs",
		}
	}

	constraint, err := platform.ParseConstraint(app.Config().AI.PythonVersion)
	if err != nil {
		return check{
			Name: "python worker", Status: "fail",
			Detail: err.Error(),
			Fix:    "fix ai.python_version in the configuration",
		}
	}

	if !constraint.IsEmpty() && !constraint.Allows(version) {
		return check{
			Name: "python worker", Status: "fail",
			Detail: fmt.Sprintf("%s reports Python %s, but %s is required",
				python, version, app.Config().AI.PythonVersion),
			Fix: "install a supported Python and recreate the AI environment, or widen ai.python_version",
		}
	}

	return check{
		Name:   "python worker",
		Status: "ok",
		Detail: fmt.Sprintf("Python %s at %s, package importable", version, python),
	}
}

func checkModels(ctx context.Context, app *app.App) check {
	installed, err := app.Models.Cached(ctx)
	if err != nil {
		return check{
			Name: "models", Status: "warn",
			Detail: err.Error(),
			Fix:    "this is a database problem rather than a missing model",
		}
	}

	wanted := app.Config().ASR.Model
	if wanted == "" {
		return check{
			Name: "models", Status: "warn",
			Detail: "no recognition model is configured",
			Fix:    "set asr.model, for example large-v3 or medium",
		}
	}

	for _, model := range installed {
		if model.Name == wanted || model.ID == "asr:"+wanted {
			return check{
				Name:   "models",
				Status: "ok",
				Detail: fmt.Sprintf("%s is installed", wanted),
			}
		}
	}

	return check{
		Name: "models", Status: "warn",
		// A warning rather than a failure: the model is downloaded on first use,
		// so this is a note about what the first run will cost, not a problem.
		Detail: fmt.Sprintf("%s is not downloaded yet", wanted),
		Fix:    "it will be fetched on the first run; `nikucooker models pull " + wanted + "` fetches it now",
	}
}

func checkProvider(ctx context.Context, app *app.App) check {
	providers, err := app.Providers.List(ctx, "")
	if err != nil {
		return check{
			Name: "language model", Status: "fail",
			Detail: err.Error(),
		}
	}

	var enabled []string
	for _, p := range providers {
		if p.Enabled && p.Kind == "llm" {
			enabled = append(enabled, fmt.Sprintf("%s (%s)", p.Name, p.Model))
		}
	}

	switch {
	case len(enabled) == 1:
		return check{Name: "language model", Status: "ok", Detail: enabled[0]}
	case len(enabled) > 1:
		return check{
			Name:   "language model",
			Status: "warn",
			Detail: fmt.Sprintf("%d enabled: %s", len(enabled), strings.Join(enabled, ", ")),
			Fix:    "set translation.provider to the one to use",
		}
	}

	if app.Config().Translation.Model != "" {
		return check{
			Name: "language model", Status: "ok",
			Detail: fmt.Sprintf("from configuration: %s", app.Config().Translation.Model),
		}
	}

	return check{
		Name: "language model", Status: "warn",
		Detail: "none is configured",
		// Only translation needs one, and only if it is not already cached, so
		// this is a warning rather than a failure.
		Fix: "translation will fail without one; set translation.model and translation.base_url, " +
			"or add a provider",
	}
}
