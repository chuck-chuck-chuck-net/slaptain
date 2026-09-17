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
	"errors"
	"fmt"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The mesh derivations of ADR-028 §4: from a SlapdMesh's site list plus this
// operator's own site identity, derive the wiring that used to be typed into
// every site's SlapdCluster by hand — serverIDBase, externalPeers, and the
// identity cross-check that makes both safe.
//
// Everything here is pure: slices and strings in, values and errors out, no
// client and no I/O. That is deliberate and not merely tidy. These functions
// decide values that are baked irreversibly into replicated data (a serverID
// ends up inside every CSN that pod writes), so they have to be exhaustively
// testable at unit speed rather than only observable on a three-site lab.
//
// They take the site SLICE rather than a whole SlapdMesh, and the site selector
// as a plain parameter rather than reading it off a SlapdCluster, because
// neither needs the object: the CR field that carries the selector does not
// exist yet (MESH-PLAN Phase 4 adds it) and the seam is honest without it.
//
// Nothing calls these yet. Phase 3 lands the seam; Phase 4 wires it.

var (
	// errSiteUnknown: a site name was not found in the mesh. Always loud —
	// never a silent fallback to the first site, which would hand this site a
	// serverID decade another site is already using.
	errSiteUnknown = errors.New("site is not a member of the mesh")
	// errSiteNotSelected: this site is a mesh member, but the cluster's site
	// selector does not include it — so this cluster does not span this site.
	// Distinct from errSiteUnknown because the caller's remedy differs: fix the
	// selector, not the mesh.
	errSiteNotSelected = errors.New("site is not in this cluster's site selector")
	// errSiteIdentity: this operator has no site identity (SITE_NAME unset).
	// Legal in general — it means "no mesh features" (ADR-028 §4) — but not
	// when something has asked for a mesh derivation.
	errSiteIdentity = errors.New("no site identity configured")
	// errMeshSites: the site list itself is unusable — empty, or carrying a
	// blank or duplicated name, or a missing, negative or duplicated
	// serverIDIndex. Both duplicate classes are two sites claiming one
	// serverID decade: the collision ADR-028 §4 names as the first thing to
	// verify.
	errMeshSites = errors.New("malformed mesh site list")
	// errServerIDCeiling: the decade-per-site scheme has run out of room. See
	// maxSiteIndex.
	errServerIDCeiling = errors.New("site index exceeds the serverID decade ceiling")
)

const (
	// serverIDDecade is the per-site stride. It is NOT a free choice: it
	// reproduces, value for value, the site_idx*100 arithmetic that
	// tests/e2e.sh has been applying by hand, and that every existing
	// multi-site deployment is therefore already running.
	//
	// olcServerID is baked into every CSN a pod writes (timestamp#count#sid#mod).
	// Changing what this derivation returns for a site that already exists
	// would file that site's future writes under a different sid from its
	// history, and per-site CSN tracking would stop being comparable with
	// itself. A derivation that changes a running pod's serverID is a defect,
	// not a migration (MESH-PLAN hazard 2, ADR-017).
	serverIDDecade = 100

	// maxSiteIndex is where the scheme runs out. slapd caps olcServerID at 4095
	// and SlapdCluster.spec.replication.serverIDBase at 4094, so the last index
	// that can be given a whole decade is 40 (base 4000, leaving pod serverIDs
	// 4001..4095 — 95 pods, far more than a site will run).
	//
	// It is a bound on the INDEX, not on the number of sites: indices are
	// sparse-legal (a decommissioned site's index is retired rather than
	// reused), so a two-site mesh can breach this just as easily as a 41-site
	// one.
	//
	// A breach is refused loudly rather than wrapped, clamped or shared: every
	// alternative silently gives two sites one decade, which is the exact
	// failure mode this whole scheme exists to prevent. A mesh that genuinely
	// outgrows 41 decades needs a narrower stride, and that is a migration with
	// a plan, not a fallback inside a derivation.
	maxSiteIndex = 4094 / serverIDDecade // 40
)

