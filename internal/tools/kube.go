package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// PodLogReader reads the pod log subresource. client-go's clientset implements
// it; the interface keeps the broker testable without a cluster.
type PodLogReader interface {
	// GetLogs returns the log bytes of one container.
	GetLogs(ctx context.Context, namespace, pod string, options *corev1.PodLogOptions) ([]byte, error)
}

// The log source labels the broker reports.
const (
	logSourcePrevious = "previous"
	logSourceCurrent  = "current"
)

const (
	// maxListedObjects bounds one list result.
	maxListedObjects = 50
	// maxLogLines bounds one container's returned lines.
	maxLogLines = 1000
	// defaultLogLines is the default tail.
	defaultLogLines = 200
	// defaultSinceSeconds is the default look-back window.
	defaultSinceSeconds = 3600
	// defaultEventLimit bounds one event result.
	defaultEventLimit = 100
	// maxEventLimit is the largest event result a caller may ask for.
	maxEventLimit = 200
)

// kubeTools serves the Kubernetes read tools. Every method re-derives the scope
// from the Page; none trusts an argument for its namespace or its ownership.
type kubeTools struct {
	reader  client.Reader
	podLogs PodLogReader
	// now reads the broker's clock, so a look-back window is anchored to the
	// same instant as the rest of the call rather than to the wall clock.
	now func() time.Time
}

// getArguments are the arguments of object_get and owner_chain.
type getArguments struct {
	Kind       string `json:"kind"`
	APIVersion string `json:"apiVersion"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
}

// listArguments are the arguments of object_list.
type listArguments struct {
	Kind          string `json:"kind"`
	APIVersion    string `json:"apiVersion"`
	LabelSelector string `json:"labelSelector"`
	Namespace     string `json:"namespace"`
}

// logArguments are the arguments of pods_logs.
type logArguments struct {
	Pod          string `json:"pod"`
	Container    string `json:"container"`
	Previous     *bool  `json:"previous"`
	TailLines    int64  `json:"tailLines"`
	SinceSeconds int64  `json:"sinceSeconds"`
}

// eventArguments are the arguments of events_list.
type eventArguments struct {
	SinceSeconds int64 `json:"sinceSeconds"`
	Limit        int   `json:"limit"`
}

// objectResult is one bounded object rendering.
type objectResult struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   map[string]any `json:"metadata"`
	Spec       any            `json:"spec,omitempty"`
	Status     any            `json:"status,omitempty"`
	Data       map[string]any `json:"data,omitempty"`
}

// logsResult is the pods_logs answer.
type logsResult struct {
	Namespace string          `json:"namespace"`
	Pods      []containerLogs `json:"pods"`
	Notes     []string        `json:"notes,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
}

// containerLogs is one container's log read.
type containerLogs struct {
	Pod       string   `json:"pod"`
	Container string   `json:"container,omitempty"`
	Previous  bool     `json:"previous,omitempty"`
	Lines     []string `json:"lines,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
}

// eventResult is the events_list answer.
type eventResult struct {
	Events    []eventEntry `json:"events"`
	Truncated bool         `json:"truncated,omitempty"`
	Notes     []string     `json:"notes,omitempty"`
}

// eventEntry is one Event from either Event API.
type eventEntry struct {
	Time    time.Time `json:"time,omitempty"`
	Type    string    `json:"type,omitempty"`
	Reason  string    `json:"reason,omitempty"`
	Object  string    `json:"object,omitempty"`
	Count   int32     `json:"count,omitempty"`
	Message string    `json:"message,omitempty"`
	Source  string    `json:"source"`
}

// ownerEntry is one link in the owner chain.
type ownerEntry struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace,omitempty"`
	UID        string `json:"uid,omitempty"`
	Controller bool   `json:"controller,omitempty"`
}

