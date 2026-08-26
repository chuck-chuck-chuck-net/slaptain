/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"time"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// Peer address discovery runs on its own cadence, decoupled from CSN monitoring.
//
// Why: under ADR-016 pod-routed transport a peer's syncrepl stanzas name our pod
// IPs, so replacing a pod invalidates them until that peer's operator
// rediscovers them. Detection was never the problem — discovery and the CSN
// check run in the same pass, and the check consumes the freshly discovered
// addresses. The problem is PARTIAL observation: discovery collects pods that
// are Running with a non-empty PodIP, and during a multi-pod restart the
// replacements come back one at a time. A tick landing mid-restart therefore
// writes a valid-looking *partial* address set and then waits a full
// csnCheckInterval before looking again. Measured end-to-end recovery on a
// three-site lab was 130-268 s, against a nominal 60 s.
//
// The fix is two-part and lives entirely in timing — nothing here changes what
// gets written:
//
//   - a discovery-only pass must be a status no-op (csnCheckDue), so running the
//     reconcile more often costs nothing in API writes or downstream fan-out;
//   - churn tightens the loop (peerAddressesUnsettled + discoveryRequeue), so the
//     sequential-return multiplier collapses instead of costing one full
//     interval per returning pod.
//
// Scope guard: only clusters with at least one externalPeers[].discovery peer
// take the new cadence. No external peers, podAddresses-only and uri-only
// clusters keep the exact code path and interval they have today, because their
// addresses come from the spec and cannot be discovered late.

// peerAddressesUnsettled reports whether this pass's discovered peer addresses
// differ from the last-applied ones — i.e. whether the cluster is still watching
// a peer's pods come and go.
//
// Compared by name-keyed SET equality, deliberately:
//
//   - by name, not by index, because status order follows spec order and a spec
//     reshuffle is not address churn;
//   - as a set, because discoverRemotePeerAddresses sorts its output, so a
//     different order is the same observation twice — treating that as churn
//     would pin the cluster at the fast interval forever;
//   - a peer appearing or disappearing counts, since that is a real change in
//     what the stanza builder will be handed.
//
// discoveryErrored is passed in rather than inferred from LastError, because
// that field conflates discovery errors with CSN-check errors. Inferring from it
// was a real bug: a peer whose last CSN check recorded an error has that error
// carried forward on every discovery-only pass, so the cluster would read as
// permanently unsettled and sit at the fast interval until the bound cut it off.
// The discovery branch is the only place that knows, so it says so.
//
// A peer with no discovered addresses on either side is a static podAddresses or
// uri peer. Its addresses live in the spec, so discovery cannot move them and it
// can never report churn.
//
// Same comparison shape as serverIDSetsEqual.
func peerAddressesUnsettled(prev, cur []ldapv1alpha1.ExternalPeerStatus, discoveryErrored bool) bool {
	if discoveryErrored {
		// The addresses we hold are not a confirmed observation; look again soon.
		// Bounded by discoveryRequeue, not here.
		return true
	}
	if len(cur) == 0 {
		// Nothing to discover (no peers, or the spec dropped them all). The
		// no-peers case never reaches the new cadence at all; returning false
		// keeps this total.
		return len(prev) != 0
	}

	prevByName := make(map[string][]string, len(prev))
	for _, p := range prev {
		prevByName[p.Name] = p.DiscoveredAddresses
	}
	if len(prev) != len(cur) {
		return true
	}

	for _, c := range cur {
		was, ok := prevByName[c.Name]
		if !ok {
			return true // a peer we had not observed before
		}
		if !addressSetsEqual(was, c.DiscoveredAddresses) {
			return true
		}
	}
	return false
}

// addressSetsEqual compares two address lists as sets.
func addressSetsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, s := range a {
		counts[s]++
	}
	for _, s := range b {
		counts[s]--
		if counts[s] < 0 {
			return false
		}
	}
	for _, n := range counts {
		if n != 0 {
			return false
		}
	}
	return true
}

// csnCheckDue reports whether the CSN monitoring interval has elapsed since the
// last check that actually ran.
//
// Derived from the PERSISTED LastChecked (the minimum across peers) rather than
// in-memory state, so it survives an operator restart, a leader-election
// handover, and a cache re-list: whichever replica reconciles next reads the
// same answer off the object. In-memory state would reset the clock on every
// restart, which on a crash-looping operator would restore exactly the per-pass
// LDAP fan-out this gating exists to remove.
//
// The minimum governs so that one stale peer pulls the check forward rather than
// a fresh one deferring it — and so a peer newly added to the spec (no
// LastChecked at all) forces a check instead of inheriting its siblings' clock.
//
// A LastChecked in the future (clock skew between operator nodes, or a status
// written by a differently-skewed replica) reads as due rather than wedging the
// check off until the clock catches up.
func csnCheckDue(prev []ldapv1alpha1.ExternalPeerStatus, now time.Time, interval time.Duration) bool {
	if len(prev) == 0 {
		return true
	}
	var oldest time.Time
	for _, p := range prev {
		if p.LastChecked == nil {
			return true // never checked
		}
		t := p.LastChecked.Time
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
	}
	if oldest.After(now) {
		return true // clock went backwards; do not wedge
	}
	return now.Sub(oldest) >= interval
}

