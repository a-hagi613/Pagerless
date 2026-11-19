package server_test

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/proposal"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

const (
	proposalPoll = 50 * time.Millisecond
	targetField  = "target"
	movedPath    = "apps/path/moved.yaml"
	// Text that several Validation errors share.
	permittedRangeText  = "permitted range"
	exceedText          = "exceed"
	requestsMemoryField = "requests[memory]"
)

func TestPatchResourcesProposalCarriesTheRenderedPatchAndItsBase(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	w := target{name: checkout}
	deploy(t, env, w)
	head := env.Sandbox.Head()
	before := liveDeployment(t, env, checkout)

	name, agent := startPage(t, env, checkout)
	result, err := agent.API.PostReport(t.Context(), patchReport(labelApp, nil, quantities("memory", "512Mi")))
	if err != nil {
		t.Fatal(err)
	}
	if result.StatusCode != http.StatusOK || result.Report == nil || result.Report.Phase != v1alpha1.PhaseProposed {
		t.Fatalf("answer = %+v, want 200 and phase Proposed", result)
	}

	tr := env.WaitForPhase(t, name, v1alpha1.PhaseProposed)
	p := tr.Status.Proposal
	if p.Action != v1alpha1.ActionPatchResources || p.Attempt != 1 {
		t.Errorf("proposal = %+v, want PatchResources, attempt 1", p)
	}
	change := p.RenderedChange
	if change == nil || change.Path != w.sourcePath() {
		t.Fatalf("rendered change = %+v, want the patch for %s", change, w.sourcePath())
	}
	for _, want := range []string{
		"--- a/" + w.sourcePath(), "+++ b/" + w.sourcePath(),
		"-              memory: 256Mi", "+              memory: 512Mi",
	} {
		if !strings.Contains(change.Patch, want) {
			t.Errorf("patch lacks %q:\n%s", want, change.Patch)
		}
	}
	if err := gitApplyCheck(t, env, change.Patch); err != nil {
		t.Errorf("the patch does not apply to the Sandbox: %v", err)
	}
	if p.Base == nil || p.Base.Commit != head || p.Base.TargetGeneration != before.Generation {
		t.Errorf("base = %+v, want commit %s and generation %d", p.Base, head, before.Generation)
	}
	want, err := proposal.Hash(tr, p.ProposedAction, p.RenderedChange, p.Base)
	if err != nil || p.Hash != want {
		t.Errorf("hash = %s, want %s (error %v): it covers the Page, Action, Target, and rendered change",
			p.Hash, want, err)
	}

	// Validation renders the change and dry-runs it. It changes nothing.
	after := liveDeployment(t, env, checkout)
	if after.Generation != before.Generation ||
		!after.Spec.Template.Spec.Containers[0].Resources.Limits.Memory().Equal(resource.MustParse("256Mi")) {
		t.Errorf("the dry run changed the Target: generation %d, resources %+v",
			after.Generation, after.Spec.Template.Spec.Containers[0].Resources)
	}
	if env.Sandbox.Head() != head {
		t.Errorf("Validation moved the Sandbox head from %s to %s", head, env.Sandbox.Head())
	}
}