// get reads one object in the Page's namespace.
func (k *kubeTools) get(ctx context.Context, page *v1alpha1.Page, scope Scope, raw json.RawMessage) (any, error) {
	var args getArguments
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidArguments, err)
	}
	if err := scope.checkKind(args.Kind); err != nil {
		return nil, err
	}
	if err := scope.checkArgumentsNamespace(args.Namespace, page.Spec.Target.Namespace); err != nil {
		return nil, err
	}
	gv, err := gvk(args.Kind, args.APIVersion)
	if err != nil {
		return nil, err
	}
	if args.Name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidArguments)
	}
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(gv)
	key := client.ObjectKey{Namespace: page.Spec.Target.Namespace, Name: args.Name}
	if err := k.reader.Get(ctx, key, object); err != nil {
		return nil, fmt.Errorf("read %s %s: %w", args.Kind, args.Name, err)
	}
	if err := checkObjectNamespace(object, page.Spec.Target.Namespace); err != nil {
		return nil, err
	}
	return renderObject(object), nil
}

// list reads one kind in the Page's namespace.
func (k *kubeTools) list(ctx context.Context, page *v1alpha1.Page, scope Scope, raw json.RawMessage) (any, error) {
	var args listArguments
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidArguments, err)
	}
	if err := scope.checkKind(args.Kind); err != nil {
		return nil, err
	}
	if err := scope.checkArgumentsNamespace(args.Namespace, page.Spec.Target.Namespace); err != nil {
		return nil, err
	}
	gv, err := gvk(args.Kind, args.APIVersion)
	if err != nil {
		return nil, err
	}
	selector, err := labels.Parse(args.LabelSelector)
	if err != nil {
		return nil, fmt.Errorf("%w: labelSelector: %v", ErrInvalidArguments, err)
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gv.GroupVersion().WithKind(args.Kind + "List"))
	options := []client.ListOption{
		client.InNamespace(page.Spec.Target.Namespace),
		client.MatchingLabelsSelector{Selector: selector},
		client.Limit(maxListedObjects),
	}
	if err := k.reader.List(ctx, list, options...); err != nil {
		return nil, fmt.Errorf("list %s: %w", args.Kind, err)
	}
	items := make([]objectResult, 0, len(list.Items))
	for i := range list.Items {
		if len(items) >= maxListedObjects {
			break
		}
		item := &list.Items[i]
		if err := checkObjectNamespace(item, page.Spec.Target.Namespace); err != nil {
			continue
		}
		items = append(items, renderObject(item))
	}
	return map[string]any{
		fieldNamespace: page.Spec.Target.Namespace,
		fieldKind:      args.Kind,
		"items":        items,
		fieldTruncated: len(list.Items) >= maxListedObjects,
	}, nil
}

// logs reads the Target's pod logs, previous container first.
func (k *kubeTools) logs(ctx context.Context, page *v1alpha1.Page, scope Scope, raw json.RawMessage) (any, error) {
	var args logArguments
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidArguments, err)
	}
	if err := scope.checkNamespace(page.Spec.Target.Namespace); err != nil {
		return nil, err
	}
	pods, err := targetPodNames(ctx, k.reader, page.Spec.Target)
	if err != nil {
		return nil, err
	}
	if args.Pod != "" {
		if _, ok := pods[args.Pod]; !ok {
			return nil, fmt.Errorf("%w: pod %s is not part of the Target", ErrOutOfScope, args.Pod)
		}
		pods = map[string]struct{}{args.Pod: {}}
	}
	if len(pods) == 0 {
		return logsResult{Namespace: page.Spec.Target.Namespace, Notes: []string{"the Target has no pods"}}, nil
	}
	tail := args.TailLines
	if tail <= 0 || tail > maxLogLines {
		tail = defaultLogLines
	}
	since := args.SinceSeconds
	if since <= 0 {
		since = defaultSinceSeconds
	}
	result := logsResult{Namespace: page.Spec.Target.Namespace}
	names := sortedKeys(pods)
	if len(names) > 5 {
		result.Notes = append(result.Notes, fmt.Sprintf("read %d of %d matching pods", 5, len(names)))
		names = names[:5]
	}
	for _, podName := range names {
		entry, truncated, notes := k.readPodLogs(ctx, page.Spec.Target.Namespace, podName, args, tail, since)
		if truncated {
			result.Truncated = true
		}
		result.Notes = append(result.Notes, notes...)
		result.Pods = append(result.Pods, entry)
	}
	return result, nil
}

