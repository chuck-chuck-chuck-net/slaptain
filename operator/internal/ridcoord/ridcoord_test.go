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

package ridcoord

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
)

// secondLayout is the "one layout proves nothing" control: M=10, C=99, so
// every derived quantity differs from the default's and an implementation
// that hardcoded 20 or 49 cannot pass both.
var secondLayout = Layout{PodsPerSite: 4, MaxSites: 6}

// admissibleLayouts spans the axes the ADR's own sizing table walks, plus the
// two degenerate ends the bounds still admit: the narrowest segment (1+1) and
// the widest one that still yields a segment at all (M=999).
var admissibleLayouts = []Layout{
	{PodsPerSite: 1, MaxSites: 1},
	{PodsPerSite: 4, MaxSites: 6},
	{PodsPerSite: 6, MaxSites: 10},
	{PodsPerSite: 8, MaxSites: 12},
	{PodsPerSite: 8, MaxSites: 16},
	{PodsPerSite: 500, MaxSites: 499},
}

// wantSegmentWidth and wantMaxSegments are the test's OWN arithmetic, written
// straight from ADR-031 decision 5. They deliberately do not call the methods
// under test — a sweep bounded by the implementation it is checking proves
// nothing.
func wantSegmentWidth(l Layout) int32 { return l.PodsPerSite + l.MaxSites }
func wantMaxSegments(l Layout) int32  { return MaxRID / wantSegmentWidth(l) }

// TestDefaultLayoutIsTheMeshDefault pins ADR-031 decision 5's chosen
// constants: P=8, S=12 → M=20, C=49. These are the values SlapdMesh defaults
// to, and the row the ADR's sizing table emphasises.
func TestDefaultLayoutIsTheMeshDefault(t *testing.T) {
	l := DefaultLayout()
	if l.PodsPerSite != 8 || l.MaxSites != 12 {
		t.Fatalf("DefaultLayout() = %+v, want {PodsPerSite:8 MaxSites:12}", l)
	}
	if got := l.SegmentWidth(); got != 20 {
		t.Errorf("SegmentWidth() = %d, want 20 (M = P + S)", got)
	}
	if got := l.MaxSegments(); got != 49 {
		t.Errorf("MaxSegments() = %d, want 49 (C = floor(999/20))", got)
	}
}

// TestSegmentWidthIsPPlusS pins the segment width as a SUM and not a product —
// the single arithmetic choice that buys headroom on both axes at once
// (ADR-031 decision 5, "Because the segment is P + S and not P · S").
func TestSegmentWidthIsPPlusS(t *testing.T) {
	for _, l := range admissibleLayouts {
		t.Run(fmt.Sprintf("P%d_S%d", l.PodsPerSite, l.MaxSites), func(t *testing.T) {
			want := wantSegmentWidth(l)
			if got := l.SegmentWidth(); got != want {
				t.Errorf("SegmentWidth() = %d, want %d (= P + S; a product would be %d)",
					got, want, l.PodsPerSite*l.MaxSites)
			}
		})
	}
}

// TestMaxSegmentsReservesTheZeroRID pins C = floor(999/M), not floor(1000/M).
// The reserved zero costs exactly one segment (ADR-031 decision 1 and 5); the
// layouts below are chosen so the two formulas disagree, which is what makes
// this test fail if someone "fixes" the off-by-one.
func TestMaxSegmentsReservesTheZeroRID(t *testing.T) {
	for _, l := range admissibleLayouts {
		t.Run(fmt.Sprintf("P%d_S%d", l.PodsPerSite, l.MaxSites), func(t *testing.T) {
			m := wantSegmentWidth(l)
			want := wantMaxSegments(l)
			if got := l.MaxSegments(); got != want {
				t.Errorf("MaxSegments() = %d, want %d (floor(999/%d)); floor(1000/%d) would be %d",
					got, want, m, m, 1000/m)
			}
		})
	}

	// The two formulas must actually differ somewhere in the table above, or
	// the assertion has no teeth at all.
	differs := false
	for _, l := range admissibleLayouts {
		m := wantSegmentWidth(l)
		if MaxRID/m != 1000/m {
			differs = true
		}
	}
	if !differs {
		t.Fatal("no layout in admissibleLayouts distinguishes floor(999/M) from floor(1000/M); the off-by-one is untested")
	}
}

