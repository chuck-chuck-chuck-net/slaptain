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
)

// olcSyncRepl is installed with one atomic modReq.Replace, so a stanza set
// shorter than the pod's current one DELETES the links it omits (RID-PLAN
// hazard 2). Three paths in the external-peer resolution skip a peer and carry
// on building: an unreadable bind-password Secret, a discovery peer with no
// providers, and a peer that resolved to no address. Each one tears a
// cross-site link off every pod of the site and reports nothing — replication
// for that link stops rather than pausing.
//
// These tests pin the verdict that replaces the three skips. They do NOT pin
// that the pods' existing stanzas survive: that is a property of a live
// cn=config, there is no slapd in a unit test, and an assertion here would be
// vacuously green. Same disclosure ridsegment_withhold_test.go makes for
// ADR-031 decision 6; the other half belongs to RID-PLAN M7.

// resolvedPeer is one declared peer, in whichever mode, as a fixture.
func declaredPeer(name string, discovery bool) ldapv1alpha1.ExternalPeer {
	ep := ldapv1alpha1.ExternalPeer{Name: name, SiteIndex: new(int32(1))}
	if discovery {
		ep.Discovery = &ldapv1alpha1.ExternalPeerDiscovery{
			KubeconfigSecret: ldapv1alpha1.KubeconfigSecretRef{Name: name + "-kubeconfig"},
		}
	} else {
		ep.PodAddresses = []string{"192.0.2.10"}
	}
	return ep
}

