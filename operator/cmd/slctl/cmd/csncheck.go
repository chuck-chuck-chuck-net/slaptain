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
	"time"
)

// csnPodReading is one pod's contextCSN evidence, keyed by data suffix.
type csnPodReading struct {
	// Pod is the pod the readings were taken from.
	Pod string
	// BySuffix maps every DATA suffix the pod serves to that database's
	// contextCSN vector. A key present with an empty vector means the pod
	// serves the database but no contextCSN could be read from it.
	BySuffix map[string][]string
}

// checkCSNConvergence judges whether the pods agree on contextCSN, PER
// DATABASE.
//
// A contextCSN vector belongs to one database on one pod: two databases have
// different vectors by construction (independent write histories, disjoint
// serverID activity, last writes at unrelated times), so the only meaningful
// comparison is one suffix across pods. This check used to read whichever
// suffix came first out of namingContexts and report its verdict as the
// cluster's — on the standard two-database fixture it covered db1 and was
// blind to db2. Same axis error the operator's ReplicationConverged carried
// until 2026-09-14 (ADR-008 amendment); the CLI is the tool a human reaches
// for to confirm or deny that condition, so it must judge on the same axis.
//
// The cluster verdict is the worst per-database verdict, and every non-pass
// segment names its database:
//
//	fail — a database is readable on some pods and not on others. Unreadable
//	       evidence never counts toward the good verdict, and the pods it
//	       could not be read from are named.
//	warn — a database's pods disagree (a real lag, or a transient one under
//	       write traffic — hence warn, not fail), or nothing was readable at
//	       all (legitimate on a fresh, empty database).
//	pass — every database's pods agree and nothing was unreadable.
//
// What this can NOT see is bounded by ADR-008's amendments: CSN equality on an
// idle database says everything replicable has replicated, not that the link
// works.
func checkCSNConvergence(readings []csnPodReading) checkResult {
	suffixes := csnSuffixes(readings)
	var fails, warns []string
	converged, readablePods := 0, 0

	for _, suffix := range suffixes {
		var sets []string
		podsBySet := map[string][]string{}
		var missing []string
		podNewest := map[string]time.Time{}

		for _, r := range readings {
			csns, serves := r.BySuffix[suffix]
			if !serves {
				continue // this pod does not hold this database
			}
			if len(csns) == 0 {
				missing = append(missing, r.Pod)
				continue
			}
			normalized := normalizeCSN(csns)
			sets = append(sets, normalized)
			podsBySet[normalized] = append(podsBySet[normalized], r.Pod)
			var newest time.Time
			for _, csn := range csns {
				if t, err := parseCSNTime(csn); err == nil && t.After(newest) {
					newest = t
				}
			}
			if !newest.IsZero() {
				podNewest[r.Pod] = newest
			}
		}

		if len(sets) > readablePods {
			readablePods = len(sets)
		}

		switch {
		case len(sets) == 0:
			// Nobody reports one: legitimate for a fresh or empty database.
			warns = append(warns, fmt.Sprintf("%s: no contextCSN on any pod", suffix))
		case len(missing) > 0:
			// Some pods report, some do not. The data exists and did not
			// reach the silent pods — but WHY is not knowable from here, and
			// the old wording ("never synced?") asserted one cause. A hidden
			// glue suffix entry takes contextCSN with it (ADR-025), and this
			// probe is anonymous, so an ACL can deny it too.
			sort.Strings(missing)
			fails = append(fails, fmt.Sprintf(
				"%s: no contextCSN on %d of %d pods (%s) while the others report one — "+
					"an initial sync that never completed, a hidden glue suffix entry (ADR-025), "+
					"or an ACL denying this anonymous read",
				suffix, len(missing), len(missing)+len(sets), strings.Join(missing, ", ")))
		case len(uniqueStrings(sets)) == 1:
			converged++
		default:
			warns = append(warns, describeCSNDivergence(suffix, uniqueStrings(sets), podsBySet, podNewest))
		}
	}

	switch {
	case len(suffixes) == 0:
		return checkResult{Name: "csn-convergence", Status: "warn", Detail: "no contextCSN data available"}
	case len(fails) > 0:
		return checkResult{Name: "csn-convergence", Status: "fail",
			Detail: strings.Join(append(fails, warns...), "; ")}
	case len(warns) > 0:
		return checkResult{Name: "csn-convergence", Status: "warn", Detail: strings.Join(warns, "; ")}
	default:
		return checkResult{Name: "csn-convergence", Status: "pass", Detail: fmt.Sprintf(
			"%d database(s): all %d pods report identical contextCSN for each",
			converged, readablePods)}
	}
}

// describeCSNDivergence renders one database's disagreement: which pods hold
// which vector, and how far the oldest is behind the newest. Both the lag and
// the grouping are computed WITHIN the suffix — a lag taken across databases
// is the age gap between two unrelated write histories, not replication lag.
func describeCSNDivergence(
	suffix string,
	unique []string,
	podsBySet map[string][]string,
	podNewest map[string]time.Time,
) string {
	var newest, oldest time.Time
	var newestPod, oldestPod string
	for _, pod := range sortedKeys(podNewest) {
		t := podNewest[pod]
		if newest.IsZero() || t.After(newest) {
			newest, newestPod = t, pod
		}
		if oldest.IsZero() || t.Before(oldest) {
			oldest, oldestPod = t, pod
		}
	}
	var groups []string
	for _, set := range unique {
		groups = append(groups, fmt.Sprintf("[%s]", strings.Join(podsBySet[set], ",")))
	}
	sort.Strings(groups)

	lag := ""
	if newestPod != "" && oldestPod != newestPod {
		lag = fmt.Sprintf(" (%s behind %s by %s)", oldestPod, newestPod, formatDuration(newest.Sub(oldest)))
	}
	return fmt.Sprintf("%s: %d distinct CSN vectors%s: %s",
		suffix, len(unique), lag, strings.Join(groups, " vs "))
}

// csnSuffixes is every data suffix any pod reported, sorted, so the verdict
// covers all of them in a deterministic order.
func csnSuffixes(readings []csnPodReading) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, r := range readings {
		for suffix := range r.BySuffix {
			if _, dup := seen[suffix]; dup {
				continue
			}
			seen[suffix] = struct{}{}
			out = append(out, suffix)
		}
	}
	sort.Strings(out)
	return out
}
