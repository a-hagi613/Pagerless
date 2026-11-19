package server_test

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/cooldown"
	"github.com/a-hagi613/pagerless/internal/testenv"
	"github.com/a-hagi613/pagerless/internal/watcher"
)

// watcherPoll bounds the watcher's own loops, so a test that steps the fake
// clock does not wait for a real minute for the evaluation pass.
const watcherPoll = 100 * time.Millisecond

// Values the watcher tests share, so the same object is spelled one way.
const (
	workloadLabel         = "component"
	containerName         = "main"
	appsAPIVersion        = "apps/v1"
	kindDeployment        = "Deployment"
	terminationError      = "Error"
	checkoutImage         = "registry.invalid/checkout:test"
	brokenAgentDeployment = "pagerless-agent-broken"
)

// checkoutWorkload is the Deployment, ReplicaSet, and crashing Pod a test
// creates, each owned by the one above it.
type checkoutWorkload struct {
	Deployment *appsv1.Deployment
	ReplicaSet *appsv1.ReplicaSet
}

// deployCheckout creates the owner chain of one Deployment without a controller,
// because envtest runs no deployment controller.
func deployCheckout(t *testing.T, env *testenv.Env) *checkoutWorkload {
	t.Helper()
	ctx := t.Context()
	labels := map[string]string{workloadLabel: checkout}
	one := int32(1)
	deployment := &appsv1.Deployment{
		Name:      checkout,
		Namespace: testenv.SandboxNamespace,
		Labels:    labels,
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				Labels: labels,
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: containerName, Image: checkoutImage,
					}},
				},
			},
		},
	}
	if err := env.Client.Create(ctx, deployment); err != nil {
		t.Fatalf("create the Deployment: %v", err)
	}
	controller := true
	replicaSet := &appsv1.ReplicaSet{
		Name:      checkout + "-7d9f",
		Namespace: testenv.SandboxNamespace,
		Labels:    labels,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: appsAPIVersion, Kind: kindDeployment, Name: deployment.Name, UID: deployment.UID,
			Controller: &controller,
		}},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: deployment.Spec.Template,
		},
	}
	if err := env.Client.Create(ctx, replicaSet); err != nil {
		t.Fatalf("create the ReplicaSet: %v", err)
	}
	return &checkoutWorkload{Deployment: deployment, ReplicaSet: replicaSet}
}

// crashLoopingPod creates a Pod of the ReplicaSet and gives it the durable status
// of a container that restarts repeatedly without a terminal failure.
func crashLoopingPod(t *testing.T, env *testenv.Env, workload *checkoutWorkload, suffix string) *corev1.Pod {
	t.Helper()
	ctx := t.Context()
	controller := true
	pod := &corev1.Pod{
		Name:      workload.ReplicaSet.Name + "-" + suffix,
		Namespace: testenv.SandboxNamespace,
		Labels:    map[string]string{workloadLabel: checkout},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: appsAPIVersion, Kind: "ReplicaSet", Name: workload.ReplicaSet.Name,
			UID: workload.ReplicaSet.UID, Controller: &controller,
		}},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: containerName, Image: checkoutImage}},
		},
	}
	if err := env.Client.Create(ctx, pod); err != nil {
		t.Fatalf("create the Pod: %v", err)
	}
	pod.Status = corev1.PodStatus{
		Phase:      corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:         containerName,
			Image:        checkoutImage,
			Ready:        false,
			RestartCount: 5,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "CrashLoopBackOff", Message: "back-off 5m0s restarting failed container=app",
			}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason: terminationError, ExitCode: 1,
			}},
		}},
	}
	if err := env.Client.Status().Update(ctx, pod); err != nil {
		t.Fatalf("write the Pod status: %v", err)
	}
	return pod
}

