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

import "testing"

// TestDecideSeedSite pins the ADR-028 §3 site gate: spec.seed.site is a
// DECLARATION read identically at every site ("who is the founder"), compared
// by each operator against its own identity (ADR-028 §4, Phase 1's
// ResolveSiteName). It is the braces to ADR-025's belt, which reacts to
// EVIDENCE (a foreign serverID already created the suffix) rather than to a
// declaration — the two are independent and both must fire.
//
// Four cases, and the last one is the one with a real decision behind it:
//
//   - unset selector → apply. Today's behaviour, byte-for-byte: single-site
//     deployments and every existing fixture predate the field.
//   - selector == identity → apply. This site is the declared founder.
//   - selector != identity → withhold. Another site founds the DIT; this one
//     receives it by replication.
//   - selector set, identity unset → withhold (decided 2026-09-17, MESH-PLAN
//     Phase 2). Unreadable identity never counts as a match — the same rule
//     ADR-008 applies to CSN evidence. A database that never seeds is loud and
//     recoverable; an ADR-025 glue suffix is silent, permanent, and makes every
//     backup taken from that pod unrestorable.
//
// The two withholding outcomes are distinct values, not one, because their
// latching differs: a site mismatch is permanent by nature, a missing identity
// is a misconfiguration that must stay repairable (see the call site in
// slapddatabase_controller.go step 8).
func TestDecideSeedSite(t *testing.T) {
	tests := []struct {
		name     string
		selector string
		identity string
		want     seedSiteDecision
	}{
		// Compatibility: the field is absent on every pre-ADR-028 CR.
		{"no selector, no identity", "", "", seedSiteApply},
		{"no selector, identity set", "", "site-a", seedSiteApply},
		{"no selector, whitespace identity", "", "   ", seedSiteApply},

		// The founder.
		{"selector matches identity", "site-a", "site-a", seedSiteApply},
		{"selector matches after trimming", " site-a ", "site-a\n", seedSiteApply},

		// A non-founder site: permanent, latchable.
		{"selector names another site", "site-a", "site-b", seedSiteWithhold},
		// Names are compared verbatim apart from surrounding whitespace: a site
		// is whatever the operator chart was given, and folding case here would
		// silently make two distinct SITE_NAME values collide.
		{"case differs is a different site", "site-a", "SITE-A", seedSiteWithhold},

		// Declared founder, but this operator cannot tell whether it is the
		// one. Withhold, and do NOT latch — fixing SITE_NAME must still seed.
		{"selector set, identity unset", "site-a", "", seedSiteUnknownIdentity},
		{"selector set, identity whitespace only", "site-a", " \t ", seedSiteUnknownIdentity},
		{"whitespace-only selector is unset", "   ", "", seedSiteApply},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideSeedSite(tc.selector, tc.identity); got != tc.want {
				t.Errorf("decideSeedSite(%q, %q) = %v, want %v",
					tc.selector, tc.identity, got, tc.want)
			}
		})
	}
}
