package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/agentauth"
)

const (
	// maxArgumentsBytes bounds one tool call's arguments.
	maxArgumentsBytes = 64 << 10
)

// Handler serves the read-only tool API: the investigation manifest and the
// scoped tool calls. The report and event endpoints live in reportapi and use
// the same authenticator.
// forbiddenMessage is the answer to every authentication refusal, so an agent
// cannot tell a missing Page from one it may not read.
const forbiddenMessage = "forbidden"

type Handler struct {
	// Auth authenticates the caller as the Page's Job with a TokenReview.
	Auth *agentauth.Authenticator
	// Broker serves the tools.
	Broker *Broker
}

// ServeHTTP routes requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/pages/{name}/investigation", h.manifest)
	mux.HandleFunc("POST /v1/pages/{name}/tools/{tool}", h.call)
	mux.ServeHTTP(w, r)
}

// manifest answers the investigation manifest in one response.
func (h *Handler) manifest(w http.ResponseWriter, r *http.Request) {
	page, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	if page.Status.Phase.IsTerminal() {
		writeError(w, http.StatusConflict, agentapi.ErrorResponse{Message: "the Page has reached a terminal phase"})
		return
	}
	manifest, err := h.Broker.Manifest(r.Context(), page)
	if err != nil {
		writeInternal(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, manifest)
}

// call serves one scoped tool call.
func (h *Handler) call(w http.ResponseWriter, r *http.Request) {
	page, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	if page.Status.Phase.IsTerminal() {
		writeError(w, http.StatusConflict, agentapi.ErrorResponse{Message: "the Page has reached a terminal phase"})
		return
	}
	idempotency := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotency == "" || len(idempotency) > 256 || strings.ContainsAny(idempotency, " \t\r\n") {
		writeRefusal(w, http.StatusBadRequest, "an Idempotency-Key header is required")
		return
	}
	body := http.MaxBytesReader(w, r.Body, maxArgumentsBytes)
	payload, err := io.ReadAll(body)
	if err != nil {
		if _, isTooLarge := errors.AsType[*http.MaxBytesError](err); isTooLarge {
			writeRefusal(w, http.StatusRequestEntityTooLarge, "the tool arguments exceed the size bound")
			return
		}
		writeRefusal(w, http.StatusBadRequest, "the tool arguments could not be read")
		return
	}
	var envelope struct {
		Arguments json.RawMessage `json:"arguments"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		writeRefusal(w, http.StatusBadRequest, "the tool body must be a JSON object with arguments")
		return
	}
	if len(envelope.Arguments) == 0 {
		envelope.Arguments = json.RawMessage(`{}`)
	}
	result, refused, status := h.Broker.Call(r.Context(), page, r.PathValue("tool"), envelope.Arguments, idempotency)
	if refused != nil {
		writeJSON(w, status, refused)
		return
	}
	writeJSON(w, status, result)
}

// authenticate loads the Page, validates the projected token for that Page's
// audience, and verifies the UID header and the Job ownership. It answers the
// request itself and returns false when anything fails.
func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request) (*v1alpha1.Page, bool) {
	ctx := r.Context()
	var page v1alpha1.Page
	key := client.ObjectKey{Namespace: h.Auth.Namespace, Name: r.PathValue("name")}
	if err := h.Auth.Reader.Get(ctx, key, &page); err != nil {
		// An authenticated agent learns nothing from a missing Page that it could
		// not learn from a forbidden one.
		writeError(w, http.StatusForbidden, agentapi.ErrorResponse{Message: forbiddenMessage})
		return nil, false
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		token = ""
	}
	caller, err := h.Auth.Identify(ctx, strings.TrimSpace(token), agentauth.Audience(&page))
	if err != nil {
		h.writeAuthError(ctx, w, err)
		return nil, false
	}
	uid := strings.TrimSpace(r.Header.Get(agentapi.PageUIDHeader))
	if uid == "" {
		uid = strings.TrimSpace(r.Header.Get(agentapi.PageUIDHeaderCompat))
	}
	if uid == "" || uid != string(page.UID) {
		writeError(w, http.StatusForbidden, agentapi.ErrorResponse{Message: forbiddenMessage})
		return nil, false
	}
	if err := h.Auth.Authorize(ctx, caller, &page); err != nil {
		h.writeAuthError(ctx, w, err)
		return nil, false
	}
	return &page, true
}

// writeAuthError maps an authentication failure to its status.
func (h *Handler) writeAuthError(ctx context.Context, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, agentauth.ErrUnauthenticated):
		writeError(w, http.StatusUnauthorized, agentapi.ErrorResponse{Message: "unauthenticated"})
	case errors.Is(err, agentauth.ErrForbidden):
		writeError(w, http.StatusForbidden, agentapi.ErrorResponse{Message: forbiddenMessage})
	default:
		writeInternal(ctx, w, err)
	}
}

// writeInternal logs and answers an unexpected failure.
func writeInternal(ctx context.Context, w http.ResponseWriter, err error) {
	logf.FromContext(ctx).Error(err, "tool broker request failed")
	writeError(w, http.StatusInternalServerError, agentapi.ErrorResponse{Message: "internal error"})
}

// writeRefusal answers a refused tool call in the agent's error shape.
func writeRefusal(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, Refusal{OK: false, Error: RefusalError{Code: CodeInvalidArguments, Message: message}})
}

// writeJSON answers with a JSON body.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError answers with the agent API's generic error shape.
func writeError(w http.ResponseWriter, status int, body agentapi.ErrorResponse) {
	writeJSON(w, status, body)
}
