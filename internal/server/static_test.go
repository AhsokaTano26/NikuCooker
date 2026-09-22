package server

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// handler builds a static handler with a fake build in it.
//
// The embedded application is a build artefact, so a test that asserted against
// it would fail whenever the frontend was rebuilt. The behaviour under test is
// the routing, which depends only on what exists — not on what it contains.
func handler(t *testing.T) *staticHandler {
	t.Helper()

	return &staticHandler{
		root:  http.FS(assetDir{}),
		index: []byte("<!doctype html><title>app</title>"),
	}
}

// assetDir is a minimal filesystem: one real asset, and a favicon.
type assetDir struct{}

func (assetDir) Open(name string) (fs.File, error) {
	switch name {
	case "index.html", "favicon.ico":
		return newFakeFile(name), nil
	default:
		return nil, fs.ErrNotExist
	}
}

func get(t *testing.T, h *staticHandler, path string) *httptest.ResponseRecorder {
	t.Helper()

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder
}

// A route the server does not know is the client router's.
//
// This is the behaviour the fallback exists for: a hard refresh on
// /projects/<id> has to reach the application, or the page works until someone
// reloads it.
func TestUnknownRouteServesTheApplication(t *testing.T) {
	h := handler(t)

	for _, path := range []string{"/", "/logs", "/projects/01J8ZP4W", "/settings"} {
		recorder := get(t, h, path)

		if recorder.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, recorder.Code)
		}
		if got := recorder.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
			t.Errorf("%s: content-type = %q, want the document", path, got)
		}
	}
}

// A missing asset is a 404, not the application.
//
// The failure this prevents is invisible. Every file under /assets carries a
// content hash, so one that is missing was deleted by a newer build — and a
// browser running the previous bundle still has the old names. Answering those
// with index.html means an `import()` receives HTML with nosniff set, so it
// rejects rather than executes; for a route's lazy chunk the visible result is
// a menu item that does nothing when clicked, and no error anywhere to explain
// it.
func TestMissingAssetIsNotFound(t *testing.T) {
	h := handler(t)

	recorder := get(t, h, "/assets/LogsView-DEADBEEF.js")

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got == "text/html; charset=utf-8" {
		t.Error("a missing asset was answered with the document")
	}
}

// A file that exists is served as itself, with a type the browser will run.
func TestRealAssetIsServed(t *testing.T) {
	h := handler(t)

	recorder := get(t, h, "/favicon.ico")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if recorder.Body.Len() == 0 {
		t.Error("the file was not written")
	}
}

// The document is never cached; the hashed assets always are.
//
// Both halves matter. A cached document keeps serving the previous build's file
// names after an upgrade, and the file names it contains are the ones that then
// cannot be found.
func TestCacheHeadersSeparateTheDocumentFromTheAssets(t *testing.T) {
	h := handler(t)

	if got := get(t, h, "/").Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("the document is cacheable: %q", got)
	}
	if got := get(t, h, "/assets/app-abc123.js").Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("assets are not immutably cacheable: %q", got)
	}
}

// fakeFile is the smallest http.File that satisfies the handler.
type fakeFile struct {
	name   string
	offset int
}

func newFakeFile(name string) *fakeFile { return &fakeFile{name: name} }

func (f *fakeFile) Read(p []byte) (int, error) {
	if f.offset > 0 {
		return 0, io.EOF
	}
	n := copy(p, "contents of "+f.name)
	f.offset += n
	return n, nil
}

func (f *fakeFile) Close() error                       { return nil }
func (f *fakeFile) Seek(int64, int) (int64, error)     { return 0, nil }
func (f *fakeFile) Readdir(int) ([]fs.FileInfo, error) { return nil, nil }

func (f *fakeFile) Stat() (fs.FileInfo, error) {
	// Not a directory, which is what the handler checks before serving.
	return fakeInfo{name: f.name}, nil
}

type fakeInfo struct{ name string }

func (i fakeInfo) Name() string       { return i.name }
func (i fakeInfo) Size() int64        { return 12 }
func (i fakeInfo) Mode() fs.FileMode  { return 0o644 }
func (i fakeInfo) ModTime() time.Time { return time.Time{} }
func (i fakeInfo) IsDir() bool        { return false }
func (i fakeInfo) Sys() any           { return nil }
