package verification

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// DefaultWindow is how long the Target is observed for new restarts and OOMKills (ADR-0006).
const DefaultWindow = 5 * time.Minute

// WindowEnvironment names the variable that may shorten the observation window.
// ADR-0006 keeps five minutes for every deployment and every published run; the
// override exists only so the credential-free CI Replay jobs can check the
// pipeline quickly, and a live evaluation never sets it.
const WindowEnvironment = "PAGERLESS_VERIFICATION_WINDOW"

// WindowFromEnvironment returns the observation window that the environment
// selects, or DefaultWindow when the variable is unset or empty.
func WindowFromEnvironment(getenv func(string) string) (time.Duration, error) {
	raw := strings.TrimSpace(getenv(WindowEnvironment))
	if raw == "" {
		return DefaultWindow, nil
	}
	window, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s is not a duration: %w", WindowEnvironment, err)
	}
	if window <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration, got %q", WindowEnvironment, raw)
	}
	return window, nil
}

// DefaultApplication is the Argo CD application that deploys the Target
// Repository when the operator configures the GitOps check without naming one.
const DefaultApplication = "pagerless"

// maxMessage bounds the message of a check.
const maxMessage = 500

// maxListedRestarts bounds how many restarted containers a message names.
const maxListedRestarts = 3

// Applications reads the status of Argo CD applications.
type Applications interface {
	// Application returns the named application's status.
	Application(ctx context.Context, name string) (*Application, error)
}

// Alerts reports whether an alert is still firing.
type Alerts interface {
	// Firing reports whether the alert with the fingerprint has not resolved.
	Firing(ctx context.Context, fingerprint string) (bool, error)
}

// Verifier runs the deterministic checks of Verification. Every check is a pure function of what the cluster,
// Argo CD, and Alertmanager report, and none involves the model (ADR-0006). A check that cannot read what it
// needs returns an error and no result, so that Verification retries rather than guessing.
type Verifier struct {
	// Reader reads Targets and their pods directly from the API server.
	Reader client.Reader
	// Applications reads Argo CD.
	Applications Applications
	// Alerts reads Alertmanager.
	Alerts Alerts
	// Application is the name of the Argo CD application that deploys the Target
	// Repository. It defaults to DefaultApplication.
	Application string
}

// Synced checks that the GitOps controller reports the application Synced and
// Healthy at the merge commit. A deployment that configures no GitOps
// controller passes the check with a message saying so: Verification then
// rests on the rollout, restart, and alert checks, which read the cluster.
func (v *Verifier) Synced(ctx context.Context, mergeCommit string) (v1alpha1.VerificationCheck, error) {
	if v.Applications == nil {
		return passed(v1alpha1.CheckArgoCDSynced, "skipped: no GitOps controller is configured"), nil
	}
	name := v.application()
	app, err := v.Applications.Application(ctx, name)
	if err != nil {
		return v1alpha1.VerificationCheck{}, err
	}
	if mergeCommit == "" {
		return failed(v1alpha1.CheckArgoCDSynced, "the pull request has no merge commit yet"), nil
	}
	return syncCheck(name, app, mergeCommit), nil
}

// application returns the name of the Argo CD application that deploys the Target Repository.
func (v *Verifier) application() string {
	if v.Application == "" {
		return DefaultApplication
	}
	return v.Application
}

// syncCheck reports the application's status, comparing its revision when there is one to compare.
func syncCheck(name string, app *Application, mergeCommit string) v1alpha1.VerificationCheck {
	switch {
	case app.SyncStatus != "Synced":
		return failed(v1alpha1.CheckArgoCDSynced, "Argo CD reports application %s %s", name, orUnknown(app.SyncStatus))
	case mergeCommit != "" && app.Revision != mergeCommit:
		return failed(v1alpha1.CheckArgoCDSynced, "Argo CD synced application %s to %s, not the merge commit %s",
			name, orUnknown(app.Revision), mergeCommit)
	case app.HealthStatus != "Healthy":
		return failed(v1alpha1.CheckArgoCDSynced, "Argo CD reports application %s %s", name, orUnknown(app.HealthStatus))
	case mergeCommit == "":
		return passed(v1alpha1.CheckArgoCDSynced, "application %s is Synced and Healthy", name)
	default:
		return passed(v1alpha1.CheckArgoCDSynced, "application %s is Synced and Healthy at %s", name, mergeCommit)
	}
}

