package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/AhsokaTano26/NikuCooker/internal/server"
)

func newServeCmd() *cobra.Command {
	var (
		host      string
		port      int
		logLevel  string
		logFormat string
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the HTTP server and the web interface",
		Long: `Starts the NikuCooker HTTP server.

The web application is embedded in this binary, so there is nothing else to
install or point at. The API lives under /api/v1 on the same origin.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger, err := newLogger(cmd, logLevel, logFormat)
			if err != nil {
				return err
			}

			srv, err := server.New(server.Options{
				Host: host,
				Port: port,
				Log:  logger,
			})
			if err != nil {
				return err
			}

			// Cancelled by either an interrupt or a termination signal, so that
			// `docker stop` and Ctrl-C behave identically.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			logger.Info("listening", "addr", srv.Addr(), "version", currentVersion().Version)
			fmt.Fprintf(cmd.OutOrStdout(), "NikuCooker is serving at http://%s\n", displayAddr(host, port))

			return srv.ListenAndServe(ctx)
		},
	}

	cmd.Flags().StringVar(&host, "host", "127.0.0.1",
		"address to bind. The default is loopback: this build has no authentication")
	cmd.Flags().IntVar(&port, "port", 8080, "port to bind")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "log level: debug, info, warn, error")
	cmd.Flags().StringVar(&logFormat, "log-format", "text", "log format: text or json")

	return cmd
}

// displayAddr renders the address as something a user can paste into a browser.
func displayAddr(host string, port int) string {
	switch host {
	case "0.0.0.0", "::", "":
		return fmt.Sprintf("localhost:%d", port)
	default:
		return fmt.Sprintf("%s:%d", host, port)
	}
}

// newLogger builds the structured logger.
//
// Fields carried here — component, and later project_id, job_id and stage — are
// what make a log line answerable to "which job, doing what, where".
func newLogger(cmd *cobra.Command, level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("invalid --log-level %q: expected debug, info, warn or error", level)
	}

	opts := &slog.HandlerOptions{Level: lvl}

	var handler slog.Handler
	switch format {
	case "text":
		handler = slog.NewTextHandler(cmd.ErrOrStderr(), opts)
	case "json":
		handler = slog.NewJSONHandler(cmd.ErrOrStderr(), opts)
	default:
		return nil, fmt.Errorf("invalid --log-format %q: expected text or json", format)
	}

	return slog.New(handler).With("component", "core"), nil
}
