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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// csnReading is one contextCSN query result: what was asked (which pod or
// remote address, which database suffix) and what came back — the CSN vector,
// or the error that prevented reading it. A failed read is a reading too: it
// carries "we could not see this", which is evidence in its own right and must
// never silently vanish from a verdict.
type csnReading struct {
	// Pod is the pod (local) or remote address the query went to.
	Pod string
	// Suffix is the database suffix the contextCSN belongs to. CSN vectors are
	// only comparable within one suffix.
	Suffix string
	// CSNs is the raw contextCSN vector; empty when the database has none.
	CSNs []string
	// Err is non-empty when the query failed. CSNs is then meaningless.
	Err string
}

// localVerdict is the outcome of comparing local per-pod contextCSN readings.
type localVerdict struct {
	Status  metav1.ConditionStatus
	Reason  string
	Message string
	// NewestBySuffix carries the newest CSN timestamp observed per database
	// suffix, the baseline cross-site comparison uses.
	NewestBySuffix map[string]time.Time
	// Silent suppresses the condition write entirely (too little data to say
	// anything at all).
	Silent bool
}

// evaluateLocalConvergence judges whether the local RW pods agree on their
// contextCSN, PER DATABASE.
//
// A contextCSN vector is a property of one database on one pod, and two
// databases have different vectors by construction: independent write
// histories, disjoint serverID activity, last writes at unrelated times. So
// the only meaningful comparison is one suffix across pods; comparing db1's
// vector against db2's asks a question with no true answer, and on a
// multi-database cluster it answered "diverged" forever, reporting a "lag"
// that was really the age gap between two databases' last writes.
//
// The cluster verdict is the AND over databases, with three outcomes and a
// deliberate severity order:
//
//	False   — at least one database's pods disagree. Observed problems win:
//	          divergence is reported even when other readings were unreadable.
//	Unknown — no divergence was observed, but at least one (pod, database)
//	          pair could not be read. Positive evidence only: an absence that
//	          could mean "could not read it" never counts toward True.
//	True    — every database's readable pods agree and nothing was unreadable.
//
// What this can NOT see is unchanged and bounded by ADR-008's amendments: CSN
// equality on an idle database says everything replicable has replicated, not
// that the link works.
func evaluateLocalConvergence(readings []csnReading, replicas int32) localVerdict {
	v := localVerdict{NewestBySuffix: map[string]time.Time{}}
	if len(readings) == 0 {
		v.Silent = true
		return v
	}

	suffixes := sortedSuffixes(readings)
	setsBySuffix := map[string][][]string{}
	oldestBySuffix := map[string]time.Time{}
	var unreadable []string

	for _, r := range readings {
		if r.Err != "" {
			unreadable = append(unreadable, fmt.Sprintf("%s/%s", r.Pod, r.Suffix))
			continue
		}
		setsBySuffix[r.Suffix] = append(setsBySuffix[r.Suffix], r.CSNs)
		t, _, err := newestCSN(r.CSNs)
		if err != nil {
			continue
		}
		if t.After(v.NewestBySuffix[r.Suffix]) {
			v.NewestBySuffix[r.Suffix] = t
		}
		if o, seen := oldestBySuffix[r.Suffix]; !seen || t.Before(o) {
			oldestBySuffix[r.Suffix] = t
		}
	}

	var diverged []string
	for _, suffix := range suffixes {
		sets := setsBySuffix[suffix]
		if len(sets) < 2 || csnConverged(sets) {
			continue
		}
		lag := v.NewestBySuffix[suffix].Sub(oldestBySuffix[suffix])
		diverged = append(diverged, fmt.Sprintf("%s (%.1fs lag)", suffix, lag.Seconds()))
	}

	switch {
	case len(diverged) > 0:
		msg := fmt.Sprintf("CSN divergence on %d of %d databases across %d pods: %s",
			len(diverged), len(suffixes), replicas, strings.Join(diverged, "; "))
		if len(unreadable) > 0 {
			msg += fmt.Sprintf(" (%d further (pod, database) pairs unreadable: %s)",
				len(unreadable), summarizeList(unreadable, 4))
		}
		v.Status = metav1.ConditionFalse
		v.Reason = "CSNsDiverged"
		v.Message = msg
	case len(unreadable) > 0:
		v.Status = metav1.ConditionUnknown
		v.Reason = "CSNQueriesIncomplete"
		v.Message = fmt.Sprintf(
			"could not read contextCSN for %d of %d (pod, database) pairs: %s — "+
				"no divergence observed on what could be read, which is not evidence of convergence",
			len(unreadable), len(readings), summarizeList(unreadable, 4))
	default:
		v.Status = metav1.ConditionTrue
		v.Reason = "CSNsMatch"
		v.Message = fmt.Sprintf("all %d pods report identical contextCSN for each of %d databases",
			replicas, len(suffixes))
	}
	return v
}

