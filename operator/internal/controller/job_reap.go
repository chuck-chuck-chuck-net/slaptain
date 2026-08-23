package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Job reaping — ADR-018.
//
// Any pod object that names a PVC blocks that PVC's deletion for as long as the
// object exists; Succeeded and Failed pods hold it exactly as firmly as Running
// ones (upstream `podUsesPVCForDeletion` tests only "scheduled" plus a name
// match — there is no phase check). Every co-located Job the operator creates
// therefore holds a *lease* on slapd PVC lifecycle, and a leaked lease silently
// blocks ADR-012 case-2 recovery for the pods it pins.
//
// So the operator owns the reaping of every Job it creates (R1), on success and
// on failure alike (R2), and only ever from an already-persisted terminal state
// so that a re-entrant reconcile cannot recreate a destructive Job (R3).
// `TTLSecondsAfterFinished` remains as a crash backstop only (R4).

// restoreIDLabel selects the Jobs belonging to one restore attempt.
const restoreIDLabel = "ldap.chuck-chuck-chuck.net/restore-id"

// reapJob deletes one Job together with its pods. Safe to call repeatedly.
func reapJob(ctx context.Context, c client.Client, namespace, name string) error {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if err := c.Delete(ctx, job, cascadingDelete()); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("reap Job %s/%s: %w", namespace, name, err)
	}
	return nil
}

// reapJobsByLabel deletes every Job in the namespace carrying the given label,
// together with their pods. Used for restores, where one attempt fans out to a
// Job per pod across every database in the restore window; the shared
// restore-id label reaps them as a set.
//
// List+Delete rather than DeleteAllOf on purpose: a collection delete needs the
// `deletecollection` verb, which the operator's ClusterRole does not grant (and
// need not).
func reapJobsByLabel(ctx context.Context, c client.Client, namespace, key, value string) error {
	var jobs batchv1.JobList
	if err := c.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{key: value}); err != nil {
		return fmt.Errorf("list Jobs in %s for %s=%s: %w", namespace, key, value, err)
	}
	var errs []error
	for i := range jobs.Items {
		j := &jobs.Items[i]
		if j.DeletionTimestamp != nil {
			continue // already reaped; the pods go with it
		}
		if err := c.Delete(ctx, j, cascadingDelete()); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("reap Job %s/%s: %w", namespace, j.Name, err))
		}
	}
	return errors.Join(errs...)
}

// cascadingDelete forces background propagation.
//
// This is the load-bearing detail of the whole ADR: a Job deleted without an
// explicit policy can leave its pods behind, and the *pod* is what holds the
// PVC lease. An orphaning delete would report success while releasing nothing.
func cascadingDelete() client.DeleteOption {
	policy := metav1.DeletePropagationBackground
	return client.PropagationPolicy(policy)
}

// jobFailureSummary renders a one-line cause for a failed Job from the Job's own
// condition plus its pods' terminated container states.
//
// ADR-018 R2 reaps failed Jobs promptly, which removes the object an operator
// would otherwise have inspected. Whatever the operator can cheaply observe is
// therefore recorded in the owning CR's status *before* the reap. Note the
// scope: this is status metadata (condition reason, exit code), not log
// retention — Job output belongs in the user's log aggregation, and shipping it
// through the operator to work around a PVC lifecycle constraint would be the
// wrong trade.
func jobFailureSummary(job *batchv1.Job, pods []corev1.Pod) string {
	var parts []string

	if job != nil {
		for _, c := range job.Status.Conditions {
			if c.Type != batchv1.JobFailed || c.Status != corev1.ConditionTrue {
				continue
			}
			switch {
			case c.Reason != "" && c.Message != "":
				parts = append(parts, c.Reason+": "+c.Message)
			case c.Message != "":
				parts = append(parts, c.Message)
			case c.Reason != "":
				parts = append(parts, c.Reason)
			}
			break
		}
	}

	if ex := firstFailedContainer(pods); ex != "" {
		parts = append(parts, ex)
	}

	if len(parts) == 0 {
		return "job failed (no condition or container status recorded)"
	}
	return strings.Join(parts, " ")
}

// firstFailedContainer describes the first container that terminated non-zero,
// init containers included (the restore Job's download/wipe steps are init
// containers, so a failure there is the common case).
func firstFailedContainer(pods []corev1.Pod) string {
	for _, p := range pods {
		for _, cs := range append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
			t := cs.State.Terminated
			if t == nil || t.ExitCode == 0 {
				continue
			}
			if t.Reason != "" {
				return fmt.Sprintf("(container %q exited %d: %s)", cs.Name, t.ExitCode, t.Reason)
			}
			return fmt.Sprintf("(container %q exited %d)", cs.Name, t.ExitCode)
		}
	}
	return ""
}

// jobPods lists the pods belonging to a Job via the built-in `job-name` label.
func jobPods(ctx context.Context, c client.Client, namespace, jobName string) []corev1.Pod {
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(namespace),
		client.MatchingLabels{"job-name": jobName}); err != nil {
		return nil // best-effort: the summary degrades to the Job condition alone
	}
	return pods.Items
}
