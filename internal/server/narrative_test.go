package server_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/controller"
	"github.com/a-hagi613/pagerless/internal/server"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

// recovered runs the whole loop and returns the Triage once Verification has ended Recovered and its Narrative
// Job is recorded.
func recoveredTriage(t *testing.T, env *testenv.Env) *v1alpha1.Triage {
	t.Helper()
	tr, commit := merged(t, env, humanTrigger)
	verifying(t, env, tr, commit)
	env.Clock.Step(5 * time.Minute)
	return env.WaitFor(t, tr.Name, "the Triage is Recovered with a Narrative Job", func(tr *v1alpha1.Triage) bool {
		return tr.Status.Phase == v1alpha1.PhaseRecovered && tr.Status.NarrativeJobName != ""
	})
}

// jobsForTriage lists the Jobs that the Triage owns.
func jobsForTriage(t *testing.T, env *testenv.Env, name string) []string {
	t.Helper()
	var jobs batchv1.JobList
	if err := env.Client.List(t.Context(), &jobs, client.InNamespace(testenv.Namespace),
		client.MatchingLabels{controller.TriageLabel: name}); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(jobs.Items))
	for i := range jobs.Items {
		names = append(names, jobs.Items[i].Name)
	}
	return names
}

func TestNarrativeJobDescribesTheVerifiedTriage(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	tr := recoveredTriage(t, env)

	narrative := env.StartNarrative(t, tr.Name)
	if deadline := narrative.Job.Spec.ActiveDeadlineSeconds; deadline == nil ||
		*deadline != int64(server.DefaultNarrativeDeadline.Seconds()) {
		t.Errorf("Narrative Job deadline = %v, want %d seconds", deadline, int64(server.DefaultNarrativeDeadline.Seconds()))
	}
	if !metav1.IsControlledBy(narrative.Job, tr) {
		t.Error("the Narrative Job is not controlled by its Triage")
	}
	if jobs := jobsForTriage(t, env, tr.Name); len(jobs) != 2 {
		t.Errorf("Jobs for Triage %s = %v, want the investigation Job and one Narrative Job", tr.Name, jobs)
	}
	variables := map[string]string{}
	for _, variable := range narrative.Job.Spec.Template.Spec.Containers[0].Env {
		variables[variable.Name] = variable.Value
	}
	for name, want := range map[string]string{
		agentapi.EnvTask:      agentapi.TaskNarrative,
		agentapi.EnvTriage:    tr.Name,
		agentapi.EnvServerURL: env.ReportURL,
	} {
		if variables[name] != want {
			t.Errorf("%s = %q, want %q", name, variables[name], want)
		}
	}
	if variables[agentapi.EnvModelProvider] != replayProvider {
		t.Errorf("the Narrative Job runs provider %q, want the default %q",
			variables[agentapi.EnvModelProvider], replayProvider)
	}
	for _, name := range []string{
		agentapi.EnvMCPGatewayURL, agentapi.EnvKeycloakIssuer, agentapi.EnvKeycloakTokenURL,
		agentapi.EnvKeycloakClientID, agentapi.EnvKeycloakTokenFile,
		agentapi.EnvWorkloadName, agentapi.EnvWorkloadNamespace, agentapi.EnvSandboxOwner, agentapi.EnvReplayFile,
	} {
		if _, exists := variables[name]; exists {
			t.Errorf("the Narrative Job received %s, which it does not need", name)
		}
	}

	record, result, err := narrative.API.GetNarrative(t.Context())
	if err != nil || result.StatusCode != http.StatusOK {
		t.Fatalf("read the Narrative record: %+v, %v, want 200", result, err)
	}
	if record.Workload != tr.Spec.Workload {
		t.Errorf("record Workload = %+v, want %+v", record.Workload, tr.Spec.Workload)
	}
	for _, want := range []string{
		tr.Status.Diagnosis,
		tr.Status.Findings[0].Source,
		"Phase: Recovered",
		"Action: PatchResources",
		"Outcome: Approved",
		string(v1alpha1.CheckAlertResolved),
	} {
		if !strings.Contains(record.Text, want) {
			t.Errorf("the Narrative record lacks %q:\n%s", want, record.Text)
		}
	}

	written := "The checkout Deployment was failing because its memory limit was too low. " +
		"The approved PatchResources change raised the limit, and Verification found the Workload recovered."
	result, err = narrative.API.PostNarrative(t.Context(), agentapi.NarrativeReport{Narrative: written})
	if err != nil || result.StatusCode != http.StatusAccepted {
		t.Fatalf("post the Narrative: %+v, %v, want 202", result, err)
	}
	got := env.Get(t, tr.Name)
	if got.Status.Narrative != written || got.Status.Phase != v1alpha1.PhaseRecovered {
		t.Errorf("status = %q in phase %s, want the Narrative in Recovered", got.Status.Narrative, got.Status.Phase)
	}
	requireChecks(t, got, map[string]bool{
		v1alpha1.CheckArgoCDSynced:     true,
		v1alpha1.CheckRolloutAvailable: true,
		v1alpha1.CheckNoNewRestarts:    true,
		v1alpha1.CheckAlertResolved:    true,
	})
	env.EndJob(t, narrative.Job.Name, testenv.JobComplete)
	if after := env.Get(t, tr.Name); after.Status.Narrative != written || after.Status.Phase != v1alpha1.PhaseRecovered {
		t.Errorf("the completed Job changed the status: %q in phase %s", after.Status.Narrative, after.Status.Phase)
	}
}

