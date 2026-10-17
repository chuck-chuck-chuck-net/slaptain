package cmd

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
	"github.com/chuck-chuck-chuck-net/slaptain/operator/internal/ridcoord"
)

// `slctl rid decode` as an executable spec (ADR-031, RID-PLAN M6).
//
// The arithmetic lives in internal/ridcoord and is tested there. What is pinned
// here is everything ridcoord deliberately does NOT do, because it serves an
// operator and this serves a human:
//
//   - a RID in slapd's band but above the matrix is an ANSWER, not an error
//     (ADR-031 Consequences, final slctl bullet — the one requirement M6 was
//     given by name);
//   - every term that could not be resolved is said in words;
//   - the decode runs with no cluster at all, and runs better with one.
//
// The arithmetic reappears here only where the surface can get it wrong in a
// way ridcoord's own tests cannot see: the local/remote boundary moving with
// the layout, and a number that is individually plausible and only becomes
// wrong once a particular layout is applied.

const (
	// The default layout, spelled out so a reader need not recompute it:
	// P=8, S=12 → M=20, C=49, and the largest RID it can produce is C·M = 980.
	defaultSegmentWidth = int32(20)
	defaultMaxSegments  = int32(49)
	defaultLastRID      = int32(980)
)

// offlineRes is the base case: default layout, nothing consulted.
func offlineRes() ridResolution {
	return ridResolution{
		Layout:        ridcoord.DefaultLayout(),
		LayoutSource:  "default",
		LayoutTrusted: true,
	}
}

func withRIDLayout(r ridResolution, p, s int32) ridResolution {
	r.Layout = ridcoord.Layout{PodsPerSite: p, MaxSites: s}
	return r
}

