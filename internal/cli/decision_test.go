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
	decisionWorkload  = "ledger"
	decisionUser      = "alice@example.com"

	approveCommand = "approve"
	rejectCommand  = "reject"
	reasonFlag     = "--reason"
	yesFlag        = "--yes"
)

// decisionWorkloadManifest is the Workload's manifest in the Sandbox, which is also what runs in the cluster.
func decisionWorkloadManifest(name string) string {
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
	if err := env.Client.Create(t.Context(), &corev1.Namespace{Name: decisionNamespace}); err != nil {
		t.Fatal(err)
	}
	return &decisionCluster{env: env, kubeconfig: env.KubeconfigAs(t, decisionUser)}
}

// deploy puts a Workload in the Sandbox and in the cluster.
func (c *decisionCluster) deploy(t *testing.T) {
	t.Helper()
	const name = decisionWorkload
	manifest := decisionWorkloadManifest(name)
	c.env.Sandbox.Commit("apps/"+name+"/deployment.yaml", manifest, "Add "+name)
	var d appsv1.Deployment
	if err := yaml.UnmarshalStrict([]byte(manifest), &d); err != nil {
		t.Fatal(err)
	}
	if err := c.env.Client.Create(t.Context(), &d); err != nil {
		t.Fatalf("create Deployment %s: %v", name, err)
	}
}

