package investigations

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a-hagi613/pagerless/internal/transcript"
)

// maxRequestBytes bounds one client body.
const maxRequestBytes = 1 << 20

// API exposes the investigation service over HTTP.
type API struct {
	Service *Service
	Auth    *Auth
	// Static serves the embedded dashboard at /.
	Static http.Handler
	// Login bounds repeated token attempts.
	Login *loginLimiter
}

// NewAPI builds an API with the default login limiter.
func NewAPI(service *Service, auth *Auth, static http.Handler) *API {
	return &API{Service: service, Auth: auth, Static: static, Login: newLoginLimiter()}
}

// Handler returns the complete HTTP surface.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("GET /readyz", a.ready)
	mux.HandleFunc("POST /api/v1/session", a.session)
	mux.Handle("POST /api/v1/pages", a.authenticated(http.HandlerFunc(a.createPage)))
	mux.Handle("GET /api/v1/pages", a.authenticated(http.HandlerFunc(a.listPages)))
	mux.Handle("GET /api/v1/pages/{id}", a.authenticated(http.HandlerFunc(a.getPage)))
	mux.Handle("POST /api/v1/pages/{id}/turns", a.authenticated(http.HandlerFunc(a.addTurn)))
	mux.Handle("POST /api/v1/pages/{id}/stop", a.authenticated(http.HandlerFunc(a.stop)))
	mux.Handle("GET /api/v1/pages/{id}/events", a.authenticated(http.HandlerFunc(a.events)))
	mux.Handle("GET /api/v1/pages/{id}/evidence/{callId}", a.authenticated(http.HandlerFunc(a.evidence)))
	mux.Handle("GET /api/v1/pages/{id}/diff", a.authenticated(http.HandlerFunc(a.diff)))
	mux.Handle("POST /internal/turns/{turnId}/tools", a.internal(http.HandlerFunc(a.toolCall)))
	mux.Handle("/", a.static())
	return securityHeaders(mux)
}

// static serves the dashboard, and refuses an unknown API path as JSON so the
// single-page shell cannot answer a mistyped API route.
func (a *API) static() http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/api/") || strings.HasPrefix(request.URL.Path, "/internal/") {
			writeError(response, notFound("The route does not exist."))
			return
		}
		if a.Static == nil {
			response.Header().Set("Content-Type", "text/plain; charset=utf-8")
			response.WriteHeader(http.StatusServiceUnavailable)
			_, _ = response.Write([]byte("the dashboard is not built into this server\n"))
			return
		}
		a.Static.ServeHTTP(response, request)
	})
}

func (a *API) health(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write([]byte("ok\n"))
}

func (a *API) ready(response http.ResponseWriter, request *http.Request) {
	readiness := a.Service.Ready(request.Context())
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	status := http.StatusOK
	if readiness.Status != "ready" {
		status = http.StatusServiceUnavailable
	}
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(readiness)
}

