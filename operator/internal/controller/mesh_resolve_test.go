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
	"strings"
	"testing"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// meshSpec wraps the golden three-site topology into a whole SlapdMeshSpec,
// with the pod-routed network the lab runs on.
func meshSpec() *ldapv1alpha1.SlapdMeshSpec {
	return &ldapv1alpha1.SlapdMeshSpec{
		Sites: threeSiteMesh(),
		Network: &ldapv1alpha1.ReplicationNetworkConfig{
			Mode: ldapv1alpha1.NetworkModePodRouted,
		},
	}
}

// peersForSite2 is what a cluster at site-2 must end up with: the OTHER two
// sites, in mesh order, addressed by ADR-007 discovery. Written out literally
// rather than by calling externalPeersForSite, so that a change in the
// derivation has to be re-approved here instead of silently agreeing with
// itself.
func peersForSite2() []ldapv1alpha1.ExternalPeer {
	return []ldapv1alpha1.ExternalPeer{
		{
			Name: "site-1",
			// The site's declared serverIDIndex, which is also its slot in the
			// RID matrix's remote block (ADR-031 decision 4). site-1 declares
			// 0 and site-3 declares 2 in threeSiteMesh; a wiring that dropped
			// the field would leave every cross-site stanza without a RID.
			SiteIndex:     new(int32(0)),
			Port:          1025,
			TLSSecretName: "site-1-ca",
			Discovery: &ldapv1alpha1.ExternalPeerDiscovery{
				KubeconfigSecret: ldapv1alpha1.KubeconfigSecretRef{Name: "site-1-kubeconfig"},
			},
		},
		{
			Name:          "site-3",
			SiteIndex:     new(int32(2)),
			Port:          1025,
			TLSSecretName: "site-3-ca",
			Discovery: &ldapv1alpha1.ExternalPeerDiscovery{
				KubeconfigSecret: ldapv1alpha1.KubeconfigSecretRef{Name: "site-3-kubeconfig"},
			},
		},
	}
}

