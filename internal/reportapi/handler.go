package reportapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/proposal"
	"github.com/a-hagi613/pagerless/internal/triage"
)

// maxBodyBytes bounds a request body. Agent output is untrusted.
const maxBodyBytes = 1 << 20

// Handler serves the agent API.
type Handler struct {
	// Auth verifies that the caller is the Triage's Job.
	Auth *Authenticator
	// Status is the only writer of Triage status.
	Status *triage.StatusStore
	// Proposals validates Proposals.
	Proposals *proposal.Pipeline
	// Clock timestamps steps.
	Clock clock.PassiveClock
}

// errNotInvestigating is returned when a request needs an Investigating Triage and the Triage is in another phase.
var errNotInvestigating = errors.New("the Triage is not Investigating")

// errNoFindings is returned when a report would leave the Diagnosis without a cited Finding.
var errNoFindings = errors.New("the report must carry at least one finding")

// ServeHTTP routes requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/triages/{name}/events", h.handle(h.events))
	mux.HandleFunc("POST /v1/triages/{name}/report", h.handle(h.report))
	mux.ServeHTTP(w, r)
}

type endpoint func(ctx context.Context, w http.ResponseWriter, body io.Reader, t *v1alpha1.Triage)

// handle authenticates the caller as the Triage's Job, then runs the endpoint.
func (h *Handler) handle(next endpoint) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			token = ""
		}
		caller, err := h.Auth.Identify(ctx, strings.TrimSpace(token))
		if err != nil {
			h.writeAuthError(ctx, w, err)
			return
		}
		var t v1alpha1.Triage
		key := types.NamespacedName{Namespace: h.Auth.Namespace, Name: r.PathValue("name")}
		if err := h.Status.Reader.Get(ctx, key, &t); err != nil {
			if apierrors.IsNotFound(err) {
				// An authenticated agent learns nothing from a missing Triage that it could not learn from a forbidden one.
				writeJSON(w, http.StatusForbidden, agentapi.ErrorResponse{Message: "forbidden"})
				return
			}
			h.writeInternal(ctx, w, err)
			return
		}
		if err := h.Auth.Authorize(ctx, caller, &t); err != nil {
			h.writeAuthError(ctx, w, err)
			return
		}
		next(ctx, w, http.MaxBytesReader(w, r.Body, maxBodyBytes), &t)
	}
}

func (h *Handler) writeAuthError(ctx context.Context, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrUnauthenticated):
		writeJSON(w, http.StatusUnauthorized, agentapi.ErrorResponse{Message: "unauthenticated"})
	case errors.Is(err, ErrForbidden):
		writeJSON(w, http.StatusForbidden, agentapi.ErrorResponse{Message: "forbidden"})
	default:
		h.writeInternal(ctx, w, err)
	}
}

func (h *Handler) writeInternal(ctx context.Context, w http.ResponseWriter, err error) {
	logf.FromContext(ctx).Error(err, "agent API request failed")
	writeJSON(w, http.StatusInternalServerError, agentapi.ErrorResponse{Message: "internal error"})
}

// decode reads a strict JSON body. It answers the request itself and returns
// false when the body is unusable.
func decode(w http.ResponseWriter, body io.Reader, into any) bool {
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeJSON(w, http.StatusRequestEntityTooLarge, agentapi.ErrorResponse{Message: fmt.Sprintf("body exceeds %d bytes", maxBodyBytes)})
			return false
		}
		writeJSON(w, http.StatusBadRequest, agentapi.ErrorResponse{Message: "malformed JSON body: " + err.Error()})
		return false
	}
	if decoder.More() {
		writeJSON(w, http.StatusBadRequest, agentapi.ErrorResponse{Message: "malformed JSON body: unexpected data after the object"})
		return false
	}
	return true
}

// events records one step in the bounded step summary.
func (h *Handler) events(ctx context.Context, w http.ResponseWriter, body io.Reader, t *v1alpha1.Triage) {
	var event agentapi.StepEvent
	if !decode(w, body, &event) {
		return
	}
	if errs := event.Check(); len(errs) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, agentapi.ReportResponse{Errors: errs})
		return
	}
	event = event.Bounded()
	now := metav1.NewTime(h.Clock.Now())
	_, err := h.Status.Update(ctx, client.ObjectKeyFromObject(t), func(latest *v1alpha1.Triage) error {
		if latest.Status.Phase != v1alpha1.PhaseInvestigating {
			return errNotInvestigating
		}
		status := &latest.Status
		status.Steps = append(status.Steps, v1alpha1.Step{Time: now, Kind: event.Kind, Tool: event.Tool, Summary: event.Summary})
		if excess := len(status.Steps) - v1alpha1.MaxSteps; excess > 0 {
			status.Steps = slices.Clone(status.Steps[excess:])
		}
		status.StepCount++
		if event.Kind == v1alpha1.StepKindToolCall {
			status.ToolCallCount++
		}
		if event.Finding != nil {
			status.Findings = mergeFindings(status.Findings, []v1alpha1.Finding{*event.Finding})
		}
		return nil
	})
	if err != nil {
		h.writeUpdateError(ctx, w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// report validates the report and, if it passes, records the Diagnosis,
// Findings, and Proposal and moves the Triage to Proposed.
func (h *Handler) report(ctx context.Context, w http.ResponseWriter, body io.Reader, t *v1alpha1.Triage) {
	var report agentapi.Report
	if !decode(w, body, &report) {
		return
	}
	if errs := report.Check(); len(errs) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, agentapi.ReportResponse{Errors: errs})
		return
	}
	built, errs, err := h.Proposals.Build(ctx, proposal.Request{Triage: t, Action: report.Proposal}, 1)
	if err != nil {
		h.writeInternal(ctx, w, err)
		return
	}
	if len(errs) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, agentapi.ReportResponse{Errors: errs})
		return
	}
	updated, err := h.Status.Update(ctx, client.ObjectKeyFromObject(t), func(latest *v1alpha1.Triage) error {
		if latest.Status.Phase != v1alpha1.PhaseInvestigating {
			return errNotInvestigating
		}
		findings := mergeFindings(latest.Status.Findings, report.Findings)
		if len(findings) == 0 {
			return errNoFindings
		}
		latest.Status.Findings = findings
		latest.Status.Diagnosis = report.Diagnosis
		latest.Status.Proposal = built
		latest.Status.Phase = v1alpha1.PhaseProposed
		return nil
	})
	if err != nil {
		h.writeUpdateError(ctx, w, err)
		return
	}
	writeJSON(w, http.StatusOK, agentapi.ReportResponse{Accepted: true, Phase: updated.Status.Phase})
}

func (h *Handler) writeUpdateError(ctx context.Context, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNotInvestigating):
		writeJSON(w, http.StatusConflict, agentapi.ErrorResponse{Message: errNotInvestigating.Error()})
	case errors.Is(err, errNoFindings):
		writeJSON(w, http.StatusUnprocessableEntity, agentapi.ReportResponse{
			Errors: []agentapi.ValidationError{{Field: "findings", Message: errNoFindings.Error()}},
		})
	default:
		h.writeInternal(ctx, w, err)
	}
}

// mergeFindings appends the new Findings that are not already recorded, up to
// the limit. Findings that arrived in step events are therefore kept when the
// report repeats them.
func mergeFindings(existing, incoming []v1alpha1.Finding) []v1alpha1.Finding {
	merged := slices.Clone(existing)
	for _, f := range incoming {
		if len(merged) >= v1alpha1.MaxFindings {
			break
		}
		if !slices.Contains(merged, f) {
			merged = append(merged, f)
		}
	}
	return merged
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
