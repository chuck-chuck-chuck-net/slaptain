package cmd

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The Phase 6a decision, exhaustively: given a SlapdCluster as the API server
// hands it over, which external peer set do the diagnostics display, where did
// it come from, and what do they say when they cannot tell?
//
// The last question is the whole point. Before this seam existed, a mesh-driven
// cluster printed an empty peer list — indistinguishable from a healthy
// standalone cluster — because the derivation lives only in the operator's
// memory (ADR-028 §4, MESH-PLAN Phase 4). Every case below that cannot produce
// a trustworthy peer set must therefore produce a non-empty Note.

func peerCluster(peers ...ldapv1alpha1.ExternalPeer) *ldapv1alpha1.SlapdCluster {
	return &ldapv1alpha1.SlapdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "slapd", Namespace: "ns"},
		Spec: ldapv1alpha1.SlapdClusterSpec{
			Replicas: 3,
			Replication: ldapv1alpha1.SlapdReplicationConfig{
				Enabled:       true,
				ExternalPeers: peers,
			},
		},
	}
}

func withMeshRef(sc *ldapv1alpha1.SlapdCluster, ref string) *ldapv1alpha1.SlapdCluster {
	sc.Spec.MeshRef = ref
	return sc
}

func withMeshCondition(sc *ldapv1alpha1.SlapdCluster, status metav1.ConditionStatus, reason, msg string) *ldapv1alpha1.SlapdCluster {
	sc.Status.Conditions = append(sc.Status.Conditions, metav1.Condition{
		Type:    "MeshResolved",
		Status:  status,
		Reason:  reason,
		Message: msg,
	})
	return sc
}

func withPeerStatuses(sc *ldapv1alpha1.SlapdCluster, names ...string) *ldapv1alpha1.SlapdCluster {
	for _, n := range names {
		sc.Status.ExternalPeerStatuses = append(sc.Status.ExternalPeerStatuses,
			ldapv1alpha1.ExternalPeerStatus{
				Name:                n,
				Connected:           true,
				ReplicationState:    ldapv1alpha1.ReplicationSynced,
				DiscoveredAddresses: []string{"10.0.0.1"},
			})
	}
	return sc
}

func TestResolveExternalPeersForDisplay(t *testing.T) {
	handWritten := []ldapv1alpha1.ExternalPeer{
		{Name: "site-b", URI: "ldaps://b.example:1025"},
		{Name: "site-c", PodAddresses: []string{"192.0.2.7"}},
	}

	tests := []struct {
		name string
		sc   *ldapv1alpha1.SlapdCluster

		wantSource   peerSourceKind
		wantPeers    []string // peer names, in order
		wantNoteHas  []string // substrings the note must carry
		wantNoteNone bool     // note must be empty (the byte-identical path)
	}{
		{
			// The compatibility invariant, half one: no meshRef, no peers.
			// Nothing is said, because there is nothing to say — this is what
			// every standalone cluster's output has always looked like.
			name:         "no meshRef, no peers",
			sc:           peerCluster(),
			wantSource:   peerSourceSpec,
			wantPeers:    nil,
			wantNoteNone: true,
		},
		{
			// The compatibility invariant, half two: hand-written peers are
			// passed through untouched and unannotated.
			name:         "no meshRef, hand-written peers",
			sc:           peerCluster(handWritten...),
			wantSource:   peerSourceSpec,
			wantPeers:    []string{"site-b", "site-c"},
			wantNoteNone: true,
		},
		{
			// The headline case. The spec carries no peers at all, yet the
			// operator derived two and published them into status.
			name: "meshRef resolved, peers in status",
			sc: withPeerStatuses(
				withMeshCondition(withMeshRef(peerCluster(), "lab"),
					metav1.ConditionTrue, "Derived", "cross-site wiring derived from SlapdMesh \"lab\""),
				"site-b", "site-c"),
			wantSource:  peerSourceMesh,
			wantPeers:   []string{"site-b", "site-c"},
			wantNoteHas: []string{"lab", "derived"},
		},
		{
			// A single-site mesh genuinely has no peers. That is a real answer
			// and must be SAID, not left as silence that reads identically to
			// a standalone cluster.
			name: "meshRef resolved, zero peers",
			sc: withMeshCondition(withMeshRef(peerCluster(), "lab"),
				metav1.ConditionTrue, "Derived", "derived"),
			wantSource:  peerSourceMesh,
			wantPeers:   nil,
			wantNoteHas: []string{"lab", "0"},
		},
		{
			// The operator refused to derive. slctl must repeat the operator's
			// own reason rather than invent a verdict, and must never render
			// this as "no external peers".
			name: "meshRef unresolved",
			sc: withMeshCondition(withMeshRef(peerCluster(), "lab"),
				metav1.ConditionFalse, "MeshNotFound", "SlapdMesh \"lab\" not found in namespace \"ns\""),
			wantSource:  peerSourceUnknown,
			wantPeers:   nil,
			wantNoteHas: []string{"MeshNotFound", "not found", "UNKNOWN, not empty"},
		},
		{
			// Stale evidence beats no evidence: the operator derived these
			// once and cannot now, so they are shown and labelled last-known.
			name: "meshRef unresolved, stale peers still in status",
			sc: withPeerStatuses(
				withMeshCondition(withMeshRef(peerCluster(), "lab"),
					metav1.ConditionFalse, "MeshUnreadable", "cannot read SlapdMesh"),
				"site-b"),
			wantSource:  peerSourceUnknown,
			wantPeers:   []string{"site-b"},
			wantNoteHas: []string{"MeshUnreadable", "LAST KNOWN"},
		},
		{
			// No condition at all: an operator that has not reconciled this
			// cluster, or one too old to know about meshes. Unknown, loudly.
			name:        "meshRef set, operator published no verdict",
			sc:          withMeshRef(peerCluster(), "lab"),
			wantSource:  peerSourceUnknown,
			wantPeers:   nil,
			wantNoteHas: []string{"lab", "no MeshResolved"},
		},
		{
			// Condition present but Unknown — same treatment as False. An
			// inconclusive verdict is not a verdict.
			name: "meshRef, MeshResolved Unknown",
			sc: withMeshCondition(withMeshRef(peerCluster(), "lab"),
				metav1.ConditionUnknown, "Pending", "not yet evaluated"),
			wantSource:  peerSourceUnknown,
			wantPeers:   nil,
			wantNoteHas: []string{"Pending"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveExternalPeersForDisplay(tc.sc)

			if got.Source != tc.wantSource {
				t.Errorf("Source = %q, want %q", got.Source, tc.wantSource)
			}

			var names []string
			for _, p := range got.Peers {
				names = append(names, p.Name)
			}
			if len(names) != len(tc.wantPeers) {
				t.Fatalf("peers = %v, want %v", names, tc.wantPeers)
			}
			for i := range names {
				if names[i] != tc.wantPeers[i] {
					t.Errorf("peer[%d] = %q, want %q", i, names[i], tc.wantPeers[i])
				}
			}

			if tc.wantNoteNone {
				if got.Note != "" {
					t.Errorf("Note = %q, want empty — the non-mesh path must stay byte-identical", got.Note)
				}
				return
			}
			if got.Note == "" {
				t.Fatalf("Note is empty; an empty peer list that means "+
					"\"I could not tell\" is the exact defect being fixed (source=%q)", got.Source)
			}
			for _, want := range tc.wantNoteHas {
				if !strings.Contains(strings.ToLower(got.Note), strings.ToLower(want)) {
					t.Errorf("Note = %q, want it to mention %q", got.Note, want)
				}
			}
		})
	}
}

