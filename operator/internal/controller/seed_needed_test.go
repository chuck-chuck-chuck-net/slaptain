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
	"testing"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// TestSeedNeeded is the ADR-012 boundary control for the DataObserved latch.
//
// ADR-012 reverted verifySeedExists because it re-created seed entries on
// ABSENCE of data — masking real data loss with a fake recovery and re-running
// the multi-pod write race. DataObserved is memory of data's presence, so it is
// exactly the input that could resurrect that behaviour if anything on the
// write path ever consulted it. It must not: seeding is decided by the spec and
// the seed latch alone, and observation state — in either direction — cannot
// move that decision.
//
// The rows that matter are the last two: data observed and then lost (the
// verifySeedExists trigger shape) must not cause a seed, and a founder whose
// data is already visible must not have its first seed suppressed either.
func TestSeedNeeded(t *testing.T) {
	withSeed := &ldapv1alpha1.DatabaseSeedConfig{Entries: []string{"dn: dc=example,dc=org"}}

	tests := []struct {
		name   string
		seed   *ldapv1alpha1.DatabaseSeedConfig
		status ldapv1alpha1.SlapdDatabaseStatus
		want   bool
	}{
		{"no seed spec", nil, ldapv1alpha1.SlapdDatabaseStatus{}, false},
		{"empty seed spec", &ldapv1alpha1.DatabaseSeedConfig{}, ldapv1alpha1.SlapdDatabaseStatus{}, false},
		{"seed spec, not yet applied", withSeed, ldapv1alpha1.SlapdDatabaseStatus{}, true},
		{"seed already applied", withSeed, ldapv1alpha1.SlapdDatabaseStatus{SeedApplied: true}, false},

		// A never-seeded database whose data was observed and is now gone: the
		// exact state verifySeedExists reacted to. No spec.seed, so nothing to
		// apply — and the latch must not invent one.
		{"seedless database that once had data", nil,
			ldapv1alpha1.SlapdDatabaseStatus{DataObserved: true}, false},
		{"restored database", nil,
			ldapv1alpha1.SlapdDatabaseStatus{RestoreApplied: true, DataObserved: true}, false},

		// And the other direction: observing data must not SUPPRESS a founder's
		// first seed either. Withholding is the withhold belt's job (ADR-025
		// decision 2), keyed on a foreign creator's serverID — not on this.
		{"founder seeds even though data is visible", withSeed,
			ldapv1alpha1.SlapdDatabaseStatus{DataObserved: true}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sd := &ldapv1alpha1.SlapdDatabase{
				Spec:   ldapv1alpha1.SlapdDatabaseSpec{Seed: tc.seed},
				Status: tc.status,
			}
			if got := seedNeeded(sd); got != tc.want {
				t.Errorf("seedNeeded(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
