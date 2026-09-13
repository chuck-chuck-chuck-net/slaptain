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

// TestCSNSID pins the sid extraction from a CSN value. The sid field is the
// third '#'-separated component, 3 hex digits (RFC 4533 / slapd csn.c). The
// live-captured glue evidence (ADR-025) carried sid 065 hex = 101 decimal.
func TestCSNSID(t *testing.T) {
	tests := []struct {
		name   string
		csn    string
		want   int
		wantOK bool
	}{
		{"live-captured glue CSN sid 065", "20260913185308.559566Z#000000#065#000000", 0x65, true},
		{"sid 001", "20260913185307.123456Z#000000#001#000000", 1, true},
		{"sid 000 parses (pre-ADR-017 epoch)", "20260101000000.000000Z#000000#000#000000", 0, true},
		{"uppercase hex sid", "20260913185308.559566Z#000000#0FF#000000", 255, true},
		{"missing fields", "20260913185308.559566Z", 0, false},
		{"two fields only", "20260913185308.559566Z#000000", 0, false},
		{"non-hex sid", "20260913185308.559566Z#000000#xyz#000000", 0, false},
		{"empty", "", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := csnSID(tc.csn)
			if ok != tc.wantOK || (ok && got != tc.want) {
				t.Errorf("csnSID(%q) = (%d, %v), want (%d, %v)", tc.csn, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestSeedCreatorIsForeign pins the ADR-025 P2 withhold decision: when pod-0's
// suffix entry was created by a serverID outside this cluster's own sid range
// (serverIDBase+1 .. serverIDBase+replicas — ADR-011/ADR-017), the DIT already
// has a creator elsewhere in the mesh and the local seed must be withheld
// wholesale. When the creator is local (our own earlier, possibly partial,
// seed) or the evidence is unreadable, seeding proceeds — withhold-create
// only, never withhold on absence of evidence.
func TestSeedCreatorIsForeign(t *testing.T) {
	cluster := func(base, replicas int32) *ldapv1alpha1.SlapdCluster {
		sc := &ldapv1alpha1.SlapdCluster{}
		sc.Spec.Replicas = replicas
		sc.Spec.Replication.ServerIDBase = base
		return sc
	}

	tests := []struct {
		name string
		csn  string
		sc   *ldapv1alpha1.SlapdCluster
		want bool
	}{
		// The live incident: a base-0, 3-replica site (local sids 1..3) finds
		// its suffix created by sid 0x65=101 — a peer site's pod. Withhold.
		{"foreign site creator withheld", "20260913185308.559566Z#000000#065#000000",
			cluster(0, 3), true},
		// Our own pod-0 (sid 1) created it — an earlier partial seed run.
		// Proceed with the idempotent per-entry loop.
		{"own pod-0 creator proceeds", "20260913185307.000000Z#000000#001#000000",
			cluster(0, 3), false},
		// Highest local ordinal is still local.
		{"own pod-2 creator proceeds", "20260913185307.000000Z#000000#003#000000",
			cluster(0, 3), false},
		// One past the local range is foreign.
		{"sid just past local range withheld", "20260913185307.000000Z#000000#004#000000",
			cluster(0, 3), true},
		// Non-zero serverIDBase shifts the local range (ADR-011).
		{"foreign below shifted base withheld", "20260913185307.000000Z#000000#001#000000",
			cluster(100, 3), true},
		{"local in shifted range proceeds", "20260913185307.000000Z#000000#066#000000", // 0x66 = 102 = base100+ord1+1
			cluster(100, 3), false},
		// replicas=0 means 1 (the controller's default elsewhere).
		{"replicas zero treated as one", "20260913185307.000000Z#000000#001#000000",
			cluster(0, 0), false},
		// Unknown evidence → proceed (never withhold on a failed read).
		{"unparseable CSN proceeds", "not-a-csn", cluster(0, 3), false},
		{"empty CSN proceeds", "", cluster(0, 3), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := seedCreatorIsForeign(tc.csn, tc.sc); got != tc.want {
				t.Errorf("seedCreatorIsForeign(%q, base=%d, replicas=%d) = %v, want %v",
					tc.csn, tc.sc.Spec.Replication.ServerIDBase, tc.sc.Spec.Replicas, got, tc.want)
			}
		})
	}
}
