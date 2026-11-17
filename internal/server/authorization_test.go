package server_test

import (
	"fmt"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

const updateVerb = "update"

const (
	agentJobPolicy         = "pagerless-server-agent-jobs"
	executorServiceAccount = "pagerless-executor"
	githubWriteSecret      = "github-write"
)

func TestServerCannotCreateJobsThatEscapeTheAgentBoundary(t *testing.T) {
	env := testenv.Start(t)
	created := env.CreateTriage(t, testenv.NewTriage(checkout))
	tr := env.WaitForPhase(t, created.Name, v1alpha1.PhaseInvestigating)
	job, err := env.GetJob(t.Context(), tr.Status.JobName)
	if err != nil {
		t.Fatal(err)
	}
	asServer := serverAdmissionClient(t, env)
	waitForAgentJobPolicy(t, env, asServer, job)
	tests := []struct {
		name   string
		mutate func(*corev1.PodSpec)
	}{
		{"Executor identity", func(p *corev1.PodSpec) { p.ServiceAccountName = executorServiceAccount }},
		{"server identity", func(p *corev1.PodSpec) { p.ServiceAccountName = "pagerless-server" }},
		{"automatic API token", func(p *corev1.PodSpec) { p.AutomountServiceAccountToken = new(true) }},
		{"write Secret volume", func(p *corev1.PodSpec) {
			p.Volumes = append(p.Volumes, corev1.Volume{Name: "write-secret",
				Secret: &corev1.SecretVolumeSource{SecretName: githubWriteSecret}})
		}},
		{"write Secret environment", func(p *corev1.PodSpec) {
			p.Containers[0].Env = append(p.Containers[0].Env, corev1.EnvVar{Name: "WRITE_TOKEN", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{Name: githubWriteSecret, Key: "token"},
			}})
		}},
		{"write Secret envFrom", func(p *corev1.PodSpec) {
			p.Containers[0].EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{Name: githubWriteSecret}}}
		}},
		{"projected write Secret", func(p *corev1.PodSpec) {
			p.Volumes[1].Projected.Sources = []corev1.VolumeProjection{{
				Secret: &corev1.SecretProjection{Name: githubWriteSecret},
			}}
		}},
		{"extra projected write Secret", func(p *corev1.PodSpec) {
			p.Volumes[1].Projected.Sources = append(p.Volumes[1].Projected.Sources,
				corev1.VolumeProjection{Secret: &corev1.SecretProjection{Name: githubWriteSecret}})
		}},
		{"Executor audience", func(p *corev1.PodSpec) {
			p.Volumes[1].Projected.Sources[0].ServiceAccountToken.Audience = executorServiceAccount
		}},
		{"default API audience", func(p *corev1.PodSpec) {
			p.Volumes[1].Projected.Sources[0].ServiceAccountToken.Audience = ""
		}},
		{"long-lived token", func(p *corev1.PodSpec) {
			p.Volumes[1].Projected.Sources[0].ServiceAccountToken.ExpirationSeconds = new(int64(86400))
		}},
		{"unrelated capture claim", func(p *corev1.PodSpec) {
			p.Volumes = append(p.Volumes, corev1.Volume{Name: "recordings",
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "private-data"}})
		}},
		{"host path", func(p *corev1.PodSpec) {
			p.Volumes = append(p.Volumes, corev1.Volume{Name: "host", HostPath: &corev1.HostPathVolumeSource{Path: "/"}})
		}},
		{"CSI credentials", func(p *corev1.PodSpec) {
			p.Volumes = append(p.Volumes, corev1.Volume{Name: "csi", CSI: &corev1.CSIVolumeSource{
				Driver: "example.test", NodePublishSecretRef: &corev1.LocalObjectReference{Name: githubWriteSecret},
			}})
		}},
		{"image-pull credentials", func(p *corev1.PodSpec) {
			p.ImagePullSecrets = []corev1.LocalObjectReference{{Name: githubWriteSecret}}
		}},
		{"host network", func(p *corev1.PodSpec) { p.HostNetwork = true }},
		{"host PID", func(p *corev1.PodSpec) { p.HostPID = true }},
		{"host IPC", func(p *corev1.PodSpec) { p.HostIPC = true }},
		{"privileged container", func(p *corev1.PodSpec) {
			p.Containers[0].SecurityContext.Privileged = new(true)
			p.Containers[0].SecurityContext.AllowPrivilegeEscalation = new(true)
		}},
		{"privilege escalation", func(p *corev1.PodSpec) {
			p.Containers[0].SecurityContext.AllowPrivilegeEscalation = new(true)
		}},
		{"writable root", func(p *corev1.PodSpec) { p.Containers[0].SecurityContext.ReadOnlyRootFilesystem = new(false) }},
		{"root UID", func(p *corev1.PodSpec) { p.Containers[0].SecurityContext.RunAsUser = new(int64(0)) }},
		{"root group", func(p *corev1.PodSpec) { p.SecurityContext.FSGroup = new(int64(0)) }},
		{"extra group", func(p *corev1.PodSpec) { p.SecurityContext.SupplementalGroups = []int64{0} }},
		{"added capabilities", func(p *corev1.PodSpec) {
			p.Containers[0].SecurityContext.Capabilities.Add = []corev1.Capability{"SYS_ADMIN"}
		}},
		{"retained capabilities", func(p *corev1.PodSpec) { p.Containers[0].SecurityContext.Capabilities.Drop = nil }},
		{"unconfined seccomp", func(p *corev1.PodSpec) {
			p.Containers[0].SecurityContext.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}
		}},
		{"host port", func(p *corev1.PodSpec) {
			p.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 8080, HostPort: 8080}}
		}},
		{"init container", func(p *corev1.PodSpec) {
			p.InitContainers = []corev1.Container{{Name: "escape", Image: testenv.AgentImage}}
		}},
		{"second container", func(p *corev1.PodSpec) {
			p.Containers = append(p.Containers, corev1.Container{Name: "escape", Image: testenv.AgentImage})
		}},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			candidate := agentJobAdmissionCandidate(job, fmt.Sprintf("escape-%d", i))
			tc.mutate(&candidate.Spec.Template.Spec)
			err := asServer.Create(t.Context(), candidate)
			if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), agentJobPolicy) {
				t.Fatalf("malicious Job creation = %v, want independent agent policy refusal", err)
			}
			var stored batchv1.Job
			if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(candidate), &stored); !apierrors.IsNotFound(err) {
				t.Fatalf("refused Job read = %v, want NotFound", err)
			}
		})
	}
	// The policy targets the server, not an operator who deploys other workloads.
	adminJob := agentJobAdmissionCandidate(job, "operator-job")
	adminJob.Spec.Template.Spec.ServiceAccountName = executorServiceAccount
	if err := env.Client.Create(t.Context(), adminJob, client.DryRunAll); err != nil {
		t.Fatalf("operator Job was denied by server-specific policy: %v", err)
	}
}

