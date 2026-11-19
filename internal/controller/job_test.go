package controller

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// failedJob is a Job the Job controller marked failed, with one terminating pod.
func failedJob(exitCode int32) (*batchv1.Job, []corev1.Pod) {
	job := &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
		Reason: batchv1.JobReasonBackoffLimitExceeded, Message: "Job has reached the specified backoff limit",
	}}}}
	terminated := &corev1.ContainerStateTerminated{ExitCode: exitCode}
	pod := corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name: agentContainerName, State: corev1.ContainerState{Terminated: terminated},
	}}}}
	return job, []corev1.Pod{pod}
}

// TestTheBudgetExhaustedExitCodeNamesTheReason covers every documented exit code
// the agent uses to explain a Job that produced no report.
func TestTheBudgetExhaustedExitCodeNamesTheReason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code   int32
		reason string
	}{
		{exitCodeBudgetExhausted, v1alpha1.ReasonBudgetExhausted},
		{exitCodeProposalRefused, v1alpha1.ReasonValidationFailedTwice},
		{exitCodeRejected, v1alpha1.ReasonAgentEndedWithoutReport},
		{exitCodeProviderAuthFailed, v1alpha1.ReasonAgentEndedWithoutReport},
		{exitCodeProviderRejected, v1alpha1.ReasonAgentEndedWithoutReport},
		{exitCodeFailed, v1alpha1.ReasonAgentEndedWithoutReport},
	}
	for _, tc := range cases {
		job, pods := failedJob(tc.code)
		outcome := outcomeOf(job, pods)
		if !outcome.finished {
			t.Errorf("exit code %d: the Job is not finished", tc.code)
		}
		if outcome.reason != tc.reason {
			t.Errorf("exit code %d: reason = %q, want %q", tc.code, outcome.reason, tc.reason)
		}
	}
	job, pods := failedJob(exitCodeBudgetExhausted)
	if outcome := outcomeOf(job, pods); !contains(outcome.message, "budget") {
		t.Errorf("message = %q, want it to explain the exhausted budget", outcome.message)
	}
}

// TestADeadlineExceededJobIsNotABudgetExhaustion keeps the two causes apart.
func TestADeadlineExceededJobIsNotABudgetExhaustion(t *testing.T) {
	t.Parallel()
	job := &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded,
	}}}}
	if outcome := outcomeOf(job, nil); outcome.reason != v1alpha1.ReasonAgentDeadlineExceeded {
		t.Errorf("reason = %q, want AgentDeadlineExceeded", outcome.reason)
	}
}

// contains reports whether text holds part.
func contains(text, part string) bool {
	for i := 0; i+len(part) <= len(text); i++ {
		if text[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