// pagesOfCheckout lists the Pages the watcher opened for the checkout Deployment.
func pagesOfCheckout(t *testing.T, env *testenv.Env) []v1alpha1.Page {
	t.Helper()
	var list v1alpha1.PageList
	if err := env.Client.List(t.Context(), &list, client.InNamespace(testenv.Namespace)); err != nil {
		t.Fatalf("list Pages: %v", err)
	}
	var pages []v1alpha1.Page
	for _, candidate := range list.Items {
		if candidate.Status.Subject != nil && candidate.Status.Subject.Name == checkout &&
			candidate.Status.Subject.Kind == v1alpha1.TargetKindDeployment {
			pages = append(pages, candidate)
		}
	}
	return pages
}

// waitForCheckoutPages waits until the watcher opened want Pages for checkout.
func waitForCheckoutPages(t *testing.T, env *testenv.Env, want int, description string) []v1alpha1.Page {
	t.Helper()
	var pages []v1alpha1.Page
	err := wait.PollUntilContextTimeout(t.Context(), 50*time.Millisecond, 30*time.Second, true,
		func(context.Context) (bool, error) {
			pages = pagesOfCheckout(t, env)
			return len(pages) == want, nil
		})
	if err != nil {
		t.Fatalf("waiting until %s: pages = %d, want %d", description, len(pages), want)
	}
	return pages
}

// TestWatcherOpensOnePagePerCrashLoopAndBacksOffOnARecurrence is the watcher's
// end-to-end acceptance test against a real API server: one Page for a crash
// loop, no Page inside the Cooldown with a rising suppressed count, and a linked
// Page at the next backoff level once the Cooldown elapses.
func TestWatcherOpensOnePagePerCrashLoopAndBacksOffOnARecurrence(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, testenv.WithWatcher(watcher.Config{
		StatusRequeue:      watcherPoll,
		EvaluationInterval: watcherPoll,
	}))
	workload := deployCheckout(t, env)
	firstPod := crashLoopingPod(t, env, workload, "first")

	pages := waitForCheckoutPages(t, env, 1, "the first crash loop opens one Page")
	first := pages[0]
	if first.Labels[v1alpha1.LabelSubjectKey] == "" {
		t.Errorf("label %s is missing; the Active Page could not be found with one list",
			v1alpha1.LabelSubjectKey)
	}
	if first.Status.Severity != v1alpha1.SeverityCritical {
		t.Errorf("severity = %s, want critical for a single-replica Deployment with no ready replica",
			first.Status.Severity)
	}
	if first.Status.Cooldown == nil || first.Status.Cooldown.BackoffLevel != 0 {
		t.Fatalf("cooldown = %+v, want the first level", first.Status.Cooldown)
	}
	if first.Spec.PreviousPage != "" {
		t.Errorf("previousPage = %q, want none for the first failure", first.Spec.PreviousPage)
	}
	env.WaitForPhase(t, first.Name, v1alpha1.PhaseInvestigating)

	// A second crashing Pod of the same Deployment joins nothing new.
	secondPod := crashLoopingPod(t, env, workload, "second")
	time.Sleep(10 * watcherPoll)
	if pages := pagesOfCheckout(t, env); len(pages) != 1 {
		t.Fatalf("pages = %d after a second crashing Pod, want 1", len(pages))
	}

	// The failure clears from live status, so the Page closes.
	for _, pod := range []*corev1.Pod{firstPod, secondPod} {
		if err := env.Client.Delete(t.Context(), pod); err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("delete the Pod: %v", err)
		}
	}
	env.Clock.Step(6 * time.Minute)
	env.WaitForPhase(t, first.Name, v1alpha1.PhaseRecovered)

	// A Trigger inside the Cooldown opens no Page and is counted instead.
	crashLoopingPod(t, env, workload, "third")
	fingerprint := first.Status.PrimaryFingerprint
	store, err := cooldown.New(env.History.DB())
	if err != nil {
		t.Fatalf("open the cooldown store: %v", err)
	}
	err = wait.PollUntilContextTimeout(t.Context(), 50*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			record, found, err := store.Get(ctx, fingerprint)
			if err != nil || !found {
				return false, err
			}
			return record.SuppressedCount >= 1, nil
		})
	if err != nil {
		t.Fatalf("waiting for a suppressed Trigger inside the Cooldown: %v", err)
	}
	if pages := pagesOfCheckout(t, env); len(pages) != 1 {
		t.Fatalf("pages inside the Cooldown = %d, want 1", len(pages))
	}

	// Past the Cooldown the same failure opens a linked Page at the next level.
	env.Clock.Step(20 * time.Minute)
	pages = waitForCheckoutPages(t, env, 2, "the Recurrence opens a linked Page")
	var recurrence *v1alpha1.Page
	for index := range pages {
		if pages[index].Name != first.Name {
			recurrence = &pages[index]
		}
	}
	if recurrence == nil {
		t.Fatal("the Recurrence Page is missing")
	}
	if recurrence.Spec.PreviousPage != first.Name {
		t.Errorf("previousPage = %q, want the first Page %q", recurrence.Spec.PreviousPage, first.Name)
	}
	if recurrence.Status.Cooldown == nil || recurrence.Status.Cooldown.BackoffLevel != 1 {
		t.Errorf("cooldown = %+v, want the second level", recurrence.Status.Cooldown)
	}
	if recurrence.Status.SuppressedCount < 1 {
		t.Errorf("suppressed count = %d, want the accumulated suppression", recurrence.Status.SuppressedCount)
	}
	if recurrence.Status.Subject == nil || recurrence.Status.Subject.UID != first.Status.Subject.UID {
		t.Errorf("subject = %+v, want the same Deployment UID", recurrence.Status.Subject)
	}
}