func serverAdmissionClient(t *testing.T, env *testenv.Env) client.Client {
	t.Helper()
	c, err := client.New(env.ServerConfig, client.Options{Scheme: env.Client.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// agentJobAdmissionCandidate retains the actual builder's Pod template while
// dropping the first Job's API identity and API-generated selector labels.
func agentJobAdmissionCandidate(job *batchv1.Job, name string) *batchv1.Job {
	candidate := &batchv1.Job{Name: name, Namespace: job.Namespace, Spec: *job.Spec.DeepCopy()}
	candidate.Spec.Selector = nil
	for _, key := range []string{
		"batch.kubernetes.io/controller-uid", "controller-uid", "batch.kubernetes.io/job-name", "job-name",
	} {
		delete(candidate.Spec.Template.Labels, key)
	}
	return candidate
}

func waitForAgentJobPolicy(t *testing.T, env *testenv.Env, asServer client.Client, job *batchv1.Job) {
	t.Helper()
	probe := agentJobAdmissionCandidate(job, "agent-admission-readiness")
	probe.Spec.Template.Spec.ServiceAccountName = executorServiceAccount
	eventually(t, "the agent Job policy is enforced", func() bool {
		err := asServer.Create(t.Context(), probe, client.DryRunAll)
		return apierrors.IsForbidden(err) && strings.Contains(err.Error(), agentJobPolicy)
	})
	var policy admissionv1.ValidatingAdmissionPolicy
	if err := env.Client.Get(t.Context(), client.ObjectKey{Name: agentJobPolicy}, &policy); err != nil {
		t.Fatal(err)
	}
	if policy.Status.TypeChecking != nil && len(policy.Status.TypeChecking.ExpressionWarnings) != 0 {
		t.Fatalf("agent Job policy has CEL type errors: %+v", policy.Status.TypeChecking.ExpressionWarnings)
	}
	valid := agentJobAdmissionCandidate(job, "agent-admission-valid")
	if err := asServer.Create(t.Context(), valid, client.DryRunAll); err != nil {
		t.Fatalf("the production Job builder's template was denied: %v", err)
	}
}

func TestServerMayValidateAWorkloadButCannotUpdateItLive(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, workload{name: checkout})
	asServer, err := client.New(env.ServerConfig, client.Options{Scheme: env.Client.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: sandboxNamespace, Name: checkout}
	// Admission policy caches are asynchronous. Wait until this identity is denied an unchanged live update
	// before testing mutations; the readiness probe cannot change the Workload's desired state.
	eventually(t, "the dry-run-only policy is enforced", func() bool {
		probe := liveDeployment(t, env, checkout)
		err := asServer.Update(t.Context(), probe)
		return apierrors.IsForbidden(err) && strings.Contains(err.Error(), "pagerless-server-dry-run-only")
	})
	var update appsv1.Deployment
	if err := asServer.Get(t.Context(), key, &update); err != nil {
		t.Fatal(err)
	}
	update.Spec.Replicas = new(int32(2))
	if err := asServer.Update(t.Context(), &update, client.DryRunAll); err != nil {
		t.Fatalf("server-side dry run was denied: %v", err)
	}
	if got := liveDeployment(t, env, checkout); *got.Spec.Replicas != 1 {
		t.Fatalf("dry run persisted %d replicas", *got.Spec.Replicas)
	}
	update = *liveDeployment(t, env, checkout)
	update.Spec.Replicas = new(int32(3))
	err = asServer.Update(t.Context(), &update)
	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "dry-run") {
		t.Fatalf("live update = %v, want the admission policy's dry-run-only refusal", err)
	}
	if got := liveDeployment(t, env, checkout); *got.Spec.Replicas != 1 {
		t.Errorf("refused server update persisted %d replicas", *got.Spec.Replicas)
	}
	update = *liveDeployment(t, env, checkout)
	update.Spec.Replicas = new(int32(2))
	if err := env.Client.Update(t.Context(), &update); err != nil {
		t.Fatalf("another identity was denied by the server-only admission policy: %v", err)
	}
	for _, verb := range []string{"get", updateVerb} {
		review, err := env.Clientset.AuthorizationV1().SubjectAccessReviews().Create(t.Context(),
			&authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
				User: env.ServerConfig.Impersonate.UserName,
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Namespace: "outside", Verb: verb, Group: "apps", Resource: "deployments",
				},
			}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if review.Status.Allowed {
			t.Errorf("server may %s Workloads outside the PatchResources allowlist", verb)
		}
	}

	const statefulName = "ledger-authorization"
	stateful := &appsv1.StatefulSet{
		Name: statefulName, Namespace: sandboxNamespace,
		Spec: appsv1.StatefulSetSpec{
			Replicas: new(int32(1)), ServiceName: statefulName,
			Selector: update.Spec.Selector, Template: update.Spec.Template,
		},
	}
	if err := env.Client.Create(t.Context(), stateful); err != nil {
		t.Fatal(err)
	}
	stateful.Spec.Replicas = new(int32(2))
	if err := asServer.Update(t.Context(), stateful, client.DryRunAll); err != nil {
		t.Fatalf("StatefulSet dry run was denied: %v", err)
	}
	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(stateful), stateful); err != nil {
		t.Fatal(err)
	}
	if *stateful.Spec.Replicas != 1 {
		t.Fatal("StatefulSet dry run changed the stored replicas")
	}
	stateful.Spec.Replicas = new(int32(2))
	if err := asServer.Update(t.Context(), stateful); !apierrors.IsForbidden(err) {
		t.Fatalf("live StatefulSet update = %v, want Forbidden", err)
	}
}