// session exchanges the operator token for a browser cookie.
func (a *API) session(response http.ResponseWriter, request *http.Request) {
	limiter := a.Login
	if limiter == nil {
		limiter = newLoginLimiter()
	}
	if !limiter.allow(request) {
		response.Header().Set("Retry-After", retryAfterSeconds())
		writeError(response, &Error{HTTPStatus: http.StatusTooManyRequests, Code: "rate_limited",
			Message: "Too many sign-in attempts; retry later."})
		return
	}
	if !SameOrigin(request) {
		writeError(response, &Error{HTTPStatus: http.StatusForbidden, Code: "cross_origin_denied",
			Message: "The request did not come from the installation's origin."})
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(request, &body); err != nil {
		writeError(response, err)
		return
	}
	if strings.TrimSpace(body.Token) == "" {
		writeError(response, invalid("The token is required."))
		return
	}
	if failure := a.Auth.Session(response, request, body.Token); failure != nil {
		limiter.fail(request)
		writeError(response, failure)
		return
	}
	limiter.reset(request)
	response.WriteHeader(http.StatusNoContent)
}

func (a *API) createPage(response http.ResponseWriter, request *http.Request) {
	key := request.Header.Get("Idempotency-Key")
	var body Request
	if err := decodeJSON(request, &body); err != nil {
		writeError(response, err)
		return
	}
	created, err := a.Service.CreatePage(request.Context(), key, body)
	if err != nil {
		a.writeServiceError(response, err)
		return
	}
	writeJSON(response, http.StatusCreated, created)
}

func (a *API) addTurn(response http.ResponseWriter, request *http.Request) {
	key := request.Header.Get("Idempotency-Key")
	var body Request
	if err := decodeJSON(request, &body); err != nil {
		writeError(response, err)
		return
	}
	created, err := a.Service.AddTurn(request.Context(), request.PathValue("id"), key, body)
	if err != nil {
		a.writeServiceError(response, err)
		return
	}
	writeJSON(response, http.StatusCreated, created)
}

func (a *API) listPages(response http.ResponseWriter, request *http.Request) {
	limit := 50
	if raw := request.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			writeError(response, invalid("The limit must be between 1 and 100."))
			return
		}
		limit = parsed
	}
	pages, err := a.Service.Store().Pages(request.Context(), limit)
	if err != nil {
		writeError(response, internal("The pages could not be read."))
		return
	}
	summaries := make([]transcript.Summary, 0, len(pages))
	for _, page := range pages {
		summaries = append(summaries, page.Summary())
	}
	writeJSON(response, http.StatusOK, struct {
		Pages []transcript.Summary `json:"pages"`
	}{Pages: summaries})
}

// messageWire is one conversation message.
type messageWire struct {
	ID        string                `json:"id"`
	TurnID    string                `json:"turnId"`
	Role      string                `json:"role"`
	Content   string                `json:"content"`
	CreatedAt time.Time             `json:"createdAt"`
	Citations []transcript.Citation `json:"citations"`
}

func (a *API) getPage(response http.ResponseWriter, request *http.Request) {
	id := request.PathValue("id")
	page, err := a.Service.Store().Page(request.Context(), id)
	if err != nil {
		a.writeStoreError(response, err)
		return
	}
	messages, err := a.Service.Store().Messages(request.Context(), id)
	if err != nil {
		writeError(response, internal("The messages could not be read."))
		return
	}
	last, err := a.Service.Store().LastEventSeq(request.Context(), id)
	if err != nil {
		writeError(response, internal("The last event could not be read."))
		return
	}
	wire := make([]messageWire, 0, len(messages))
	for _, message := range messages {
		citations := message.Citations
		if citations == nil {
			citations = []transcript.Citation{}
		}
		wire = append(wire, messageWire{
			ID: message.ID, TurnID: message.TurnID, Role: message.Role,
			Content: message.Content, CreatedAt: message.CreatedAt, Citations: citations,
		})
	}
	writeJSON(response, http.StatusOK, struct {
		transcript.Summary
		Messages    []messageWire `json:"messages"`
		LastEventID int64         `json:"lastEventId"`
	}{Summary: page.Summary(), Messages: wire, LastEventID: last})
}

func (a *API) stop(response http.ResponseWriter, request *http.Request) {
	status, err := a.Service.Stop(request.Context(), request.PathValue("id"))
	if err != nil {
		a.writeServiceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, struct {
		Status string `json:"status"`
	}{Status: status})
}

func (a *API) events(response http.ResponseWriter, request *http.Request) {
	id := request.PathValue("id")
	if _, err := a.Service.Store().Page(request.Context(), id); err != nil {
		a.writeStoreError(response, err)
		return
	}
	after, parseErr := cursorOf(request)
	if parseErr != nil {
		writeError(response, parseErr)
		return
	}
	if strings.Contains(request.Header.Get("Accept"), "text/event-stream") {
		a.stream(response, request, id, after)
		return
	}
	events, err := a.Service.Store().EventsAfter(request.Context(), id, after)
	if err != nil {
		writeError(response, internal("The events could not be read."))
		return
	}
	writeJSON(response, http.StatusOK, struct {
		Events []transcript.Event `json:"events"`
	}{Events: events})
}