func TestDecodeRID(t *testing.T) {
	tests := []struct {
		name string
		rid  int32
		res  ridResolution

		wantStatus    ridDecodeStatus
		wantSegment   *int32
		wantKind      ridcoord.Kind
		wantOrdinal   *int32
		wantSiteIndex *int32
		wantDatabases []string
		wantPod       string
		wantSite      string
		// wantSummaryHas / wantNotesHave are substring checks, case-insensitive.
		wantSummaryHas []string
		wantNotesHave  []string
		// wantNoNotes pins the clean path: a fully resolved coordinate has
		// nothing to apologise for.
		wantNoNotes bool
	}{
		// ── The coordinate, offline ───────────────────────────────────────
		{
			// ADR-031 decision 1: offset < P names a pod at the reader's own
			// site, by ordinal. rid 4 → 3 = 0·20 + 3.
			name:        "an offset below P names a pod at the reader's own site",
			rid:         4,
			res:         offlineRes(),
			wantStatus:  ridDecodeOK,
			wantSegment: new(int32(0)),
			wantKind:    ridcoord.KindLocal,
			wantOrdinal: new(int32(3)),
		},
		{
			// The last slot of the local block. One below the boundary.
			name:        "offset P-1 is still the local block",
			rid:         8,
			res:         offlineRes(),
			wantStatus:  ridDecodeOK,
			wantSegment: new(int32(0)),
			wantKind:    ridcoord.KindLocal,
			wantOrdinal: new(int32(7)),
		},
		{
			// The boundary itself: offset == P is the FIRST remote slot, peer
			// site index 0. Off-by-one here silently renames every peer.
			name:          "offset exactly P is the first remote slot, site index 0",
			rid:           9,
			res:           offlineRes(),
			wantStatus:    ridDecodeOK,
			wantSegment:   new(int32(0)),
			wantKind:      ridcoord.KindRemote,
			wantSiteIndex: new(int32(0)),
		},
		{
			// The ADR's own worked example (Consequences, slctl bullet):
			// 317 → 316 = 15·20 + 16, offset 16 ≥ P, peer site 16 − 8 = 8.
			name:          "the ADR's worked example: 317 is segment 15, peer site 8",
			rid:           317,
			res:           offlineRes(),
			wantStatus:    ridDecodeOK,
			wantSegment:   new(int32(15)),
			wantKind:      ridcoord.KindRemote,
			wantSiteIndex: new(int32(8)),
		},
		{
			// C·M exactly: the last RID the default layout can produce, and
			// the one the outside-matrix answer is measured against.
			name:          "the largest RID the default layout produces is a coordinate",
			rid:           defaultLastRID,
			res:           offlineRes(),
			wantStatus:    ridDecodeOK,
			wantSegment:   new(int32(48)),
			wantKind:      ridcoord.KindRemote,
			wantSiteIndex: new(int32(11)),
		},

		// ── The layout changes what a number MEANS ───────────────────────
		{
			// The boundary-discipline case. rid 9 is individually plausible
			// under any layout and decodes to two different KINDS of provider
			// depending on P: a peer link at the default (above), a pod of
			// one's own site here. Nothing about the input is invalid; it only
			// becomes wrong once the wrong layout is applied to it.
			name:        "the local/remote boundary moves with the layout",
			rid:         9,
			res:         withRIDLayout(offlineRes(), 12, 8),
			wantStatus:  ridDecodeOK,
			wantSegment: new(int32(0)),
			wantKind:    ridcoord.KindLocal,
			wantOrdinal: new(int32(8)),
		},
		{
			// Same shape, through the segment term instead of the offset term:
			// 950 at the default layout is a perfectly ordinary coordinate
			// (segment 47, peer site 1), and at P=50,S=50 (M=100, C=9) the
			// matrix stops at 900, so the same number is outside it. The
			// refusal is produced by the layout, not by the number.
			name:           "a RID the default layout accepts falls outside a wider layout's matrix",
			rid:            950,
			res:            withRIDLayout(offlineRes(), 50, 50),
			wantStatus:     ridDecodeOutsideMatrix,
			wantSummaryHas: []string{"900", "999"},
		},

		// ── The four ways a number is not a coordinate ───────────────────
		{
			// ADR-031 decision 1: rid 0 is reserved and never allocated, so a
			// zero is a bug and never a provider. Saying "out of range" here
			// would send the reader looking for a layout problem.
			name:           "rid 0 is reserved, never a coordinate",
			rid:            0,
			res:            offlineRes(),
			wantStatus:     ridDecodeReserved,
			wantSummaryHas: []string{"reserved"},
		},
		{
			// THE requirement M6 was given by name (ADR-031 Consequences,
			// final slctl bullet; RID-PLAN M6). 981 is the first RID inside
			// slapd's 0..999 band and outside the default matrix. ridcoord
			// refuses it; a diagnostic must answer it, naming BOTH ceilings —
			// the human pasted this off a real stanza, most likely one written
			// before the renumber.
			name:           "the first RID above the matrix is answered, not refused",
			rid:            981,
			res:            offlineRes(),
			wantStatus:     ridDecodeOutsideMatrix,
			wantSummaryHas: []string{"980", "999"},
		},
		{
			name:           "a RID well inside slapd's band but above the matrix is answered too",
			rid:            985,
			res:            offlineRes(),
			wantStatus:     ridDecodeOutsideMatrix,
			wantSummaryHas: []string{"980", "999"},
		},
		{
			// Above slapd's own ceiling: not a replica ID at all, which is a
			// different sentence from "not one of ours".
			name:           "above slapd's band is not a replica ID at all",
			rid:            1000,
			res:            offlineRes(),
			wantStatus:     ridDecodeOutsideBand,
			wantSummaryHas: []string{"999"},
		},
		{
			name:           "a negative number is outside the band",
			rid:            -1,
			res:            offlineRes(),
			wantStatus:     ridDecodeOutsideBand,
			wantSummaryHas: []string{"999"},
		},
		{
			// A zero Layout reaches this code from an unmarshalled CR or a
			// --layout typo (ADR-031 decision 5, third arithmetic bullet). The
			// fault is the layout, and reporting the RID as out of range would
			// point at the wrong thing.
			name:           "a zero layout is reported as the layout's fault, not the RID's",
			rid:            317,
			res:            withRIDLayout(offlineRes(), 0, 0),
			wantStatus:     ridDecodeLayoutUnusable,
			wantSummaryHas: []string{"layout"},
		},
		{
			// Both axes individually inside the band, their sum above it, so
			// no segment fits. Plausible at the input boundary, unusable
			// through the arithmetic.
			name:           "a layout whose segment width exceeds the band is unusable",
			rid:            317,
			res:            withRIDLayout(offlineRes(), 600, 600),
			wantStatus:     ridDecodeLayoutUnusable,
			wantSummaryHas: []string{"layout"},
		},
		{
			// ADR-031 decision 5: P=1, S=1 is a configuration and not a typo —
			// a single-pod standalone cluster. M=2, C=499, so rid 3 is
			// segment 1, offset 0, local ordinal 0.
			name:        "P=1 S=1 is a legal layout, not a typo",
			rid:         3,
			res:         withRIDLayout(offlineRes(), 1, 1),
			wantStatus:  ridDecodeOK,
			wantSegment: new(int32(1)),
			wantKind:    ridcoord.KindLocal,
			wantOrdinal: new(int32(0)),
		},

		// ── Resolution against a cluster ─────────────────────────────────
		{
			// The good path with everything readable: nothing is left to say,
			// so there are no notes at all.
			name: "a fully resolved coordinate carries no notes",
			rid:  317,
			res: ridResolution{
				Layout:        ridcoord.DefaultLayout(),
				LayoutSource:  `SlapdMesh "lab"`,
				LayoutTrusted: true,
				ClusterName:   "slapd",
				Replicas:      3,
				MeshName:      "lab",
				Databases:     map[int32][]string{15: {"customers"}},
				Sites:         map[int32]string{8: "site-i"},
			},
			wantStatus:    ridDecodeOK,
			wantSegment:   new(int32(15)),
			wantKind:      ridcoord.KindRemote,
			wantSiteIndex: new(int32(8)),
			wantDatabases: []string{"customers"},
			wantSite:      "site-i",
			wantNoNotes:   true,
		},
		{
			// "Say plainly what it could not resolve." A cluster WAS consulted
			// and declares no such segment — that is a finding about the
			// cluster, and silence here reads as "resolved to nothing".
			name: "an unresolved segment names the segment and the cluster",
			rid:  317,
			res: ridResolution{
				Layout:        ridcoord.DefaultLayout(),
				LayoutSource:  "default",
				LayoutTrusted: true,
				ClusterName:   "slapd",
				Databases:     map[int32][]string{0: {"default"}},
			},
			wantStatus:    ridDecodeOK,
			wantSegment:   new(int32(15)),
			wantKind:      ridcoord.KindRemote,
			wantSiteIndex: new(int32(8)),
			wantNotesHave: []string{"15", "slapd", "no slapddatabase"},
		},
		{
			// Offline is a different sentence from "looked and found nothing",
			// and conflating them is the one thing property 3 forbids.
			name:          "offline, an unresolved segment says no cluster was consulted",
			rid:           317,
			res:           offlineRes(),
			wantStatus:    ridDecodeOK,
			wantSegment:   new(int32(15)),
			wantKind:      ridcoord.KindRemote,
			wantSiteIndex: new(int32(8)),
			wantNotesHave: []string{"no cluster"},
		},
		{
			// ADR-031 decision 3 refuses a segment collision in the operator;
			// a decode that showed only the first database would hide exactly
			// the state the reader is hunting.
			name: "a segment declared twice names both databases",
			rid:  317,
			res: ridResolution{
				Layout:        ridcoord.DefaultLayout(),
				LayoutSource:  "default",
				LayoutTrusted: true,
				ClusterName:   "slapd",
				Databases:     map[int32][]string{15: {"customers", "staging"}},
			},
			wantStatus:    ridDecodeOK,
			wantSegment:   new(int32(15)),
			wantKind:      ridcoord.KindRemote,
			wantSiteIndex: new(int32(8)),
			wantDatabases: []string{"customers", "staging"},
			wantNotesHave: []string{"customers", "staging", "15"},
		},
		{
			// A mesh WAS consulted and declares no site on that index: a real
			// finding (a retired site, a stanza from before a renumber), and
			// the note has to name where it looked.
			name: "an unresolved site index names the index and the mesh",
			rid:  317,
			res: ridResolution{
				Layout:        ridcoord.DefaultLayout(),
				LayoutSource:  `SlapdMesh "lab"`,
				LayoutTrusted: true,
				ClusterName:   "slapd",
				MeshName:      "lab",
				Databases:     map[int32][]string{15: {"customers"}},
				Sites:         map[int32]string{0: "site-a"},
			},
			wantStatus:    ridDecodeOK,
			wantSegment:   new(int32(15)),
			wantKind:      ridcoord.KindRemote,
			wantSiteIndex: new(int32(8)),
			wantDatabases: []string{"customers"},
			wantSite:      "",
			wantNotesHave: []string{"8", "lab", "serveridindex"},
		},
		{
			// A local coordinate names a pod of the reader's own site, and the
			// cluster name is what turns an ordinal into that pod's name.
			name: "a local coordinate names the pod when the cluster is known",
			rid:  3,
			res: ridResolution{
				Layout:        ridcoord.DefaultLayout(),
				LayoutSource:  "default",
				LayoutTrusted: true,
				ClusterName:   "slapd",
				Replicas:      3,
				Databases:     map[int32][]string{0: {"default"}},
			},
			wantStatus:    ridDecodeOK,
			wantSegment:   new(int32(0)),
			wantKind:      ridcoord.KindLocal,
			wantOrdinal:   new(int32(2)),
			wantDatabases: []string{"default"},
			wantPod:       "slapd-2",
			wantNoNotes:   true,
		},
		{
			// Plausible at the input boundary — ordinal 4 is well inside P=8 —
			// and only wrong once the cluster's actual size is applied. This
			// is the shape a stale stanza from a scaled-down fleet has, and
			// the reader is owed the observation rather than a pod name that
			// does not exist.
			name: "a local ordinal beyond the cluster's replicas is flagged",
			rid:  5,
			res: ridResolution{
				Layout:        ridcoord.DefaultLayout(),
				LayoutSource:  "default",
				LayoutTrusted: true,
				ClusterName:   "slapd",
				Replicas:      3,
				Databases:     map[int32][]string{0: {"default"}},
			},
			wantStatus:    ridDecodeOK,
			wantSegment:   new(int32(0)),
			wantKind:      ridcoord.KindLocal,
			wantOrdinal:   new(int32(4)),
			wantDatabases: []string{"default"},
			wantPod:       "slapd-4",
			wantNotesHave: []string{"3", "slapd"},
		},
		{
			// An untrusted layout must be loud: every term downstream of it is
			// conditional on it, so a silent best-effort decode against the
			// wrong M is the worst answer this command can give.
			name: "a layout that could not be read is labelled, not silently assumed",
			rid:  317,
			res: ridResolution{
				Layout:        ridcoord.DefaultLayout(),
				LayoutSource:  `SlapdMesh "lab" (unreadable; showing the defaults)`,
				LayoutTrusted: false,
				ClusterName:   "slapd",
				MeshName:      "lab",
				Databases:     map[int32][]string{15: {"customers"}},
				Sites:         map[int32]string{8: "site-i"},
			},
			wantStatus:    ridDecodeOK,
			wantSegment:   new(int32(15)),
			wantKind:      ridcoord.KindRemote,
			wantSiteIndex: new(int32(8)),
			wantDatabases: []string{"customers"},
			wantSite:      "site-i",
			wantNotesHave: []string{"lab", "layout"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeRID(tc.rid, tc.res)

			if got.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.RID != tc.rid {
				t.Errorf("RID = %d, want %d — the result must echo what was asked", got.RID, tc.rid)
			}
			if got.Summary == "" {
				t.Errorf("Summary is empty; every status owes the reader a sentence")
			}

			assertI32Ptr(t, "Segment", got.Segment, tc.wantSegment)
			assertI32Ptr(t, "Ordinal", got.Ordinal, tc.wantOrdinal)
			assertI32Ptr(t, "SiteIndex", got.SiteIndex, tc.wantSiteIndex)

			if got.Kind != tc.wantKind {
				t.Errorf("Kind = %v, want %v", got.Kind, tc.wantKind)
			}
			if got.Pod != tc.wantPod {
				t.Errorf("Pod = %q, want %q", got.Pod, tc.wantPod)
			}
			if got.Site != tc.wantSite {
				t.Errorf("Site = %q, want %q", got.Site, tc.wantSite)
			}
			assertStrings(t, "Databases", got.Databases, tc.wantDatabases)

			for _, want := range tc.wantSummaryHas {
				if !strings.Contains(strings.ToLower(got.Summary), strings.ToLower(want)) {
					t.Errorf("Summary = %q, want it to mention %q", got.Summary, want)
				}
			}
			joined := strings.ToLower(strings.Join(got.Notes, " | "))
			for _, want := range tc.wantNotesHave {
				if !strings.Contains(joined, strings.ToLower(want)) {
					t.Errorf("Notes = %v, want one of them to mention %q", got.Notes, want)
				}
			}
			if tc.wantNoNotes && len(got.Notes) != 0 {
				t.Errorf("Notes = %v, want none — everything resolved", got.Notes)
			}
		})
	}
}

