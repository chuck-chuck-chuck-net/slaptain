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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A pod that stops answering must say so on the CR.
//
// During the 874 s freeze the operator dialled the frozen pod every 60 s and
// timed out every time — 106 consecutive failures, logged at V(1), reaching
// nobody. The evidence was gathered and thrown away. ADR-030's probe now takes
// such a pod out of the Service endpoints, but nothing yet says WHY, and
// `kubectl get slapdcluster` stayed as uninformative as it was.
//
// This is the same observation, promoted to a condition. It costs no new
// queries: the readings already exist.
func TestEvaluatePodsResponsive(t *testing.T) {
	ok := func(pod, suffix string) csnReading {
		return csnReading{Pod: pod, Suffix: suffix, Answered: true, CSNs: []string{"2026…"}}
	}
	// The server answered and refused — err=49, err=32, anything. It is UP.
	refused := func(pod, suffix string) csnReading {
		return csnReading{Pod: pod, Suffix: suffix, Answered: true, Err: "LDAP Result Code 49"}
	}
	// No answer at all: dial timeout, connection refused, TLS handshake stall.
	silent := func(pod, suffix string) csnReading {
		return csnReading{Pod: pod, Suffix: suffix, Answered: false, Err: "i/o timeout"}
	}

	for _, tc := range []struct {
		name       string
		readings   []csnReading
		replicas   int32
		wantStatus metav1.ConditionStatus
		wantReason string
		mustName   string
		because    string
	}{
		{
			name:       "every pod answers",
			readings:   []csnReading{ok("slapd-0", "a"), ok("slapd-1", "a")},
			replicas:   2,
			wantStatus: metav1.ConditionTrue,
			wantReason: reasonAllPodsAnswering,
			because:    "the normal case must not be noisy",
		},
		{
			name:       "one pod answers nothing on any database",
			readings:   []csnReading{ok("slapd-0", "a"), silent("slapd-1", "a")},
			replicas:   2,
			wantStatus: metav1.ConditionFalse,
			wantReason: reasonPodsNotAnswering,
			mustName:   "slapd-1",
			because:    "this is the freeze, and the condition exists to name the pod",
		},
		{
			// The discrimination that makes the condition mean something: a pod
			// that answers for one database and not another is UP. That is a
			// per-database problem and ReplicationConverged already reports it
			// as CSNQueriesIncomplete; duplicating it here would make this
			// condition fire for reasons that have nothing to do with the pod.
			name:       "a pod silent on one database only is still responsive",
			readings:   []csnReading{ok("slapd-0", "a"), ok("slapd-1", "a"), silent("slapd-1", "b")},
			replicas:   2,
			wantStatus: metav1.ConditionTrue,
			wantReason: reasonAllPodsAnswering,
			because:    "one unreadable database is not an unresponsive pod",
		},
		{
			// Same rule as ADR-030's probe: any LDAP answer means the server is
			// up. A bind rejection is a credential problem, not a dead pod, and
			// calling it unresponsive would send someone to the wrong place.
			name:       "a pod that rejects the bind is answering",
			readings:   []csnReading{ok("slapd-0", "a"), refused("slapd-1", "a")},
			replicas:   2,
			wantStatus: metav1.ConditionTrue,
			wantReason: reasonAllPodsAnswering,
			because:    "err=49 is an answer; the server is up and saying no",
		},
		{
			name:       "several silent pods are all named",
			readings:   []csnReading{silent("slapd-0", "a"), ok("slapd-1", "a"), silent("slapd-2", "a")},
			replicas:   3,
			wantStatus: metav1.ConditionFalse,
			wantReason: reasonPodsNotAnswering,
			mustName:   "slapd-2",
			because:    "naming one of three sends someone to the wrong pod",
		},
		{
			// Nothing was asked, so nothing is known. Reporting True here would
			// be asserting health from an absence of evidence — the mistake
			// ADR-008's amendment corrected for CSN readings.
			name:       "no readings at all",
			readings:   nil,
			replicas:   3,
			wantStatus: metav1.ConditionUnknown,
			wantReason: "NoPodsQueried",
			because:    "unreadable evidence must never count toward a good verdict",
		},
		{
			// A pod the loop never reached — no reading was produced for it at
			// all. Silence by omission is still silence.
			name:       "a pod produced no reading whatsoever",
			readings:   []csnReading{ok("slapd-0", "a")},
			replicas:   2,
			wantStatus: metav1.ConditionFalse,
			wantReason: reasonPodsNotAnswering,
			mustName:   "slapd-1",
			because:    "a pod that was never reached is not a pod that is fine",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := evaluatePodsResponsive(tc.readings, tc.replicas)
			if got.Status != tc.wantStatus {
				t.Errorf("status = %s, want %s — %s", got.Status, tc.wantStatus, tc.because)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", got.Reason, tc.wantReason)
			}
			if tc.mustName != "" && !strings.Contains(got.Message, tc.mustName) {
				t.Errorf("message %q does not name %s — %s", got.Message, tc.mustName, tc.because)
			}
		})
	}
}
