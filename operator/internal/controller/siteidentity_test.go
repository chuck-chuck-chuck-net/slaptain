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

// TestNormalizeSiteName pins the ADR-028 §4 self-identity resolution: the
// operator's site name is the single per-site fact in the system, supplied by
// its own installation config rather than by any CR. Absent is legal and means
// "no mesh features" — never an error, never a guessed default.
func TestNormalizeSiteName(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		// A configured site, as the operator chart renders it.
		{"plain site name", "site-a", "site-a"},
		// Env var absent entirely: os.Getenv yields "". No mesh features.
		{"unset", "", ""},
		// A values file with a stray trailing newline or space must not produce
		// a name that compares unequal to every real site.
		{"surrounding whitespace trimmed", "  site-b\n", "site-b"},
		{"leading whitespace trimmed", "\tsite-c", "site-c"},
		// Whitespace-only is indistinguishable from an operator mistake; treat
		// it as unset rather than as a site literally named " ".
		{"whitespace only treated as unset", "   ", ""},
		{"newline only treated as unset", "\n", ""},
		// Interior characters are not our business at this phase: no SlapdMesh
		// type exists, so there is no sites[] to validate against (MESH-PLAN
		// Phase 3). Pass the name through verbatim.
		{"interior spacing preserved verbatim", "site a", "site a"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeSiteName(tc.raw); got != tc.want {
				t.Errorf("normalizeSiteName(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestResolveSiteName_Env covers the thin env-reading wrapper: a set value is
// trimmed, an unset/empty one stays empty.
func TestResolveSiteName_Env(t *testing.T) {
	t.Setenv("SITE_NAME", " site-a ")
	if got := ResolveSiteName(); got != "site-a" {
		t.Errorf("ResolveSiteName() with SITE_NAME=%q = %q, want %q", " site-a ", got, "site-a")
	}
	t.Setenv("SITE_NAME", "")
	if got := ResolveSiteName(); got != "" {
		t.Errorf("ResolveSiteName() with SITE_NAME empty = %q, want %q", got, "")
	}
}