// readPodLogs reads one pod's containers, trying the previous instance first.
func (k *kubeTools) readPodLogs(
	ctx context.Context, namespace, podName string, args logArguments, tail, since int64,
) (containerLogs, bool, []string) {
	entry := containerLogs{Pod: podName, Container: args.Container}
	var notes []string
	containers := []string{args.Container}
	if args.Container == "" {
		var pod corev1.Pod
		if err := k.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: podName}, &pod); err != nil {
			notes = append(notes, fmt.Sprintf("read pod %s: %v", podName, err))
			return entry, false, notes
		}
		for _, container := range pod.Spec.Containers {
			containers = append(containers, container.Name)
		}
	}
	previous := args.Previous == nil || *args.Previous
	for _, container := range containers {
		if container == "" {
			continue
		}
		lines, source, truncated, err := k.readContainerLogs(ctx, namespace, podName, container, previous, tail, since)
		if err != nil {
			notes = append(notes, fmt.Sprintf("read %s container %s: %v", podName, container, err))
			continue
		}
		if len(lines) == 0 && previous {
			// A previous instance may not exist, so fall back to the current one.
			lines, source, truncated, err = k.readContainerLogs(ctx, namespace, podName, container, false, tail, since)
			if err != nil {
				notes = append(notes, fmt.Sprintf("read %s container %s: %v", podName, container, err))
				continue
			}
		}
		entry.Container = container
		entry.Lines = lines
		entry.Previous = source == logSourcePrevious
		entry.Truncated = truncated
		return entry, truncated, notes
	}
	return entry, false, notes
}

// readContainerLogs reads one container's log and returns its lines.
func (k *kubeTools) readContainerLogs(
	ctx context.Context, namespace, podName, container string, previous bool, tail, since int64,
) ([]string, string, bool, error) {
	if k.podLogs == nil {
		return nil, "", false, fmt.Errorf("pod log reads are not configured")
	}
	options := &corev1.PodLogOptions{
		Container:    container,
		Previous:     previous,
		TailLines:    &tail,
		SinceSeconds: &since,
	}
	raw, err := k.podLogs.GetLogs(ctx, namespace, podName, options)
	if err != nil {
		return nil, "", false, err
	}
	if strings.TrimSpace(string(raw)) == "" {
		return nil, logSourceCurrent, false, nil
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	truncated := false
	if len(lines) > int(tail) {
		lines = lines[len(lines)-int(tail):]
		truncated = true
	}
	source := logSourceCurrent
	if previous {
		source = logSourcePrevious
	}
	return lines, source, truncated, nil
}

// events reads Events for the Target from both Event APIs and merges them.
func (k *kubeTools) events(ctx context.Context, page *v1alpha1.Page, scope Scope, raw json.RawMessage) (any, error) {
	var args eventArguments
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidArguments, err)
	}
	if err := scope.checkNamespace(page.Spec.Target.Namespace); err != nil {
		return nil, err
	}
	since := args.SinceSeconds
	if since <= 0 {
		since = defaultSinceSeconds
	}
	limit := args.Limit
	if limit <= 0 {
		limit = defaultEventLimit
	}
	if limit > maxEventLimit {
		limit = maxEventLimit
	}
	cutoff := k.now().Add(-time.Duration(since) * time.Second)
	result := eventResult{}
	var notes []string
	core, note, err := k.coreEvents(ctx, page, cutoff)
	if err != nil {
		notes = append(notes, note)
	} else {
		result.Events = append(result.Events, core...)
	}
	modern, note, err := k.modernEvents(ctx, page, cutoff)
	if err != nil {
		notes = append(notes, note)
	} else {
		result.Events = append(result.Events, modern...)
	}
	if len(result.Events) == 0 && len(notes) == 0 {
		notes = append(notes, "no Events for the Target in the look-back window")
	}
	sortEvents(result.Events)
	if len(result.Events) > limit {
		result.Events = result.Events[len(result.Events)-limit:]
		result.Truncated = true
	}
	result.Notes = notes
	return result, nil
}