// peerVerdict is the outcome of comparing one external peer's contextCSN
// readings against our own.
type peerVerdict struct {
	State      ldapv1alpha1.ExternalPeerReplicationState
	LagSeconds string
	LastError  string
}

// evaluatePeerConvergence judges one external peer from the readings taken
// against it, PER DATABASE.
//
// A database is *verified* when at least one of the peer's addresses answered
// for it; its lag is then our newest CSN for that same suffix minus the peer's
// newest for it. Comparing across suffixes is meaningless and actively
// misleading: two healthy databases whose last writes are minutes apart would
// read as minutes of "lag".
//
// Severity order — observed problems win over missing evidence:
//
//	Unreachable       — nothing answered for any database.
//	Lagging           — a VERIFIED database is behind by more than threshold.
//	PartiallyVerified — every verified database is current, but at least one
//	                    database yielded no readable evidence (named in
//	                    LastError). Never Synced: a verdict computed from
//	                    whichever database answered is not the peer's health.
//	Synced            — every database was read and every one is current.
//
// ADR-008's bound is unchanged: this is consumer-side and one-directional, and
// on an idle database equality is not proof the link works.
func evaluatePeerConvergence(
	localNewest map[string]time.Time,
	readings []csnReading,
	threshold time.Duration,
) peerVerdict {
	suffixes := sortedSuffixes(readings)
	remoteNewest := map[string]time.Time{}
	verified := map[string]bool{}

	for _, r := range readings {
		if r.Err != "" {
			continue
		}
		verified[r.Suffix] = true
		if t, _, err := newestCSN(r.CSNs); err == nil && t.After(remoteNewest[r.Suffix]) {
			remoteNewest[r.Suffix] = t
		}
	}

	if len(verified) == 0 {
		return peerVerdict{
			State:     ldapv1alpha1.ReplicationUnreachable,
			LastError: "all remote pod CSN queries failed",
		}
	}

	var unverified []string
	var maxLag time.Duration
	lagging := false
	for _, suffix := range suffixes {
		if !verified[suffix] {
			unverified = append(unverified, suffix)
			continue
		}
		remote := remoteNewest[suffix]
		if remote.IsZero() {
			// The peer answered but has no CSN for this database. Behind us
			// only if we have one; otherwise both are empty and there is
			// nothing to compare.
			if !localNewest[suffix].IsZero() {
				lagging = true
			}
			continue
		}
		lag := localNewest[suffix].Sub(remote)
		if lag < 0 {
			lag = 0 // remote is ahead — clocks, or it received a write first
		}
		if lag > threshold {
			lagging = true
			if lag > maxLag {
				maxLag = lag
			}
		}
	}

	var lastError string
	if len(unverified) > 0 {
		lastError = fmt.Sprintf("no readable contextCSN for %d of %d databases: %s",
			len(unverified), len(suffixes), summarizeList(unverified, 4))
	}

	switch {
	case lagging:
		v := peerVerdict{State: ldapv1alpha1.ReplicationLagging, LastError: lastError}
		if maxLag > 0 {
			v.LagSeconds = fmt.Sprintf("%.1f", maxLag.Seconds())
		}
		return v
	case len(unverified) > 0:
		return peerVerdict{State: ldapv1alpha1.ReplicationPartiallyVerified, LastError: lastError}
	default:
		return peerVerdict{State: ldapv1alpha1.ReplicationSynced}
	}
}

// summarizeList renders at most max items, noting how many were elided, so a
// large cluster cannot produce an unbounded condition message.
func summarizeList(items []string, max int) string {
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s, and %d more", strings.Join(items[:max], ", "), len(items)-max)
}

// sortedSuffixes returns the distinct suffixes of a reading set, sorted, so
// messages are deterministic.
func sortedSuffixes(readings []csnReading) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, r := range readings {
		if _, dup := seen[r.Suffix]; dup {
			continue
		}
		seen[r.Suffix] = struct{}{}
		out = append(out, r.Suffix)
	}
	sort.Strings(out)
	return out
}
