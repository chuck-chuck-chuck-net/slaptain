package controller

import (
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

// failedJob builds a Job carrying a JobFailed condition.
func failedJob(reason, msg string, status corev1.ConditionStatus) *batchv1.Job {
	return &batchv1.Job{Status: batchv1.JobStatus{
		Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: status, Reason: reason, Message: msg,
		}},
	}}
}

// podWithExit builds a pod whose init or main container terminated non-zero.
func podWithExit(name string, code int32, reason string, initContainer bool) corev1.Pod {
	cs := corev1.ContainerStatus{
		Name:  name,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Reason: reason}},
	}
	var p corev1.Pod
	if initContainer {
		p.Status.InitContainerStatuses = []corev1.ContainerStatus{cs}
	} else {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{cs}
	}
	return p
}

// ADR-018 R2: failed Jobs are reaped promptly, so the operator must first
// record what it can cheaply observe — the Job's failure condition and the
// failing container's exit code — into the owning CR's status.
func TestJobFailureSummary(t *testing.T) {
	tests := []struct {
		name    string
		job     *batchv1.Job
		pods    []corev1.Pod
		want    []string // substrings that must all appear
		wantNot []string
	}{
		{
			name: "condition and container exit code are both reported",
			job:  failedJob("BackoffLimitExceeded", "Job has reached the specified backoff limit", corev1.ConditionTrue),
			pods: []corev1.Pod{podWithExit("restore", 1, "Error", true)},
			want: []string{"BackoffLimitExceeded", "backoff limit", `container "restore" exited 1`, "Error"},
		},
		{
			name: "condition alone when no pods survive to inspect",
			job:  failedJob("DeadlineExceeded", "Job was active longer than specified deadline", corev1.ConditionTrue),
			pods: nil,
			want: []string{"DeadlineExceeded", "deadline"},
		},
		{
			name: "container exit code alone when the Job has no condition yet",
			job:  &batchv1.Job{},
			pods: []corev1.Pod{podWithExit("upload", 2, "", false)},
			want: []string{`container "upload" exited 2`},
		},
		{
			name:    "a JobFailed condition that is not True must be ignored",
			job:     failedJob("BackoffLimitExceeded", "not actually failed", corev1.ConditionFalse),
			pods:    nil,
			want:    []string{"no condition or container status recorded"},
			wantNot: []string{"BackoffLimitExceeded", "not actually failed"},
		},
		{
			name: "nothing observable still yields a non-empty message",
			job:  &batchv1.Job{},
			pods: nil,
			want: []string{"job failed"},
		},
		{
			name:    "a zero exit code is not a failure",
			job:     &batchv1.Job{},
			pods:    []corev1.Pod{podWithExit("restore", 0, "Completed", true)},
			want:    []string{"no condition or container status recorded"},
			wantNot: []string{"restore"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := jobFailureSummary(tc.job, tc.pods)
			if got == "" {
				t.Fatal("summary must never be empty — status would carry no cause at all")
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("summary %q does not contain %q", got, w)
				}
			}
			for _, w := range tc.wantNot {
				if strings.Contains(got, w) {
					t.Errorf("summary %q must not contain %q", got, w)
				}
			}
		})
	}
}

// The restore Job's download and wipe steps are init containers, so an init
// failure is the common case and must not be masked by the main container.
func TestJobFailureSummaryPrefersInitContainer(t *testing.T) {
	p := podWithExit("download", 3, "Error", true)
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "restore",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 9}},
	}}
	got := jobFailureSummary(&batchv1.Job{}, []corev1.Pod{p})
	if !strings.Contains(got, "download") {
		t.Errorf("expected the init container failure to be reported, got %q", got)
	}
}