// coreEvents reads the core/v1 Event API.
func (k *kubeTools) coreEvents(
	ctx context.Context, page *v1alpha1.Page, cutoff time.Time,
) ([]eventEntry, string, error) {
	var list corev1.EventList
	if err := k.reader.List(ctx, &list, client.InNamespace(page.Spec.Target.Namespace)); err != nil {
		return nil, fmt.Sprintf("read core/v1 Events: %v", err), err
	}
	var entries []eventEntry
	for i := range list.Items {
		event := &list.Items[i]
		if !eventInvolves(coreReference(event.InvolvedObject), page.Spec.Target) {
			continue
		}
		at := event.LastTimestamp.Time
		if at.IsZero() {
			at = event.EventTime.Time
		}
		if !at.IsZero() && at.Before(cutoff) {
			continue
		}
		entries = append(entries, eventEntry{
			Time: at, Type: event.Type, Reason: event.Reason, Count: event.Count,
			Object: objectLabel(coreReference(event.InvolvedObject)), Message: event.Message, Source: "core/v1",
		})
	}
	return entries, "", nil
}

// modernEvents reads the events.k8s.io/v1 Event API.
func (k *kubeTools) modernEvents(
	ctx context.Context, page *v1alpha1.Page, cutoff time.Time,
) ([]eventEntry, string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "events.k8s.io", Version: "v1", Kind: "EventList"})
	if err := k.reader.List(ctx, list, client.InNamespace(page.Spec.Target.Namespace)); err != nil {
		return nil, fmt.Sprintf("read events.k8s.io/v1 Events: %v", err), err
	}
	var entries []eventEntry
	for i := range list.Items {
		item := &list.Items[i]
		regarding, _, _ := unstructured.NestedMap(item.Object, "regarding")
		if !eventInvolves(unstructuredReference(regarding), page.Spec.Target) {
			continue
		}
		at, _, _ := unstructured.NestedString(item.Object, "eventTime")
		parsed, _ := time.Parse(time.RFC3339, at)
		if !parsed.IsZero() && parsed.Before(cutoff) {
			continue
		}
		entry := eventEntry{Time: parsed, Source: "events.k8s.io/v1"}
		entry.Type, _, _ = unstructured.NestedString(item.Object, "type")
		entry.Reason, _, _ = unstructured.NestedString(item.Object, "reason")
		entry.Message, _, _ = unstructured.NestedString(item.Object, "note")
		entry.Object = objectLabel(unstructuredReference(regarding))
		count, _, _ := unstructured.NestedInt64(item.Object, "deprecatedCount")
		entry.Count = int32(count)
		entries = append(entries, entry)
	}
	return entries, "", nil
}

// ownerChain returns the owner chain of one object.
func (k *kubeTools) ownerChain(
	ctx context.Context, page *v1alpha1.Page, scope Scope, raw json.RawMessage,
) (any, error) {
	var args getArguments
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidArguments, err)
	}
	if err := scope.checkKind(args.Kind); err != nil {
		return nil, err
	}
	if err := scope.checkArgumentsNamespace(args.Namespace, page.Spec.Target.Namespace); err != nil {
		return nil, err
	}
	gv, err := gvk(args.Kind, args.APIVersion)
	if err != nil {
		return nil, err
	}
	if args.Name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidArguments)
	}
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(gv)
	key := client.ObjectKey{Namespace: page.Spec.Target.Namespace, Name: args.Name}
	if err := k.reader.Get(ctx, key, object); err != nil {
		return nil, fmt.Errorf("read %s %s: %w", args.Kind, args.Name, err)
	}
	chain := []ownerEntry{{
		Kind: object.GetKind(), Name: object.GetName(), Namespace: object.GetNamespace(), UID: string(object.GetUID()),
	}}
	current := object
	for range 8 {
		owner := metav1.GetControllerOf(current)
		if owner == nil {
			break
		}
		chain = append(chain, ownerEntry{
			Kind: owner.Kind, Name: owner.Name, Namespace: current.GetNamespace(), UID: string(owner.UID), Controller: true,
		})
		next := &unstructured.Unstructured{}
		next.SetGroupVersionKind(schema.FromAPIVersionAndKind(owner.APIVersion, owner.Kind))
		if err := k.reader.Get(ctx, client.ObjectKey{Namespace: current.GetNamespace(), Name: owner.Name}, next); err != nil {
			break
		}
		if string(next.GetUID()) != string(owner.UID) {
			break
		}
		current = next
	}
	return map[string]any{"start": args.Kind + "/" + args.Name, "chain": chain}, nil
}

