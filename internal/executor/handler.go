package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/a-hagi613/pagerless/internal/executorapi"
)

const (
	// maxRequestSize bounds a request body; a valid one is under a hundred bytes.
	maxRequestSize = 4 << 10
	// requestTimeout bounds one execution, including the clone, push, and GitHub calls.
	requestTimeout = 3 * time.Minute
)

// Authenticator verifies that a request comes from pagerless-server: a valid
// projected service account token, issued for this component's audience, that
// belongs to the server's service account.
type Authenticator struct {
	// Client creates TokenReviews.
	Client client.Client
	// Namespace is the namespace of the server's service account.
	Namespace string
	// ServiceAccount is the name of the server's service account.
	ServiceAccount string
}

// Authenticate verifies the bearer token.
func (a *Authenticator) Authenticate(ctx context.Context, token string) *executorapi.Error {
	if token == "" {
		return refuse(http.StatusUnauthorized, executorapi.ReasonUnauthenticated, "no bearer token")
	}
	review := &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token, Audiences: []string{executorapi.Audience}},
	}
	if err := a.Client.Create(ctx, review); err != nil {
		return refuse(http.StatusInternalServerError, "", "review the token: %v", err)
	}
	if !review.Status.Authenticated || !slices.Contains(review.Status.Audiences, executorapi.Audience) {
		return refuse(http.StatusUnauthorized, executorapi.ReasonUnauthenticated,
			"the token is not valid for audience %s", executorapi.Audience)
	}
	want := fmt.Sprintf("system:serviceaccount:%s:%s", a.Namespace, a.ServiceAccount)
	if review.Status.User.Username != want {
		return refuse(http.StatusForbidden, executorapi.ReasonForbidden, "only %s may request an execution", want)
	}
	return nil
}

// Handler serves the Executor's HTTP API.
type Handler struct {
	// Auth verifies callers.
	Auth *Authenticator
	// Executor carries out requests.
	Executor *Executor
}

// Routes returns the handler for the API, including the unauthenticated probe.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/triages/{triage}/execute", h.execute)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

func (h *Handler) execute(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	log := logf.FromContext(ctx).WithValues("triage", r.PathValue("triage"))

	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if failure := h.Auth.Authenticate(ctx, token); failure != nil {
		log.Info("refused an unauthenticated request", "reason", failure.Reason)
		write(w, failure.StatusCode, failure.Response())
		return
	}
	var request executorapi.Request
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestSize))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		write(w, http.StatusBadRequest, executorapi.ErrorResponse{
			Reason: executorapi.ReasonBadRequest, Message: "the request body is not a valid execution request",
		})
		return
	}
	response, err := h.Executor.Execute(ctx, r.PathValue("triage"), request.ProposalHash)
	var refusal *executorapi.Error
	switch {
	case errors.As(err, &refusal):
		log.Info("refused an execution", "reason", refusal.Reason, "message", refusal.Message)
		write(w, refusal.StatusCode, refusal.Response())
	case err != nil:
		log.Error(err, "execution failed")
		write(w, http.StatusInternalServerError,
			executorapi.ErrorResponse{Message: "execution failed; see the Executor's log"})
	default:
		log.Info("executed", "pullRequest", response.PullRequestURL, "state", response.State)
		write(w, http.StatusOK, response)
	}
}

func write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
