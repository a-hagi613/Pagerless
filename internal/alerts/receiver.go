// Package alerts is the Alertmanager webhook receiver of pagerless-server. An
// opted-in alert for a Target in an allowlisted namespace starts a Page,
// or joins the Active Page of that Target as a repeat Trigger.
package alerts

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/page"
)

// Path is the path that Alertmanager posts its webhook payloads to.
const Path = "/v1/alerts/alertmanager"

// OptInLabel is the alert label that opts an alert in to starting a Page.
// Only the value "true" opts in.
const OptInLabel = "pagerless_page"

const (
	optInValue       = "true"
	firingStatus     = "firing"
	labelNamespace   = "namespace"
	labelPod         = "pod"
	labelDeployment  = "deployment"
	labelStatefulSet = "statefulset"
	// maxBodyBytes bounds a webhook payload.
	maxBodyBytes = 1 << 20
	// maxAttempts bounds how often a Trigger retries when the Active Page it
	// joins ends or disappears between the refusal and the join.
	maxAttempts = 3
)

// errNotActive means the Page a Trigger tried to join is no longer active.
var errNotActive = errors.New("the Page is no longer active")

// Receiver accepts Alertmanager webhook payloads and turns opted-in alerts
// into Pages. It creates Pages and records repeat Triggers through the
// same paths as every other writer, so the duplicate-refusal webhook applies.
type Receiver struct {
	// Client creates Pages.
	Client client.Client
	// Store records repeat Triggers in the status of the Active Page.
	Store *page.StatusStore
	// Namespace is the namespace that holds Pages.
	Namespace string
	// Namespaces are the namespaces whose alerts may start Pages. An empty list accepts none.
	Namespaces []string
	// Token reads the expected bearer token for each request, so Secret rotation takes effect.
	Token func() (string, error)
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
// are outside the allowlist, or that name no single Target are ignored.
func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Token == nil {
		http.Error(w, "alert authentication unavailable", http.StatusServiceUnavailable)
		return
	}
	expected, err := r.Token()
	if err != nil || expected == "" {
		http.Error(w, "alert authentication unavailable", http.StatusServiceUnavailable)
		return
	}
	provided, bearer := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	want, got := sha256.Sum256([]byte(expected)), sha256.Sum256([]byte(provided))
	if !bearer || subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
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
		target, ok := r.target(ctx, a)
		if !ok {
			continue
		}
		if err := r.trigger(ctx, target, a); err != nil {
			logf.FromContext(ctx).Error(err, "record the Trigger of an alert", "target", target)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

// target returns the Target an alert names, and false when the alert must
// be ignored.
func (r *Receiver) target(ctx context.Context, a alert) (v1alpha1.TargetReference, bool) {
	if a.Status != firingStatus || a.Labels[OptInLabel] != optInValue {
		return v1alpha1.TargetReference{}, false
	}
	namespace := a.Labels[labelNamespace]
	if !slices.Contains(r.Namespaces, namespace) {
		return v1alpha1.TargetReference{}, false
	}
	deployment, statefulSet := a.Labels[labelDeployment], a.Labels[labelStatefulSet]
	switch {
	case deployment != "" && statefulSet == "":
		return v1alpha1.TargetReference{Kind: v1alpha1.TargetKindDeployment, Namespace: namespace, Name: deployment}, true
	case statefulSet != "" && deployment == "":
		return v1alpha1.TargetReference{
			Kind: v1alpha1.TargetKindStatefulSet, Namespace: namespace, Name: statefulSet,
		}, true
	default:
		logf.FromContext(ctx).Info("ignore an opted-in alert that does not name exactly one Target",
			"namespace", namespace, "fingerprint", a.Fingerprint)
		return v1alpha1.TargetReference{}, false
	}
}

// trigger starts a Page for the Target or, when the Target already has an
// Active Page for the alert's Fingerprint, records the alert as a repeat
// Trigger of it. The chart installs no admission webhook, so the receiver
// selects the canonical Active Page itself and the controller repairs a race.
func (r *Receiver) trigger(ctx context.Context, target v1alpha1.TargetReference, a alert) error {
	fingerprint := v1alpha1.Fingerprint(target, a.Fingerprint)
	for range maxAttempts {
		name, found, err := r.activePage(ctx, fingerprint, target)
		if err != nil {
			return err
		}
		if found {
			err = r.join(ctx, name)
			if errors.Is(err, errNotActive) || apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		err = r.Client.Create(ctx, r.newPage(target, a))
		if err == nil {
			return nil
		}
		if apierrors.IsInvalid(err) {
			// The alert cannot be represented as a Page, and retrying it would not help.
			logf.FromContext(ctx).Info("ignore an alert that is not a valid Trigger", "reason", err.Error())
			return nil
		}
		return fmt.Errorf("create Page: %w", err)
	}
	return fmt.Errorf("the Active Page of %s/%s ended during %d attempts to join it",
		target.Namespace, target.Name, maxAttempts)
}

// activePage returns the canonical Active Page that the alert joins: the one for
// its Fingerprint first, and otherwise the Target's Active Page, because an
// alert carries no container or reason and therefore may not share a failure
// Fingerprint with the watcher's Page for the same fault.
func (r *Receiver) activePage(
	ctx context.Context, fingerprint string, target v1alpha1.TargetReference,
) (string, bool, error) {
	reader := r.Store.Reader
	if reader == nil {
		reader = r.Client
	}
	canonical, _, err := page.CanonicalActive(ctx, reader, r.Namespace, fingerprint)
	if err != nil {
		return "", false, fmt.Errorf("read the Active Page of the alert: %w", err)
	}
	if canonical != nil {
		return canonical.Name, true, nil
	}
	var all v1alpha1.PageList
	if err := reader.List(ctx, &all, client.InNamespace(r.Namespace)); err != nil {
		return "", false, fmt.Errorf("list Pages: %w", err)
	}
	for i := range all.Items {
		if all.Items[i].Spec.Target == target && all.Items[i].Status.Phase.IsActive() &&
			all.Items[i].DeletionTimestamp.IsZero() {
			return all.Items[i].Name, true, nil
		}
	}
	return "", false, nil
}

// newPage returns the Page that an alert starts.
func (r *Receiver) newPage(target v1alpha1.TargetReference, a alert) *v1alpha1.Page {
	return &v1alpha1.Page{
		GenerateName: "page-", Namespace: r.Namespace,
		Spec: v1alpha1.PageSpec{
			Target:      target,
			Fingerprint: v1alpha1.Fingerprint(target, a.Fingerprint),
			Trigger: v1alpha1.Trigger{
				Source:           v1alpha1.TriggerSourceAlertmanager,
				Pod:              a.Labels[labelPod],
				AlertFingerprint: a.Fingerprint,
			},
		},
	}
}

// join counts one more Trigger for the named Page, which must be active.
func (r *Receiver) join(ctx context.Context, name string) error {
	_, err := r.Store.Update(ctx, types.NamespacedName{Namespace: r.Namespace, Name: name},
		func(latest *v1alpha1.Page) error {
			if latest.Status.Phase.IsTerminal() {
				return errNotActive
			}
			latest.Status.TriggerCount++
			return nil
		})
	return err
}
