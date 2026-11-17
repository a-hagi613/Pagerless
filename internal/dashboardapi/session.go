package dashboardapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// maxWriteBodyBytes bounds a dashboard write body.
const maxWriteBodyBytes = 64 << 10

// SessionResponse is the body of GET /api/session. The browser reads it to
// render the signed-in user and to obtain the CSRF token that every write must
// repeat.
type SessionResponse struct {
	Authenticated bool      `json:"authenticated"`
	Username      string    `json:"username"`
	Subject       string    `json:"subject"`
	Groups        []string  `json:"groups"`
	CanApprove    bool      `json:"canApprove"`
	CSRF          string    `json:"csrfToken"`
	ExpiresAt     time.Time `json:"expiresAt"`
}

// session answers with the signed-in user.
func (h *Handler) session(w http.ResponseWriter, r *http.Request) {
	user := h.auth.User(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "sign in to use the dashboard")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, SessionResponse{
		Authenticated: true,
		Username:      user.Username,
		Subject:       user.Subject,
		Groups:        user.Groups,
		CanApprove:    user.Approver,
		CSRF:          user.CSRF,
		ExpiresAt:     user.ExpiresAt,
	})
}

// signedOut is a standalone page, so that signing out needs neither the
// session nor the application bundle.
func (h *Handler) signedOut(w http.ResponseWriter, r *http.Request) {
	message := "You are signed out."
	if r.URL.Query().Get("failed") != "" {
		message = "Sign-in did not complete. Nothing was changed."
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Pagerless</title>
<style>body{font-family:system-ui,sans-serif;margin:0;background:#0f172a;color:#e2e8f0}
main{max-width:32rem;margin:15vh auto;padding:2rem;background:#1e293b;border-radius:.5rem}
a{color:#7dd3fc}</style></head>
<body><main><h1>Pagerless</h1><p>`+message+`</p><p><a href="/login">Sign in</a></p></main></body>
</html>
`)
}

// requireUser returns the signed-in user, answering 401 when there is none.
func (h *Handler) requireUser(w http.ResponseWriter, r *http.Request) *User {
	user := h.auth.User(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "sign in to use the dashboard")
		return nil
	}
	return user
}

// requireCSRF returns the signed-in user when the request carries that
// session's CSRF token in the X-CSRF-Token header. The session cookie is
// SameSite=Lax, so a cross-site form cannot send it; the token additionally
// stops a script that can make same-site requests from acting without reading
// the session.
func (h *Handler) requireCSRF(w http.ResponseWriter, r *http.Request) *User {
	user := h.requireUser(w, r)
	if user == nil {
		return nil
	}
	supplied := r.Header.Get("X-CSRF-Token")
	if subtle.ConstantTimeCompare([]byte(supplied), []byte(user.CSRF)) != 1 {
		writeError(w, http.StatusForbidden, "the CSRF token is missing or wrong")
		return nil
	}
	return user
}

// decodeWrite reads one strict JSON body. It answers the request itself and
// returns false when the body is unusable.
func decodeWrite(w http.ResponseWriter, r *http.Request, into any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxWriteBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("the body exceeds %d bytes", maxWriteBodyBytes))
			return false
		}
		writeError(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
		return false
	}
	if decoder.More() {
		writeError(w, http.StatusBadRequest, "malformed JSON body: unexpected data after the object")
		return false
	}
	return true
}
