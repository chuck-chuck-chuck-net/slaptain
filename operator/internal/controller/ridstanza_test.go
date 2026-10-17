/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"errors"
	"testing"

	"github.com/chuck-chuck-chuck-net/slaptain/operator/internal/ridcoord"
)

// ADR-031 M4a: the consumption seam. internal/ridcoord owns the formula and is
// already green; what these tests pin is that the stanza side reaches it — with
// the right coordinate for the right provider, and with a refusal it never
// paves over.
//
// The stake is specific. slapd does not reject a duplicate RID (ADR-031, "What
// slapd actually requires"): two stanzas carrying one number both load, share a
// connid and become indistinguishable in the logs. So a RID that is wrong by
// one is not a loud failure, it is a silent one — which is why every case below
// asserts a number or a refusal, and never "something sensible".

// tightLayout is P=2, S=2 → M=4, C=249. It exists so that a value which is
// individually plausible — pod ordinal 2, peer site 2 — is wrong only THROUGH
// the arithmetic: both are below M, both land inside slapd's band, and both
// land in a slot that belongs to something else. (ridsegment_test.go's
// narrowLayout is a different shape, chosen to squeeze the SEGMENT axis.)
func tightLayout() ridcoord.Layout { return ridcoord.Layout{PodsPerSite: 2, MaxSites: 2} }

func TestInClusterStanzaRIDsAreTheLocalCoordinateOfEachProvider(t *testing.T) {
	layout := ridcoord.DefaultLayout()
	const segment, replicas, self = int32(3), int32(4), int32(1)

	got, err := inClusterStanzaRIDs(layout, segment, replicas, i32(self))
	if err != nil {
		t.Fatalf("inClusterStanzaRIDs: unexpected refusal: %v", err)
	}

	for ordinal := range replicas {
		if ordinal == self {
			continue
		}
		want, err := layout.Local(segment, ordinal)
		if err != nil {
			t.Fatalf("ridcoord.Local(%d, %d): %v", segment, ordinal, err)
		}
		if got[ordinal] != want {
			t.Errorf("provider ordinal %d: rid = %d, want %d (ridcoord.Local) — "+
				"a stanza carrying another provider's number is accepted by slapd in silence",
				ordinal, got[ordinal], want)
		}
	}
}

// A pod never replicates from itself, so its own slot is simply unused. The
// property that matters is the one after that: the slot is not RECLAIMED. If
// the set were compacted — the survivors renumbered to close the gap — every
// pod would hold a different number for the same provider, which is exactly the
// uniformity ADR-031 decision 1 pays two dead slots for.
func TestInClusterStanzaRIDsSkipSelfWithoutReclaimingTheSlot(t *testing.T) {
	layout := ridcoord.DefaultLayout()
	const segment, replicas = int32(0), int32(3)

	for self := range replicas {
		got, err := inClusterStanzaRIDs(layout, segment, replicas, new(self))
		if err != nil {
			t.Fatalf("self=%d: unexpected refusal: %v", self, err)
		}
		if _, present := got[self]; present {
			t.Errorf("self=%d: the pod's own ordinal carries rid %d — a pod replicating from itself is a loop",
				self, got[self])
		}
		if len(got) != int(replicas)-1 {
			t.Errorf("self=%d: %d stanzas for %d replicas, want %d", self, len(got), replicas, replicas-1)
		}
		for ordinal, rid := range got {
			want, err := layout.Local(segment, ordinal)
			if err != nil {
				t.Fatalf("ridcoord.Local(%d, %d): %v", segment, ordinal, err)
			}
			if rid != want {
				t.Errorf("self=%d, provider %d: rid = %d, want %d — the set was compacted around the skipped slot, "+
					"so two pods now disagree about which number names this provider", self, ordinal, rid, want)
			}
		}
	}
}