// discoveryRequeue picks the next requeue interval for a cluster with discovery
// peers, and returns the updated consecutive-fast-pass counter.
//
// While the address set is still moving, requeue on the short settle interval so
// the sequential return of replacement pods is observed as it happens instead of
// once per base interval. Once it stops moving, drop back to base.
//
// The bound (maxFast) is about REMOTE API LOAD, not about churn: a peer whose
// discovery keeps failing, or whose address set genuinely flaps, reports the
// same status every pass, and an unchanged status is already an SSA no-op — so
// the fast phase costs no writes and no downstream fan-out. What it does cost is
// a remote Kubernetes API call per pass against someone else's cluster. The
// bound caps that, then falls back to base and stays there until things settle
// (which clears the budget, so the next real restart gets the fast path again).
//
// Pure so the ladder is table-testable; the caller owns the counter.
func discoveryRequeue(unsettled bool, fastPasses int, base, settle time.Duration, maxFast int) (time.Duration, int) {
	if !unsettled {
		return base, 0
	}
	if fastPasses >= maxFast {
		return base, fastPasses
	}
	return settle, fastPasses + 1
}

// clusterHasDiscoveryPeer reports whether any external peer resolves its
// addresses through the remote Kubernetes API (ADR-007 amendment / ADR-016).
// Only such a cluster takes the decoupled discovery cadence: a peer with static
// podAddresses or a uri carries its addresses in the spec, so there is nothing
// to discover late and no reason to look more often.
func clusterHasDiscoveryPeer(sc *ldapv1alpha1.SlapdCluster) bool {
	for i := range sc.Spec.Replication.ExternalPeers {
		if sc.Spec.Replication.ExternalPeers[i].Discovery != nil {
			return true
		}
	}
	return false
}

// carryForwardPeerCSNStatus copies the CSN-monitoring fields of the previously
// applied status for this peer onto a freshly built one, leaving the just
// discovered addresses in place.
//
// This is what makes a discovery-only pass a status no-op: the rebuilt status is
// byte-identical to the applied one whenever the addresses did not change, so
// server-side apply writes nothing, resourceVersion does not move, and the
// SlapdDatabase watch does not fan out.
//
// LastChecked is deliberately carried verbatim rather than refreshed — it means
// "when the CSN check last actually ran", and a discovery pass did not run one.
// csnCheckDue reads it back, so refreshing it here would also defer the real
// check forever.
//
// LastError is carried only when this pass did not produce one of its own. The
// field conflates discovery errors and CSN errors (pre-existing), so on a skipped
// pass the honest thing is: a fresh discovery error wins, otherwise keep
// whatever the last full check reported.
func carryForwardPeerCSNStatus(prev []ldapv1alpha1.ExternalPeerStatus, cur *ldapv1alpha1.ExternalPeerStatus) {
	for i := range prev {
		if prev[i].Name != cur.Name {
			continue
		}
		cur.ReplicationState = prev[i].ReplicationState
		cur.LagSeconds = prev[i].LagSeconds
		cur.LastChecked = prev[i].LastChecked
		if cur.LastError == "" {
			cur.LastError = prev[i].LastError
		}
		return
	}
}

// nextDiscoveryRequeue advances this cluster's tightened-pass budget and returns
// the interval to requeue on. Wraps the pure discoveryRequeue with the
// controller-local counter; see the SlapdClusterReconciler field comment for why
// the counter lives here rather than in status.
func (r *SlapdClusterReconciler) nextDiscoveryRequeue(key string, unsettled bool) time.Duration {
	r.discoveryFastMu.Lock()
	defer r.discoveryFastMu.Unlock()
	if r.discoveryFastPasses == nil {
		r.discoveryFastPasses = map[string]int{}
	}
	interval, next := discoveryRequeue(
		unsettled, r.discoveryFastPasses[key],
		peerDiscoveryInterval, peerDiscoverySettleInterval, peerDiscoveryMaxFastPasses,
	)
	if next == 0 {
		delete(r.discoveryFastPasses, key)
	} else {
		r.discoveryFastPasses[key] = next
	}
	return interval
}

// forgetDiscoveryFastPasses drops a deleted cluster's budget so the map cannot
// grow without bound across create/delete cycles.
func (r *SlapdClusterReconciler) forgetDiscoveryFastPasses(key string) {
	r.discoveryFastMu.Lock()
	defer r.discoveryFastMu.Unlock()
	delete(r.discoveryFastPasses, key)
}
