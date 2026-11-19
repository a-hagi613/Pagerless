// Package logs holds the read-only log Connectors: the mandatory native
// Kubernetes collector and the opt-in backends. Every Connector implements
// contract.LogConnector, whose only method queries logs.
package logs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/connectors/contract"
)

// NativeName is the connector name recorded in evidence for the mandatory
// Kubernetes log collector.
const NativeName = "native-k8s-logs"

// ErrTargetUnsupported means the collector cannot select Pods for the Target
// kind. It degrades the native section rather than failing the Page.
var ErrTargetUnsupported = errors.New("native logs do not support this Target kind")

// PodLogReader reads the pod log subresource. It is an interface so the
// collector can be tested without an API server.
type PodLogReader interface {
	// GetLogs opens one container's log stream.
	GetLogs(ctx context.Context, namespace, pod string, options corev1.PodLogOptions) (io.ReadCloser, error)
}

// KubernetesPodLogs reads pod logs through the Kubernetes client set.
type KubernetesPodLogs struct {
	// Client is the client set.
	Client kubernetes.Interface
}

// GetLogs opens one container's log stream.
func (k *KubernetesPodLogs) GetLogs(
	ctx context.Context, namespace, pod string, options corev1.PodLogOptions,
) (io.ReadCloser, error) {
	request := k.Client.CoreV1().Pods(namespace).GetLogs(pod, &options)
	stream, err := request.Stream(ctx)
	if err != nil {
		return nil, err
	}
	return stream, nil
}

// Native collects the logs of the failing Target from the Kubernetes API. It is
// always enabled: the Kubernetes API is the only backend every cluster has.
type Native struct {
	// Reader lists Pods and their owners.
	Reader client.Reader
	// Logs reads the pod log subresource.
	Logs PodLogReader
	// Config bounds the collection.
	Config struct {
		TailLines           int64
		LimitBytes          int64
		SinceSeconds        int64
		MaxPods             int
		MaxContainersPerPod int
		Concurrency         int
	}
	// Clock defaults to the real clock.
	Clock clock.PassiveClock
}

// NewNative builds the mandatory collector from the validated configuration.
func NewNative(reader client.Reader, podLogs PodLogReader, tailLines, limitBytes, sinceSeconds int64,
	maxPods, maxContainers, concurrency int, now clock.PassiveClock,
) *Native {
	if now == nil {
		now = clock.RealClock{}
	}
	return &Native{
		Reader: reader, Logs: podLogs, Clock: now,
		Config: struct {
			TailLines           int64
			LimitBytes          int64
			SinceSeconds        int64
			MaxPods             int
			MaxContainersPerPod int
			Concurrency         int
		}{
			TailLines: tailLines, LimitBytes: limitBytes, SinceSeconds: sinceSeconds,
			MaxPods: maxPods, MaxContainersPerPod: maxContainers, Concurrency: concurrency,
		},
	}
}

// Name returns the connector name.
func (n *Native) Name() string { return NativeName }

