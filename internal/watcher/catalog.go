// Package watcher is the deterministic half of Pagerless: it binds both
// Kubernetes Event APIs and the durable status of every Page kind, canonicalizes
// each signal to the Target that owns it, reduces it to a stable Fingerprint, and
// decides whether the signal opens a Page, joins one, or is suppressed by a
// Cooldown. No model is involved, and nothing here writes to a workload.
package watcher

import (
	"time"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// ReasonClass is the catalog constant that classifies one failure. The string
// values are part of the Page status API and of the metric labels, so changing
// one is a versioned change.
type ReasonClass string

// Container and runtime states, derived from the durable fields of a Pod.
const (
	// ReasonPodCrashLoop is a container in CrashLoopBackOff, or three restarts with a failing exit.
	ReasonPodCrashLoop ReasonClass = "pod.crashloop"
	// ReasonPodOOMKilled is a container terminated because the kernel killed it for memory.
	ReasonPodOOMKilled ReasonClass = "pod.oomkilled"
	// ReasonPodImagePull is ImagePullBackOff or ErrImagePull.
	ReasonPodImagePull ReasonClass = "pod.imagepull"
	// ReasonPodInvalidImage is an image name that can never be pulled.
	ReasonPodInvalidImage ReasonClass = "pod.invalidimage"
	// ReasonPodConfigError is CreateContainerConfigError: a referenced Secret or ConfigMap is missing.
	ReasonPodConfigError ReasonClass = "pod.configerror"
	// ReasonPodCreateError is CreateContainerError or RunContainerError.
	ReasonPodCreateError ReasonClass = "pod.createerror"
	// ReasonPodRestartStorm is a restart count that keeps rising with no waiting reason.
	ReasonPodRestartStorm ReasonClass = "pod.restartstorm"
	// ReasonPodPending is a Pod that stayed Pending with no more specific reason.
	ReasonPodPending ReasonClass = "scheduling.pending"
	// ReasonPodFailed is a Pod in the Failed phase.
	ReasonPodFailed ReasonClass = "pod.failed"
	// ReasonPodEvicted is a Pod the kubelet evicted.
	ReasonPodEvicted ReasonClass = "pod.evicted"
)

// Scheduling, sandbox, and storage states.
const (
	// ReasonSchedulingFailed is a Pod the scheduler cannot place.
	ReasonSchedulingFailed ReasonClass = "scheduling.failedscheduling"
	// ReasonStorageFailedMount is a volume that will not mount.
	ReasonStorageFailedMount ReasonClass = "storage.failedmount"
	// ReasonStorageFailedAttach is a volume that will not attach, map, or resize.
	ReasonStorageFailedAttach ReasonClass = "storage.failedattach"
	// ReasonStorageFailedSandbox is a pod sandbox that cannot be created.
	ReasonStorageFailedSandbox ReasonClass = "storage.failedsandbox"
	// ReasonStoragePVCProvisioning is a claim that stays Pending, or is Lost.
	ReasonStoragePVCProvisioning ReasonClass = "storage.pvcprovisioning"
)

// Health, rollout, and batch states.
const (
	// ReasonHealthUnhealthy is a failing readiness, liveness, or startup probe.
	ReasonHealthUnhealthy ReasonClass = "health.unhealthy"
	// ReasonRolloutProgressDeadline is a Deployment that passed its progress deadline.
	ReasonRolloutProgressDeadline ReasonClass = "rollout.progressdeadline"
	// ReasonRolloutReplicaFailure is a Deployment that cannot create its replicas.
	ReasonRolloutReplicaFailure ReasonClass = "rollout.replicafailure"
	// ReasonRolloutDaemonSetUnavailable is a DaemonSet with unavailable nodes.
	ReasonRolloutDaemonSetUnavailable ReasonClass = "rollout.daemonsetunavailable"
	// ReasonRolloutStatefulSetStuck is a StatefulSet that cannot finish its rollout.
	ReasonRolloutStatefulSetStuck ReasonClass = "rollout.statefulsetstuck"
	// ReasonJobBackoffLimit is a Job that exhausted its backoff limit or deadline.
	ReasonJobBackoffLimit ReasonClass = "job.backofflimit"
	// ReasonJobCronFailing is a CronJob whose recent runs keep failing.
	ReasonJobCronFailing ReasonClass = "job.cronfailing"
)

// Node and infrastructure states.
const (
	// ReasonNodeNotReady is a node whose Ready condition is False or Unknown.
	ReasonNodeNotReady ReasonClass = "node.notready"
	// ReasonNodeMemoryPressure is a node reporting MemoryPressure.
	ReasonNodeMemoryPressure ReasonClass = "node.memorypressure"
	// ReasonNodeDiskPressure is a node reporting DiskPressure.
	ReasonNodeDiskPressure ReasonClass = "node.diskpressure"
	// ReasonNodePIDPressure is a node reporting PIDPressure.
	ReasonNodePIDPressure ReasonClass = "node.pidpressure"
	// ReasonNodeNetworkUnavailable is a node reporting NetworkUnavailable.
	ReasonNodeNetworkUnavailable ReasonClass = "node.networkunavailable"
)

// Entry is one catalog row: the reason class, its severity before the
// availability modifier, and the minimum continuous duration before the class
// may open a Page.
type Entry struct {
	// Class is the reason class constant.
	Class ReasonClass
	// Base is the severity before the availability, occurrence, and suppression modifiers.
	Base v1alpha1.Severity
	// Settle is how long the signal must persist before it may open a Page.
	Settle time.Duration
}

// catalog is the failure catalog of the watcher specification. Every reason
// class the watcher can emit appears here exactly once.
var catalog = map[ReasonClass]Entry{
	ReasonPodCrashLoop:    {Class: ReasonPodCrashLoop, Base: v1alpha1.SeverityInfo, Settle: time.Minute},
	ReasonPodOOMKilled:    {Class: ReasonPodOOMKilled, Base: v1alpha1.SeverityWarning},
	ReasonPodImagePull:    {Class: ReasonPodImagePull, Base: v1alpha1.SeverityWarning, Settle: 30 * time.Second},
	ReasonPodInvalidImage: {Class: ReasonPodInvalidImage, Base: v1alpha1.SeverityWarning},
	ReasonPodConfigError:  {Class: ReasonPodConfigError, Base: v1alpha1.SeverityWarning, Settle: 30 * time.Second},
	ReasonPodCreateError:  {Class: ReasonPodCreateError, Base: v1alpha1.SeverityWarning, Settle: 30 * time.Second},
	ReasonPodRestartStorm: {
		Class: ReasonPodRestartStorm, Base: v1alpha1.SeverityInfo, Settle: 15 * time.Minute,
	},
	ReasonPodPending: {Class: ReasonPodPending, Base: v1alpha1.SeverityWarning, Settle: 10 * time.Minute},
	ReasonPodFailed:  {Class: ReasonPodFailed, Base: v1alpha1.SeverityWarning, Settle: 5 * time.Minute},
	ReasonPodEvicted: {Class: ReasonPodEvicted, Base: v1alpha1.SeverityWarning},

	ReasonSchedulingFailed: {
		Class: ReasonSchedulingFailed, Base: v1alpha1.SeverityWarning, Settle: 2 * time.Minute,
	},
	ReasonStorageFailedMount: {
		Class: ReasonStorageFailedMount, Base: v1alpha1.SeverityWarning, Settle: 2 * time.Minute,
	},
	ReasonStorageFailedAttach: {
		Class: ReasonStorageFailedAttach, Base: v1alpha1.SeverityWarning, Settle: 2 * time.Minute,
	},
	ReasonStorageFailedSandbox: {
		Class: ReasonStorageFailedSandbox, Base: v1alpha1.SeverityWarning, Settle: time.Minute,
	},
	ReasonStoragePVCProvisioning: {
		Class: ReasonStoragePVCProvisioning, Base: v1alpha1.SeverityWarning, Settle: 5 * time.Minute,
	},

	ReasonHealthUnhealthy: {Class: ReasonHealthUnhealthy, Base: v1alpha1.SeverityInfo, Settle: 30 * time.Second},
	ReasonRolloutProgressDeadline: {
		Class: ReasonRolloutProgressDeadline, Base: v1alpha1.SeverityWarning,
	},
	ReasonRolloutReplicaFailure: {
		Class: ReasonRolloutReplicaFailure, Base: v1alpha1.SeverityWarning, Settle: 2 * time.Minute,
	},
	ReasonRolloutDaemonSetUnavailable: {
		Class: ReasonRolloutDaemonSetUnavailable, Base: v1alpha1.SeverityWarning, Settle: 5 * time.Minute,
	},
	ReasonRolloutStatefulSetStuck: {
		Class: ReasonRolloutStatefulSetStuck, Base: v1alpha1.SeverityWarning, Settle: 10 * time.Minute,
	},
	ReasonJobBackoffLimit: {Class: ReasonJobBackoffLimit, Base: v1alpha1.SeverityWarning},
	ReasonJobCronFailing: {
		Class: ReasonJobCronFailing, Base: v1alpha1.SeverityWarning, Settle: 5 * time.Minute,
	},

	ReasonNodeNotReady:       {Class: ReasonNodeNotReady, Base: v1alpha1.SeverityCritical, Settle: 2 * time.Minute},
	ReasonNodeMemoryPressure: {Class: ReasonNodeMemoryPressure, Base: v1alpha1.SeverityWarning, Settle: 5 * time.Minute},
	ReasonNodeDiskPressure:   {Class: ReasonNodeDiskPressure, Base: v1alpha1.SeverityWarning, Settle: 5 * time.Minute},
	ReasonNodePIDPressure:    {Class: ReasonNodePIDPressure, Base: v1alpha1.SeverityWarning, Settle: 5 * time.Minute},
	ReasonNodeNetworkUnavailable: {
		Class: ReasonNodeNetworkUnavailable, Base: v1alpha1.SeverityWarning, Settle: 5 * time.Minute,
	},
}

// LookupEntry returns the catalog entry for a reason class. An unknown class is
// ignored rather than guessed.
func LookupEntry(class ReasonClass) (Entry, bool) {
	entry, ok := catalog[class]
	return entry, ok
}

// Classes returns every catalog class, in a stable order, for the tests and the
// catalogue documentation.
func Classes() []ReasonClass {
	ordered := []ReasonClass{
		ReasonPodCrashLoop, ReasonPodOOMKilled, ReasonPodImagePull, ReasonPodInvalidImage,
		ReasonPodConfigError, ReasonPodCreateError, ReasonPodRestartStorm, ReasonPodPending,
		ReasonPodFailed, ReasonPodEvicted, ReasonSchedulingFailed, ReasonStorageFailedMount,
		ReasonStorageFailedAttach, ReasonStorageFailedSandbox, ReasonStoragePVCProvisioning,
		ReasonHealthUnhealthy, ReasonRolloutProgressDeadline, ReasonRolloutReplicaFailure,
		ReasonRolloutDaemonSetUnavailable, ReasonRolloutStatefulSetStuck, ReasonJobBackoffLimit,
		ReasonJobCronFailing, ReasonNodeNotReady, ReasonNodeMemoryPressure, ReasonNodeDiskPressure,
		ReasonNodePIDPressure, ReasonNodeNetworkUnavailable,
	}
	return ordered
}

// ClassifyEventReason maps a Kubernetes Event reason to a reason class. It is
// deliberately narrow: a reason the catalog does not know is not ingested,
// because guessing a class from a note is how a watcher pages on noise.
func ClassifyEventReason(reason string) (ReasonClass, bool) {
	switch reason {
	case "FailedScheduling":
		return ReasonSchedulingFailed, true
	case "FailedMount":
		return ReasonStorageFailedMount, true
	case "FailedAttachVolume", "FailedMapVolume", "VolumeResizeFailed":
		return ReasonStorageFailedAttach, true
	case "FailedCreatePodSandBox":
		return ReasonStorageFailedSandbox, true
	case "ProvisioningFailed":
		return ReasonStoragePVCProvisioning, true
	case "Unhealthy":
		return ReasonHealthUnhealthy, true
	case "FailedToPullImage", "BackOffPullImage", "ErrImagePull":
		return ReasonPodImagePull, true
	case "BackOff", "CrashLoopBackOff":
		return ReasonPodCrashLoop, true
	case "Evicted", "TaintManagerEviction", "Preempted":
		return ReasonPodEvicted, true
	case "OOMKilling":
		return ReasonPodOOMKilled, true
	case "NodeNotReady":
		return ReasonNodeNotReady, true
	default:
		return "", false
	}
}
