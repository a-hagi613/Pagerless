// Package executor is pagerless-executor: the only component that holds write
// credentials (ADR-0005). It carries out an approved Proposal by committing
// the stored patch to the Sandbox and opening a pull request with auto-merge
// enabled (ADR-0011).
//
// The server asks for an execution by Triage and Proposal hash, but the
// Executor does not rely on the request alone: it confirms for itself that the
// Triage is Approved and that a TriageApproval exists for exactly that hash
// (ADR-0014), and that the stored change is the one the hash covers
// (ADR-0008). Only then does it use its credentials.
package executor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/executorapi"
	"github.com/a-hagi613/pagerless/internal/proposal"
	"github.com/a-hagi613/pagerless/internal/sandbox"
)

// proposalHash matches a Proposal hash.
var proposalHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// forbiddenRoot is the top-level Sandbox directory whose files GitHub runs
// as workflows with the repository's credentials. An approved change may never
// touch it.
const forbiddenRoot = ".github"

// Config configures an Executor.
type Config struct {
	// Reader reads Triages and TriageApprovals directly from the API server,
	// never from a cache, so that a decision just applied is always seen.
	Reader client.Reader
	// Writer deletes the Workload's pods for RestartWorkload. It is the same
	// client as Reader in every deployment.
	Writer client.Writer
	// Namespace is the namespace that holds Triages and TriageApprovals.
	Namespace string
	// Sandbox is the Sandbox repository that changes are committed to and pull requests target.
	Sandbox sandbox.Repository
	// GitHub opens the pull requests.
	GitHub *GitHub
	// Token returns the write-scoped token that git pushes with.
	Token func() (string, error)
	// RestartTimeout bounds one live RestartWorkload. It defaults to DefaultRestartTimeout.
	RestartTimeout time.Duration
}

// Executor carries out approved Proposals.
type Executor struct {
	cfg Config
	// mu serializes executions. The Executor runs as one replica and executes
	// rarely, so serializing is what makes concurrent requests for one hash
	// open one pull request.
	mu sync.Mutex
}

// New returns an Executor.
func New(cfg Config) (*Executor, error) {
	switch {
	case cfg.Reader == nil:
		return nil, errors.New("a reader is required")
	case cfg.Writer == nil:
		return nil, errors.New("a writer is required")
	case cfg.Namespace == "":
		return nil, errors.New("a namespace is required")
	case cfg.GitHub == nil || cfg.Token == nil || cfg.GitHub.Token == nil:
		return nil, errors.New("GitHub and a token are required")
	case !repositoryName.MatchString(cfg.GitHub.Repository):
		return nil, fmt.Errorf("%q is not a GitHub repository as <owner>/<name>", cfg.GitHub.Repository)
	}
	if err := cfg.Sandbox.Check(); err != nil {
		return nil, err
	}
	if cfg.RestartTimeout == 0 {
		cfg.RestartTimeout = DefaultRestartTimeout
	}
	return &Executor{cfg: cfg}, nil
}

func refuse(status int, reason executorapi.Reason, format string, args ...any) *executorapi.Error {
	return &executorapi.Error{StatusCode: status, Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// Execute carries out the Proposal with the hash for the named Triage and
// returns its pull request. It is idempotent per hash: when the pull request
// exists it is returned with its current state, and its auto-merge is turned
// on if an earlier attempt failed before that.
//
// An *executorapi.Error means the request was refused or malformed. Any other
// error is a failure that repeating the request may cure.
func (e *Executor) Execute(ctx context.Context, name, hash string) (*executorapi.Response, error) {
	tr, change, err := e.authorize(ctx, name, hash)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	if tr.Status.Proposal.Action.ExecutesLive() {
		return e.restart(ctx, tr)
	}

	branch := branchName(tr, hash)
	pr, err := e.cfg.GitHub.FindPullRequest(ctx, branch)
	if err != nil {
		return nil, err
	}
	if pr == nil {
		if pr, err = e.deliver(ctx, tr, change, branch); err != nil {
			return nil, err
		}
	}
	if pr.State == "open" && pr.AutoMerge == nil {
		if err := e.cfg.GitHub.EnableAutoMerge(ctx, pr); err != nil {
			return nil, err
		}
	}
	return answer(pr), nil
}

func answer(pr *PullRequest) *executorapi.Response {
	response := &executorapi.Response{PullRequestURL: pr.HTMLURL, State: executorapi.PullRequestOpen}
	switch {
	case pr.MergedAt != nil:
		response.State = executorapi.PullRequestMerged
		response.MergeCommit = pr.MergeCommitSHA
	case pr.State != "open":
		response.State = executorapi.PullRequestClosed
	}
	return response
}

// authorize is the Executor's own check of the Approval, which does not
// trust the request, the Triage's phase alone, or the stored change. It
// returns the Triage and the change to apply when every check passes.
func (e *Executor) authorize(
	ctx context.Context, name, hash string,
) (*v1alpha1.Triage, *v1alpha1.RenderedChange, error) {
	if !proposalHash.MatchString(hash) {
		return nil, nil, refuse(http.StatusBadRequest, executorapi.ReasonBadRequest,
			"the proposal hash must be 64 lowercase hexadecimal digits")
	}
	var tr v1alpha1.Triage
	if err := e.cfg.Reader.Get(ctx, types.NamespacedName{Namespace: e.cfg.Namespace, Name: name}, &tr); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, refuse(http.StatusNotFound, executorapi.ReasonTriageNotFound, "Triage %s does not exist", name)
		}
		return nil, nil, fmt.Errorf("get Triage %s: %w", name, err)
	}
	if err := checkTriage(&tr, hash); err != nil {
		return nil, nil, err
	}
	if err := e.checkApproval(ctx, &tr, hash); err != nil {
		return nil, nil, err
	}
	return &tr, tr.Status.Proposal.RenderedChange, nil
}