// meshSiteIndex resolves a site name to its POSITION in the slice, applying the
// shared normalisation rules and validating the whole site list on the way.
//
// The position is an addressing detail — where the entry sits — and is
// deliberately not the site's serverID slot; that is the serverIDIndex field
// the validation below enforces. Callers that want the slot read the field
// through this position.
//
// The list is validated wholesale, names and indices together, so a malformed
// mesh fails every derivation rather than half of them: a mesh with two sites
// on one decade is broken for the peer set too, even though peers never read an
// index.
//
// Comparison semantics are decideSeedSite's, deliberately: both sides trimmed,
// case NOT folded (ADR-028 §3 amendment). A site is whatever the operator chart
// was given; folding would let two distinct SITE_NAME values collide, which is
// precisely the decade collision §4 warns about.
func meshSiteIndex(sites []ldapv1alpha1.MeshSite, name string) (int, error) {
	if len(sites) == 0 {
		return 0, fmt.Errorf("%w: no sites declared", errMeshSites)
	}
	seen := make(map[string]struct{}, len(sites))
	seenIdx := make(map[int32]string, len(sites))
	idx := -1
	want := normalizeSiteName(name)
	for i, s := range sites {
		n := normalizeSiteName(s.Name)
		if n == "" {
			return 0, fmt.Errorf("%w: site %d has an empty name", errMeshSites, i)
		}
		if _, dup := seen[n]; dup {
			return 0, fmt.Errorf("%w: site %q is declared twice — two sites claiming "+
				"one identity share one serverID decade", errMeshSites, n)
		}
		seen[n] = struct{}{}

		// The index is required and never inferred. Defaulting it — to the
		// position, or to anything else — would put back the silent
		// renumbering the explicit field exists to remove, and would do it on
		// precisely the edit nobody reviews carefully.
		if s.ServerIDIndex == nil {
			return 0, fmt.Errorf("%w: site %q has no serverIDIndex; it is required "+
				"and is never inferred from list position", errMeshSites, n)
		}
		si := *s.ServerIDIndex
		if si < 0 {
			return 0, fmt.Errorf("%w: site %q has a negative serverIDIndex (%d)", errMeshSites, n, si)
		}
		if other, dup := seenIdx[si]; dup {
			return 0, fmt.Errorf("%w: sites %q and %q both claim serverIDIndex %d — "+
				"they would share one serverID decade, and slapd validates nothing: "+
				"contextCSN keeps the highest CSN per serverID, so each site's writes "+
				"read as already-seen at the other", errMeshSites, other, n, si)
		}
		seenIdx[si] = n

		if n == want && idx < 0 {
			idx = i
		}
	}
	// The whole list is validated before the lookup verdict, so a malformed
	// mesh reports as malformed even when the requested site happens to resolve.
	if want == "" {
		return 0, fmt.Errorf("%w: set siteName on the operator chart (SITE_NAME)", errSiteIdentity)
	}
	if idx < 0 {
		return 0, fmt.Errorf("%w: %q (names are compared verbatim, case included)", errSiteUnknown, want)
	}
	return idx, nil
}

// The operator's access to the new kind. Read-only and permanently so: a mesh
// has no single writer by construction, so a corrective write would be action
// on state this operator does not own — ADR-026 R2, restated for the mesh layer
// in ADR-028 §6 ("report and stall"). Status is included because ADR-028 §6's
// standing parity condition will land there; the spec is never written.
//
// +kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapdmeshes,verbs=get;list;watch
// +kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapdmeshes/status,verbs=get;update;patch

// validateSelfSite cross-checks this operator's own site identity against the
// mesh's site list — the Phase 1 resolver's promotion, now that sites[] exists
// to check against.
//
// A configured identity that is absent from the mesh is an error the caller
// must surface, never a silent default to some site. Defaulting is how two
// sites end up sharing a serverID decade, and that failure is invisible: slapd
// validates nothing, contextCSN tracks the highest CSN per serverID, and the
// only symptom is that some writes never propagate.
func validateSelfSite(sites []ldapv1alpha1.MeshSite, identity string) error {
	_, err := meshSiteIndex(sites, identity)
	return err
}

// serverIDBaseForSite derives the site's serverID decade from its DECLARED
// serverIDIndex: index × 100, with ADR-017's serverIDBase + ordinal + 1 applied
// downstream unchanged. Index 2, pod 3 → 203, readable straight out of a CSN.
//
// The number comes from the field and never from the site's position in
// spec.sites. That is the point: olcServerID is baked into every CSN a pod has
// written, so a site whose number moves files its future writes under a
// different sid from its history and per-site CSN tracking stops being
// comparable with itself. Positional derivation would trigger that on an edit
// nobody thinks twice about — inserting a site in the middle, or a formatter
// sorting the list. Bound to a field, spec.sites is freely reorderable and the
// identity is immovable.
//
// A three-site mesh declaring indices 0/1/2 therefore yields 0/100/200 — the
// values every existing multi-site deployment is already running, so adopting
// the mesh costs no migration.
//
// Indices are mesh-wide, never per-cluster-selector: two clusters on one mesh
// spanning different subsets must still agree about who owns decade 100. (Two
// clusters may safely share serverID values with EACH OTHER — a CSN is only
// compared within one replication topology, and clusters with different
// suffixes never exchange CSNs. What must not happen is two SITES sharing a
// decade inside one topology.)
//
// See serverIDDecade for why the stride is not a free choice, and maxSiteIndex
// for where it stops.
func serverIDBaseForSite(sites []ldapv1alpha1.MeshSite, identity string) (int32, error) {
	pos, err := meshSiteIndex(sites, identity)
	if err != nil {
		return 0, err
	}
	// meshSiteIndex has already proven the pointer is non-nil, non-negative and
	// unique across the mesh.
	idx := *sites[pos].ServerIDIndex
	if idx > maxSiteIndex {
		return 0, fmt.Errorf("%w: site %q declares serverIDIndex %d; the decade-per-site "+
			"scheme allows at most %d before olcServerID's 4095 limit",
			errServerIDCeiling, normalizeSiteName(identity), idx, maxSiteIndex)
	}
	return idx * serverIDDecade, nil
}

