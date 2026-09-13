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

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// csnSID extracts the serverID from an entryCSN/contextCSN value
// (YYYYMMDDHHMMSS.µsZ#count#sid#modcount; sid is 3 hex digits — RFC 4533,
// slapd csn.c). ok=false when the CSN does not parse.
func csnSID(csn string) (int, bool) {
	parts := strings.Split(csn, "#")
	if len(parts) < 3 {
		return 0, false
	}
	sid, err := strconv.ParseInt(parts[2], 16, 32)
	if err != nil || sid < 0 {
		return 0, false
	}
	return int(sid), true
}

// seedCreatorIsForeign reports whether the suffix entry visible on pod-0 was
// created by a serverID OUTSIDE this cluster's own sid range — i.e. the DIT
// already has a creator elsewhere in the mesh (the founder site), and this
// cluster's seed must be withheld entirely (ADR-025): seeding the same DNs
// with fresh entryUUIDs is the multi-site seed race.
//
// Withhold-create direction only: unknown evidence (unparseable CSN) returns
// false, falling back to the pre-existing per-entry idempotent seeding — an
// absence that could mean "could not read it" must never suppress the seed.
func seedCreatorIsForeign(entryCSN string, sc *ldapv1alpha1.SlapdCluster) bool {
	sid, ok := csnSID(entryCSN)
	if !ok {
		return false
	}
	replicas := sc.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}
	base := int(sc.Spec.Replication.ServerIDBase)
	// Local sids are serverIDBase+ordinal+1 for ordinal 0..replicas-1
	// (ADR-011/ADR-017, same arithmetic as desiredServerID). A sid outside
	// that range was minted by another cluster in the mesh.
	//
	// Known edge, accepted: after a scale-DOWN, an entry created by a former
	// local pod reads as foreign. Seed only runs before SeedApplied latches —
	// i.e. at the start of a cluster's life — and withholding in that exotic
	// order (scale-down before first seed completes, on a DIT that already has
	// entries) latches SeedApplied on a DIT that demonstrably has a creator,
	// which is the correct outcome anyway.
	return sid < base+1 || sid > base+int(replicas)
}