// QueryLogs reads the previous container log first for every restarted
// container, falls back to the current log, and records a deleted Pod without
// retrying. It returns at most the configured number of Pods, containers, tail
// lines and bytes.
func (n *Native) QueryLogs(ctx context.Context, q contract.Query) (contract.LogPage, error) {
	pods, err := n.selectPods(ctx, q.Subject)
	if err != nil {
		return contract.LogPage{}, err
	}
	tasks := make([]logTask, 0, len(pods)*2)
	for _, pod := range pods {
		for _, container := range selectContainers(&pod, n.Config.MaxContainersPerPod) {
			tasks = append(tasks, logTask{pod: pod.Name, container: container.name, previous: container.previous})
		}
	}
	if len(tasks) == 0 {
		return contract.LogPage{Notes: []string{"no Pods matched the Target for native log collection"}}, nil
	}
	results := n.readAll(ctx, q, tasks)
	page := contract.LogPage{}
	limit := q.Limit
	if limit <= 0 || limit > 20000 {
		limit = 20000
	}
	totalBudget := n.Config.LimitBytes * int64(len(tasks))
	if totalBudget <= 0 || totalBudget > 2<<20 {
		totalBudget = 2 << 20
	}
	remaining := totalBudget
	for _, result := range results {
		page.Notes = append(page.Notes, result.notes...)
		if result.truncated || result.previousFallback {
			page.Truncated = true
		}
		for _, entry := range result.entries {
			if len(page.Entries) >= limit || remaining <= 0 {
				page.Truncated = true
				break
			}
			size := int64(len(entry.Line))
			if size > remaining {
				entry.Line = entry.Line[:remaining]
				page.Truncated = true
				size = remaining
			}
			page.Entries = append(page.Entries, entry)
			page.Bytes += int(size)
			remaining -= size
		}
	}
	slices.SortStableFunc(page.Entries, func(a, b contract.LogEntry) int {
		return a.Timestamp.Compare(b.Timestamp)
	})
	return page, nil
}

// logTask is one container log read.
type logTask struct {
	pod       string
	container string
	previous  bool
}

// logResult is one completed read.
type logResult struct {
	entries          []contract.LogEntry
	notes            []string
	truncated        bool
	previousFallback bool
}

// readAll reads the tasks with bounded concurrency and returns results in task
// order, so the bundle is deterministic.
func (n *Native) readAll(ctx context.Context, q contract.Query, tasks []logTask) []logResult {
	results := make([]logResult, len(tasks))
	concurrency := max(1, n.Config.Concurrency)
	semaphore := make(chan struct{}, concurrency)
	var wait sync.WaitGroup
	for index := range tasks {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			results[index] = n.read(ctx, q, tasks[index])
		}(index)
	}
	wait.Wait()
	return results
}

// read reads one container's log, previous first when it has restarted.
func (n *Native) read(ctx context.Context, q contract.Query, task logTask) logResult {
	now := n.Clock.Now()
	window := int64(0)
	if !q.Since.IsZero() {
		window = int64(now.Sub(q.Since).Seconds())
		if window <= 0 {
			window = 1
		}
	}
	if limit := n.Config.SinceSeconds; limit > 0 && (window == 0 || window > limit) {
		window = limit
	}
	options := corev1.PodLogOptions{
		Container:  task.container,
		Timestamps: true,
		TailLines:  new(n.Config.TailLines),
		LimitBytes: new(n.Config.LimitBytes),
	}
	if window > 0 {
		options.SinceSeconds = new(window)
	}
	result := logResult{}
	if task.previous {
		options.Previous = true
		entries, bytesRead, err := n.stream(ctx, q.Namespace, task, options)
		if err == nil && len(entries) > 0 {
			result.entries = entries
			result.truncated = bytesRead >= n.Config.LimitBytes
			return result
		}
		if err != nil && isDeleted(err) {
			result.notes = append(result.notes, fmt.Sprintf(
				"Pod %s in namespace %s was deleted; its %s logs are unavailable", task.pod, q.Namespace, task.container))
			return result
		}
		// The previous container never terminated, or its log is empty. The
		// current log is the only evidence available.
		options.Previous = false
		result.previousFallback = true
	}
	entries, bytesRead, err := n.stream(ctx, q.Namespace, task, options)
	if err != nil {
		if isDeleted(err) {
			result.notes = append(result.notes, fmt.Sprintf(
				"Pod %s in namespace %s was deleted; its %s logs are unavailable", task.pod, q.Namespace, task.container))
			return result
		}
		result.notes = append(result.notes, fmt.Sprintf(
			"container %s in Pod %s could not be read: %s", task.container, task.pod, shortError(err)))
		return result
	}
	result.entries = entries
	result.truncated = bytesRead >= n.Config.LimitBytes
	return result
}

