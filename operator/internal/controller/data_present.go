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
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// podPresence is one pod's answer to "is the suffix's root entry visible to an
// ordinary base search?". present and glued are only meaningful when assessed
// is true.
type podPresence struct {
	pod string
	// readOnly: this pod belongs to the read-only consumer StatefulSet.
	readOnly bool
	// assessed: the probe reached a verdict on this pod. False when the pod
	// could not be dialled, bound, or searched — "could not read it" is never
	// evidence of absence.
	assessed bool
	// present: an ordinary base search returned the suffix entry.
	present bool
	// glued: a ManageDsaIT search positively identified a glue entry — this pod
	// is silently broken for base searches (ADR-025).
	glued bool
}

// probeTarget is one pod the suffix probe visits, with the headless-service
// host it is reached at.
type probeTarget struct {
	pod      string
	host     string
	readOnly bool
}

// suffixProbeTargets lists the pods the DataPresent probe visits: every RW pod,
// plus every read-only consumer pod when the cluster has any.
//
// The RO fleet is in scope because a glue suffix propagates to it — ADR-025
// evidence item 5 measured the incident site's RO pod carrying the same glue
// with the same entryUUID as its provider, a consumer having initial-synced the
// corruption faithfully. With readReplicas=0 (the common case) the list is the
// RW list and nothing else: no extra dials, no behaviour change.
//
// Both host forms follow the pods' own headless services, the same naming every
// other per-pod loop in the operator uses (cn=config is node-local, ADR-002).
func suffixProbeTargets(sc *ldapv1alpha1.SlapdCluster, clusterDomain string) []probeTarget {
	replicas := sc.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}
	targets := make([]probeTarget, 0, replicas+sc.Spec.ReadReplicas)
	for i := int32(0); i < replicas; i++ {
		pod := fmt.Sprintf("%s-%d", sc.Name, i)
		targets = append(targets, probeTarget{
			pod:  pod,
			host: fmt.Sprintf("%s.%s-headless.%s.svc.%s", pod, sc.Name, sc.Namespace, clusterDomain),
		})
	}
	for i := int32(0); i < sc.Spec.ReadReplicas; i++ {
		pod := fmt.Sprintf("%s-readonly-%d", sc.Name, i)
		targets = append(targets, probeTarget{
			pod:      pod,
			host:     fmt.Sprintf("%s.%s-readonly-headless.%s.svc.%s", pod, sc.Name, sc.Namespace, clusterDomain),
			readOnly: true,
		})
	}
	return targets
}

// databaseKnownPopulated reports whether this database is known ever to have
// held data. Three independent, monotone sources of positive evidence: the
// operator seeded it (ADR-012), a bootstrapFrom restore loaded it (ADR-014), or
// its root entry was observed on a pod at least once.
//
// It decides one thing only: whether an all-empty reading is an alert or a
// database still waiting for its first data. Never an input to any write.
func databaseKnownPopulated(sd *ldapv1alpha1.SlapdDatabase) bool {
	return sd.Status.SeedApplied || sd.Status.RestoreApplied || sd.Status.DataObserved
}

// dataWasObserved reports whether this pass saw the root entry on any pod —
// the only thing that may set the DataObserved latch. Positive evidence only:
// a glue is not data (it is the corpse of an entry, and latching on it would
// turn a never-populated database's emptiness into a DataMissing alert), and an
// unassessable pod is not evidence of anything.
func dataWasObserved(results []podPresence) bool {
	for _, r := range results {
		if r.assessed && r.present {
			return true
		}
	}
	return false
}

