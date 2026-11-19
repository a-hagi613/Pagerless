package watcher

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// observedAt is the fixed instant the status tests read.
var observedAt = time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)

// classesOf returns the reason classes of a signal set.
func classesOf(signals []Signal) []ReasonClass {
	classes := make([]ReasonClass, 0, len(signals))
	for _, signal := range signals {
		classes = append(classes, signal.Class)
	}
	return classes
}

// hasClass reports whether a signal set carries a class.
func hasClass(signals []Signal, class ReasonClass) bool {
	for _, signal := range signals {
		if signal.Class == class {
			return true
		}
	}
	return false
}

func TestPodSignalsReadWaitingAndLastTerminatedState(t *testing.T) {
	t.Parallel()
	pod := &corev1.Pod{
		Namespace: testNamespace,
		Name:      testPodName,
		UID:       testPodUID,
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: testScope,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "CrashLoopBackOff",
				}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					Reason: "OOMKilled",
				}},
				RestartCount: 5,
			}},
		},
	}
	signals := PodSignals(pod, observedAt, ObserveConfig{})
	if !hasClass(signals, ReasonPodCrashLoop) || !hasClass(signals, ReasonPodOOMKilled) {
		t.Fatalf("classes = %v, want the crash loop and the OOM kill", classesOf(signals))
	}
	for _, signal := range signals {
		if signal.Class == ReasonPodCrashLoop && !signal.Immediate {
			t.Error("a third restart with a terminated container must bypass the settle window")
		}
		if signal.Scope != testScope {
			t.Errorf("scope = %q, want the container name", signal.Scope)
		}
	}
}

func TestPodSignalsNameTheRemainingDurableStates(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		pod   corev1.Pod
		class ReasonClass
	}{
		"image pull": {
			pod: corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name: testScope, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "ImagePullBackOff",
				}},
			}}}},
			class: ReasonPodImagePull,
		},
		"missing config": {
			pod: corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name: testScope, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "CreateContainerConfigError",
				}},
			}}}},
			class: ReasonPodConfigError,
		},
		"unschedulable": {
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				Conditions: []corev1.PodCondition{{
					Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
					LastTransitionTime: metav1.NewTime(observedAt.Add(-time.Minute)),
				}},
			}},
			class: ReasonSchedulingFailed,
		},
		"evicted": {
			pod:   corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted"}},
			class: ReasonPodEvicted,
		},
		"restart storm": {
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{{
					Name: testScope, RestartCount: 3,
				}},
			}},
			class: ReasonPodRestartStorm,
		},
	}
	for name, testCase := range cases {
		pod := testCase.pod
		pod.Namespace, pod.Name, pod.UID = testNamespace, testPodName, testPodUID
		signals := PodSignals(&pod, observedAt, ObserveConfig{})
		if !hasClass(signals, testCase.class) {
			t.Errorf("%s: classes = %v, want %s", name, classesOf(signals), testCase.class)
		}
	}
}

func TestPodSignalsNeedThePendingTimeout(t *testing.T) {
	t.Parallel()
	pod := &corev1.Pod{
		Namespace:         testNamespace,
		Name:              testPodName,
		UID:               testPodUID,
		CreationTimestamp: metav1.NewTime(observedAt.Add(-time.Minute)),
		Status:            corev1.PodStatus{Phase: corev1.PodPending},
	}
	if signals := PodSignals(pod, observedAt, ObserveConfig{}); len(signals) != 0 {
		t.Fatalf("a Pod pending for one minute produced %v, want nothing", classesOf(signals))
	}
	pod.CreationTimestamp = metav1.NewTime(observedAt.Add(-ObserveConfig{}.withDefaults().PendingPodTimeout))
	if signals := PodSignals(pod, observedAt, ObserveConfig{}); !hasClass(signals, ReasonPodPending) {
		t.Fatalf("classes = %v, want the pending reason class", classesOf(signals))
	}
}

func TestDeploymentSignalsReadTheConditions(t *testing.T) {
	t.Parallel()
	deploymentReplicas := int32(3)
	deployment := &appsv1.Deployment{
		Namespace: testNamespace,
		Name:      testDeployment,
		UID:       testDeploymentUID,
		Spec:      appsv1.DeploymentSpec{Replicas: &deploymentReplicas},
		Status: appsv1.DeploymentStatus{
			Replicas: 3, ReadyReplicas: 2,
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentProgressing, Reason: "ProgressDeadlineExceeded"},
				{Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue},
			},
		},
	}
	signals := DeploymentSignals(deployment, observedAt)
	if !hasClass(signals, ReasonRolloutProgressDeadline) || !hasClass(signals, ReasonRolloutReplicaFailure) {
		t.Fatalf("classes = %v, want both deployment conditions", classesOf(signals))
	}
	if got := DeploymentAvailability(deployment); got.Desired != 3 || got.Ready != 2 {
		t.Fatalf("availability = %+v, want 3 desired and 2 ready", got)
	}
}

