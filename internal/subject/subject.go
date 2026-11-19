// Package subject resolves the canonical Kubernetes subject that an alert names
// through labels, an annotation, or a configured extraction rule. The watcher
// and every inbound webhook dialect share this resolver so that one fault has
// one identity regardless of which sender noticed it first (ADR-0040).
package subject

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// Label keys that name a subject. The explicit pair wins over every single
// workload label, and a single workload label wins over a pod owner chain.
const (
	// LabelWorkloadKind names the Target kind explicitly, for example Deployment.
	LabelWorkloadKind = "pagerless.dev/workload-kind"
	// LabelWorkloadName names the Target explicitly.
	LabelWorkloadName = "pagerless.dev/workload-name"
	// LabelNamespace names the Target namespace. It is the conventional
	// namespace label of every supported sender.
	LabelNamespace = "namespace"
	// LabelDeployment names a Deployment, when it is the only workload label.
	LabelDeployment = "deployment"
	// LabelStatefulSet names a StatefulSet, when it is the only workload label.
	LabelStatefulSet = "statefulset"
	// LabelDaemonSet names a DaemonSet, when it is the only workload label.
	LabelDaemonSet = "daemonset"
	// LabelJob names a Job, when it is the only workload label.
	LabelJob = "job"
	// LabelCronJob names a CronJob, when it is the only workload label.
	LabelCronJob = "cronjob"
	// LabelPod names a Pod, which then resolves through its controller owner chain.
	LabelPod = "pod"
)

// AnnotationSubject carries the subject as a JSON object with the fields
// apiVersion, kind, namespace, name, and optional uid.
const AnnotationSubject = "pagerless.dev/subject"

// Opt-in labels. An inbound webhook is authenticated but not necessarily scoped
// to a Target, so an alert must say that the cluster watcher should act on it.
const (
	// LabelWatch opts an external alert in to opening or joining a Page.
	LabelWatch = "pagerless.dev/watch"
	// LegacyLabelWatch is the v1 opt-in label value, which survives v2 migration
	// by design: an installation that upgrades keeps its existing workload
	// labels, and dropping the value would silently stop its alerts. It is a
	// label value, not a retired resource name, and IsOptedIn accepts both
	// spellings.
	LegacyLabelWatch = "pagerless_triage"
)

// OptInValue is the only label value that opts an alert in.
const OptInValue = "true"

// IsOptedIn reports whether the labels carry the opt-in label in either the v2
// or the legacy spelling.
func IsOptedIn(labels map[string]string) bool {
	return labels[LabelWatch] == OptInValue || labels[LegacyLabelWatch] == OptInValue
}

// Resolution names the rule that produced a Subject. It is recorded on
// Page.status.subject.resolution so an operator can see how much to trust it.
type Resolution string

const (
	// ResolutionExplicit means the pagerless.dev/workload-kind and
	// pagerless.dev/workload-name labels named the Target.
	ResolutionExplicit Resolution = "Explicit"
	// ResolutionWorkloadLabel means exactly one workload label named the Target.
	ResolutionWorkloadLabel Resolution = "WorkloadLabel"
	// ResolutionPodOwner means a pod label resolved through the controller owner chain.
	ResolutionPodOwner Resolution = "PodOwner"
	// ResolutionAnnotation means the pagerless.dev/subject annotation named the Target.
	ResolutionAnnotation Resolution = "Annotation"
	// ResolutionExtraction means a configured extraction rule matched a label.
	ResolutionExtraction Resolution = "Extraction"
	// ResolutionMissing means the alert named a Target that no longer exists.
	ResolutionMissing Resolution = "Missing"
	// ResolutionNone means the subject was not resolved.
	ResolutionNone Resolution = "None"
)

// Subject is the canonical Kubernetes object identity of a Target.
type Subject struct {
	// APIVersion is the Target's API version, such as apps/v1.
	APIVersion string
	// Kind is the Target's kind.
	Kind v1alpha1.TargetKind
	// Namespace is the Target's namespace. It is empty only for a Node.
	Namespace string
	// Name is the Target's name.
	Name string
	// UID is the Target's UID when it is known.
	UID string
}

// IsZero reports whether the subject names no object.
func (s Subject) IsZero() bool {
	return s.Kind == "" || s.Name == ""
}

// TargetReference converts the subject into the Page's Target reference.
func (s Subject) TargetReference() v1alpha1.TargetReference {
	return v1alpha1.TargetReference{
		APIVersion: s.APIVersion,
		Kind:       s.Kind,
		Namespace:  s.Namespace,
		Name:       s.Name,
		UID:        s.UID,
	}
}

// Key returns a stable identity for the subject. It includes the cluster-scoped
// namespace and the UID when one is known, so a recreated object and a
// same-named object in another namespace are different identities.
func (s Subject) Key() string {
	digest := sha256.New()
	for _, field := range []string{
		"pagerless.dev/subject/v1", s.APIVersion, string(s.Kind), s.Namespace, s.Name, s.UID,
	} {
		digest.Write([]byte(field))
		digest.Write([]byte{0})
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// Result is a resolved subject and the rule that produced it.
type Result struct {
	// Subject is the resolved identity.
	Subject Subject
	// Resolution names the rule that matched.
	Resolution Resolution
	// ResolvedFrom lists the label or annotation keys that named the subject,
	// in the order they were read.
	ResolvedFrom []string
}

// Ignore is returned when an alert cannot name a subject. The reason is safe to
// return to a sender: it reveals no configuration and no credential.
type Ignore struct {
	// Reason is a short, stable explanation, such as "ambiguous-workload-labels".
	Reason string
}

// Error implements error.
func (e *Ignore) Error() string { return e.Reason }

// newIgnore returns an Ignore error with the given reason.
func newIgnore(reason string) error { return &Ignore{Reason: reason} }

// IgnoreReason returns the reason carried by an Ignore error.
func IgnoreReason(err error) (string, bool) {
	if ignore, ok := errors.AsType[*Ignore](err); ok {
		return ignore.Reason, true
	}
	return "", false
}

// workloadLabels maps a single workload label to the Target kind it names.
var workloadLabels = []struct {
	label string
	kind  v1alpha1.TargetKind
}{
	{LabelDeployment, v1alpha1.TargetKindDeployment},
	{LabelStatefulSet, v1alpha1.TargetKindStatefulSet},
	{LabelDaemonSet, v1alpha1.TargetKindDaemonSet},
	{LabelJob, v1alpha1.TargetKindJob},
	{LabelCronJob, v1alpha1.TargetKindCronJob},
}

// ParseKind converts a kind string to a Page Target kind.
func ParseKind(kind string) (v1alpha1.TargetKind, bool) {
	k := v1alpha1.TargetKind(strings.TrimSpace(kind))
	switch k {
	case v1alpha1.TargetKindDeployment, v1alpha1.TargetKindStatefulSet, v1alpha1.TargetKindDaemonSet,
		v1alpha1.TargetKindJob, v1alpha1.TargetKindCronJob, v1alpha1.TargetKindPod,
		v1alpha1.TargetKindPersistentVolumeClaim, v1alpha1.TargetKindNode:
		return k, true
	default:
		return "", false
	}
}

// APIVersion returns the API version that serves a Target kind.
func APIVersion(kind v1alpha1.TargetKind) string {
	switch kind {
	case v1alpha1.TargetKindDeployment, v1alpha1.TargetKindStatefulSet, v1alpha1.TargetKindDaemonSet:
		return "apps/v1"
	case v1alpha1.TargetKindJob, v1alpha1.TargetKindCronJob:
		return "batch/v1"
	default:
		return "v1"
	}
}
