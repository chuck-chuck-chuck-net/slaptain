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

package cmd

import (
	"strings"
	"testing"

	"github.com/chuck-chuck-chuck-net/slaptain/operator/internal/suffixprobe"
)

const testSuffix = "dc=example,dc=org"

func suffixPod(name string, obs ...suffixObservation) podState {
	return podState{name: name, ready: true, suffixEntries: obs}
}

// obs builds one pod's observation in the vocabulary these checks reason in:
// visible / glue / hidden. Hidden means the probe positively found nothing
// (ManageDsaIT included) — a FAILED probe is obsUnassessable, which is a
// different thing and must not count as hidden.
func obs(visible, glue bool, uuid string) suffixObservation {
	o := suffixObservation{Suffix: testSuffix, UUID: uuid, Outcome: suffixprobe.OutcomeMissing}
	switch {
	case glue:
		o.Outcome = suffixprobe.OutcomeGlue
	case visible:
		o.Outcome = suffixprobe.OutcomeVisible
	}
	return o
}

// obsUnassessable is a probe that could not reach a verdict: a denied read, a
// search error, or an entry hidden without being glue.
func obsUnassessable() suffixObservation {
	return suffixObservation{Suffix: testSuffix, Outcome: suffixprobe.OutcomeError}
}

var testDBs = []dbIdentity{{Name: "example-db", Suffix: testSuffix, WantLog: true}}

// TestCheckSuffixVisibility pins the ADR-025 D2 detector: the live incident's
// signature is per-pod disagreement — the suffix entry visible on healthy pods,
// hidden (glue) on the broken one — plus the absolute glue verdict from a
// ManageDSAIT probe.
func TestCheckSuffixVisibility(t *testing.T) {
	tests := []struct {
		name       string
		pods       []podState
		wantStatus string
		wantInMsg  string
	}{
		{"all pods visible", []podState{
			suffixPod("slapd-0", obs(true, false, "u1")),
			suffixPod("slapd-1", obs(true, false, "u1")),
		}, "pass", ""},

		// The live incident: slapd-0 hides a glue while peers show the entry.
		{"glue on one pod fails", []podState{
			suffixPod("slapd-0", obs(false, true, "u-glue")),
			suffixPod("slapd-1", obs(true, false, "u1")),
		}, "fail", "slapd-0"},

		{"glue verdict names glue", []podState{
			suffixPod("slapd-0", obs(false, true, "u-glue")),
			suffixPod("slapd-1", obs(true, false, "u1")),
		}, "fail", "glue"},

		// Hidden without a glue verdict (ManageDSAIT also empty or denied) on
		// one pod while another shows it: still a divergence — fail.
		{"hidden on one pod fails", []podState{
			suffixPod("slapd-0", obs(false, false, "")),
			suffixPod("slapd-1", obs(true, false, "u1")),
		}, "fail", "slapd-0"},

		// RO pods count: a consumer that initial-synced from a glued provider
		// replicates the glue (measured live 2026-09-13).
		{"glue on RO pod fails", []podState{
			suffixPod("slapd-0", obs(true, false, "u1")),
			suffixPod("slapd-readonly-0", obs(false, true, "u-glue")),
		}, "fail", "slapd-readonly-0"},

		// Uniform invisibility is not the divergence signature — anonymous
		// ACLs can hide the entry from this probe on every pod equally.
		{"hidden everywhere warns", []podState{
			suffixPod("slapd-0", obs(false, false, "")),
			suffixPod("slapd-1", obs(false, false, "")),
		}, "warn", ""},

		// A glue is absolute evidence even when every pod hides the entry.
		{"glue everywhere fails", []podState{
			suffixPod("slapd-0", obs(false, true, "u-glue")),
			suffixPod("slapd-1", obs(false, true, "u-glue")),
		}, "fail", "glue"},

		// A pod whose probe FAILED (denied read, search error) is not "hidden":
		// treating it as hidden turned a failed read into a per-pod divergence
		// FAIL. Evidence that degrades to empty on a read failure is the class
		// this repo has been bitten by; it now reads as not-assessed.
		{"unassessable pod does not manufacture divergence", []podState{
			suffixPod("slapd-0", obsUnassessable()),
			suffixPod("slapd-1", obs(true, false, "u1")),
		}, "pass", ""},

		{"unassessable everywhere warns", []podState{
			suffixPod("slapd-0", obsUnassessable()),
			suffixPod("slapd-1", obsUnassessable()),
		}, "warn", ""},

		// Unqueryable pods are pod-readiness's problem, not this check's.
		{"skipped pod ignored", []podState{
			suffixPod("slapd-0", obs(true, false, "u1")),
			{name: "slapd-1", err: "port-forward: connection refused"},
		}, "pass", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := checkSuffixVisibility(testDBs, tc.pods)
			if res.Name != "suffix-visibility" {
				t.Errorf("check name = %q, want suffix-visibility", res.Name)
			}
			if res.Status != tc.wantStatus {
				t.Errorf("status = %q (detail %q), want %q", res.Status, res.Detail, tc.wantStatus)
			}
			if tc.wantInMsg != "" && !strings.Contains(res.Detail, tc.wantInMsg) {
				t.Errorf("detail %q does not mention %q", res.Detail, tc.wantInMsg)
			}
		})
	}
}

// TestCheckSuffixUUIDAgreement pins the identity check: entryUUID is
// syncrepl's identity, and pods disagreeing on the suffix entry's UUID hold
// different entries at the same DN — divergence that CSN comparison cannot see
// (the live glue carried the winner's entryCSN with its own UUID).
func TestCheckSuffixUUIDAgreement(t *testing.T) {
	tests := []struct {
		name       string
		pods       []podState
		wantStatus string
		wantInMsg  string
	}{
		{"all agree", []podState{
			suffixPod("slapd-0", obs(true, false, "1b03268c")),
			suffixPod("slapd-1", obs(true, false, "1b03268c")),
		}, "pass", ""},

		// The live incident: the glue's locally minted UUID vs the real one.
		{"disagreement fails naming pods", []podState{
			suffixPod("slapd-0", obs(false, true, "1a914f3a")),
			suffixPod("slapd-1", obs(true, false, "1b03268c")),
		}, "fail", "slapd-0"},

		{"disagreement lists both uuids", []podState{
			suffixPod("slapd-0", obs(false, true, "1a914f3a")),
			suffixPod("slapd-1", obs(true, false, "1b03268c")),
		}, "fail", "1a914f3a"},

		// No UUID readable anywhere: cannot assess — warn, don't guess.
		{"no uuids warns", []podState{
			suffixPod("slapd-0", obs(false, false, "")),
			suffixPod("slapd-1", obs(false, false, "")),
		}, "warn", ""},

		{"single reader passes", []podState{
			suffixPod("slapd-0", obs(true, false, "1b03268c")),
			suffixPod("slapd-1", obs(false, false, "")),
		}, "pass", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := checkSuffixUUIDAgreement(testDBs, tc.pods)
			if res.Name != "suffix-uuid-agreement" {
				t.Errorf("check name = %q, want suffix-uuid-agreement", res.Name)
			}
			if res.Status != tc.wantStatus {
				t.Errorf("status = %q (detail %q), want %q", res.Status, res.Detail, tc.wantStatus)
			}
			if tc.wantInMsg != "" && !strings.Contains(res.Detail, tc.wantInMsg) {
				t.Errorf("detail %q does not mention %q", res.Detail, tc.wantInMsg)
			}
		})
	}
}