// checkTriage confirms what the Triage itself says: it is Approved or already
// Executing, its current Proposal has the hash, the Proposal delivers by pull
// request, and the stored change still hashes to the hash that was approved.
// A Triage moves to Executing only from Approved, and only the hash binds the
// execution to a decision, so Executing is treated like Approved.
func checkTriage(tr *v1alpha1.Triage, hash string) *executorapi.Error {
	if phase := tr.Status.Phase; phase != v1alpha1.PhaseApproved && phase != v1alpha1.PhaseExecuting {
		return refuse(http.StatusConflict, executorapi.ReasonNotApproved,
			"Triage %s is in phase %q, not Approved", tr.Name, phase)
	}
	decision, current := tr.Status.Decision, tr.Status.Proposal
	if decision == nil || decision.Outcome != v1alpha1.DecisionApproved || current == nil {
		return refuse(http.StatusConflict, executorapi.ReasonNotApproved,
			"Triage %s holds no Approval of a Proposal", tr.Name)
	}
	if current.Hash != hash {
		return refuse(http.StatusConflict, executorapi.ReasonHashMismatch,
			"hash %s is not the hash of Triage %s's current Proposal", hash, tr.Name)
	}
	recomputed, err := proposal.Hash(tr, current.ProposedAction, current.RenderedChange, current.Base)
	if err != nil || recomputed != hash {
		return refuse(http.StatusConflict, executorapi.ReasonProposalAltered,
			"the stored Proposal of Triage %s does not hash to the approved hash", tr.Name)
	}
	switch {
	case current.Action.DeliveredByPullRequest():
		return checkChange(current)
	case current.Action.ExecutesLive():
		return checkLiveBase(current)
	default:
		return refuse(http.StatusConflict, executorapi.ReasonUnsupportedAction,
			"the Executor does not execute %s", current.Action)
	}
}

// checkLiveBase confirms that a live Action's stored Proposal holds no Sandbox
// change and names the Workload generation it was rendered against.
func checkLiveBase(current *v1alpha1.Proposal) *executorapi.Error {
	if change := current.RenderedChange; change != nil && (change.Path != "" || change.Patch != "") {
		return refuse(http.StatusConflict, executorapi.ReasonUnsafeChange,
			"a live Action's stored Proposal must hold no Sandbox change")
	}
	if base := current.Base; base == nil || base.WorkloadGeneration == 0 {
		return refuse(http.StatusConflict, executorapi.ReasonUnsafeChange,
			"the stored Proposal holds no Workload generation")
	}
	return nil
}

// checkChange confirms that the stored change names a base and a patch, and
// that the file it targets is one an approved change may touch.
func checkChange(current *v1alpha1.Proposal) *executorapi.Error {
	change, base := current.RenderedChange, current.Base
	if change == nil || change.Patch == "" || base == nil || !isObjectID(base.Commit) {
		return refuse(http.StatusConflict, executorapi.ReasonUnsafeChange,
			"the stored Proposal holds no patch on a base commit")
	}
	if err := sandbox.CheckPath(change.Path); err != nil {
		return refuse(http.StatusConflict, executorapi.ReasonUnsafeChange,
			"the stored change names an unusable path: %v", err)
	}
	if first, _, _ := strings.Cut(change.Path, "/"); first == forbiddenRoot {
		return refuse(http.StatusConflict, executorapi.ReasonUnsafeChange,
			"the stored change touches %s, which an approved change may not", change.Path)
	}
	return nil
}

// checkApproval confirms that a TriageApproval exists for exactly this
// Triage and hash, with an Approval decision and a stamped approver, and that
// no Rejection of the same Proposal exists beside it.
func (e *Executor) checkApproval(ctx context.Context, tr *v1alpha1.Triage, hash string) *executorapi.Error {
	var approvals v1alpha1.TriageApprovalList
	if err := e.cfg.Reader.List(ctx, &approvals, client.InNamespace(e.cfg.Namespace)); err != nil {
		return &executorapi.Error{StatusCode: http.StatusInternalServerError, Message: "list TriageApprovals: " + err.Error()}
	}
	approved := false
	for i := range approvals.Items {
		spec := approvals.Items[i].Spec
		if spec.Triage != tr.Name || spec.ProposalHash != hash {
			continue
		}
		switch {
		case spec.Decision == v1alpha1.DecisionRejected:
			return refuse(http.StatusConflict, executorapi.ReasonNotApproved,
				"Triage %s's Proposal was also rejected, and a Rejection is final", tr.Name)
		case spec.Decision == v1alpha1.DecisionApproved && spec.Approver != "":
			approved = true
		}
	}
	if !approved {
		return refuse(http.StatusConflict, executorapi.ReasonNoApproval,
			"no TriageApproval approves hash %s of Triage %s", hash, tr.Name)
	}
	return nil
}

// branchName is the branch that holds one Proposal's change. It is derived
// from the Triage and the hash, so that every request for the Proposal finds
// the same branch and pull request.
func branchName(tr *v1alpha1.Triage, hash string) string {
	return "pagerless/" + tr.Name + "-" + hash[:12]
}
