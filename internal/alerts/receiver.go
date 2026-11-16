// Package alerts is the Alertmanager webhook receiver of pagerless-server. An
// opted-in alert for a Workload in an allowlisted namespace starts a Triage,
// or joins the Active Triage of that Workload as a repeat Trigger.
package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/triage"
)

// Path is the path that Alertmanager posts its webhook payloads to.
const Path = "/v1/alerts/alertmanager"

// OptInLabel is the alert label that opts an alert in to starting a Triage.
// Only the value "true" opts in.
const OptInLabel = "pagerless.dev/triage"

const (
	optInValue       = "true"
	firingStatus     = "firing"
	labelNamespace   = "namespace"
	labelPod         = "pod"
	labelDeployment  = "deployment"
	labelStatefulSet = "statefulset"
	// maxBodyBytes bounds a webhook payload.
	maxBodyBytes = 1 << 20
	// maxAttempts bounds how often a Trigger retries when the Active Triage it
	// joins ends or disappears between the refusal and the join.
	maxAttempts = 3
)

// errNotActive means the Triage a Trigger tried to join is no longer active.
var errNotActive = errors.New("the Triage is no longer active")

// Receiver accepts Alertmanager webhook payloads and turns opted-in alerts
// into Triages. It creates Triages and records repeat Triggers through the
// same paths as every other writer, so the duplicate-refusal webhook applies.
type Receiver struct {
	// Client creates Triages.
	Client client.Client
	// Store records repeat Triggers in the status of the Active Triage.
	Store *triage.StatusStore
	// Namespace is the namespace that holds Triages.
	Namespace string
	// Namespaces are the namespaces whose alerts may start Triages. An empty list accepts none.
	Namespaces []string
}

// payload is the part of an Alertmanager webhook payload that the receiver reads.
type payload struct {
	Alerts []alert `json:"alerts"`
}

// alert is one alert of a payload.
type alert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Fingerprint string            `json:"fingerprint"`
}

// ServeHTTP answers a webhook payload. It answers 200 once every opted-in
// alert has been applied, and 500 when a Trigger could not be recorded, so
// that Alertmanager retries the payload. Alerts that are not opted in, that
// are outside the allowlist, or that name no single Workload are ignored.
func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body payload
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxBodyBytes)).Decode(&body); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			http.Error(w, fmt.Sprintf("body exceeds %d bytes", maxBodyBytes), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "malformed Alertmanager payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	for _, a := range body.Alerts {
		workload, ok := r.target(ctx, a)
		if !ok {
			continue
		}
		if err := r.trigger(ctx, workload, a); err != nil {
			logf.FromContext(ctx).Error(err, "record the Trigger of an alert", "workload", workload)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

// target returns the Workload an alert names, and false when the alert must
// be ignored.
func (r *Receiver) target(ctx context.Context, a alert) (v1alpha1.WorkloadReference, bool) {
	if a.Status != firingStatus || a.Labels[OptInLabel] != optInValue {
		return v1alpha1.WorkloadReference{}, false
	}
	namespace := a.Labels[labelNamespace]
	if !slices.Contains(r.Namespaces, namespace) {
		return v1alpha1.WorkloadReference{}, false
	}
	deployment, statefulSet := a.Labels[labelDeployment], a.Labels[labelStatefulSet]
	switch {
	case deployment != "" && statefulSet == "":
		return v1alpha1.WorkloadReference{Kind: v1alpha1.WorkloadKindDeployment, Namespace: namespace, Name: deployment}, true
	case statefulSet != "" && deployment == "":
		return v1alpha1.WorkloadReference{
			Kind: v1alpha1.WorkloadKindStatefulSet, Namespace: namespace, Name: statefulSet,
		}, true
	default:
		logf.FromContext(ctx).Info("ignore an opted-in alert that does not name exactly one Workload",
			"namespace", namespace, "fingerprint", a.Fingerprint)
		return v1alpha1.WorkloadReference{}, false
	}
}

// trigger starts a Triage for the Workload or, when the Workload already has
// an Active Triage, records the alert as a repeat Trigger of it. The
// duplicate-refusal webhook names the Active Triage in its refusal.
func (r *Receiver) trigger(ctx context.Context, workload v1alpha1.WorkloadReference, a alert) error {
	for range maxAttempts {
		err := r.Client.Create(ctx, r.newTriage(workload, a))
		if err == nil {
			return nil
		}
		if apierrors.IsInvalid(err) {
			// The alert cannot be represented as a Triage, and retrying it would not help.
			logf.FromContext(ctx).Info("ignore an alert that is not a valid Trigger", "reason", err.Error())
			return nil
		}
		name, ok := activeTriageName(err)
		if !ok {
			return fmt.Errorf("create Triage: %w", err)
		}
		err = r.join(ctx, name)
		if errors.Is(err, errNotActive) || apierrors.IsNotFound(err) {
			continue
		}
		return err
	}
	return fmt.Errorf("the Active Triage of %s/%s ended during %d attempts to join it",
		workload.Namespace, workload.Name, maxAttempts)
}

// newTriage returns the Triage that an alert starts.
func (r *Receiver) newTriage(workload v1alpha1.WorkloadReference, a alert) *v1alpha1.Triage {
	return &v1alpha1.Triage{
		GenerateName: "triage-", Namespace: r.Namespace,
		Spec: v1alpha1.TriageSpec{
			Workload: workload,
			Trigger: v1alpha1.Trigger{
				Source:           v1alpha1.TriggerSourceAlertmanager,
				Pod:              a.Labels[labelPod],
				AlertFingerprint: a.Fingerprint,
			},
		},
	}
}

// join counts one more Trigger for the named Triage, which must be active.
func (r *Receiver) join(ctx context.Context, name string) error {
	_, err := r.Store.Update(ctx, types.NamespacedName{Namespace: r.Namespace, Name: name},
		func(latest *v1alpha1.Triage) error {
			if latest.Status.Phase.IsTerminal() {
				return errNotActive
			}
			latest.Status.TriggerCount++
			return nil
		})
	return err
}

// activeTriageName returns the name of the Active Triage that a refused create
// names, when the refusal is the duplicate refusal of the admission webhook.
func activeTriageName(err error) (string, bool) {
	var status apierrors.APIStatus
	if !errors.As(err, &status) || !apierrors.IsAlreadyExists(err) {
		return "", false
	}
	details := status.Status().Details
	if details == nil || details.Name == "" {
		return "", false
	}
	return details.Name, true
}