// Peers recovered from status must present as discovery-mode peers, because
// that is the only shape the mesh derivation produces (externalPeersForSite
// sets Discovery and never URI or PodAddresses). Every downstream reader in
// inspect.go branches on that shape, so getting it wrong would relabel a
// cross-site stanza as a single-URI one.
func TestResolvedMeshPeersPresentAsDiscovery(t *testing.T) {
	sc := withPeerStatuses(
		withMeshCondition(withMeshRef(peerCluster(), "lab"),
			metav1.ConditionTrue, "Derived", "derived"),
		"site-b")

	got := resolveExternalPeersForDisplay(sc)
	if len(got.Peers) != 1 {
		t.Fatalf("peers = %d, want 1", len(got.Peers))
	}
	p := got.Peers[0]
	if p.Discovery == nil {
		t.Errorf("peer %q has no Discovery; a mesh-derived peer is always a discovery peer", p.Name)
	}
	if p.URI != "" || len(p.PodAddresses) > 0 {
		t.Errorf("peer %q carries URI=%q podAddresses=%v; the mesh derivation sets neither",
			p.Name, p.URI, p.PodAddresses)
	}
	if p.ReplicasPerPeer != nil {
		t.Errorf("peer %q pins replicasPerPeer=%d; the mesh derivation leaves it unset so the CRD default applies",
			p.Name, *p.ReplicasPerPeer)
	}
}

// applyResolvedPeers is the in-memory application, mirroring the operator's own
// applyMeshWiring. The load-bearing half is the negative: a cluster with no
// meshRef must come out of it untouched, since every documented slctl example
// is a non-mesh cluster.
func TestApplyResolvedPeersLeavesNonMeshClustersAlone(t *testing.T) {
	handWritten := []ldapv1alpha1.ExternalPeer{{Name: "site-b", URI: "ldaps://b.example:1025"}}
	sc := peerCluster(handWritten...)

	res := applyResolvedPeers(sc)

	if res.Source != peerSourceSpec {
		t.Errorf("Source = %q, want %q", res.Source, peerSourceSpec)
	}
	if len(sc.Spec.Replication.ExternalPeers) != 1 || sc.Spec.Replication.ExternalPeers[0].URI != "ldaps://b.example:1025" {
		t.Errorf("spec peers were rewritten: %+v", sc.Spec.Replication.ExternalPeers)
	}
}

func TestApplyResolvedPeersFillsMeshClusterSpec(t *testing.T) {
	sc := withPeerStatuses(
		withMeshCondition(withMeshRef(peerCluster(), "lab"),
			metav1.ConditionTrue, "Derived", "derived"),
		"site-b", "site-c")

	res := applyResolvedPeers(sc)

	if res.Source != peerSourceMesh {
		t.Fatalf("Source = %q, want %q", res.Source, peerSourceMesh)
	}
	if len(sc.Spec.Replication.ExternalPeers) != 2 {
		t.Fatalf("spec peers = %d, want 2 — every existing reader of "+
			"Spec.Replication.ExternalPeers depends on this in-memory fill",
			len(sc.Spec.Replication.ExternalPeers))
	}
}
