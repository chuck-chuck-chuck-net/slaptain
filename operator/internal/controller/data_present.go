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

// podPresence is one RW pod's answer to "is the suffix's root entry visible to
// an ordinary base search?". present and glued are only meaningful when
// assessed is true.
type podPresence struct {
	pod string
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
// Pure observability (ADR-012: never drives reconciler action). Two rules, each
// bought with an incident:
//
//   - Every assessed pod must show the root entry (ADR-025). A suffix demoted
//     to a hidden GLUE entry is invisible to ordinary searches on exactly one
//     pod while its peers look healthy, so an any-pod-visible verdict reads
//     True over a silently broken pod — the blindness the 2026-09-13 incident
//     rode on. A ManageDsaIT-confirmed glue is reported as such: it is positive
//     evidence of corruption on that pod and stands alone, needing neither a
//     healthy peer to contrast with nor any knowledge of the database's past.
//   - An all-empty reading means different things on different databases, and
//     knownPopulated is what separates them (2026-09-14). A database that was
//     seeded, restored, or has been seen holding data is missing its data:
//     alert. A database that has never held data — a non-founder site of a mesh
//     waiting for its first refresh (ADR-025 D1 tells peers to omit spec.seed),
//     a migration cluster waiting for the legacy provider (ADR-011) — is simply
//     not there yet, and must not be reported as loss. Unknown falls to
//     Unknown; no timer is involved, because the thing that makes emptiness
//     alarming is evidence that data once existed, not elapsed time.
func aggregateDataPresent(suffix string, results []podPresence, knownPopulated bool) (metav1.ConditionStatus, string, string) {
	var present, missing, unreached, glued []string
	for _, r := range results {
		switch {
		case !r.assessed:
			unreached = append(unreached, r.pod)
		case r.glued:
			glued = append(glued, r.pod)
		case r.present:
			present = append(present, r.pod)
		default:
			missing = append(missing, r.pod)
		}
	}

	uncheckedNote := ""
	if len(unreached) > 0 {
		uncheckedNote = fmt.Sprintf(" (unreachable, not assessed: %s)", strings.Join(unreached, ", "))
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
			"could not reach any RW pod to verify data presence"
	case len(missing) == 0:
		return metav1.ConditionTrue, "RootEntryVisible",
			fmt.Sprintf("root entry %s visible on all %d reached RW pod(s)%s",
				suffix, len(present), uncheckedNote)
	case len(present) == 0 && !knownPopulated:
		return metav1.ConditionUnknown, "NoDataYet",
			fmt.Sprintf("root entry %s is not present on any reached RW pod, and this database has "+
				"never been seeded, restored or observed holding data — it is waiting for its first "+
				"data (replication from a founder site or a legacy provider). Not an alert: nothing "+
				"is known to have been lost%s", suffix, uncheckedNote)
	case len(present) == 0:
		return metav1.ConditionFalse, "DataMissing",
			fmt.Sprintf("root entry %s not visible on any reachable RW pod — possible data loss; "+
				"this condition is informational and does not trigger operator action (see ADR-012)%s",
				suffix, uncheckedNote)
	default:
		return metav1.ConditionFalse, "DataMissingOnPods",
			fmt.Sprintf("root entry %s hidden or absent on pod(s) %s while visible on %s — "+
				"likely a hidden glue suffix entry from a multi-site seed race (ADR-025); "+
				"verify with a ManageDSAIT base search; informational only (ADR-012)%s",
				suffix, strings.Join(missing, ", "), strings.Join(present, ", "), uncheckedNote)
	}
}
