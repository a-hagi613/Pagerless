package cli_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/cli"
	"github.com/a-hagi613/pagerless/internal/proposal"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

const (
	decisionNamespace = "sandbox"
	decisionTarget    = "ledger"
	decisionUser      = "alice@example.com"

	approveCommand = "approve"
	dismissCommand = "dismiss"
	reasonFlag     = "--reason"
	yesFlag        = "--yes"
	byFlag         = "--by"
)

// decisionTargetManifest is the Target's manifest in the Target Repository,
// which is also what runs in the cluster.
func decisionTargetManifest(name string) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
  namespace: %[2]s
  annotations:
    %[3]s: apps/%[1]s/deployment.yaml
spec:
  replicas: 1
  selector:
    matchLabels:
      app: %[1]s
  template:
    metadata:
      labels:
        app: %[1]s
    spec:
      containers:
        - name: app
          image: registry.invalid/%[1]s:1.0
          resources:
            limits:
              memory: 256Mi
`, name, decisionNamespace, v1alpha1.AnnotationSourcePath)
}

// decisionCluster is a harness with Validation for PatchResources, an approver, and that approver's kubeconfig.
type decisionCluster struct {
	env        *testenv.Env
	kubeconfig string
}

func startDecisionCluster(t *testing.T) *decisionCluster {
	t.Helper()
	env := testenv.Start(t, testenv.WithValidators(func(e *testenv.Env) map[v1alpha1.Action]proposal.ActionValidator {
		return e.PatchResources(decisionNamespace)
	}))
	env.GrantApprover(t, decisionUser)
	return &decisionCluster{env: env, kubeconfig: env.KubeconfigAs(t, decisionUser)}
}

// deploy puts a Target in the Target Repository and in the cluster.
func (c *decisionCluster) deploy(t *testing.T) {
	t.Helper()
	const name = decisionTarget
	manifest := decisionTargetManifest(name)
	c.env.Sandbox.Commit("apps/"+name+"/deployment.yaml", manifest, "Add "+name)
	var d appsv1.Deployment
	if err := yaml.UnmarshalStrict([]byte(manifest), &d); err != nil {
		t.Fatal(err)
	}
	if err := c.env.Client.Create(t.Context(), &d); err != nil {
		t.Fatalf("create Deployment %s: %v", name, err)
	}
}

// propose runs an agent for the Target that reports the Proposal, and waits for Proposed.
func (c *decisionCluster) propose(t *testing.T, action v1alpha1.ProposedAction) *v1alpha1.Page {
	t.Helper()
	const target = decisionTarget
	created := c.env.CreatePage(t, testenv.NewPage(target))
	agent := c.env.StartAgent(t, created.Name)
	result, err := agent.API.PostReport(t.Context(), agentapi.Report{
		Diagnosis: "The container is OOMKilled because its memory limit is too low.",
		Findings: []v1alpha1.Finding{{
			Kind: v1alpha1.FindingKindLog, Source: "loki: {app=\"" + target + "\"}",
			Summary: "the container was killed with exit code 137",
		}},
		Proposal: action,
	})
	if err != nil || result.StatusCode != http.StatusOK {
		t.Fatalf("report: %+v, %v", result, err)
	}
	return c.env.WaitForPhase(t, created.Name, v1alpha1.PhaseProposed)
}

// raiseMemory is the Proposal to raise the container's memory limit from 256Mi to 512Mi.
var raiseMemory = v1alpha1.ProposedAction{
	Action: v1alpha1.ActionPatchResources,
	Parameters: &v1alpha1.ActionParameters{PatchResources: &v1alpha1.PatchResourcesParameters{
		Container: "app",
		Limits:    corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
	}},
}

var noAction = v1alpha1.ProposedAction{Action: v1alpha1.ActionNoAction}

// decisionStdin supplies the answer to a prompt, and runs before the first read, which is when the
// approver has seen the change and is about to answer.
type decisionStdin struct {
	answer   string
	beforeAn func()

	once   sync.Once
	reader io.Reader
}

func (s *decisionStdin) Read(p []byte) (int, error) {
	s.once.Do(func() {
		if s.beforeAn != nil {
			s.beforeAn()
		}
		s.reader = strings.NewReader(s.answer)
	})
	return s.reader.Read(p)
}

// run runs the plugin as the cluster's approver.
func (c *decisionCluster) run(t *testing.T, stdin io.Reader, args ...string) (string, error) {
	t.Helper()
	return runKubectlPage(t, c.kubeconfig, stdin, args...)
}

// runKubectlPage runs the plugin against the kubeconfig and returns what it printed.
func runKubectlPage(t *testing.T, kubeconfig string, stdin io.Reader, args ...string) (string, error) {
	t.Helper()
	root := cli.NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(stdin)
	root.SetArgs(append([]string{"--kubeconfig", kubeconfig}, args...))
	err := root.ExecuteContext(t.Context())
	return out.String(), err
}

func (c *decisionCluster) requireProposed(t *testing.T, name string) {
	t.Helper()
	page := c.env.Get(t, name)
	if page.Status.Phase != v1alpha1.PhaseProposed || page.Status.Decision != nil {
		t.Errorf("status = %+v, want Proposed with no decision", page.Status)
	}
	if page.Annotations[v1alpha1.AnnotationDecision] != "" {
		t.Errorf("annotations = %v, want no decision annotation", page.Annotations)
	}
}

func TestApproveShowsTheRenderedChangeAndBindsTheHash(t *testing.T) {
	t.Parallel()
	c := startDecisionCluster(t)
	c.deploy(t)
	proposed := c.propose(t, raiseMemory)
	change := proposed.Status.Proposal.RenderedChange

	out, err := c.run(t, strings.NewReader("y\n"), approveCommand, proposed.Name)
	if err != nil {
		t.Fatalf("approve: %v\n%s", err, out)
	}
	for _, want := range []string{
		proposed.Name, "Deployment " + decisionNamespace + "/" + decisionTarget, "PatchResources",
		"container app, limits memory=512Mi",
		change.Path, proposed.Status.Proposal.Base.Commit, proposed.Status.Proposal.Hash,
		"-              memory: 256Mi", "+              memory: 512Mi",
		"Approve this change? [y/N]",
		"approved by " + decisionUser,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	page := c.env.WaitForApproved(t, proposed.Name)
	decision := page.Status.Decision
	if decision == nil || decision.DecidedBy != decisionUser ||
		decision.Outcome != v1alpha1.DecisionApproved ||
		decision.ProposalHash != proposed.Status.Proposal.Hash {
		t.Fatalf("decision = %+v, want an Approval of %s by %s",
			decision, proposed.Status.Proposal.Hash, decisionUser)
	}
}

func TestApproveWithoutConfirmationApprovesNothing(t *testing.T) {
	t.Parallel()
	c := startDecisionCluster(t)
	c.deploy(t)
	proposed := c.propose(t, raiseMemory)

	for name, answer := range map[string]string{
		"no":              "n\n",
		"anything else":   "maybe\n",
		"an empty answer": "\n",
		"input that ends": "",
	} {
		t.Run(name, func(t *testing.T) {
			out, err := c.run(t, strings.NewReader(answer), approveCommand, proposed.Name)
			if err == nil || !strings.Contains(err.Error(), "not approved") {
				t.Errorf("error = %v, want a refusal that says nothing was approved\n%s", err, out)
			}
			c.requireProposed(t, proposed.Name)
		})
	}

	t.Run("an answer in words", func(t *testing.T) {
		if out, err := c.run(t, strings.NewReader("YES\n"), approveCommand, proposed.Name); err != nil {
			t.Fatalf("approve: %v\n%s", err, out)
		}
		c.env.WaitForApproved(t, proposed.Name)
	})
}

func TestApproveYesSkipsTheConfirmation(t *testing.T) {
	t.Parallel()
	c := startDecisionCluster(t)
	c.deploy(t)
	proposed := c.propose(t, raiseMemory)

	out, err := c.run(t, strings.NewReader(""), approveCommand, proposed.Name, yesFlag,
		reasonFlag, "limit is right", byFlag, "reviewer@example.com")
	if err != nil {
		t.Fatalf("approve --yes: %v\n%s", err, out)
	}
	if strings.Contains(out, "[y/N]") || !strings.Contains(out, "+              memory: 512Mi") {
		t.Errorf("output = %s, want the change shown and no question", out)
	}
	page := c.env.WaitForApproved(t, proposed.Name)
	if got := page.Status.Decision; got == nil || got.Reason != "limit is right" ||
		got.DecidedBy != "reviewer@example.com" {
		t.Errorf("decision = %+v, want the reason and the declared reviewer", got)
	}
}

func TestADecisionOnAProposalThatChangedIsIgnoredByTheServer(t *testing.T) {
	t.Parallel()
	c := startDecisionCluster(t)
	c.deploy(t)
	proposed := c.propose(t, raiseMemory)

	changeTarget := func() {
		var d appsv1.Deployment
		key := client.ObjectKey{Namespace: decisionNamespace, Name: decisionTarget}
		if err := c.env.Client.Get(t.Context(), key, &d); err != nil {
			t.Error(err)
			return
		}
		d.Spec.Replicas = new(int32(3))
		if err := c.env.Client.Update(t.Context(), &d); err != nil {
			t.Error(err)
		}
	}
	stdin := &decisionStdin{answer: "y\n", beforeAn: changeTarget}
	if out, err := c.run(t, stdin, approveCommand, proposed.Name); err != nil {
		t.Fatalf("approve: %v\n%s", err, out)
	}
	// The controller ignores the decision because the base moved, then re-renders
	// the Proposal under a new hash; the Page is never approved on the old one.
	c.env.WaitFor(t, proposed.Name, "the Proposal is re-rendered", func(page *v1alpha1.Page) bool {
		return page.Status.Proposal != nil && page.Status.Proposal.Hash != proposed.Status.Proposal.Hash
	})
	if page := c.env.Get(t, proposed.Name); page.Status.Decision != nil {
		t.Fatalf("decision = %+v, want none on the stale Proposal", page.Status.Decision)
	}
}

func TestApproveAcknowledgesANoActionProposal(t *testing.T) {
	t.Parallel()
	c := startDecisionCluster(t)
	proposed := c.propose(t, noAction)

	out, err := c.run(t, strings.NewReader("y\n"), approveCommand, proposed.Name)
	if err != nil {
		t.Fatalf("approve: %v\n%s", err, out)
	}
	for _, want := range []string{"makes no change", "Acknowledge this Diagnosis? [y/N]"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	c.env.WaitForPhase(t, proposed.Name, v1alpha1.PhaseDiagnosed)
}

func TestDismissRecordsTheReasonAndEndsThePage(t *testing.T) {
	t.Parallel()
	c := startDecisionCluster(t)
	c.deploy(t)
	proposed := c.propose(t, raiseMemory)

	out, err := c.run(t, strings.NewReader(""),
		dismissCommand, proposed.Name, reasonFlag, "1Gi is safer")
	if err != nil {
		t.Fatalf("dismiss: %v\n%s", err, out)
	}
	if want := "rejected by " + decisionUser; !strings.Contains(out, want) {
		t.Errorf("output = %q, want it to contain %q", out, want)
	}
	page := c.env.WaitForPhase(t, proposed.Name, v1alpha1.PhaseRejected)
	decision := page.Status.Decision
	if decision == nil || decision.Outcome != v1alpha1.DecisionRejected || decision.Reason != "1Gi is safer" ||
		decision.DecidedBy != decisionUser {
		t.Errorf("decision = %+v, want a Rejection with its reason by %s", decision, decisionUser)
	}
}

func TestDismissRequiresAReason(t *testing.T) {
	t.Parallel()
	c := startDecisionCluster(t)
	c.deploy(t)
	proposed := c.propose(t, raiseMemory)

	cases := map[string][]string{
		"no flag":          {dismissCommand, proposed.Name},
		"an empty reason":  {dismissCommand, proposed.Name, reasonFlag, ""},
		"a blank reason":   {dismissCommand, proposed.Name, reasonFlag, "   "},
		"an oversize text": {dismissCommand, proposed.Name, reasonFlag, strings.Repeat("x", v1alpha1.MaxReasonLength+1)},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := c.run(t, strings.NewReader(""), args...)
			if err == nil {
				t.Fatalf("dismiss succeeded without a usable reason:\n%s", out)
			}
			c.requireProposed(t, proposed.Name)
		})
	}
}

func TestDecisionsNeedAProposedPage(t *testing.T) {
	t.Parallel()
	c := startDecisionCluster(t)
	investigating := c.env.CreatePage(t, testenv.NewPage(decisionTarget))
	c.env.WaitForPhase(t, investigating.Name, v1alpha1.PhaseInvestigating)

	decisions := [][]string{
		{approveCommand, investigating.Name, yesFlag},
		{dismissCommand, investigating.Name, reasonFlag, "no"},
	}
	for _, args := range decisions {
		out, err := c.run(t, strings.NewReader(""), args...)
		if err == nil || !strings.Contains(err.Error(), "Investigating") {
			t.Errorf("%s: error = %v, want a refusal that names the phase\n%s", args[0], err, out)
		}
	}
	for _, args := range [][]string{
		{approveCommand, "ghost", yesFlag},
		{dismissCommand, "ghost", reasonFlag, "no"},
	} {
		out, err := c.run(t, strings.NewReader(""), args...)
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("%s: error = %v, want a refusal that says the Page is missing\n%s", args[0], err, out)
		}
	}
	page := c.env.Get(t, investigating.Name)
	if page.Status.Phase != v1alpha1.PhaseInvestigating || page.Status.Decision != nil ||
		page.Annotations[v1alpha1.AnnotationDecision] != "" {
		t.Fatalf("status = %+v, want the Investigating Page left undecided", page.Status)
	}
}

func TestADecisionIsRefusedWithoutPermissionToPatchPages(t *testing.T) {
	t.Parallel()
	c := startDecisionCluster(t)
	c.deploy(t)
	proposed := c.propose(t, raiseMemory)
	// The stranger may read Pages, as any user of the plugin may, but holds no approver Role.
	stranger := "stranger@example.com"
	c.env.GrantReader(t, stranger)

	as := c.env.KubeconfigAs(t, stranger)
	out, err := runKubectlPage(t, as, strings.NewReader(""), approveCommand, proposed.Name, yesFlag)
	if !apierrors.IsForbidden(err) {
		t.Errorf("error = %v, want Forbidden\n%s", err, out)
	}
	c.requireProposed(t, proposed.Name)
}

func TestVersionPrintsTheBuildAndTheExpectedChart(t *testing.T) {
	t.Parallel()
	out, err := runKubectlPage(t, "", strings.NewReader(""), "version")
	if err != nil {
		t.Fatalf("version: %v\n%s", err, out)
	}
	for _, want := range []string{"kubectl-page", "commit", "built", "expected chart", "go"} {
		if !strings.Contains(out, want) {
			t.Errorf("version output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, cli.ExpectedChartVersion) {
		t.Errorf("version output lacks the expected chart version %q:\n%s", cli.ExpectedChartVersion, out)
	}
}