// TestRoundTripDefaultLayout walks EVERY legal coordinate at the default
// layout — 49 segments × 20 offsets = 980 values — and requires Decode to
// invert Local and Remote exactly, including which block the offset fell in
// (ADR-031 decision 1). Exhaustive is cheap here, and cheap exhaustive is the
// point: there is no sampling gap to hide an edge in.
func TestRoundTripDefaultLayout(t *testing.T) {
	assertRoundTrip(t, DefaultLayout())
}

// TestRoundTripSecondLayout repeats the sweep at M=10, C=99. One layout
// passing proves nothing about the arithmetic — every constant in the formula
// moves here.
func TestRoundTripSecondLayout(t *testing.T) {
	assertRoundTrip(t, secondLayout)
}

func assertRoundTrip(t *testing.T, l Layout) {
	t.Helper()
	c := wantMaxSegments(l)
	seen := 0

	for segment := range c {
		for ordinal := int32(0); ordinal < l.PodsPerSite; ordinal++ {
			rid, err := l.Local(segment, ordinal)
			if err != nil {
				t.Fatalf("Local(segment=%d, ordinal=%d) at %+v: unexpected error %v", segment, ordinal, l, err)
			}
			got, err := l.Decode(rid)
			if err != nil {
				t.Fatalf("Decode(%d) from Local(segment=%d, ordinal=%d) at %+v: unexpected error %v",
					rid, segment, ordinal, l, err)
			}
			want := Coordinate{Segment: segment, Kind: KindLocal, Ordinal: ordinal}
			if got != want {
				t.Fatalf("Decode(Local(segment=%d, ordinal=%d)) at %+v: rid=%d decoded to %+v, want %+v",
					segment, ordinal, l, rid, got, want)
			}
			seen++
		}
		for siteIndex := int32(0); siteIndex < l.MaxSites; siteIndex++ {
			rid, err := l.Remote(segment, siteIndex)
			if err != nil {
				t.Fatalf("Remote(segment=%d, siteIndex=%d) at %+v: unexpected error %v", segment, siteIndex, l, err)
			}
			got, err := l.Decode(rid)
			if err != nil {
				t.Fatalf("Decode(%d) from Remote(segment=%d, siteIndex=%d) at %+v: unexpected error %v",
					rid, segment, siteIndex, l, err)
			}
			want := Coordinate{Segment: segment, Kind: KindRemote, SiteIndex: siteIndex}
			if got != want {
				t.Fatalf("Decode(Remote(segment=%d, siteIndex=%d)) at %+v: rid=%d decoded to %+v, want %+v",
					segment, siteIndex, l, rid, got, want)
			}
			seen++
		}
	}

	if want := int(c * wantSegmentWidth(l)); seen != want {
		t.Fatalf("swept %d coordinates at %+v, want %d — the sweep bounds are wrong, not the implementation", seen, l, want)
	}
}

// TestBlockBoundaryIsAtP pins where the local block ends and the remote one
// begins: offset P-1 is the last pod ordinal, offset P is peer site 0
// (ADR-031 decision 1). The rid=317 case is the ADR's own worked example from
// the Consequences list.
func TestBlockBoundaryIsAtP(t *testing.T) {
	l := DefaultLayout()
	tests := []struct {
		name string
		rid  int32
		want Coordinate
	}{
		{"first RID is segment 0, local ordinal 0", 1, Coordinate{Segment: 0, Kind: KindLocal, Ordinal: 0}},
		{"last slot of the local block is ordinal P-1", 308, Coordinate{Segment: 15, Kind: KindLocal, Ordinal: 7}},
		{"first slot of the remote block is peer site 0", 309, Coordinate{Segment: 15, Kind: KindRemote, SiteIndex: 0}},
		{"ADR worked example: 317 is the link to peer site 8", 317, Coordinate{Segment: 15, Kind: KindRemote, SiteIndex: 8}},
		{"last slot of a segment is peer site S-1", 320, Coordinate{Segment: 15, Kind: KindRemote, SiteIndex: 11}},
		{"the very last legal RID", 980, Coordinate{Segment: 48, Kind: KindRemote, SiteIndex: 11}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := l.Decode(tc.rid)
			if err != nil {
				t.Fatalf("Decode(%d): unexpected error %v", tc.rid, err)
			}
			if got != tc.want {
				t.Errorf("Decode(%d) = %+v, want %+v", tc.rid, got, tc.want)
			}
		})
	}
}