// TestJudgePeerResolutions is the whole of M4b's decision in one table: a peer
// that cannot be resolved withholds the DATABASE's stanza convergence for the
// pass, with a reason that says which peer and what failed.
func TestJudgePeerResolutions(t *testing.T) {
	unreadable := errors.New("secrets \"site-2-repl\" not found")

	for _, tc := range []struct {
		name       string
		peers      []ldapv1alpha1.ExternalPeer
		outcomes   []peerResolution
		wantOK     bool
		wantReason string
		naming     []string
		why        string
	}{
		{
			name:  "no external peers at all is nothing to resolve",
			peers: nil, outcomes: nil,
			wantOK: true,
			why: "a single-site cluster has no cross-site links, and withholding its in-cluster " +
				"stanzas over an empty list would stop replication at every site that has no peers",
		},
		{
			name:  "every peer resolving cleanly converges",
			peers: []ldapv1alpha1.ExternalPeer{declaredPeer("site-2", true), declaredPeer("site-3", false)},
			outcomes: []peerResolution{
				{Name: "site-2", Discovery: true, Addresses: 3},
				{Name: "site-3", Addresses: 1},
			},
			wantOK: true,
			why:    "the ordinary mesh: this is the path that must stay exactly as it is today",
		},
		{
			// The boundary the brief asks for, half one. An empty password is
			// a READ value: externalBindIdentity deliberately keeps it rather
			// than borrowing the per-database credential, so that a broken
			// override fails loudly at the consumer. It is not this path's
			// business, and refusing it here would be a behaviour change
			// M4b did not ask for.
			name:  "a bind-password Secret that was read but carries nothing still resolves",
			peers: []ldapv1alpha1.ExternalPeer{declaredPeer("site-2", true)},
			outcomes: []peerResolution{
				{Name: "site-2", Discovery: true, BindPasswordSecret: "site-2-repl", Addresses: 2},
			},
			wantOK: true,
			why: "\"could not read it\" is not \"not set\"; only the unreadable Secret withholds, " +
				"and judging on the password's value instead of the read's outcome would swap the two",
		},
		{
			// Half two of the same boundary: individually identical from the
			// stanza's point of view (both end with no credential), opposite
			// verdicts, and the difference is the read.
			name:  "a bind-password Secret that could not be read withholds the pass",
			peers: []ldapv1alpha1.ExternalPeer{declaredPeer("site-2", true)},
			outcomes: []peerResolution{
				{Name: "site-2", Discovery: true, BindPasswordSecret: "site-2-repl",
					SecretErr: unreadable, Addresses: 2},
			},
			wantReason: reasonPeerBindPasswordUnreadable,
			naming:     []string{"site-2", "site-2-repl", unreadable.Error()},
			why: "today it `continue`s: a transiently unreadable Secret deletes that peer's " +
				"stanzas from every pod, and the message is the only place an RBAC regression surfaces",
		},
		{
			name:  "a discovery peer that has resolved no provider withholds the pass",
			peers: []ldapv1alpha1.ExternalPeer{declaredPeer("site-2", true)},
			outcomes: []peerResolution{
				{Name: "site-2", Discovery: true, Addresses: 0},
			},
			wantReason: reasonPeerProvidersUndiscovered,
			naming:     []string{"site-2"},
			why: "M1 added two triggers to this path — an unparseable remote pod name and a " +
				"duplicate remote ordinal both publish as an empty provider list — and neither may ship",
		},
		{
			// The builder's `len(ep.URIs) == 0 { continue }`, which is the
			// third path and the one with no mode to hide behind: nothing
			// about this peer will resolve on a later pass.
			name:  "a peer that resolved to no address at all withholds the pass",
			peers: []ldapv1alpha1.ExternalPeer{declaredPeer("site-3", false)},
			outcomes: []peerResolution{
				{Name: "site-3", Addresses: 0},
			},
			wantReason: reasonPeerAddressesEmpty,
			naming:     []string{"site-3"},
			why: "a CR naming a peer site with nothing to connect to is a defect that does not heal, " +
				"and it must not read the same as discovery still working",
		},
		{
			// The property the milestone exists for, stated directly: a
			// healthy peer beside a broken one does not get a stanza set of
			// its own. A set holding only site-2 is a deletion of site-3's
			// link on every pod.
			name:  "one failing peer withholds the whole set, not just its own stanzas",
			peers: []ldapv1alpha1.ExternalPeer{declaredPeer("site-2", true), declaredPeer("site-3", true)},
			outcomes: []peerResolution{
				{Name: "site-2", Discovery: true, Addresses: 3},
				{Name: "site-3", Discovery: true, Addresses: 0},
			},
			wantReason: reasonPeerProvidersUndiscovered,
			naming:     []string{"site-3"},
			why: "this is the shape the backlog entry names: two peers, one broken, and the " +
				"converged set must not be the short one",
		},
		{
			// Determinism, pinned so the condition text cannot flap:
			// peer-major in DECLARED order. The first failing peer decides the
			// reason; every failing peer is named.
			name:  "two failing peers are named in declared order and the first decides the reason",
			peers: []ldapv1alpha1.ExternalPeer{declaredPeer("site-2", true), declaredPeer("site-3", false)},
			outcomes: []peerResolution{
				{Name: "site-2", Discovery: true, BindPasswordSecret: "site-2-repl",
					SecretErr: unreadable, Addresses: 2},
				{Name: "site-3", Addresses: 0},
			},
			wantReason: reasonPeerBindPasswordUnreadable,
			naming:     []string{"site-2", "site-3"},
			why: "a reason that depended on which fault was checked first, or a message naming " +
				"only one of two broken peers, sends a human to fix a coin flip",
		},
		{
			// Within one peer, the credential outranks the address list: a
			// peer can fail both ways at once, and the Secret is the fault
			// that does not resolve itself.
			name:  "an unreadable Secret outranks an empty address list on the same peer",
			peers: []ldapv1alpha1.ExternalPeer{declaredPeer("site-2", true)},
			outcomes: []peerResolution{
				{Name: "site-2", Discovery: true, BindPasswordSecret: "site-2-repl",
					SecretErr: unreadable, Addresses: 0},
			},
			wantReason: reasonPeerBindPasswordUnreadable,
			naming:     []string{"site-2"},
			why: "reporting the empty provider list would send a human to the remote cluster when " +
				"the local Secret is what failed, and discovery may never have been attempted",
		},
		{
			// The short set caught one level up. A resolution loop that still
			// `continue`s drops both the peer's stanzas AND its outcome, so a
			// judge inspecting only the outcomes it was handed would call this
			// complete and approve the deletion it describes.
			name:  "a declared peer with no outcome is unknown, not resolved",
			peers: []ldapv1alpha1.ExternalPeer{declaredPeer("site-2", true), declaredPeer("site-3", true)},
			outcomes: []peerResolution{
				{Name: "site-2", Discovery: true, Addresses: 3},
			},
			wantReason: reasonPeerResolutionIncomplete,
			naming:     []string{"site-3"},
			why: "the three skips are exactly a loop that produces fewer entries than it was given; " +
				"an unaccounted peer is unknown, and unknown withholds (RID-PLAN hazard 3's rule)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := judgePeerResolutions(tc.peers, tc.outcomes)

			if v.OK != tc.wantOK {
				t.Fatalf("OK = %v, want %v (reason %q, message %q) — %s",
					v.OK, tc.wantOK, v.Reason, v.Message, tc.why)
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

// TestJudgePeerResolutionsThreeCausesAreThreeReasons pins the property the
// table can only imply one row at a time: the three skips are three different
// things a human fixes differently — the local Secret or the RBAC that reads
// it; the remote cluster and the kubeconfig that reaches it; the CR itself —
// so conflating any two of them makes the reason useless as a diagnosis.
//
// The precedent is RIDSegmentUnverified beside RIDSegmentCollision, and
// RIDLayoutUnresolved beside RIDLayoutInvalid: identical withholds, deliberately
// distinct reasons, because alerting and slctl key on the reason.
func TestJudgePeerResolutionsThreeCausesAreThreeReasons(t *testing.T) {
	peers := []ldapv1alpha1.ExternalPeer{declaredPeer("site-2", true)}

	causes := map[string]peerResolution{
		"unreadable Secret": {Name: "site-2", Discovery: true,
			BindPasswordSecret: "site-2-repl", SecretErr: errors.New("forbidden"), Addresses: 1},
		"discovery with no providers": {Name: "site-2", Discovery: true, Addresses: 0},
		"no address at all":           {Name: "site-2", Addresses: 0},
	}

	seen := map[string]string{}
	for cause, outcome := range causes {
		v := judgePeerResolutions(peers, []peerResolution{outcome})
		if v.OK {
			t.Fatalf("%s: resolved — a peer that cannot be resolved must withhold the pass", cause)
		}
		if v.Reason == "" {
			t.Fatalf("%s: refused with an empty reason; a refusal with no diagnosis reads as "+
				"\"replication is just not working\", which is the state this milestone removes", cause)
		}
		if other, clash := seen[v.Reason]; clash {
			t.Errorf("%q and %q both report reason %q: they are fixed in different places "+
				"(the local Secret, the remote cluster, the CR), so one reason cannot serve both",
				cause, other, v.Reason)
		}
		seen[v.Reason] = cause
	}
}

// TestJudgePeerResolutionsFirstBringupSaysWhatItCosts pins the price of M4b's
// decision, in the one case where the decision is a BEHAVIOUR CHANGE.
//
// On a first bringup a discovery peer legitimately has no providers — nothing
// has run yet. Withholding then means the database writes no stanzas at all,
// its in-cluster ones included. That is correct (nothing is deleted, because
// nothing exists), and it is accepted deliberately: the alternative is to
// converge everything but the unresolved peer, which is the short set.
//
// But it means a PERMANENTLY broken peer — a bad kubeconfig, a remote RBAC
// denial — also blocks in-cluster replication, which previously worked. The
// whole mitigation is the message: it must name the peer, say what failed, and
// say that the in-cluster stanzas went with it. A human whose single-site
// replication stopped after adding a peer has to be able to read that from the
// condition, not infer it from a log line.
func TestJudgePeerResolutionsFirstBringupSaysWhatItCosts(t *testing.T) {
	peers := []ldapv1alpha1.ExternalPeer{declaredPeer("site-2", true)}
	v := judgePeerResolutions(peers, []peerResolution{{Name: "site-2", Discovery: true, Addresses: 0}})

	if v.OK {
		t.Fatalf("a discovery peer with no providers resolved: on a later pass that is a " +
			"deletion of the link, and the first pass is not distinguishable from it here")
	}
	if v.Reason != reasonPeerProvidersUndiscovered {
		t.Fatalf("reason = %q, want %q", v.Reason, reasonPeerProvidersUndiscovered)
	}

	msg := strings.ToLower(v.Message)
	for _, want := range []string{
		"site-2",     // which peer
		"discovery",  // what failed
		"kubeconfig", // where to look, because this is also what a bad kubeconfig looks like
		"in-cluster", // and what it costs: the local stanzas are withheld too
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not mention %q — a human whose in-cluster replication "+
				"stopped after declaring a peer must learn all four from this one sentence",
				v.Message, want)
		}
	}
}
