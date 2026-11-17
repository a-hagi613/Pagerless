// Package rollback performs Validation for the Rollback Action: it decides
// whether the requested commit is one that the Workload may revert, renders the
// revert as an exact git patch against the current Sandbox commit, and decides
// when a rendered Proposal has become a Stale Proposal (ADR-0002, ADR-0008,
// ADR-0021).
package rollback

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/proposal"
	"github.com/a-hagi613/pagerless/internal/sandbox"
)

const (
	// Window is the number of commits that a Workload may roll back: the last 20
	// commits touching its own path or a path it declares it depends on.
	Window = 20
	// maxPatchBytes is the largest patch that a Proposal can store.
	maxPatchBytes = 32768
	// maxMessageLength bounds a message that is copied from another system into a Validation error.
	maxMessageLength = 1000
	// forbiddenRoot is the top-level Sandbox directory whose files GitHub runs as
	// workflows with the repository's credentials. The Executor refuses a change
	// there, so Validation refuses a revert of one.
	forbiddenRoot = ".github"
)

const (
	fieldPrefix   = "proposal.parameters.rollback"
	fieldCommit   = fieldPrefix + ".commit"
	fieldWorkload = "workload"
)

var errNoLiveWorkload = errors.New("the Workload does not exist")

// Config configures the Rollback validator.
type Config struct {
	// Reader reads Workloads directly from the API server, not from a cache.
	Reader client.Reader
	// Sandbox is the repository that holds the Workloads' manifests.
	Sandbox sandbox.Repository
	// AllowedNamespaces are the namespaces whose Workloads may be rolled back. None are allowed when it is empty.
	AllowedNamespaces []string
}

// Validator validates Rollback Proposals and renders their revert.
type Validator struct {
	cfg Config
}

// New returns a Validator.
func New(cfg Config) *Validator { return &Validator{cfg: cfg} }

var _ proposal.ActionValidator = (*Validator)(nil)
var _ proposal.StalenessChecker = (*Validator)(nil)

// commitParameter is the shape that the Triage schema admits for a commit.
var commitParameter = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// Validate checks the Proposal and, when it passes, renders the revert of the
// one commit the Proposal names.
func (v *Validator) Validate(ctx context.Context, req proposal.Request) (proposal.Outcome, error) {
	var params v1alpha1.RollbackParameters
	if p := req.Action.Parameters; p != nil && p.Rollback != nil {
		params = *p.Rollback
	} else {
		return refuse(fieldPrefix, "rollback parameters are required"), nil
	}
	workload := req.Triage.Spec.Workload

	var errs []agentapi.ValidationError
	if !slices.Contains(v.cfg.AllowedNamespaces, workload.Namespace) {
		errs = append(errs, agentapi.ValidationError{
			Field:   "workload.namespace",
			Message: fmt.Sprintf("namespace %s is not in the allowlist for Rollback", workload.Namespace),
		})
	}
	if !commitParameter.MatchString(params.Commit) {
		errs = append(errs, agentapi.ValidationError{
			Field:   fieldCommit,
			Message: "commit must be 7 to 64 lowercase hexadecimal digits",
		})
	}
	if len(errs) > 0 {
		return proposal.Outcome{Errors: errs}, nil
	}

	live, err := v.workload(ctx, workload)
	if errors.Is(err, errNoLiveWorkload) {
		return refuse(fieldWorkload,
			fmt.Sprintf("%s %s/%s does not exist", workload.Kind, workload.Namespace, workload.Name)), nil
	}
	if err != nil {
		return proposal.Outcome{}, err
	}
	paths, errs := allowedPaths(live.object.GetAnnotations())
	if len(errs) > 0 {
		return proposal.Outcome{Errors: errs}, nil
	}

	history, err := v.cfg.Sandbox.History(ctx)
	if err != nil {
		return proposal.Outcome{}, err
	}
	defer func() { _ = history.Close() }()

	commit, err := history.Resolve(ctx, params.Commit)
	if errors.Is(err, sandbox.ErrNoCommit) {
		return refuse(fieldCommit, fmt.Sprintf("commit %s is not in the Sandbox", params.Commit)), nil
	}
	if err != nil {
		return proposal.Outcome{}, err
	}
	window, err := history.Commits(ctx, paths, Window)
	if err != nil {
		return proposal.Outcome{}, err
	}
	if !slices.Contains(window, commit) {
		return refuse(fieldCommit, fmt.Sprintf(
			"commit %s is not among the last %d commits touching %s or its declared dependency paths",
			commit, Window, strings.Join(paths, ", "))), nil
	}

	revert, err := history.Revert(ctx, commit)
	switch {
	case errors.Is(err, sandbox.ErrRevertConflict):
		return refuse(fieldCommit, truncate(fmt.Sprintf(
			"commit %s does not revert cleanly against the current Sandbox commit %s: %v",
			commit, history.Commit, err), maxMessageLength)), nil
	case errors.Is(err, sandbox.ErrRevertNotOneFile):
		return refuse(fieldCommit, fmt.Sprintf(
			"reverting commit %s would change more than one file, and an approved change may touch only one", commit)), nil
	case errors.Is(err, sandbox.ErrAlreadyReverted):
		return refuse(fieldCommit, fmt.Sprintf("commit %s is already reverted in the Sandbox", commit)), nil
	case err != nil:
		return proposal.Outcome{}, err
	}
	if first, _, _ := strings.Cut(revert.Path, "/"); first == forbiddenRoot {
		return refuse(fieldCommit, fmt.Sprintf(
			"the revert of commit %s would change %s, which an approved change may not touch", commit, revert.Path)), nil
	}
	if len(revert.Patch) > maxPatchBytes {
		return refuse(fieldPrefix, fmt.Sprintf("the rendered patch exceeds %d bytes", maxPatchBytes)), nil
	}
	return proposal.Outcome{
		Change: &v1alpha1.RenderedChange{Path: revert.Path, Patch: revert.Patch},
		Base: &v1alpha1.ProposalBase{
			Commit:             history.Commit,
			WorkloadGeneration: live.object.GetGeneration(),
		},
	}, nil
}

