/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"fmt"
	"sort"
	"strings"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
	"github.com/chuck-chuck-chuck-net/slaptain/operator/internal/suffixprobe"
)

// backupSourceConvergedCondition is the condition a SlapdBackup carries to
// record whether the cluster it read from was converged at backup time.
const backupSourceConvergedCondition = "SourceConverged"

// replicationConvergedCondition is the SlapdCluster condition this consumes.
const replicationConvergedCondition = "ReplicationConverged"

// backupSourcePod names the pod a backup's artifact is read from. The backup Job
// mounts pod-0's config/data PVCs and (by default) pins itself to pod-0's node,
// so pod-0 is the source by construction — see buildBackupJob, which this must
// agree with (pinned by TestBackupSourcePodMatchesJobAffinity).
func backupSourcePod(sc *ldapv1alpha1.SlapdCluster) string {
	return sc.Name + "-0"
}

// sourceConvergedCondition derives a SlapdBackup's SourceConverged condition
// from the SlapdCluster's own ReplicationConverged condition (ADR-014 amendment
// 2026-09-12).
//
// A backup always takes a backup — this condition gates nothing. It records the
// circumstances the artifact was taken under, so that a restore, months later,
// can tell whether the dump came off a pod that was current with its peers.
//
// It deliberately CONSUMES the cluster's signal rather than re-deriving one:
// judging replication health is the SlapdCluster controller's job (its CSN
// monitoring already does it on a 60 s cadence, ADR-008), and a second,
// backup-local notion of "converged" would be a second authority that can
// disagree with the first. The freshness of the verdict is therefore the
// freshness of that tick, which the message makes explicit.
//
// Three shapes:
//   - the cluster has the condition → mirror its Status, carrying its message;
//   - the cluster is not a replication participant → True/NotReplicated: pod-0
//     is the only writable copy, so its view is by construction the whole truth;
//   - the cluster is a participant but has not produced the signal yet →
//     Unknown/NoConvergenceSignal. Silence is not evidence of convergence.
func sourceConvergedCondition(sc *ldapv1alpha1.SlapdCluster, generation int64, now metav1.Time) metav1.Condition {
	cond := metav1.Condition{
		Type:               backupSourceConvergedCondition,
		LastTransitionTime: now,
		ObservedGeneration: generation,
	}

	if rc := apimeta.FindStatusCondition(sc.Status.Conditions, replicationConvergedCondition); rc != nil {
		switch rc.Status {
		case metav1.ConditionTrue:
			cond.Status = metav1.ConditionTrue
			cond.Reason = "ClusterConverged"
		case metav1.ConditionFalse:
			cond.Status = metav1.ConditionFalse
			cond.Reason = "ClusterDiverged"
		default:
			cond.Status = metav1.ConditionUnknown
			cond.Reason = "ConvergenceUnknown"
		}
		cond.Message = fmt.Sprintf(
			"SlapdCluster %s reported %s=%s (%s) as of %s: %s",
			sc.Name, replicationConvergedCondition, rc.Status, rc.Reason,
			rc.LastTransitionTime.UTC().Format("2006-01-02T15:04:05Z"), rc.Message)
		return cond
	}

	if !isReplicationParticipant(sc) {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "NotReplicated"
		cond.Message = fmt.Sprintf(
			"SlapdCluster %s is not replicated; the backup source is the only writable copy", sc.Name)
		return cond
	}

	cond.Status = metav1.ConditionUnknown
	cond.Reason = "NoConvergenceSignal"
	cond.Message = fmt.Sprintf(
		"SlapdCluster %s is a replication participant but reports no %s condition yet; "+
			"convergence of the backup source is unknown",
		sc.Name, replicationConvergedCondition)
	return cond
}

// backupSuffixHealthyCondition is the condition a SlapdBackup carries to record
// whether the source pod's suffix entry was a real, restorable entry at backup
// time (ADR-025). Record-only, like SourceConverged: nothing here can refuse,
// delay or fail a backup — a backup always takes a backup.
const backupSuffixHealthyCondition = "SourceSuffixHealthy"

