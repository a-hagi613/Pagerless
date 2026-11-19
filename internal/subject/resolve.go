package subject

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// Rule is a configured extraction rule. It reads one label or annotation and
// matches it with a regular expression whose named groups name the Target. A
// rule exists for senders that carry no Kubernetes labels at all, such as a
// CloudWatch alarm.
type Rule struct {
	// Label is the label or annotation key the rule reads.
	Label string
	// Pattern is the regular expression. It must contain a "name" group and,
	// unless Namespace is fixed, a "namespace" group.
	Pattern string
	// Kind is the Target kind the rule produces.
	Kind v1alpha1.TargetKind
	// Namespace is the fixed namespace when the pattern does not capture one.
	Namespace string

	pattern *regexp.Regexp
}

// Compile validates the rule and prepares its pattern. Every rule is compiled
// at startup so a malformed rule fails the boot rather than the first Page.
func (r *Rule) Compile() error {
	switch {
	case r.Label == "":
		return fmt.Errorf("extraction rule: a label is required")
	case r.Pattern == "":
		return fmt.Errorf("extraction rule %q: a pattern is required", r.Label)
	}
	kind, ok := ParseKind(string(r.Kind))
	if !ok {
		return fmt.Errorf("extraction rule %q: unknown kind %q", r.Label, r.Kind)
	}
	if kind.IsClusterScoped() && r.Namespace != "" {
		return fmt.Errorf("extraction rule %q: a Node subject cannot have a namespace", r.Label)
	}
	pattern, err := regexp.Compile(r.Pattern)
	if err != nil {
		return fmt.Errorf("extraction rule %q: %w", r.Label, err)
	}
	if pattern.SubexpIndex("name") < 0 {
		return fmt.Errorf("extraction rule %q: the pattern needs a named group (?P<name>...)", r.Label)
	}
	if !kind.IsClusterScoped() && r.Namespace == "" && pattern.SubexpIndex("namespace") < 0 {
		return fmt.Errorf("extraction rule %q: the pattern needs a namespace group or a fixed namespace",
			r.Label)
	}
	r.pattern = pattern
	return nil
}

// Resolver resolves a subject from labels and annotations. It is safe for
// concurrent use once constructed.
type Resolver struct {
	// Reader reads Pods and their owner objects for the owner chain. When it is
	// nil, a pod label is ignored rather than guessed.
	Reader client.Reader
	// Rules are the configured extraction rules, tried after the annotation.
	Rules []Rule
}

// Validate compiles every extraction rule.
func (r *Resolver) Validate() error {
	for i := range r.Rules {
		if err := r.Rules[i].Compile(); err != nil {
			return err
		}
	}
	return nil
}

// Resolve applies the documented resolution order and returns an Ignore error
// when the alert names no subject. It never guesses a subject.
//
// The order is: the explicit pagerless.dev/workload-kind and
// pagerless.dev/workload-name pair, one single workload label, a pod label
// followed by the controller owner chain, the pagerless.dev/subject annotation,
// and finally a configured extraction rule.
func (r *Resolver) Resolve(
	ctx context.Context, labels, annotations map[string]string,
) (Result, error) {
	namespace := labels[LabelNamespace]
	if result, done, err := r.fromExplicitLabels(labels, namespace); done {
		return result, err
	}
	if result, done, err := r.fromWorkloadLabel(labels, namespace); done {
		return result, err
	}
	if pod := labels[LabelPod]; pod != "" {
		return r.fromPod(ctx, namespace, pod)
	}
	if result, done, err := r.fromAnnotation(annotations); done {
		return result, err
	}
	if result, done, err := r.fromRule(labels, annotations); done {
		return result, err
	}
	return Result{Resolution: ResolutionNone}, newIgnore("no-subject")
}

// fromExplicitLabels resolves the explicit kind and name pair.
func (r *Resolver) fromExplicitLabels(
	labels map[string]string, namespace string,
) (Result, bool, error) {
	kindName, name := labels[LabelWorkloadKind], labels[LabelWorkloadName]
	if kindName == "" && name == "" {
		return Result{}, false, nil
	}
	if kindName == "" || name == "" {
		return Result{}, true, newIgnore("incomplete-explicit-workload")
	}
	kind, ok := ParseKind(kindName)
	if !ok {
		return Result{}, true, newIgnore("unknown-workload-kind")
	}
	if err := checkNamespace(kind, namespace); err != nil {
		return Result{}, true, err
	}
	return Result{
		Subject:      Subject{APIVersion: APIVersion(kind), Kind: kind, Namespace: namespace, Name: name},
		Resolution:   ResolutionExplicit,
		ResolvedFrom: []string{LabelWorkloadKind, LabelWorkloadName, LabelNamespace},
	}, true, nil
}