// propose runs an agent for the Workload that reports the Proposal, and waits for Proposed.
func (c *decisionCluster) propose(t *testing.T, action v1alpha1.ProposedAction) *v1alpha1.Triage {
	t.Helper()
	const workload = decisionWorkload
	created := c.env.CreateTriage(t, testenv.NewTriage(workload))
	agent := c.env.StartAgent(t, created.Name)
	result, err := agent.API.PostReport(t.Context(), agentapi.Report{
		Diagnosis: "The container is OOMKilled because its memory limit is too low.",
		Findings: []v1alpha1.Finding{{
			Kind: v1alpha1.FindingKindLog, Source: "loki: {app=\"" + workload + "\"}",
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
	return runKubectlTriage(t, c.kubeconfig, stdin, args...)
}

// runKubectlTriage runs the plugin against the kubeconfig and returns what it printed.
func runKubectlTriage(t *testing.T, kubeconfig string, stdin io.Reader, args ...string) (string, error) {
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

func (c *decisionCluster) approvals(t *testing.T, triage string) []v1alpha1.TriageApproval {
	t.Helper()
	var list v1alpha1.TriageApprovalList
	if err := c.env.Client.List(t.Context(), &list, client.InNamespace(testenv.Namespace)); err != nil {
		t.Fatal(err)
	}
	var matching []v1alpha1.TriageApproval
	for _, a := range list.Items {
		if a.Spec.Triage == triage {
			matching = append(matching, a)
		}
	}
	return matching
}

func (c *decisionCluster) requireProposed(t *testing.T, name string) {
	t.Helper()
	if got := c.env.Get(t, name).Status; got.Phase != v1alpha1.PhaseProposed || got.Decision != nil {
		t.Errorf("status = %+v, want Proposed with no decision", got)
	}
	if got := c.approvals(t, name); len(got) != 0 {
		t.Errorf("TriageApprovals = %+v, want none", got)
	}
}

func TestApproveShowsTheRenderedChangeAndApprovesItsHash(t *testing.T) {
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
		proposed.Name, "Deployment " + decisionNamespace + "/" + decisionWorkload, "PatchResources",
		"container app, limits memory=512Mi",
		change.Path, proposed.Status.Proposal.Base.Commit, proposed.Status.Proposal.Hash,
		"-              memory: 256Mi", "+              memory: 512Mi",
		"Approve this change? [y/N]",
		"created (approved by " + decisionUser + ")",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	tr := c.env.WaitForApproved(t, proposed.Name)
	if got := tr.Status.Decision; got == nil || got.Approver != decisionUser || got.Outcome != v1alpha1.DecisionApproved {
		t.Errorf("decision = %+v, want an Approval by %s", got, decisionUser)
	}
	approvals := c.approvals(t, proposed.Name)
	if len(approvals) != 1 || approvals[0].Spec.ProposalHash != proposed.Status.Proposal.Hash ||
		approvals[0].Spec.Approver != decisionUser {
		t.Errorf("TriageApprovals = %+v, want one for hash %s by %s", approvals, proposed.Status.Proposal.Hash, decisionUser)
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

	out, err := c.run(t, strings.NewReader(""), approveCommand, proposed.Name, yesFlag, reasonFlag, "limit is right")
	if err != nil {
		t.Fatalf("approve --yes: %v\n%s", err, out)
	}
	if strings.Contains(out, "[y/N]") || !strings.Contains(out, "+              memory: 512Mi") {
		t.Errorf("output = %s, want the change shown and no question", out)
	}
	tr := c.env.WaitForApproved(t, proposed.Name)
	if got := tr.Status.Decision; got == nil || got.Reason != "limit is right" {
		t.Errorf("decision = %+v, want the reason recorded", got)
	}
}

func TestApproveRefusesWhenTheProposalChangesWhileTheApproverDecides(t *testing.T) {
	t.Parallel()
	c := startDecisionCluster(t)
	c.deploy(t)
	proposed := c.propose(t, raiseMemory)

	changeWorkload := func() {
		var d appsv1.Deployment
		key := client.ObjectKey{Namespace: decisionNamespace, Name: decisionWorkload}
		if err := c.env.Client.Get(t.Context(), key, &d); err != nil {
			t.Error(err)
			return
		}
		d.Spec.Replicas = new(int32(3))
		if err := c.env.Client.Update(t.Context(), &d); err != nil {
			t.Error(err)
		}
	}
	stdin := &decisionStdin{answer: "y\n", beforeAn: changeWorkload}
	out, err := c.run(t, stdin, approveCommand, proposed.Name)
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("error = %v, want the webhook's refusal because the Proposal became stale\n%s", err, out)
	}
	c.requireProposed(t, proposed.Name)
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

func TestRejectRecordsTheReasonAndEndsTheTriage(t *testing.T) {
	t.Parallel()
	c := startDecisionCluster(t)
	c.deploy(t)
	proposed := c.propose(t, raiseMemory)

	out, err := c.run(t, strings.NewReader(""),
		rejectCommand, proposed.Name, reasonFlag, "1Gi is safer")
	if err != nil {
		t.Fatalf("reject: %v\n%s", err, out)
	}
	if want := "created (rejected by " + decisionUser + ")"; !strings.Contains(out, want) {
		t.Errorf("output = %q, want it to contain %q", out, want)
	}
	tr := c.env.WaitForPhase(t, proposed.Name, v1alpha1.PhaseRejected)
	got := tr.Status.Decision
	if got == nil || got.Outcome != v1alpha1.DecisionRejected || got.Reason != "1Gi is safer" ||
		got.Approver != decisionUser {
		t.Errorf("decision = %+v, want a Rejection with its reason by %s", got, decisionUser)
	}
}

func TestRejectRequiresAReason(t *testing.T) {
	t.Parallel()
	c := startDecisionCluster(t)
	c.deploy(t)
	proposed := c.propose(t, raiseMemory)

	cases := map[string][]string{
		"no flag":          {rejectCommand, proposed.Name},
		"an empty reason":  {rejectCommand, proposed.Name, reasonFlag, ""},
		"a blank reason":   {rejectCommand, proposed.Name, reasonFlag, "   "},
		"an oversize text": {rejectCommand, proposed.Name, reasonFlag, strings.Repeat("x", v1alpha1.MaxReasonLength+1)},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := c.run(t, strings.NewReader(""), args...)
			if err == nil {
				t.Fatalf("reject succeeded without a usable reason:\n%s", out)
			}
			c.requireProposed(t, proposed.Name)
		})
	}
}

func TestDecisionsNeedAProposedTriage(t *testing.T) {
	t.Parallel()
	c := startDecisionCluster(t)
	investigating := c.env.CreateTriage(t, testenv.NewTriage(decisionWorkload))
	c.env.WaitForPhase(t, investigating.Name, v1alpha1.PhaseInvestigating)

	decisions := [][]string{
		{approveCommand, investigating.Name, yesFlag},
		{rejectCommand, investigating.Name, reasonFlag, "no"},
	}
	for _, args := range decisions {
		out, err := c.run(t, strings.NewReader(""), args...)
		if err == nil || !strings.Contains(err.Error(), "Investigating") {
			t.Errorf("%s: error = %v, want a refusal that names the phase\n%s", args[0], err, out)
		}
	}
	for _, args := range [][]string{{approveCommand, "ghost", yesFlag}, {rejectCommand, "ghost", reasonFlag, "no"}} {
		out, err := c.run(t, strings.NewReader(""), args...)
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("%s: error = %v, want a refusal that says the Triage is missing\n%s", args[0], err, out)
		}
	}
	if got := c.approvals(t, investigating.Name); len(got) != 0 {
		t.Errorf("TriageApprovals = %+v, want none", got)
	}
}

func TestDecisionIsRefusedWithoutPermissionToApprove(t *testing.T) {
	t.Parallel()
	c := startDecisionCluster(t)
	c.deploy(t)
	proposed := c.propose(t, raiseMemory)
	// The stranger may read Triages, as any user of the plugin may, but holds no approver Role.
	stranger := "stranger@example.com"
	c.env.GrantReader(t, stranger)

	as := c.env.KubeconfigAs(t, stranger)
	out, err := runKubectlTriage(t, as, strings.NewReader(""), approveCommand, proposed.Name, yesFlag)
	if !apierrors.IsForbidden(err) {
		t.Errorf("error = %v, want Forbidden\n%s", err, out)
	}
	c.requireProposed(t, proposed.Name)
}