// The backup's suffix probe is the shared one (internal/suffixprobe), which the
// DataPresent condition and slctl inspect also use — one implementation of
// ADR-025's ordinary-then-ManageDsaIT classification, so the three cannot drift
// apart on what counts as a glue. These aliases keep the local vocabulary.
type suffixProbeOutcome = suffixprobe.Outcome

const (
	// suffixProbeVisible — the ordinary base search returned the entry.
	suffixProbeVisible = suffixprobe.OutcomeVisible
	// suffixProbeGlue — hidden from ordinary search, and ManageDsaIT revealed a
	// glue entry (ADR-025: the multi-site seed-race artifact). The artifact this
	// backup produces will FAIL restore preflight.
	suffixProbeGlue = suffixprobe.OutcomeGlue
	// suffixProbeMissing — hidden from ordinary search and ManageDsaIT found
	// nothing either: the suffix entry does not exist on the source pod.
	suffixProbeMissing = suffixprobe.OutcomeMissing
	// suffixProbeError — the probe itself failed (dial, bind, search error) or
	// found something it cannot classify. Silence is not evidence of health.
	suffixProbeError = suffixprobe.OutcomeError
)

// sourceSuffixHealthyCondition shapes the SourceSuffixHealthy condition from a
// probe outcome. detail carries the probe's specifics (glue entryUUID, error
// text) into the message.
func sourceSuffixHealthyCondition(outcome suffixProbeOutcome, detail string, generation int64, now metav1.Time) metav1.Condition {
	cond := metav1.Condition{
		Type:               backupSuffixHealthyCondition,
		LastTransitionTime: now,
		ObservedGeneration: generation,
	}
	withDetail := func(msg string) string {
		if detail == "" {
			return msg
		}
		return msg + " (" + detail + ")"
	}
	switch outcome {
	case suffixProbeVisible:
		cond.Status = metav1.ConditionTrue
		cond.Reason = "SuffixEntryVisible"
		cond.Message = withDetail("the source pod's suffix entry is a real entry, visible to ordinary searches")
	case suffixProbeGlue:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "GlueSuffix"
		cond.Message = withDetail("the source pod's suffix entry is a hidden GLUE entry " +
			"(multi-site seed race, ADR-025) — this artifact will FAIL restore preflight; " +
			"take the backup from a pod whose suffix entry is real")
	case suffixProbeMissing:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "SuffixMissing"
		cond.Message = withDetail("the source pod has no suffix entry at all — the artifact is empty or truncated")
	default:
		cond.Status = metav1.ConditionUnknown
		cond.Reason = "CheckFailed"
		cond.Message = withDetail("could not probe the source pod's suffix entry; " +
			"silence is not evidence of health")
	}
	return cond
}

// isReplicationParticipant reports whether writes can reach this cluster's data
// anywhere other than the pod a backup reads. False only for a genuinely
// standalone cluster.
func isReplicationParticipant(sc *ldapv1alpha1.SlapdCluster) bool {
	if !sc.Spec.Replication.Enabled {
		return false
	}
	return sc.Spec.Replicas > 1 || len(sc.Spec.Replication.ExternalPeers) > 0
}

// normalizedCSNVector makes a contextCSN vector safe to store in status:
// trimmed, blank-free, de-duplicated and sorted, so the same server state always
// produces the same slice and a re-read never churns the status subresource.
// The caller's slice is never mutated.
func normalizedCSNVector(csns []string) []string {
	seen := make(map[string]struct{}, len(csns))
	out := make([]string, 0, len(csns))
	for _, c := range csns {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// shouldRecordSourceCircumstances decides whether this reconcile pass must
// stamp the source circumstances (sourcePod / sourceContextCSN /
// SourceConverged / SourceSuffixHealthy) onto a backup's status.
//
// NOTE (staging commit): this is a faithful extraction of the trigger the
// controller used until now — "record iff the backup Job does not exist yet" —
// so the extraction itself changes no behaviour. The table test pinned against
// it is red on the incident shape, which is the point.
func shouldRecordSourceCircumstances(st ldapv1alpha1.SlapdBackupStatus, jobExists bool) bool {
	return !jobExists
}
