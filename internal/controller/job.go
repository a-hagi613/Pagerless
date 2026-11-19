package controller

import (
	"context"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
)

const (
	// PageLabel labels every object created for a Page with the Page's name.
	PageLabel = "pagerless.dev/page"
	// agentContainerName is the name of the Job's only container.
	agentContainerName = "agent"
	// reportTokenVolumeName locates the projected service account token whose
	// audience is bound to this Page and no other.
	reportTokenVolumeName = "report-token"
	// reportTokenMountPath is where the projected token appears in the agent.
	reportTokenMountPath = "/var/run/pagerless/report-token"
	// tokenFileName is the file inside the projected volume.
	tokenFileName = "token"
	// modelKeyVolumeName locates the one Secret key that holds the model API key.
	modelKeyVolumeName = "model-key"
	// modelKeyMountPath is where the model key appears in the agent.
	modelKeyMountPath = "/var/run/pagerless/model-key"
	// modelKeyFileName is the file inside the model-key volume.
	modelKeyFileName = "api-key"
	// tokenExpirationSeconds is the lifetime of the projected token, which the kubelet refreshes.
	tokenExpirationSeconds = 600
	// agentUID is the non-root user and group the agent runs as.
	agentUID = 65532
	// maxJobNameLength keeps the Job name a valid DNS label with room for the controller's pod suffix.
	maxJobNameLength = 52
)

// AgentJobName returns the name of the agent Job for a Page.
func AgentJobName(pageName string) string {
	return jobName(pageName, "-agent")
}

// narrativeJobName returns the name of the Narrative Job for a Page. It is
// deterministic, so that a repeated reconcile creates no second Job.
func narrativeJobName(pageName string) string {
	return jobName(pageName, "-narrative")
}

// jobName names a Job for a Page, shortened to a valid DNS label when the Page's name is long.
func jobName(pageName, suffix string) string {
	name := pageName + suffix
	if len(name) > maxJobNameLength {
		name = strings.TrimRight(pageName[:maxJobNameLength-len(suffix)], "-.") + suffix
	}
	return name
}

// AgentJobConfig holds the settings of the agent Jobs.
type AgentJobConfig struct {
	// Image is the agent container image.
	Image string
	// ServiceAccount is the zero-RBAC service account the agent runs as.
	ServiceAccount string
	// ServerURL is the base URL that the agent posts step events, tool calls, its
	// report, and the Narrative to.
	ServerURL string
	// Deadline is the Job's active deadline.
	Deadline int64
	// Runtime configures the model endpoint and the budgets.
	Runtime AgentRuntimeConfig
}

// RuntimeSource returns the model settings to record in a Job, so that a
// settings write through the Portal or the CLI takes effect on the next Page
// without restarting pagerless-server. It receives the startup configuration
// and may return it unchanged when the credentials Secret has no override.
type RuntimeSource func(ctx context.Context, fallback AgentRuntimeConfig) AgentRuntimeConfig

// jobConfig returns the Job configuration with the current model settings.
func (r RuntimeSource) jobConfig(ctx context.Context, cfg AgentJobConfig) AgentJobConfig {
	if r == nil {
		return cfg
	}
	cfg.Runtime = r(ctx, cfg.Runtime)
	return cfg
}

// jobSpec is what distinguishes the investigation Job from the Narrative Job.
type jobSpec struct {
	// name is the Job's name.
	name string
	// deadline is the Job's active deadline in seconds.
	deadline int64
	// env is the container environment.
	env []corev1.EnvVar
}

