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

package suffixprobe

import (
	"strings"
	"testing"

	"github.com/go-ldap/ldap/v3"
)

const testSuffix = "dc=example,dc=org"

func entry(attrs map[string][]string) *ldap.Entry {
	e := &ldap.Entry{DN: testSuffix}
	for name, vals := range attrs {
		e.Attributes = append(e.Attributes, &ldap.EntryAttribute{Name: name, Values: vals})
	}
	return e
}

// TestClassify pins ADR-025's glue signature against the entry shapes measured
// live on 2026-09-13. Both slapd paths that manufacture a glue at the suffix
// are represented: the non-present-delete demotion (objectClass replaced with
// top+glue in place) and syncrepl_add_glue_ancestors (a bare entry with no RDN
// attribute at all).
func TestClassify(t *testing.T) {
	tests := []struct {
		name     string
		e        *ldap.Entry
		want     Outcome
		wantUUID string
	}{
		// The captured artifact: objectClass top+glue, structuralObjectClass
		// glue, no dc — and a locally minted entryUUID the healthy pods did
		// not carry.
		{"captured glue entry", entry(map[string][]string{
			"objectClass":           {"top", "glue"},
			"structuralObjectClass": {"glue"},
			"entryUUID":             {"1a914f3a-43f0-1041-9ed3-896165d17446"},
		}), OutcomeGlue, "1a914f3a-43f0-1041-9ed3-896165d17446"},

		// slapd returns attribute names in their canonical case; the repo has
		// been bitten by exact-match attribute reads before (2026-04-16).
		{"glue in mixed case", entry(map[string][]string{
			"objectclass": {"TOP", "GLUE"},
			"entryuuid":   {"u-glue"},
		}), OutcomeGlue, "u-glue"},

		{"glue via structuralObjectClass only", entry(map[string][]string{
			"objectClass":           {"top"},
			"structuralObjectClass": {"glue"},
		}), OutcomeGlue, ""},

		// Revealed by the control but hidden without it, and not glue: a
		// referral or subentry. The probe's job is the glue class — it says
		// "cannot assess" rather than inventing a verdict.
		{"hidden but not glue is not a verdict", entry(map[string][]string{
			"objectClass":           {"top", "referral", "extensibleObject"},
			"structuralObjectClass": {"referral"},
		}), OutcomeError, ""},

		// A real suffix entry reached through the control (it would normally
		// have been visible without it) is still not glue.
		{"real entry is not glue", entry(map[string][]string{
			"objectClass":           {"top", "dcObject", "organization"},
			"structuralObjectClass": {"organization"},
			"entryUUID":             {"1b03268c-0000-0000-0000-000000000000"},
		}), OutcomeError, "1b03268c-0000-0000-0000-000000000000"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.e, testSuffix)
			if got.Outcome != tc.want {
				t.Errorf("Classify(%s).Outcome = %s, want %s (detail %q)",
					tc.name, got.Outcome, tc.want, got.Detail)
			}
			if got.UUID != tc.wantUUID {
				t.Errorf("Classify(%s).UUID = %q, want %q", tc.name, got.UUID, tc.wantUUID)
			}
			if got.Outcome == OutcomeGlue && tc.wantUUID != "" &&
				!strings.Contains(got.Detail, tc.wantUUID) {
				t.Errorf("Classify(%s).Detail = %q, want it to name the entryUUID", tc.name, got.Detail)
			}
			if got.Suffix != testSuffix {
				t.Errorf("Classify(%s).Suffix = %q, want %q", tc.name, got.Suffix, testSuffix)
			}
		})
	}
}

// TestObservationPredicates pins the three readings callers make, and in
// particular that OutcomeError is neither visible nor assessable: "could not
// read it" must never pass for "it is not there" or "it is fine".
func TestObservationPredicates(t *testing.T) {
	tests := []struct {
		outcome                   Outcome
		visible, glue, assessable bool
	}{
		{OutcomeVisible, true, false, true},
		{OutcomeGlue, false, true, true},
		{OutcomeMissing, false, false, true},
		{OutcomeError, false, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.outcome.String(), func(t *testing.T) {
			o := Observation{Outcome: tc.outcome}
			if o.Visible() != tc.visible || o.Glue() != tc.glue || o.Assessable() != tc.assessable {
				t.Errorf("%s: visible=%v glue=%v assessable=%v, want %v/%v/%v",
					tc.outcome, o.Visible(), o.Glue(), o.Assessable(),
					tc.visible, tc.glue, tc.assessable)
			}
		})
	}
}