// stream reads one container log into entries.
func (n *Native) stream(
	ctx context.Context, namespace string, task logTask, options corev1.PodLogOptions,
) ([]contract.LogEntry, int64, error) {
	if n.Logs == nil {
		return nil, 0, errors.New("no pod log reader is configured")
	}
	stream, err := n.Logs.GetLogs(ctx, namespace, task.pod, options)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = stream.Close() }()
	limited := io.LimitReader(stream, n.Config.LimitBytes+1)
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	entries := make([]contract.LogEntry, 0, 64)
	var bytesRead int64
	for scanner.Scan() {
		line := scanner.Text()
		bytesRead += int64(len(line)) + 1
		timestamp, text := splitTimestamp(line)
		entries = append(entries, contract.LogEntry{
			Timestamp: timestamp, Line: text, Stream: map[string]string{
				"pod": task.pod, "container": task.container, "previous": fmt.Sprint(options.Previous),
			},
		})
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return entries, bytesRead, err
	}
	return entries, bytesRead, nil
}

// selectPods lists and orders the Pods a bundle reads.
func (n *Native) selectPods(ctx context.Context, target v1alpha1.TargetReference) ([]corev1.Pod, error) {
	if n.Reader == nil {
		return nil, errors.New("no reader is configured for native logs")
	}
	pods, err := n.candidatePods(ctx, target)
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(pods, func(a, b corev1.Pod) int {
		af, bf := failing(&a), failing(&b)
		if af != bf {
			if af {
				return -1
			}
			return 1
		}
		return a.CreationTimestamp.Compare(b.CreationTimestamp.Time)
	})
	spread := make([]corev1.Pod, 0, min(len(pods), n.Config.MaxPods))
	seenNodes := make(map[string]struct{}, len(pods))
	for i := range pods {
		if _, seen := seenNodes[pods[i].Spec.NodeName]; seen {
			continue
		}
		seenNodes[pods[i].Spec.NodeName] = struct{}{}
		spread = append(spread, pods[i])
		if len(spread) == n.Config.MaxPods {
			return spread, nil
		}
	}
	for i := range pods {
		if len(spread) == n.Config.MaxPods {
			break
		}
		if slices.ContainsFunc(spread, func(existing corev1.Pod) bool { return existing.UID == pods[i].UID }) {
			continue
		}
		spread = append(spread, pods[i])
	}
	return spread, nil
}

// PodsForTarget returns the Pods that belong to a Target. The Context Bundle
// uses it to summarize Pod states without a second ownership implementation.
func PodsForTarget(ctx context.Context, reader client.Reader, target v1alpha1.TargetReference) ([]corev1.Pod, error) {
	native := &Native{Reader: reader}
	return native.candidatePods(ctx, target)
}

// candidatePods lists the Pods that belong to the Target.
func (n *Native) candidatePods(
	ctx context.Context, target v1alpha1.TargetReference,
) ([]corev1.Pod, error) {
	switch target.Kind {
	case v1alpha1.TargetKindPod:
		var pod corev1.Pod
		if err := n.Reader.Get(ctx, client.ObjectKey{Namespace: target.Namespace, Name: target.Name}, &pod); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("read Pod %s/%s: %w", target.Namespace, target.Name, err)
		}
		return []corev1.Pod{pod}, nil
	case v1alpha1.TargetKindDeployment, v1alpha1.TargetKindStatefulSet, v1alpha1.TargetKindDaemonSet,
		v1alpha1.TargetKindJob, v1alpha1.TargetKindCronJob:
		return n.workloadPods(ctx, target)
	case v1alpha1.TargetKindPersistentVolumeClaim:
		var pods corev1.PodList
		if err := n.Reader.List(ctx, &pods, client.InNamespace(target.Namespace)); err != nil {
			return nil, fmt.Errorf("list Pods in %s: %w", target.Namespace, err)
		}
		matches := make([]corev1.Pod, 0, 4)
		for i := range pods.Items {
			if consumesClaim(&pods.Items[i], target.Name) {
				matches = append(matches, pods.Items[i])
			}
		}
		return matches, nil
	case v1alpha1.TargetKindNode:
		var pods corev1.PodList
		if err := n.Reader.List(ctx, &pods); err != nil {
			return nil, fmt.Errorf("list Pods for Node %s: %w", target.Name, err)
		}
		matches := make([]corev1.Pod, 0, 8)
		for i := range pods.Items {
			if pods.Items[i].Spec.NodeName == target.Name {
				matches = append(matches, pods.Items[i])
			}
		}
		return matches, nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrTargetUnsupported, target.Kind)
	}
}