// AgentJob returns the Job that runs the investigation for a Page. The
// agent holds no Kubernetes RBAC, so the Job mounts no automatic service
// account token and projects one token whose audience names this Page alone. It
// mounts exactly two credentials: that report token and the model API key.
func AgentJob(page *v1alpha1.Page, cfg AgentJobConfig) *batchv1.Job {
	env := []corev1.EnvVar{
		{Name: agentapi.EnvServerURL, Value: cfg.ServerURL},
		{Name: agentapi.EnvTokenFile, Value: reportTokenMountPath + "/" + tokenFileName},
		{Name: agentapi.EnvPage, Value: page.Name},
		{Name: agentapi.EnvPageUID, Value: string(page.UID)},
		{Name: agentapi.EnvTask, Value: agentapi.TaskInvestigate},
	}
	if page.Spec.Hint != "" {
		env = append(env, corev1.EnvVar{Name: agentapi.EnvHint, Value: page.Spec.Hint})
	}
	env = append(env, cfg.Runtime.environment()...)
	return buildJob(page, cfg, jobSpec{
		name:     AgentJobName(page.Name),
		deadline: cfg.Deadline,
		env:      env,
	})
}

// NarrativeJob returns the Job that writes the Narrative of a Page whose
// Verification has ended. It makes one model call and needs no tool access, but
// it authenticates to the server with the same per-Page audience.
func NarrativeJob(page *v1alpha1.Page, cfg AgentJobConfig) *batchv1.Job {
	env := make([]corev1.EnvVar, 0, 5+len(cfg.Runtime.modelEnvironment()))
	env = append(env,
		corev1.EnvVar{Name: agentapi.EnvServerURL, Value: cfg.ServerURL},
		corev1.EnvVar{Name: agentapi.EnvTokenFile, Value: reportTokenMountPath + "/" + tokenFileName},
		corev1.EnvVar{Name: agentapi.EnvPage, Value: page.Name},
		corev1.EnvVar{Name: agentapi.EnvPageUID, Value: string(page.UID)},
		corev1.EnvVar{Name: agentapi.EnvTask, Value: agentapi.TaskNarrative},
	)
	env = append(env, cfg.Runtime.modelEnvironment()...)
	return buildJob(page, cfg, jobSpec{
		name:     narrativeJobName(page.Name),
		deadline: cfg.Deadline,
		env:      env,
	})
}

// buildJob returns a Job that runs one short-lived agent process under the
// restricted security context that agent Job admission permits.
func buildJob(page *v1alpha1.Page, cfg AgentJobConfig, spec jobSpec) *batchv1.Job {
	labels := map[string]string{
		"app.kubernetes.io/name": "pagerless-agent",
		PageLabel:                page.Name,
	}
	job := &batchv1.Job{
		Name:      spec.name,
		Namespace: page.Namespace,
		Labels:    labels,
		Spec: batchv1.JobSpec{
			ActiveDeadlineSeconds: new(spec.deadline),
			BackoffLimit:          new(int32(0)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					ServiceAccountName:           cfg.ServiceAccount,
					AutomountServiceAccountToken: new(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   new(true),
						RunAsUser:      new(int64(agentUID)),
						RunAsGroup:     new(int64(agentUID)),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:            agentContainerName,
						Image:           cfg.Image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Env:             spec.env,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: new(false),
							ReadOnlyRootFilesystem:   new(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("512Mi"),
							},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "temporary", MountPath: "/tmp"},
							{Name: reportTokenVolumeName, MountPath: reportTokenMountPath, ReadOnly: true},
							{Name: modelKeyVolumeName, MountPath: modelKeyMountPath, ReadOnly: true},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "temporary", EmptyDir: &corev1.EmptyDirVolumeSource{}},
						{
							Name: reportTokenVolumeName,
							Projected: &corev1.ProjectedVolumeSource{
								Sources: []corev1.VolumeProjection{{
									ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
										Audience:          agentapi.AudienceForPage(string(page.UID)),
										ExpirationSeconds: new(int64(tokenExpirationSeconds)),
										Path:              tokenFileName,
									},
								}},
							},
						},
						{
							Name: modelKeyVolumeName,
							Secret: &corev1.SecretVolumeSource{
								SecretName: cfg.Runtime.secretName(),
								Items: []corev1.KeyToPath{{
									Key:  cfg.Runtime.secretKey(),
									Path: modelKeyFileName,
								}},
							},
						},
					},
				},
			},
		},
	}
	return job
}

