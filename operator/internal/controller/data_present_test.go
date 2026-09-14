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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// TestAggregateDataPresent pins the ADR-025 D1 semantics: DataPresent is True
// only when EVERY assessed RW pod shows the suffix's root entry to an ordinary
// base search. The pre-ADR-025 any-pod-visible verdict read True over a pod
// whose suffix had been demoted to a hidden glue entry — the standing signal
// that would have caught the 2026-09-13 incident.
//
// knownPopulated is the second axis (2026-09-14): whether this database is
// known ever to have held data — by seed, by restore, or by having been
// observed. It decides only what an ALL-EMPTY reading means. Empty with no such
// knowledge is a legitimate first bring-up on a never-seeded database (the
// founder-only peer site, the migration cluster) and must read Unknown, not
// False; empty after data was known to be there is the data-loss alert.
func TestAggregateDataPresent(t *testing.T) {
	const suffix = "dc=example,dc=org"
	p := func(pod string, assessed, present bool) podPresence {
		return podPresence{pod: pod, assessed: assessed, present: present}
	}
	glued := func(pod string) podPresence {
		return podPresence{pod: pod, assessed: true, glued: true}
	}

	tests := []struct {
		name       string
		results    []podPresence
		known      bool
		wantStatus metav1.ConditionStatus
		wantReason string
		wantInMsg  string // substring the message must carry ("" = don't check)
	}{
		{"all pods present", []podPresence{
			p("slapd-0", true, true), p("slapd-1", true, true), p("slapd-2", true, true),
		}, true, metav1.ConditionTrue, "RootEntryVisible", ""},

		// The live incident shape: pod-0 hides a glue, peers look healthy.
		// Any-pod-visible said True; all-pods must say False and name the pod.
		{"one pod hides the entry", []podPresence{
			p("slapd-0", true, false), p("slapd-1", true, true), p("slapd-2", true, true),
		}, true, metav1.ConditionFalse, "DataMissingOnPods", "slapd-0"},

		{"missing everywhere", []podPresence{
			p("slapd-0", true, false), p("slapd-1", true, false),
		}, true, metav1.ConditionFalse, "DataMissing", ""},

		{"no pod reachable", []podPresence{
			p("slapd-0", false, false), p("slapd-1", false, false),
		}, true, metav1.ConditionUnknown, "NoReachablePod", ""},

		{"empty result set", nil, true, metav1.ConditionUnknown, "NoReachablePod", ""},

		// An unassessable pod is no evidence either way: the assessed pods carry
		// the verdict, and the message discloses what went unchecked.
		{"present on assessed, one unreachable", []podPresence{
			p("slapd-0", true, true), p("slapd-1", false, false),
		}, true, metav1.ConditionTrue, "RootEntryVisible", "slapd-1"},

		{"hidden on assessed, one unreachable", []podPresence{
			p("slapd-0", true, false), p("slapd-1", false, false), p("slapd-2", true, true),
		}, true, metav1.ConditionFalse, "DataMissingOnPods", "slapd-0"},

		{"single pod present", []podPresence{
			p("slapd-0", true, true),
		}, true, metav1.ConditionTrue, "RootEntryVisible", ""},

		{"single pod hidden", []podPresence{
			p("slapd-0", true, false),
		}, true, metav1.ConditionFalse, "DataMissing", ""},

		// ── row a: the incident shape on a NEVER-SEEDED database ────────────
		// The migration cluster and every non-founder site of a mesh (ADR-025
		// D1 tells them to omit spec.seed). Split visibility is positive
		// evidence in itself — it needs no seed latch and no memory.
		{"seedless: one pod hides the entry", []podPresence{
			p("slapd-0", true, false), p("slapd-1", true, true), p("slapd-2", true, true),
		}, false, metav1.ConditionFalse, "DataMissingOnPods", "slapd-0"},

		// ── row b: the cry-wolf control ─────────────────────────────────────
		// Never seeded, never restored, never observed, nothing anywhere yet:
		// a peer site waiting for its first refresh. MUST NOT be False.
		{"seedless and empty everywhere is not an alert", []podPresence{
			p("slapd-0", true, false), p("slapd-1", true, false),
		}, false, metav1.ConditionUnknown, "NoDataYet", ""},

		// ── row d: a glue is absolute ───────────────────────────────────────
		// Positively identified by ManageDsaIT, so it stands on its own: no
		// latch, no peer to disagree with, nothing else required.
		{"glue on one pod fails even when never seeded", []podPresence{
			glued("slapd-0"), p("slapd-1", true, true),
		}, false, metav1.ConditionFalse, "GlueSuffix", "slapd-0"},

		{"glue on every pod fails", []podPresence{
			glued("slapd-0"), glued("slapd-1"),
		}, false, metav1.ConditionFalse, "GlueSuffix", "slapd-1"},

		// A glue outranks the all-empty reading: it is a verdict, not an absence.
		{"glue beats NoDataYet", []podPresence{
			glued("slapd-0"), p("slapd-1", true, false),
		}, false, metav1.ConditionFalse, "GlueSuffix", ""},

		// ── row c: once known populated, empty everywhere is the alert ──────
		{"known populated and empty everywhere alerts", []podPresence{
			p("slapd-0", true, false), p("slapd-1", true, false),
		}, true, metav1.ConditionFalse, "DataMissing", ""},

		// ── row e: positive control — the healthy seedless steady state ─────
		// A refusal-heavy change passes vacuously against a deny-everything
		// implementation; this row must still report True.
		{"seedless and present everywhere is True", []podPresence{
			p("slapd-0", true, true), p("slapd-1", true, true),
		}, false, metav1.ConditionTrue, "RootEntryVisible", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, reason, msg := aggregateDataPresent(suffix, tc.results, tc.known)
			if status != tc.wantStatus || reason != tc.wantReason {
				t.Errorf("aggregateDataPresent(%s) = (%s, %s, %q), want (%s, %s)",
					tc.name, status, reason, msg, tc.wantStatus, tc.wantReason)
			}
			if tc.wantInMsg != "" && !strings.Contains(msg, tc.wantInMsg) {
				t.Errorf("aggregateDataPresent(%s) message %q does not mention %q", tc.name, msg, tc.wantInMsg)
			}
		})
	}
}

