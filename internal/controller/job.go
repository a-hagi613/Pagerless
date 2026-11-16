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
	// agentContainerName is the name of the agent's only container.
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
	name := triageName + "-agent"
	if len(name) > maxJobNameLength {
		name = strings.TrimRight(triageName[:maxJobNameLength-len("-agent")], "-.") + "-agent"
	}
	return name
}

// AgentJobConfig holds the settings of the agent Job.
type AgentJobConfig struct {
	// Image is the agent container image.
	Image string
	// ServiceAccount is the read-only service account the agent runs as.
	ServiceAccount string
	// ServerURL is the base URL that the agent posts step events and its report to.
	ServerURL string
	// Deadline is the Job's active deadline.
	Deadline int64
}

// buildAgentJob returns the Job that runs the agent for a Triage. The agent
// has no write permissions, so the Job mounts no default service account token
// and projects a single token whose audience is the server's.
func buildAgentJob(triage *v1alpha1.Triage, cfg AgentJobConfig) *batchv1.Job {
	spec := triage.Spec
	env := []corev1.EnvVar{
		{Name: agentapi.EnvServerURL, Value: cfg.ServerURL},
		{Name: agentapi.EnvTokenFile, Value: tokenMountPath + "/" + tokenFileName},
		{Name: agentapi.EnvTriage, Value: triage.Name},
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
	labels := map[string]string{
		"app.kubernetes.io/name": "pagerless-agent",
		TriageLabel:              triage.Name,
	}
	return &batchv1.Job{
		Name:      AgentJobName(triage.Name),
		Namespace: triage.Namespace,
		Labels:    labels,
		Spec: batchv1.JobSpec{
			ActiveDeadlineSeconds: new(cfg.Deadline),
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
						Name:  agentContainerName,
						Image: cfg.Image,
						Env:   env,
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
						VolumeMounts: []corev1.VolumeMount{{
							Name:      tokenVolumeName,
							MountPath: tokenMountPath,
							ReadOnly:  true,
						}},
					}},
					Volumes: []corev1.Volume{{
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
