/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"strings"
	"testing"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The phase precedence has lived inside Reconcile since ADR-031 M3, and it has
// been DEAD in every test that touches it: the envtest fixtures have no pods,
// so appliedPods is always empty, the phase is already Error, and the branch
// that ranks a RID refusal against PartiallyApplied is never reached. Lifting
// it out is what makes it assertable at all — so these cases are not a
// restatement of code that was already checked, they are the first check.
//
// What each rank is worth to a human:
//
//   - Error/NoPodsReachable means "the operator cannot talk to this database",
//     and it must survive a refusal, or a cluster that is down reports a CR
//     problem and sends someone to edit a perfectly good manifest.
//   - A RID refusal means "editing a CR fixes this", which is strictly more
//     actionable than PartiallyApplied's "wait and see".
//   - PartiallyApplied means "keep waiting".

func TestDatabasePhasePrecedence(t *testing.T) {
	refusal := &ridSegmentVerdict{
		Reason:  reasonRIDSegmentCollision,
		Message: "spec.replication.ridSegment 3 is also declared by SlapdDatabase \"bravo\"",
	}

	for _, tc := range []struct {
		name        string
		applied     []string
		failed      []string
		pendingWork bool
		refusal     *ridSegmentVerdict
		wantPhase   ldapv1alpha1.SlapdDatabasePhase
		wantReason  string
		why         string
	}{
		{
			name:       "a clean pass is Running",
			applied:    []string{testPod0, testPod1},
			wantPhase:  ldapv1alpha1.DatabasePhaseRunning,
			wantReason: reasonApplied,
			why:        "every pod took the database and nothing was withheld",
		},
		{
			name:        "work still pending is Degraded/PartiallyApplied",
			applied:     []string{testPod0},
			failed:      []string{testPod1},
			pendingWork: true,
			wantPhase:   ldapv1alpha1.DatabasePhaseDegraded,
			wantReason:  reasonPartiallyApplied,
			why:         "the generic wait-and-retry state",
		},
		{
			name:        "a RID refusal outranks PartiallyApplied",
			applied:     []string{testPod0, testPod1},
			pendingWork: true, // a refusal always sets this; the reason must still be the refusal's
			refusal:     refusal,
			wantPhase:   ldapv1alpha1.DatabasePhaseDegraded,
			wantReason:  reasonRIDSegmentCollision,
			why: "it is the one pending item a human fixes by editing a CR rather than by waiting " +
				"(ADR-031 decision 6); PartiallyApplied hides that behind a count",
		},
		{
			name:       "a RID refusal on an otherwise clean pass still reports the refusal",
			applied:    []string{testPod0},
			refusal:    refusal,
			wantPhase:  ldapv1alpha1.DatabasePhaseDegraded,
			wantReason: reasonRIDSegmentCollision,
			why: "the stanzas were withheld, so the database is NOT Running however well the rest of " +
				"the pass went — a withheld mesh reported as healthy is the worst reading of all",
		},
		{
			name:       "no pod reachable is Error, whatever else was judged",
			failed:     []string{testPod0, testPod1},
			refusal:    refusal,
			wantPhase:  ldapv1alpha1.DatabasePhaseError,
			wantReason: reasonNoPodsReachable,
			why: "a database no pod answers on has a louder problem than its spec, and the refusal " +
				"would send a human to edit a CR that may be fine",
		},
		{
			name:        "no pod reachable with no refusal is Error too",
			failed:      []string{testPod0},
			pendingWork: true,
			wantPhase:   ldapv1alpha1.DatabasePhaseError,
			wantReason:  reasonNoPodsReachable,
			why:         "unchanged from before the lift — the regression guard for the ranking above it",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			phase, reason, msg := databasePhase(tc.applied, tc.failed, tc.pendingWork, tc.refusal)
			if phase != tc.wantPhase {
				t.Errorf("phase = %q, want %q — %s", phase, tc.wantPhase, tc.why)
			}
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q — %s", reason, tc.wantReason, tc.why)
			}
			if msg == "" {
				t.Errorf("message is empty: the phase is what a human reads first and the reason alone is a label")
			}
			if tc.wantReason == reasonRIDSegmentCollision && !strings.Contains(msg, "bravo") {
				t.Errorf("message %q drops the refusal's own text, which is the half that names "+
					"what to edit", msg)
			}
		})
	}
}
