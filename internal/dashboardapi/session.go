package dashboardapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// maxWriteBodyBytes bounds a dashboard write body.
const maxWriteBodyBytes = 64 << 10

// SessionResponse is the body of GET /api/auth/session and GET /api/session.
// The browser reads it to render the signed-in user and to obtain the CSRF
// token that every write must repeat.
type SessionResponse struct {
	Authenticated bool      `json:"authenticated"`
	Username      string    `json:"username"`
	Subject       string    `json:"subject"`
	Groups        []string  `json:"groups"`
	CanApprove    bool      `json:"canApprove"`
	CSRF          string    `json:"csrfToken"`
	ExpiresAt     time.Time `json:"expiresAt"`
	Mode          string    `json:"mode"`
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
		Mode:          h.auth.Mode(),
	})
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
// session's CSRF token. The session cookie is SameSite=Lax, so a cross-site
// form cannot send it; the token additionally stops a script that can make
// same-site requests from acting without reading the session. Disabled mode and
// a Bearer request are exempt for that request alone (ADR-0032).
func (h *Handler) requireCSRF(w http.ResponseWriter, r *http.Request) *User {
	user := h.requireUser(w, r)
	if user == nil {
		return nil
	}
	if !h.auth.ValidateCSRF(r, user) {
		writeAPIError(w, http.StatusForbidden, "csrf", "the CSRF token is missing or wrong", nil)
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