// workloadPods selects the Pods of a workload through its controller ownership.
func (n *Native) workloadPods(
	ctx context.Context, target v1alpha1.TargetReference,
) ([]corev1.Pod, error) {
	var pods corev1.PodList
	if err := n.Reader.List(ctx, &pods, client.InNamespace(target.Namespace)); err != nil {
		return nil, fmt.Errorf("list Pods in %s: %w", target.Namespace, err)
	}
	match := ownerMatcher{kind: target.Kind, name: target.Name, uid: types.UID(target.UID)}
	switch target.Kind {
	case v1alpha1.TargetKindDeployment:
		replicas, err := matchingReplicaSets(ctx, n.Reader, target)
		if err != nil {
			return nil, err
		}
		match.replicaSets = replicas
	case v1alpha1.TargetKindCronJob:
		jobs, err := matchingJobs(ctx, n.Reader, target)
		if err != nil {
			return nil, err
		}
		match.jobs = jobs
	}
	matches := make([]corev1.Pod, 0, 8)
	for i := range pods.Items {
		if match.matches(&pods.Items[i]) {
			matches = append(matches, pods.Items[i])
		}
	}
	return matches, nil
}

// ownerMatcher matches a Pod against a workload through its owner references.
type ownerMatcher struct {
	kind        v1alpha1.TargetKind
	name        string
	uid         types.UID
	replicaSets map[types.UID]bool
	jobs        map[types.UID]bool
}

// matches reports whether the Pod belongs to the workload.
func (m ownerMatcher) matches(pod *corev1.Pod) bool {
	reference := controllerOf(&pod.ObjectMeta)
	if reference == nil {
		return false
	}
	switch m.kind {
	case v1alpha1.TargetKindDeployment:
		return m.replicaSets[reference.UID]
	case v1alpha1.TargetKindCronJob:
		return m.jobs[reference.UID]
	default:
		return reference.Kind == string(m.kind) && referenceMatches(reference, m.name, m.uid)
	}
}

// matchingReplicaSets returns the ReplicaSets owned by a Deployment.
func matchingReplicaSets(
	ctx context.Context, reader client.Reader, target v1alpha1.TargetReference,
) (map[types.UID]bool, error) {
	var list appsv1.ReplicaSetList
	if err := reader.List(ctx, &list, client.InNamespace(target.Namespace)); err != nil {
		return nil, fmt.Errorf("list ReplicaSets in %s: %w", target.Namespace, err)
	}
	matching := make(map[types.UID]bool, len(list.Items))
	for i := range list.Items {
		reference := controllerOf(&list.Items[i].ObjectMeta)
		if reference != nil && reference.Kind == "Deployment" && referenceMatches(reference, target.Name,
			types.UID(target.UID)) {
			matching[list.Items[i].UID] = true
		}
	}
	return matching, nil
}

// matchingJobs returns the Jobs owned by a CronJob.
func matchingJobs(
	ctx context.Context, reader client.Reader, target v1alpha1.TargetReference,
) (map[types.UID]bool, error) {
	var list batchv1.JobList
	if err := reader.List(ctx, &list, client.InNamespace(target.Namespace)); err != nil {
		return nil, fmt.Errorf("list Jobs in %s: %w", target.Namespace, err)
	}
	matching := make(map[types.UID]bool, len(list.Items))
	for i := range list.Items {
		reference := controllerOf(&list.Items[i].ObjectMeta)
		if reference != nil && reference.Kind == "CronJob" && referenceMatches(reference, target.Name,
			types.UID(target.UID)) {
			matching[list.Items[i].UID] = true
		}
	}
	return matching, nil
}

