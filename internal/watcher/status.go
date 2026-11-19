package watcher

import (
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// ObserveConfig tunes the durable-status observers. The zero value uses the
// catalog's own settle windows and the specified restart and pending thresholds.
type ObserveConfig struct {
	// RestartThreshold is the restart count that itself is a restart storm.
	RestartThreshold int32
	// PendingPodTimeout is how long a Pod may stay Pending without a more specific reason.
	PendingPodTimeout time.Duration
	// CronFailureWindow is how recent a failed Job must be to count against its CronJob.
	CronFailureWindow time.Duration
	// CronSuccessFloor is the minimum staleness of lastSuccessfulTime that counts as failing.
	CronSuccessFloor time.Duration
}

// withDefaults fills the specified thresholds.
func (o ObserveConfig) withDefaults() ObserveConfig {
	if o.RestartThreshold <= 0 {
		o.RestartThreshold = 3
	}
	if o.PendingPodTimeout <= 0 {
		o.PendingPodTimeout = 10 * time.Minute
	}
	if o.CronFailureWindow <= 0 {
		o.CronFailureWindow = 30 * time.Minute
	}
	if o.CronSuccessFloor <= 0 {
		o.CronSuccessFloor = 3 * time.Hour
	}
	return o
}

// podReference returns the Reference a Pod's signals name.
func podReference(pod *corev1.Pod) Reference {
	return Reference{
		APIVersion: apiVersionCore,
		Kind:       string(v1alpha1.TargetKindPod),
		Namespace:  pod.Namespace,
		Name:       pod.Name,
		UID:        string(pod.UID),
	}
}

// podSignal builds a Pod-scoped signal.
func podSignal(pod *corev1.Pod, class ReasonClass, scope, note string, now time.Time) Signal {
	return Signal{
		Reference: podReference(pod),
		Class:     class,
		Scope:     scope,
		Note:      note,
		Pod:       pod.Name,
		Source:    v1alpha1.TriggerSourceCondition,
		Reason:    string(class),
		FirstSeen: now,
	}
}

// objectSignal builds a signal for a controller object.
func objectSignal(reference Reference, class ReasonClass, note string, firstSeen time.Time) Signal {
	return Signal{
		Reference: reference,
		Class:     class,
		Source:    v1alpha1.TriggerSourceCondition,
		Reason:    string(class),
		Note:      note,
		FirstSeen: firstSeen,
	}
}

// PodSignals returns the live failure signals of a Pod. It reads only durable
// fields — waiting and terminated reasons, restart counts, phase, and conditions
// — because CrashLoopBackOff is a kubectl display string, not a phase.
func PodSignals(pod *corev1.Pod, now time.Time, cfg ObserveConfig) []Signal {
	cfg = cfg.withDefaults()
	var signals []Signal
	for _, status := range pod.Status.InitContainerStatuses {
		signals = appendContainerSignals(signals, pod, status, "init/"+status.Name, now)
	}
	for _, status := range pod.Status.ContainerStatuses {
		signals = appendContainerSignals(signals, pod, status, status.Name, now)
	}
	// A restart storm is a container that keeps restarting without a waiting
	// reason: a container already in a specific waiting state is reported once.
	if len(signals) == 0 {
		for _, status := range pod.Status.ContainerStatuses {
			if status.RestartCount < cfg.RestartThreshold {
				continue
			}
			signals = appendSignal(signals, podSignal(pod, ReasonPodRestartStorm, status.Name,
				"the container restarts repeatedly", now))
		}
	}

	switch pod.Status.Phase {
	case corev1.PodPending:
		if len(signals) == 0 {
			firstSeen := pod.CreationTimestamp.Time
			if pod.Status.StartTime != nil {
				firstSeen = pod.Status.StartTime.Time
			}
			if !firstSeen.IsZero() && now.Sub(firstSeen) >= cfg.PendingPodTimeout {
				signal := podSignal(pod, ReasonPodPending, "", "the Pod stays Pending", now)
				signal.FirstSeen = firstSeen
				signals = append(signals, signal)
			}
		}
		if condition := podCondition(pod, corev1.PodScheduled); condition != nil &&
			condition.Status == corev1.ConditionFalse && condition.Reason == corev1.PodReasonUnschedulable {
			signal := podSignal(pod, ReasonSchedulingFailed, "", condition.Message, now)
			signal.FirstSeen = podConditionTime(condition, now)
			signals = appendSignal(signals, signal)
		}
	case corev1.PodFailed:
		switch pod.Status.Reason {
		case "Evicted":
			signals = appendSignal(signals, podSignal(pod, ReasonPodEvicted, "", pod.Status.Message, now))
		case "Shutdown", "UnexpectedAdmissionError":
			signals = appendSignal(signals, podSignal(pod, ReasonPodFailed, "", pod.Status.Message, now))
		}
		if condition := podCondition(pod, corev1.DisruptionTarget); condition != nil &&
			condition.Status == corev1.ConditionTrue {
			signal := podSignal(pod, ReasonPodFailed, "", condition.Message, now)
			signal.FirstSeen = podConditionTime(condition, now)
			signals = appendSignal(signals, signal)
		}
	}
	return signals
}

// appendContainerSignals adds the signals one container status shows.
func appendContainerSignals(signals []Signal, pod *corev1.Pod, status corev1.ContainerStatus,
	scope string, now time.Time,
) []Signal {
	if waiting := status.State.Waiting; waiting != nil {
		switch waiting.Reason {
		case "CrashLoopBackOff":
			signal := podSignal(pod, ReasonPodCrashLoop, scope, waiting.Message, now)
			// A third restart with a terminated container has already lasted long
			// enough; waiting another minute would only delay a certain crash loop.
			signal.Immediate = status.RestartCount >= 3 && status.LastTerminationState.Terminated != nil
			signals = appendSignal(signals, signal)
		case "ImagePullBackOff", "ErrImagePull":
			signals = appendSignal(signals, podSignal(pod, ReasonPodImagePull, scope, waiting.Message, now))
		case "InvalidImageName", "ErrImageNeverPull", "ImageInspectError":
			signals = appendSignal(signals, podSignal(pod, ReasonPodInvalidImage, scope, waiting.Message, now))
		case "CreateContainerConfigError":
			signals = appendSignal(signals, podSignal(pod, ReasonPodConfigError, scope, waiting.Message, now))
		case "CreateContainerError", "RunContainerError":
			signals = appendSignal(signals, podSignal(pod, ReasonPodCreateError, scope, waiting.Message, now))
		}
	}
	for _, terminated := range []*corev1.ContainerStateTerminated{
		status.LastTerminationState.Terminated,
		status.State.Terminated,
	} {
		if terminated == nil || terminated.Reason != "OOMKilled" {
			continue
		}
		signals = appendSignal(signals,
			podSignal(pod, ReasonPodOOMKilled, scope, "the container was killed for memory", now))
		break
	}
	return signals
}

// appendSignal adds a signal unless the same class and scope is already present,
// so a Pod that shows one failure through two fields reports it once.
func appendSignal(signals []Signal, signal Signal) []Signal {
	for _, existing := range signals {
		if existing.Class == signal.Class && existing.Scope == signal.Scope {
			return signals
		}
	}
	return append(signals, signal)
}

// PodAvailability is a Pod's replica health: one desired and one ready while its
// Ready condition is true.
func PodAvailability(pod *corev1.Pod) Availability {
	ready := int32(0)
	if condition := podCondition(pod, corev1.PodReady); condition != nil &&
		condition.Status == corev1.ConditionTrue {
		ready = 1
	}
	return Availability{Desired: 1, Ready: ready}
}

// DeploymentSignals returns the live failure signals of a Deployment. A rollout
// that is still inside its progress deadline is not a failure, so only the
// controller's own condition reasons page.
func DeploymentSignals(deployment *appsv1.Deployment, now time.Time) []Signal {
	reference := Reference{
		APIVersion: apiVersionApps,
		Kind:       string(v1alpha1.TargetKindDeployment),
		Namespace:  deployment.Namespace,
		Name:       deployment.Name,
		UID:        string(deployment.UID),
	}
	var signals []Signal
	for _, condition := range deployment.Status.Conditions {
		switch {
		case condition.Type == appsv1.DeploymentProgressing &&
			condition.Reason == "ProgressDeadlineExceeded":
			signals = append(signals, objectSignal(reference, ReasonRolloutProgressDeadline,
				condition.Message, condition.LastTransitionTime.Time))
		case condition.Type == appsv1.DeploymentReplicaFailure && condition.Status == corev1.ConditionTrue:
			signals = append(signals, objectSignal(reference, ReasonRolloutReplicaFailure,
				condition.Message, condition.LastTransitionTime.Time))
		}
	}
	return signals
}

// DeploymentAvailability is a Deployment's replica health.
func DeploymentAvailability(deployment *appsv1.Deployment) Availability {
	desired := int32(1)
	if deployment.Spec.Replicas != nil {
		desired = *deployment.Spec.Replicas
	}
	return Availability{Desired: desired, Ready: deployment.Status.ReadyReplicas}
}

// StatefulSetSignals returns the live failure signals of a StatefulSet. Older
// clusters carry no condition for a stuck partition, so the replica and revision
// math is the signal.
func StatefulSetSignals(statefulSet *appsv1.StatefulSet, now time.Time) []Signal {
	desired := int32(1)
	if statefulSet.Spec.Replicas != nil {
		desired = *statefulSet.Spec.Replicas
	}
	partition := int32(0)
	if rolling := statefulSet.Spec.UpdateStrategy.RollingUpdate; rolling != nil && rolling.Partition != nil {
		partition = *rolling.Partition
	}
	stuck := statefulSet.Status.CurrentRevision != statefulSet.Status.UpdateRevision ||
		statefulSet.Status.UpdatedReplicas < desired-partition ||
		statefulSet.Status.ReadyReplicas < desired
	if !stuck {
		return nil
	}
	return []Signal{objectSignal(Reference{
		APIVersion: apiVersionApps,
		Kind:       string(v1alpha1.TargetKindStatefulSet),
		Namespace:  statefulSet.Namespace,
		Name:       statefulSet.Name,
		UID:        string(statefulSet.UID),
	}, ReasonRolloutStatefulSetStuck, "the StatefulSet has not finished its rollout", now)}
}

// StatefulSetAvailability is a StatefulSet's replica health.
func StatefulSetAvailability(statefulSet *appsv1.StatefulSet) Availability {
	desired := int32(1)
	if statefulSet.Spec.Replicas != nil {
		desired = *statefulSet.Spec.Replicas
	}
	return Availability{Desired: desired, Ready: statefulSet.Status.ReadyReplicas}
}

// DaemonSetSignals returns the live failure signals of a DaemonSet. Node-local
// agents fail per node, so numberUnavailable is the authoritative count.
func DaemonSetSignals(daemonSet *appsv1.DaemonSet, now time.Time) []Signal {
	if daemonSet.Status.NumberUnavailable == 0 ||
		daemonSet.Status.ObservedGeneration != daemonSet.Generation {
		return nil
	}
	return []Signal{objectSignal(Reference{
		APIVersion: apiVersionApps,
		Kind:       string(v1alpha1.TargetKindDaemonSet),
		Namespace:  daemonSet.Namespace,
		Name:       daemonSet.Name,
		UID:        string(daemonSet.UID),
	}, ReasonRolloutDaemonSetUnavailable, "the DaemonSet has unavailable nodes", now)}
}

// DaemonSetAvailability is a DaemonSet's node coverage.
func DaemonSetAvailability(daemonSet *appsv1.DaemonSet) Availability {
	return Availability{
		Desired: daemonSet.Status.DesiredNumberScheduled,
		Ready:   daemonSet.Status.NumberReady,
	}
}

// JobSignals returns the live failure signals of a Job. A Job owned by a CronJob
// canonicalizes to the CronJob, so seven nightly failures are one Page.
func JobSignals(job *batchv1.Job, now time.Time) []Signal {
	reference := Reference{
		APIVersion: apiVersionBatch,
		Kind:       string(v1alpha1.TargetKindJob),
		Namespace:  job.Namespace,
		Name:       job.Name,
		UID:        string(job.UID),
	}
	for _, condition := range job.Status.Conditions {
		if condition.Type != batchv1.JobFailed || condition.Status != corev1.ConditionTrue {
			continue
		}
		return []Signal{objectSignal(reference, ReasonJobBackoffLimit, condition.Message,
			condition.LastTransitionTime.Time)}
	}
	if job.Spec.BackoffLimit != nil && job.Status.Failed > *job.Spec.BackoffLimit {
		return []Signal{objectSignal(reference, ReasonJobBackoffLimit,
			"the Job exhausted its backoff limit", now)}
	}
	return nil
}

// JobAvailability is a Job's single-replica health: complete is ready.
func JobAvailability(job *batchv1.Job) Availability {
	ready := int32(0)
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
			ready = 1
		}
	}
	return Availability{Desired: 1, Ready: ready}
}

