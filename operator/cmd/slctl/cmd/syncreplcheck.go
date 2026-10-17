/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package cmd

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The syncrepl checks, judged PER DATABASE.
//
// Same axis error ADR-008's 2026-09-14 amendment corrected for contextCSN, in
// the checks that were left behind: `olcSyncRepl` and `olcMultiProvider` live
// on ONE `olcDatabase={N}mdb` entry, so reading whichever data database came
// first and reporting its verdict as the pod's covered db1 and was blind to
// db2. On the standard two-database fixture that is half the cluster, reported
// as all of it.
//
// The blind spot is worst for rid-uniqueness, whose entire purpose is a
// collision BETWEEN databases: ADR-031 gives each database its own `ridSegment`
// precisely so their RIDs cannot overlap, and a check that reads one database
// is structurally incapable of finding an overlap. It said "all RIDs unique
// within each pod" while having verified one database's worth.

// syncreplDBReading is one database's syncrepl-relevant cn=config state on one
// pod.
type syncreplDBReading struct {
	// Stanzas are the raw olcSyncRepl values (credentials already redacted).
	Stanzas []string
	// MultiProvider is olcMultiProvider, "" when the attribute is absent.
	MultiProvider string
}

// syncreplPodReading is one pod's readings, keyed by DATA suffix. A suffix
// absent from the map is a database this pod does not serve at all — which for
// a database the cluster declares is itself the finding.
type syncreplPodReading struct {
	Pod      string
	RO       bool
	BySuffix map[string]syncreplDBReading
}

// checkSyncreplStanzaCount verifies every replicated database on every pod
// carries the expected number of stanzas.
//
// Scoped to databases the cluster declares replicated: a non-replicated
// database legitimately carries none, and holding it to the replicated count
// would turn a supported configuration into a permanent failure.
func checkSyncreplStanzaCount(dbs []dbIdentity, readings []syncreplPodReading, expectedRW, expectedRO int) checkResult {
	if len(readings) == 0 {
		return notEvaluated("syncrepl-stanza-count")
	}
	var issues []string
	for _, r := range readings {
		want := expectedRW
		if r.RO {
			want = expectedRO
		}
		for _, db := range replicatedDBs(dbs) {
			got := len(r.BySuffix[db.Suffix].Stanzas)
			if got != want {
				issues = append(issues,
					fmt.Sprintf("%s/%s: %d stanzas (want %d)", r.Pod, db.Suffix, got, want))
			}
		}
	}
	if len(issues) > 0 {
		return checkResult{Name: "syncrepl-stanza-count", Status: checkFail, Detail: strings.Join(issues, "; ")}
	}
	return checkResult{
		Name:   "syncrepl-stanza-count",
		Status: checkPass,
		Detail: fmt.Sprintf("%d replicated database(s): RW %d stanzas each, RO %d each",
			len(replicatedDBs(dbs)), expectedRW, expectedRO),
	}
}

// checkSyncreplSkipSelf verifies no RW pod consumes from itself, on any
// database. A self-stanza is harmless in slapd but always a generation bug.
func checkSyncreplSkipSelf(readings []syncreplPodReading, headlessSvc string) checkResult {
	if len(readings) == 0 {
		return notEvaluated("syncrepl-skip-self")
	}
	var issues []string
	for _, r := range readings {
		if r.RO {
			continue
		}
		self := fmt.Sprintf("%s.%s.", r.Pod, headlessSvc)
		for _, suffix := range sortedSuffixes(r.BySuffix) {
			for _, s := range r.BySuffix[suffix].Stanzas {
				if strings.Contains(s, self) {
					issues = append(issues,
						fmt.Sprintf("%s replicates from itself on %s", r.Pod, suffix))
					break
				}
			}
		}
	}
	if len(issues) > 0 {
		return checkResult{Name: "syncrepl-skip-self", Status: checkFail, Detail: strings.Join(issues, "; ")}
	}
	return checkResult{Name: "syncrepl-skip-self", Status: checkPass,
		Detail: "no RW pod replicates from itself on any database"}
}

var ridPattern = regexp.MustCompile(`rid=(\d+)`)

// checkRIDUniqueness verifies a pod's RIDs are unique ACROSS its databases.
//
// The cross-database part is the point. A RID identifies a consumer connection
// within a slapd instance, not within a database, so two databases sharing a
// ridSegment (ADR-031 requires distinct ones; the operator and the charts both
// enforce it — see
// docs/BACKLOG.md) collide on the same server. The first-database-only version
// of this check could never have seen that.
func checkRIDUniqueness(readings []syncreplPodReading) checkResult {
	if len(readings) == 0 {
		return notEvaluated("rid-uniqueness")
	}
	var issues []string
	for _, r := range readings {
		owner := map[string]string{} // rid -> the suffix that claimed it first
		for _, suffix := range sortedSuffixes(r.BySuffix) {
			for _, s := range r.BySuffix[suffix].Stanzas {
				m := ridPattern.FindStringSubmatch(s)
				if len(m) < 2 {
					continue
				}
				rid := m[1]
				if prev, dup := owner[rid]; dup {
					where := suffix
					if prev != suffix {
						where = prev + " and " + suffix
					}
					issues = append(issues, fmt.Sprintf("%s: RID %s used twice (%s)", r.Pod, rid, where))
					continue
				}
				owner[rid] = suffix
			}
		}
	}
	if len(issues) > 0 {
		return checkResult{Name: "rid-uniqueness", Status: checkFail, Detail: strings.Join(issues, "; ")}
	}
	return checkResult{Name: "rid-uniqueness", Status: checkPass,
		Detail: "all RIDs unique within each pod, across its databases"}
}