func TestPatchResourcesProposalsThatFailValidationAreRefusedWithErrors(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())

	tests := []struct {
		name      string
		target    target
		namespace string
		container string
		requests  corev1.ResourceList
		limits    corev1.ResourceList
		wantField string
		wantText  string
	}{
		{name: "namespace outside the allowlist", namespace: "production", limits: quantities("memory", "512Mi"),
			wantField: "target.namespace", wantText: "allowlist"},
		{name: "resource without bounds", limits: quantities("ephemeral-storage", "1Gi"),
			wantField: "limits[ephemeral-storage]", wantText: "not allowed"},
		{name: "memory above the bound", limits: quantities("memory", "1Ti"),
			wantField: "limits[memory]", wantText: permittedRangeText},
		{name: "memory below the bound", limits: quantities("memory", "1Mi"),
			wantField: "limits[memory]", wantText: permittedRangeText},
		{name: "request above the limit in the proposal", requests: quantities("cpu", "2"), limits: quantities("cpu", "1"),
			wantField: "requests[cpu]", wantText: exceedText},
		{name: "request above the limit in the manifest", requests: quantities("memory", "512Mi"),
			wantField: requestsMemoryField, wantText: exceedText},
		{name: "nothing to set", wantField: "proposal.parameters.patchResources", wantText: "at least one"},
		{name: "container the Target lacks", container: "ghost", limits: quantities("memory", "512Mi"),
			wantField: "patchResources.container", wantText: "no container named ghost"},
		{name: "values the Sandbox already declares", limits: quantities("memory", "256Mi"),
			wantField: "patchResources", wantText: "already"},
		{name: "target without a source path", target: target{noAnnotation: true},
			limits: quantities("memory", "512Mi"), wantField: targetField, wantText: v1alpha1.AnnotationSourcePath},
		{name: "source path that climbs out of the Sandbox", target: target{annotation: "../../etc/passwd.yaml"},
			limits: quantities("memory", "512Mi"), wantField: targetField, wantText: "inside the Sandbox"},
		{name: "source path that is not in the Sandbox", target: target{annotation: "apps/nowhere.yaml"},
			limits: quantities("memory", "512Mi"), wantField: targetField, wantText: "regular file"},
		{name: "target that does not exist", target: target{noLive: true, noManifest: true},
			limits: quantities("memory", "512Mi"), wantField: targetField, wantText: "does not exist"},
		// Argo CD may not have applied the manifest yet, so the live Target can
		// differ from the Sandbox copy. Validation must judge the resources the
		// API server will see and name the rule the Proposal breaks.
		{name: "live Target request above the proposed limit", limits: quantities("memory", "384Mi"),
			target: target{mutateLive: func(d *appsv1.Deployment) {
				c := &d.Spec.Template.Spec.Containers[0]
				c.Resources.Requests = quantities("memory", "512Mi")
				c.Resources.Limits = quantities("memory", "512Mi")
			}},
			wantField: requestsMemoryField, wantText: exceedText},
	}
	for i, tc := range tests {
		tc.target.name = "refused-" + strings.ReplaceAll(strings.ToLower(tc.name), " ", "-")
		deploy(t, env, tc.target)
		tests[i] = tc
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			page := testenv.NewPage(tc.target.name)
			if tc.namespace != "" {
				page.Spec.Target.Namespace = tc.namespace
			}
			created := env.CreatePage(t, page)
			agent := env.StartAgent(t, created.Name)
			container := tc.container
			if container == "" {
				container = labelApp
			}

			result, err := agent.API.PostReport(t.Context(), patchReport(container, tc.requests, tc.limits))
			if err != nil {
				t.Fatal(err)
			}
			if result.StatusCode != http.StatusUnprocessableEntity || result.Report == nil || result.Report.Accepted {
				t.Fatalf("answer = %+v, want 422 with errors", result)
			}
			found := false
			for _, e := range result.Report.Errors {
				found = found || (strings.Contains(e.Field, tc.wantField) && strings.Contains(e.Message, tc.wantText))
			}
			if !found {
				t.Errorf("errors = %+v, want one for %q mentioning %q", result.Report.Errors, tc.wantField, tc.wantText)
			}
			if result.Report.Phase != v1alpha1.PhaseInvestigating {
				t.Errorf("phase after the first failure = %s, want Investigating so the agent can revise", result.Report.Phase)
			}
			tr := env.Get(t, created.Name)
			if tr.Status.Proposal != nil || tr.Status.ValidationAttempts != 1 {
				t.Errorf("proposal = %+v, validationAttempts = %d, want no proposal and one failed attempt",
					tr.Status.Proposal, tr.Status.ValidationAttempts)
			}
		})
	}
}

