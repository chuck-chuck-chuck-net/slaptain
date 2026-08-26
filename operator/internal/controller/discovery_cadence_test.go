/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

func peerStatus(name string, addrs ...string) ldapv1alpha1.ExternalPeerStatus {
	return ldapv1alpha1.ExternalPeerStatus{Name: name, DiscoveredAddresses: addrs}
}

func TestPeerAddressesUnsettled(t *testing.T) {
	errored := peerStatus("siteB", "10.0.0.1")
	errored.LastError = "dial tcp: i/o timeout"

	cases := []struct {
		name      string
		prev, cur []ldapv1alpha1.ExternalPeerStatus
		errored   bool
		want      bool
	}{
		{
			name: "no peers at all",
			want: false,
		},
		{
			name: "unchanged address set",
			prev: []ldapv1alpha1.ExternalPeerStatus{peerStatus("siteB", "10.0.0.1", "10.0.0.2")},
			cur:  []ldapv1alpha1.ExternalPeerStatus{peerStatus("siteB", "10.0.0.1", "10.0.0.2")},
			want: false,
		},
		{
			name: "address added — a replacement pod came back",
			prev: []ldapv1alpha1.ExternalPeerStatus{peerStatus("siteB", "10.0.0.1")},
			cur:  []ldapv1alpha1.ExternalPeerStatus{peerStatus("siteB", "10.0.0.1", "10.0.0.2")},
			want: true,
		},
		{
			name: "address removed — a pod went away",
			prev: []ldapv1alpha1.ExternalPeerStatus{peerStatus("siteB", "10.0.0.1", "10.0.0.2")},
			cur:  []ldapv1alpha1.ExternalPeerStatus{peerStatus("siteB", "10.0.0.1")},
			want: true,
		},
		{
			name: "address replaced — same count, different IP",
			prev: []ldapv1alpha1.ExternalPeerStatus{peerStatus("siteB", "10.0.0.1", "10.0.0.2")},
			cur:  []ldapv1alpha1.ExternalPeerStatus{peerStatus("siteB", "10.0.0.1", "10.0.0.9")},
			want: true,
		},
		{
			// discoverRemotePeerAddresses sorts, so a different order is the same
			// set observed twice. Treating it as churn would pin the cluster at
			// the fast interval forever.
			name: "reordered — must NOT count as churn",
			prev: []ldapv1alpha1.ExternalPeerStatus{peerStatus("siteB", "10.0.0.2", "10.0.0.1")},
			cur:  []ldapv1alpha1.ExternalPeerStatus{peerStatus("siteB", "10.0.0.1", "10.0.0.2")},
			want: false,
		},
		{
			name:    "discovery error this pass",
			prev:    []ldapv1alpha1.ExternalPeerStatus{peerStatus("siteB", "10.0.0.1")},
			cur:     []ldapv1alpha1.ExternalPeerStatus{peerStatus("siteB", "10.0.0.1")},
			errored: true,
			want:    true,
		},
		{
			// Regression: LastError conflates discovery errors with CSN-check
			// errors, and a CSN error is carried forward on every discovery-only
			// pass. Inferring churn from it pinned the cluster at the fast
			// interval for a peer that was merely lagging.
			name: "carried-forward CSN error must NOT read as churn",
			prev: []ldapv1alpha1.ExternalPeerStatus{errored},
			cur:  []ldapv1alpha1.ExternalPeerStatus{errored},
			want: false,
		},
		{
			// A peer with no discovered addresses at all is a static
			// podAddresses or uri peer: its addresses live in the spec and
			// cannot churn from discovery.
			name: "static podAddresses / uri peer — no discovered addresses either side",
			prev: []ldapv1alpha1.ExternalPeerStatus{peerStatus("siteB")},
			cur:  []ldapv1alpha1.ExternalPeerStatus{peerStatus("siteB")},
			want: false,
		},
		{
			name: "peer added to the spec",
			prev: []ldapv1alpha1.ExternalPeerStatus{peerStatus("siteB", "10.0.0.1")},
			cur: []ldapv1alpha1.ExternalPeerStatus{
				peerStatus("siteB", "10.0.0.1"), peerStatus("siteC", "10.1.0.1"),
			},
			want: true,
		},
		{
			name: "peer removed from the spec",
			prev: []ldapv1alpha1.ExternalPeerStatus{
				peerStatus("siteB", "10.0.0.1"), peerStatus("siteC", "10.1.0.1"),
			},
			cur:  []ldapv1alpha1.ExternalPeerStatus{peerStatus("siteB", "10.0.0.1")},
			want: true,
		},
		{
			// Peer order in status follows spec order; a spec reshuffle is not
			// address churn, so match peers by name rather than by index.
			name: "peers reordered in the spec",
			prev: []ldapv1alpha1.ExternalPeerStatus{
				peerStatus("siteB", "10.0.0.1"), peerStatus("siteC", "10.1.0.1"),
			},
			cur: []ldapv1alpha1.ExternalPeerStatus{
				peerStatus("siteC", "10.1.0.1"), peerStatus("siteB", "10.0.0.1"),
			},
			want: false,
		},
		{
			name: "first observation — no previous status yet",
			prev: nil,
			cur:  []ldapv1alpha1.ExternalPeerStatus{peerStatus("siteB", "10.0.0.1")},
			want: true,
		},
		{
			name: "one settled peer, one churning",
			prev: []ldapv1alpha1.ExternalPeerStatus{
				peerStatus("siteB", "10.0.0.1"), peerStatus("siteC", "10.1.0.1"),
			},
			cur: []ldapv1alpha1.ExternalPeerStatus{
				peerStatus("siteB", "10.0.0.1"), peerStatus("siteC", "10.1.0.2"),
			},
			want: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := peerAddressesUnsettled(c.prev, c.cur, c.errored); got != c.want {
				t.Errorf("peerAddressesUnsettled() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestCSNCheckDue(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *metav1.Time {
		t := metav1.NewTime(now.Add(d))
		return &t
	}

	cases := []struct {
		name     string
		prev     []ldapv1alpha1.ExternalPeerStatus
		interval time.Duration
		want     bool
	}{
		{
			// No persisted check: the first pass after operator start (or on a
			// brand-new cluster) must check.
			name:     "no previous peer statuses",
			prev:     nil,
			interval: time.Minute,
			want:     true,
		},
		{
			name:     "peer exists but was never checked",
			prev:     []ldapv1alpha1.ExternalPeerStatus{peerStatus("siteB", "10.0.0.1")},
			interval: time.Minute,
			want:     true,
		},
		{
			name: "checked 10s ago, interval 60s — not due",
			prev: []ldapv1alpha1.ExternalPeerStatus{
				{Name: "siteB", LastChecked: at(-10 * time.Second)},
			},
			interval: time.Minute,
			want:     false,
		},
		{
			name: "checked 61s ago — due",
			prev: []ldapv1alpha1.ExternalPeerStatus{
				{Name: "siteB", LastChecked: at(-61 * time.Second)},
			},
			interval: time.Minute,
			want:     true,
		},
		{
			// The oldest check governs: a peer added mid-flight must not defer
			// the others, and one stale peer must pull the whole check forward.
			name: "min across peers governs — one fresh, one stale",
			prev: []ldapv1alpha1.ExternalPeerStatus{
				{Name: "siteB", LastChecked: at(-5 * time.Second)},
				{Name: "siteC", LastChecked: at(-90 * time.Second)},
			},
			interval: time.Minute,
			want:     true,
		},
		{
			name: "min across peers — both fresh",
			prev: []ldapv1alpha1.ExternalPeerStatus{
				{Name: "siteB", LastChecked: at(-5 * time.Second)},
				{Name: "siteC", LastChecked: at(-20 * time.Second)},
			},
			interval: time.Minute,
			want:     false,
		},
		{
			// A clock going backwards (node clock skew, or a status written by
			// an operator on another node) must not wedge the check off forever.
			name: "LastChecked in the future — check anyway",
			prev: []ldapv1alpha1.ExternalPeerStatus{
				{Name: "siteB", LastChecked: at(10 * time.Minute)},
			},
			interval: time.Minute,
			want:     true,
		},
		{
			name: "exactly at the interval — due",
			prev: []ldapv1alpha1.ExternalPeerStatus{
				{Name: "siteB", LastChecked: at(-60 * time.Second)},
			},
			interval: time.Minute,
			want:     true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := csnCheckDue(c.prev, now, c.interval); got != c.want {
				t.Errorf("csnCheckDue() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestDiscoveryRequeue(t *testing.T) {
	const (
		base   = 15 * time.Second
		settle = 3 * time.Second
		maxN   = 10
	)

	cases := []struct {
		name          string
		unsettled     bool
		fastPasses    int
		wantInterval  time.Duration
		wantNextCount int
	}{
		{
			name:      "settled — base cadence, counter reset",
			unsettled: false, fastPasses: 4,
			wantInterval: base, wantNextCount: 0,
		},
		{
			name:      "churn — tighten to the settle interval",
			unsettled: true, fastPasses: 0,
			wantInterval: settle, wantNextCount: 1,
		},
		{
			name:      "churn continuing — still fast, counter advances",
			unsettled: true, fastPasses: 3,
			wantInterval: settle, wantNextCount: 4,
		},
		{
			name:      "one pass before the bound — still fast",
			unsettled: true, fastPasses: maxN - 1,
			wantInterval: settle, wantNextCount: maxN,
		},
		{
			// A peer that flaps or errors forever must not pin the namespace at
			// the fast interval: fall back to base and stay there.
			name:      "bound reached — fall back to base despite churn",
			unsettled: true, fastPasses: maxN,
			wantInterval: base, wantNextCount: maxN,
		},
		{
			name:      "past the bound — stays at base",
			unsettled: true, fastPasses: maxN + 5,
			wantInterval: base, wantNextCount: maxN + 5,
		},
		{
			// Settling after being bounded clears the budget, so the next real
			// restart gets the fast path again.
			name:      "settled after being bounded — budget clears",
			unsettled: false, fastPasses: maxN,
			wantInterval: base, wantNextCount: 0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotInterval, gotCount := discoveryRequeue(c.unsettled, c.fastPasses, base, settle, maxN)
			if gotInterval != c.wantInterval {
				t.Errorf("interval = %v, want %v", gotInterval, c.wantInterval)
			}
			if gotCount != c.wantNextCount {
				t.Errorf("nextCount = %d, want %d", gotCount, c.wantNextCount)
			}
		})
	}
}

// A discovery-only pass must rebuild a status that is byte-identical to the
// applied one whenever the discovered addresses did not change. That is the
// property M8a rests on: server-side apply then writes nothing, resourceVersion
// does not move, and the SlapdDatabase watch (which has no predicate) does not
// fan out to an LDAP dial per pod per database.
func TestDiscoveryOnlyPassIsAStatusNoOp(t *testing.T) {
	checked := metav1.NewTime(time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC))
	prev := []ldapv1alpha1.ExternalPeerStatus{
		{
			Name:                "siteB",
			Connected:           true,
			DiscoveredAddresses: []string{"10.0.0.1", "10.0.0.2"},
			ReplicationState:    ldapv1alpha1.ReplicationSynced,
			LastChecked:         &checked,
		},
		{
			Name:                "siteC",
			Connected:           true,
			DiscoveredAddresses: []string{"10.1.0.1"},
			ReplicationState:    ldapv1alpha1.ReplicationLagging,
			LagSeconds:          "7.5",
			LastChecked:         &checked,
			LastError:           "one pod unreachable",
		},
	}

	// Rebuild the way Reconcile does on a skipped pass: fresh struct carrying
	// only the name and the freshly discovered addresses, then carry-forward.
	var rebuilt []ldapv1alpha1.ExternalPeerStatus
	for _, p := range prev {
		cur := ldapv1alpha1.ExternalPeerStatus{
			Name:                p.Name,
			DiscoveredAddresses: append([]string(nil), p.DiscoveredAddresses...),
		}
		carryForwardPeerCSNStatus(prev, &cur)
		cur.Connected = cur.ReplicationState != "" && cur.ReplicationState != ldapv1alpha1.ReplicationUnreachable
		rebuilt = append(rebuilt, cur)
	}

	if !reflect.DeepEqual(prev, rebuilt) {
		t.Errorf("a discovery-only pass changed the status:\n prev = %+v\n got  = %+v", prev, rebuilt)
	}
	if peerAddressesUnsettled(prev, rebuilt, false) {
		t.Error("an unchanged rebuild must not read as churn")
	}
	if csnCheckDue(rebuilt, checked.Time.Add(10*time.Second), time.Minute) {
		t.Error("carrying LastChecked forward must not make the next check due early")
	}
	if !csnCheckDue(rebuilt, checked.Time.Add(61*time.Second), time.Minute) {
		t.Error("carrying LastChecked forward must not defer the real check past its interval")
	}
}

// A fresh discovery error on a skipped pass must win over the carried-forward
// one, and must not silently erase the previous CSN verdict.
func TestCarryForwardPrefersAFreshDiscoveryError(t *testing.T) {
	old := metav1.NewTime(time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC))
	prev := []ldapv1alpha1.ExternalPeerStatus{{
		Name:             "siteB",
		ReplicationState: ldapv1alpha1.ReplicationSynced,
		LastChecked:      &old,
		LastError:        "stale CSN error",
	}}
	cur := ldapv1alpha1.ExternalPeerStatus{Name: "siteB", LastError: "remote list failed"}
	carryForwardPeerCSNStatus(prev, &cur)

	if cur.LastError != "remote list failed" {
		t.Errorf("LastError = %q, want the fresh discovery error", cur.LastError)
	}
	if cur.ReplicationState != ldapv1alpha1.ReplicationSynced {
		t.Errorf("ReplicationState = %q, want the carried-forward verdict", cur.ReplicationState)
	}
	if cur.LastChecked == nil || !cur.LastChecked.Equal(&old) {
		t.Error("LastChecked must be carried verbatim — it means 'when we last actually checked'")
	}
}

// A peer with no previously applied status (newly added to the spec) gets
// nothing carried forward and therefore reads as never-checked, which forces a
// real check on the next pass rather than inheriting a sibling's clock.
func TestCarryForwardOnAnUnknownPeerLeavesItUnchecked(t *testing.T) {
	checked := metav1.NewTime(time.Now())
	prev := []ldapv1alpha1.ExternalPeerStatus{{Name: "siteB", LastChecked: &checked}}
	cur := ldapv1alpha1.ExternalPeerStatus{Name: "siteC", DiscoveredAddresses: []string{"10.1.0.1"}}
	carryForwardPeerCSNStatus(prev, &cur)

	if cur.LastChecked != nil {
		t.Error("an unknown peer must not inherit a LastChecked")
	}
	if !csnCheckDue([]ldapv1alpha1.ExternalPeerStatus{cur}, time.Now(), time.Minute) {
		t.Error("a never-checked peer must make the CSN check due")
	}
}