// fromWorkloadLabel resolves one single workload label.
func (r *Resolver) fromWorkloadLabel(
	labels map[string]string, namespace string,
) (Result, bool, error) {
	label, kind, name := "", v1alpha1.TargetKind(""), ""
	for _, candidate := range workloadLabels {
		if candidateName := labels[candidate.label]; candidateName != "" {
			if name != "" {
				return Result{}, true, newIgnore("ambiguous-workload-labels")
			}
			label, kind, name = candidate.label, candidate.kind, candidateName
		}
	}
	if name == "" {
		return Result{}, false, nil
	}
	if err := checkNamespace(kind, namespace); err != nil {
		return Result{}, true, err
	}
	return Result{
		Subject:      Subject{APIVersion: APIVersion(kind), Kind: kind, Namespace: namespace, Name: name},
		Resolution:   ResolutionWorkloadLabel,
		ResolvedFrom: []string{label, LabelNamespace},
	}, true, nil
}

// fromAnnotation resolves the structured subject annotation.
func (r *Resolver) fromAnnotation(annotations map[string]string) (Result, bool, error) {
	raw := annotations[AnnotationSubject]
	if raw == "" {
		return Result{}, false, nil
	}
	var named struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Namespace  string `json:"namespace"`
		Name       string `json:"name"`
		UID        string `json:"uid"`
	}
	if err := json.Unmarshal([]byte(raw), &named); err != nil {
		return Result{}, true, newIgnore("malformed-subject-annotation")
	}
	kind, ok := ParseKind(named.Kind)
	if !ok || named.Name == "" {
		return Result{}, true, newIgnore("malformed-subject-annotation")
	}
	if err := checkNamespace(kind, named.Namespace); err != nil {
		return Result{}, true, err
	}
	apiVersion := named.APIVersion
	if apiVersion == "" {
		apiVersion = APIVersion(kind)
	}
	return Result{
		Subject: Subject{
			APIVersion: apiVersion, Kind: kind, Namespace: named.Namespace, Name: named.Name, UID: named.UID,
		},
		Resolution:   ResolutionAnnotation,
		ResolvedFrom: []string{AnnotationSubject},
	}, true, nil
}

// fromRule resolves a configured extraction rule.
func (r *Resolver) fromRule(labels, annotations map[string]string) (Result, bool, error) {
	for i := range r.Rules {
		rule := &r.Rules[i]
		value := labels[rule.Label]
		if value == "" {
			value = annotations[rule.Label]
		}
		if value == "" {
			continue
		}
		if rule.pattern == nil {
			if err := rule.Compile(); err != nil {
				return Result{}, true, newIgnore("misconfigured-extraction-rule")
			}
		}
		groups := namedGroups(rule.pattern, value)
		name := groups["name"]
		if name == "" {
			return Result{}, true, newIgnore("extraction-rule-did-not-match")
		}
		namespace := rule.Namespace
		if captured := groups["namespace"]; captured != "" {
			namespace = captured
		}
		if err := checkNamespace(rule.Kind, namespace); err != nil {
			return Result{}, true, err
		}
		return Result{
			Subject: Subject{
				APIVersion: APIVersion(rule.Kind), Kind: rule.Kind, Namespace: namespace, Name: name,
			},
			Resolution:   ResolutionExtraction,
			ResolvedFrom: []string{rule.Label},
		}, true, nil
	}
	return Result{}, false, nil
}

// namedGroups returns the named capture groups of the first match.
func namedGroups(pattern *regexp.Regexp, value string) map[string]string {
	match := pattern.FindStringSubmatch(value)
	if match == nil {
		return nil
	}
	groups := make(map[string]string)
	for i, name := range pattern.SubexpNames() {
		if i == 0 || name == "" || i >= len(match) {
			continue
		}
		groups[name] = match[i]
	}
	return groups
}

// checkNamespace enforces the Target namespace rule: a namespaced kind needs a
// namespace, and a Node must not have one.
func checkNamespace(kind v1alpha1.TargetKind, namespace string) error {
	if kind.IsClusterScoped() {
		if namespace != "" {
			return newIgnore("namespace-not-allowed-for-node")
		}
		return nil
	}
	if namespace == "" {
		return newIgnore("namespace-missing")
	}
	return nil
}