// The signature's own property, and the reason selfOrdinal is a pointer: who is
// asking is carried by the TYPE, not by a sentinel a caller has to remember.
//
// nil means "not a provider" — the read-only pod — and yields every provider.
// A non-nil ordinal means "I am this provider" and yields every provider but
// that one. The two readings differ by exactly one stanza, and the stanza they
// differ by is a real syncrepl link: an RO caller that passed its own 0 under a
// sentinel convention would silently drop provider 0 from an otherwise complete
// set, which is a deletion of that link under the atomic Replace (RID-PLAN
// hazard 2) and raises no error anywhere.
func TestInClusterStanzaRIDsReadNilSelfAsNotAProvider(t *testing.T) {
	layout := ridcoord.DefaultLayout()
	const segment, replicas = int32(1), int32(3)

	notAProvider, err := inClusterStanzaRIDs(layout, segment, replicas, nil)
	if err != nil {
		t.Fatalf("nil selfOrdinal: unexpected refusal: %v", err)
	}
	if len(notAProvider) != int(replicas) {
		t.Errorf("nil selfOrdinal: %d stanzas, want one per provider (%d): %v",
			len(notAProvider), replicas, notAProvider)
	}
	for ordinal := range replicas {
		if _, present := notAProvider[ordinal]; !present {
			t.Errorf("nil selfOrdinal: provider %d is missing — a caller that is not itself a "+
				"provider replicates from all of them", ordinal)
		}
	}

	provider0, err := inClusterStanzaRIDs(layout, segment, replicas, i32(0))
	if err != nil {
		t.Fatalf("selfOrdinal 0: unexpected refusal: %v", err)
	}
	if _, present := provider0[0]; present {
		t.Errorf("selfOrdinal 0: the caller's own ordinal carries rid %d", provider0[0])
	}
	if len(provider0) != len(notAProvider)-1 {
		t.Errorf("selfOrdinal 0 yielded %d stanzas and nil yielded %d: the two readings must differ "+
			"by exactly the one provider the caller is", len(provider0), len(notAProvider))
	}

	// A non-nil ordinal past the fleet skips nothing and is not an error: there
	// is no such provider to omit. Pinned rather than left to a doc comment,
	// because the alternative — refusing — withholds a whole site's stanzas
	// over a transient (a scale-down pass) that ADR-031 does not call a fault.
	beyond, err := inClusterStanzaRIDs(layout, segment, replicas, i32(replicas))
	if err != nil {
		t.Fatalf("selfOrdinal past the fleet: unexpected refusal: %v", err)
	}
	if len(beyond) != int(replicas) {
		t.Errorf("selfOrdinal %d: %d stanzas, want %d — there is no such provider to skip",
			replicas, len(beyond), replicas)
	}
}

// ADR-031 Consequences, "read replicas stay free": an RO pod consumes from the
// same providers, so it carries the same RIDs. It is never a provider itself,
// so no ordinal of the local block is its own and none is skipped.
func TestInClusterStanzaRIDsForAReadOnlyPodCoverEveryProviderWithTheSameNumbers(t *testing.T) {
	layout := ridcoord.DefaultLayout()
	const segment, replicas = int32(7), int32(3)

	ro, err := inClusterStanzaRIDs(layout, segment, replicas, nil)
	if err != nil {
		t.Fatalf("read-only pod: unexpected refusal: %v", err)
	}
	if len(ro) != int(replicas) {
		t.Fatalf("read-only pod: %d stanzas, want one per read-write provider (%d): %v", len(ro), replicas, ro)
	}

	// Every number an RO pod holds is the number the RW pods hold for that same
	// provider. Compared against a RW pod's own set rather than against
	// ridcoord directly, because the regression this guards is the two call
	// sites drifting apart — the RO builder is a second copy of the loop.
	rw, err := inClusterStanzaRIDs(layout, segment, replicas, i32(0))
	if err != nil {
		t.Fatalf("read-write pod: unexpected refusal: %v", err)
	}
	for ordinal, rid := range rw {
		if ro[ordinal] != rid {
			t.Errorf("provider %d: read-only pod holds rid %d, read-write pod holds %d — "+
				"they consume from the same provider and must name it the same",
				ordinal, ro[ordinal], rid)
		}
	}
}

// A refusal from ridcoord propagates as a refusal (RID-PLAN hazard 1). Never a
// capped ordinal, never a wrapped segment, never a partial map: olcSyncRepl is
// installed with one atomic Replace, so a set missing one provider is a
// deletion of that link (hazard 2).
func TestInClusterStanzaRIDsPropagateRidcoordRefusals(t *testing.T) {
	cases := []struct {
		name              string
		layout            ridcoord.Layout
		segment, replicas int32
		selfOrd           *int32
		want              error
	}{
		{
			name:    "an unusable layout refuses before any coordinate exists",
			layout:  ridcoord.Layout{}, // the Go zero value: a CR that never met API-server defaulting
			segment: 0, replicas: 3, selfOrd: i32(0),
			want: ridcoord.ErrInvalidLayout,
		},
		{
			name:    "a segment at the layout's ceiling has no run of RIDs",
			layout:  ridcoord.DefaultLayout(),
			segment: ridcoord.DefaultLayout().MaxSegments(), replicas: 3, selfOrd: i32(0),
			want: ridcoord.ErrSegmentOutOfRange,
		},
		{
			name:    "a negative segment names no coordinate",
			layout:  ridcoord.DefaultLayout(),
			segment: -1, replicas: 3, selfOrd: i32(0),
			want: ridcoord.ErrSegmentOutOfRange,
		},
		{
			// The boundary case that is wrong only THROUGH the arithmetic.
			// P=2, so ordinal 2 is one past the local block — and 1 + 0*4 + 2
			// = 3 is a perfectly legal RID, inside slapd's band, which is this
			// segment's link to peer site 0. A builder that does not refuse
			// here points an in-cluster stanza at a cross-site slot and the
			// two overwrite each other.
			name:    "more replicas than the layout's podsPerSite walks into the remote block",
			layout:  tightLayout(),
			segment: 0, replicas: 3, selfOrd: i32(0),
			want: ridcoord.ErrOrdinalOutOfRange,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := inClusterStanzaRIDs(tc.layout, tc.segment, tc.replicas, tc.selfOrd)
			if err == nil {
				t.Fatalf("no refusal; got rids %v — slapd accepts every one of these into a stanza", got)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want one wrapping %v: the refusal names which axis a human must change", err, tc.want)
			}
			if got != nil {
				t.Errorf("a refusal returned %d rids as well — a partial set is a deletion under the atomic Replace", len(got))
			}
		})
	}
}