// TestNeverProducesZeroRID pins ADR-031 decision 1's reservation: no legal
// input at any admissible layout may yield 0. Zero is what a forgotten int32
// assignment looks like, and slapd uses connid 0 for its own outbound client
// connections — a scheme where 0 is a legal coordinate cannot tell those apart.
func TestNeverProducesZeroRID(t *testing.T) {
	for _, l := range admissibleLayouts {
		t.Run(fmt.Sprintf("P%d_S%d", l.PodsPerSite, l.MaxSites), func(t *testing.T) {
			c := wantMaxSegments(l)
			for segment := range c {
				for ordinal := int32(0); ordinal < l.PodsPerSite; ordinal++ {
					if rid, err := l.Local(segment, ordinal); err != nil || rid == 0 {
						t.Fatalf("Local(segment=%d, ordinal=%d) at %+v = (%d, %v), want a non-zero RID and no error",
							segment, ordinal, l, rid, err)
					}
				}
				for siteIndex := int32(0); siteIndex < l.MaxSites; siteIndex++ {
					if rid, err := l.Remote(segment, siteIndex); err != nil || rid == 0 {
						t.Fatalf("Remote(segment=%d, siteIndex=%d) at %+v = (%d, %v), want a non-zero RID and no error",
							segment, siteIndex, l, rid, err)
					}
				}
			}
		})
	}
}

// TestMaximumLegalRIDFitsSlapdsBand pins the ceiling slapd actually enforces
// (SLAP_SYNC_RID_MAX = 999, ADR-031 "What slapd actually requires"). At the
// default layout the maximum is exactly 980 — asserted as a number, because a
// bound-only assertion would survive a layout that silently lost a segment.
func TestMaximumLegalRIDFitsSlapdsBand(t *testing.T) {
	if got := maxLegalRID(t, DefaultLayout()); got != 980 {
		t.Errorf("maximum RID at the default layout = %d, want 980 (= C·M = 49·20)", got)
	}
	for _, l := range admissibleLayouts {
		if got := maxLegalRID(t, l); got > MaxRID {
			t.Errorf("maximum RID at %+v = %d, exceeds slapd's ceiling of %d", l, got, MaxRID)
		}
	}
}

// maxLegalRID is the largest RID the layout can produce: the last slot of the
// last segment, which is a remote slot.
func maxLegalRID(t *testing.T, l Layout) int32 {
	t.Helper()
	rid, err := l.Remote(wantMaxSegments(l)-1, l.MaxSites-1)
	if err != nil {
		t.Fatalf("Remote(last segment, last site) at %+v: unexpected error %v", l, err)
	}
	return rid
}

// TestLocalAndRemoteNeverCollide pins that the two blocks of the matrix are
// disjoint across the WHOLE space, not just within one segment. A collision is
// silent in slapd — add_syncrepl has no duplicate check, two stanzas share a
// connid and become indistinguishable in the logs (ADR-031, "slapd does not
// reject a duplicate RID").
func TestLocalAndRemoteNeverCollide(t *testing.T) {
	for _, l := range []Layout{DefaultLayout(), secondLayout} {
		t.Run(fmt.Sprintf("P%d_S%d", l.PodsPerSite, l.MaxSites), func(t *testing.T) {
			type origin struct {
				kind string
				a, b int32
			}
			seen := map[int32]origin{}
			record := func(rid int32, o origin) {
				if prev, dup := seen[rid]; dup {
					t.Fatalf("rid=%d produced twice at %+v: %s(%d,%d) and %s(%d,%d)",
						rid, l, prev.kind, prev.a, prev.b, o.kind, o.a, o.b)
				}
				seen[rid] = o
			}
			c := wantMaxSegments(l)
			for segment := range c {
				for ordinal := int32(0); ordinal < l.PodsPerSite; ordinal++ {
					rid, err := l.Local(segment, ordinal)
					if err != nil {
						t.Fatalf("Local(segment=%d, ordinal=%d): unexpected error %v", segment, ordinal, err)
					}
					record(rid, origin{"Local", segment, ordinal})
				}
				for siteIndex := int32(0); siteIndex < l.MaxSites; siteIndex++ {
					rid, err := l.Remote(segment, siteIndex)
					if err != nil {
						t.Fatalf("Remote(segment=%d, siteIndex=%d): unexpected error %v", segment, siteIndex, err)
					}
					record(rid, origin{"Remote", segment, siteIndex})
				}
			}
			if want := int(c * wantSegmentWidth(l)); len(seen) != want {
				t.Fatalf("collected %d distinct RIDs at %+v, want %d", len(seen), l, want)
			}
		})
	}
}

