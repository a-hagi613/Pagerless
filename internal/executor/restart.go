package executor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/executorapi"
	"github.com/a-hagi613/pagerless/internal/verification"
)

// DefaultRestartTimeout bounds one live RestartWorkload.
const DefaultRestartTimeout = 2 * time.Minute

// restartPollInterval is how often the Executor checks for a replacement pod
// and for the Workload's availability.
const restartPollInterval = 100 * time.Millisecond

// restart carries out RestartWorkload live: it deletes the Workload's pods one
// at a time and waits for each replacement to become ready before deleting the
// next, so that the Workload keeps serving. Deleting pods changes no manifest,
// so Argo CD sees no drift and self-heal has nothing to revert (ADR-0002).
func (e *Executor) restart(ctx context.Context, tr *v1alpha1.Triage) (*executorapi.Response, error) {
	// A repeated request after the server recorded the result must not restart the Workload again.
	if execution := tr.Status.Execution; execution != nil && execution.ExecutedAt != nil {
		return &executorapi.Response{Live: true, RestartedPods: slices.Clone(execution.RestartedPods)}, nil
	}
	ref := tr.Spec.Workload
	pods, err := e.workloadPods(ctx, ref)
	if err != nil {
		return nil, err
	}
	sortPods(pods, ref.Kind)

	restarted := make([]string, 0, len(pods))
	for i := range pods {
		name := pods[i].Name
		current, err := e.workloadPods(ctx, ref)
		if err != nil {
			return nil, err
		}
		before := make(map[types.UID]struct{}, len(current))
		for j := range current {
			before[current[j].UID] = struct{}{}
		}
		at := slices.IndexFunc(current, func(pod corev1.Pod) bool { return pod.Name == name })
		if at < 0 {
			// The pod disappeared on its own, so there is nothing to restart.
			continue
		}
		pod := current[at]
		if err := e.cfg.Writer.Delete(ctx, &pod); err != nil && !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("delete pod %s: %w", name, err)
		}
		restarted = append(restarted, name)
		if err := e.waitForReplacement(ctx, ref, &pod, before); err != nil {
			return nil, err
		}
	}
	if err := e.waitForAvailable(ctx, ref); err != nil {
		return nil, err
	}
	return &executorapi.Response{Live: true, RestartedPods: restarted}, nil
}

// workloadPods returns the pods that the Workload selects.
func (e *Executor) workloadPods(ctx context.Context, ref v1alpha1.WorkloadReference) ([]corev1.Pod, error) {
	selector, err := verification.PodSelector(ctx, e.cfg.Reader, ref)
	if err != nil {
		return nil, err
	}
	var pods corev1.PodList
	if err := e.cfg.Reader.List(ctx, &pods,
		client.InNamespace(ref.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, fmt.Errorf("list the pods of %s %s/%s: %w", ref.Kind, ref.Namespace, ref.Name, err)
	}
	return pods.Items, nil
}

// sortPods orders the pods the way the Workload's controller replaces them: by
// ordinal for a StatefulSet, and by creation time for a Deployment.
func sortPods(pods []corev1.Pod, kind v1alpha1.WorkloadKind) {
	slices.SortFunc(pods, func(a, b corev1.Pod) int {
		if kind == v1alpha1.WorkloadKindStatefulSet {
			return strings.Compare(a.Name, b.Name)
		}
		if order := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); order != 0 {
			return order
		}
		return strings.Compare(a.Name, b.Name)
	})
}

// waitForReplacement waits until a pod that did not exist before the deletion is ready.
func (e *Executor) waitForReplacement(
	ctx context.Context, ref v1alpha1.WorkloadReference, deleted *corev1.Pod, before map[types.UID]struct{},
) error {
	err := wait.PollUntilContextTimeout(ctx, restartPollInterval, e.cfg.RestartTimeout, true,
		func(ctx context.Context) (bool, error) {
			pods, err := e.workloadPods(ctx, ref)
			if err != nil {
				return false, err
			}
			for i := range pods {
				pod := &pods[i]
				if pod.UID == deleted.UID {
					continue
				}
				if _, existed := before[pod.UID]; existed {
					continue
				}
				if podReady(pod) {
					return true, nil
				}
			}
			return false, nil
		})
	if err != nil {
		return fmt.Errorf("wait for the replacement of pod %s: %w", deleted.Name, err)
	}
	return nil
}

// waitForAvailable waits until the Workload's rollout is complete and every
// replica is available, using the same check that Verification applies.
func (e *Executor) waitForAvailable(ctx context.Context, ref v1alpha1.WorkloadReference) error {
	verifier := &verification.Verifier{Reader: e.cfg.Reader}
	var last v1alpha1.VerificationCheck
	err := wait.PollUntilContextTimeout(ctx, restartPollInterval, e.cfg.RestartTimeout, true,
		func(ctx context.Context) (bool, error) {
			check, err := verifier.Rollout(ctx, ref)
			if err != nil {
				return false, err
			}
			last = check
			return check.Passed, nil
		})
	if err != nil {
		return fmt.Errorf("wait for %s %s/%s to be available: %w: %s",
			ref.Kind, ref.Namespace, ref.Name, err, last.Message)
	}
	return nil
}

// podReady reports whether every container of the pod is ready.
func podReady(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	if len(pod.Status.ContainerStatuses) == 0 {
		return false
	}
	for _, status := range pod.Status.ContainerStatuses {
		if !status.Ready {
			return false
		}
	}
	return true
}
