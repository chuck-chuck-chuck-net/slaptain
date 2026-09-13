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
)

// TestAggregateDataPresent pins the ADR-025 D1 semantics: DataPresent is True
// only when EVERY reached RW pod shows the suffix's root entry to an ordinary
// base search. The pre-ADR-025 any-pod-visible verdict read True over a pod
// whose suffix had been demoted to a hidden glue entry — the standing signal
// that would have caught the 2026-09-13 incident.
func TestAggregateDataPresent(t *testing.T) {
	const suffix = "dc=example,dc=org"
	p := func(pod string, reached, present bool) podPresence {
		return podPresence{pod: pod, reached: reached, present: present}
	}

	tests := []struct {
		name       string
		results    []podPresence
		wantStatus metav1.ConditionStatus
		wantReason string
		wantInMsg  string // substring the message must carry ("" = don't check)
	}{
		{"all pods present", []podPresence{
			p("slapd-0", true, true), p("slapd-1", true, true), p("slapd-2", true, true),
		}, metav1.ConditionTrue, "RootEntryVisible", ""},

		// The live incident shape: pod-0 hides a glue, peers look healthy.
		// Any-pod-visible said True; all-pods must say False and name the pod.
		{"one pod hides the entry", []podPresence{
			p("slapd-0", true, false), p("slapd-1", true, true), p("slapd-2", true, true),
		}, metav1.ConditionFalse, "DataMissingOnPods", "slapd-0"},

		{"missing everywhere", []podPresence{
			p("slapd-0", true, false), p("slapd-1", true, false),
		}, metav1.ConditionFalse, "DataMissing", ""},

		{"no pod reachable", []podPresence{
			p("slapd-0", false, false), p("slapd-1", false, false),
		}, metav1.ConditionUnknown, "NoReachablePod", ""},

		{"empty result set", nil, metav1.ConditionUnknown, "NoReachablePod", ""},

		// An unreachable pod is no evidence either way: the reached pods carry
		// the verdict, and the message discloses what went unchecked.
		{"present on reached, one unreachable", []podPresence{
			p("slapd-0", true, true), p("slapd-1", false, false),
		}, metav1.ConditionTrue, "RootEntryVisible", "slapd-1"},

		{"hidden on reached, one unreachable", []podPresence{
			p("slapd-0", true, false), p("slapd-1", false, false), p("slapd-2", true, true),
		}, metav1.ConditionFalse, "DataMissingOnPods", "slapd-0"},

		{"single pod present", []podPresence{
			p("slapd-0", true, true),
		}, metav1.ConditionTrue, "RootEntryVisible", ""},

		{"single pod hidden", []podPresence{
			p("slapd-0", true, false),
		}, metav1.ConditionFalse, "DataMissing", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, reason, msg := aggregateDataPresent(suffix, tc.results)
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