// TestDatabaseKnownPopulated pins what counts as "this database has held data".
// Three independent sources, all positive evidence, all monotone: the seed
// latch, the bootstrapFrom restore latch, and the observation latch this change
// adds for the databases that have neither.
func TestDatabaseKnownPopulated(t *testing.T) {
	tests := []struct {
		name   string
		status ldapv1alpha1.SlapdDatabaseStatus
		want   bool
	}{
		{"fresh seedless database", ldapv1alpha1.SlapdDatabaseStatus{}, false},
		{"seeded", ldapv1alpha1.SlapdDatabaseStatus{SeedApplied: true}, true},
		{"restored via bootstrapFrom", ldapv1alpha1.SlapdDatabaseStatus{RestoreApplied: true}, true},
		{"data observed once", ldapv1alpha1.SlapdDatabaseStatus{DataObserved: true}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sd := &ldapv1alpha1.SlapdDatabase{Status: tc.status}
			if got := databaseKnownPopulated(sd); got != tc.want {
				t.Errorf("databaseKnownPopulated(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestDataObservedLatch pins the latch's direction: it is set by positive
// evidence only and never cleared. An absent entry, an unreachable pod and a
// glue are all NOT observations of data — in particular a glue must not latch,
// or a cluster that saw one would afterwards report DataMissing (an alert) for
// a state that may never have held data.
func TestDataObservedLatch(t *testing.T) {
	tests := []struct {
		name    string
		results []podPresence
		want    bool
	}{
		{"nothing anywhere", []podPresence{
			{pod: "slapd-0", assessed: true}, {pod: "slapd-1", assessed: true},
		}, false},
		{"unreachable pods", []podPresence{
			{pod: "slapd-0"}, {pod: "slapd-1"},
		}, false},
		{"glue only", []podPresence{
			{pod: "slapd-0", assessed: true, glued: true},
		}, false},
		{"present on one pod", []podPresence{
			{pod: "slapd-0", assessed: true, present: true}, {pod: "slapd-1", assessed: true},
		}, true},
		{"present everywhere", []podPresence{
			{pod: "slapd-0", assessed: true, present: true},
			{pod: "slapd-1", assessed: true, present: true},
		}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := dataWasObserved(tc.results); got != tc.want {
				t.Errorf("dataWasObserved(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
