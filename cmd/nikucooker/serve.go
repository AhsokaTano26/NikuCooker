package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/AhsokaTano26/NikuCooker/internal/platform"
	"github.com/AhsokaTano26/NikuCooker/internal/server"
)

func newServeCmd(g *globals) *cobra.Command {
	var (
		host     string
		port     int
		openPage bool
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

			// Cancelled by either an interrupt or a termination signal, so that
			// a service manager's SIGTERM and Ctrl-C behave identically.
			signalCtx, stopSignals := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stopSignals()

			// Cancellable from inside the process as well as from outside it.
			//
			// stopSignals does not cancel anything — it only stops the signal
			// handler — so being able to end the process from the interface
			// needs a context of our own to cancel. Everything downstream sees
			// the same shutdown either way: ListenAndServe returns, maintenance
			// stops, the worker pool and the database close.
			ctx, cancel := context.WithCancel(signalCtx)
			defer cancel()

			srv, err := server.New(server.Options{
				Host:    host,
				Port:    port,
				Log:     application.Logger(),
				App:     application,
				Version: currentVersion().Version,
				Commit:  currentVersion().Commit,

				// The one handle the web interface has on this process.
				RequestShutdown: cancel,

				// Everything that should happen only once the server is
				// genuinely up. Both of these were done before ListenAndServe
				// was called, which is before the port was bound — so a port
				// already in use printed "serving at" and then failed, and a
				// browser opened there had nothing to reach.
				OnListening: func(bound string) {
					url := "http://" + browsableAddr(bound)

					application.Logger().Info("listening",
						"addr", bound, "version", currentVersion().Version)
					fmt.Fprintf(cmd.OutOrStdout(), "NikuCooker is serving at %s\n", url)

					if !openPage {
						return
					}

					err := platform.OpenBrowser(cmd.Context(), url)
					switch {
					case err == nil:
						application.Logger().Info("opened the control page in a browser", "url", url)
					case errors.Is(err, platform.ErrNoOpener):
						// Expected wherever there is no desktop — a container,
						// a build machine, a shell on a server somewhere. It is
						// not a problem and it has no fix, so it is not
						// reported as one.
						application.Logger().Debug("no browser to open the control page in",
							"url", url)
					default:
						application.Logger().Warn("could not open the control page",
							"url", url, "error", err)
					}
				},
			})
			if err != nil {
				return err
			}

			// Started here rather than in the application, because a
			// `nikucooker run` must not decide on its way past that some other
			// project has expired. The server is the long-lived process, so it
			// is the one that enforces a retention measured in days.
			stopMaintenance := application.StartMaintenance(ctx)
			defer stopMaintenance()

			// Whether this installation can transcribe at all, answered in the
			// background and announced on the event stream. A user who cannot
			// run a job should not have to open the right page to find out why,
			// and the interface has to be usable before the answer arrives —
			// the check spawns Python, and it does not always resolve quickly.
			application.StartEnvironmentCheck(ctx)

			return srv.ListenAndServe(ctx)
		},
	}

	cmd.Flags().StringVar(&host, "host", "127.0.0.1",
		"address to bind. The default is loopback: this build has no authentication")
	cmd.Flags().IntVar(&port, "port", 8080, "port to bind")
	// On by default, and harmless where it cannot work: a machine with no
	// browser reports that it has none and the server carries on. Disable it
	// with --open=false on a machine where the browser would open somewhere
	// unhelpful, such as a remote shell with a forwarded display.
	cmd.Flags().BoolVar(&openPage, "open", true,
		"open the control page in a browser once the server is listening")

	return cmd
}

// browsableAddr turns the address a listener bound into one a browser can use.
//
// A listener bound to a wildcard address is reachable at localhost and not at
// 0.0.0.0, which is not an address any browser will connect to. Taken from what
// was actually bound rather than from the flags, so that a port of 0 — ask the
// kernel — still produces the port the kernel chose.
func browsableAddr(bound string) string {
	host, port, err := net.SplitHostPort(bound)
	if err != nil {
		return bound
	}

	switch host {
	case "0.0.0.0", "::", "":
		return net.JoinHostPort("localhost", port)
	default:
		return net.JoinHostPort(host, port)
	}
}
