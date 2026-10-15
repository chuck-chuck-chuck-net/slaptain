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

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// TestAggregateStartupConfigConverged pins ADR-034 R3 as the 2026-10-15
// amendment renamed it: StartupConfigConverged is True when every pod the
// controller READ carries the declared table's value, False with reason
// RestartRequired naming the pods that do not, and a pod that could not be read
// is never evidence of divergence (ADR-025's rule for the suffix probe).
//
// The condition is observation only. Nothing here may ever reach a write path:
// olcSpSessionlogSource is a double free on every online path, which is the
// whole reason the attribute travels through the init container instead.
func TestAggregateStartupConfigConverged(t *testing.T) {
	const want = "cn=accesslog-default,dc=example,dc=org"

	ok := func(pod string) startupConfigObservation {
		return startupConfigObservation{pod: pod, assessed: true}
	}
	bad := func(pod, got string) startupConfigObservation {
		return startupConfigObservation{pod: pod, assessed: true, diverged: []startupAttrDivergence{
			{Attribute: startupAttrSessionlogSource, Want: want, Got: got},
		}}
	}
	unread := func(pod string) startupConfigObservation {
		return startupConfigObservation{pod: pod}
	}

	tests := []struct {
		name       string
		declared   bool
		results    []startupConfigObservation
		wantStatus metav1.ConditionStatus
		wantReason string
		wantInMsg  []string
		wantNotMsg []string
	}{
		{
			name:       "every pod carries the declared value",
			declared:   true,
			results:    []startupConfigObservation{ok("slapd-0"), ok("slapd-1"), ok("slapd-2")},
			wantStatus: metav1.ConditionTrue,
			wantReason: "Converged",
			wantInMsg:  []string{"3"},
		},
		{
			name:       "one pod diverges and is named",
			declared:   true,
			results:    []startupConfigObservation{ok("slapd-0"), bad("slapd-1", ""), ok("slapd-2")},
			wantStatus: metav1.ConditionFalse,
			wantReason: "RestartRequired",
			wantInMsg:  []string{"slapd-1", startupAttrSessionlogSource, want},
			wantNotMsg: []string{"slapd-0", "slapd-2"},
		},
		{
			name:     "two pods diverge and both are named",
			declared: true,
			results: []startupConfigObservation{
				ok("slapd-0"), bad("slapd-1", ""), bad("slapd-2", "cn=accesslog-stale,dc=example,dc=org"),
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: "RestartRequired",
			wantInMsg:  []string{"slapd-1", "slapd-2", "cn=accesslog-stale,dc=example,dc=org"},
		},
		{
			// ADR-025's rule, applied here: unreadable evidence never counts
			// toward a verdict. The reached pods agree, so the condition is
			// True — the pod that was not read is noted, not indicted.
			name:       "one pod unreachable while the rest match is True, not False",
			declared:   true,
			results:    []startupConfigObservation{ok("slapd-0"), unread("slapd-1"), ok("slapd-2")},
			wantStatus: metav1.ConditionTrue,
			wantReason: "Converged",
			wantInMsg:  []string{"slapd-1"},
		},
		{
			name:       "every pod unreachable is Unknown",
			declared:   true,
			results:    []startupConfigObservation{unread("slapd-0"), unread("slapd-1")},
			wantStatus: metav1.ConditionUnknown,
			wantReason: "NoReachablePod",
			wantInMsg:  []string{"slapd-0", "slapd-1"},
		},
		{
			// A database asking for no startup-only attribute is converged by
			// definition, and says so rather than going Unknown for want of
			// evidence it does not need.
			name:       "database declares nothing",
			declared:   false,
			results:    []startupConfigObservation{ok("slapd-0"), ok("slapd-1")},
			wantStatus: metav1.ConditionTrue,
			wantReason: "NoStartupOnlyAttributes",
		},
		{
			name:       "database declares nothing and no pod was read",
			declared:   false,
			results:    []startupConfigObservation{unread("slapd-0")},
			wantStatus: metav1.ConditionTrue,
			wantReason: "NoStartupOnlyAttributes",
		},
		{
			// A leftover value on disk after the field was dropped is still a
			// divergence: the converger removes it at the next pod start, and
			// until then the live configuration is not the declared one.
			name:     "database declares nothing but a pod still carries the attribute",
			declared: false,
			results: []startupConfigObservation{ok("slapd-0"), {
				pod: "slapd-1", assessed: true, diverged: []startupAttrDivergence{
					{Attribute: startupAttrSessionlogSource, Want: "", Got: want},
				},
			}},
			wantStatus: metav1.ConditionFalse,
			wantReason: "RestartRequired",
			wantInMsg:  []string{"slapd-1", startupAttrSessionlogSource},
		},
		{
			// Read-only consumer pods carry no syncprov overlay, so they
			// contribute no observation at all. An RO-only result set is
			// "nothing to say", never a divergence.
			name:       "no observations at all",
			declared:   false,
			results:    nil,
			wantStatus: metav1.ConditionTrue,
			wantReason: "NoStartupOnlyAttributes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, reason, msg := aggregateStartupConfigConverged(tt.declared, tt.results)
			if status != tt.wantStatus {
				t.Errorf("status = %v, want %v (msg: %s)", status, tt.wantStatus, msg)
			}
			if reason != tt.wantReason {
				t.Errorf("reason = %q, want %q (msg: %s)", reason, tt.wantReason, msg)
			}
			for _, sub := range tt.wantInMsg {
				if !strings.Contains(msg, sub) {
					t.Errorf("message %q does not contain %q", msg, sub)
				}
			}
			for _, sub := range tt.wantNotMsg {
				if strings.Contains(msg, sub) {
					t.Errorf("message %q must not contain %q", msg, sub)
				}
			}
		})
	}
}