func TestStatefulSetAndDaemonSetSignalsReadReplicaMath(t *testing.T) {
	t.Parallel()
	replicas := int32(3)
	statefulSet := &appsv1.StatefulSet{
		Namespace: testNamespace,
		Name:      "db",
		UID:       "s-1",
		Spec:      appsv1.StatefulSetSpec{Replicas: &replicas},
		Status: appsv1.StatefulSetStatus{
			CurrentRevision: "rev-1", UpdateRevision: "rev-2", ReadyReplicas: 1, UpdatedReplicas: 1,
		},
	}
	if signals := StatefulSetSignals(statefulSet, observedAt); !hasClass(signals, ReasonRolloutStatefulSetStuck) {
		t.Fatalf("classes = %v, want a stuck rollout", classesOf(signals))
	}
	statefulSet.Status.CurrentRevision = "rev-2"
	statefulSet.Status.ReadyReplicas = 3
	statefulSet.Status.UpdatedReplicas = 3
	if signals := StatefulSetSignals(statefulSet, observedAt); len(signals) != 0 {
		t.Fatalf("classes = %v, want nothing for a finished rollout", classesOf(signals))
	}

	daemonSet := &appsv1.DaemonSet{
		Namespace:  testNamespace,
		Name:       "agent",
		UID:        "ds-1",
		Generation: 2,
		Status: appsv1.DaemonSetStatus{
			ObservedGeneration: 2, DesiredNumberScheduled: 3, NumberReady: 2, NumberUnavailable: 1,
		},
	}
	signals := DaemonSetSignals(daemonSet, observedAt)
	if !hasClass(signals, ReasonRolloutDaemonSetUnavailable) {
		t.Fatalf("classes = %v, want the unavailable DaemonSet", classesOf(signals))
	}
	if got := DaemonSetAvailability(daemonSet); got.Desired != 3 || got.Ready != 2 {
		t.Fatalf("availability = %+v, want 3 desired and 2 ready", got)
	}
}

func TestJobAndCronJobSignalsReadBatchFailures(t *testing.T) {
	t.Parallel()
	backoff := int32(2)
	job := &batchv1.Job{
		Namespace: testNamespace,
		Name:      "nightly-1",
		UID:       "j-1",
		Spec:      batchv1.JobSpec{BackoffLimit: &backoff},
		Status: batchv1.JobStatus{
			Failed: 3,
			Conditions: []batchv1.JobCondition{{
				Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "the Job failed",
			}},
		},
	}
	if signals := JobSignals(job, observedAt); !hasClass(signals, ReasonJobBackoffLimit) {
		t.Fatalf("classes = %v, want the backoff limit", classesOf(signals))
	}
	if got := JobAvailability(job); got.Ready != 0 {
		t.Fatalf("availability = %+v, want not ready", got)
	}

	cronJob := &batchv1.CronJob{
		Namespace: testNamespace,
		Name:      "nightly",
		UID:       "c-1",
		Status:    batchv1.CronJobStatus{LastSuccessfulTime: &metav1.Time{Time: observedAt.Add(-24 * time.Hour)}},
	}
	if signals := CronJobSignals(cronJob, nil, observedAt, ObserveConfig{}); !hasClass(signals, ReasonJobCronFailing) {
		t.Fatalf("classes = %v, want the stale success", classesOf(signals))
	}
	suspend := true
	cronJob.Spec.Suspend = &suspend
	if signals := CronJobSignals(cronJob, nil, observedAt, ObserveConfig{}); len(signals) != 0 {
		t.Fatalf("classes = %v, want nothing for a suspended CronJob", classesOf(signals))
	}
}

func TestNodeSignalsReadThePressureConditions(t *testing.T) {
	t.Parallel()
	node := &corev1.Node{
		Name: testNodeName,
		UID:  "n-1",
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
			{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue},
			{Type: corev1.NodeDiskPressure, Status: corev1.ConditionFalse},
		}},
	}
	signals := NodeSignals(node, observedAt)
	if !hasClass(signals, ReasonNodeNotReady) || !hasClass(signals, ReasonNodeMemoryPressure) {
		t.Fatalf("classes = %v, want NotReady and MemoryPressure", classesOf(signals))
	}
	if hasClass(signals, ReasonNodeDiskPressure) {
		t.Fatal("a False disk-pressure condition must not produce a signal")
	}
	if got := NodeAvailability(node); got.Ready != 0 {
		t.Fatalf("availability = %+v, want not ready", got)
	}
}

func TestPersistentVolumeClaimSignalsReadThePhase(t *testing.T) {
	t.Parallel()
	claim := &corev1.PersistentVolumeClaim{
		Namespace: testNamespace,
		Name:      "data",
		UID:       "pvc-1",
		Status:    corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
	if signals := PersistentVolumeClaimSignals(claim, observedAt); !hasClass(signals,
		ReasonStoragePVCProvisioning) {
		t.Fatalf("classes = %v, want the provisioning failure", classesOf(signals))
	}
	claim.Status.Phase = corev1.ClaimBound
	if signals := PersistentVolumeClaimSignals(claim, observedAt); len(signals) != 0 {
		t.Fatalf("classes = %v, want nothing for a Bound claim", classesOf(signals))
	}
	if got := PersistentVolumeClaimAvailability(claim); got.Ready != 1 {
		t.Fatalf("availability = %+v, want ready", got)
	}
}

// TestObserversCoverEveryPageKind proves the kind list the watcher registers and
// the kind list the status package can derive stay the same.
func TestObserversCoverEveryPageKind(t *testing.T) {
	t.Parallel()
	want := []v1alpha1.TargetKind{
		v1alpha1.TargetKindPod, v1alpha1.TargetKindDeployment, v1alpha1.TargetKindStatefulSet,
		v1alpha1.TargetKindDaemonSet, v1alpha1.TargetKindJob, v1alpha1.TargetKindCronJob,
		v1alpha1.TargetKindNode, v1alpha1.TargetKindPersistentVolumeClaim,
	}
	if len(observableKinds) != len(want) {
		t.Fatalf("observableKinds has %d kinds, want %d", len(observableKinds), len(want))
	}
	for index, kind := range want {
		if observableKinds[index] != kind {
			t.Errorf("observableKinds[%d] = %s, want %s", index, observableKinds[index], kind)
		}
		if _, err := newObject(string(kind)); err != nil {
			t.Errorf("kind %s has no object reader: %v", kind, err)
		}
		if APIVersionFor(string(kind)) == "" {
			t.Errorf("kind %s has no API version", kind)
		}
	}
}