// Rollout checks that the Target's rollout is complete and available: the controller has observed the latest
// generation, and every replica is updated and available.
func (v *Verifier) Rollout(ctx context.Context, ref v1alpha1.TargetReference) (v1alpha1.VerificationCheck, error) {
	key := client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}
	switch ref.Kind {
	case v1alpha1.TargetKindDeployment:
		var d appsv1.Deployment
		if err := v.Reader.Get(ctx, key, &d); err != nil {
			return missing(v1alpha1.CheckRolloutAvailable, ref, err)
		}
		return deploymentRollout(&d), nil
	case v1alpha1.TargetKindStatefulSet:
		var s appsv1.StatefulSet
		if err := v.Reader.Get(ctx, key, &s); err != nil {
			return missing(v1alpha1.CheckRolloutAvailable, ref, err)
		}
		return statefulSetRollout(&s), nil
	default:
		return v1alpha1.VerificationCheck{}, fmt.Errorf("unsupported Target kind %q", ref.Kind)
	}
}

func deploymentRollout(d *appsv1.Deployment) v1alpha1.VerificationCheck {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	s := d.Status
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Reason == "ProgressDeadlineExceeded" {
			return failed(v1alpha1.CheckRolloutAvailable, "the rollout exceeded its progress deadline")
		}
	}
	switch {
	case s.ObservedGeneration < d.Generation:
		return failed(v1alpha1.CheckRolloutAvailable,
			"the Deployment controller has not observed generation %d", d.Generation)
	case s.UpdatedReplicas != want || s.Replicas != want:
		return failed(v1alpha1.CheckRolloutAvailable,
			"%d of %d replicas are updated, and %d exist", s.UpdatedReplicas, want, s.Replicas)
	case s.AvailableReplicas != want:
		return failed(v1alpha1.CheckRolloutAvailable,
			"%d of %d replicas are available", s.AvailableReplicas, want)
	}
	return passed(v1alpha1.CheckRolloutAvailable, "%d of %d replicas are updated and available", want, want)
}

func statefulSetRollout(s *appsv1.StatefulSet) v1alpha1.VerificationCheck {
	want := int32(1)
	if s.Spec.Replicas != nil {
		want = *s.Spec.Replicas
	}
	st := s.Status
	switch {
	case st.ObservedGeneration < s.Generation:
		return failed(v1alpha1.CheckRolloutAvailable,
			"the StatefulSet controller has not observed generation %d", s.Generation)
	case st.UpdatedReplicas != want || st.Replicas != want:
		return failed(v1alpha1.CheckRolloutAvailable,
			"%d of %d replicas are updated, and %d exist", st.UpdatedReplicas, want, st.Replicas)
	case st.ReadyReplicas != want:
		return failed(v1alpha1.CheckRolloutAvailable,
			"%d of %d replicas are ready", st.ReadyReplicas, want)
	}
	return passed(v1alpha1.CheckRolloutAvailable, "%d of %d replicas are updated and ready", want, want)
}

// Restarts checks that no container of the Target's pods was terminated, restarted or OOMKilled, after the
// time. The kubelet records a container's last termination in the pod's status, so a restart during the
// observation remains visible after the container comes back, unless its pod was deleted in between.
func (v *Verifier) Restarts(
	ctx context.Context, ref v1alpha1.TargetReference, since time.Time,
) (v1alpha1.VerificationCheck, error) {
	selector, err := v.selector(ctx, ref)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return failed(v1alpha1.CheckNoNewRestarts, "%s %s/%s no longer exists", ref.Kind, ref.Namespace, ref.Name), nil
		}
		return v1alpha1.VerificationCheck{}, err
	}
	var pods corev1.PodList
	if err := v.Reader.List(ctx, &pods,
		client.InNamespace(ref.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return v1alpha1.VerificationCheck{},
			fmt.Errorf("list the pods of %s %s/%s: %w", ref.Kind, ref.Namespace, ref.Name, err)
	}
	var restarts []string
	for i := range pods.Items {
		restarts = append(restarts, terminationsSince(&pods.Items[i], since)...)
	}
	if len(restarts) > 0 {
		listed := restarts
		if len(listed) > maxListedRestarts {
			listed = listed[:maxListedRestarts]
		}
		message := fmt.Sprintf("%d container terminations since %s: %s",
			len(restarts), since.UTC().Format(time.RFC3339), strings.Join(listed, "; "))
		if len(restarts) > len(listed) {
			message += "; and more"
		}
		return failed(v1alpha1.CheckNoNewRestarts, "%s", message), nil
	}
	return passed(v1alpha1.CheckNoNewRestarts, "no container of %d pods restarted since %s",
		len(pods.Items), since.UTC().Format(time.RFC3339)), nil
}

// terminationsSince describes each container of the pod that terminated after the time. A termination that
// ended before the window began is the incident's own history, not a new failure.
func terminationsSince(pod *corev1.Pod, since time.Time) []string {
	var found []string
	statuses := pod.Status.ContainerStatuses
	for _, status := range statuses {
		for _, terminated := range []*corev1.ContainerStateTerminated{
			status.LastTerminationState.Terminated, status.State.Terminated,
		} {
			if terminated != nil && terminated.FinishedAt.After(since) {
				found = append(found, fmt.Sprintf("container %s of pod %s ended %s at %s",
					status.Name, pod.Name, orUnknown(terminated.Reason), terminated.FinishedAt.UTC().Format(time.RFC3339)))
				break
			}
		}
	}
	return found
}

