/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"strings"
	"testing"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
	"github.com/chuck-chuck-chuck-net/slaptain/operator/internal/ridcoord"
)

// The remote block of a segment is S slots wide, indexed by the PEER's site
// index (ADR-031 decision 1 and 4). Everything that can make that indexing
// ill-defined is refused here, and every one of those refusals is a silent
// failure if it is not: slapd loads two stanzas sharing a RID without a word,
// and the two then share a connid.
//
// RID-PLAN hazard 5, as corrected on 2026-10-05: the collision to refuse is two
// PEERS declaring one index. The local site has no index in this scheme at all
// — the local block is indexed by pod ordinal — so a peer declaring the same
// index as the site it is configured on is harmless and deliberately left
// alone.

// peerCluster builds a replicating SlapdCluster carrying `peers`.
func peerCluster(peers ...ldapv1alpha1.ExternalPeer) *ldapv1alpha1.SlapdCluster {
	sc := ridCluster("")
	sc.Spec.Replication.Enabled = true
	sc.Spec.Replication.ExternalPeers = peers
	return sc
}

// peer is a discovery-mode external peer declaring `siteIndex` (nil for none).
func ridPeer(name string, siteIndex *int32) ldapv1alpha1.ExternalPeer {
	return ldapv1alpha1.ExternalPeer{
		Name:      name,
		SiteIndex: siteIndex,
		Discovery: &ldapv1alpha1.ExternalPeerDiscovery{
			KubeconfigSecret: ldapv1alpha1.KubeconfigSecretRef{Name: name + "-kubeconfig"},
		},
	}
}

func TestJudgeExternalPeers(t *testing.T) {
	def := ridcoord.DefaultLayout() // P=8, S=12

	for _, tc := range []struct {
		name       string
		cluster    *ldapv1alpha1.SlapdCluster
		layout     ridcoord.Layout
		wantOK     bool
		wantReason string
		naming     []string
		why        string
	}{
		{
			name:    "a peer list with distinct indices converges",
			cluster: peerCluster(ridPeer(testSite2, new(int32(1))), ridPeer(testSite3, new(int32(2)))),
			layout:  def,
			wantOK:  true,
			why:     "the ordinary mesh: one slot per peer site, nothing shared",
		},
		{
			name:    "no external peers at all is nothing to coordinate",
			cluster: peerCluster(),
			layout:  def,
			wantOK:  true,
			why:     "a single-site cluster writes in-cluster stanzas only, and the remote block stays empty",
		},
		{
			name:       "two peers declaring one siteIndex are refused, both named",
			cluster:    peerCluster(ridPeer(testSite2, new(int32(1))), ridPeer(testSite3, new(int32(1)))),
			layout:     def,
			wantReason: reasonPeerSiteIndexCollision,
			naming:     []string{testSite2, testSite3},
			why: "they share remote slot P+1, so both links carry one RID — the shared-connid state " +
				"slapd never rejects. Naming only one of them tells a human to edit a coin flip",
		},
		{
			name:       "a peer with no siteIndex has no slot to live in",
			cluster:    peerCluster(ridPeer(testSite2, new(int32(1))), ridPeer(testSite3, nil)),
			layout:     def,
			wantReason: reasonPeerSiteIndexMissing,
			naming:     []string{testSite3},
			why: "nothing else carries a peer site's identity off-mesh, and a positional fallback is " +
				"exactly the inference ADR-031 forbids",
		},
		{
			// Individually plausible, wrong only against the layout: 12 is a
			// legal CRD value (0..998) and what a thirteenth site would
			// declare. Against S=12 it is one past the remote block, and the
			// RID it would produce is inside slapd's band and belongs to the
			// NEXT segment.
			name:       "a siteIndex at the layout's maxSites is past the remote block",
			cluster:    peerCluster(ridPeer("site-13", new(int32(12)))),
			layout:     def,
			wantReason: reasonPeerSiteIndexOutOfRange,
			naming:     []string{"site-13", "12"},
			why:        "the deployer's response is to widen maxSites on the mesh, which the message must name",
		},
		{
			name:       "a negative siteIndex names no slot",
			cluster:    peerCluster(ridPeer(testSite2, new(int32(-1)))),
			layout:     def,
			wantReason: reasonPeerSiteIndexOutOfRange,
			naming:     []string{testSite2},
			why:        "the Go zero value is 0 and a negative can only be typed, so it is a refusal and never a default",
		},
		{
			// Legal at the default layout, refused under a narrower one. The
			// layout is mesh-scoped, so the same CR is judged differently at a
			// mesh that declared fewer sites — which is ADR-031 decision 5's
			// whole shape.
			name:       "a siteIndex legal at the default layout is refused under a narrower one",
			cluster:    peerCluster(ridPeer("site-5", new(int32(4)))),
			layout:     ridcoord.Layout{PodsPerSite: 8, MaxSites: 3},
			wantReason: reasonPeerSiteIndexOutOfRange,
			naming:     []string{"site-5"},
			why:        "a layout the mesh narrowed must be obeyed, not silently widened back to the defaults",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := judgeExternalPeers(tc.cluster, tc.layout)
			if v.OK != tc.wantOK {
				t.Fatalf("OK = %v, want %v (reason %q, message %q) — %s", v.OK, tc.wantOK, v.Reason, v.Message, tc.why)
			}
			if tc.wantOK {
				return
			}
			if v.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q — %s", v.Reason, tc.wantReason, tc.why)
			}
			for _, want := range tc.naming {
				if !strings.Contains(v.Message, want) {
					t.Errorf("message %q does not name %q — %s", v.Message, want, tc.why)
				}
			}
		})
	}
}