// A non-coordinate carries no coordinate terms. Stated separately because the
// table above asserts the absence per-field, and the property worth naming is
// the whole of it: a status that is not OK must not leave a half-filled
// coordinate behind for a caller to read as real.
func TestDecodeRIDLeavesNoPartialCoordinate(t *testing.T) {
	for _, rid := range []int32{0, 985, 1000, -7} {
		got := decodeRID(rid, offlineRes())
		if got.Status == ridDecodeOK {
			t.Fatalf("rid %d decoded as OK; it is not a coordinate of the default layout", rid)
		}
		if got.Segment != nil || got.Ordinal != nil || got.SiteIndex != nil {
			t.Errorf("rid %d (%s): coordinate terms are set (segment=%v ordinal=%v siteIndex=%v); "+
				"a non-coordinate must leave them nil",
				rid, got.Status, got.Segment, got.Ordinal, got.SiteIndex)
		}
		if got.Kind != 0 {
			t.Errorf("rid %d (%s): Kind = %v, want the zero Kind", rid, got.Status, got.Kind)
		}
	}
}

func TestParseRIDLayoutFlag(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    ridcoord.Layout
		wantErr bool
	}{
		{
			name: "the default layout, spelled",
			in:   "8x12",
			want: ridcoord.Layout{PodsPerSite: 8, MaxSites: 12},
		},
		{
			name: "pods first, sites second",
			in:   "12x8",
			want: ridcoord.Layout{PodsPerSite: 12, MaxSites: 8},
		},
		{
			name: "the smallest legal layout",
			in:   "1x1",
			want: ridcoord.Layout{PodsPerSite: 1, MaxSites: 1},
		},
		{
			name: "surrounding whitespace is trimmed",
			in:   "  8x12 ",
			want: ridcoord.Layout{PodsPerSite: 8, MaxSites: 12},
		},
		{
			name: "the separator is case-insensitive",
			in:   "8X12",
			want: ridcoord.Layout{PodsPerSite: 8, MaxSites: 12},
		},
		{
			// Syntactically two numbers, arithmetically unusable. It parses, and
			// decodeRID reports ridDecodeLayoutUnusable — so the message names
			// the arithmetic rather than the flag syntax.
			name: "a layout too wide to hold a segment still parses",
			in:   "600x600",
			want: ridcoord.Layout{PodsPerSite: 600, MaxSites: 600},
		},
		{
			name: "a negative axis still parses, and is the decode's problem",
			in:   "-1x12",
			want: ridcoord.Layout{PodsPerSite: -1, MaxSites: 12},
		},
		{
			// The boundary-discipline case, and the one this effort has already
			// paid for twice: 4294967304 = 2^32 + 8, which a naive Atoi plus an
			// int32 conversion turns into a perfectly plausible podsPerSite of
			// 8. The value is wrong only once the width of the destination is
			// applied to it, which is exactly where the M2 overflow hid.
			name:    "an axis that overflows int32 is refused, not wrapped",
			in:      "4294967304x12",
			wantErr: true,
		},
		{
			name:    "an empty value is not a layout",
			in:      "",
			wantErr: true,
		},
		{
			name:    "one number is not a layout",
			in:      "8",
			wantErr: true,
		},
		{
			name:    "a missing second axis is not a layout",
			in:      "8x",
			wantErr: true,
		},
		{
			name:    "three axes are not a layout",
			in:      "8x12x3",
			wantErr: true,
		},
		{
			name:    "a non-numeric axis is not a layout",
			in:      "ax12",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRIDLayoutFlag(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseRIDLayoutFlag(%q) = %+v, nil; want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRIDLayoutFlag(%q) returned %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parseRIDLayoutFlag(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

// ── buildRIDResolution ────────────────────────────────────────────────────────

func ridCluster(name, meshRef string, replicas int32) *ldapv1alpha1.SlapdCluster {
	return &ldapv1alpha1.SlapdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: ldapv1alpha1.SlapdClusterSpec{
			Replicas: replicas,
			MeshRef:  meshRef,
		},
	}
}

func ridDatabase(name, clusterRef string, segment *int32) ldapv1alpha1.SlapdDatabase {
	return ldapv1alpha1.SlapdDatabase{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: ldapv1alpha1.SlapdDatabaseSpec{
			ClusterRef: clusterRef,
			Replication: ldapv1alpha1.DatabaseReplicationConfig{
				RIDSegment: segment,
			},
		},
	}
}

func ridMesh(name string, layout *ldapv1alpha1.RIDLayout, sites ...ldapv1alpha1.MeshSite) *ldapv1alpha1.SlapdMesh {
	return &ldapv1alpha1.SlapdMesh{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: ldapv1alpha1.SlapdMeshSpec{
			Sites:     sites,
			RIDLayout: layout,
		},
	}
}

// The offline base case. No cluster is not a degraded mode: the default layout
// is a compiled-in constant and decoding against it is the whole offline
// promise (ADR-031 Consequences — "it needs no cluster state at all").
func TestBuildRIDResolutionOffline(t *testing.T) {
	res := buildRIDResolution(nil, nil, nil)

	if res.Layout != ridcoord.DefaultLayout() {
		t.Errorf("Layout = %+v, want the default %+v", res.Layout, ridcoord.DefaultLayout())
	}
	if !res.LayoutTrusted {
		t.Errorf("LayoutTrusted = false; the compiled-in default is not a fallback for anything")
	}
	if res.LayoutSource == "" {
		t.Errorf("LayoutSource is empty; the output states where the layout came from")
	}
	if res.Databases != nil {
		t.Errorf("Databases = %v, want nil — a nil map is what distinguishes "+
			"\"not consulted\" from \"consulted, found nothing\"", res.Databases)
	}
	if res.Sites != nil {
		t.Errorf("Sites = %v, want nil, for the same reason", res.Sites)
	}
	if res.ClusterName != "" || res.MeshName != "" {
		t.Errorf("ClusterName=%q MeshName=%q, want both empty", res.ClusterName, res.MeshName)
	}
}

func TestBuildRIDResolutionFromCluster(t *testing.T) {
	// A cluster with no meshRef uses the defaults and consults no mesh
	// (ADR-031 decision 5: such a cluster has only to be self-consistent).
	t.Run("no meshRef uses the default layout and resolves databases", func(t *testing.T) {
		sc := ridCluster("slapd", "", 3)
		dbs := []ldapv1alpha1.SlapdDatabase{
			ridDatabase("default", "slapd", new(int32(0))),
			ridDatabase("customers", "slapd", new(int32(15))),
		}

		res := buildRIDResolution(sc, nil, dbs)

		if res.Layout != ridcoord.DefaultLayout() {
			t.Errorf("Layout = %+v, want the default", res.Layout)
		}
		if !res.LayoutTrusted {
			t.Errorf("LayoutTrusted = false; a cluster outside a mesh has nothing unread")
		}
		if res.ClusterName != "slapd" || res.Replicas != 3 {
			t.Errorf("ClusterName=%q Replicas=%d, want slapd/3", res.ClusterName, res.Replicas)
		}
		assertStrings(t, "Databases[0]", res.Databases[0], []string{"default"})
		assertStrings(t, "Databases[15]", res.Databases[15], []string{"customers"})
		if res.Databases == nil {
			t.Errorf("Databases is nil although a cluster was consulted")
		}
		if res.Sites != nil {
			t.Errorf("Sites = %v, want nil — no mesh was consulted", res.Sites)
		}
	})

	// Only databases of THIS cluster, and only those declaring a segment.
	// A database naming another cluster is a different RID space entirely, and
	// one with no ridSegment occupies no column.
	t.Run("databases are filtered by clusterRef and by a declared segment", func(t *testing.T) {
		sc := ridCluster("slapd", "", 3)
		dbs := []ldapv1alpha1.SlapdDatabase{
			ridDatabase("mine", "slapd", new(int32(1))),
			ridDatabase("theirs", "other", new(int32(1))),
			ridDatabase("unnumbered", "slapd", nil),
		}

		res := buildRIDResolution(sc, nil, dbs)

		assertStrings(t, "Databases[1]", res.Databases[1], []string{"mine"})
		if len(res.Databases) != 1 {
			t.Errorf("Databases = %v, want exactly segment 1", res.Databases)
		}
	})

	// ADR-031 decision 3 makes a collision a refusal in the operator, so the
	// state exists in the world and the decode must be able to show it.
	t.Run("two databases on one segment are both recorded", func(t *testing.T) {
		sc := ridCluster("slapd", "", 3)
		dbs := []ldapv1alpha1.SlapdDatabase{
			ridDatabase("staging", "slapd", new(int32(7))),
			ridDatabase("customers", "slapd", new(int32(7))),
		}

		res := buildRIDResolution(sc, nil, dbs)

		assertStrings(t, "Databases[7]", res.Databases[7], []string{"customers", "staging"})
	})
}

func TestBuildRIDResolutionFromMesh(t *testing.T) {
	// An absent ridLayout means the documented defaults, not an unusable zero.
	t.Run("a mesh without ridLayout means the defaults", func(t *testing.T) {
		sc := ridCluster("slapd", "lab", 3)
		res := buildRIDResolution(sc, ridMesh("lab", nil), nil)

		if res.Layout != ridcoord.DefaultLayout() {
			t.Errorf("Layout = %+v, want the default", res.Layout)
		}
		if !res.LayoutTrusted {
			t.Errorf("LayoutTrusted = false; the mesh was read and declares the defaults")
		}
		if res.MeshName != "lab" {
			t.Errorf("MeshName = %q, want lab", res.MeshName)
		}
	})

	// Per-axis defaulting, mirroring the operator's own ridLayoutFor: API-server
	// defaulting runs on the API server's path and nowhere else, so a mesh
	// carrying only one axis reaches this code with a zero in the other
	// (ADR-031 decision 5, second arithmetic bullet). Taking the zero would
	// make M=4 and renumber everything this command then prints.
	t.Run("a half-declared ridLayout is defaulted per axis", func(t *testing.T) {
		sc := ridCluster("slapd", "lab", 3)
		mesh := ridMesh("lab", &ldapv1alpha1.RIDLayout{PodsPerSite: 4})

		res := buildRIDResolution(sc, mesh, nil)

		want := ridcoord.Layout{PodsPerSite: 4, MaxSites: 12}
		if res.Layout != want {
			t.Errorf("Layout = %+v, want %+v", res.Layout, want)
		}
	})

	t.Run("a fully declared ridLayout is taken verbatim", func(t *testing.T) {
		sc := ridCluster("slapd", "lab", 3)
		mesh := ridMesh("lab", &ldapv1alpha1.RIDLayout{PodsPerSite: 4, MaxSites: 6})

		res := buildRIDResolution(sc, mesh, nil)

		want := ridcoord.Layout{PodsPerSite: 4, MaxSites: 6}
		if res.Layout != want {
			t.Errorf("Layout = %+v, want %+v", res.Layout, want)
		}
	})

	// ADR-028: a site's slot is its DECLARED serverIDIndex, never its position
	// in spec.sites. A decode indexing by position renames every site the
	// moment someone sorts the YAML.
	t.Run("sites are indexed by serverIDIndex, not list position", func(t *testing.T) {
		sc := ridCluster("slapd", "lab", 3)
		mesh := ridMesh("lab", nil,
			ldapv1alpha1.MeshSite{Name: "site-c", ServerIDIndex: new(int32(5))},
			ldapv1alpha1.MeshSite{Name: "site-a", ServerIDIndex: new(int32(0))},
			ldapv1alpha1.MeshSite{Name: "site-b", ServerIDIndex: new(int32(2))},
		)

		res := buildRIDResolution(sc, mesh, nil)

		for idx, want := range map[int32]string{0: "site-a", 2: "site-b", 5: "site-c"} {
			if res.Sites[idx] != want {
				t.Errorf("Sites[%d] = %q, want %q", idx, res.Sites[idx], want)
			}
		}
		if len(res.Sites) != 3 {
			t.Errorf("Sites = %v, want three entries", res.Sites)
		}
	})

	t.Run("a site with no serverIDIndex claims no slot", func(t *testing.T) {
		sc := ridCluster("slapd", "lab", 3)
		mesh := ridMesh("lab", nil,
			ldapv1alpha1.MeshSite{Name: "site-a", ServerIDIndex: new(int32(0))},
			ldapv1alpha1.MeshSite{Name: "nameless"},
		)

		res := buildRIDResolution(sc, mesh, nil)

		if len(res.Sites) != 1 || res.Sites[0] != "site-a" {
			t.Errorf("Sites = %v, want only site-a at 0", res.Sites)
		}
	})

	// The operator withholds when the layout is unknown (RID-PLAN hazard 3).
	// A diagnostic cannot withhold — the user asked a question — so it answers
	// against the defaults and marks the answer untrusted, naming the mesh it
	// could not read.
	t.Run("a meshRef that could not be read answers untrusted, naming the mesh", func(t *testing.T) {
		sc := ridCluster("slapd", "lab", 3)

		res := buildRIDResolution(sc, nil, nil)

		if res.Layout != ridcoord.DefaultLayout() {
			t.Errorf("Layout = %+v, want the default as the stand-in", res.Layout)
		}
		if res.LayoutTrusted {
			t.Errorf("LayoutTrusted = true, but the declared layout was never read; "+
				"every term downstream of M is conditional on it (source=%q)", res.LayoutSource)
		}
		if !strings.Contains(res.LayoutSource, "lab") {
			t.Errorf("LayoutSource = %q, want it to name the unreadable mesh", res.LayoutSource)
		}
		if res.Sites != nil {
			t.Errorf("Sites = %v, want nil — the mesh was not read, so no site is known", res.Sites)
		}
	})
}

// ── Rendering ─────────────────────────────────────────────────────────────────

// The text is what a person actually meets, so the two things that must never
// be dropped on the way out are the sentence and the notes.
func TestRenderRIDDecodeText(t *testing.T) {
	r := decodeRID(317, ridResolution{
		Layout:        ridcoord.DefaultLayout(),
		LayoutSource:  `SlapdMesh "lab"`,
		LayoutTrusted: true,
		ClusterName:   "slapd",
		Replicas:      3,
		MeshName:      "lab",
		Databases:     map[int32][]string{15: {"customers"}},
		Sites:         map[int32]string{8: "site-i"},
	})
	out := renderRIDDecodeText(r)

	for _, want := range []string{"317", "15", "customers", "site-i", "8"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered text is missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, r.Summary) {
		t.Errorf("rendered text drops the summary %q:\n%s", r.Summary, out)
	}
}

func TestRenderRIDDecodeTextCarriesEveryNote(t *testing.T) {
	r := decodeRID(317, ridResolution{
		Layout:        ridcoord.DefaultLayout(),
		LayoutSource:  "default",
		LayoutTrusted: true,
		ClusterName:   "slapd",
		Databases:     map[int32][]string{0: {"default"}},
		Sites:         map[int32]string{0: "site-a"},
		MeshName:      "lab",
	})
	out := renderRIDDecodeText(r)

	if len(r.Notes) == 0 {
		t.Fatalf("expected notes for an unresolved segment and site index; got none")
	}
	for _, n := range r.Notes {
		if !strings.Contains(out, n) {
			t.Errorf("rendered text drops note %q:\n%s", n, out)
		}
	}
}

// The outside-matrix sentence is the deliverable of this whole command, so it
// is pinned at the surface a user reads and not only in the struct.
func TestRenderRIDDecodeTextExplainsAPreRenumberRID(t *testing.T) {
	out := renderRIDDecodeText(decodeRID(985, offlineRes()))

	for _, want := range []string{"985", "980", "999"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered text is missing %q; a RID off a pre-renumber stanza "+
				"needs both ceilings named:\n%s", want, out)
		}
	}
}

// ── JSON ──────────────────────────────────────────────────────────────────────

func TestBuildRIDDecodeJSON(t *testing.T) {
	r := decodeRID(317, ridResolution{
		Layout:        ridcoord.DefaultLayout(),
		LayoutSource:  `SlapdMesh "lab"`,
		LayoutTrusted: true,
		ClusterName:   "slapd",
		Replicas:      3,
		MeshName:      "lab",
		Databases:     map[int32][]string{15: {"customers"}},
		Sites:         map[int32]string{8: "site-i"},
	})
	j := buildRIDDecodeJSON(r, true)

	if j.RID != 317 || j.Status != string(ridDecodeOK) {
		t.Errorf("rid=%d status=%q, want 317/ok", j.RID, j.Status)
	}
	// The derived quantities are emitted so a consumer need not re-implement
	// the formula to know where the matrix ends.
	if j.Layout.SegmentWidth != defaultSegmentWidth || j.Layout.MaxSegments != defaultMaxSegments {
		t.Errorf("layout segmentWidth=%d maxSegments=%d, want %d/%d",
			j.Layout.SegmentWidth, j.Layout.MaxSegments, defaultSegmentWidth, defaultMaxSegments)
	}
	if j.Layout.PodsPerSite != 8 || j.Layout.MaxSites != 12 || !j.Layout.Trusted {
		t.Errorf("layout = %+v, want 8/12 trusted", j.Layout)
	}
	if j.Kind != ridcoord.KindRemote.String() {
		t.Errorf("kind = %q, want %q", j.Kind, ridcoord.KindRemote.String())
	}
	if j.Segment == nil || *j.Segment != 15 {
		t.Errorf("segment = %v, want 15", j.Segment)
	}
	if j.SiteIndex == nil || *j.SiteIndex != 8 {
		t.Errorf("siteIndex = %v, want 8", j.SiteIndex)
	}
	if j.Ordinal != nil {
		t.Errorf("ordinal = %v, want nil for a remote coordinate", j.Ordinal)
	}
	assertStrings(t, "databases", j.Databases, []string{"customers"})
	if j.Site != "site-i" {
		t.Errorf("site = %q, want site-i", j.Site)
	}
	if j.Summary == "" {
		t.Errorf("summary is empty")
	}
}

// A non-coordinate's JSON must omit the coordinate terms rather than emit
// zeros: a consumer reading `"segment": 0` would believe it.
func TestBuildRIDDecodeJSONOmitsTermsOfANonCoordinate(t *testing.T) {
	j := buildRIDDecodeJSON(decodeRID(985, offlineRes()), true)

	if j.Status != string(ridDecodeOutsideMatrix) {
		t.Fatalf("status = %q, want %q", j.Status, ridDecodeOutsideMatrix)
	}
	if j.Segment != nil || j.Ordinal != nil || j.SiteIndex != nil {
		t.Errorf("coordinate terms emitted for a non-coordinate: segment=%v ordinal=%v siteIndex=%v",
			j.Segment, j.Ordinal, j.SiteIndex)
	}
	if j.Kind != "" {
		t.Errorf("kind = %q, want empty", j.Kind)
	}
	if j.Summary == "" {
		t.Errorf("summary is empty; it is the whole answer for this status")
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func assertI32Ptr(t *testing.T, field string, got, want *int32) {
	t.Helper()
	switch {
	case want == nil && got == nil:
	case want == nil:
		t.Errorf("%s = %d, want nil", field, *got)
	case got == nil:
		t.Errorf("%s = nil, want %d", field, *want)
	case *got != *want:
		t.Errorf("%s = %d, want %d", field, *got, *want)
	}
}

func assertStrings(t *testing.T, field string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %v, want %v", field, got, want)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s[%d] = %q, want %q (full: %v)", field, i, got[i], want[i], got)
		}
	}
}
