package main

import (
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/AhsokaTano26/NikuCooker/internal/app"
	"github.com/AhsokaTano26/NikuCooker/internal/logging"
)

const appName = "nikucooker"

// globals are the flags every command shares.
//
// A struct passed to each command rather than a package-level variable, so that
// a test can build two independent command trees without them sharing state.
type globals struct {
	configPath string
	dataDir    string
	logLevel   string
	logFormat  string

	// logs retains recent records so the interface can show them. Created once
	// per command tree and shared with the application, so the logger and the
	// log endpoint are looking at the same buffer.
	logs *logging.Buffer
}

// logger builds the structured logger from the shared flags.
func (g *globals) logger(cmd *cobra.Command) (*slog.Logger, error) {
	var level slog.Level
	switch g.logLevel {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return nil, fmt.Errorf("invalid --log-level %q: expected debug, info, warn or error", g.logLevel)
	}

	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	switch g.logFormat {
	case "text":
		handler = slog.NewTextHandler(cmd.ErrOrStderr(), opts)
	case "json":
		handler = slog.NewJSONHandler(cmd.ErrOrStderr(), opts)
	default:
		return nil, fmt.Errorf("invalid --log-format %q: expected text or json", g.logFormat)
	}

	if g.logs == nil {
		g.logs = logging.NewBuffer(logging.DefaultCapacity)
	}

	// Tee'd into the buffer as well as written to the terminal: stderr is still
	// where a person looks when the server is not running, and the buffer is
	// what the interface reads when it is.
	return slog.New(logging.NewHandler(handler, g.logs)).With("component", "core"), nil
}

// openApp builds the wired core from the shared flags.
//
// Every command that touches a project goes through here, which is what makes
// the CLI and the HTTP API the same program with two front ends rather than two
// implementations that agree until they do not.
func (g *globals) openApp(cmd *cobra.Command) (*app.App, error) {
	logger, err := g.logger(cmd)
	if err != nil {
		return nil, err
	}

	// A data directory on the command line is the highest-precedence
	// configuration layer, which is what makes `--data-dir` usable for a
	// one-off run against a copied data tree.
	var flags map[string]any
	if g.dataDir != "" {
		flags = map[string]any{"storage": map[string]any{"data_dir": g.dataDir}}
	}

	return app.New(cmd.Context(), app.Options{
		ConfigPath:   g.configPath,
		Flags:        flags,
		CodeRevision: currentVersion().CodeRevision,
		Log:          logger,
		Logs:         g.logs,
	})
}

// newRootCmd builds the command tree.
//
// Commands are constructed here rather than registered from init() so that a
// fresh, independent tree can be built per test run, with no shared global
// state leaking between them.
func newRootCmd() *cobra.Command {
	g := &globals{}

	root := &cobra.Command{
		Use:   appName,
		Short: "Local-first AI fansubbing pipeline",
		Long: `NikuCooker turns raw, untranslated video into translated, reviewed and
ready-to-watch releases.

Speech recognition and media processing run entirely on this machine; audio and
video never leave it. Only subtitle text is sent to an LLM, and only to the
provider you configure.`,
		// A usage dump after every runtime error buries the actual message.
		SilenceUsage: true,
	}

	root.PersistentFlags().StringVar(&g.configPath, "config", "",
		"configuration file to read. Absent files are not an error: the defaults are complete")
	root.PersistentFlags().StringVar(&g.dataDir, "data-dir", "",
		"data directory, overriding the configuration")
	root.PersistentFlags().StringVar(&g.logLevel, "log-level", "info",
		"log level: debug, info, warn, error")
	root.PersistentFlags().StringVar(&g.logFormat, "log-format", "text",
		"log format: text or json")

	root.AddCommand(
		newServeCmd(g),
		newConfigCmd(g),
		newVersionCmd(),
		newDoctorCmd(g),
		newProjectCmd(g),
		newRunCmd(g),
		newGlossaryCmd(g),
		newModelsCmd(g),
	)

	return root
}