func TestOnlyTheNarrativeJobMayWriteTheNarrative(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	tr := recoveredTriage(t, env)
	env.StartNarrative(t, tr.Name)

	investigation, err := env.GetJob(t.Context(), tr.Status.JobName)
	if err != nil {
		t.Fatal(err)
	}
	pod := env.CreatePodFor(t, investigation, server.DefaultAgentServiceAccount)
	token := env.TokenFor(t, pod, server.DefaultAgentServiceAccount, agentapi.Audience)
	agent := env.ClientWithToken(tr.Name, token)

	if _, result, err := agent.GetNarrative(t.Context()); err == nil || result.StatusCode != http.StatusForbidden {
		t.Errorf("the investigation Job read the Narrative record: %+v, %v, want 403", result, err)
	}
	result, err := agent.PostNarrative(t.Context(), agentapi.NarrativeReport{Narrative: "A forged Narrative."})
	if err != nil || result.StatusCode != http.StatusForbidden {
		t.Errorf("the investigation Job wrote a Narrative: %+v, %v, want 403", result, err)
	}

	anonymous := env.ClientWithToken(tr.Name, "not-a-token")
	if _, result, err := anonymous.GetNarrative(t.Context()); err == nil || result.StatusCode != http.StatusUnauthorized {
		t.Errorf("an unauthenticated caller read the Narrative record: %+v, %v, want 401", result, err)
	}
	if got := env.Get(t, tr.Name); got.Status.Narrative != "" {
		t.Errorf("a refused caller wrote the Narrative %q", got.Status.Narrative)
	}
}

func TestNarrativeIsWrittenOnceAndIsBounded(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	tr := recoveredTriage(t, env)
	narrative := env.StartNarrative(t, tr.Name)

	written := "The checkout Deployment recovered after the approved change."
	for range 2 {
		result, err := narrative.API.PostNarrative(t.Context(), agentapi.NarrativeReport{Narrative: written})
		if err != nil || result.StatusCode != http.StatusAccepted {
			t.Fatalf("post the same Narrative: %+v, %v, want 202", result, err)
		}
	}
	result, err := narrative.API.PostNarrative(t.Context(),
		agentapi.NarrativeReport{Narrative: "A different account of the same Triage."})
	if err != nil || result.StatusCode != http.StatusConflict {
		t.Errorf("a second, different Narrative was accepted: %+v, %v, want 409", result, err)
	}
	for _, refused := range []string{"", "   ", strings.Repeat("x", v1alpha1.MaxNarrativeLength+1)} {
		result, err := narrative.API.PostNarrative(t.Context(), agentapi.NarrativeReport{Narrative: refused})
		if err != nil || result.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("a Narrative of %d characters was accepted: %+v, %v, want 422", len(refused), result, err)
		}
	}
	if got := env.Get(t, tr.Name); got.Status.Narrative != written {
		t.Errorf("Narrative = %q, want %q", got.Status.Narrative, written)
	}
}