// controllerOf returns the Pod's controller owner reference.
func controllerOf(meta *metav1.ObjectMeta) *metav1.OwnerReference {
	return metav1.GetControllerOf(meta)
}

// referenceMatches compares an owner reference to a name and an optional UID.
func referenceMatches(reference *metav1.OwnerReference, name string, uid types.UID) bool {
	if uid != "" {
		return reference.UID == uid
	}
	return reference.Name == name
}

// selectContainers returns the containers to read, failing first.
func selectContainers(pod *corev1.Pod, limit int) []containerRef {
	candidates := make([]containerRef, 0, len(pod.Status.ContainerStatuses)+len(pod.Status.InitContainerStatuses))
	for i := range pod.Status.InitContainerStatuses {
		status := &pod.Status.InitContainerStatuses[i]
		if status.LastTerminationState.Terminated == nil || status.LastTerminationState.Terminated.ExitCode == 0 {
			continue
		}
		candidates = append(candidates, containerRef{
			name: status.Name, previous: true, failing: true,
		})
	}
	for i := range pod.Status.ContainerStatuses {
		status := &pod.Status.ContainerStatuses[i]
		previous := status.RestartCount > 0 || status.LastTerminationState.Terminated != nil
		candidates = append(candidates, containerRef{
			name: status.Name, previous: previous, failing: containerFailing(status),
		})
	}
	slices.SortStableFunc(candidates, func(a, b containerRef) int {
		if a.failing == b.failing {
			return 0
		}
		if a.failing {
			return -1
		}
		return 1
	})
	if limit > 0 && len(candidates) > limit {
		candidates = candidates[:limit]
	}
	return candidates
}

// containerRef is one container to read.
type containerRef struct {
	name     string
	previous bool
	failing  bool
}

// containerFailing reports whether the container is waiting, terminated with a
// failure, or not ready.
func containerFailing(status *corev1.ContainerStatus) bool {
	if status.State.Waiting != nil || !status.Ready {
		return true
	}
	if status.State.Terminated != nil && status.State.Terminated.ExitCode != 0 {
		return true
	}
	return status.LastTerminationState.Terminated != nil && status.LastTerminationState.Terminated.ExitCode != 0
}

// failing reports whether the Pod shows a failure that makes its logs valuable.
func failing(pod *corev1.Pod) bool {
	if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodPending {
		return true
	}
	for i := range pod.Status.ContainerStatuses {
		if containerFailing(&pod.Status.ContainerStatuses[i]) {
			return true
		}
	}
	return false
}

// consumesClaim reports whether the Pod mounts the claim.
func consumesClaim(pod *corev1.Pod, claim string) bool {
	for i := range pod.Spec.Volumes {
		volume := &pod.Spec.Volumes[i]
		if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == claim {
			return true
		}
	}
	return false
}

// splitTimestamp separates the RFC3339Nano timestamp the pod log subresource
// prepends when timestamps are requested.
func splitTimestamp(line string) (time.Time, string) {
	first, rest, found := strings.Cut(line, " ")
	if !found {
		return time.Time{}, line
	}
	parsed, err := time.Parse(time.RFC3339Nano, first)
	if err != nil {
		return time.Time{}, line
	}
	return parsed, rest
}

// isDeleted reports whether the log read failed because the Pod is gone.
func isDeleted(err error) bool {
	return apierrors.IsNotFound(err) || strings.Contains(err.Error(), "not found")
}

// shortError bounds an error for a note.
func shortError(err error) string {
	return truncate(err.Error(), 200)
}

// truncate bounds text.
func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}