// TestEachAxisRefusesAtItsOwnCeiling pins ADR-031 decision 5's "Exceeding any
// axis is a refusal naming which one": three axes, three sentinels, selected
// with errors.Is and never by string matching. A single shared error would
// pass a laxer test and leave the operator unable to tell "too many databases"
// from "too many pods".
func TestEachAxisRefusesAtItsOwnCeiling(t *testing.T) {
	l := DefaultLayout() // C=49, P=8, S=12

	tests := []struct {
		name string
		call func() (int32, error)
		want error
	}{
		{"Local refuses the segment at C", func() (int32, error) { return l.Local(49, 0) }, ErrSegmentOutOfRange},
		{"Local refuses a negative segment", func() (int32, error) { return l.Local(-1, 0) }, ErrSegmentOutOfRange},
		{"Local refuses the ordinal at P", func() (int32, error) { return l.Local(0, 8) }, ErrOrdinalOutOfRange},
		{"Local refuses a negative ordinal", func() (int32, error) { return l.Local(0, -1) }, ErrOrdinalOutOfRange},
		{"Remote refuses the segment at C", func() (int32, error) { return l.Remote(49, 0) }, ErrSegmentOutOfRange},
		{"Remote refuses a negative segment", func() (int32, error) { return l.Remote(-1, 0) }, ErrSegmentOutOfRange},
		{"Remote refuses the site index at S", func() (int32, error) { return l.Remote(0, 12) }, ErrSiteIndexOutOfRange},
		{"Remote refuses a negative site index", func() (int32, error) { return l.Remote(0, -1) }, ErrSiteIndexOutOfRange},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rid, err := tc.call()
			if !errors.Is(err, tc.want) {
				t.Fatalf("got (%d, %v), want an error matching %v", rid, err, tc.want)
			}
			if rid != 0 {
				t.Errorf("a refused coordinate returned rid=%d; a refusal must not hand back a usable number", rid)
			}
		})
	}
}

// TestRefusalNamesTheAxisAndItsCeiling pins the human half of the contract:
// three refusals mean three different human responses, so the message has to
// say which axis ran out and at what. This is deliberately a string assertion
// — the text IS the deliverable here; selection is errors.Is above.
func TestRefusalNamesTheAxisAndItsCeiling(t *testing.T) {
	l := DefaultLayout()
	tests := []struct {
		name    string
		call    func() (int32, error)
		mustSay []string
	}{
		{"segment refusal names the segment ceiling", func() (int32, error) { return l.Local(49, 0) }, []string{"segment", "49"}},
		{"ordinal refusal names the pods-per-site ceiling", func() (int32, error) { return l.Local(0, 8) }, []string{"ordinal", "8"}},
		{"site refusal names the max-sites ceiling", func() (int32, error) { return l.Remote(0, 12) }, []string{"site", "12"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.call()
			if err == nil {
				t.Fatal("want a refusal, got nil")
			}
			msg := strings.ToLower(err.Error())
			for _, want := range tc.mustSay {
				if !strings.Contains(msg, want) {
					t.Errorf("refusal %q does not mention %q", err.Error(), want)
				}
			}
		})
	}
}

