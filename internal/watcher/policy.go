package watcher

import (
	"slices"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// NamespaceMode is how the watcher treats a namespace or a Node Page.
//
// +kubebuilder:validation:Enum=watch;warn-only;ignore
type NamespaceMode string

const (
	// ModeWatch opens Pages that are investigated.
	ModeWatch NamespaceMode = "watch"
	// ModeWarnOnly opens a visible Page and never spends model budget on it,
	// because an application pull request usually cannot fix the component.
	ModeWarnOnly NamespaceMode = "warn-only"
	// ModeIgnore drops the signal before it is stored.
	ModeIgnore NamespaceMode = "ignore"
)

// kubeSystemNamespace holds the control plane. Its churn is visible by default
// but never investigated, because an application pull request cannot fix it.
const kubeSystemNamespace = "kube-system"

// reservedNamespaces are always ignored, in every configuration. They carry
// cluster bookkeeping whose churn is never an application failure.
var reservedNamespaces = map[string]struct{}{
	"kube-public":     {},
	"kube-node-lease": {},
}

// Policy decides whether a Signal is watched, warn-only, or dropped. The order
// of evaluation is the specification's: the release namespace, the reserved
// namespaces, self-exclusion, kube-system, the include list, and the exclude
// list.
type Policy struct {
	// ReleaseNamespace holds Pagerless's own Pods and Jobs; watching it would let
	// a failing agent Job page on itself.
	ReleaseNamespace string
	// Include, when non-empty, restricts watching to exactly these namespaces.
	Include []string
	// Exclude drops every listed namespace.
	Exclude []string
	// KubeSystem is how kube-system is treated. It defaults to warn-only.
	KubeSystem NamespaceMode
	// NodePages is how cluster-scoped Node faults are treated. It defaults to watch.
	NodePages NamespaceMode
}

// Verdict is a Policy decision.
type Verdict struct {
	// Mode is watch or warn-only; an ignored signal has no Verdict at all.
	Mode NamespaceMode
	// Reason records why a signal was dropped or made warn-only.
	Reason string
}

// Ignored reports whether the signal is dropped.
func (v Verdict) Ignored() bool { return v.Mode == ModeIgnore }

// Evaluate applies the namespace policy to one Subject. The Subject's own
// annotations and labels are checked first, because an operator's ignore
// annotation is a statement about the object, not about its namespace.
func (p Policy) Evaluate(subject Subject) Verdict {
	if ignoredByMetadata(subject.Annotations, subject.Labels) {
		return Verdict{Mode: ModeIgnore, Reason: "annotation"}
	}
	if v1alpha1.TargetKind(subject.Kind) == v1alpha1.TargetKindNode {
		return Verdict{Mode: p.nodeMode(), Reason: "node-page"}
	}
	switch namespace := subject.Namespace; {
	case namespace == p.ReleaseNamespace:
		return Verdict{Mode: ModeIgnore, Reason: "release-namespace"}
	case reserved(namespace):
		return Verdict{Mode: ModeIgnore, Reason: "reserved-namespace"}
	case excluded(namespace, p.Exclude):
		return Verdict{Mode: ModeIgnore, Reason: "excluded-namespace"}
	case namespace == kubeSystemNamespace:
		return Verdict{Mode: p.kubeSystemMode(), Reason: kubeSystemNamespace}
	case len(p.Include) > 0 && !contains(p.Include, namespace):
		return Verdict{Mode: ModeIgnore, Reason: "not-included"}
	default:
		return Verdict{Mode: ModeWatch}
	}
}

// ignoredByMetadata reports whether an object opted out with the ignore
// annotation or declared itself part of Pagerless.
func ignoredByMetadata(annotations, labels map[string]string) bool {
	if annotations[v1alpha1.AnnotationIgnore] == "true" {
		return true
	}
	return labels[v1alpha1.LabelPartOf] == "pagerless"
}

// kubeSystemMode returns the configured kube-system treatment, which is
// warn-only unless the operator opted in.
func (p Policy) kubeSystemMode() NamespaceMode {
	if p.KubeSystem == "" {
		return ModeWarnOnly
	}
	return p.KubeSystem
}

// nodeMode returns the configured Node treatment, which is watch unless the
// operator opted out.
func (p Policy) nodeMode() NamespaceMode {
	if p.NodePages == "" {
		return ModeWatch
	}
	return p.NodePages
}

func reserved(namespace string) bool {
	_, ok := reservedNamespaces[namespace]
	return ok
}

func excluded(namespace string, list []string) bool {
	return contains(list, namespace)
}

func contains(list []string, value string) bool {
	return slices.Contains(list, value)
}
