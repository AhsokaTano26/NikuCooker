package server

import (
	"io"
	"io/fs"
	"net/http"
	"strings"

	"github.com/AhsokaTano26/NikuCooker/web"
)

// staticHandler serves the embedded web application.
//
// The application is a single-page app, so any path that is not a real file
// falls back to index.html and lets the client router resolve it. Serving a 404
// for /projects/01J8ZP on a hard refresh is the classic way this breaks.
type staticHandler struct {
	root http.FileSystem
	// index is the document root, served for unmatched paths. Nil when the
	// application was not built into this binary.
	index []byte
}

func newStaticHandler() (*staticHandler, error) {
	sub, err := fs.Sub(web.Dist, web.DistDir)
	if err != nil {
		return nil, err
	}

	h := &staticHandler{root: http.FS(sub)}

	// A build without the frontend still has dist/ present — it holds the
	// .gitkeep that makes //go:embed compile — but no index.html. Detecting that
	// here means the handler can say so, instead of returning 404s that look
	// like a broken deployment.
	if raw, err := fs.ReadFile(sub, "index.html"); err == nil {
		h.index = raw
	}
	return h, nil
}

func (h *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if h.index == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusServiceUnavailable)
		if r.Method == http.MethodGet {
			_, _ = w.Write(web.UnbuiltPage())
		}
		return
	}

	// Vite emits content-hashed asset names, so anything under /assets can be
	// cached indefinitely. index.html must not be, or a user keeps loading the
	// previous deployment's bundle after an upgrade.
	if strings.HasPrefix(r.URL.Path, "/assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}

	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		h.serveIndex(w, r)
		return
	}

	file, err := h.root.Open(path)
	if err != nil {
		// Not a real file: hand it to the client router.
		h.serveIndex(w, r)
		return
	}
	defer func() { _ = file.Close() }()

	stat, err := file.Stat()
	if err != nil || stat.IsDir() {
		h.serveIndex(w, r)
		return
	}

	w.Header().Set("Content-Type", contentTypeFor(path))
	if r.Method == http.MethodGet {
		_, _ = io.Copy(w, file)
	}
}

func (h *staticHandler) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet {
		_, _ = w.Write(h.index)
	}
}

// contentTypeFor maps the extensions Vite emits.
//
// A hand-written table rather than mime.TypeByExtension, which consults the
// host's registry on Windows and the system mime database elsewhere — so the
// same build could serve a different Content-Type per machine, and on Windows
// .js has historically resolved to text/plain.
func contentTypeFor(path string) string {
	switch {
	case strings.HasSuffix(path, ".js"), strings.HasSuffix(path, ".mjs"):
		return "text/javascript; charset=utf-8"
	case strings.HasSuffix(path, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(path, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(path, ".json"):
		return "application/json; charset=utf-8"
	case strings.HasSuffix(path, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(path, ".woff2"):
		return "font/woff2"
	case strings.HasSuffix(path, ".png"):
		return "image/png"
	case strings.HasSuffix(path, ".ico"):
		return "image/x-icon"
	default:
		return "application/octet-stream"
	}
}
