/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"testing"
	"time"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// A healthy SlapdDatabase must still come back on its own.
//
// Measured on the t3e lab, 2026-09-15, while validating ADR-027's rotation:
// the Secret's replication-password was changed and NOTHING happened. Last
// SlapdDatabase reconcile 12:34:45; Secret rewritten ~12:38; at 12:40:03 the
// node-local identity still held the previous password and the phase was
// Running. Mechanism: SetupWithManager watches SlapdDatabase and SlapdCluster
// and nothing else, and the Running path returned a bare ctrl.Result{}. So a
// change confined to a Secret is invisible — there is no event and no timer.
//
// ADR-002's 2026-08-26 amendment already recorded the shape of this ("its only
// periodic trigger is incidental… a non-replicated cluster has no periodic
// resync at all"). ADR-027 turns it from a latent gap into a correctness
// problem, because the Secret is now the declared source of truth for the
// replication identity and "converged on every reconcile" is worth nothing if
// no reconcile is ever scheduled.
func TestDatabaseRequeueAfter(t *testing.T) {
	cases := []struct {
		name  string
		phase ldapv1alpha1.SlapdDatabasePhase
		want  time.Duration
	}{
		// Unhealthy: the existing tight retry, unchanged.
		{"degraded retries fast", ldapv1alpha1.DatabasePhaseDegraded, 10 * time.Second},
		{"error retries fast", ldapv1alpha1.DatabasePhaseError, 10 * time.Second},
		// Healthy: a slow resync, so a Secret-only change is eventually seen.
		{"running resyncs slowly", ldapv1alpha1.DatabasePhaseRunning, 5 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := databaseRequeueAfter(tc.phase); got != tc.want {
				t.Errorf("databaseRequeueAfter(%q) = %v, want %v", tc.phase, got, tc.want)
			}
		})
	}
}

// The resync is the upper bound on how long a rotated Secret can go
// unnoticed, so it has to be short enough to be an operation rather than an
// outage — and long enough not to turn a per-pod LDAP sweep into a busy loop.
func TestDatabaseResyncIntervalIsSane(t *testing.T) {
	if databaseResyncInterval < time.Minute {
		t.Errorf("resync %v is a busy loop: every tick sweeps every pod over LDAP",
			databaseResyncInterval)
	}
	if databaseResyncInterval > 10*time.Minute {
		t.Errorf("resync %v is too long: it is the worst-case delay before a rotated "+
			"Secret reaches the pods", databaseResyncInterval)
	}
}
