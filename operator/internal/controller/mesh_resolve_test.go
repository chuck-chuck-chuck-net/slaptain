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
			Name:          "site-1",
			Port:          1025,
			TLSSecretName: "site-1-ca",
			Discovery: &ldapv1alpha1.ExternalPeerDiscovery{
				KubeconfigSecret: ldapv1alpha1.KubeconfigSecretRef{Name: "site-1-kubeconfig"},
			},
		},
		{
			Name:          "site-3",
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
				ServerIDBase:  100,
				ExternalPeers: peersForSite2(),
				Network:       &ldapv1alpha1.ReplicationNetworkConfig{Mode: ldapv1alpha1.NetworkModePodRouted},
			},
		},
		{
			// Golden: the lab's live decades. A derivation that moves any of
			// these is a defect, not a migration (MESH-PLAN hazard 2).
			name: "first site keeps decade 0",
			mesh: meshSpec(),
			spec: ldapv1alpha1.SlapdClusterSpec{MeshRef: "mesh"},
			self: "site-1",
			want: &meshWiring{
				ServerIDBase: 0,
				ExternalPeers: []ldapv1alpha1.ExternalPeer{
					{Name: "site-2", Port: 1025, TLSSecretName: "site-2-ca",
						Discovery: &ldapv1alpha1.ExternalPeerDiscovery{
							KubeconfigSecret: ldapv1alpha1.KubeconfigSecretRef{Name: "site-2-kubeconfig"},
						}},
					{Name: "site-3", Port: 1025, TLSSecretName: "site-3-ca",
						Discovery: &ldapv1alpha1.ExternalPeerDiscovery{
							KubeconfigSecret: ldapv1alpha1.KubeconfigSecretRef{Name: "site-3-kubeconfig"},
						}},
				},
				Network: &ldapv1alpha1.ReplicationNetworkConfig{Mode: ldapv1alpha1.NetworkModePodRouted},
			},
		},
		{
			// A subset selector narrows the PEERS and nothing else: the decade
			// stays mesh-wide, because two clusters spanning different subsets
			// must still agree about who owns decade 100.
			name: "site selector narrows the peer set but not the decade",
			mesh: meshSpec(),
			spec: ldapv1alpha1.SlapdClusterSpec{
				MeshRef: "mesh",
				Sites:   []string{"site-2", "site-3"},
			},
			self: "site-2",
			want: &meshWiring{
				ServerIDBase: 100,
				ExternalPeers: []ldapv1alpha1.ExternalPeer{
					{Name: "site-3", Port: 1025, TLSSecretName: "site-3-ca",
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
			// preferring the spec hands this site a decade the mesh gave to
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
				Replication: ldapv1alpha1.SlapdReplicationConfig{ServerIDBase: 100},
			},
			self: "site-2",
			want: &meshWiring{
				ServerIDBase:  100,
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
			// decade 0 is precisely the collision.
			name: "explicit zero serverIDBase reads as unset",
			mesh: meshSpec(),
			spec: ldapv1alpha1.SlapdClusterSpec{
				MeshRef:     "mesh",
				Replication: ldapv1alpha1.SlapdReplicationConfig{ServerIDBase: 0},
			},
			self: "site-2",
			want: &meshWiring{
				ServerIDBase:  100,
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
				ServerIDBase:  100,
				ExternalPeers: peersForSite2(),
				Network:       nil,
			},
		},
		{
			// No SITE_NAME while meshRef is set: withhold and stay loud, the
			// Phase 2 precedent. Never a guess at the first site — that hands
			// this site a decade another site already owns.
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
				{Name: "site-1", ServerIDIndex: si(0)},
				{Name: "site-2", ServerIDIndex: si(0)},
			}},
			spec:       ldapv1alpha1.SlapdClusterSpec{MeshRef: "mesh"},
			self:       "site-2",
			wantReason: reasonMeshSitesInvalid,
		},
		{
			name: "serverID decade ceiling",
			mesh: &ldapv1alpha1.SlapdMeshSpec{Sites: []ldapv1alpha1.MeshSite{
				{Name: "site-1", ServerIDIndex: si(0)},
				{Name: "site-2", ServerIDIndex: si(41)},
			}},
			spec:       ldapv1alpha1.SlapdClusterSpec{MeshRef: "mesh"},
			self:       "site-2",
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
	s := "base=" + itoa(int(w.ServerIDBase))
	for _, p := range w.ExternalPeers {
		s += " peer{" + p.Name + " ca=" + p.TLSSecretName
		if p.Discovery != nil {
			s += " kubeconfig=" + p.Discovery.KubeconfigSecret.Name
		}
		s += "}"
	}
	if w.Network != nil {
		s += " net=" + w.Network.Mode + "/" + w.Network.MultusNetwork
	} else {
		s += " net=<nil>"
	}
	return s
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
