package watcher

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// maxOwnerDepth is the defensive cap on the owner chain. A real chain is at most
// two hops (Pod to ReplicaSet to Deployment); anything longer is corruption.
const maxOwnerDepth = 4

// maxRelatedWorkloads bounds the evidence a shared claim or a Node records.
const maxRelatedWorkloads = 10

// ErrNoSubject means a signal named an object that is not, and does not resolve
// to, something a Page can investigate.
var ErrNoSubject = errors.New("the signal names no supported Subject")

// ErrIgnored means the object is deliberately not watched: a mirror Pod belongs
// to its node, and a static Pod's kubelet restarts it outside the control plane.
var ErrIgnored = errors.New("the object is deliberately not watched")

// Reference names one Kubernetes object without fetching it.
type Reference struct {
	// APIVersion is the object's API version, such as apps/v1.
	APIVersion string
	// Kind is the object's kind.
	Kind string
	// Namespace is the object's namespace; it is empty for cluster-scoped kinds.
	Namespace string
	// Name is the object's name.
	Name string
	// UID is the object's UID, when the caller has it.
	UID string
}

// String renders the reference for a log line or an error.
func (r Reference) String() string {
	if r.Namespace == "" {
		return r.Kind + "/" + r.Name
	}
	return r.Kind + "/" + r.Namespace + "/" + r.Name
}

// Subject is the canonical Target a Page investigates, with the evidence that
// explains how a raw signal resolved to it.
type Subject struct {
	// ClusterID identifies the Cluster, so two Clusters sharing a database cannot collide.
	ClusterID string
	// APIVersion, Kind, Namespace, Name, and UID are the Subject's identity.
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
	UID        string
	// Resolution reports whether the whole owner chain resolved.
	Resolution v1alpha1.SubjectResolution
	// ResolvedFrom names the raw field the Subject came from when it is not the
	// signal's own object: "related" or "pvc".
	ResolvedFrom string
	// Related are the consumers a shared claim resolved to, or the Workloads a
	// Node fault affects. They are evidence, never the Subject.
	Related []v1alpha1.TargetReference
	// Annotations and Labels are the Subject object's metadata. They decide the
	// ignore annotation and the self-exclusion label, and are never identity.
	Annotations map[string]string
	Labels      map[string]string
}

// Target returns the Subject as a Page Target.
func (s Subject) Target() v1alpha1.TargetReference {
	return v1alpha1.TargetReference{
		APIVersion: s.APIVersion,
		Kind:       v1alpha1.TargetKind(s.Kind),
		Namespace:  s.Namespace,
		Name:       s.Name,
		UID:        s.UID,
	}
}

// FieldValues returns the identity fields in the order every hash uses them.
func (s Subject) FieldValues() []string {
	return []string{s.ClusterID, s.APIVersion, s.Kind, s.Namespace, s.Name, s.UID}
}

// Status returns the mirror of the Subject that a Page carries.
func (s Subject) Status() *v1alpha1.SubjectStatus {
	return &v1alpha1.SubjectStatus{
		APIVersion:       s.APIVersion,
		Kind:             v1alpha1.TargetKind(s.Kind),
		Namespace:        s.Namespace,
		Name:             s.Name,
		UID:              s.UID,
		Resolution:       s.Resolution,
		ResolvedFrom:     s.ResolvedFrom,
		RelatedWorkloads: s.Related,
	}
}

// Object is the part of a Kubernetes object that canonicalization reads.
type Object struct {
	// Reference is the object's identity.
	Reference Reference
	// ControllerRef is the single controller owner, or nil.
	ControllerRef *Reference
	// ControllerRefs counts the controller owner references, so more than one can
	// be reported as corruption rather than silently ignored.
	ControllerRefs int
	// Annotations and Labels are the object's metadata.
	Annotations map[string]string
	Labels      map[string]string
}

// Lookup fetches one object. It reads the informer cache; a cache miss is
// reported with ErrObjectNotFound.
type Lookup interface {
	Get(ctx context.Context, reference Reference) (*Object, error)
}