// checkMultiProviderPerDB verifies olcMultiProvider is TRUE on every
// replicated database of every RW pod, and set on none of an RO pod's.
//
// Per database because the attribute is per database: a cluster can carry one
// database that accepts writes on every pod and another that silently does not.
func checkMultiProviderPerDB(dbs []dbIdentity, readings []syncreplPodReading) checkResult {
	if len(readings) == 0 {
		return notEvaluated("multi-provider")
	}
	var issues []string
	for _, r := range readings {
		if r.RO {
			for _, suffix := range sortedSuffixes(r.BySuffix) {
				if mp := r.BySuffix[suffix].MultiProvider; mp != "" && !strings.EqualFold(mp, "FALSE") {
					issues = append(issues, fmt.Sprintf("%s/%s: RO pod has multiProvider=%q", r.Pod, suffix, mp))
				}
			}
			continue
		}
		for _, db := range replicatedDBs(dbs) {
			if mp := r.BySuffix[db.Suffix].MultiProvider; !strings.EqualFold(mp, "TRUE") {
				issues = append(issues, fmt.Sprintf("%s/%s: multiProvider=%q (want TRUE)", r.Pod, db.Suffix, mp))
			}
		}
	}
	if len(issues) > 0 {
		return checkResult{Name: "multi-provider", Status: checkFail, Detail: strings.Join(issues, "; ")}
	}
	return checkResult{Name: "multi-provider", Status: checkPass,
		Detail: "TRUE on every replicated database of every RW pod, absent on RO"}
}

func replicatedDBs(dbs []dbIdentity) []dbIdentity {
	var out []dbIdentity
	for _, db := range dbs {
		if db.Replicated {
			out = append(out, db)
		}
	}
	return out
}

// sortedSuffixes keeps the output stable, so a diff between two runs is a
// change in the cluster rather than in map iteration order.
func sortedSuffixes(m map[string]syncreplDBReading) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ── Unreadable cn=config ─────────────────────────────────────────────────────
//
// Every check above reads cn=config. When the bind fails there is no evidence,
// and a check with no evidence must say so rather than render a verdict:
// "0 stanzas (want 4)" is a claim about the cluster's configuration, and it is
// false — nobody looked.
//
// Live case, 2026-09-28: a reinstall over surviving PVCs (docs/BACKLOG.md) left
// the operator's newly generated config password out of step with the olcRootPW
// already on disk. Every bind returned err=49, and slctl answered with eight
// confident FAIL lines about missing syncrepl stanzas on a mesh that was
// replicating perfectly well — sending the reader to their values file while
// the cause sat three lines above in the same output.

// configReadFailure is one pod whose cn=config could not be read.
type configReadFailure struct {
	Pod string
	Err string
}

// checkConfigReadable promotes the bind failure from a per-pod line in the
// display to a check, so it reaches the exit code and appears among the things
// a reader scans, ahead of the checks it invalidates.
func checkConfigReadable(failures []configReadFailure) checkResult {
	if len(failures) == 0 {
		return checkResult{Name: "cn-config-readable", Status: checkPass, Detail: "cn=config readable on every queried pod"}
	}
	parts := make([]string, 0, len(failures))
	for _, f := range failures {
		parts = append(parts, fmt.Sprintf("%s: %s", f.Pod, f.Err))
	}
	sort.Strings(parts)
	return checkResult{
		Name:   "cn-config-readable",
		Status: checkFail,
		Detail: fmt.Sprintf("%d pod(s) refused the cn=config bind, so every check below that reads it "+
			"was NOT evaluated: %s", len(failures), strings.Join(parts, "; ")),
	}
}

// notEvaluated is what a cn=config check returns when no pod yielded readable
// cn=config. Deliberately not "pass": a vacuous green is how a broken cluster
// reports clean, and it is the failure mode ADR-008's amendment named for
// contextCSN. Deliberately not "fail" either — the failure is the bind, which
// cn-config-readable already reports, and repeating it once per dependent check
// buries it.
func notEvaluated(name string) checkResult {
	return checkResult{
		Name:   name,
		Status: checkWarn,
		Detail: "not evaluated: no pod's cn=config could be read (see cn-config-readable)",
	}
}
