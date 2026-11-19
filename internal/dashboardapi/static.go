package dashboardapi

import (
	"bytes"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// distDirectory is the Vite output directory inside the embedded assets.
const distDirectory = "dist"

// unavailableMessage explains why the browser has no application to load.
const unavailableMessage = "the dashboard assets are not built into this server; run `make dashboard-build` before building pagerless-server\n"

// NewSPA returns a handler that serves the built single-page application. Assets are immutable and hashed, so they
// may be cached; index.html is revalidated, and an unknown client route falls back to it so that a deep link works.
// A server built without the application answers 503 with the build instruction instead of an empty page.
func NewSPA(assets fs.FS) http.Handler {
	if assets == nil {
		return unavailable()
	}
	root, err := fs.Sub(assets, distDirectory)
	if err != nil {
		return unavailable()
	}
	index, err := fs.ReadFile(root, "index.html")
	if err != nil {
		return unavailable()
	}
	return &spa{root: root, index: index}
}

// unavailable answers every request with the build instruction.
func unavailable() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(unavailableMessage))
	})
}

// spa serves the Vite build and falls back to index.html for client-side routes.
type spa struct {
	root  fs.FS
	index []byte
}

// ServeHTTP serves one file, the application shell, or a 404.
func (s *spa) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	default:
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	requested := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if requested == "" {
		s.serveIndex(w, r)
		return
	}
	if info, err := fs.Stat(s.root, requested); err == nil && !info.IsDir() {
		s.serveFile(w, r, requested, info)
		return
	}
	// A client route such as /pages/foo has no extension and is answered with the application shell.
	if path.Ext(requested) != "" {
		http.NotFound(w, r)
		return
	}
	s.serveIndex(w, r)
}

// serveFile serves one built file with a cache policy that matches its content.
func (s *spa) serveFile(w http.ResponseWriter, r *http.Request, name string, info fs.FileInfo) {
	content, err := fs.ReadFile(s.root, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if strings.HasPrefix(name, "assets/") {
		// Vite writes content-hashed file names, so a file never changes under its name.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, name, info.ModTime(), bytes.NewReader(content))
}

// serveIndex serves the application shell, which must be revalidated so a new build is picked up.
func (s *spa) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(s.index))
}