// ErrObjectNotFound means the Lookup has no such object, usually a deleted owner.
var ErrObjectNotFound = errors.New("object not found")

// ClaimConsumers lists the Pods that mount one claim in one namespace.
type ClaimConsumers interface {
	Consumers(ctx context.Context, namespace, claim string) ([]Reference, error)
}

// Resolver canonicalizes a raw reference to the supported Subject that owns it.
type Resolver struct {
	// Lookup reads objects from the cache.
	Lookup Lookup
	// Claims lists the consumers of a PersistentVolumeClaim. It is required only
	// when claims are watched.
	Claims ClaimConsumers
}

// supported reports whether a kind is a Page Target.
func supported(kind string) bool {
	switch v1alpha1.TargetKind(kind) {
	case v1alpha1.TargetKindDeployment, v1alpha1.TargetKindStatefulSet, v1alpha1.TargetKindDaemonSet,
		v1alpha1.TargetKindJob, v1alpha1.TargetKindCronJob, v1alpha1.TargetKindPod,
		v1alpha1.TargetKindPersistentVolumeClaim, v1alpha1.TargetKindNode:
		return true
	default:
		return false
	}
}

// Resolve follows controller owner references from reference to the highest
// supported kind. It follows only `controller: true`, verifies the owner's UID,
// requires the same namespace, caps the depth, and detects a cycle by UID.
//
// A chain that breaks returns the highest supported object that did resolve and
// reports Partial: the Deployment was deleted while its Pods remained, so the
// Pod is the Subject and the evidence records that the owner is gone. The
// deterministic identity is always the fetched object's, never the reference's.
func (r *Resolver) Resolve(ctx context.Context, reference Reference) (Subject, error) {
	object, err := r.Lookup.Get(ctx, reference)
	if err != nil {
		return Subject{}, err
	}
	if mirrorPod(object) {
		return Subject{}, fmt.Errorf("%s is a mirror Pod: %w", reference, ErrIgnored)
	}
	if v1alpha1.TargetKind(reference.Kind) == v1alpha1.TargetKindPersistentVolumeClaim {
		return r.resolveClaim(ctx, object)
	}

	subject := Subject{}
	if supported(object.Reference.Kind) {
		subject = subjectOf(object, nil)
	}
	visited := map[string]struct{}{object.Reference.UID: {}}
	current := object
	for range maxOwnerDepth {
		switch {
		case current.ControllerRefs > 1:
			return partial(subject, reference)
		case current.ControllerRef == nil:
			return finish(subject, reference)
		}
		owner := *current.ControllerRef
		if owner.Namespace != current.Reference.Namespace {
			return partial(subject, reference)
		}
		if _, seen := visited[owner.UID]; seen {
			return partial(subject, reference)
		}
		next, err := r.Lookup.Get(ctx, owner)
		if errors.Is(err, ErrObjectNotFound) {
			return partial(subject, reference)
		}
		if err != nil {
			return Subject{}, err
		}
		if next.Reference.UID != owner.UID {
			// The owner was recreated: the stale reference points at a dead object,
			// so following it would attach a new incident to an old Page.
			return partial(subject, reference)
		}
		visited[next.Reference.UID] = struct{}{}
		current = next
		if supported(current.Reference.Kind) {
			subject = subjectOf(current, subject.Related)
		}
	}
	// The cap was reached with the chain still going: treat the last supported
	// object as the Subject rather than looping.
	return partial(subject, reference)
}

// finish returns the resolved Subject, or ErrNoSubject when nothing on the chain
// is supported.
func finish(subject Subject, reference Reference) (Subject, error) {
	if subject.Kind == "" {
		return Subject{}, fmt.Errorf("%s: %w", reference, ErrNoSubject)
	}
	subject.Resolution = v1alpha1.ResolutionCanonical
	return subject, nil
}