// jobOutcome describes how an agent Job ended.
type jobOutcome struct {
	finished bool
	reason   string
	message  string
}

// The agent's exit codes, which tell the server why a Job produced no report
// (spec §3.6.5). A budget that ran out is not an agent failure that needs a
// retry: it is a Page that ended with the Findings it had gathered.
const (
	exitCodeFailed             = 1
	exitCodeBudgetExhausted    = 2
	exitCodeProposalRefused    = 3
	exitCodeRejected           = 4
	exitCodeProviderAuthFailed = 5
	exitCodeProviderRejected   = 6
)

// outcomeOf reports whether the Job has ended and, if so, why. The pods carry
// the agent's exit code, which distinguishes an exhausted budget from a crash.
func outcomeOf(job *batchv1.Job, pods []corev1.Pod) jobOutcome {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobFailed:
			if c.Reason == batchv1.JobReasonDeadlineExceeded {
				return jobOutcome{
					finished: true,
					reason:   v1alpha1.ReasonAgentDeadlineExceeded,
					message: fmt.Sprintf("the agent Job exceeded its active deadline of %d seconds without a report",
						ptr.Deref(job.Spec.ActiveDeadlineSeconds, 0)),
				}
			}
			return outcomeForExitCode(agentExitCode(pods), c.Message)
		case batchv1.JobComplete:
			return jobOutcome{
				finished: true,
				reason:   v1alpha1.ReasonAgentEndedWithoutReport,
				message:  "the agent Job completed without a report",
			}
		}
	}
	return jobOutcome{}
}

// outcomeForExitCode names the reason the agent's documented exit codes stand for.
func outcomeForExitCode(code *int32, detail string) jobOutcome {
	if code == nil {
		return jobOutcome{
			finished: true,
			reason:   v1alpha1.ReasonAgentEndedWithoutReport,
			message:  fmt.Sprintf("the agent Job failed without a report: %s", detail),
		}
	}
	switch *code {
	case exitCodeBudgetExhausted:
		return jobOutcome{
			finished: true,
			reason:   v1alpha1.ReasonBudgetExhausted,
			message:  "an investigation budget was exhausted; the Findings gathered so far are kept",
		}
	case exitCodeProposalRefused:
		return jobOutcome{
			finished: true,
			reason:   v1alpha1.ReasonValidationFailedTwice,
			message:  "the agent ended because the server refused its Proposal twice",
		}
	case exitCodeRejected:
		return jobOutcome{
			finished: true,
			reason:   v1alpha1.ReasonAgentEndedWithoutReport,
			message:  "the server refused the agent, so the investigation stopped",
		}
	case exitCodeProviderAuthFailed, exitCodeProviderRejected, exitCodeFailed:
		return jobOutcome{
			finished: true,
			reason:   v1alpha1.ReasonAgentEndedWithoutReport,
			message:  fmt.Sprintf("the agent Job failed with exit code %d without a report", *code),
		}
	default:
		return jobOutcome{
			finished: true,
			reason:   v1alpha1.ReasonAgentEndedWithoutReport,
			message:  fmt.Sprintf("the agent Job failed with exit code %d without a report", *code),
		}
	}
}

// agentExitCode returns the exit code of the agent container in the first pod
// that reported one.
func agentExitCode(pods []corev1.Pod) *int32 {
	for i := range pods {
		for _, status := range pods[i].Status.ContainerStatuses {
			if status.Name != agentContainerName {
				continue
			}
			terminated := status.State.Terminated
			if terminated == nil {
				terminated = status.LastTerminationState.Terminated
			}
			if terminated != nil {
				return &terminated.ExitCode
			}
		}
	}
	return nil
}