func refuse(field, message string) proposal.Outcome {
	return proposal.Outcome{Errors: refuseErrors(field, message)}
}

// allowedPaths returns the Workload's own Sandbox path followed by the
// dependency paths it declares, with the Validation errors that make them
// unusable.
func allowedPaths(annotations map[string]string) ([]string, []agentapi.ValidationError) {
	sourcePath := annotations[v1alpha1.AnnotationSourcePath]
	if sourcePath == "" {
		return nil, refuseErrors(fieldWorkload, fmt.Sprintf(
			"the Workload has no %s annotation, so its manifest cannot be located", v1alpha1.AnnotationSourcePath))
	}
	if err := sandbox.CheckPath(sourcePath); err != nil {
		return nil, refuseErrors(fieldWorkload, fmt.Sprintf("annotation %s: %v", v1alpha1.AnnotationSourcePath, err))
	}
	paths := []string{sourcePath}
	for _, p := range v1alpha1.DependsOnPaths(annotations) {
		if err := sandbox.CheckPath(p); err != nil {
			return nil, refuseErrors(fieldWorkload,
				fmt.Sprintf("annotation %s: %v", v1alpha1.AnnotationDependsOnPaths, err))
		}
		paths = append(paths, p)
	}
	return paths, nil
}

func refuseErrors(field, message string) []agentapi.ValidationError {
	return []agentapi.ValidationError{{Field: field, Message: message}}
}

// liveWorkload is a Deployment or StatefulSet read from the cluster.
type liveWorkload struct {
	object client.Object
}

func (v *Validator) workload(ctx context.Context, ref v1alpha1.WorkloadReference) (*liveWorkload, error) {
	key := client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}
	var live liveWorkload
	switch ref.Kind {
	case v1alpha1.WorkloadKindDeployment:
		live = liveWorkload{object: &appsv1.Deployment{}}
	case v1alpha1.WorkloadKindStatefulSet:
		live = liveWorkload{object: &appsv1.StatefulSet{}}
	default:
		return nil, fmt.Errorf("unsupported Workload kind %q", ref.Kind)
	}
	if err := v.cfg.Reader.Get(ctx, key, live.object); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errNoLiveWorkload
		}
		return nil, fmt.Errorf("read %s %s: %w", ref.Kind, key, err)
	}
	return &live, nil
}

// Stale reports why the Proposal's base no longer holds, or an empty string
// when it still does. A Rollback Proposal holds while the Sandbox's tip is the
// commit it was rendered against, or while re-rendering the same revert against
// the new tip gives the same patch.
func (v *Validator) Stale(ctx context.Context, t *v1alpha1.Triage) (string, error) {
	p := t.Status.Proposal
	if p == nil || p.Base == nil || p.RenderedChange == nil ||
		p.Parameters == nil || p.Parameters.Rollback == nil {
		return "", nil
	}
	head, err := v.cfg.Sandbox.Head(ctx)
	if err != nil {
		return "", err
	}
	if head == p.Base.Commit {
		return "", nil
	}
	live, err := v.workload(ctx, t.Spec.Workload)
	if errors.Is(err, errNoLiveWorkload) {
		return v1alpha1.ReasonWorkloadDeleted, nil
	}
	if err != nil {
		return "", err
	}
	paths, errs := allowedPaths(live.object.GetAnnotations())
	if len(errs) > 0 {
		return v1alpha1.ReasonSourcePathChanged, nil
	}
	// The Proposal holds only while the file it reverts is still one of the paths
	// the Workload declares, because otherwise the window itself has changed.
	if !slices.Contains(paths, p.RenderedChange.Path) {
		return v1alpha1.ReasonSourcePathChanged, nil
	}
	return v.revalidated(ctx, p, paths)
}

// revalidated decides whether the Proposal's revert still holds after the
// Sandbox moved, and names the reason when it does not.
func (v *Validator) revalidated(
	ctx context.Context, p *v1alpha1.Proposal, paths []string,
) (string, error) {
	history, err := v.cfg.Sandbox.History(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = history.Close() }()
	commit, err := history.Resolve(ctx, p.Parameters.Rollback.Commit)
	if errors.Is(err, sandbox.ErrNoCommit) {
		return v1alpha1.ReasonRollbackCommitChanged, nil
	}
	if err != nil {
		return "", err
	}
	window, err := history.Commits(ctx, paths, Window)
	if err != nil {
		return "", err
	}
	if !slices.Contains(window, commit) {
		return v1alpha1.ReasonRollbackCommitChanged, nil
	}
	revert, err := history.Revert(ctx, commit)
	switch {
	case errors.Is(err, sandbox.ErrRevertConflict), errors.Is(err, sandbox.ErrRevertNotOneFile),
		errors.Is(err, sandbox.ErrAlreadyReverted):
		return v1alpha1.ReasonSandboxFileChanged, nil
	case err != nil:
		return "", err
	}
	if revert.Path != p.RenderedChange.Path || revert.Patch != p.RenderedChange.Patch {
		return v1alpha1.ReasonSandboxFileChanged, nil
	}
	return "", nil
}

func truncate(s string, limit int) string {
	if r := []rune(s); len(r) > limit {
		return strings.TrimSpace(string(r[:limit]))
	}
	return s
}
