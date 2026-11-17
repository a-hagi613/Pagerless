package cli

import (
	"context"
	"fmt"
	"io"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// follower prints the step events of one Triage, and its phase changes, as
// the status of the Triage shows them.
type follower struct {
	cl  client.WithWatch
	key client.ObjectKey
	out *printer
	// seenSteps is the step count that the follower has already printed.
	seenSteps int32
	// phase is the phase that the follower has last printed.
	phase v1alpha1.Phase
}

// follow watches the status of a Triage and prints each new step and phase
// change. It returns the Triage once the Triage is Proposed, which is when the
// agent has finished and a human decision is next, or once it has ended.
func follow(ctx context.Context, cl client.WithWatch, key client.ObjectKey, w io.Writer) (*v1alpha1.Triage, error) {
	f := &follower{cl: cl, key: key, out: &printer{w: w}}
	for {
		var tr v1alpha1.Triage
		if err := cl.Get(ctx, key, &tr); err != nil {
			return nil, triageError(err, key.Name)
		}
		if f.observe(&tr) {
			return &tr, nil
		}
		done, err := f.watch(ctx, tr.ResourceVersion)
		if err != nil {
			return nil, err
		}
		if done != nil {
			return done, nil
		}
		// The watch ended without a verdict, so the loop reads the Triage again and resumes from there.
	}
}

// watch consumes the events of one watch that starts after resourceVersion. It
// returns the Triage once it is done, or nil when the watch ended first.
func (f *follower) watch(ctx context.Context, resourceVersion string) (*v1alpha1.Triage, error) {
	events, err := f.cl.Watch(ctx, &v1alpha1.TriageList{},
		client.InNamespace(f.key.Namespace),
		client.MatchingFields{"metadata.name": f.key.Name},
		&client.ListOptions{Raw: &metav1.ListOptions{ResourceVersion: resourceVersion}})
	if err != nil {
		return nil, fmt.Errorf("watch Triage %q: %w", f.key.Name, err)
	}
	defer events.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case event, ok := <-events.ResultChan():
			if !ok {
				return nil, nil
			}
			switch event.Type {
			case watch.Added, watch.Modified:
				tr, ok := event.Object.(*v1alpha1.Triage)
				if ok && f.observe(tr) {
					return tr, nil
				}
			case watch.Deleted:
				return nil, fmt.Errorf("triage %q was deleted while it was followed", f.key.Name)
			case watch.Error:
				return nil, nil
			}
		}
	}
}

// observe prints what has changed since the previous observation. It reports
// whether the Triage has reached a point where the watch ends.
func (f *follower) observe(tr *v1alpha1.Triage) bool {
	f.printNewSteps(tr)
	if tr.Status.Phase != f.phase {
		f.phase = tr.Status.Phase
		f.out.printf("phase: %s\n", phaseLabel(f.phase))
	}
	return tr.Status.Phase == v1alpha1.PhaseProposed || tr.Status.Phase.IsTerminal()
}

// printNewSteps prints the steps that the Triage has recorded since the previous observation.
func (f *follower) printNewSteps(tr *v1alpha1.Triage) {
	fresh := tr.Status.StepCount - f.seenSteps
	if fresh <= 0 {
		return
	}
	recorded := tr.Status.Steps
	shown := min(int(fresh), len(recorded))
	if omitted := int(fresh) - shown; omitted > 0 {
		f.out.printf("... %d earlier steps are no longer in the step summary\n", omitted)
	}
	for _, step := range recorded[len(recorded)-shown:] {
		f.out.printf("%s\n", stepLine(step))
	}
	f.seenSteps = tr.Status.StepCount
}

// stepLine formats one step for the watch output.
func stepLine(step v1alpha1.Step) string {
	kind := string(step.Kind)
	if step.Tool != "" {
		kind += " " + step.Tool
	}
	return fmt.Sprintf("%s %s: %s", step.Time.Format("15:04:05"), kind, step.Summary)
}
