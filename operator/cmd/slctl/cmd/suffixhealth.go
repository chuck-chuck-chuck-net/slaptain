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

package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/chuck-chuck-chuck-net/slaptain/operator/internal/suffixprobe"
)

// Suffix-entry health checks (ADR-025). A multi-site seed race can demote one
// pod's suffix entry to a GLUE entry — hidden from ordinary searches on exactly
// that pod, invisible in status.phase, and fatal to any backup taken from it
// (the artifact fails restore preflight). The signature is per-pod
// disagreement: an ordinary base search returns the entry on healthy pods and
// nothing on the glued one, and a ManageDSAIT base search (RFC 3296) reveals
// the glue with objectClass top+glue and a DIFFERENT entryUUID than the peers'.

// suffixObservation is one pod's view of one data suffix's base entry. The
// probe and its classification are shared with the operator
// (internal/suffixprobe): the SlapdDatabase controller's DataPresent condition
// and the SlapdBackup controller's SourceSuffixHealthy condition ask the same
// question of the same DN, and three copies of "what is a glue" would be three
// chances to disagree.
type suffixObservation = suffixprobe.Observation

// checkSuffixVisibility verifies that every data suffix's base entry is
// visible to an ordinary base search on every queryable pod (RW and RO — a
// consumer initial-syncing from a glued provider replicates the glue). A
// positively identified glue always fails; per-pod disagreement fails; a
// suffix invisible on EVERY pod warns (anonymous ACLs can legitimately hide
// it from this probe, and uniform invisibility across pods is not the
// divergence signature).
func checkSuffixVisibility(dbs []dbIdentity, pods []podState) checkResult {
	var fails, warns []string
	assessed := 0

	for _, db := range dbs {
		var visible, hidden, glued, unassessed []string
		for _, ps := range pods {
			if ps.err != "" {
				continue // unreachable pods are pod-readiness's problem
			}
			for _, o := range ps.suffixEntries {
				if !strings.EqualFold(o.Suffix, db.Suffix) {
					continue
				}
				switch o.Outcome {
				case suffixprobe.OutcomeGlue:
					glued = append(glued, ps.name)
				case suffixprobe.OutcomeVisible:
					visible = append(visible, ps.name)
				case suffixprobe.OutcomeMissing:
					hidden = append(hidden, ps.name)
				default:
					// The probe could not reach a verdict (denied read, search
					// error, an entry hidden without being glue). Not evidence
					// in either direction — leave this pod out of the tally
					// rather than let it read as "hidden" and manufacture a
					// per-pod divergence out of a failed read.
					unassessed = append(unassessed, ps.name)
				}
			}
		}
		if len(visible)+len(hidden)+len(glued) == 0 {
			if len(unassessed) > 0 {
				warns = append(warns, fmt.Sprintf(
					"%s: base entry could not be assessed on any pod (%s) — cannot assess",
					db.Suffix, strings.Join(unassessed, ", ")))
			}
			continue // suffix not probed on any pod
		}
		assessed++

		switch {
		case len(glued) > 0:
			fails = append(fails, fmt.Sprintf(
				"%s: hidden glue suffix entry on %s (ManageDSAIT-verified; ADR-025 — "+
					"base searches return nothing, backups from these pods are unrestorable)",
				db.Suffix, strings.Join(glued, ", ")))
		case len(hidden) > 0 && len(visible) > 0:
			fails = append(fails, fmt.Sprintf(
				"%s: base entry visible on %s but hidden on %s — per-pod divergence (ADR-025)",
				db.Suffix, strings.Join(visible, ", "), strings.Join(hidden, ", ")))
		case len(visible) == 0:
			warns = append(warns, fmt.Sprintf(
				"%s: base entry not readable on any pod with this probe (anonymous ACL?) — cannot assess",
				db.Suffix))
		}
	}

	switch {
	case len(fails) > 0:
		return checkResult{Name: "suffix-visibility", Status: "fail", Detail: strings.Join(fails, "; ")}
	case len(warns) > 0:
		return checkResult{Name: "suffix-visibility", Status: "warn", Detail: strings.Join(warns, "; ")}
	default:
		return checkResult{Name: "suffix-visibility", Status: "pass",
			Detail: fmt.Sprintf("%d database suffix(es): base entry visible on every queried pod", assessed)}
	}
}

// checkSuffixUUIDAgreement verifies that every pod reports the SAME entryUUID
// for each data suffix's base entry. entryUUID is syncrepl's identity: a
// disagreement means the pods hold DIFFERENT entries at the same DN — the
// multi-site seed race's conflict residue (ADR-025) — which CSN-based
// convergence checks are structurally blind to (the losing pod's glue carries
// the winner's entryCSN).
func checkSuffixUUIDAgreement(dbs []dbIdentity, pods []podState) checkResult {
	var fails, warns []string
	agreed := 0

	for _, db := range dbs {
		byUUID := map[string][]string{} // uuid → pods
		probedAny := false
		for _, ps := range pods {
			if ps.err != "" {
				continue
			}
			for _, o := range ps.suffixEntries {
				if !strings.EqualFold(o.Suffix, db.Suffix) {
					continue
				}
				probedAny = true
				if o.UUID != "" {
					byUUID[o.UUID] = append(byUUID[o.UUID], ps.name)
				}
			}
		}
		switch {
		case len(byUUID) > 1:
			uuids := make([]string, 0, len(byUUID))
			for u := range byUUID {
				uuids = append(uuids, u)
			}
			sort.Strings(uuids)
			var parts []string
			for _, u := range uuids {
				parts = append(parts, fmt.Sprintf("%s on %s", u, strings.Join(byUUID[u], ", ")))
			}
			fails = append(fails, fmt.Sprintf(
				"%s: pods hold DIFFERENT entries at the suffix DN (%s) — seed-race conflict residue, "+
					"invisible to CSN comparison (ADR-025)", db.Suffix, strings.Join(parts, "; ")))
		case len(byUUID) == 0 && probedAny:
			warns = append(warns, fmt.Sprintf(
				"%s: entryUUID not readable on any pod — cannot assess agreement", db.Suffix))
		case len(byUUID) == 1:
			agreed++
		}
	}

	switch {
	case len(fails) > 0:
		return checkResult{Name: "suffix-uuid-agreement", Status: "fail", Detail: strings.Join(fails, "; ")}
	case len(warns) > 0:
		return checkResult{Name: "suffix-uuid-agreement", Status: "warn", Detail: strings.Join(warns, "; ")}
	default:
		return checkResult{Name: "suffix-uuid-agreement", Status: "pass",
			Detail: fmt.Sprintf("%d database suffix(es): all pods agree on the base entry's entryUUID", agreed)}
	}
}