// TestWatcherIgnoresItsOwnNamespace proves the self-exclusion guardrail: a
// failing object in the release namespace never opens a Page, because a broken
// agent Job would otherwise page on itself.
func TestWatcherIgnoresItsOwnNamespace(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, testenv.WithWatcher(watcher.Config{
		StatusRequeue:      watcherPoll,
		EvaluationInterval: watcherPoll,
	}))
	controller := true
	deployment := &appsv1.Deployment{
		Name:      brokenAgentDeployment,
		Namespace: testenv.Namespace,
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{workloadLabel: brokenAgentDeployment}},
			Template: corev1.PodTemplateSpec{
				Labels: map[string]string{workloadLabel: brokenAgentDeployment},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: containerName, Image: "registry.invalid/agent:test",
				}}},
			},
		},
	}
	if err := env.Client.Create(t.Context(), deployment); err != nil {
		t.Fatalf("create the Deployment: %v", err)
	}
	pod := &corev1.Pod{
		Name:      "pagerless-agent-broken-abc",
		Namespace: testenv.Namespace,
		Labels:    map[string]string{workloadLabel: brokenAgentDeployment},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: appsAPIVersion, Kind: kindDeployment, Name: deployment.Name, UID: deployment.UID,
			Controller: &controller,
		}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: containerName, Image: "registry.invalid/agent:test"}}},
	}
	if err := env.Client.Create(t.Context(), pod); err != nil {
		t.Fatalf("create the Pod: %v", err)
	}
	pod.Status = corev1.PodStatus{
		Phase: corev1.PodFailed,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: containerName, RestartCount: 5,
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason: terminationError, ExitCode: 1,
			}},
		}},
	}
	if err := env.Client.Status().Update(t.Context(), pod); err != nil {
		t.Fatalf("write the Pod status: %v", err)
	}
	// Give the watcher several evaluation passes to open a Page it must not open.
	time.Sleep(20 * watcherPoll)
	var list v1alpha1.PageList
	if err := env.Client.List(t.Context(), &list, client.InNamespace(testenv.Namespace)); err != nil {
		t.Fatalf("list Pages: %v", err)
	}
	for _, page := range list.Items {
		if page.Status.Subject != nil && page.Status.Subject.Namespace == testenv.Namespace {
			t.Fatalf("Page %s watches the release namespace", page.Name)
		}
	}
}