func TestNarrativeRecordingIsMountedForALiveCapture(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources(), testenv.WithAgentRuntime(controller.AgentRuntimeConfig{
		Provider:               anthropicProvider,
		RecordingPVC:           recordingClaimName,
		NarrativeRecordingFile: "/recordings/narrative.json",
	}))
	tr := recoveredTriage(t, env)
	narrative := env.StartNarrative(t, tr.Name)

	pod := narrative.Job.Spec.Template.Spec
	variables := map[string]corev1.EnvVar{}
	for _, variable := range pod.Containers[0].Env {
		variables[variable.Name] = variable
	}
	if variables[agentapi.EnvModelProvider].Value != anthropicProvider {
		t.Errorf("provider = %q, want %q", variables[agentapi.EnvModelProvider].Value, anthropicProvider)
	}
	if got := variables[agentapi.EnvRecordingFile].Value; got != "/recordings/narrative.json" {
		t.Errorf("%s = %q, want the narrative recording", agentapi.EnvRecordingFile, got)
	}
	requireSecretKey(t, variables["ANTHROPIC_API_KEY"], controller.AgentCredentialsSecret, "ANTHROPIC_API_KEY", false)
	var mounted bool
	for _, volume := range pod.Volumes {
		if volume.PersistentVolumeClaim != nil {
			if volume.PersistentVolumeClaim.ClaimName != recordingClaimName {
				t.Errorf("recording claim = %q, want %q", volume.PersistentVolumeClaim.ClaimName, recordingClaimName)
			}
			mounted = true
		}
	}
	if !mounted {
		t.Error("a Narrative Job that captures a recording must mount the capture claim")
	}
	if pod.SecurityContext.FSGroup == nil || *pod.SecurityContext.FSGroup != 65532 {
		t.Error("the capture claim must be writable by filesystem group 65532")
	}
}

func TestFailedNarrativeJobLeavesTheVerificationOutcomeUnchanged(t *testing.T) {
	for _, end := range []struct {
		name string
		kind testenv.JobEnd
	}{
		{name: "failed", kind: testenv.JobFailed},
		{name: "timed out", kind: testenv.JobDeadlineExceeded},
	} {
		t.Run(end.name, func(t *testing.T) {
			t.Parallel()
			env := testenv.Start(t, withPatchResources())
			tr := recoveredTriage(t, env)
			before := env.Get(t, tr.Name)
			narrativeJob := before.Status.NarrativeJobName
			if narrativeJob == "" {
				t.Fatal("the Triage records no Narrative Job")
			}

			env.EndJob(t, narrativeJob, end.kind)
			settle()

			after := env.Get(t, tr.Name)
			if after.Status.Phase != before.Status.Phase {
				t.Errorf("phase = %s after the Narrative Job ended %s, want %s",
					after.Status.Phase, end.name, before.Status.Phase)
			}
			if !equality.Semantic.DeepEqual(after.Status.VerificationChecks, before.Status.VerificationChecks) {
				t.Errorf("Verification checks changed from %+v to %+v", before.Status.VerificationChecks,
					after.Status.VerificationChecks)
			}
			if after.Status.Narrative != "" {
				t.Errorf("a %s Narrative Job wrote %q", end.name, after.Status.Narrative)
			}
			if !equality.Semantic.DeepEqual(after.Status, before.Status) {
				t.Errorf("the %s Narrative Job changed the status:\nbefore %+v\nafter  %+v",
					end.name, before.Status, after.Status)
			}
			if jobs := jobsForTriage(t, env, tr.Name); len(jobs) != 2 {
				t.Errorf("Jobs for Triage %s = %v, want no replacement Narrative Job", tr.Name, jobs)
			}
		})
	}
}