// CronJobSignals returns the live failure signals of a CronJob, derived from the
// Jobs it owns. A CronJob has no failure condition of its own.
func CronJobSignals(cronJob *batchv1.CronJob, jobs []batchv1.Job, now time.Time,
	cfg ObserveConfig,
) []Signal {
	cfg = cfg.withDefaults()
	if cronJob.Spec.Suspend != nil && *cronJob.Spec.Suspend {
		return nil
	}
	failed := 0
	for i := range jobs {
		if jobFailed(&jobs[i]) && jobWithin(&jobs[i], now, cfg.CronFailureWindow) {
			failed++
		}
	}
	failing := failed >= 3
	if !failing && cronJob.Status.LastSuccessfulTime != nil {
		failing = now.Sub(cronJob.Status.LastSuccessfulTime.Time) > cfg.CronSuccessFloor
	}
	if !failing {
		return nil
	}
	return []Signal{objectSignal(Reference{
		APIVersion: apiVersionBatch,
		Kind:       string(v1alpha1.TargetKindCronJob),
		Namespace:  cronJob.Namespace,
		Name:       cronJob.Name,
		UID:        string(cronJob.UID),
	}, ReasonJobCronFailing, "the CronJob's recent runs failed", now)}
}

// jobFailed reports whether a Job ended as failed.
func jobFailed(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// jobWithin reports whether the Job was created inside the window.
func jobWithin(job *batchv1.Job, now time.Time, window time.Duration) bool {
	return now.Sub(job.CreationTimestamp.Time) <= window
}

// NodeSignals returns the live failure signals of a Node. A Node NotReady is
// critical because it is a node-level outage; the pressure conditions are
// warnings.
func NodeSignals(node *corev1.Node, now time.Time) []Signal {
	reference := Reference{
		APIVersion: apiVersionCore,
		Kind:       string(v1alpha1.TargetKindNode),
		Name:       node.Name,
		UID:        string(node.UID),
	}
	var signals []Signal
	for _, condition := range node.Status.Conditions {
		class, isPressure := nodePressureClass(condition.Type)
		switch {
		case condition.Type == corev1.NodeReady && condition.Status != corev1.ConditionTrue:
			signals = append(signals, objectSignal(reference, ReasonNodeNotReady,
				condition.Message, condition.LastTransitionTime.Time))
		case isPressure && condition.Status == corev1.ConditionTrue:
			signals = append(signals, Signal{
				Reference: reference,
				Class:     class,
				Scope:     string(condition.Type),
				Source:    v1alpha1.TriggerSourceCondition,
				Reason:    string(class),
				Note:      condition.Message,
				FirstSeen: condition.LastTransitionTime.Time,
			})
		}
	}
	return signals
}

// nodePressureClass maps a Node condition type to its reason class.
func nodePressureClass(condition corev1.NodeConditionType) (ReasonClass, bool) {
	switch condition {
	case corev1.NodeMemoryPressure:
		return ReasonNodeMemoryPressure, true
	case corev1.NodeDiskPressure:
		return ReasonNodeDiskPressure, true
	case corev1.NodePIDPressure:
		return ReasonNodePIDPressure, true
	case corev1.NodeNetworkUnavailable:
		return ReasonNodeNetworkUnavailable, true
	default:
		return "", false
	}
}

// NodeAvailability is a Node's single-replica health.
func NodeAvailability(node *corev1.Node) Availability {
	ready := int32(0)
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			ready = 1
		}
	}
	return Availability{Desired: 1, Ready: ready}
}

