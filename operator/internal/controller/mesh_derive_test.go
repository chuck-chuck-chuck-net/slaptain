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
	"reflect"
	"testing"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// si spells an explicit serverIDIndex. The field is a pointer so that "not
// set" stays distinguishable from index 0 in Go — the API server enforces
// presence, but a Go caller writing a MeshSite literal would otherwise get
// index 0 by accident, and index 0 is a real site's decade.
func si(i int32) *int32 { return &i }

// threeSiteMesh is the golden topology: the shape a three-site lab runs today,
// with the site names the e2e fixture assigns (site-1..site-N, deliberately
// independent of any lab's context names — MESH-PLAN Phase 2b) and the indices
// 0/1/2 that reproduce its live serverID decades 0/100/200.
func threeSiteMesh() []ldapv1alpha1.MeshSite {
	return []ldapv1alpha1.MeshSite{
		{Name: "site-1", ServerIDIndex: si(0)},
		{Name: "site-2", ServerIDIndex: si(1)},
		{Name: "site-3", ServerIDIndex: si(2)},
	}
}

// TestServerIDBaseForSite pins MESH-PLAN hazard 2, and it is the single most
// load-bearing assertion in this milestone.
//
// olcServerID is baked into every CSN ever written (timestamp#count#sid#mod).
// If this derivation hands a RUNNING pod a different serverID than it has
// today, its historical CSNs stay filed under the old sid while new ones appear
// under a new one, and per-site CSN tracking stops being comparable with
// itself. So the derivation is not free to pick a scheme: it must reproduce,
// value for value, what tests/e2e.sh computes by hand today —
// site_idx * 100, with ADR-017's serverIDBase + ordinal + 1 applied downstream
// unchanged. A derivation that changes an existing pod's serverID is a defect,
// not a migration.
//
// The golden case is therefore the three-site lab's live values: first site 0,
// second 100, third 200.
func TestServerIDBaseForSite(t *testing.T) {
	// reordered is the golden mesh with its entries shuffled and nothing else
	// changed. Every site must keep the decade it had — that is the whole point
	// of the explicit index.
	reordered := []ldapv1alpha1.MeshSite{
		{Name: "site-3", ServerIDIndex: si(2)},
		{Name: "site-1", ServerIDIndex: si(0)},
		{Name: "site-2", ServerIDIndex: si(1)},
	}

	// sparse: index 1 has been decommissioned and its decade retired rather
	// than reused, and a later site was added at 9. Gaps are legal.
	sparse := []ldapv1alpha1.MeshSite{
		{Name: "site-a", ServerIDIndex: si(0)},
		{Name: "site-c", ServerIDIndex: si(5)},
		{Name: "site-d", ServerIDIndex: si(9)},
	}

	tests := []struct {
		name    string
		sites   []ldapv1alpha1.MeshSite
		self    string
		want    int32
		wantErr error
	}{
		// Golden case — the values a three-site lab is running right now.
		{"first site keeps decade 0", threeSiteMesh(), "site-1", 0, nil},
		{"second site gets decade 100", threeSiteMesh(), "site-2", 100, nil},
		{"third site gets decade 200", threeSiteMesh(), "site-3", 200, nil},

		// A single-site mesh is the degenerate case and must stay at 0, which
		// is also what every standalone cluster has today.
		{"single-site mesh", []ldapv1alpha1.MeshSite{{Name: "solo", ServerIDIndex: si(0)}}, "solo", 0, nil},

		// Identity handling matches decideSeedSite exactly: trimmed, never
		// case-folded. Folding would let two distinct SITE_NAME values collide
		// their decades, which is the precise failure ADR-028 §4 warns about.
		{"identity is trimmed", threeSiteMesh(), "  site-2\n", 100, nil},
		{"identity is not case-folded", threeSiteMesh(), "SITE-2", 0, errSiteUnknown},

		// A configured identity absent from the mesh is LOUD. Never a silent
		// default to the first site: that would hand this site decade 0, which
		// some other site is already using, and collide the two.
		{"self absent from mesh", threeSiteMesh(), "site-4", 0, errSiteUnknown},
		{"empty identity", threeSiteMesh(), "", 0, errSiteIdentity},
		{"whitespace identity", threeSiteMesh(), "   ", 0, errSiteIdentity},

		// A malformed mesh is rejected rather than silently indexed: duplicate
		// names are two sites claiming one identity, one decade.
		{
			"duplicate site names",
			[]ldapv1alpha1.MeshSite{{Name: "site-1", ServerIDIndex: si(0)}, {Name: "site-1", ServerIDIndex: si(1)}},
			"site-1", 0, errMeshSites,
		},
		{
			"empty site name in mesh",
			[]ldapv1alpha1.MeshSite{{Name: "site-1", ServerIDIndex: si(0)}, {Name: " ", ServerIDIndex: si(1)}},
			"site-1", 0, errMeshSites,
		},
		{"empty mesh", nil, "site-1", 0, errMeshSites},

		// The regression that motivates the explicit index. spec.sites is a
		// YAML list; people reorder those, formatters sort them, a merge moves
		// an entry. Under positional derivation each of those silently
		// renumbers every site after the edit — and a renumbered site files its
		// future writes under a different sid from its entire history, which is
		// unrecoverable by design (MESH-PLAN hazard 2). Bound to a field, the
		// same edit is a no-op.
		{"reordered mesh: first site keeps 0", reordered, "site-1", 0, nil},
		{"reordered mesh: second site keeps 100", reordered, "site-2", 100, nil},
		{"reordered mesh: third site keeps 200", reordered, "site-3", 200, nil},

		// Gaps are legal and are the correct way to remove a site: retire the
		// index, never reuse it, because a reused index inherits the retired
		// site's CSN lineage.
		{"sparse indices, first", sparse, "site-a", 0, nil},
		{"sparse indices, middle", sparse, "site-c", 500, nil},
		{"sparse indices, last", sparse, "site-d", 900, nil},

		// Two sites on one index is the same class of fault as two sites on one
		// name, and it is invisible at runtime: slapd validates nothing, and
		// contextCSN keeps the highest CSN per serverID, so the colliding sites
		// merge into one bucket and read each other's writes as already-seen.
		{
			"duplicate indices",
			[]ldapv1alpha1.MeshSite{
				{Name: "site-1", ServerIDIndex: si(3)},
				{Name: "site-2", ServerIDIndex: si(3)},
			},
			"site-1", 0, errMeshSites,
		},
		{
			// Required, not defaulted-to-position: a missing index is rejected
			// rather than quietly filled in, because filling it in from the
			// position is exactly the fragility the field exists to remove.
			"missing index",
			[]ldapv1alpha1.MeshSite{
				{Name: "site-1", ServerIDIndex: si(0)},
				{Name: "site-2"},
			},
			"site-1", 0, errMeshSites,
		},
		{
			"negative index",
			[]ldapv1alpha1.MeshSite{{Name: "site-1", ServerIDIndex: si(-1)}},
			"site-1", 0, errMeshSites,
		},

		// The ceiling, stated rather than left implicit — and keyed off the
		// INDEX, not the site count, since the list is sparse-legal.
		{
			"last index below the ceiling",
			[]ldapv1alpha1.MeshSite{{Name: "site-1", ServerIDIndex: si(0)}, {Name: "far", ServerIDIndex: si(40)}},
			"far", 4000, nil,
		},
		{
			"first index past the ceiling",
			[]ldapv1alpha1.MeshSite{{Name: "site-1", ServerIDIndex: si(0)}, {Name: "far", ServerIDIndex: si(41)}},
			"far", 0, errServerIDCeiling,
		},
		{
			// A two-site mesh can still breach the ceiling: the limit is on the
			// index, and nothing about the list length bounds it.
			"tiny mesh, huge index",
			[]ldapv1alpha1.MeshSite{{Name: "solo-far", ServerIDIndex: si(9000)}},
			"solo-far", 0, errServerIDCeiling,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := serverIDBaseForSite(tc.sites, tc.self)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("serverIDBase = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestValidateSelfSite pins the Phase 1 resolver's promotion: now that sites[]
// exists, an identity can be cross-checked against it. A configured identity
// absent from the mesh is an error the caller must surface, never a silent
// default to the first site — defaulting is how a serverID decade gets shared
// by two sites quietly.
//
// An UNSET identity is a different thing from a wrong one and keeps Phase 1's
// meaning: "no mesh features", legal for every single-site deployment. It is
// still an error here because a cluster that named a meshRef has asked for mesh
// behaviour and cannot have it — but it is a distinguishable one, so the caller
// can word the condition as "set siteName on the operator chart" rather than
// "this site is not in the mesh".
func TestValidateSelfSite(t *testing.T) {
	tests := []struct {
		name    string
		sites   []ldapv1alpha1.MeshSite
		self    string
		wantErr error
	}{
		{"member", threeSiteMesh(), "site-2", nil},
		{"member after trimming", threeSiteMesh(), " site-3 ", nil},
		{"single-site mesh", []ldapv1alpha1.MeshSite{{Name: "solo", ServerIDIndex: si(0)}}, "solo", nil},
		{"not a member", threeSiteMesh(), "site-4", errSiteUnknown},
		{"case mismatch is not a member", threeSiteMesh(), "Site-1", errSiteUnknown},
		{"identity unset", threeSiteMesh(), "", errSiteIdentity},
		{"identity whitespace only", threeSiteMesh(), "\t\n", errSiteIdentity},
		{"malformed mesh", []ldapv1alpha1.MeshSite{{Name: "a", ServerIDIndex: si(0)}, {Name: "a", ServerIDIndex: si(1)}}, "a", errMeshSites},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateSelfSite(tc.sites, tc.self); !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// peer is the expected shape, spelled out once. It reproduces exactly what
// tests/e2e.sh's setup_slapd_clusters builds by hand today in discovery mode:
// per-peer name, port 1025, the peer's CA Secret, and the peer's kubeconfig
// Secret for ADR-007 dynamic discovery. No bindDN and no bindPasswordSecretName
// — the operator derives the bind identity per database, so a peer-level value
// spans every SlapdDatabase and can be right for at most one of them
// (ADR-019 R9's axis argument).
func peer(name, caSecret, kubeconfigSecret string) ldapv1alpha1.ExternalPeer {
	return ldapv1alpha1.ExternalPeer{
		Name:          name,
		Port:          1025,
		TLSSecretName: caSecret,
		Discovery: &ldapv1alpha1.ExternalPeerDiscovery{
			KubeconfigSecret: ldapv1alpha1.KubeconfigSecretRef{Name: kubeconfigSecret},
		},
	}
}

// TestExternalPeersForSite pins the peer set a site derives from the mesh.
//
// Three properties, each of which has already cost somebody a debugging session
// somewhere:
//
//   - self is excluded. A syncrepl stanza pointing at its own pods is a loop.
//   - the selector narrows, and it narrows the PEERS only. A cluster may span a
//     subset of the mesh (cluster X over A+B, cluster Y over A+B+C, one mesh —
//     ADR-028 §1), but the serverID decade still comes from the site's index in
//     the MESH, not in the selector, or two clusters on one mesh would disagree
//     about who owns decade 100.
//   - mesh order is preserved, so the derived list is stable across reconciles
//     and a diff of cn=config stays empty.
//
// The peer NAME is the mesh site name verbatim. It is not cosmetic: the
// operator mounts each peer's CA at /etc/openldap/tls/peers/<name>/ca.crt and
// that path is written into the syncrepl stanza's tls_cacert, so the name is
// baked into cn=config.
func TestExternalPeersForSite(t *testing.T) {
	tests := []struct {
		name     string
		sites    []ldapv1alpha1.MeshSite
		selector []string
		self     string
		want     []ldapv1alpha1.ExternalPeer
		wantErr  error
	}{
		{
			name:  "golden three-site mesh, middle site",
			sites: threeSiteMesh(),
			self:  "site-2",
			want: []ldapv1alpha1.ExternalPeer{
				peer("site-1", "site-1-ca", "site-1-kubeconfig"),
				peer("site-3", "site-3-ca", "site-3-kubeconfig"),
			},
		},
		{
			name:  "golden three-site mesh, first site",
			sites: threeSiteMesh(),
			self:  "site-1",
			want: []ldapv1alpha1.ExternalPeer{
				peer("site-2", "site-2-ca", "site-2-kubeconfig"),
				peer("site-3", "site-3-ca", "site-3-kubeconfig"),
			},
		},
		{
			// A single-site mesh has no peers at all — and that is an empty
			// list, not an error. It is exactly today's single-site cluster.
			name:  "single-site mesh has no peers",
			sites: []ldapv1alpha1.MeshSite{{Name: "solo", ServerIDIndex: si(0)}},
			self:  "solo",
			want:  nil,
		},
		{
			name:     "selector narrows the peer set",
			sites:    threeSiteMesh(),
			selector: []string{"site-1", "site-2"},
			self:     "site-2",
			want: []ldapv1alpha1.ExternalPeer{
				peer("site-1", "site-1-ca", "site-1-kubeconfig"),
			},
		},
		{
			// Selector order must not leak into the stanza order: the mesh is
			// the authority on ordering.
			name:     "selector order does not reorder peers",
			sites:    threeSiteMesh(),
			selector: []string{"site-3", "site-1"},
			self:     "site-1",
			want: []ldapv1alpha1.ExternalPeer{
				peer("site-3", "site-3-ca", "site-3-kubeconfig"),
			},
		},
		{
			// Explicit Secret names win over the convention — the point of
			// having the fields at all (trust-manager / External Secrets name
			// their outputs, ADR-028 §7).
			name: "explicit secret names override the convention",
			sites: []ldapv1alpha1.MeshSite{
				{Name: "site-1", ServerIDIndex: si(0), CASecretName: "bundle-a", KubeconfigSecret: &ldapv1alpha1.KubeconfigSecretRef{Name: "kc-a", Key: "config"}},
				{Name: "site-2", ServerIDIndex: si(1)},
			},
			self: "site-2",
			want: []ldapv1alpha1.ExternalPeer{
				{
					Name:          "site-1",
					Port:          1025,
					TLSSecretName: "bundle-a",
					Discovery: &ldapv1alpha1.ExternalPeerDiscovery{
						KubeconfigSecret: ldapv1alpha1.KubeconfigSecretRef{Name: "kc-a", Key: "config"},
					},
				},
			},
		},
		{
			// The separation the explicit index buys, asserted directly: peers
			// are derived from NAMES and Secret references, never from the
			// index. Same three sites, same list order, wildly different
			// indices — byte-identical peer set. (The index still has to be
			// well-formed, because a malformed site list is rejected wholesale
			// rather than half-used; that is the "malformed mesh" case below.)
			name: "peer set does not depend on the indices",
			sites: []ldapv1alpha1.MeshSite{
				{Name: "site-1", ServerIDIndex: si(7)},
				{Name: "site-2", ServerIDIndex: si(0)},
				{Name: "site-3", ServerIDIndex: si(39)},
			},
			self: "site-2",
			want: []ldapv1alpha1.ExternalPeer{
				peer("site-1", "site-1-ca", "site-1-kubeconfig"),
				peer("site-3", "site-3-ca", "site-3-kubeconfig"),
			},
		},
		{
			// Reordering the list DOES reorder the peers, on purpose: the
			// stanza order follows the list so that an unchanged mesh keeps
			// producing an unchanged cn=config. What it must not do is change
			// any site's identity — that is TestServerIDBaseForSite's job.
			name: "peer order follows the list, not the index",
			sites: []ldapv1alpha1.MeshSite{
				{Name: "site-3", ServerIDIndex: si(2)},
				{Name: "site-2", ServerIDIndex: si(1)},
				{Name: "site-1", ServerIDIndex: si(0)},
			},
			self: "site-2",
			want: []ldapv1alpha1.ExternalPeer{
				peer("site-3", "site-3-ca", "site-3-kubeconfig"),
				peer("site-1", "site-1-ca", "site-1-kubeconfig"),
			},
		},
		{
			// A selector that does not contain this site is not an empty peer
			// set — it means this cluster does not belong here at all, and the
			// caller has to decide that (Phase 4), loudly.
			name:     "self not in selector",
			sites:    threeSiteMesh(),
			selector: []string{"site-1", "site-3"},
			self:     "site-2",
			wantErr:  errSiteNotSelected,
		},
		{
			name:     "selector names a site the mesh does not have",
			sites:    threeSiteMesh(),
			selector: []string{"site-1", "site-9"},
			self:     "site-1",
			wantErr:  errSiteUnknown,
		},
		{"self absent from mesh", threeSiteMesh(), nil, "site-4", nil, errSiteUnknown},
		{"identity unset", threeSiteMesh(), nil, "", nil, errSiteIdentity},
		{
			"malformed mesh",
			[]ldapv1alpha1.MeshSite{{Name: "a", ServerIDIndex: si(0)}, {Name: "a", ServerIDIndex: si(1)}}, nil, "a", nil, errMeshSites,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := externalPeersForSite(tc.sites, tc.selector, tc.self)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("peers =\n  %+v\nwant\n  %+v", got, tc.want)
			}
		})
	}
}

// TestMeshSiteSecretConventions pins the documented defaults on their own, so
// the convention is a stated contract rather than an accident of the peer
// builder. The default is what makes a mesh spec short; the explicit field is
// what makes it compatible with a platform that names Secrets its own way.
func TestMeshSiteSecretConventions(t *testing.T) {
	bare := ldapv1alpha1.MeshSite{Name: "site-1"}
	if got := bare.CASecretNameFor(); got != "site-1-ca" {
		t.Errorf("default CA secret = %q, want %q", got, "site-1-ca")
	}
	if got := bare.KubeconfigSecretFor(); got.Name != "site-1-kubeconfig" || got.Key != "" {
		t.Errorf("default kubeconfig ref = %+v, want name site-1-kubeconfig and the CRD-defaulted key", got)
	}

	named := ldapv1alpha1.MeshSite{
		Name:             "site-1",
		CASecretName:     "trust-bundle",
		KubeconfigSecret: &ldapv1alpha1.KubeconfigSecretRef{Name: "remote-kc", Key: "value"},
	}
	if got := named.CASecretNameFor(); got != "trust-bundle" {
		t.Errorf("explicit CA secret = %q, want %q", got, "trust-bundle")
	}
	if got := named.KubeconfigSecretFor(); got.Name != "remote-kc" || got.Key != "value" {
		t.Errorf("explicit kubeconfig ref = %+v, want remote-kc/value", got)
	}

	// A half-specified ref keeps the convention for the part it omits.
	half := ldapv1alpha1.MeshSite{Name: "site-2", KubeconfigSecret: &ldapv1alpha1.KubeconfigSecretRef{Key: "k"}}
	if got := half.KubeconfigSecretFor(); got.Name != "site-2-kubeconfig" || got.Key != "k" {
		t.Errorf("half-specified ref = %+v, want site-2-kubeconfig/k", got)
	}
}