// renderObject returns the bounded rendering of one object.
func renderObject(object *unstructured.Unstructured) objectResult {
	result := objectResult{
		APIVersion: object.GetAPIVersion(),
		Kind:       object.GetKind(),
		Metadata: map[string]any{
			fieldName:      object.GetName(),
			fieldNamespace: object.GetNamespace(),
			"uid":          string(object.GetUID()),
		},
	}
	if created := object.GetCreationTimestamp(); !created.IsZero() {
		result.Metadata["creationTimestamp"] = created.UTC().Format(time.RFC3339)
	}
	if generation := object.GetGeneration(); generation != 0 {
		result.Metadata["generation"] = generation
	}
	if objectLabels := object.GetLabels(); len(objectLabels) > 0 {
		result.Metadata["labels"] = objectLabels
	}
	if annotations := object.GetAnnotations(); len(annotations) > 0 {
		result.Metadata["annotations"] = annotations
	}
	if spec, ok, err := unstructured.NestedFieldNoCopy(object.Object, "spec"); err == nil && ok {
		result.Spec = spec
	}
	if status, ok, err := unstructured.NestedFieldNoCopy(object.Object, "status"); err == nil && ok {
		result.Status = status
	}
	if data, ok, err := unstructured.NestedFieldNoCopy(object.Object, "data"); err == nil && ok {
		if mapped, isMap := data.(map[string]any); isMap {
			result.Data = mapped
		}
	}
	return result
}

// checkObjectNamespace refuses an object that reports a different namespace.
func checkObjectNamespace(object *unstructured.Unstructured, namespace string) error {
	if object.GetNamespace() != "" && object.GetNamespace() != namespace {
		return fmt.Errorf("%w: %s is outside the Page's namespace", ErrOutOfScope, object.GetName())
	}
	return nil
}

// eventInvolves reports whether an Event's involved object is the Target.
func eventInvolves(reference v1alpha1.TargetReference, target v1alpha1.TargetReference) bool {
	referenceKind, targetKind := string(reference.Kind), string(target.Kind)
	if referenceKind != "" && referenceKind != targetKind {
		return false
	}
	if reference.Name != "" && reference.Name != target.Name {
		return false
	}
	if reference.Namespace != "" && reference.Namespace != target.Namespace {
		return false
	}
	if target.UID != "" && reference.UID != "" && reference.UID != target.UID {
		return false
	}
	return true
}

// coreReference converts a core/v1 object reference to a TargetReference.
func coreReference(reference corev1.ObjectReference) v1alpha1.TargetReference {
	return v1alpha1.TargetReference{
		Kind: v1alpha1.TargetKind(reference.Kind), Name: reference.Name,
		Namespace: reference.Namespace, UID: string(reference.UID),
	}
}

// unstructuredReference converts an Event's object reference to a TargetReference.
func unstructuredReference(reference map[string]any) v1alpha1.TargetReference {
	result := v1alpha1.TargetReference{}
	if reference == nil {
		return result
	}
	kind, _, _ := unstructured.NestedString(reference, fieldKind)
	name, _, _ := unstructured.NestedString(reference, fieldName)
	namespace, _, _ := unstructured.NestedString(reference, fieldNamespace)
	uid, _, _ := unstructured.NestedString(reference, "uid")
	result.Kind = v1alpha1.TargetKind(kind)
	result.Name = name
	result.Namespace = namespace
	result.UID = uid
	return result
}

// objectLabel renders an object reference for an Event entry.
func objectLabel(reference v1alpha1.TargetReference) string {
	if reference.Namespace == "" {
		return string(reference.Kind) + "/" + reference.Name
	}
	return string(reference.Kind) + "/" + reference.Namespace + "/" + reference.Name
}

// sortEvents orders events oldest first, keeping the two APIs' entries together.
func sortEvents(events []eventEntry) {
	slices.SortStableFunc(events, func(a, b eventEntry) int { return a.Time.Compare(b.Time) })
}

// sortedKeys returns a map's keys in ascending order.
func sortedKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
