package controller

import (
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
	// TriageLabel labels every object created for a Triage with the Triage's name.
	TriageLabel = "pagerless.dev/triage"
	// agentContainerName is the name of the Job's only container.
	agentContainerName = "agent"
	// tokenVolumeName and tokenMountPath locate the projected service account token.
	tokenVolumeName = "pagerless-token"
	tokenMountPath  = "/var/run/pagerless"
	tokenFileName   = "token"
	// tokenExpirationSeconds is the lifetime of the projected token, which the kubelet refreshes.
	tokenExpirationSeconds = 600
	// agentUID is the non-root user and group the agent runs as.
	agentUID = 65532
	// maxJobNameLength keeps the Job name a valid DNS label with room for the controller's pod suffix.
	maxJobNameLength = 52
)

// AgentJobName returns the name of the agent Job for a Triage.
func AgentJobName(triageName string) string {
	return jobName(triageName, "-agent")
}

// narrativeJobName returns the name of the Narrative Job for a Triage. It is
// deterministic, so that a repeated reconcile creates no second Job.
func narrativeJobName(triageName string) string {
	return jobName(triageName, "-narrative")
}

// jobName names a Job for a Triage, shortened to a valid DNS label when the Triage's name is long.
func jobName(triageName, suffix string) string {
	name := triageName + suffix
	if len(name) > maxJobNameLength {
		name = strings.TrimRight(triageName[:maxJobNameLength-len(suffix)], "-.") + suffix
	}
	return name
}

// AgentJobConfig holds the settings of the agent Jobs.
type AgentJobConfig struct {
	// Image is the agent container image.
	Image string
	// ServiceAccount is the read-only service account the agent runs as.
	ServiceAccount string
	// ServerURL is the base URL that the agent posts step events, its report, and the Narrative to.
	ServerURL string
	// Deadline is the Job's active deadline.
	Deadline int64
	// Runtime selects the provider and read-only MCP endpoints.
	Runtime AgentRuntimeConfig
}

// jobSpec is what distinguishes the investigation Job from the Narrative Job.
type jobSpec struct {
	// name is the Job's name.
	name string
	// deadline is the Job's active deadline in seconds.
	deadline int64
	// env is the container environment.
	env []corev1.EnvVar
	// recordingPVC is the existing claim to mount at /recordings, when this Job writes a recording.
	recordingPVC string
}

// buildAgentJob returns the Job that runs the investigation for a Triage. The
// agent has no write permissions, so the Job mounts no default service account
// token and projects a single token whose audience is the server's.
func buildAgentJob(triage *v1alpha1.Triage, cfg AgentJobConfig) *batchv1.Job {
	spec := triage.Spec
	env := []corev1.EnvVar{
		{Name: agentapi.EnvServerURL, Value: cfg.ServerURL},
		{Name: agentapi.EnvTokenFile, Value: tokenMountPath + "/" + tokenFileName},
		{Name: agentapi.EnvTriage, Value: triage.Name},
		{Name: agentapi.EnvTask, Value: agentapi.TaskInvestigate},
		{Name: agentapi.EnvWorkloadKind, Value: string(spec.Workload.Kind)},
		{Name: agentapi.EnvWorkloadNamespace, Value: spec.Workload.Namespace},
		{Name: agentapi.EnvWorkloadName, Value: spec.Workload.Name},
	}
	if spec.Trigger.Pod != "" {
		env = append(env, corev1.EnvVar{Name: agentapi.EnvTriggeringPod, Value: spec.Trigger.Pod})
	}
	if spec.Hint != "" {
		env = append(env, corev1.EnvVar{Name: agentapi.EnvHint, Value: spec.Hint})
	}
	env = append(env, cfg.Runtime.environment()...)
	return buildJob(triage, cfg, jobSpec{
		name:         AgentJobName(triage.Name),
		deadline:     cfg.Deadline,
		env:          env,
		recordingPVC: cfg.Runtime.RecordingPVC,
	})
}

// buildNarrativeJob returns the Job that writes the Narrative of a Triage whose
// Verification has ended. It makes one model call and needs no Workload access,
// so it receives neither the Workload's identity nor any MCP endpoint.
func buildNarrativeJob(triage *v1alpha1.Triage, cfg AgentJobConfig) *batchv1.Job {
	runtimeEnv := cfg.Runtime.narrativeEnvironment()
	env := make([]corev1.EnvVar, 0, 4+len(runtimeEnv))
	env = append(env,
		corev1.EnvVar{Name: agentapi.EnvServerURL, Value: cfg.ServerURL},
		corev1.EnvVar{Name: agentapi.EnvTokenFile, Value: tokenMountPath + "/" + tokenFileName},
		corev1.EnvVar{Name: agentapi.EnvTriage, Value: triage.Name},
		corev1.EnvVar{Name: agentapi.EnvTask, Value: agentapi.TaskNarrative},
	)
	env = append(env, runtimeEnv...)
	spec := jobSpec{
		name:     narrativeJobName(triage.Name),
		deadline: cfg.Deadline,
		env:      env,
	}
	if cfg.Runtime.NarrativeRecordingFile != "" {
		spec.recordingPVC = cfg.Runtime.RecordingPVC
	}
	return buildJob(triage, cfg, spec)
}

// buildJob returns a Job that runs one short-lived agent process under the
// restricted security context that agent Job admission permits.
func buildJob(triage *v1alpha1.Triage, cfg AgentJobConfig, spec jobSpec) *batchv1.Job {
	labels := map[string]string{
		"app.kubernetes.io/name": "pagerless-agent",
		TriageLabel:              triage.Name,
	}
	job := &batchv1.Job{
		Name:      spec.name,
		Namespace: triage.Namespace,
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
						VolumeMounts: []corev1.VolumeMount{{Name: "temporary", MountPath: "/tmp"}, {
							Name:      tokenVolumeName,
							MountPath: tokenMountPath,
							ReadOnly:  true,
						}},
					}},
					Volumes: []corev1.Volume{{Name: "temporary",
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					}, {
						Name: tokenVolumeName,
						Projected: &corev1.ProjectedVolumeSource{
							Sources: []corev1.VolumeProjection{{
								ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
									Audience:          agentapi.Audience,
									ExpirationSeconds: new(int64(tokenExpirationSeconds)),
									Path:              tokenFileName,
								},
							}},
						},
					}},
				},
			},
		},
	}
	if spec.recordingPVC != "" {
		pod := &job.Spec.Template.Spec
		pod.SecurityContext.FSGroup = new(int64(agentUID))
		pod.Volumes = append(pod.Volumes, corev1.Volume{
			Name:                  "recordings",
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: spec.recordingPVC},
		})
		pod.Containers[0].VolumeMounts = append(pod.Containers[0].VolumeMounts,
			corev1.VolumeMount{Name: "recordings", MountPath: "/recordings"})
	}
	return job
}

// jobOutcome describes how an agent Job ended.
type jobOutcome struct {
	finished bool
	reason   string
	message  string
}

// outcomeOf reports whether the Job has ended and, if so, why.
func outcomeOf(job *batchv1.Job) jobOutcome {
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
			return jobOutcome{
				finished: true,
				reason:   v1alpha1.ReasonAgentEndedWithoutReport,
				message:  fmt.Sprintf("the agent Job failed without a report: %s", c.Message),
			}
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