// TestValidFirstPatchResourcesOnMemoryHolderIsAccepted guards the shape a live
// OOMKilled investigation submits: the first Proposal names the container that
// OOMs and raises its memory limit, and Validation must accept it without a
// refusal or a revision.
func TestValidFirstPatchResourcesOnMemoryHolderIsAccepted(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	ns := &corev1.Namespace{Name: sandboxNamespace}
	if err := env.Client.Create(t.Context(), ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}
	manifest := oomInjectedCheckout(t)
	env.Sandbox.Commit(checkoutManifestPath, manifest, "Inject the oom Scenario into checkout")
	var live appsv1.Deployment
	if err := yaml.UnmarshalStrict([]byte(manifest), &live); err != nil {
		t.Fatal(err)
	}
	if err := env.Client.Create(t.Context(), &live); err != nil {
		t.Fatalf("create the injected Deployment: %v", err)
	}

	name, agent := startPage(t, env, checkout)
	result, err := agent.API.PostReport(t.Context(), patchReport("memory-holder",
		quantities("memory", "64Mi"), quantities("memory", "64Mi")))
	if err != nil {
		t.Fatal(err)
	}
	if result.StatusCode != http.StatusOK || result.Report == nil || !result.Report.Accepted {
		t.Fatalf("answer = %+v (errors %+v), want the first Proposal accepted", result, result.Report.Errors)
	}
	tr := env.WaitForPhase(t, name, v1alpha1.PhaseProposed)
	if tr.Status.ValidationAttempts != 0 {
		t.Errorf("validationAttempts = %d, want 0 for a first-attempt acceptance", tr.Status.ValidationAttempts)
	}
	if tr.Status.Proposal == nil || tr.Status.Proposal.Attempt != 1 {
		t.Fatalf("proposal = %+v, want the first attempt", tr.Status.Proposal)
	}
	condition := meta.FindStatusCondition(tr.Status.Conditions, v1alpha1.ConditionInvestigated)
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != v1alpha1.ReasonAgentReported {
		t.Errorf("Investigated condition = %+v, want True/AgentReported after an accepted Proposal", condition)
	}
	change := tr.Status.Proposal.RenderedChange
	if change == nil {
		t.Fatal("the accepted Proposal carries no rendered change")
	}
	for _, want := range []string{"-            memory: 32Mi", "+            memory: 64Mi"} {
		if !strings.Contains(change.Patch, want) {
			t.Errorf("patch lacks %q:\n%s", want, change.Patch)
		}
	}
}

// TestEveryRefusedProposalIsRecordedOnThePage keeps the refused Action, its
// parameters, and its Validation errors on the Page after the agent replaces
// it, so an operator can see what the agent first got wrong.
func TestEveryRefusedProposalIsRecordedOnThePage(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, target{name: "recorded"})
	name, agent := startPage(t, env, "recorded")

	first, err := agent.API.PostReport(t.Context(), patchReport(labelApp, nil, quantities("memory", "1Ti")))
	if err != nil || first.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("first report: %+v, %v, want 422", first, err)
	}
	condition := func() string {
		tr := env.Get(t, name)
		found := meta.FindStatusCondition(tr.Status.Conditions, v1alpha1.ConditionInvestigated)
		if found == nil {
			t.Fatalf("the Page records no %s condition", v1alpha1.ConditionInvestigated)
		}
		if found.Status != metav1.ConditionFalse || found.Reason != v1alpha1.ReasonValidationRefused {
			t.Errorf("refusal condition = %+v, want status False and reason %s",
				found, v1alpha1.ReasonValidationRefused)
		}
		return found.Message
	}
	message := condition()
	for _, want := range []string{"attempt 1: PatchResources", "container=app", "limits[memory]=1Ti", permittedRangeText} {
		if !strings.Contains(message, want) {
			t.Errorf("the refusal message lacks %q:\n%s", want, message)
		}
	}

	// A second refusal of a different Action keeps the first line, so both
	// attempts stay visible after the Page ends as Invalid.
	second, err := agent.API.PostReport(t.Context(), agentapi.Report{
		Diagnosis: "A replica count of zero would stop the traffic.",
		Findings:  []v1alpha1.Finding{logFinding},
		Proposal: agentapi.ReportProposal{ProposedAction: v1alpha1.ProposedAction{
			Action:     v1alpha1.ActionScale,
			Parameters: &v1alpha1.ActionParameters{Scale: &v1alpha1.ScaleParameters{Replicas: 0}},
		}},
	})
	if err != nil || second.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("second report: %+v, %v, want 422", second, err)
	}
	message = condition()
	for _, want := range []string{"attempt 1: PatchResources", "attempt 2: Scale replicas=0", "not accepted"} {
		if !strings.Contains(message, want) {
			t.Errorf("the refusal message lacks %q:\n%s", want, message)
		}
	}
}

