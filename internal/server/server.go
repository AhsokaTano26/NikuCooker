// Package server hosts the HTTP surface: the embedded web application and,
// from Phase 7, the REST API described in docs/api.md.
//
// The API and the UI are served by the same binary on the same origin. That is
// why there is no CORS configuration anywhere in this project — there is no
// cross-origin request to permit.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/api"
	"github.com/AhsokaTano26/NikuCooker/internal/app"
)

// Options configures the HTTP server.
type Options struct {
	Host string
	Port int
	Log  *slog.Logger

	// App is the wired business core the API serves.
	App *app.App

	// Version and Commit are reported by the system endpoint.
	Version string
	Commit  string

	// RequestShutdown ends the process, on behalf of the interface.
	//
	// A function rather than a signal: the only thing the caller can do with it
	// is what the OS signal already does, which is to cancel the context
	// ListenAndServe was given. Left nil, the endpoint reports that the server
	// cannot be stopped from here — which is the truth for an embedded server
	// and for a test.
	RequestShutdown func()

	// OnListening is called once the socket is bound and requests are being
	// served, with the address that was actually bound.
	//
	// It exists so that a caller can announce a server that is genuinely up.
	// Anything done before ListenAndServe returns is done before the listener
	// exists — a message saying "serving at" printed there is a claim the
	// process has not yet earned, and a browser opened there may reach a port
	// with nothing behind it.
	//
	// Called from the goroutine running ListenAndServe, so it must not block.
	OnListening func(addr string)
}

// Server is the running HTTP surface.
type Server struct {
	opts Options
	log  *slog.Logger
	http *http.Server
}

// New builds the server and its routing table.
func New(opts Options) (*Server, error) {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}

	if opts.App == nil {
		return nil, fmt.Errorf("server: the application is required")
	}

	static, err := newStaticHandler()
	if err != nil {
		return nil, fmt.Errorf("server: prepare static assets: %w", err)
	}

	// The API is mounted at the root and matches its own prefix, so an
	// unrouted path under /api/v1 gets the API's JSON 404 rather than the
	// web application's HTML — which a JSON client would have to guess about.
	api, err := api.New(api.Options{
		App:             opts.App,
		Log:             opts.Log,
		Version:         opts.Version,
		Commit:          opts.Commit,
		RequestShutdown: opts.RequestShutdown,
	})
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.Handle("/api/v1/", api.Routes())
	mux.Handle("/", static)

	addr := net.JoinHostPort(opts.Host, fmt.Sprint(opts.Port))

	return &Server{
		opts: opts,
		log:  opts.Log,
		http: &http.Server{
			Addr:    addr,
			Handler: securityHeaders(mux),
			// A local tool has no slow clients to defend against, but an
			// unbounded read is still unbounded. These are generous rather than
			// tight because a media upload arrives on this same server.
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
			// No WriteTimeout: server-sent events are a long-lived response and
			// any write deadline would sever the event stream mid-run.
		},
	}, nil
}

// Addr reports the address the server will listen on.
func (s *Server) Addr() string { return s.http.Addr }

// ListenAndServe blocks until the context is cancelled or the listener fails.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		return fmt.Errorf("server: listen on %s: %w", s.http.Addr, err)
	}

	s.warnIfExposed()

	errCh := make(chan error, 1)
	go func() {
		if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	if s.opts.OnListening != nil {
		s.opts.OnListening(ln.Addr().String())
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	// The event stream is severed before waiting, not by the wait.
	//
	// Shutdown waits for in-flight requests but does not cancel their contexts,
	// and this server has no write deadline because the event stream is a
	// long-lived response. An open browser therefore holds a connection that
	// will never end on its own, and Shutdown spends its entire timeout waiting
	// for one that the shutdown itself would have to close. Closing the
	// subscriptions first turns a ten-second exit into an immediate one.
	//
	// It costs nothing on the way out: the bus is not left in a closed state —
	// a subscription made afterwards still works — and the process is about to
	// stop anyway.
	s.opts.App.Events.Close()

	// Shutdown waits for in-flight requests, which is what lets a running
	// render finish rather than being severed mid-write.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := s.http.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("server: shutdown: %w", err)
	}
	return nil
}

// warnIfExposed prints the one thing a user needs to know before putting this
// on a network.
//
// v1 has no authentication, and the service reads files from the host. Binding
// it to a public interface without saying so would be negligent; saying so once
// at startup is the whole mitigation.
func (s *Server) warnIfExposed() {
	host := s.opts.Host
	if host == "127.0.0.1" || host == "localhost" || host == "::1" {
		return
	}

	s.log.Warn("listening on a non-loopback address, but this build has no authentication",
		"host", host,
		"advice", "restrict access with a firewall or a reverse proxy that authenticates, or bind to 127.0.0.1")
}

// securityHeaders applies the headers that cost nothing and are wrong to omit.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		// The UI is self-contained: no external fonts, scripts or styles. A
		// local-first tool that phoned out to a CDN would contradict its own
		// premise, so the policy is strict rather than permissive.
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; media-src 'self' blob:; "+
				"script-src 'self'; style-src 'self' 'unsafe-inline'; "+
				"connect-src 'self'; frame-ancestors 'none'; base-uri 'self'")
		next.ServeHTTP(w, r)
	})
}

// apiPending answers every /api/v1 request until the API exists.
//
// It returns the documented error envelope (docs/api.md §1.3) rather than a
// bare 404, so a client written against the spec receives a well-formed
// response whose code names exactly what is missing.