// TestDeriveClusterWiring is the Phase 4 decision table.
//
// The resolution decides values that are baked irreversibly into replicated
// data — a serverID ends up inside every CSN the pod writes, and a peer name
// ends up as a directory component of the CA mount path and therefore inside
// every external syncrepl stanza's tls_cacert. So the decision is a pure
// function and the controller is a thin shell over it, and every branch is
// pinned here at unit speed rather than only observable on a three-site lab.
//
// The first case is the compatibility invariant and is not negotiable: with no
// meshRef, the resolution derives NOTHING. Every deployment that exists today
// is that case.
func TestDeriveClusterWiring(t *testing.T) {
	tests := []struct {
		name string
		mesh *ldapv1alpha1.SlapdMeshSpec
		spec ldapv1alpha1.SlapdClusterSpec
		self string

		want       *meshWiring
		wantReason string
	}{
		{
			// The compatibility invariant. An unset meshRef means the operator
			// keeps its hands off the spec entirely — not "derives defaults",
			// not "derives an empty peer set", but derives nothing at all, so
			// there is no path by which mesh code can perturb a cluster that
			// never asked for it.
			name: "meshRef unset derives nothing",
			mesh: meshSpec(),
			spec: ldapv1alpha1.SlapdClusterSpec{},
			self: "site-2",
			want: nil,
		},
		{
			name: "valid mesh derives base, peers and network",
			mesh: meshSpec(),
			spec: ldapv1alpha1.SlapdClusterSpec{MeshRef: "mesh"},
			self: "site-2",
			want: &meshWiring{
				ServerIDBase:  16,
				ExternalPeers: peersForSite2(),
				Network:       &ldapv1alpha1.ReplicationNetworkConfig{Mode: ldapv1alpha1.NetworkModePodRouted},
			},
		},
		{
			// Golden: the lab's live stride slots. A derivation that moves any of
			// these is a defect, not a migration (MESH-PLAN hazard 2).
			name: "first site keeps stride slot 0",
			mesh: meshSpec(),
			spec: ldapv1alpha1.SlapdClusterSpec{MeshRef: "mesh"},
			self: "site-1",
			want: &meshWiring{
				ServerIDBase: 0,
				ExternalPeers: []ldapv1alpha1.ExternalPeer{
					{Name: "site-2", SiteIndex: new(int32(1)), Port: 1025, TLSSecretName: "site-2-ca",
						Discovery: &ldapv1alpha1.ExternalPeerDiscovery{
							KubeconfigSecret: ldapv1alpha1.KubeconfigSecretRef{Name: "site-2-kubeconfig"},
						}},
					{Name: "site-3", SiteIndex: new(int32(2)), Port: 1025, TLSSecretName: "site-3-ca",
						Discovery: &ldapv1alpha1.ExternalPeerDiscovery{
							KubeconfigSecret: ldapv1alpha1.KubeconfigSecretRef{Name: "site-3-kubeconfig"},
						}},
				},
				Network: &ldapv1alpha1.ReplicationNetworkConfig{Mode: ldapv1alpha1.NetworkModePodRouted},
			},
		},
		{
			// A subset selector narrows the PEERS and nothing else: the stride slot
			// stays mesh-wide, because two clusters spanning different subsets
			// must still agree about who owns stride slot 1.
			name: "site selector narrows the peer set but not the stride slot",
			mesh: meshSpec(),
			spec: ldapv1alpha1.SlapdClusterSpec{
				MeshRef: "mesh",
				Sites:   []string{"site-2", "site-3"},
			},
			self: "site-2",
			want: &meshWiring{
				ServerIDBase: 16,
				ExternalPeers: []ldapv1alpha1.ExternalPeer{
					{Name: "site-3", SiteIndex: new(int32(2)), Port: 1025, TLSSecretName: "site-3-ca",
						Discovery: &ldapv1alpha1.ExternalPeerDiscovery{
							KubeconfigSecret: ldapv1alpha1.KubeconfigSecretRef{Name: "site-3-kubeconfig"},
						}},
				},
				Network: &ldapv1alpha1.ReplicationNetworkConfig{Mode: ldapv1alpha1.NetworkModePodRouted},
			},
		},
		{
			// An explicit serverIDBase alongside meshRef is refused rather
			// than silently overridden in either direction. Silently
			// preferring the spec hands this site a stride slot the mesh gave to
			// someone else; silently preferring the mesh discards a value the
			// user believed was in force. Both are how a serverID collision
			// gets in quietly, and slapd validates nothing.
			name: "explicit serverIDBase conflicts",
			mesh: meshSpec(),
			spec: ldapv1alpha1.SlapdClusterSpec{
				MeshRef:     "mesh",
				Replication: ldapv1alpha1.SlapdReplicationConfig{ServerIDBase: 900},
			},
			self:       "site-2",
			wantReason: reasonSpecConflictsWithMesh,
		},
		{
			// A serverIDBase that AGREES with the mesh is not a conflict.
			// Rejecting it would make the upgrade path (write the mesh, keep
			// the old field for one release, remove it) impossible.
			name: "serverIDBase agreeing with the mesh is not a conflict",
			mesh: meshSpec(),
			spec: ldapv1alpha1.SlapdClusterSpec{
				MeshRef:     "mesh",
				Replication: ldapv1alpha1.SlapdReplicationConfig{ServerIDBase: 16},
			},
			self: "site-2",
			want: &meshWiring{
				ServerIDBase:  16,
				ExternalPeers: peersForSite2(),
				Network:       &ldapv1alpha1.ReplicationNetworkConfig{Mode: ldapv1alpha1.NetworkModePodRouted},
			},
		},
		{
			// serverIDBase is a plain int32 with a CRD default of 0, so an
			// explicit 0 is indistinguishable on the wire from an unset field.
			// It is therefore read as UNSET, and the mesh's value wins. Safe in
			// the only direction that matters: the mesh value is the
			// collision-free one, and 0 at a site the mesh did not put on
			// stride slot 0 is precisely the collision.
			name: "explicit zero serverIDBase reads as unset",
			mesh: meshSpec(),
			spec: ldapv1alpha1.SlapdClusterSpec{
				MeshRef:     "mesh",
				Replication: ldapv1alpha1.SlapdReplicationConfig{ServerIDBase: 0},
			},
			self: "site-2",
			want: &meshWiring{
				ServerIDBase:  16,
				ExternalPeers: peersForSite2(),
				Network:       &ldapv1alpha1.ReplicationNetworkConfig{Mode: ldapv1alpha1.NetworkModePodRouted},
			},
		},
		{
			// externalPeers has no CRD default, so a non-empty list is an
			// unambiguous statement of intent and conflicts. A cluster that
			// really wants hand-written peers (ADR-011's foreign migration
			// source) simply does not set meshRef.
			name: "explicit externalPeers conflict",
			mesh: meshSpec(),
			spec: ldapv1alpha1.SlapdClusterSpec{
				MeshRef: "mesh",
				Replication: ldapv1alpha1.SlapdReplicationConfig{
					ExternalPeers: []ldapv1alpha1.ExternalPeer{{Name: "legacy", URI: "ldaps://legacy.example:636"}},
				},
			},
			self:       "site-2",
			wantReason: reasonSpecConflictsWithMesh,
		},
		{
			name: "explicit network conflicts when the mesh declares one",
			mesh: meshSpec(),
			spec: ldapv1alpha1.SlapdClusterSpec{
				MeshRef: "mesh",
				Replication: ldapv1alpha1.SlapdReplicationConfig{
					Network: &ldapv1alpha1.ReplicationNetworkConfig{Mode: ldapv1alpha1.NetworkModeMultus, MultusNetwork: "infra/repl"},
				},
			},
			self:       "site-2",
			wantReason: reasonSpecConflictsWithMesh,
		},
		{
			// A mesh that declares no network supplies no value, so there is
			// nothing for the cluster's own network to be ambiguous WITH. The
			// cluster keeps it, and the derivation reports "not derived" so the
			// applier leaves the field alone.
			name: "cluster network survives a mesh that declares none",
			mesh: &ldapv1alpha1.SlapdMeshSpec{Sites: threeSiteMesh()},
			spec: ldapv1alpha1.SlapdClusterSpec{
				MeshRef: "mesh",
				Replication: ldapv1alpha1.SlapdReplicationConfig{
					Network: &ldapv1alpha1.ReplicationNetworkConfig{Mode: ldapv1alpha1.NetworkModePodRouted},
				},
			},
			self: "site-2",
			want: &meshWiring{
				ServerIDBase:  16,
				ExternalPeers: peersForSite2(),
				Network:       nil,
			},
		},
		{
			// No SITE_NAME while meshRef is set: withhold and stay loud, the
			// Phase 2 precedent. Never a guess at the first site — that hands
			// this site a stride slot another site already owns.
			name: "no site identity",
			mesh: meshSpec(),
			spec: ldapv1alpha1.SlapdClusterSpec{MeshRef: "mesh"},
			self: "",

			wantReason: reasonSiteIdentityMissing,
		},
		{
			name:       "identity absent from the mesh",
			mesh:       meshSpec(),
			spec:       ldapv1alpha1.SlapdClusterSpec{MeshRef: "mesh"},
			self:       "site-9",
			wantReason: reasonSiteNotInMesh,
		},
		{
			name: "identity excluded by the cluster's own selector",
			mesh: meshSpec(),
			spec: ldapv1alpha1.SlapdClusterSpec{
				MeshRef: "mesh",
				Sites:   []string{"site-1", "site-3"},
			},
			self:       "site-2",
			wantReason: reasonSiteNotSelected,
		},
		{
			name: "selector naming a site the mesh does not have",
			mesh: meshSpec(),
			spec: ldapv1alpha1.SlapdClusterSpec{
				MeshRef: "mesh",
				Sites:   []string{"site-2", "site-typo"},
			},
			self:       "site-2",
			wantReason: reasonSiteNotInMesh,
		},
		{
			name: "malformed mesh is rejected, never half-derived",
			mesh: &ldapv1alpha1.SlapdMeshSpec{Sites: []ldapv1alpha1.MeshSite{
				{Name: "site-1", ServerIDIndex: new(int32(0))},
				{Name: "site-2", ServerIDIndex: new(int32(0))},
			}},
			spec:       ldapv1alpha1.SlapdClusterSpec{MeshRef: "mesh"},
			self:       "site-2",
			wantReason: reasonMeshSitesInvalid,
		},
		{
			// maxSites is set wide on purpose so that ONLY slapd's ceiling is
			// breached: index 256 is the first that cannot be given a stride
			// slot under serverIDStride 16 (base 4096, past the 4094 cap).
			name: "serverID stride ceiling",
			mesh: &ldapv1alpha1.SlapdMeshSpec{
				Sites: []ldapv1alpha1.MeshSite{
					{Name: "site-1", ServerIDIndex: new(int32(0))},
					{Name: "site-2", ServerIDIndex: new(int32(256))},
				},
				RIDLayout: &ldapv1alpha1.RIDLayout{PodsPerSite: 8, MaxSites: 999},
			},
			spec:       ldapv1alpha1.SlapdClusterSpec{MeshRef: "mesh"},
			self:       "site-2",
			wantReason: reasonServerIDCeiling,
		},
		{
			// ADR-028's amendment of 2026-10-05, operator side. The CEL rule on
			// SlapdMesh refuses this index at admission; this row covers the CR
			// that reached the cluster another way — an object that predates the
			// rule, a restore, an apply with validation off — which is the whole
			// reason a second layer exists at all.
			//
			// Index 5 is deliberately unremarkable: it is legal under the
			// default layout and illegal only because THIS mesh declared a
			// 4-wide remote block. A check that read the default rather than
			// the mesh's own layout passes every other row in this table and
			// fails here.
			//
			// What the refusal prevents is quiet: the derivation would hand back
			// a peer whose siteIndex has no slot, judgeExternalPeers would then
			// refuse the database one layer down, and the message a human reads
			// would name externalPeers[].siteIndex — a field nobody typed,
			// because under meshRef the whole peer list is derived.
			name: "a site index past this mesh's own maxSites is refused",
			mesh: &ldapv1alpha1.SlapdMeshSpec{
				Sites: []ldapv1alpha1.MeshSite{
					{Name: "site-1", ServerIDIndex: new(int32(0))},
					{Name: "site-2", ServerIDIndex: new(int32(5))},
				},
				RIDLayout: &ldapv1alpha1.RIDLayout{PodsPerSite: 8, MaxSites: 4},
			},
			spec:       ldapv1alpha1.SlapdClusterSpec{MeshRef: "mesh"},
			self:       "site-1",
			wantReason: reasonSiteIndexPastLayout,
		},
		{
			// The tier order, which is load-bearing and otherwise rests on the
			// single pre-existing ceiling row above.
			//
			// Index 256 breaches BOTH bounds at once: slapd's serverID ceiling
			// of 255, and this mesh's maxSites of 4. Both statements are true
			// and only one is useful, so the reason must be the CEILING: 255
			// comes from olcServerID's 4095 and no field widens it, while
			// "widen spec.ridLayout.maxSites" is advice that cannot work here —
			// a deployer who followed it would raise maxSites to 257, re-apply,
			// and be refused again by a bound they were never told about.
			//
			// Same rule that produced RIDSegmentUnverified and
			// RIDLayoutUnresolved: a reason is a diagnosis. Here it decides
			// which of two true statements a human is handed.
			name: "slapd's immovable ceiling outranks the layout's movable one",
			mesh: &ldapv1alpha1.SlapdMeshSpec{
				Sites: []ldapv1alpha1.MeshSite{
					{Name: "site-1", ServerIDIndex: new(int32(0))},
					{Name: "site-2", ServerIDIndex: new(int32(256))},
				},
				RIDLayout: &ldapv1alpha1.RIDLayout{PodsPerSite: 8, MaxSites: 4},
			},
			spec:       ldapv1alpha1.SlapdClusterSpec{MeshRef: "mesh"},
			self:       "site-1",
			wantReason: reasonServerIDCeiling,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := deriveClusterWiring(tc.mesh, tc.spec, tc.self)

			if tc.wantReason != "" {
				if err == nil {
					t.Fatalf("deriveClusterWiring() = %+v, nil; want error with reason %q", got, tc.wantReason)
				}
				var re *meshResolveError
				if !errors.As(err, &re) {
					t.Fatalf("deriveClusterWiring() error %v is not a *meshResolveError", err)
				}
				if re.Reason != tc.wantReason {
					t.Errorf("reason = %q, want %q (message: %s)", re.Reason, tc.wantReason, re.Message)
				}
				if got != nil {
					t.Errorf("a failed resolution must derive nothing, got %+v", got)
				}
				return
			}

			if err != nil {
				t.Fatalf("deriveClusterWiring() unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("deriveClusterWiring() =\n  %s\nwant\n  %s", fmtWiring(got), fmtWiring(tc.want))
			}
		})
	}
}