// checkoutManifestPath is where the demo's checkout Deployment lives in the Sandbox.
const checkoutManifestPath = "workloads/deployments/checkout.yaml"

// oomInjectedCheckout returns the rendered checkout manifest with the oom
// Scenario's injection applied, so Validation renders against the file the live
// Scenario would hold.
func oomInjectedCheckout(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../sandbox/testdata/known-good-checkout.yaml")
	if err != nil {
		t.Fatal(err)
	}
	knownGood := "            memory: 128Mi\n          requests:\n            memory: 64Mi"
	injected := "            memory: 32Mi\n          requests:\n            memory: 32Mi"
	manifest := strings.Replace(string(raw), knownGood, injected, 1)
	if manifest == string(raw) {
		t.Fatal("the known-good manifest does not carry the memory-holder resources the oom Scenario lowers")
	}
	return manifest
}

func TestSecondFailedProposalEndsThePageAsInvalid(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, target{name: "revised"})
	deploy(t, env, target{name: "hopeless"})
	deploy(t, env, target{name: "malformed"})
	tooBig := patchReport(labelApp, nil, quantities("memory", "1Ti"))
	fine := patchReport(labelApp, nil, quantities("memory", "512Mi"))

	t.Run("a revised proposal that passes is attempt 2", func(t *testing.T) {
		t.Parallel()
		name, agent := startPage(t, env, "revised")
		first, err := agent.API.PostReport(t.Context(), tooBig)
		if err != nil || first.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("first report: %+v, %v, want 422", first, err)
		}
		result, err := agent.API.PostReport(t.Context(), fine)
		if err != nil || result.StatusCode != http.StatusOK {
			t.Fatalf("second report: %+v, %v, want 200", result, err)
		}
		tr := env.WaitForPhase(t, name, v1alpha1.PhaseProposed)
		if got := tr.Status.Proposal.Attempt; got != 2 {
			t.Errorf("attempt = %d, want 2", got)
		}
	})

	t.Run("two refused reports of any kind end the Page", func(t *testing.T) {
		t.Parallel()
		name, agent := startPage(t, env, "malformed")
		noDiagnosis := fine
		noDiagnosis.Diagnosis = ""
		first, err := agent.API.PostReport(t.Context(), noDiagnosis)
		if err != nil || first.StatusCode != http.StatusUnprocessableEntity ||
			first.Report.Phase != v1alpha1.PhaseInvestigating {
			t.Fatalf("first report: %+v, %v, want 422 in Investigating", first, err)
		}
		second, err := agent.API.PostReport(t.Context(), tooBig)
		if err != nil || second.StatusCode != http.StatusUnprocessableEntity ||
			second.Report.Phase != v1alpha1.PhaseInvalid {
			t.Fatalf("second report: %+v, %v, want 422 in Invalid", second, err)
		}
		env.WaitForPhase(t, name, v1alpha1.PhaseInvalid)
	})

	t.Run("a second failure is final", func(t *testing.T) {
		t.Parallel()
		name, agent := startPage(t, env, "hopeless")
		first, err := agent.API.PostReport(t.Context(), tooBig)
		refused := func(r agentapi.Result) bool {
			return r.StatusCode == http.StatusUnprocessableEntity && r.Report != nil
		}
		if err != nil || !refused(first) || first.Report.Phase != v1alpha1.PhaseInvestigating {
			t.Fatalf("first report: %+v, %v, want 422 in Investigating", first, err)
		}
		second, err := agent.API.PostReport(t.Context(), tooBig)
		if err != nil || !refused(second) || len(second.Report.Errors) == 0 || second.Report.Phase != v1alpha1.PhaseInvalid {
			t.Fatalf("second report: %+v, %v, want 422 with errors in Invalid", second, err)
		}
		tr := env.WaitForPhase(t, name, v1alpha1.PhaseInvalid)
		condition := meta.FindStatusCondition(tr.Status.Conditions, v1alpha1.ConditionFailed)
		if condition == nil || condition.Reason != v1alpha1.ReasonValidationFailedTwice ||
			!strings.Contains(condition.Message, "limits[memory]") {
			t.Errorf("Invalid condition = %+v, want reason %s and the errors", condition, v1alpha1.ReasonValidationFailedTwice)
		}
		if tr.Status.Proposal != nil {
			t.Errorf("an Invalid Page holds proposal %+v", tr.Status.Proposal)
		}
		if late, err := agent.API.PostReport(t.Context(), fine); err != nil || late.StatusCode != http.StatusConflict {
			t.Errorf("report after Invalid: %+v, %v, want 409", late, err)
		}
		if got := env.Get(t, name).Status.Phase; got != v1alpha1.PhaseInvalid {
			t.Errorf("phase after a late report = %s, want Invalid", got)
		}
	})
}

