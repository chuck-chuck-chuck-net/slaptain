/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"fmt"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// Reasons a SlapdDatabase's phase carries; Applied and PartiallyApplied are
// shared with the SlapdSchema controller, whose condition says the same thing.
const (
	reasonNoPodsReachable  = "NoPodsReachable"
	reasonPartiallyApplied = "PartiallyApplied"
	reasonApplied          = "Applied"
)

// databasePhase selects the phase, reason and message a SlapdDatabase reconcile
// publishes, from what that pass achieved.
//
// Lifted out of Reconcile because the precedence it encodes is otherwise dead
// in every test: the envtest fixtures have no pods, so appliedPods is always
// empty, the phase is already Error, and the branch that ranks a RID refusal
// against PartiallyApplied is never reached.
//
// The precedence, highest first:
//
//  1. No pod could be reached at all → Error/NoPodsReachable. A database no pod
//     answers on has a louder problem than any judgement about its spec, and a
//     refusal reported in its place would send a human to edit a CR that is
//     fine.
//  2. A refused RID segment or peer list → Degraded, naming the refusal's own
//     reason and message. It outranks the generic PartiallyApplied because it
//     is the one pending item a human fixes by editing a CR rather than by
//     waiting (ADR-031 decision 6).
//  3. Work still pending → Degraded/PartiallyApplied.
//  4. Otherwise → Running/Applied.
//
// ridRefusal is nil when nothing was refused. failedPods and appliedPods are
// the pass's own tallies, used for the message a human reads first.
func databasePhase(
	appliedPods, failedPods []string,
	pendingWork bool,
	ridRefusal *ridSegmentVerdict,
) (ldapv1alpha1.SlapdDatabasePhase, string, string) {
	if len(appliedPods) == 0 {
		return ldapv1alpha1.DatabasePhaseError, reasonNoPodsReachable,
			"Could not apply database to any pod"
	}
	if ridRefusal != nil {
		return ldapv1alpha1.DatabasePhaseDegraded, ridRefusal.Reason, ridRefusal.Message
	}
	if pendingWork {
		return ldapv1alpha1.DatabasePhaseDegraded, reasonPartiallyApplied,
			fmt.Sprintf("Database applied to %d pods, %d failed or pending",
				len(appliedPods), len(failedPods))
	}
	return ldapv1alpha1.DatabasePhaseRunning, reasonApplied,
		fmt.Sprintf("Database applied to %d pods", len(appliedPods))
}