func TestExternalStanzaRIDIsTheRemoteCoordinateOfThePeerSite(t *testing.T) {
	layout := ridcoord.DefaultLayout()
	const segment = int32(2)

	for site := int32(0); site < layout.MaxSites; site++ {
		want, err := layout.Remote(segment, site)
		if err != nil {
			t.Fatalf("ridcoord.Remote(%d, %d): %v", segment, site, err)
		}
		got, err := externalStanzaRID(layout, segment, site)
		if err != nil {
			t.Fatalf("peer site %d: unexpected refusal: %v", site, err)
		}
		if got != want {
			t.Errorf("peer site %d: rid = %d, want %d (ridcoord.Remote)", site, got, want)
		}
	}
}

// The boundary case the brief asks for, on the remote axis: a segment and a
// peer index each individually plausible, whose naive product lands in a
// NEIGHBOURING segment while staying inside slapd's band.
//
// Default layout: segment 48 is the last legal one (C = 49) and siteIndex 12 is
// what a thirteenth site would declare. 1 + 48*20 + 8 + 12 = 981 — below 999, so
// slapd takes it, and it decodes to segment 49 ordinal 0, a database that does
// not exist. Nothing downstream would notice.
func TestExternalStanzaRIDRefusesAPeerSitePastTheRemoteBlock(t *testing.T) {
	layout := ridcoord.DefaultLayout()
	const segment, siteIndex = int32(48), int32(12)

	const wouldBe = 981 // 1 + segment*M + P + siteIndex, if nothing checked

	got, err := externalStanzaRID(layout, segment, siteIndex)
	if err == nil {
		t.Fatalf("rid = %d for segment %d, peer site %d: no refusal. "+
			"That number is inside slapd's 0..999 band, so it is accepted into a stanza, "+
			"and it decodes to segment %d — a database this cluster does not have",
			got, segment, siteIndex, (got-1)/layout.SegmentWidth())
	}
	if !errors.Is(err, ridcoord.ErrSiteIndexOutOfRange) {
		t.Errorf("error = %v, want one wrapping %v — the refusal must name the axis the deployer can widen "+
			"(maxSites), not the one they cannot (the segment is legal)", err, ridcoord.ErrSiteIndexOutOfRange)
	}
	if got == wouldBe {
		t.Errorf("rid = %d was returned alongside the error: a refused coordinate must not also hand back "+
			"the number it refused", got)
	}

	// The same peer index one segment lower is still refused: this is a
	// property of the remote block's width, not of running out of band.
	if _, err := externalStanzaRID(layout, 0, siteIndex); !errors.Is(err, ridcoord.ErrSiteIndexOutOfRange) {
		t.Errorf("segment 0, peer site %d: error = %v, want %v", siteIndex, err, ridcoord.ErrSiteIndexOutOfRange)
	}
}

// One segment's two blocks share a run of RIDs, so the local and the remote
// derivations must never land on the same number for one database. ADR-031
// decision 1 guarantees it structurally (the blocks are disjoint); this pins
// that the seam preserves it, which is the property an off-by-one in either
// builder breaks.
func TestLocalAndRemoteStanzaRIDsOfOneDatabaseNeverCollide(t *testing.T) {
	for _, layout := range []ridcoord.Layout{ridcoord.DefaultLayout(), tightLayout()} {
		for segment := range int32(3) {
			seen := map[int32]string{}

			local, err := inClusterStanzaRIDs(layout, segment, layout.PodsPerSite, nil)
			if err != nil {
				t.Fatalf("layout %+v segment %d: in-cluster refusal: %v", layout, segment, err)
			}
			for ordinal, rid := range local {
				if prev, dup := seen[rid]; dup {
					t.Errorf("layout %+v segment %d: rid %d names both %s and in-cluster provider %d",
						layout, segment, rid, prev, ordinal)
				}
				seen[rid] = "in-cluster provider"
				if rid < 1 || rid > ridcoord.MaxRID {
					t.Errorf("layout %+v segment %d: in-cluster provider %d got rid %d, outside 1..%d",
						layout, segment, ordinal, rid, ridcoord.MaxRID)
				}
			}

			for site := int32(0); site < layout.MaxSites; site++ {
				rid, err := externalStanzaRID(layout, segment, site)
				if err != nil {
					t.Fatalf("layout %+v segment %d peer site %d: refusal: %v", layout, segment, site, err)
				}
				if prev, dup := seen[rid]; dup {
					t.Errorf("layout %+v segment %d: rid %d names both %s and the link to peer site %d — "+
						"slapd loads both stanzas and shares one connid between them",
						layout, segment, rid, prev, site)
				}
				seen[rid] = "a peer-site link"
				if rid < 1 || rid > ridcoord.MaxRID {
					t.Errorf("layout %+v segment %d: peer site %d got rid %d, outside 1..%d",
						layout, segment, site, rid, ridcoord.MaxRID)
				}
			}
		}
	}
}
