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
	"strings"
	"testing"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
	"github.com/chuck-chuck-chuck-net/slaptain/operator/internal/ridcoord"
)

// Three findings of the M4a correctness review, all of them the same shape: the
// operator does the RIGHT thing and says the WRONG thing, or nothing at all.
// A withhold that nobody can diagnose is only half of ADR-031 decision 6 —
// "writes no stanzas AND reports the reason" — and the half that is missing is
// the half a human reads.

// R3. Scale a cluster past the layout's local block and every stanza is
// withheld, correctly: inClusterStanzaRIDs refuses ordinal 8 when podsPerSite
// is 8. But the refusal happens inside reconcileReplication, so SyncreplStanzasReady
// stays True, the phase reads "applied to 9 pods, 0 failed", and the only
// mention of podsPerSite is a log line. ADR-031 decision 5 requires a refusal
// naming the axis and its ceiling.
func TestJudgeClusterFleetRefusesAFleetLargerThanTheLocalBlock(t *testing.T) {
	def := ridcoord.DefaultLayout() // P=8

	for _, tc := range []struct {
		name       string
		replicas   int32
		readOnly   int32
		layout     ridcoord.Layout
		wantOK     bool
		wantReason string
		naming     []string
		why        string
	}{
		{
			name:     "a fleet inside the local block converges",
			replicas: 8, layout: def, wantOK: true,
			why: "podsPerSite 8 means ordinals 0..7, so 8 replicas is the largest fleet that fits exactly",
		},
		{
			name:     "one pod past podsPerSite is refused, naming the axis and the ceiling",
			replicas: 9, layout: def,
			wantReason: reasonRIDPodsPerSiteExceeded,
			naming:     []string{"podsPerSite", "9", "8"},
			why: "the deployer's response is to widen podsPerSite on the mesh or scale back down, " +
				"and neither is guessable from \"applied to 9 pods, 0 failed\"",
		},
		{
			// Individually plausible, wrong only against the layout — the same
			// boundary discipline the coordinate functions get. 4 replicas is
			// an ordinary cluster; it is this MESH that declared a narrow
			// local block.
			name:     "a fleet legal at the default layout is refused under a narrower one",
			replicas: 4, layout: ridcoord.Layout{PodsPerSite: 2, MaxSites: 4},
			wantReason: reasonRIDPodsPerSiteExceeded,
			naming:     []string{"podsPerSite", "2"},
			why:        "a layout the mesh narrowed must be obeyed, not silently widened back to the defaults",
		},
		{
			// An RO pod is never a provider, so it occupies no slot in the
			// local block (ADR-031 Consequences, "read replicas stay free").
			// Counting them would refuse a perfectly legal cluster and withhold
			// its whole mesh.
			name:     "read replicas do not count against podsPerSite",
			replicas: 3, readOnly: 20, layout: def, wantOK: true,
			why: "an RO pod consumes from providers and is never one, so it takes no ordinal",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := ridCluster("")
			sc.Spec.Replicas = tc.replicas
			sc.Spec.ReadReplicas = tc.readOnly
			sc.Spec.Replication.Enabled = true

			v := judgeClusterFleet(sc, tc.layout)
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

// R1, this milestone's regression, as a verdict.
//
// A consumer-only cluster has no in-cluster mesh (ADR-010), so reconcileReplication
// hands the builders an in-cluster provider count of 0. The RW builder suppresses
// its own in-cluster block explicitly and is fine. The READ-ONLY builder has no
// such suppression: it loops over the read-write fleet and looks every ordinal up
// in an EMPTY map, misses every one, skips every stanza, and hands
// applySyncreplToPod a set of nothing — which is installed with one atomic
// Replace. Measured by the reviewer: 3 stanzas before, 0 after.
//
// That those links may well be dead anyway — a consumer-only pod carries no
// syncprov, so there is nothing for an RO pod to consume from — is an argument
// for suppressing them EXPLICITLY AND SAYING SO, which is what a refusal is. It
// is not an argument for arriving at an empty stanza set through a map lookup
// that misses.
//
// The combination is already refused by a CEL rule on SlapdClusterSpec. It is
// refused again here for the reason the siteIndex rule is: a CR can reach a
// controller without passing that admission — an older object, a restore, a
// webhook turned off — and the cost of trusting admission is the deletion above.
func TestJudgeClusterFleetRefusesConsumerOnlyWithReadReplicas(t *testing.T) {
	def := ridcoord.DefaultLayout()

	mk := func(mode string, readReplicas int32) *ldapv1alpha1.SlapdCluster {
		sc := ridCluster("")
		sc.Spec.Replicas = 2
		sc.Spec.ReadReplicas = readReplicas
		sc.Spec.Replication.Enabled = true
		sc.Spec.Replication.Mode = mode
		return sc
	}

	t.Run("consumer-only with read replicas is refused", func(t *testing.T) {
		// The mode string verbatim rather than a constant: the API declares it
		// as a CEL enum and IsConsumerOnly compares it literally, so a
		// constant here would only hide a rename from this test.
		sc := mk("consumer-only", 2)
		if !sc.IsConsumerOnly() {
			t.Fatalf("fixture is not consumer-only; the mode constant moved")
		}

		v := judgeClusterFleet(sc, def)
		if v.OK {
			t.Fatalf("accepted: every read-only pod's olcSyncRepl is about to be Replace()d with " +
				"an empty set, which deletes live replication and raises nothing")
		}
		if v.Reason != reasonConsumerOnlyReadReplicas {
			t.Errorf("reason = %q, want %q", v.Reason, reasonConsumerOnlyReadReplicas)
		}
		for _, want := range []string{replicationModeConsumerOnly, "readReplicas"} {
			if !strings.Contains(v.Message, want) {
				t.Errorf("message %q does not name %q", v.Message, want)
			}
		}
	})

	t.Run("consumer-only without read replicas is the supported shape", func(t *testing.T) {
		if v := judgeClusterFleet(mk("consumer-only", 0), def); !v.OK {
			t.Fatalf("refused a plain consumer-only cluster: %s (%s)", v.Reason, v.Message)
		}
	})

	t.Run("read replicas on a peer cluster are untouched", func(t *testing.T) {
		if v := judgeClusterFleet(mk("", 2), def); !v.OK {
			t.Fatalf("refused an ordinary cluster with read replicas: %s (%s)", v.Reason, v.Message)
		}
	})
}

// R1's general shape, one level down: a lookup that misses is a refusal, not a
// continue. Both builders read the RID map with `coord, known := rids[i]` and
// skip on a miss, which is why the consumer-only bug above produced an empty
// set rather than an error. It is the same class of bug selfOrdinal's pointer
// type exists to make unexpressible: a convention a call site has to remember.
func TestStanzaRIDForRefusesAMissingOrdinal(t *testing.T) {
	rids := map[int32]int32{0: 21, 2: 23}

	for ordinal, want := range map[int32]int32{0: 21, 2: 23} {
		got, err := stanzaRIDFor(rids, ordinal)
		if err != nil {
			t.Fatalf("ordinal %d: unexpected refusal: %v", ordinal, err)
		}
		if got != want {
			t.Errorf("ordinal %d: rid = %d, want %d", ordinal, got, want)
		}
	}

	for _, missing := range []int32{1, 3, -1} {
		got, err := stanzaRIDFor(rids, missing)
		if err == nil {
			t.Errorf("ordinal %d: no refusal, rid = %d. A miss that returns a number is worse than a "+
				"miss that skips: 0 is a legal int32 and an illegal RID", missing, got)
			continue
		}
		if !strings.Contains(err.Error(), "1") && missing == 1 {
			t.Errorf("ordinal 1: error %q does not name the ordinal that was missing", err)
		}
	}

	// The empty map is the consumer-only case exactly: the builder is asked to
	// address a fleet and holds nothing for any of it.
	if _, err := stanzaRIDFor(map[int32]int32{}, 0); err == nil {
		t.Errorf("an empty map answered for ordinal 0: that is the precise shape of the read-only " +
			"regression — every lookup misses and the whole stanza set silently becomes empty")
	}
	if _, err := stanzaRIDFor(nil, 0); err == nil {
		t.Errorf("a nil map answered for ordinal 0")
	}
}

// R2. ridLayoutFor returns a zero layout plus an error; the caller logs the
// error and passes the zero layout on, so judgeRIDSegment publishes
// "RIDLayoutInvalid — fix spec.ridLayout on the SlapdMesh" when there may be
// nothing wrong with ridLayout at all and the API server, RBAC or the cache is
// what failed.
//
// This is the distinction ridsegment.go argues at length when it introduces
// reasonRIDSegmentUnverified for a failed List, applied in the other direction:
// could-not-read is not is-wrong, and a reason is a diagnosis.
func TestJudgeRIDLayoutSeparatesUnreadableFromUnusable(t *testing.T) {
	sc := ridCluster(testMeshName)
	boom := errors.New("etcdserver: request timed out")

	t.Run("an unreadable layout is Unresolved, not Invalid", func(t *testing.T) {
		v := judgeRIDLayout(sc, ridcoord.Layout{}, boom)
		if v == nil {
			t.Fatalf("no refusal: an unknown layout must withhold (RID-PLAN hazard 3)")
		}
		if v.OK {
			t.Fatalf("verdict is OK while the layout could not be read")
		}
		if v.Reason != reasonRIDLayoutUnresolved {
			t.Errorf("reason = %q, want %q — %q sends a human to edit spec.ridLayout, which may be "+
				"perfectly correct; what failed is the read", v.Reason, reasonRIDLayoutUnresolved, v.Reason)
		}
		if !strings.Contains(v.Message, boom.Error()) {
			t.Errorf("message %q drops the underlying error, which is the only thing that tells an "+
				"operator whether this is RBAC, the cache or the API server", v.Message)
		}
		if !strings.Contains(v.Message, testMeshName) {
			t.Errorf("message %q does not name the mesh that could not be read", v.Message)
		}
	})

	t.Run("a declared but unusable layout stays Invalid", func(t *testing.T) {
		// No error: the mesh was read perfectly well and what it declares
		// admits no segment. Here "fix spec.ridLayout" is exactly right.
		v := judgeRIDLayout(sc, ridcoord.Layout{PodsPerSite: 0, MaxSites: 0}, nil)
		if v == nil || v.OK {
			t.Fatalf("a layout that admits no segment was accepted: %+v", v)
		}
		if v.Reason != reasonRIDLayoutInvalid {
			t.Errorf("reason = %q, want %q", v.Reason, reasonRIDLayoutInvalid)
		}
	})

	t.Run("a usable layout yields no verdict at all", func(t *testing.T) {
		if v := judgeRIDLayout(sc, ridcoord.DefaultLayout(), nil); v != nil {
			t.Fatalf("a usable layout produced a verdict %+v: nil is what lets the segment judgement "+
				"proceed, and a non-nil OK verdict would have to be unwrapped by every caller", v)
		}
	})
}