// externalPeersForSite derives the cross-site peer set for one site: every
// other site the cluster spans, addressed by ADR-007 dynamic discovery.
//
// The output reproduces what tests/e2e.sh builds by hand today — per-peer name,
// port 1025, the peer's CA Secret as tlsSecretName, and the peer's kubeconfig
// Secret for discovery — so Phase 4 can be checked by comparing cn=config
// against a hand-configured cluster rather than by reading the code twice.
//
// Three things it deliberately does not set:
//
//   - bindDN / bindPasswordSecretName. The operator derives the bind identity
//     per database (ADR-027's node-local cn=repl-<db>,cn=slaptain-auth plus that
//     database's replication password). A peer-level value spans every
//     SlapdDatabase and so can be correct for at most one of them — ADR-019 R9's
//     axis argument. Those fields survive as the ADR-011 override for foreign
//     sources.
//   - replicasPerPeer. Left nil so the CRD default (1, the diagonal) applies,
//     exactly as an unset field does today. Writing the default in explicitly
//     would make the derived object differ textually from the hand-written one
//     for no behavioural gain.
//   - uri / podAddresses. A mesh describes sites, and a site is reached through
//     its API server; static addressing is the pre-discovery path and stays
//     hand-configured.
//
// selector is the cluster's site subset (MESH-PLAN Phase 4's `sites` field).
// Empty or nil means the whole mesh, which is the default a cluster gets when
// it says nothing. The selector narrows the PEERS only — it never touches the
// serverID derivation above.
//
// Peer ORDER follows the mesh, not the selector, so the derived list is stable
// across reconciles and an unchanged mesh produces an unchanged cn=config.
func externalPeersForSite(sites []ldapv1alpha1.MeshSite, selector []string, identity string) ([]ldapv1alpha1.ExternalPeer, error) {
	selfIdx, err := meshSiteIndex(sites, identity)
	if err != nil {
		return nil, err
	}

	// Resolve the selector against the mesh first: a selector naming a site the
	// mesh does not have is a typo that would otherwise silently shrink the
	// peer set, and a shrunken peer set is a half-connected topology that still
	// looks healthy on both sides of the links that do exist.
	var selected map[int]struct{}
	if len(selector) > 0 {
		selected = make(map[int]struct{}, len(selector))
		for _, name := range selector {
			idx, err := meshSiteIndex(sites, name)
			if err != nil {
				return nil, fmt.Errorf("site selector: %w", err)
			}
			selected[idx] = struct{}{}
		}
		if _, ok := selected[selfIdx]; !ok {
			return nil, fmt.Errorf("%w: %q spans %v", errSiteNotSelected,
				normalizeSiteName(identity), selector)
		}
	}

	var peers []ldapv1alpha1.ExternalPeer
	for i, s := range sites {
		if i == selfIdx {
			continue // never peer with yourself: a stanza pointing at its own pods is a loop
		}
		if selected != nil {
			if _, ok := selected[i]; !ok {
				continue
			}
		}
		kubeconfig := s.KubeconfigSecretFor()
		peers = append(peers, ldapv1alpha1.ExternalPeer{
			Name:          normalizeSiteName(s.Name),
			Port:          externalPeerLDAPSPort,
			TLSSecretName: s.CASecretNameFor(),
			Discovery: &ldapv1alpha1.ExternalPeerDiscovery{
				KubeconfigSecret: kubeconfig,
			},
		})
	}
	return peers, nil
}

// externalPeerLDAPSPort is slapd's non-privileged LDAPS container port — the
// port every slaptain pod listens on and the one tests/e2e.sh has always
// written into its peer specs. It matches the ExternalPeer.port CRD default;
// it is set explicitly here so the derived peer is complete on its own rather
// than depending on API defaulting having run.
const externalPeerLDAPSPort = 1025
