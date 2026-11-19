package dashboardapi

import "net/http"

// VersionResponse is the body of GET /api/version. The Portal footer and
// `kubectl page version` in server mode read the same answer.
type VersionResponse struct {
	// Version is the server's release version.
	Version string `json:"version"`
	// Mode is the authentication mode, so a client can report it without asking
	// the session route first.
	Mode string `json:"mode"`
}

// version answers with the server's release version and authentication mode.
func (h *Handler) version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, VersionResponse{Version: h.release, Mode: h.auth.Mode()})
}