// partial returns the highest supported object when the chain broke.
func partial(subject Subject, reference Reference) (Subject, error) {
	if subject.Kind == "" {
		return Subject{}, fmt.Errorf("%s: %w", reference, ErrNoSubject)
	}
	subject.Resolution = v1alpha1.ResolutionPartial
	return subject, nil
}

// resolveClaim resolves a PersistentVolumeClaim. A claim has no owner reference
// to a workload, so the Pods that mount it are resolved instead: exactly one
// distinct Workload makes that Workload the Subject, and anything else leaves
// the claim as the Subject with the consumers recorded as evidence. That is what
// stops one broken shared claim from fanning out into an unbounded number of
// Pages.
func (r *Resolver) resolveClaim(ctx context.Context, claim *Object) (Subject, error) {
	if r.Claims == nil {
		return finish(subjectOf(claim, nil), claim.Reference)
	}
	consumers, err := r.Claims.Consumers(ctx, claim.Reference.Namespace, claim.Reference.Name)
	if err != nil {
		return Subject{}, fmt.Errorf("list the consumers of %s: %w", claim.Reference, err)
	}
	resolved := make([]Subject, 0, len(consumers))
	seen := make(map[string]struct{}, len(consumers))
	for _, consumer := range consumers {
		subject, err := r.Resolve(ctx, consumer)
		if err != nil {
			if errors.Is(err, ErrNoSubject) || errors.Is(err, ErrIgnored) {
				continue
			}
			return Subject{}, err
		}
		key := SubjectKey(subject)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		resolved = append(resolved, subject)
	}
	if len(resolved) == 1 {
		subject := resolved[0]
		subject.ResolvedFrom = "pvc"
		return subject, nil
	}
	subject := subjectOf(claim, nil)
	subject.ResolvedFrom = "pvc"
	for _, consumer := range resolved {
		if len(subject.Related) == maxRelatedWorkloads {
			break
		}
		subject.Related = append(subject.Related, consumer.Target())
	}
	return finish(subject, claim.Reference)
}

// subjectOf builds the Subject a supported object names, carrying evidence.
func subjectOf(object *Object, related []v1alpha1.TargetReference) Subject {
	return Subject{
		APIVersion:  object.Reference.APIVersion,
		Kind:        object.Reference.Kind,
		Namespace:   object.Reference.Namespace,
		Name:        object.Reference.Name,
		UID:         object.Reference.UID,
		Related:     related,
		Annotations: object.Annotations,
		Labels:      object.Labels,
	}
}

// mirrorPod reports whether a Pod is a static or mirror Pod, whose failure
// belongs to its node.
func mirrorPod(object *Object) bool {
	if v1alpha1.TargetKind(object.Reference.Kind) != v1alpha1.TargetKindPod {
		return false
	}
	_, isMirror := object.Annotations["kubernetes.io/config.mirror"]
	return isMirror
}

// ReferenceFor returns the Reference of a Target-shaped object, which is how the
// Event decoders and the status observers name what they saw.
func ReferenceFor(apiVersion, kind, namespace, name string) Reference {
	return Reference{APIVersion: apiVersion, Kind: kind, Namespace: namespace, Name: name}
}

// watchable reports whether an Event's object reference can produce a Subject,
// either directly or through the one intermediate owner kind the watcher reads.
// It decides the `related` fallback of the events.k8s.io/v1 decoder, so a kind
// that cannot resolve to a Subject — a Service, an Ingress, an autoscaler —
// must be reported as unsupported even though the RBAC role may read it.
func watchable(kind string) bool {
	switch strings.TrimSpace(kind) {
	case string(v1alpha1.TargetKindPod), string(v1alpha1.TargetKindDeployment),
		string(v1alpha1.TargetKindStatefulSet), string(v1alpha1.TargetKindDaemonSet),
		string(v1alpha1.TargetKindJob), string(v1alpha1.TargetKindCronJob),
		string(v1alpha1.TargetKindPersistentVolumeClaim), string(v1alpha1.TargetKindNode),
		"ReplicaSet":
		return true
	default:
		return false
	}
}