// PersistentVolumeClaimSignals returns the live failure signals of a claim. A
// claim has no conditions: its phase is the signal.
func PersistentVolumeClaimSignals(claim *corev1.PersistentVolumeClaim, now time.Time) []Signal {
	if claim.Status.Phase != corev1.ClaimPending && claim.Status.Phase != corev1.ClaimLost {
		return nil
	}
	return []Signal{objectSignal(Reference{
		APIVersion: apiVersionCore,
		Kind:       string(v1alpha1.TargetKindPersistentVolumeClaim),
		Namespace:  claim.Namespace,
		Name:       claim.Name,
		UID:        string(claim.UID),
	}, ReasonStoragePVCProvisioning, "the claim is "+string(claim.Status.Phase), now)}
}

// PersistentVolumeClaimAvailability is a claim's single-replica health: Bound is ready.
func PersistentVolumeClaimAvailability(claim *corev1.PersistentVolumeClaim) Availability {
	ready := int32(0)
	if claim.Status.Phase == corev1.ClaimBound {
		ready = 1
	}
	return Availability{Desired: 1, Ready: ready}
}

// podCondition returns one Pod condition, or nil.
func podCondition(pod *corev1.Pod, conditionType corev1.PodConditionType) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == conditionType {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}

// podConditionTime returns a condition's transition time, or now when it has none.
func podConditionTime(condition *corev1.PodCondition, now time.Time) time.Time {
	if condition.LastTransitionTime.IsZero() {
		return now
	}
	return condition.LastTransitionTime.Time
}
