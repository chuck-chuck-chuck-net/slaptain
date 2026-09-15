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

// How often a HEALTHY SlapdDatabase reconciles with nothing prompting it.
//
// Found by running ADR-027's rotation on a live cluster rather than by reading
// the code (t3e, 2026-09-15): the Secret's replication-password was changed and
// nothing happened at all. Last reconcile 12:34:45, Secret rewritten ~12:38,
// and at 12:40:03 the node-local identity still held the old password with the
// database phase reading Running.
//
// The mechanism is a gap ADR-002's 2026-08-26 amendment already named — per-pod
// convergence is event-driven and "its only periodic trigger is incidental".
// SetupWithManager watches SlapdDatabase and SlapdCluster and nothing else, and
// the healthy path returned a bare ctrl.Result{}. A change confined to a Secret
// produces neither event: no object the controller watches changed, and no
// timer was pending. The incidental trigger (the SlapdCluster's 60s
// `lastChecked` status churn) only fires on a cluster that has external peers
// to report on, so a single-site cluster has no periodic resync whatsoever.
//
// ADR-027 promotes that from latent to load-bearing. The Secret is now the
// declared single source of truth for the replication identity, and "converged
// on every reconcile" means nothing if no reconcile is ever scheduled. The
// convergence code was correct; it was simply never called.
//
// Five minutes is chosen as the worst-case delay between rotating a Secret and
// the pods accepting the new credential — long enough that the per-pod LDAP
// sweep stays negligible, short enough that rotation is an operation rather
// than an outage. Anything that actually changes still reconciles immediately
// on its own event; this is only the floor under the ones that produce none.
//
// A Secret watch would make it immediate, and is a fair alternative rather than
// an unworkable one: the obvious objection (caching every Secret in every
// watched namespace to observe one key per database) applies to a naive watch,
// not to a label- or field-scoped cache. That buys a selector the operator then
// has to own and keep correct on every Secret it cares about, against four
// lines here. Revisit if the five-minute worst case ever becomes the thing
// standing between an operator and a credential rotation.
const databaseResyncInterval = 5 * time.Minute

// databaseRequeueAfter is how long the SlapdDatabase reconcile asks to wait
// before the next pass, given the phase it just concluded with.
//
// Unhealthy phases keep the tight 10s retry they have always had — there is
// work outstanding and it should land as soon as the obstacle clears. Healthy
// gets the slow resync above; it is not a retry but a floor, so that state the
// controller cannot observe through a watch (a Secret) is still eventually
// picked up.
func databaseRequeueAfter(phase ldapv1alpha1.SlapdDatabasePhase) time.Duration {
	if phase != ldapv1alpha1.DatabasePhaseRunning {
		return 10 * time.Second
	}
	return databaseResyncInterval
}