// ADR-031 decision 9: the remote block is S wide because a pod holds AT MOST
// ONE link to any peer site for one database. replicasPerPeer >= 2 asks for a
// second, and there is no second slot for it to live in — so it is refused
// until ADR-032 redefines the quantity as links per site PAIR, spread across
// distinct local pods.
//
// Refused rather than capped, because capping would silently deliver less
// redundancy than the spec asks for, and the whole value of the field was
// redundancy (ADR-028 amendment, 2026-09-28).
func TestJudgeExternalPeersRefusesASecondLinkPerPeerSite(t *testing.T) {
	def := ridcoord.DefaultLayout()

	t.Run("per-peer replicasPerPeer", func(t *testing.T) {
		p := ridPeer(testSite2, new(int32(1)))
		p.ReplicasPerPeer = new(int32(2))
		sc := peerCluster(p, ridPeer(testSite3, new(int32(2))))

		v := judgeExternalPeers(sc, def)
		if v.OK {
			t.Fatalf("accepted: a second link from one pod to site-2 has no slot in the remote block")
		}
		if v.Reason != reasonReplicasPerPeerUnsupported {
			t.Errorf("reason = %q, want %q", v.Reason, reasonReplicasPerPeerUnsupported)
		}
		if !strings.Contains(v.Message, testSite2) {
			t.Errorf("message %q does not name the peer whose fan-out is refused", v.Message)
		}
	})

	t.Run("cluster-wide replicasPerPeer", func(t *testing.T) {
		sc := peerCluster(ridPeer(testSite2, new(int32(1))))
		sc.Spec.Replication.ReplicasPerPeer = new(int32(3))

		v := judgeExternalPeers(sc, def)
		if v.OK {
			t.Fatalf("accepted: the cluster-wide default is stamped onto every derived peer, " +
				"so it is the only form a mesh cluster can even reach")
		}
		if v.Reason != reasonReplicasPerPeerUnsupported {
			t.Errorf("reason = %q, want %q", v.Reason, reasonReplicasPerPeerUnsupported)
		}
		if !strings.Contains(v.Message, "3") {
			t.Errorf("message %q does not name the value that was refused", v.Message)
		}
	})

	t.Run("one link per peer site is the supported topology", func(t *testing.T) {
		sc := peerCluster(ridPeer(testSite2, new(int32(1))))
		sc.Spec.Replication.ReplicasPerPeer = new(int32(1))
		if v := judgeExternalPeers(sc, def); !v.OK {
			t.Fatalf("refused the topology as deployed today: %s (%s)", v.Reason, v.Message)
		}
		sc.Spec.Replication.ReplicasPerPeer = nil
		sc.Spec.Replication.ExternalPeers[0].ReplicasPerPeer = nil
		if v := judgeExternalPeers(sc, def); !v.OK {
			t.Fatalf("refused an unset fan-out, which the builder already reads as 1: %s (%s)", v.Reason, v.Message)
		}
	})
}

