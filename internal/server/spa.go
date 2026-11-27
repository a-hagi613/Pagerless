package server

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
const unavailableMessage = "the dashboard assets are not built into this server; " +
	"run `make dashboard-build` before building pagerless-server\n"

// NewSPA returns a handler that serves the built single-page application.
// Content-hashed assets are immutable; index.html is revalidated, and an
// unknown client route falls back to it so a deep link works.
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

func unavailable() http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "text/plain; charset=utf-8")
		response.Header().Set("Cache-Control", "no-store")
		response.WriteHeader(http.StatusServiceUnavailable)
		_, _ = response.Write([]byte(unavailableMessage))
	})
}

// spa serves the Vite build and falls back to index.html for client routes.
type spa struct {
	root  fs.FS
	index []byte
}

// ServeHTTP serves one file, the application shell, or a 404.
func (s *spa) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet, http.MethodHead:
	default:
		response.Header().Set("Allow", "GET, HEAD")
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	requested := strings.TrimPrefix(path.Clean("/"+request.URL.Path), "/")
	if requested == "" {
		s.serveIndex(response, request)
		return
	}
	if info, err := fs.Stat(s.root, requested); err == nil && !info.IsDir() {
		s.serveFile(response, request, requested, info)
		return
	}
	if path.Ext(requested) != "" {
		http.NotFound(response, request)
		return
	}
	s.serveIndex(response, request)
}

func (s *spa) serveFile(response http.ResponseWriter, request *http.Request, name string, info fs.FileInfo) {
	content, err := fs.ReadFile(s.root, name)
	if err != nil {
		http.NotFound(response, request)
		return
	}
	if strings.HasPrefix(name, "assets/") {
		response.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		response.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(response, request, name, info.ModTime(), bytes.NewReader(content))
}

func (s *spa) serveIndex(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(response, request, "index.html", time.Time{}, bytes.NewReader(s.index))
}