// TestDecodeRefusesOutsideTheRIDRange pins the inverse's bounds: below 1
// (0 is reserved and negatives are nonsense) and above the last RID the layout
// can produce. 981..999 are inside slapd's band but outside THIS layout's
// matrix, and decoding one would invent a segment that no database can hold.
func TestDecodeRefusesOutsideTheRIDRange(t *testing.T) {
	l := DefaultLayout() // last legal RID is 980

	tests := []struct {
		name string
		rid  int32
	}{
		{"zero is reserved, never a coordinate", 0},
		{"negative is nonsense", -1},
		{"one past the last legal RID", 981},
		{"inside slapd's band but outside the matrix", 999},
		{"above slapd's band entirely", 1000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := l.Decode(tc.rid)
			if !errors.Is(err, ErrRIDOutOfRange) {
				t.Fatalf("Decode(%d) = (%+v, %v), want an error matching %v", tc.rid, got, err, ErrRIDOutOfRange)
			}
		})
	}
}

// TestValidateRejectsUnusableLayouts pins the admissibility rule: both axes at
// least 1, and a segment width that still leaves room for one segment — C < 1 is
// a layout that can express no database at all (ADR-031 decision 5).
func TestValidateRejectsUnusableLayouts(t *testing.T) {
	tests := []struct {
		name   string
		layout Layout
		valid  bool
	}{
		{"the mesh default", DefaultLayout(), true},
		{"the narrowest usable layout", Layout{PodsPerSite: 1, MaxSites: 1}, true},
		{"the widest segment that still yields one", Layout{PodsPerSite: 500, MaxSites: 499}, true},
		{"zero pods per site", Layout{PodsPerSite: 0, MaxSites: 12}, false},
		{"negative pods per site", Layout{PodsPerSite: -1, MaxSites: 12}, false},
		{"zero sites", Layout{PodsPerSite: 8, MaxSites: 0}, false},
		{"negative sites", Layout{PodsPerSite: 8, MaxSites: -3}, false},
		{"both axes zero", Layout{}, false},
		{"segment wider than the RID band leaves no segment", Layout{PodsPerSite: 500, MaxSites: 500}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.layout.Validate()
			if tc.valid {
				if err != nil {
					t.Fatalf("Validate() on %+v = %v, want nil", tc.layout, err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidLayout) {
				t.Fatalf("Validate() on %+v = %v, want an error matching %v", tc.layout, err, ErrInvalidLayout)
			}
		})
	}
}

// TestInvalidLayoutRefusesBeforeDividing pins that an unusable layout is a
// refusal on every entry point rather than a panic. M = 0 is the live hazard:
// C = 999/M divides by zero, and the zero Layout is exactly what an
// unmarshalled-but-unset SlapdMesh.spec.ridLayout produces.
func TestInvalidLayoutRefusesBeforeDividing(t *testing.T) {
	for _, l := range []Layout{{}, {PodsPerSite: -1, MaxSites: -1}, {PodsPerSite: 500, MaxSites: 500}} {
		t.Run(fmt.Sprintf("P%d_S%d", l.PodsPerSite, l.MaxSites), func(t *testing.T) {
			if got := l.MaxSegments(); got != 0 {
				t.Errorf("MaxSegments() on the unusable layout %+v = %d, want 0 and no panic", l, got)
			}
			if _, err := l.Local(0, 0); !errors.Is(err, ErrInvalidLayout) {
				t.Errorf("Local(0,0) on %+v = %v, want an error matching %v", l, err, ErrInvalidLayout)
			}
			if _, err := l.Remote(0, 0); !errors.Is(err, ErrInvalidLayout) {
				t.Errorf("Remote(0,0) on %+v = %v, want an error matching %v", l, err, ErrInvalidLayout)
			}
			if _, err := l.Decode(1); !errors.Is(err, ErrInvalidLayout) {
				t.Errorf("Decode(1) on %+v = %v, want an error matching %v", l, err, ErrInvalidLayout)
			}
		})
	}
}

// TestZeroKindIsNotAKind is a tripwire, not a behaviour test: it protects the
// `iota + 1` on the Kind enumeration. The zero Kind must stay unexpressible
// for the same reason rid = 0 is reserved — zero is what a forgotten
// assignment produces, and an enumeration whose zero value silently means
// "local provider" cannot tell a decoded coordinate from an unset Coordinate.
//
// If you reverted KindLocal to `iota` and landed here: every Coordinate{} in
// the codebase now reads as a valid local provider. Put the +1 back, or write
// the spec that says why a zero Kind is safe.
func TestZeroKindIsNotAKind(t *testing.T) {
	if KindLocal == 0 {
		t.Error("KindLocal == 0: the zero Kind is expressible again; the enumeration must start at iota + 1")
	}
	if KindRemote == 0 {
		t.Error("KindRemote == 0: the zero Kind is expressible again")
	}
	if KindLocal == KindRemote {
		t.Fatalf("KindLocal == KindRemote == %d: the two blocks are indistinguishable", int(KindLocal))
	}
	// The failure messages above are only legible if String() stays honest
	// about a value it does not know.
	if got := Kind(0).String(); got == KindLocal.String() || got == KindRemote.String() {
		t.Errorf("Kind(0).String() = %q, which collides with a real Kind", got)
	}
}

// TestAxisLargeEnoughToOverflowTheSumIsRefused pins that each axis is bounded
// against MaxRID BEFORE the two are added, so the addition cannot overflow.
// Bounding first is free — 999 + 999 fits an int32 with room to spare — and
// bounding only the sum is a guard that silently inverts: P = S = MaxInt32
// sums to -2, which is not greater than 999, so the layout passes.
//
// The failure that follows is not a wrong RID, which is why the rest of this
// suite does not see it. It is a wrong REFUSAL: SegmentWidth answers -2,
// MaxSegments answers 999 / -2 = -499, and Local(0, 0) then refuses segment 0
// as "outside 0..-500" with ErrSegmentOutOfRange — sending a human to look at
// the segment ledger when the layout is what is broken. A refusal that names
// the wrong axis is worse than no refusal, because it is actionable and the
// action is wrong.
func TestAxisLargeEnoughToOverflowTheSumIsRefused(t *testing.T) {
	tests := []struct {
		name   string
		layout Layout
	}{
		// MaxInt32 + MaxInt32 wraps to -2: positive axes, negative width.
		{"both axes at MaxInt32 wrap the width to -2", Layout{PodsPerSite: math.MaxInt32, MaxSites: math.MaxInt32}},
		// One huge axis is enough; MaxInt32 + 1 wraps to MinInt32.
		{"one axis at MaxInt32 wraps the width to MinInt32", Layout{PodsPerSite: math.MaxInt32, MaxSites: 1}},
		{"the other axis at MaxInt32 wraps it just the same", Layout{PodsPerSite: 1, MaxSites: math.MaxInt32}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := tc.layout

			if err := l.Validate(); !errors.Is(err, ErrInvalidLayout) {
				t.Errorf("Validate() on %+v = %v, want an error matching %v (the sum %d overflowed)",
					l, err, ErrInvalidLayout, l.PodsPerSite+l.MaxSites)
			}
			if got := l.SegmentWidth(); got != 0 {
				t.Errorf("SegmentWidth() on %+v = %d, want 0; an unusable layout must not report a width",
					l, got)
			}
			if got := l.MaxSegments(); got != 0 {
				t.Errorf("MaxSegments() on %+v = %d, want 0; a negative segment count is not a ceiling",
					l, got)
			}

			// The refusal must name the layout, not an axis of the matrix the
			// layout failed to define. This is the half the defect actually
			// breaks: it refuses, but as ErrSegmentOutOfRange.
			if _, err := l.Local(0, 0); !errors.Is(err, ErrInvalidLayout) {
				t.Errorf("Local(0, 0) on %+v = %v, want an error matching %v", l, err, ErrInvalidLayout)
			}
			if _, err := l.Remote(0, 0); !errors.Is(err, ErrInvalidLayout) {
				t.Errorf("Remote(0, 0) on %+v = %v, want an error matching %v", l, err, ErrInvalidLayout)
			}
			if _, err := l.Decode(1); !errors.Is(err, ErrInvalidLayout) {
				t.Errorf("Decode(1) on %+v = %v, want an error matching %v", l, err, ErrInvalidLayout)
			}
		})
	}
}