// TestCompareStartupSessionlogSource pins the per-pod comparison:
// what the live overlay carries versus what desiredSessionlog resolved to. The
// aggregator above is fed by this, so a comparison that reads "" and
// "cn=accesslog-x" as equal would make the condition permanently True.
func TestCompareStartupSessionlogSource(t *testing.T) {
	tests := []struct {
		name         string
		want         string
		live         []string
		wantDiverged bool
	}{
		{"both absent", "", nil, false},
		{"both absent, empty slice", "", []string{}, false},
		{"match", "cn=accesslog-default,dc=x", []string{"cn=accesslog-default,dc=x"}, false},
		{"DN comparison is case-insensitive", "CN=Accesslog-Default,DC=x", []string{"cn=accesslog-default,dc=x"}, false},
		{"whitespace is trimmed", "cn=accesslog-default,dc=x", []string{" cn=accesslog-default,dc=x "}, false},
		{"wanted but absent", "cn=accesslog-default,dc=x", nil, true},
		{"present but not wanted", "", []string{"cn=accesslog-default,dc=x"}, true},
		{"different value", "cn=accesslog-a,dc=x", []string{"cn=accesslog-b,dc=x"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, diverged := compareStartupAttr(startupAttrSessionlogSource, tt.want, tt.live)
			if diverged != tt.wantDiverged {
				t.Fatalf("diverged = %v, want %v", diverged, tt.wantDiverged)
			}
			if diverged && d.Attribute != startupAttrSessionlogSource {
				t.Errorf("attribute = %q, want %q", d.Attribute, startupAttrSessionlogSource)
			}
		})
	}
}

// TestObserveStartupConfig pins the per-pod collector: it marks the pod
// assessed only where it actually read the overlay, is nil-safe for the
// read-only pods that have none, and resolves what is WANTED through
// desiredSessionlog rather than re-deriving it.
func TestObserveStartupConfig(t *testing.T) {
	const sourceDN = "cn=accesslog-default,dc=example,dc=org"

	t.Run("nil observation is a no-op (read-only pod)", func(t *testing.T) {
		observeStartupConfig(nil, sessionlogDecision{
			Kind: sessionlogKindAccesslog, SourceDN: sourceDN}, nil)
	})

	t.Run("reading the overlay marks the pod assessed", func(t *testing.T) {
		obs := startupConfigObservation{pod: "slapd-0"}
		observeStartupConfig(&obs, sessionlogDecision{
			Kind: sessionlogKindAccesslog, SourceDN: sourceDN}, []string{sourceDN})
		if !obs.assessed {
			t.Fatal("pod was read but is not marked assessed")
		}
		if len(obs.diverged) != 0 {
			t.Fatalf("matching value reported as divergence: %#v", obs.diverged)
		}
	})

	t.Run("a missing declared value is a divergence", func(t *testing.T) {
		obs := startupConfigObservation{pod: "slapd-0"}
		observeStartupConfig(&obs, sessionlogDecision{
			Kind: sessionlogKindAccesslog, SourceDN: sourceDN}, nil)
		if !obs.assessed {
			t.Fatal("pod was read but is not marked assessed")
		}
		if len(obs.diverged) != 1 || obs.diverged[0].Want != sourceDN {
			t.Fatalf("divergence not recorded: %#v", obs.diverged)
		}
	})

	t.Run("the in-memory session log wants the attribute absent", func(t *testing.T) {
		obs := startupConfigObservation{pod: "slapd-0"}
		observeStartupConfig(&obs,
			sessionlogDecision{Kind: sessionlogKindInMemory, Ops: 5000}, nil)
		if len(obs.diverged) != 0 {
			t.Fatalf("absent attribute reported as divergence: %#v", obs.diverged)
		}

		left := startupConfigObservation{pod: "slapd-1"}
		observeStartupConfig(&left,
			sessionlogDecision{Kind: sessionlogKindInMemory, Ops: 5000}, []string{sourceDN})
		if len(left.diverged) != 1 || left.diverged[0].Want != "" {
			t.Fatalf("leftover attribute not reported as divergence: %#v", left.diverged)
		}
	})
}

// TestDeclaresStartupOnlyAttributes: exactly the databases whose records the
// startup-config ConfigMap carries, read off the same desiredSessionlog the
// ConfigMap writer uses. A disagreement here would publish a condition about a
// table entry the pods were never given.
func TestDeclaresStartupOnlyAttributes(t *testing.T) {
	accesslog := &ldapv1alpha1.SlapdDatabase{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: ldapv1alpha1.SlapdDatabaseSpec{
			Replication: ldapv1alpha1.DatabaseReplicationConfig{
				SyncprovSessionlogSource: ldapv1alpha1.SyncprovSessionlogSourceAccesslog,
			},
		},
	}
	if !declaresStartupOnlyAttributes(accesslog) {
		t.Error("a database sourcing its session log from its journal declares a startup-only attribute")
	}
	if declaresStartupOnlyAttributes(&ldapv1alpha1.SlapdDatabase{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
	}) {
		t.Error("a default database declares no startup-only attribute")
	}

	// Drift guard against the ConfigMap writer: the condition's "declared" and
	// the table's records must be the same question.
	recs := startupConfigFor([]ldapv1alpha1.SlapdDatabase{*accesslog}, "")
	if len(recs.Records) != 1 || recs.Records[0].Attribute != startupAttrSessionlogSource {
		t.Fatalf("startupConfigFor and declaresStartupOnlyAttributes disagree: %#v", recs.Records)
	}
}