// TestApplyMeshWiring_NoMeshRefIsByteIdentical is the compatibility invariant
// stated as an assertion on the WHOLE spec rather than on the fields the
// derivation happens to touch. Every deployment in existence has no meshRef, so
// the only acceptable effect of this milestone on them is none.
func TestApplyMeshWiring_NoMeshRefIsByteIdentical(t *testing.T) {
	before := ldapv1alpha1.SlapdClusterSpec{
		Replicas: 3,
		Replication: ldapv1alpha1.SlapdReplicationConfig{
			Enabled:      true,
			ServerIDBase: 200,
			ExternalPeers: []ldapv1alpha1.ExternalPeer{
				{Name: "site-other", Port: 1025, TLSSecretName: "site-other-ca"},
			},
			Network: &ldapv1alpha1.ReplicationNetworkConfig{Mode: ldapv1alpha1.NetworkModePodRouted},
		},
	}
	sc := &ldapv1alpha1.SlapdCluster{Spec: *before.DeepCopy()}

	w, err := deriveClusterWiring(meshSpec(), sc.Spec, "site-2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	applyMeshWiring(sc, w)

	if !reflect.DeepEqual(sc.Spec, before) {
		t.Errorf("a cluster without meshRef was modified:\n got %+v\nwant %+v", sc.Spec, before)
	}
}

// TestApplyMeshWiring_Overwrites pins the applier: the derived values land on
// the object the rest of the operator reads, and a nil Network leaves the
// cluster's own in place.
func TestApplyMeshWiring_Overwrites(t *testing.T) {
	sc := &ldapv1alpha1.SlapdCluster{Spec: ldapv1alpha1.SlapdClusterSpec{
		MeshRef: "mesh",
		Replication: ldapv1alpha1.SlapdReplicationConfig{
			Enabled: true,
			Network: &ldapv1alpha1.ReplicationNetworkConfig{Mode: ldapv1alpha1.NetworkModeMultus, MultusNetwork: "infra/repl"},
		},
	}}
	applyMeshWiring(sc, &meshWiring{
		ServerIDBase:  100,
		ExternalPeers: peersForSite2(),
		Network:       nil,
	})

	if sc.Spec.Replication.ServerIDBase != 100 {
		t.Errorf("ServerIDBase = %d, want 100", sc.Spec.Replication.ServerIDBase)
	}
	if !reflect.DeepEqual(sc.Spec.Replication.ExternalPeers, peersForSite2()) {
		t.Errorf("ExternalPeers = %+v, want the derived pair", sc.Spec.Replication.ExternalPeers)
	}
	if sc.Spec.Replication.Network == nil || sc.Spec.Replication.Network.MultusNetwork != "infra/repl" {
		t.Errorf("a nil derived Network must leave the cluster's own alone, got %+v", sc.Spec.Replication.Network)
	}
	if !sc.Spec.Replication.Enabled {
		t.Error("applyMeshWiring must not touch fields it does not derive")
	}
}

func fmtWiring(w *meshWiring) string {
	if w == nil {
		return "<nil>"
	}
	var s strings.Builder
	s.WriteString("base=" + itoa(int(w.ServerIDBase)))
	for _, p := range w.ExternalPeers {
		// siteIndex is rendered because it is a RID coordinate (ADR-031
		// decision 4): a peer whose index is wrong looks identical to a
		// correct one in every other field, and a diff that prints two
		// identical lines is how a reader concludes the test is broken rather
		// than the code.
		idx := "<unset>"
		if p.SiteIndex != nil {
			idx = itoa(int(*p.SiteIndex))
		}
		s.WriteString(" peer{" + p.Name + " siteIndex=" + idx + " ca=" + p.TLSSecretName)
		if p.Discovery != nil {
			s.WriteString(" kubeconfig=" + p.Discovery.KubeconfigSecret.Name)
		}
		s.WriteString("}")
	}
	if w.Network != nil {
		s.WriteString(" net=" + w.Network.Mode + "/" + w.Network.MultusNetwork)
	} else {
		s.WriteString(" net=<nil>")
	}
	return s.String()
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
