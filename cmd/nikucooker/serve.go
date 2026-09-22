package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/AhsokaTano26/NikuCooker/internal/server"
)

func newServeCmd(g *globals) *cobra.Command {
	var (
		host string
		port int
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the HTTP server and the web interface",
		Long: `Starts the NikuCooker HTTP server.

The web application is embedded in this binary, so there is nothing else to
install or point at. The API lives under /api/v1 on the same origin.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			application, err := g.openApp(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = application.Close() }()

			srv, err := server.New(server.Options{
				Host:    host,
				Port:    port,
				Log:     application.Logger(),
				App:     application,
				Version: currentVersion().Version,
				Commit:  currentVersion().Commit,
			})
			if err != nil {
				return err
			}

			// Cancelled by either an interrupt or a termination signal, so that
			// `docker stop` and Ctrl-C behave identically.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			application.Logger().Info("listening", "addr", srv.Addr(), "version", currentVersion().Version)
			fmt.Fprintf(cmd.OutOrStdout(), "NikuCooker is serving at http://%s\n", displayAddr(host, port))

			return srv.ListenAndServe(ctx)
		},
	}

	cmd.Flags().StringVar(&host, "host", "127.0.0.1",
		"address to bind. The default is loopback: this build has no authentication")
	cmd.Flags().IntVar(&port, "port", 8080, "port to bind")

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