// selector returns the label selector of the Target's pods.
func (v *Verifier) selector(ctx context.Context, ref v1alpha1.TargetReference) (labels.Selector, error) {
	return PodSelector(ctx, v.Reader, ref)
}

// PodSelector returns the label selector of the Target's pods.
func PodSelector(
	ctx context.Context, reader client.Reader, ref v1alpha1.TargetReference,
) (labels.Selector, error) {
	key := client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}
	var raw *metav1.LabelSelector
	switch ref.Kind {
	case v1alpha1.TargetKindDeployment:
		var d appsv1.Deployment
		if err := reader.Get(ctx, key, &d); err != nil {
			return nil, err
		}
		raw = d.Spec.Selector
	case v1alpha1.TargetKindStatefulSet:
		var s appsv1.StatefulSet
		if err := reader.Get(ctx, key, &s); err != nil {
			return nil, err
		}
		raw = s.Spec.Selector
	default:
		return nil, fmt.Errorf("unsupported Target kind %q", ref.Kind)
	}
	if raw == nil || (len(raw.MatchLabels) == 0 && len(raw.MatchExpressions) == 0) {
		// An empty selector would match every pod in the namespace.
		return nil, fmt.Errorf("%s %s/%s has an empty pod selector", ref.Kind, ref.Namespace, ref.Name)
	}
	selector, err := metav1.LabelSelectorAsSelector(raw)
	if err != nil {
		return nil, fmt.Errorf("convert the pod selector of %s %s/%s: %w", ref.Kind, ref.Namespace, ref.Name, err)
	}
	return selector, nil
}

// AlertRequired reports whether Verification must check that the triggering alert resolved. A Trigger from
// Alertmanager always requires it, and so does any other Trigger that carries an alert's fingerprint, such as a
// Follow-up Page of an alert's Page. Human Triggers have no alert to check.
func AlertRequired(trigger v1alpha1.Trigger) bool {
	return trigger.Source == v1alpha1.TriggerSourceAlertmanager || trigger.AlertFingerprint != ""
}

// Alert checks that the triggering alert has resolved in Alertmanager, found by its fingerprint. The check is
// skipped, and passes, for a Trigger that has no alert. An alert Trigger without a fingerprint cannot be
// confirmed, so the check fails.
func (v *Verifier) Alert(ctx context.Context, trigger v1alpha1.Trigger) (v1alpha1.VerificationCheck, error) {
	if !AlertRequired(trigger) {
		return passed(v1alpha1.CheckAlertResolved, "skipped: the Trigger is a human request, not an alert"), nil
	}
	if v.Alerts == nil {
		return passed(v1alpha1.CheckAlertResolved, "skipped: no Alertmanager is configured"), nil
	}
	if trigger.AlertFingerprint == "" {
		return failed(v1alpha1.CheckAlertResolved,
			"the Trigger records no alert fingerprint, so resolution cannot be confirmed"), nil
	}
	firing, err := v.Alerts.Firing(ctx, trigger.AlertFingerprint)
	if err != nil {
		return v1alpha1.VerificationCheck{}, err
	}
	if firing {
		return failed(v1alpha1.CheckAlertResolved, "alert %s is still firing in Alertmanager", trigger.AlertFingerprint), nil
	}
	return passed(v1alpha1.CheckAlertResolved, "alert %s has resolved in Alertmanager", trigger.AlertFingerprint), nil
}

// missing turns a missing Target into a failed check, and any other error into an error.
func missing(name string, ref v1alpha1.TargetReference, err error) (v1alpha1.VerificationCheck, error) {
	if apierrors.IsNotFound(err) {
		return failed(name, "%s %s/%s no longer exists", ref.Kind, ref.Namespace, ref.Name), nil
	}
	return v1alpha1.VerificationCheck{}, fmt.Errorf("get %s %s/%s: %w", ref.Kind, ref.Namespace, ref.Name, err)
}

func passed(name, format string, args ...any) v1alpha1.VerificationCheck {
	return check(name, true, format, args...)
}

func failed(name, format string, args ...any) v1alpha1.VerificationCheck {
	return check(name, false, format, args...)
}

func check(name string, ok bool, format string, args ...any) v1alpha1.VerificationCheck {
	message := fmt.Sprintf(format, args...)
	if len(message) > maxMessage {
		message = message[:maxMessage-3] + "..."
	}
	return v1alpha1.VerificationCheck{Name: name, Passed: ok, Message: message}
}

func orUnknown(s string) string {
	if s == "" {
		return "Unknown"
	}
	return s
}