func (a *API) evidence(response http.ResponseWriter, request *http.Request) {
	pageID := request.PathValue("id")
	callID := request.PathValue("callId")
	item, err := a.Service.Store().Evidence(request.Context(), pageID, callID)
	if err != nil {
		a.writeStoreError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, struct {
		CallID     string          `json:"callId"`
		Tool       string          `json:"tool"`
		Arguments  json.RawMessage `json:"arguments"`
		Result     json.RawMessage `json:"result"`
		Status     string          `json:"status"`
		DurationMS int64           `json:"durationMs"`
		Redacted   bool            `json:"redacted"`
		Truncated  bool            `json:"truncated"`
	}{
		CallID: item.CallID, Tool: item.Tool, Arguments: item.Arguments, Result: item.Result,
		Status: item.Status, DurationMS: item.DurationMS,
		Redacted: item.Redacted > 0, Truncated: item.Truncated,
	})
}

func (a *API) diff(response http.ResponseWriter, request *http.Request) {
	proposal, found, err := a.Service.Store().Proposal(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(response, internal("The proposal could not be read."))
		return
	}
	files := []transcript.FileDiff{}
	pullRequest := ""
	if found && proposal.Status == transcript.ProposalPublished {
		files = append(files, proposal.Diff...)
		pullRequest = proposal.PRURL
	}
	writeJSON(response, http.StatusOK, struct {
		Files []transcript.FileDiff `json:"files"`
		PRURL string                `json:"prUrl,omitempty"`
	}{Files: files, PRURL: pullRequest})
}

// toolCall executes one private tool callback.
func (a *API) toolCall(response http.ResponseWriter, request *http.Request) {
	var body ToolCallRequest
	if err := decodeJSON(request, &body); err != nil {
		writeError(response, err)
		return
	}
	result, err := a.Service.CallTool(request.Context(), request.PathValue("turnId"), body)
	if err != nil {
		a.writeServiceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

// authenticated wraps a public handler.
func (a *API) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		ok, err := a.Auth.Authenticate(request)
		if err != nil {
			writeError(response, internal("The request could not be authenticated."))
			return
		}
		if !ok {
			writeError(response, &Error{HTTPStatus: http.StatusUnauthorized, Code: codeUnauthorized,
				Message: "The request is not authorized."})
			return
		}
		if IsBrowser(request) && request.Method != http.MethodGet && request.Method != http.MethodHead {
			if !SameOrigin(request) {
				writeError(response, &Error{HTTPStatus: http.StatusForbidden, Code: "cross_origin_denied",
					Message: "The request did not come from the installation's origin."})
				return
			}
		}
		next.ServeHTTP(response, request)
	})
}

// internal wraps a private handler.
func (a *API) internal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !a.Auth.Internal(request) {
			writeError(response, &Error{HTTPStatus: http.StatusUnauthorized, Code: codeUnauthorized,
				Message: "The request is not authorized."})
			return
		}
		next.ServeHTTP(response, request)
	})
}

// decodeJSON reads one bounded JSON object and rejects unknown fields.
func decodeJSON(request *http.Request, destination any) *Error {
	limited := http.MaxBytesReader(nil, request.Body, maxRequestBytes)
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		if errors.Is(err, io.EOF) {
			return invalid("The request body is required.")
		}
		return invalid("The request body is not a valid JSON object.")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return invalid("The request body must contain exactly one JSON object.")
	}
	return nil
}

// cursorOf reads after or Last-Event-ID.
func cursorOf(request *http.Request) (int64, *Error) {
	raw := request.URL.Query().Get("after")
	if raw == "" {
		raw = request.Header.Get("Last-Event-ID")
	}
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, invalid("The event cursor is not a non-negative integer.")
	}
	return value, nil
}

func (a *API) writeServiceError(response http.ResponseWriter, err error) {
	if failure, found := errors.AsType[*Error](err); found {
		writeError(response, failure)
		return
	}
	writeError(response, internal("The request failed."))
}

func (a *API) writeStoreError(response http.ResponseWriter, err error) {
	if errors.Is(err, transcript.ErrNotFound) {
		writeError(response, notFound("The resource does not exist."))
		return
	}
	writeError(response, internal("The resource could not be read."))
}

func writeJSON(response http.ResponseWriter, status int, body any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(body)
}
