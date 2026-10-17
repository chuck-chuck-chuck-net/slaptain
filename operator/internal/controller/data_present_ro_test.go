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
	"strconv"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// pod0Target is testPod0's suffix-probe target, rendered name|host|readOnly.
const pod0Target = "slapd-0|slapd-0.slapd-headless.ns.svc.cluster.local|false"

// TestAggregateDataPresentReadOnly pins the RO-fleet extension of ADR-025
// Decision 5. A glue provably propagates to a read-only replica — the
// 2026-09-13 incident's RO pod carried the SAME glue with the SAME entryUUID as
// its provider (ADR-025 evidence item 5) — so RO pods are probed too. What they
// must NOT do is become indistinguishable from a writable pod's failure: an RO
// replica lacking the suffix is legitimately transient while it performs its
// initial sync, and a writable pod lacking it never is.
func TestAggregateDataPresentReadOnly(t *testing.T) {
	const suffix = "dc=example,dc=org"
	rw := func(pod string, assessed, present bool) podPresence {
		return podPresence{pod: pod, assessed: assessed, present: present}
	}
	ro := func(pod string, assessed, present bool) podPresence {
		return podPresence{pod: pod, readOnly: true, assessed: assessed, present: present}
	}
	gluedRO := func(pod string) podPresence {
		return podPresence{pod: pod, readOnly: true, assessed: true, glued: true}
	}

	tests := []struct {
		name       string
		results    []podPresence
		known      bool
		wantStatus metav1.ConditionStatus
		wantReason string
		wantInMsg  string
	}{
		// The reach this change buys: an RO-only divergence used to be invisible
		// to the standing signal. It is reported now — under its OWN reason,
		// because it is the one shape of this condition that has a legitimate
		// transient cause.
		{"RO pod hides the entry while every RW pod has it", []podPresence{
			rw(testPod0, true, true), rw(testPod1, true, true),
			ro(testROPod0, true, false),
		}, true, metav1.ConditionFalse, reasonDataMissingOnReadOnlyPods, testROPod0},

		// A glue is corruption, not a sync stage: the same reason regardless of
		// the pod's role, naming the RO pod.
		{"glue on an RO pod is the glue reason", []podPresence{
			rw(testPod0, true, true), gluedRO(testROPod0),
		}, true, metav1.ConditionFalse, reasonGlueSuffix, testROPod0},

		// Positive control: a writable pod's divergence keeps the RW reason and
		// is not softened by a healthy RO fleet.
		{"RW divergence with the RO fleet healthy keeps the RW reason", []podPresence{
			rw(testPod0, true, false), rw(testPod1, true, true),
			ro(testROPod0, true, true),
		}, true, metav1.ConditionFalse, reasonDataMissingOnPods, testPod0},

		// Every RW pod hiding it while only the RO fleet still shows it is still
		// the RW alert — an RO sighting is not an excuse.
		{"every RW pod hides it, RO still has it", []podPresence{
			rw(testPod0, true, false), rw(testPod1, true, false),
			ro(testROPod0, true, true),
		}, true, metav1.ConditionFalse, reasonDataMissingOnPods, testPod0},

		// Unreachable is never evidence: an RO pod that could not be dialled
		// must not manufacture a verdict in either direction.
		{"RO pod unreachable, every RW pod present", []podPresence{
			rw(testPod0, true, true), rw(testPod1, true, true),
			ro(testROPod0, false, false),
		}, true, metav1.ConditionTrue, reasonRootEntryVisible, testROPod0},

		{"RO pod unreachable, RW divergence", []podPresence{
			rw(testPod0, true, false), rw(testPod1, true, true),
			ro(testROPod0, false, false),
		}, true, metav1.ConditionFalse, reasonDataMissingOnPods, testPod0},

		// Nothing visible anywhere: an RO pod's absence adds no new verdict, the
		// knownPopulated axis still decides. A never-populated peer site bringing
		// up an RO replica must not read as loss.
		{"RO absent and RW unreachable on a never-populated database", []podPresence{
			rw(testPod0, false, false), ro(testROPod0, true, false),
		}, false, metav1.ConditionUnknown, reasonNoDataYet, ""},

		{"RO absent and RW unreachable on a populated database", []podPresence{
			rw(testPod0, false, false), ro(testROPod0, true, false),
		}, true, metav1.ConditionFalse, reasonDataMissing, ""},

		{"healthy fleet including RO is True", []podPresence{
			rw(testPod0, true, true), rw(testPod1, true, true),
			ro(testROPod0, true, true),
		}, true, metav1.ConditionTrue, reasonRootEntryVisible, "read-only"},
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

// TestAggregateDataPresentRWOnlyMessagesUnchanged is the readReplicas=0 positive
// control: without read-only replicas the verdict AND its message must be
// byte-identical to what the RW-only implementation produced. The RO extension
// is reach, not a rewrite of the standing signal.
func TestAggregateDataPresentRWOnlyMessagesUnchanged(t *testing.T) {
	const suffix = "dc=example,dc=org"
	rw := func(pod string, assessed, present bool) podPresence {
		return podPresence{pod: pod, assessed: assessed, present: present}
	}

	tests := []struct {
		name    string
		results []podPresence
		known   bool
		wantMsg string
	}{
		{"all present", []podPresence{rw(testPod0, true, true), rw(testPod1, true, true)}, true,
			"root entry dc=example,dc=org visible on all 2 reached RW pod(s)"},
		{"none reachable", []podPresence{rw(testPod0, false, false)}, true,
			"could not reach any RW pod to verify data presence"},
		{"missing everywhere, known populated", []podPresence{rw(testPod0, true, false)}, true,
			"root entry dc=example,dc=org not visible on any reachable RW pod — possible data loss; " +
				"this condition is informational and does not trigger operator action (see ADR-012)"},
		{"missing everywhere, never populated", []podPresence{rw(testPod0, true, false)}, false,
			"root entry dc=example,dc=org is not present on any reached RW pod, and this database has " +
				"never been seeded, restored or observed holding data — it is waiting for its first " +
				"data (replication from a founder site or a legacy provider). Not an alert: nothing " +
				"is known to have been lost"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, msg := aggregateDataPresent(suffix, tc.results, tc.known)
			if msg != tc.wantMsg {
				t.Errorf("aggregateDataPresent(%s) message\n got: %q\nwant: %q", tc.name, msg, tc.wantMsg)
			}
		})
	}
}

// TestSuffixProbeTargets pins which pods the probe visits. readReplicas=0 (the
// common case) must produce exactly the RW list and nothing else — no extra
// dials, no behaviour change at all.
func TestSuffixProbeTargets(t *testing.T) {
	cluster := func(replicas, readReplicas int32) *ldapv1alpha1.SlapdCluster {
		return &ldapv1alpha1.SlapdCluster{
			ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Namespace: "ns"},
			Spec: ldapv1alpha1.SlapdClusterSpec{
				Replicas:     replicas,
				ReadReplicas: readReplicas,
			},
		}
	}

	tests := []struct {
		name string
		sc   *ldapv1alpha1.SlapdCluster
		want []string // "pod|host|readOnly"
	}{
		{"no read replicas", cluster(2, 0), []string{
			pod0Target,
			"slapd-1|slapd-1.slapd-headless.ns.svc.cluster.local|false",
		}},
		{"replicas unset defaults to one", cluster(0, 0), []string{
			pod0Target,
		}},
		{"read replicas ride the RO headless service", cluster(2, 2), []string{
			pod0Target,
			"slapd-1|slapd-1.slapd-headless.ns.svc.cluster.local|false",
			"slapd-readonly-0|slapd-readonly-0.slapd-readonly-headless.ns.svc.cluster.local|true",
			"slapd-readonly-1|slapd-readonly-1.slapd-readonly-headless.ns.svc.cluster.local|true",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, tg := range suffixProbeTargets(tc.sc, "cluster.local") {
				got = append(got, tg.pod+"|"+tg.host+"|"+strconv.FormatBool(tg.readOnly))
			}
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("suffixProbeTargets(%s) =\n%s\nwant\n%s",
					tc.name, strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}