// Under meshRef nothing is hand-written: the peer set is derived, so the
// derivation is what has to supply each peer's coordinate. ADR-028 already
// requires serverIDIndex to be declared and unique mesh-wide, which is exactly
// the uniqueness the remote block needs — so the peer's siteIndex IS the peer
// site's serverIDIndex, and not its position in spec.sites.
//
// Position would be wrong for the same reason it is wrong for serverIDBase
// (MESH-PLAN hazard 2): spec.sites is freely reorderable and gaps are legal, so
// the site at position 1 is not the site at index 1.
func TestMeshDerivedPeersCarryThePeerSitesServerIDIndex(t *testing.T) {
	// The golden three-site mesh with a gap: site-3 was added after a
	// decommissioned site's index was retired, so position and index differ.
	sites := []ldapv1alpha1.MeshSite{
		{Name: testSite1, ServerIDIndex: new(int32(0))},
		{Name: testSite2, ServerIDIndex: new(int32(1))},
		{Name: testSite3, ServerIDIndex: new(int32(5))},
	}

	peers, err := externalPeersForSite(sites, nil, testSite1, nil)
	if err != nil {
		t.Fatalf("externalPeersForSite: %v", err)
	}
	if len(peers) != 2 {
		t.Fatalf("got %d peers, want 2 (every site but this one)", len(peers))
	}

	want := map[string]int32{testSite2: 1, testSite3: 5}
	for _, p := range peers {
		idx, known := want[p.Name]
		if !known {
			t.Fatalf("unexpected peer %q", p.Name)
		}
		if p.SiteIndex == nil {
			t.Errorf("peer %q carries no siteIndex: under meshRef nothing else can supply one, "+
				"so its cross-site stanza has no RID at all", p.Name)
			continue
		}
		if *p.SiteIndex != idx {
			t.Errorf("peer %q: siteIndex = %d, want %d (its declared serverIDIndex) — "+
				"a derivation from list position renumbers every peer when the list is reordered",
				p.Name, *p.SiteIndex, idx)
		}
	}
}

// ADR-028's "meshRef + an explicitly set derived field is refused, never
// silently preferred" already covers siteIndex, and covers it without a new
// rule: externalPeers is derived WHOLESALE under meshRef, so a hand-written
// peer carrying a siteIndex is refused as a peer, before anyone looks at the
// field.
//
// GREEN FROM BIRTH, deliberately, and kept as a tripwire: if the peer-list
// conflict is ever relaxed — to let a deployer add one peer outside the mesh,
// say — then siteIndex becomes a derived field a user can set, and THIS is the
// test that should have gone red first. Its failure message is the spec for
// what to write next.
func TestASiteIndexSetAlongsideMeshRefIsRefused(t *testing.T) {
	mesh := ldapv1alpha1.SlapdMeshSpec{Sites: []ldapv1alpha1.MeshSite{
		{Name: testSite1, ServerIDIndex: new(int32(0))},
		{Name: testSite2, ServerIDIndex: new(int32(1))},
	}}
	spec := ldapv1alpha1.SlapdClusterSpec{
		MeshRef: testMeshName,
		Replication: ldapv1alpha1.SlapdReplicationConfig{
			Enabled:       true,
			ExternalPeers: []ldapv1alpha1.ExternalPeer{ridPeer(testSite2, new(int32(1)))},
		},
	}

	if err := checkMeshSpecConflicts(&mesh, spec); err == nil {
		t.Fatalf("a hand-written peer carrying siteIndex was accepted alongside meshRef. " +
			"If externalPeers is now partially hand-writable under a mesh, siteIndex is a derived " +
			"field a user can set, and it needs a conflict rule of its own: under meshRef it is the " +
			"peer site's serverIDIndex, and setting it must be refused rather than preferred")
	}
}