// proposedPage reports a valid patch for the Target and waits for the Proposal.
func proposedPage(t *testing.T, env *testenv.Env, targetName string) *v1alpha1.Page {
	t.Helper()
	name, agent := startPage(t, env, targetName)
	result, err := agent.API.PostReport(t.Context(), patchReport(labelApp, nil, quantities("memory", "512Mi")))
	if err != nil || result.StatusCode != http.StatusOK {
		t.Fatalf("report: %+v, %v, want 200", result, err)
	}
	return env.WaitForPhase(t, name, v1alpha1.PhaseProposed)
}

// revalidated waits until the Page holds a Proposal under a different hash.
func revalidated(t *testing.T, env *testenv.Env, name, oldHash string) *v1alpha1.Page {
	t.Helper()
	return env.WaitFor(t, name, "the Proposal is re-validated under a new hash", func(tr *v1alpha1.Page) bool {
		return tr.Status.Proposal != nil && tr.Status.Proposal.Hash != oldHash
	})
}

func TestProposalBecomesStaleWhenItsBaseChanges(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources(), testenv.WithProposalPollInterval(proposalPoll))
	for _, name := range []string{"generation", "path", "file", "unrelated", "vanished"} {
		deploy(t, env, target{name: name})
	}

	assertRevalidated := func(t *testing.T, before, after *v1alpha1.Page, reason string) {
		t.Helper()
		if after.Status.Phase != v1alpha1.PhaseProposed {
			t.Errorf("phase = %s, want Proposed after the Stale Proposal was re-rendered (%s)",
				after.Status.Phase, reason)
		}
		old, current := before.Status.Proposal, after.Status.Proposal
		if current.Attempt != old.Attempt || !current.ExpiresAt.Equal(&old.ExpiresAt) {
			t.Errorf("re-validation changed the attempt or the expiry: %+v to %+v", old, current)
		}
		want, err := proposal.Hash(after, current.ProposedAction, current.RenderedChange, current.Base)
		if err != nil || current.Hash != want {
			t.Errorf("hash = %s, want %s (error %v): the new hash must cover the new base", current.Hash, want, err)
		}
	}

	t.Run("the Target's generation changes", func(t *testing.T) {
		before := proposedPage(t, env, "generation")
		d := liveDeployment(t, env, "generation")
		d.Spec.Replicas = new(int32(2))
		if err := env.Client.Update(t.Context(), d); err != nil {
			t.Fatal(err)
		}
		after := revalidated(t, env, before.Name, before.Status.Proposal.Hash)
		assertRevalidated(t, before, after, v1alpha1.ReasonTargetGenerationChanged)
		want := liveDeployment(t, env, "generation").Generation
		if got := after.Status.Proposal.Base.TargetGeneration; got != want {
			t.Errorf("base generation = %d, want %d", got, want)
		}
	})

	t.Run("the Target's source path changes", func(t *testing.T) {
		before := proposedPage(t, env, "path")
		moved := target{name: "path", annotation: movedPath}
		env.Sandbox.Commit(movedPath, moved.manifest(), "Move the manifest")
		d := liveDeployment(t, env, "path")
		d.Annotations[v1alpha1.AnnotationSourcePath] = movedPath
		if err := env.Client.Update(t.Context(), d); err != nil {
			t.Fatal(err)
		}
		after := revalidated(t, env, before.Name, before.Status.Proposal.Hash)
		assertRevalidated(t, before, after, v1alpha1.ReasonSourcePathChanged)
		if got := after.Status.Proposal.RenderedChange.Path; got != movedPath {
			t.Errorf("rendered path = %s, want the new source path", got)
		}
	})

	t.Run("the Sandbox file changes", func(t *testing.T) {
		before := proposedPage(t, env, "file")
		changed := target{name: "file"}.manifest() + "# edited in the Sandbox\n"
		head := env.Sandbox.Commit("apps/file/deployment.yaml", changed, "Edit the manifest")
		after := revalidated(t, env, before.Name, before.Status.Proposal.Hash)
		assertRevalidated(t, before, after, v1alpha1.ReasonTargetRepositoryFileChanged)
		if got := after.Status.Proposal.Base.Commit; got != head {
			t.Errorf("base commit = %s, want the new head %s", got, head)
		}
		if err := gitApplyCheck(t, env, after.Status.Proposal.RenderedChange.Patch); err != nil {
			t.Errorf("the re-rendered patch does not apply to the Sandbox: %v", err)
		}
	})

	t.Run("an unrelated Sandbox commit does not make it stale", func(t *testing.T) {
		before := proposedPage(t, env, "unrelated")
		env.Sandbox.Commit("apps/other/deployment.yaml", "kind: Deployment\n", "Change another Target")
		time.Sleep(20 * proposalPoll)
		after := env.Get(t, before.Name)
		if after.Status.Proposal.Hash != before.Status.Proposal.Hash {
			t.Errorf("an unrelated commit re-validated the Proposal: %+v", after.Status)
		}
	})

	t.Run("a Proposal that fails re-validation ends the Page as Invalid", func(t *testing.T) {
		before := proposedPage(t, env, "vanished")
		d := liveDeployment(t, env, "vanished")
		d.Spec.Template.Spec.Containers[0].Name = "renamed"
		if err := env.Client.Update(t.Context(), d); err != nil {
			t.Fatal(err)
		}
		after := env.WaitForPhase(t, before.Name, v1alpha1.PhaseInvalid)
		condition := meta.FindStatusCondition(after.Status.Conditions, v1alpha1.ConditionFailed)
		if condition == nil || condition.Reason != v1alpha1.ReasonValidationFailedTwice ||
			!strings.Contains(condition.Message, "no container named app") {
			t.Errorf("Invalid condition = %+v, want reason %s and the error", condition, v1alpha1.ReasonValidationFailedTwice)
		}
	})
}

