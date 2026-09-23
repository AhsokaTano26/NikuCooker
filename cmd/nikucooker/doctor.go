package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/AhsokaTano26/NikuCooker/internal/app"
	"github.com/AhsokaTano26/NikuCooker/internal/media"
	"github.com/AhsokaTano26/NikuCooker/internal/models"
	"github.com/AhsokaTano26/NikuCooker/internal/platform"
	"github.com/AhsokaTano26/NikuCooker/internal/provision"
	"github.com/AhsokaTano26/NikuCooker/internal/subtitle"
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
	checks = append(checks, checkSubtitleFont(ctx, application))
	checks = append(checks, checkPython(ctx, application))
	checks = append(checks, checkRuntime(application))
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
			Fix:    burnFix(caps.Path),
		}
	}

	return check{Name: "ffmpeg", Status: status, Detail: detail + ", can burn subtitles"}
}

// checkSubtitleFont reports a subtitle font this machine cannot resolve.
//
// Its own check because its failure is the quiet one. A missing libass is
// refused before any encoding starts, with a message naming the setting; a
// missing *font* is not — FFmpeg and libass both succeed, the file plays, and
// every subtitle is a row of empty boxes. The only way to find out is to watch
// the whole thing, by which point the box of boxes is the deliverable.
func checkSubtitleFont(ctx context.Context, app *app.App) check {
	preset, err := subtitle.PresetByName(app.Config().Subtitle.Preset)
	if err != nil {
		// The preset is validated at startup, so this is unreachable; reporting
		// it rather than ignoring it keeps the reason visible if that changes.
		return check{
			Name: "subtitle font", Status: "warn",
			Detail: err.Error(),
			Fix:    "check subtitle.preset",
		}
	}

	// fontconfig's lookup, which is the one libass uses. Machines without it —
	// macOS among them — cannot be asked, and on those there is nothing to
	// answer with.
	fcMatch, err := exec.LookPath("fc-match")
	if err != nil {
		return check{
			Name: "subtitle font", Status: "ok",
			Detail: fmt.Sprintf("%s (fontconfig is not installed, so the lookup cannot be checked)", preset.FontName),
		}
	}

	out, err := exec.CommandContext(ctx, fcMatch, "-f", "%{family}", preset.FontName).Output()
	if err != nil {
		return check{
			Name: "subtitle font", Status: "ok",
			Detail: fmt.Sprintf("%s (fontconfig did not answer)", preset.FontName),
		}
	}

	// fc-match answers with its best match rather than failing, so a different
	// family is how "that one is not installed" reads.
	resolved := strings.TrimSpace(string(out))
	if strings.EqualFold(resolved, preset.FontName) {
		return check{Name: "subtitle font", Status: "ok", Detail: preset.FontName + " (needed to burn subtitles in)"}
	}

	return check{
		Name: "subtitle font", Status: "warn",
		Detail: fmt.Sprintf("%s is not installed; fontconfig offers %q instead", preset.FontName, resolved),
		Fix: "burned-in subtitles will render as empty boxes. Soft subtitles are unaffected — " +
			"the font is the player's on those. Install a font matching the name, or set " +
			"subtitle.preset to a style naming one you have",
	}
}

// burnFix says how to get an FFmpeg that can burn subtitles, on the machine
// asking.
//
// "Install a build with --enable-libass" is the true answer everywhere and a
// usable one almost nowhere: it names a compile flag, not something to
// install. Worse on macOS, where the advice sounds like a reinstall and is
// not — Homebrew's formula has no libass dependency at all, so `brew install
// ffmpeg` produces exactly the binary the user already has.
func burnFix(ffmpegPath string) string {
	switch runtime.GOOS {
	case "darwin":
		if strings.Contains(ffmpegPath, "/Cellar/ffmpeg/") || strings.Contains(ffmpegPath, "/opt/homebrew/") {
			return "soft subtitles still work. Homebrew's ffmpeg is built without libass, " +
				"so reinstalling it changes nothing — either install the tap that has the option " +
				"(brew trust homebrew-ffmpeg/ffmpeg && brew tap homebrew-ffmpeg/ffmpeg && " +
				"brew install homebrew-ffmpeg/ffmpeg/ffmpeg --with-libass) and point media.ffmpeg_path at it, " +
				"or run the container, whose FFmpeg does have it"
		}
		return "soft subtitles still work; for burned-in subtitles install an FFmpeg built with --enable-libass"

	case "linux":
		return "soft subtitles still work. For burned-in subtitles, install a build with libass — " +
			"Debian and Ubuntu's ffmpeg packages have it, and the container image uses one of those"

	default:
		return "soft subtitles still work; for burned-in subtitles install an FFmpeg built with --enable-libass"
	}
}