// aggregateDataPresent computes the DataPresent verdict from per-pod probes.
//
// Pure observability (ADR-012: never drives reconciler action). Three rules,
// each bought with an incident or a measured gap:
//
//   - Every assessed pod must show the root entry (ADR-025). A suffix demoted
//     to a hidden GLUE entry is invisible to ordinary searches on exactly one
//     pod while its peers look healthy, so an any-pod-visible verdict reads
//     True over a silently broken pod — the blindness the 2026-09-13 incident
//     rode on. A ManageDsaIT-confirmed glue is reported as such: it is positive
//     evidence of corruption on that pod and stands alone, needing neither a
//     healthy peer to contrast with nor any knowledge of the database's past —
//     and that holds whatever the pod's role is, so a glued RO pod reports
//     GlueSuffix like any other.
//   - An all-empty reading means different things on different databases, and
//     knownPopulated is what separates them (2026-09-14). A database that was
//     seeded, restored, or has been seen holding data is missing its data:
//     alert. A database that has never held data — a non-founder site of a mesh
//     waiting for its first refresh (ADR-025 D1 tells peers to omit spec.seed),
//     a migration cluster waiting for the legacy provider (ADR-011) — is simply
//     not there yet, and must not be reported as loss. Unknown falls to
//     Unknown; no timer is involved, because the thing that makes emptiness
//     alarming is evidence that data once existed, not elapsed time.
//   - A divergence confined to the read-only fleet gets its OWN reason,
//     DataMissingOnReadOnlyPods. RO pods are probed because a glue propagates
//     to them (ADR-025 evidence item 5), but they are the one pod class whose
//     missing suffix has a routine benign cause: a consumer performing its
//     initial sync has not received the root entry yet. The verdict stays
//     False — the absence is measured, not unknown — while the distinct reason
//     lets a reader and an alert rule separate "a read-only replica is behind
//     or broken" from "a writable pod is broken". A writable pod's divergence
//     always outranks it: DataMissingOnPods fires whenever any RW pod hides the
//     entry, whatever the RO fleet shows.
//
// Message wording is role-aware only when RO pods were actually probed, so a
// readReplicas=0 cluster produces byte-identical messages to the RW-only
// implementation.
func aggregateDataPresent(suffix string, results []podPresence, knownPopulated bool) (metav1.ConditionStatus, string, string) {
	var present, presentRO, missing, missingRW, missingRO, unreached, glued []string
	haveRO := false
	for _, r := range results {
		if r.readOnly {
			haveRO = true
		}
		switch {
		case !r.assessed:
			unreached = append(unreached, r.pod)
		case r.glued:
			glued = append(glued, r.pod)
		case r.present:
			present = append(present, r.pod)
			if r.readOnly {
				presentRO = append(presentRO, r.pod)
			}
		default:
			missing = append(missing, r.pod)
			if r.readOnly {
				missingRO = append(missingRO, r.pod)
			} else {
				missingRW = append(missingRW, r.pod)
			}
		}
	}

	uncheckedNote := ""
	if len(unreached) > 0 {
		uncheckedNote = fmt.Sprintf(" (unreachable, not assessed: %s)", strings.Join(unreached, ", "))
	}

	// "RW pod" is kept verbatim when no RO pod was probed: the RO extension is
	// reach, and must not churn the standing messages of every other cluster.
	scope := "RW pod"
	if haveRO {
		scope = "pod"
	}

	switch {
	case len(glued) > 0:
		return metav1.ConditionFalse, "GlueSuffix",
			fmt.Sprintf("root entry %s is a hidden GLUE entry on pod(s) %s "+
				"(ManageDsaIT-confirmed; ADR-025 multi-site seed race) — base searches against "+
				"those pods return nothing and backups taken from them are unrestorable; "+
				"heal per the ADR-025 runbook. Informational only (ADR-012)%s",
				suffix, strings.Join(glued, ", "), uncheckedNote)
	case len(present) == 0 && len(missing) == 0:
		return metav1.ConditionUnknown, "NoReachablePod",
			fmt.Sprintf("could not reach any %s to verify data presence", scope)
	case len(missing) == 0 && len(presentRO) == 0:
		return metav1.ConditionTrue, "RootEntryVisible",
			fmt.Sprintf("root entry %s visible on all %d reached RW pod(s)%s",
				suffix, len(present), uncheckedNote)
	case len(missing) == 0:
		return metav1.ConditionTrue, "RootEntryVisible",
			fmt.Sprintf("root entry %s visible on all %d reached RW pod(s) and %d read-only pod(s)%s",
				suffix, len(present)-len(presentRO), len(presentRO), uncheckedNote)
	case len(missingRW) == 0 && len(present) > 0:
		return metav1.ConditionFalse, "DataMissingOnReadOnlyPods",
			fmt.Sprintf("root entry %s absent on read-only pod(s) %s while visible on every reached "+
				"writable pod (%s) — expected transiently while a read-only replica performs its "+
				"initial sync; if it persists, that replica is broken or its syncrepl stanzas are "+
				"not converging. Writable pods are unaffected; informational only (ADR-012)%s",
				suffix, strings.Join(missingRO, ", "), strings.Join(present, ", "), uncheckedNote)
	case len(present) == 0 && !knownPopulated:
		return metav1.ConditionUnknown, "NoDataYet",
			fmt.Sprintf("root entry %s is not present on any reached %s, and this database has "+
				"never been seeded, restored or observed holding data — it is waiting for its first "+
				"data (replication from a founder site or a legacy provider). Not an alert: nothing "+
				"is known to have been lost%s", suffix, scope, uncheckedNote)
	case len(present) == 0:
		return metav1.ConditionFalse, "DataMissing",
			fmt.Sprintf("root entry %s not visible on any reachable %s — possible data loss; "+
				"this condition is informational and does not trigger operator action (see ADR-012)%s",
				suffix, scope, uncheckedNote)
	default:
		return metav1.ConditionFalse, "DataMissingOnPods",
			fmt.Sprintf("root entry %s hidden or absent on pod(s) %s while visible on %s — "+
				"likely a hidden glue suffix entry from a multi-site seed race (ADR-025); "+
				"verify with a ManageDSAIT base search; informational only (ADR-012)%s",
				suffix, strings.Join(missing, ", "), strings.Join(present, ", "), uncheckedNote)
	}
}