func TestUnapprovedProposalExpiresAfterOneHour(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources(), testenv.WithProposalPollInterval(proposalPoll))
	deploy(t, env, target{name: checkout})
	proposed := proposedPage(t, env, checkout)
	if want := testenv.StartTime.Add(time.Hour); !proposed.Status.Proposal.ExpiresAt.Time.Equal(want) {
		t.Fatalf("expiresAt = %v, want %v", proposed.Status.Proposal.ExpiresAt.Time, want)
	}

	env.Clock.Step(time.Hour - time.Second)
	time.Sleep(20 * proposalPoll)
	if got := env.Get(t, proposed.Name).Status.Phase; got != v1alpha1.PhaseProposed {
		t.Fatalf("phase one second before expiry = %s, want Proposed", got)
	}

	env.Clock.Step(time.Second)
	expired := env.WaitForPhase(t, proposed.Name, v1alpha1.PhaseExpired)
	if expired.Status.Proposal == nil || expired.Status.Proposal.Hash != proposed.Status.Proposal.Hash {
		t.Errorf("an Expired Page should keep its Proposal for the record, got %+v", expired.Status.Proposal)
	}
}

func TestNoActionProposalExpiresToo(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources(), testenv.WithProposalPollInterval(proposalPoll))
	created := env.CreatePage(t, testenv.NewPage(checkout))
	agent := env.StartAgent(t, created.Name)
	if _, err := agent.API.PostReport(t.Context(), noActionReport()); err != nil {
		t.Fatal(err)
	}
	env.WaitForPhase(t, created.Name, v1alpha1.PhaseProposed)

	env.Clock.Step(time.Hour)
	env.WaitForPhase(t, created.Name, v1alpha1.PhaseExpired)
}