// pythonWorkerFix says what to do about a missing AI environment.
//
// Three answers, in the order a user is likely to want them. A release binary
// can install its own environment from the interface, and saying so is the
// whole point of that feature — "run make ai-install" is advice for someone
// with a checkout, which a user who downloaded a binary does not have.
func pythonWorkerFix(application *app.App) string {
	if ok, _ := application.ProvisioningAvailable(); ok {
		return "install it from the System page of the interface (about 300 MB), " +
			"or run `make ai-install` in a checkout"
	}
	if _, err := application.AIDir(); err != nil {
		return "the AI worker's source is missing next to the program; " +
			"extract the archive again, keeping its ai/ directory beside the binary"
	}
	return "run `make ai-install` in a checkout, or set ai.python to an interpreter " +
		"that has the AI dependencies installed"
}

// checkRuntime reports a provisioned environment that no longer works.
//
// A virtual environment records absolute paths, so an environment built before
// the program or the data directory was moved still exists and no longer runs.
// Without this check that shows up as a generic "no interpreter was found",
// which sends the user looking for a Python they already installed.
func checkRuntime(application *app.App) check {
	runtimeDir := application.RuntimeDir()
	if runtimeDir == "" {
		return check{Name: "AI runtime", Status: "ok", Detail: "not used on this installation"}
	}

	python := platform.RuntimeVenvPython(runtimeDir)
	if _, err := os.Stat(python); err != nil {
		return check{
			Name: "AI runtime", Status: "warn",
			Detail: "no environment is installed; the interface can install one " +
				"(about 300 MB, or 1 GB with the NVIDIA libraries)",
		}
	}

	if err := exec.Command(python, "-c", "pass").Run(); err != nil {
		return check{
			Name: "AI runtime", Status: "fail",
			Detail: fmt.Sprintf("%s exists but will not run: %v", python, err),
			Fix:    "install it again from the System page of the interface",
		}
	}

	manifest, err := provision.ReadManifest(platform.RuntimeManifestPath(runtimeDir))
	if err != nil {
		return check{Name: "AI runtime", Status: "ok", Detail: python}
	}

	// The recorded source directory is the thing that goes missing when an
	// archive is moved after the environment was built.
	if _, err := os.Stat(manifest.AIDir); err != nil {
		return check{
			Name: "AI runtime", Status: "fail",
			Detail: fmt.Sprintf("built from %s, which is no longer there", manifest.AIDir),
			Fix: "extract the archive where you want it to stay, then install again " +
				"from the System page",
		}
	}

	// Which dependency set it was built with, because "is my GPU being used" is
	// the question this answer is read for, and the machine's card does not
	// answer it — the environment may have been installed for the CPU.
	accelerator := "CPU"
	if manifest.Extra == provision.ExtraCUDA {
		accelerator = "NVIDIA CUDA"
	}

	return check{
		Name:   "AI runtime",
		Status: "ok",
		Detail: fmt.Sprintf("%s (built %s, %s)", python, manifest.CreatedAt.Format("2006-01-02"), accelerator),
	}
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
		RuntimeDir:    app.RuntimeDir(),
		RequireImport: true,
	})
	if err != nil {
		return check{
			Name: "python worker", Status: "fail",
			Detail: err.Error(),
			Fix:    pythonWorkerFix(app),
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
		if model.Name != wanted && model.ID != "asr:"+wanted {
			continue
		}

		// The row exists for every model this build knows about, whether or not
		// it was ever downloaded, so its presence says nothing. Only the status
		// does — and it is derived from the files on disk, which is the fact
		// this check exists to report.
		if model.Status != models.StatusReady {
			break
		}
		return check{
			Name:   "models",
			Status: "ok",
			Detail: fmt.Sprintf("%s is installed", wanted),
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
