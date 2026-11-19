package dashboardapi

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// builtAssets is the smallest file tree that the Vite build produces.
func builtAssets() fs.FS {
	return fstest.MapFS{
		"dist/index.html":    {Data: []byte(`<!doctype html><div id="root"></div>`)},
		"dist/assets/app.js": {Data: []byte("console.log('pagerless');")},
	}
}

func TestSPAServesTheShellAndClientRoutes(t *testing.T) {
	t.Parallel()
	handler := NewSPA(builtAssets())
	for _, target := range []string{"/", "/pages/checkout-abc"} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", target, response.Code)
		}
		if body := response.Body.String(); !strings.Contains(body, `<div id="root">`) {
			t.Errorf("GET %s served %q, want the application shell", target, body)
		}
		if got := response.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("GET %s Cache-Control = %q, want no-cache", target, got)
		}
	}
}

func TestSPAServesHashedAssetsForALongTime(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodGet, "/assets/app.js", nil)
	response := httptest.NewRecorder()
	NewSPA(builtAssets()).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("GET /assets/app.js = %d, want 200", response.Code)
	}
	if body := response.Body.String(); !strings.Contains(body, "pagerless") {
		t.Errorf("body = %q, want the built file", body)
	}
	if got := response.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q, want the immutable policy", got)
	}
}

func TestSPARejectsAnUnknownFileAndAWrongMethod(t *testing.T) {
	t.Parallel()
	handler := NewSPA(builtAssets())
	for _, test := range []struct {
		name, method, target string
		status               int
	}{
		{name: "unknown file", method: http.MethodGet, target: "/missing.js", status: http.StatusNotFound},
		{name: "wrong method", method: http.MethodPost, target: "/", status: http.StatusMethodNotAllowed},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.target, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Errorf("%s %s = %d, want %d", test.method, test.target, response.Code, test.status)
			}
		})
	}
}

func TestSPAWithoutABuildExplainsHowToBuildIt(t *testing.T) {
	t.Parallel()
	handler := NewSPA(fstest.MapFS{"dist/.gitkeep": {Data: []byte("marker")}})
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Errorf("GET / = %d, want 503", response.Code)
	}
	if body := response.Body.String(); !strings.Contains(body, "make dashboard-build") {
		t.Errorf("body = %q, want the build instruction", body)
	}
}
