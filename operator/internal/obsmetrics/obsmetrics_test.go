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

package obsmetrics

import (
	"sort"
	"strings"
	"testing"
	"time"
)

// ─── Fixtures ───────────────────────────────────────────────────────────────
//
// Two of everything, on ridcoord_test.go's "one layout proves nothing"
// convention (ridcoord_test.go:28-31): the control fixture shares no value
// with the primary one — not a namespace, not a cluster name, not a count, not
// a timestamp — so a render that hardcoded any single number or label cannot
// pass both. Every whole-sample-set assertion below runs over the pair.

// healthyCluster is a three-pod, two-database, two-peer site with nothing
// wrong: every pair read, both spreads computable, both peers' discovery
// resolving to remote pods. The two peers deliberately resolve to DIFFERENT
// counts — 3 and 2 — so a render that computed one peer's count and reused it
// for the rest cannot pass.
func healthyCluster() ClusterObservation {
	return ClusterObservation{
		Namespace: testNamespace, Cluster: testCluster, Mesh: testMesh,
		Observed: time.Unix(1700000000, 0),
		Databases: []DatabaseCSN{
			{Database: testDB, PairsExpected: 3, PairsRead: 3, Spread: new(time.Duration(0))},
			{Database: testDB2, PairsExpected: 3, PairsRead: 3, Spread: new(500 * time.Millisecond)},
		},
		Peers: []PeerObservation{
			{Peer: testPeerSite2, UsesDiscovery: true, DiscoveryRan: true, ProvidersDiscovered: 3},
			{Peer: testPeerSite3, UsesDiscovery: true, DiscoveryRan: true, ProvidersDiscovered: 2},
		},
	}
}

// wantHealthyCluster is healthyCluster's complete sample set, in the order the
// contract promises. Note the `default` spread of 0: three pods answered and
// they agree, so 0 is a MEASUREMENT here and belongs in the output. That is
// the opposite of the absent-spread cases further down, and the distinction
// decision 5 turns on.
//
// There is no peer_providers_expected line: the family was adjudicated away,
// because `replicasPerPeer` is fan-out rather than a prediction of discovery
// and ADR-032 makes the link count a property of a site pair. The detectable
// condition is `providers_discovered == 0`, pinned below.
var wantHealthyCluster = []string{
	`slaptain_cluster_observation_timestamp_seconds{cluster="slapd",mesh="ldap",namespace="slaptain-testing"} 1.7e+09`,
	`slaptain_csn_pairs_expected{cluster="slapd",database="db2",mesh="ldap",namespace="slaptain-testing"} 3`,
	`slaptain_csn_pairs_expected{cluster="slapd",database="default",mesh="ldap",namespace="slaptain-testing"} 3`,
	`slaptain_csn_pairs_read{cluster="slapd",database="db2",mesh="ldap",namespace="slaptain-testing"} 3`,
	`slaptain_csn_pairs_read{cluster="slapd",database="default",mesh="ldap",namespace="slaptain-testing"} 3`,
	`slaptain_csn_spread_seconds{cluster="slapd",database="db2",mesh="ldap",namespace="slaptain-testing"} 0.5`,
	`slaptain_csn_spread_seconds{cluster="slapd",database="default",mesh="ldap",namespace="slaptain-testing"} 0`,
	`slaptain_peer_providers_discovered{cluster="slapd",mesh="ldap",namespace="slaptain-testing",peer="site-2"} 3`,
	`slaptain_peer_providers_discovered{cluster="slapd",mesh="ldap",namespace="slaptain-testing",peer="site-3"} 2`,
}

// controlCluster shares no value with healthyCluster. It is also degraded on
// every axis at once, which is deliberate: the two fixtures together pin that
// the render reads its input rather than its own defaults.
func controlCluster() ClusterObservation {
	return ClusterObservation{
		Namespace: "other-ns", Cluster: testControlCluster, Mesh: testControlMesh,
		Observed: time.Unix(1200000000, 0),
		Databases: []DatabaseCSN{
			{Database: "people", PairsExpected: 5, PairsRead: 4, Spread: new(7 * time.Second)},
		},
		Peers: []PeerObservation{
			{Peer: "site-9", UsesDiscovery: true, DiscoveryRan: true, ProvidersDiscovered: 1},
		},
	}
}

var wantControlCluster = []string{
	`slaptain_cluster_observation_timestamp_seconds{cluster="ldap-b",mesh="mesh-b",namespace="other-ns"} 1.2e+09`,
	`slaptain_csn_pairs_expected{cluster="ldap-b",database="people",mesh="mesh-b",namespace="other-ns"} 5`,
	`slaptain_csn_pairs_read{cluster="ldap-b",database="people",mesh="mesh-b",namespace="other-ns"} 4`,
	`slaptain_csn_spread_seconds{cluster="ldap-b",database="people",mesh="mesh-b",namespace="other-ns"} 7`,
	`slaptain_peer_providers_discovered{cluster="ldap-b",mesh="mesh-b",namespace="other-ns",peer="site-9"} 1`,
}

// healthyDatabase is a two-RW-pod database plus one read-only consumer, every
// pod probed and healthy. The RO pod is in the HEALTHY fixture on purpose: the
// `role` label is a property of every probe sample, not something that appears
// only when something is wrong.
func healthyDatabase() DatabaseObservation {
	return DatabaseObservation{
		Namespace: testNamespace, Cluster: testCluster, Mesh: testMesh,
		Database: testDB,
		Observed: time.Unix(1700000000, 0),
		Pods: []SuffixProbe{
			{Pod: testPod0, Probed: true, Healthy: true},
			{Pod: testPod1, Probed: true, Healthy: true},
			{Pod: "slapd-ro-0", ReadOnly: true, Probed: true, Healthy: true},
		},
	}
}

var wantHealthyDatabase = []string{
	`slaptain_database_observation_timestamp_seconds{cluster="slapd",database="default",mesh="ldap",namespace="slaptain-testing"} 1.7e+09`,
	`slaptain_suffix_probe_ok{cluster="slapd",database="default",mesh="ldap",namespace="slaptain-testing",pod="slapd-0",role="readwrite"} 1`,
	`slaptain_suffix_probe_ok{cluster="slapd",database="default",mesh="ldap",namespace="slaptain-testing",pod="slapd-1",role="readwrite"} 1`,
	`slaptain_suffix_probe_ok{cluster="slapd",database="default",mesh="ldap",namespace="slaptain-testing",pod="slapd-ro-0",role="readonly"} 1`,
}

// controlDatabase shares no value with healthyDatabase: different identity,
// different database, different timestamp, and both bad outcomes at once — a
// read-write pod whose suffix entry is a GLUE (ADR-025: never heals, backups
// from it unrestorable) and a read-only consumer that simply has not finished
// its initial sync. They render the same 0 on the probe gauge and are told
// apart by the `role` label and by the glue family, which is the whole reason
// both exist.
func controlDatabase() DatabaseObservation {
	return DatabaseObservation{
		Namespace: "other-ns", Cluster: testControlCluster, Mesh: testControlMesh,
		Database: "people",
		Observed: time.Unix(1200000000, 0),
		Pods: []SuffixProbe{
			{Pod: "ldap-b-4", Probed: true, Healthy: false, Glue: true},
			{Pod: "ldap-b-ro-0", ReadOnly: true, Probed: true, Healthy: false},
		},
	}
}

var wantControlDatabase = []string{
	`slaptain_database_observation_timestamp_seconds{cluster="ldap-b",database="people",mesh="mesh-b",namespace="other-ns"} 1.2e+09`,
	`slaptain_suffix_glue{cluster="ldap-b",database="people",mesh="mesh-b",namespace="other-ns",pod="ldap-b-4",role="readwrite"} 1`,
	`slaptain_suffix_probe_ok{cluster="ldap-b",database="people",mesh="mesh-b",namespace="other-ns",pod="ldap-b-4",role="readwrite"} 0`,
	`slaptain_suffix_probe_ok{cluster="ldap-b",database="people",mesh="mesh-b",namespace="other-ns",pod="ldap-b-ro-0",role="readonly"} 0`,
}

// ─── Helpers ────────────────────────────────────────────────────────────────

// rendered is the sample set as comparable strings, in the order the render
// returned them. It does NOT sort: ordering is part of the contract, so a test
// that normalised it away could not see an ordering regression.
func rendered(samples []Sample) []string {
	out := make([]string, 0, len(samples))
	for _, s := range samples {
		out = append(out, s.String())
	}
	return out
}

// assertSamples compares a render against the exact expected set, in order.
func assertSamples(t *testing.T, got []Sample, want []string) {
	t.Helper()
	gotStrs := rendered(got)
	if len(gotStrs) != len(want) {
		t.Errorf("rendered %d samples, want %d\n got: %s\nwant: %s",
			len(gotStrs), len(want), strings.Join(gotStrs, "\n      "), strings.Join(want, "\n      "))
		return
	}
	for i := range want {
		if gotStrs[i] != want[i] {
			t.Errorf("sample %d:\n got %s\nwant %s", i, gotStrs[i], want[i])
		}
	}
}

// names returns the distinct metric names present, sorted — for the absence
// assertions, which care whether a family appeared at all.
func names(samples []Sample) []string {
	seen := map[string]bool{}
	for _, s := range samples {
		seen[s.Name] = true
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// withName returns every sample of one metric family.
func withName(samples []Sample, name string) []Sample {
	var out []Sample
	for _, s := range samples {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

// has reports whether any sample of that family carries every label in want.
func has(samples []Sample, name string, want map[string]string) (Sample, bool) {
	for _, s := range withName(samples, name) {
		match := true
		for k, v := range want {
			if s.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			return s, true
		}
	}
	return Sample{}, false
}

// ─── Whole-set renders ──────────────────────────────────────────────────────

// TestHealthyClusterRendersTheCompleteSampleSet pins what a good pass looks
// like end to end (ADR-033 decision 3's cluster-pass bullet list), including
// the measured spread of 0 for a database whose three pods agree.
func TestHealthyClusterRendersTheCompleteSampleSet(t *testing.T) {
	assertSamples(t, RenderCluster(healthyCluster()), wantHealthyCluster)
}

// TestControlClusterRendersTheCompleteSampleSet repeats the whole-set
// assertion against a fixture sharing no value with the first — the
// ridcoord_test.go:28-31 control. A render that hardcoded "slapd", "3" or
// 1.7e9 passes the test above and fails this one.
func TestControlClusterRendersTheCompleteSampleSet(t *testing.T) {
	assertSamples(t, RenderCluster(controlCluster()), wantControlCluster)
}

// TestHealthyDatabaseRendersTheCompleteSampleSet pins the database pass's good
// case: a timestamp plus one suffix-probe sample per pod, nothing withheld.
func TestHealthyDatabaseRendersTheCompleteSampleSet(t *testing.T) {
	assertSamples(t, RenderDatabase(healthyDatabase()), wantHealthyDatabase)
}

// TestControlDatabaseRendersTheCompleteSampleSet is the database control:
// different identity, a probed-and-bad pod and a withheld convergence.
func TestControlDatabaseRendersTheCompleteSampleSet(t *testing.T) {
	assertSamples(t, RenderDatabase(controlDatabase()), wantControlDatabase)
}

// ─── Decision 5: the zero observation ───────────────────────────────────────

// TestZeroObservationRendersNoSamplesAtAll is decision 5 at its bluntest: a
// pass that observed nothing must produce nothing, not a set of zeros. A
// render that emitted a timestamp of 0 here would publish 1970 as the freshness
// anchor, and every staleness rule would fire against a cluster that is fine.
func TestZeroObservationRendersNoSamplesAtAll(t *testing.T) {
	if got := RenderCluster(ClusterObservation{}); len(got) != 0 {
		t.Errorf("RenderCluster(zero) rendered %d samples, want none:\n%s",
			len(got), strings.Join(rendered(got), "\n"))
	}
	if got := RenderDatabase(DatabaseObservation{}); len(got) != 0 {
		t.Errorf("RenderDatabase(zero) rendered %d samples, want none:\n%s",
			len(got), strings.Join(rendered(got), "\n"))
	}
}

// TestTimestampAbsentWhenThePassRecordedNoTime pins the timestamp's own
// absence rule, with readings present so the suppression is visibly the
// timestamp's and not the whole render's. The anchor must not be invented: a
// zero time rendered as 0 is a 1970 reading that reads as "scraped just now
// from a very old clock", which is worse than no series.
func TestTimestampAbsentWhenThePassRecordedNoTime(t *testing.T) {
	cluster := healthyCluster()
	cluster.Observed = time.Time{}
	got := RenderCluster(cluster)
	if ts := withName(got, MetricClusterObservationTimestamp); len(ts) != 0 {
		t.Errorf("RenderCluster with a zero Observed rendered %v, want no timestamp sample", rendered(ts))
	}
	if len(withName(got, MetricCSNPairsRead)) == 0 {
		t.Error("the zero timestamp suppressed the CSN samples too; only the anchor should go")
	}

	db := healthyDatabase()
	db.Observed = time.Time{}
	gotDB := RenderDatabase(db)
	if ts := withName(gotDB, MetricDatabaseObservationTimestamp); len(ts) != 0 {
		t.Errorf("RenderDatabase with a zero Observed rendered %v, want no timestamp sample", rendered(ts))
	}
	if len(withName(gotDB, MetricSuffixProbeOK)) == 0 {
		t.Error("the zero timestamp suppressed the suffix probes too; only the anchor should go")
	}
}

// ─── Decision 5: csn_spread, where 0 may never mean "missing" ───────────────

// TestCSNSpreadAbsentWhenOnlyOnePodAnswered is the headline absence rule. One
// readable pod is not a spread; rendering 0 would be indistinguishable from
// "three pods, identical contextCSN", which is the one reading an operator
// most wants to trust. The pair counts still render, because the attempt
// happened.
//
// The producer says so with a nil Spread (2026-10-06), and the nil is what this
// render acts on — see TestCSNSpreadGatesOnThePointerAndNothingElse.
func TestCSNSpreadAbsentWhenOnlyOnePodAnswered(t *testing.T) {
	obs := healthyCluster()
	obs.Databases = []DatabaseCSN{{Database: testDB, PairsExpected: 3, PairsRead: 1, Spread: nil}}
	got := RenderCluster(obs)

	if s, ok := has(got, MetricCSNSpread, map[string]string{LabelDatabase: testDB}); ok {
		t.Errorf("rendered %s with one readable pod; a spread needs two readings and 0 reads as perfect convergence", s)
	}
	if _, ok := has(got, MetricCSNPairsRead, map[string]string{LabelDatabase: testDB}); !ok {
		t.Error("csn_pairs_read is missing; the database WAS attempted, so the count is the observation")
	}
}

// TestCSNSpreadAbsentWhenNoPodAnswered is the same rule at the other end, and
// it is the 2026-10-06 incident's shape: the site could read none of its own
// databases. Spread must be silent and the pair counts must say 0 of N.
func TestCSNSpreadAbsentWhenNoPodAnswered(t *testing.T) {
	obs := healthyCluster()
	obs.Databases = []DatabaseCSN{{Database: testDB, PairsExpected: 3, PairsRead: 0, Spread: nil}}
	got := RenderCluster(obs)

	if s, ok := has(got, MetricCSNSpread, map[string]string{LabelDatabase: testDB}); ok {
		t.Errorf("rendered %s with no readable pod; unreadable evidence is absent, never 0 (ADR-033 decision 5)", s)
	}
}

// TestCSNSpreadOfZeroIsRenderedWhenTwoPodsAgree is the control for the two
// tests above, and the reason they have teeth: an implementation that dropped
// every zero spread would pass both and silently lose the healthiest reading
// on the surface. Zero is absent only when it was never measured.
func TestCSNSpreadOfZeroIsRenderedWhenTwoPodsAgree(t *testing.T) {
	obs := healthyCluster()
	obs.Databases = []DatabaseCSN{{Database: testDB, PairsExpected: 2, PairsRead: 2, Spread: new(time.Duration(0))}}
	got := RenderCluster(obs)

	s, ok := has(got, MetricCSNSpread, map[string]string{LabelDatabase: testDB})
	if !ok {
		t.Fatalf("no %s sample for a database two pods agreed on; a measured 0 is a measurement", MetricCSNSpread)
	}
	if s.Value != 0 {
		t.Errorf("%s = %v, want 0", s, s.Value)
	}
}

// TestCSNPairsReadIsZeroAndPresentWhenEveryReadFailed is the 2026-10-06
// incident verbatim: "could not read contextCSN for 6 of 6 (pod, database)
// pairs", a fact that lived only as English inside a condition message. Here 0
// is the right value precisely because the ATTEMPT is the observation — and
// `read < expected` is then an alertable comparison, with no division.
func TestCSNPairsReadIsZeroAndPresentWhenEveryReadFailed(t *testing.T) {
	obs := healthyCluster()
	obs.Databases = []DatabaseCSN{
		{Database: testDB, PairsExpected: 3, PairsRead: 0},
		{Database: testDB2, PairsExpected: 3, PairsRead: 0},
	}
	got := RenderCluster(obs)

	for _, db := range []string{testDB, testDB2} {
		read, ok := has(got, MetricCSNPairsRead, map[string]string{LabelDatabase: db})
		if !ok {
			t.Errorf("no %s for database %q; dropping it loses the whole incident — 6 of 6 unreadable looks identical to never attempted",
				MetricCSNPairsRead, db)
			continue
		}
		if read.Value != 0 {
			t.Errorf("%s = %v, want 0", read, read.Value)
		}
		expected, ok := has(got, MetricCSNPairsExpected, map[string]string{LabelDatabase: db})
		if !ok {
			t.Errorf("no %s for database %q; without it `read < expected` cannot be written", MetricCSNPairsExpected, db)
			continue
		}
		if expected.Value != 3 {
			t.Errorf("%s = %v, want 3", expected, expected.Value)
		}
	}
}

// TestCSNSamplesAbsentForADatabaseNeverAttempted pins presence-in-the-input as
// the only thing that means "attempted". A database missing from the
// observation gets no series at all — not a zero expected count, which would
// claim the operator looked.
func TestCSNSamplesAbsentForADatabaseNeverAttempted(t *testing.T) {
	obs := healthyCluster()
	obs.Databases = []DatabaseCSN{{Database: testDB, PairsExpected: 3, PairsRead: 3}}
	got := RenderCluster(obs)

	for _, name := range []string{MetricCSNPairsRead, MetricCSNPairsExpected, MetricCSNSpread} {
		if s, ok := has(got, name, map[string]string{LabelDatabase: testDB2}); ok {
			t.Errorf("rendered %s for a database the pass never attempted", s)
		}
	}
	if _, ok := has(got, MetricCSNPairsExpected, map[string]string{LabelDatabase: testDB}); !ok {
		t.Error("the attempted database lost its counts too")
	}
}

// ─── Decision 5: peer provider counts exist only under discovery ────────────

// TestPeerProviderCountsAbsentWithoutDiscovery pins that a `uri` or
// `podAddresses` peer has no discovery result to report. Rendering 0 found for
// it would put an outage-shaped reading on a static configuration that is
// working exactly as declared — and the one query this family exists to serve,
// `providers_discovered == 0`, would then fire on every static peer in the
// mesh.
func TestPeerProviderCountsAbsentWithoutDiscovery(t *testing.T) {
	obs := healthyCluster()
	obs.Peers = []PeerObservation{
		{Peer: testStaticPeer, UsesDiscovery: false},
		{Peer: testPeerSite2, UsesDiscovery: true, DiscoveryRan: true, ProvidersDiscovered: 3},
	}
	got := RenderCluster(obs)

	if s, ok := has(got, MetricPeerProvidersDiscovered, map[string]string{LabelPeer: testStaticPeer}); ok {
		t.Errorf("rendered %s for a non-discovery peer; there is no discovery result, not a result of zero", s)
	}
	if _, ok := has(got, MetricPeerProvidersDiscovered, map[string]string{LabelPeer: testPeerSite2}); !ok {
		t.Errorf("no %s for the discovery peer alongside it", MetricPeerProvidersDiscovered)
	}
}

// TestPeerProvidersDiscoveredIsZeroAndPresentWhenDiscoveryFoundNothing and
// TestPeerProvidersDiscoveredAbsentWhenDiscoveryCouldNotBeAsked are the pair
// this family exists to separate, and they are deliberately adjacent and
// deliberately named for the difference — the same treatment as probed-and-bad
// against not-probed on the suffix probe:
//
//	discovery RAN, found nothing  → 0, present. The attempt is the
//	                                observation, and `== 0` is the alert.
//	discovery could not be ASKED  → absent. A local fault (a rotated
//	                                kubeconfig, an unreachable remote API)
//	                                rendered as 0 sends a reader to a remote
//	                                site that is fine.
//
// This one is the first: discovery ran and resolved to nothing, which ADR-033
// decision 3 calls out as how a cross-site outage begins. The series must be
// there carrying 0, because with no companion "expected" series
// `providers_discovered == 0` is the whole alert — and a dropped sample makes
// it unwritable.
func TestPeerProvidersDiscoveredIsZeroAndPresentWhenDiscoveryFoundNothing(t *testing.T) {
	obs := healthyCluster()
	obs.Peers = []PeerObservation{{Peer: testPeerSite2, UsesDiscovery: true, DiscoveryRan: true, ProvidersDiscovered: 0}}
	got := RenderCluster(obs)

	found, ok := has(got, MetricPeerProvidersDiscovered, map[string]string{LabelPeer: testPeerSite2})
	if !ok {
		t.Fatalf("no %s for a peer whose discovery returned nothing; a link that resolved to nothing is the signal", MetricPeerProvidersDiscovered)
	}
	if found.Value != 0 {
		t.Errorf("%s = %v, want 0", found, found.Value)
	}
}

// TestPeerProvidersDiscoveredAbsentWhenDiscoveryCouldNotBeAsked is the other
// half of the pair above, and the correctness review's finding:
// discoverRemotePeerProviders returns an error for mundane LOCAL causes — a
// rotated kubeconfig Secret, an unparseable kubeconfig, an unset replication
// network mode, a remote API that will not answer — leaving no providers at
// all. Counting them yields 0, and 0 here says "the remote StatefulSet has no
// pods", which sends a reader to a site that is fine.
//
// The alert is not lost by this absence, which is the part worth knowing before
// reinstating the zero: the same failure leaves no addresses, so the peer reads
// Unreachable and Surface 1 publishes
// slaptain_cluster_peer_connected{...,replication_state="Unreachable"} 0. The
// surface that owns peer reachability already says it; this one must not invent
// a provider count to say it again (ADR-033 decision 1), and must not publish a
// measurement it did not take (decision 5).
func TestPeerProvidersDiscoveredAbsentWhenDiscoveryCouldNotBeAsked(t *testing.T) {
	obs := healthyCluster()
	obs.Peers = []PeerObservation{
		{Peer: testPeerSite2, UsesDiscovery: true, DiscoveryRan: false},
		// A peer whose discovery DID run, alongside it: the suppression must be
		// per peer, not a render that gives up on the family.
		{Peer: testPeerSite3, UsesDiscovery: true, DiscoveryRan: true, ProvidersDiscovered: 2},
	}
	got := RenderCluster(obs)

	if s, ok := has(got, MetricPeerProvidersDiscovered, map[string]string{LabelPeer: testPeerSite2}); ok {
		t.Errorf("rendered %s for a peer whose discovery could not be asked; 0 there reads "+
			"as \"the remote site has no pods\" for a fault that is entirely local", s)
	}
	if _, ok := has(got, MetricPeerProvidersDiscovered, map[string]string{LabelPeer: testPeerSite3}); !ok {
		t.Errorf("no %s for the peer whose discovery DID run; the absence above must be "+
			"per peer", MetricPeerProvidersDiscovered)
	}
}

// TestNoPeerProvidersExpectedFamilyExists is a tripwire for the adjudication
// that removed it. `replicasPerPeer` is fan-out, not a prediction of discovery,
// and under ADR-032 the link count belongs to a site PAIR — so any "expected"
// computable here would be derived from discovery and the comparison would be
// circular. A floor is local policy and belongs in an alert rule.
//
// If you landed here intending to add it back: say in the ADR what observes
// the expectation DIRECTLY (decision 1), because nothing in the operator does
// today.
func TestNoPeerProvidersExpectedFamilyExists(t *testing.T) {
	for name, samples := range allRenders(t) {
		t.Run(name, func(t *testing.T) {
			for _, s := range samples {
				if s.Name == "slaptain_peer_providers_expected" {
					t.Errorf("rendered %s; the expected-providers family was adjudicated away, not deferred", s)
				}
			}
		})
	}
}

// ─── Decision 5: the suffix probe, where 0 IS evidence ──────────────────────

// TestSuffixProbeZeroMeansProbedAndBad and
// TestSuffixProbeAbsentMeansNotProbedAtAll are deliberately two tests with
// names that state the difference. This is the distinction the package exists
// for: suffixprobe's contract is positive evidence only, so "we looked and
// found a glue entry" (0) and "we could not look" (nothing) must not share a
// number. A render that emitted 0 for both would make the ADR-025 glue alert —
// which has no `for:` because glue never heals — fire on every transport
// blip, and a render that dropped both would never fire it at all.
func TestSuffixProbeZeroMeansProbedAndBad(t *testing.T) {
	obs := healthyDatabase()
	obs.Pods = []SuffixProbe{{Pod: testPod1, Probed: true, Healthy: false}}
	got := RenderDatabase(obs)

	s, ok := has(got, MetricSuffixProbeOK, map[string]string{LabelPod: testPod1})
	if !ok {
		t.Fatalf("no %s for a pod that WAS probed and came back unhealthy; 0 is evidence here (ADR-025 glue suffix)", MetricSuffixProbeOK)
	}
	if s.Value != 0 {
		t.Errorf("%s = %v, want 0", s, s.Value)
	}
}

func TestSuffixProbeAbsentMeansNotProbedAtAll(t *testing.T) {
	obs := healthyDatabase()
	obs.Pods = []SuffixProbe{
		{Pod: testPod0, Probed: false, Healthy: false},
		// Healthy left true on an unprobed pod on purpose: the verdict field
		// is meaningless without Probed, and a render reading it first would
		// publish a health claim nobody made.
		{Pod: testPod1, Probed: false, Healthy: true},
		{Pod: testPod2, Probed: true, Healthy: true},
	}
	got := RenderDatabase(obs)

	for _, pod := range []string{testPod0, testPod1} {
		if s, ok := has(got, MetricSuffixProbeOK, map[string]string{LabelPod: pod}); ok {
			t.Errorf("rendered %s for a pod that was not probed; unreadable evidence is absent, never 0 and never 1", s)
		}
	}
	if _, ok := has(got, MetricSuffixProbeOK, map[string]string{LabelPod: testPod2}); !ok {
		t.Error("the one probed pod lost its sample too")
	}
}

// ─── Decision 1: the family that was removed ────────────────────────────────

// TestNoSyncreplWithheldFamilyExists is a tripwire for a removal, not for an
// absence that was always there. slaptain_syncrepl_withheld lived here until
// 2026-10-06 and was deleted because it violates the rule ADR-033 exists to
// establish: SyncreplStanzasReady is a SlapdDatabase STATUS CONDITION, so
// Surface 1 already exports it —
//
//	slaptain_database_condition{condition="SyncreplStanzasReady",reason="…"} 1
//
// — and exports it better, because kube-state-metrics carries the Unknown case
// that a presence-is-the-signal gauge cannot express at all. ADR-033 decision 3
// asked for it while decision 2 had already given it away; decision 1 settles
// which one wins.
//
// If you landed here intending to re-add it: the question is not whether the
// operator knows, it is whether the operator is the ONLY one who knows.
func TestNoSyncreplWithheldFamilyExists(t *testing.T) {
	for _, name := range Families {
		if strings.Contains(name, "withheld") || strings.Contains(name, "syncrepl") {
			t.Errorf("Families declares %q; a withheld-convergence family is Surface 1's "+
				"(slaptain_database_condition{condition=\"SyncreplStanzasReady\"})", name)
		}
	}
	for fixture, samples := range allRenders(t) {
		for _, n := range names(samples) {
			if strings.Contains(n, "withheld") || strings.Contains(n, "syncrepl") {
				t.Errorf("%s rendered %q", fixture, n)
			}
		}
	}
}

// ─── Identity labels ────────────────────────────────────────────────────────

// allRenders is every render over every fixture, plus the degraded variants,
// so the label assertions below sweep each series family at least once.
func allRenders(t *testing.T) map[string][]Sample {
	t.Helper()

	withoutDiscovery := healthyCluster()
	withoutDiscovery.Peers = []PeerObservation{{Peer: testStaticPeer, UsesDiscovery: false}}

	unreadable := healthyCluster()
	unreadable.Databases = []DatabaseCSN{{Database: testDB, PairsExpected: 3, PairsRead: 0}}

	degradedPods := healthyDatabase()
	degradedPods.Pods = append(degradedPods.Pods,
		SuffixProbe{Pod: testPod2, Probed: true, Healthy: false, Glue: true},
		SuffixProbe{Pod: "slapd-3", Probed: false},
	)

	return map[string][]Sample{
		"healthy cluster":                          RenderCluster(healthyCluster()),
		"control cluster":                          RenderCluster(controlCluster()),
		"cluster without discovery":                RenderCluster(withoutDiscovery),
		"cluster with unreadable databases":        RenderCluster(unreadable),
		"healthy database":                         RenderDatabase(healthyDatabase()),
		"control database":                         RenderDatabase(controlDatabase()),
		"database with a glue and an unprobed pod": RenderDatabase(degradedPods),
	}
}

// TestEverySampleCarriesTheIdentityLabels pins the join key. A sample missing
// namespace, cluster or mesh cannot be joined to Surface 1 at all, and a
// dashboard panel built on it silently aggregates across clusters.
func TestEverySampleCarriesTheIdentityLabels(t *testing.T) {
	for name, samples := range allRenders(t) {
		t.Run(name, func(t *testing.T) {
			if len(samples) == 0 {
				t.Fatal("no samples rendered, so nothing was checked")
			}
			for _, s := range samples {
				for _, label := range []string{LabelNamespace, LabelCluster, LabelMesh} {
					if _, present := s.Labels[label]; !present {
						t.Errorf("%s has no %q label", s, label)
					}
				}
			}
		})
	}
}

// TestNoSampleCarriesASiteLabel is the ADR-033 boundary made executable:
// monitoring is site-local (amendment 2026-10-09), each site's Prometheus
// scrapes only that site, and no surface carries `site`. The operator adding
// one would give Surface 2 a join key Surfaces 1 and 3 lack, and every join on
// it would silently match nothing.
//
// If you landed here having added `site`: which site a series came from is
// which Prometheus holds it. Nothing in this package needs to say so.
func TestNoSampleCarriesASiteLabel(t *testing.T) {
	for name, samples := range allRenders(t) {
		t.Run(name, func(t *testing.T) {
			for _, s := range samples {
				if v, present := s.Labels["site"]; present {
					t.Errorf("%s carries site=%q; monitoring is site-local and no surface carries `site` — a label on Surface 2 alone empties every join on it against Surfaces 1 and 3", s, v)
				}
			}
		})
	}
}

// TestMeshLabelIsPresentAndEmptyForAStandaloneCluster pins the non-mesh case.
// A cluster with no meshRef is a supported deployment, and it must not lose
// the label — a family whose label set changes per sample is a family no
// `sum by (mesh)` can be written over.
func TestMeshLabelIsPresentAndEmptyForAStandaloneCluster(t *testing.T) {
	cluster := healthyCluster()
	cluster.Mesh = ""
	db := healthyDatabase()
	db.Mesh = ""

	for name, samples := range map[string][]Sample{
		"cluster":  RenderCluster(cluster),
		"database": RenderDatabase(db),
	} {
		t.Run(name, func(t *testing.T) {
			if len(samples) == 0 {
				t.Fatal("no samples rendered, so nothing was checked")
			}
			for _, s := range samples {
				v, present := s.Labels[LabelMesh]
				if !present {
					t.Errorf("%s dropped the mesh label for a standalone cluster", s)
					continue
				}
				if v != "" {
					t.Errorf("%s has mesh=%q, want the empty string", s, v)
				}
			}
		})
	}
}

// TestDimensionalLabelsAreOnTheRightFamilies pins which families carry which
// extra dimension. A `database` label leaking onto a peer series, or a `pod`
// label onto a CSN series, is a cardinality bug that reads as a working
// dashboard until someone groups by it.
func TestDimensionalLabelsAreOnTheRightFamilies(t *testing.T) {
	tests := []struct {
		name    string
		samples []Sample
		family  string
		want    []string
		absent  []string
	}{
		{"csn spread is per database", RenderCluster(healthyCluster()), MetricCSNSpread,
			[]string{LabelDatabase}, []string{LabelPeer, LabelPod, LabelRole}},
		{"csn pairs read is per database", RenderCluster(healthyCluster()), MetricCSNPairsRead,
			[]string{LabelDatabase}, []string{LabelPeer, LabelPod, LabelRole}},
		{"csn pairs expected is per database", RenderCluster(healthyCluster()), MetricCSNPairsExpected,
			[]string{LabelDatabase}, []string{LabelPeer, LabelPod, LabelRole}},
		{"providers discovered is per peer", RenderCluster(healthyCluster()), MetricPeerProvidersDiscovered,
			[]string{LabelPeer}, []string{LabelDatabase, LabelPod, LabelRole}},
		{"the cluster pass anchor has no extra dimension", RenderCluster(healthyCluster()), MetricClusterObservationTimestamp,
			nil, []string{LabelDatabase, LabelPeer, LabelPod, LabelRole}},
		{"suffix probe is per database and pod", RenderDatabase(controlDatabase()), MetricSuffixProbeOK,
			[]string{LabelDatabase, LabelPod, LabelRole}, []string{LabelPeer}},
		{"suffix glue is per database, pod and role", RenderDatabase(controlDatabase()), MetricSuffixGlue,
			[]string{LabelDatabase, LabelPod, LabelRole}, []string{LabelPeer}},
		{"the database pass anchor is per database", RenderDatabase(controlDatabase()), MetricDatabaseObservationTimestamp,
			[]string{LabelDatabase}, []string{LabelPeer, LabelPod, LabelRole}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := withName(tc.samples, tc.family)
			if len(got) == 0 {
				t.Fatalf("no %s samples in the fixture, so nothing was checked", tc.family)
			}
			for _, s := range got {
				for _, label := range tc.want {
					if v, present := s.Labels[label]; !present || v == "" {
						t.Errorf("%s is missing its %q dimension", s, label)
					}
				}
				for _, label := range tc.absent {
					if _, present := s.Labels[label]; present {
						t.Errorf("%s carries %q, which is not a dimension of this family", s, label)
					}
				}
			}
		})
	}
}

// ─── Ordering ───────────────────────────────────────────────────────────────

// TestRenderOrderIsDeterministic pins ordering as part of the contract: by
// metric name, then by the rendered label set. Maps iterate randomly in Go, so
// a render that walked one would pass a set-comparison test and fail this one
// — and would make any golden-file or diff-based review of this surface
// impossible.
func TestRenderOrderIsDeterministic(t *testing.T) {
	// Input order deliberately unsorted, to pin that the render sorts its
	// OUTPUT rather than trusting its caller.
	cluster := ClusterObservation{
		Namespace: "ns", Cluster: "c", Mesh: "m",
		Observed: time.Unix(1700000000, 0),
		Databases: []DatabaseCSN{
			{Database: "zeta", PairsExpected: 2, PairsRead: 2, Spread: new(time.Second)},
			{Database: testNameFirst, PairsExpected: 2, PairsRead: 2, Spread: new(2 * time.Second)},
		},
		Peers: []PeerObservation{
			{Peer: testNameLast, UsesDiscovery: true, DiscoveryRan: true, ProvidersDiscovered: 1},
			{Peer: "alfa", UsesDiscovery: true, DiscoveryRan: true, ProvidersDiscovered: 1},
		},
	}
	db := DatabaseObservation{
		Namespace: "ns", Cluster: "c", Mesh: "m",
		Database: "d",
		Observed: time.Unix(1700000000, 0),
		Pods: []SuffixProbe{
			{Pod: "p-2", Probed: true, Healthy: true},
			{Pod: "p-0", Probed: true, Healthy: true},
			{Pod: "p-1", Probed: true, Healthy: false},
		},
	}

	// Each render is called inside its own subtest closure rather than in a
	// shared map literal, so the panicking stub's unreachable-code shadow does
	// not fall across the other fixtures.
	t.Run("cluster", func(t *testing.T) { assertSorted(t, RenderCluster(cluster)) })
	t.Run("database", func(t *testing.T) { assertSorted(t, RenderDatabase(db)) })

	// Repeated renders of the same input must be identical, which a
	// map-ordered implementation only passes by luck.
	t.Run("repeated renders agree", func(t *testing.T) {
		for i := range 8 {
			if a, b := rendered(RenderCluster(cluster)), rendered(RenderCluster(cluster)); strings.Join(a, "|") != strings.Join(b, "|") {
				t.Fatalf("two renders of the same cluster observation differ on attempt %d:\n%s\nvs\n%s",
					i, strings.Join(a, "\n"), strings.Join(b, "\n"))
			}
			if a, b := rendered(RenderDatabase(db)), rendered(RenderDatabase(db)); strings.Join(a, "|") != strings.Join(b, "|") {
				t.Fatalf("two renders of the same database observation differ on attempt %d:\n%s\nvs\n%s",
					i, strings.Join(a, "\n"), strings.Join(b, "\n"))
			}
		}
	})
}

// assertSorted requires the render's output to be in the order the contract
// promises. It compares against sort.Strings of the rendered forms, which is
// the same order as (name, label set) because no metric name here is a prefix
// of another — if one ever is, this helper is the thing to revisit.
func assertSorted(t *testing.T, samples []Sample) {
	t.Helper()
	got := rendered(samples)
	if len(got) == 0 {
		t.Fatal("no samples rendered, so nothing was checked")
	}
	want := append([]string(nil), got...)
	sort.Strings(want)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("output is not sorted at index %d:\n got %s\nwant %s\nfull: %s",
				i, got[i], want[i], strings.Join(got, "\n      "))
		}
	}
}

// TestRenderEmitsOnlyTheDocumentedFamilies is a tripwire: the eight names
// above are this package's whole render, and a ninth appearing here is either
// an unreviewed addition or a quantity that belongs to Surface 1 or Surface 3
// by the decision-1 ownership rule (no quantity is exported from two
// surfaces).
//
// They are not yet all of Surface 2. ADR-033 decision 3 also names per-peer
// `replicationState` and `lagSeconds` from `externalPeerStatuses`, a refused
// RID segment, and the restore sub-phase; none of those is rendered here. So
// this list is a closed set for THIS package and an open one for the surface,
// and an addition is reviewable rather than refused.
//
// If you landed here having added a family: decide which vantage point
// observes it DIRECTLY, and if the answer is still the operator, add it to
// this list and to monitoring/prometheus-rules.yaml in the same change.
func TestRenderEmitsOnlyTheDocumentedFamilies(t *testing.T) {
	allowed := map[string]bool{}
	for _, name := range Families {
		allowed[name] = true
	}
	for name, samples := range allRenders(t) {
		t.Run(name, func(t *testing.T) {
			for _, n := range names(samples) {
				if !allowed[n] {
					t.Errorf("rendered undocumented metric family %q", n)
				}
			}
		})
	}
}

// TestMetricNamesAreNamespacedAndUnique is green-from-birth by construction —
// it asserts constants — and exists for the one mistake constants invite: two
// of them collapsing onto the same string, which would silently merge two
// families with different label shapes into one unqueryable series. The
// `slaptain_` prefix is ADR-033 decision 3's namespace, kept apart from
// Surface 3's `openldap_*` families so the two surfaces never share a name.
// The two freshness anchors are the case that makes
// uniqueness load-bearing rather than pedantic: they are one quantity at two
// scopes, and collapsing them back onto one name is exactly the mistake the
// `slaptain_cluster_*` / `slaptain_database_*` split exists to prevent.
//
// Disclosure: the duplicate-name branch below cannot be shown red by mutation
// and is belt to the compiler's braces. `allowed` in
// TestRenderEmitsOnlyTheDocumentedFamilies keys a map literal on all eight
// constants, so making any two of them equal is a BUILD error
// ("duplicate key ... in map literal") before this assertion ever runs —
// verified against three different pairs. That is the stronger guard and it is
// where the protection actually lives. The two prefix branches do go red under
// mutation. Note also that `all` is hand-maintained: a NINTH constant added to
// obsmetrics.go but never rendered is checked by neither.
func TestMetricNamesAreNamespacedAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range Families {
		if seen[n] {
			t.Errorf("metric name %q is declared twice; two families would merge into one series", n)
		}
		seen[n] = true
		if !strings.HasPrefix(n, "slaptain_") {
			t.Errorf("metric name %q is outside the slaptain_ namespace (ADR-033 decision 3)", n)
		}
		if strings.HasPrefix(n, "openldap_") {
			t.Errorf("metric name %q claims Surface 3's upstream namespace", n)
		}
	}
}

// TestSampleStringIsStableAndLabelSorted is green-from-birth: String is this
// file's own comparison key, so it is asserted directly rather than trusted.
// A String that iterated the label map would make every whole-set assertion
// above flaky, and a flaky comparison key reads as a flaky implementation.
func TestSampleStringIsStableAndLabelSorted(t *testing.T) {
	s := Sample{
		Name:   "slaptain_example",
		Labels: map[string]string{testNameLast: "9", "alfa": "1", "mike": "5"},
		Value:  2.5,
	}
	const want = `slaptain_example{alfa="1",mike="5",zulu="9"} 2.5`
	for i := range 32 {
		if got := s.String(); got != want {
			t.Fatalf("attempt %d: Sample.String() = %s, want %s", i, got, want)
		}
	}
	if got := (Sample{Name: "slaptain_bare"}).String(); got != `slaptain_bare{} 0` {
		t.Errorf("Sample.String() with no labels = %s, want %s", got, `slaptain_bare{} 0`)
	}
}

// ─── The pointer is the gate ────────────────────────────────────────────────

// TestCSNSpreadGatesOnThePointerAndNothingElse pins the 2026-10-06 type change.
//
// The hole it closes was executed, not theorised: a DatabaseCSN carrying
// PairsRead 3 and no measurement rendered `csn_spread_seconds 0` — a database
// nobody measured, published as the healthiest reading on the surface, which is
// the failure decision 5 calls the one it is least able to detect.
//
// The fix is that the absence is now carried by the VALUE's own type, so it
// cannot be lost between producer and render. The gate is `Spread != nil` and
// nothing else, deliberately: a second gate on PairsRead would make this render
// silently drop a measurement its caller believed it had — the same silent loss,
// pointing the other way — and would restore the two-authorities-that-must-agree
// arrangement the pointer exists to delete. That a spread implies two readings
// is the producer's invariant, asserted where it is computed
// (controller.TestEvaluateLocalConvergenceReadoutBySuffix), which is the only
// place that can know it.
func TestCSNSpreadGatesOnThePointerAndNothingElse(t *testing.T) {
	t.Run("no measurement renders nothing however many pairs were read", func(t *testing.T) {
		obs := healthyCluster()
		obs.Databases = []DatabaseCSN{
			{Database: testDB, PairsExpected: 3, PairsRead: 3, Spread: nil},
		}
		if s, ok := has(RenderCluster(obs), MetricCSNSpread, map[string]string{LabelDatabase: testDB}); ok {
			t.Errorf("rendered %s for a database with no measurement; a 0 here is perfect "+
				"convergence published for a reading nobody took (ADR-033 decision 5)", s)
		}
	})

	t.Run("a measurement renders whatever the pair counts say", func(t *testing.T) {
		// Unreachable from the real producer — its invariant forbids it — and
		// asserted anyway, because it is what makes the pointer the SOLE
		// authority rather than one of two that have to agree. A render that
		// suppressed this would be making a judgement about data it was handed,
		// using a field that is not the one carrying the rule.
		obs := healthyCluster()
		obs.Databases = []DatabaseCSN{
			{Database: testDB, PairsExpected: 3, PairsRead: 1, Spread: new(3 * time.Second)},
		}
		s, ok := has(RenderCluster(obs), MetricCSNSpread, map[string]string{LabelDatabase: testDB})
		if !ok {
			t.Fatalf("no %s sample for a database carrying a measured spread; the pointer is "+
				"the gate, and a second gate on PairsRead drops a measurement the caller "+
				"believed it had", MetricCSNSpread)
		}
		if s.Value != 3 {
			t.Errorf("%s = %v, want 3 seconds", MetricCSNSpread, s.Value)
		}
	})
}

// ─── ADR-025: the two distinctions the probe must keep ──────────────────────

// TestSuffixProbeCarriesTheRoleOfEveryPod pins the `role` label on every probe
// sample, healthy and unhealthy alike.
//
// Without it an RO consumer mid-initial-sync and an RW pod with a broken suffix
// entry render identically, and ADR-025's 2026-09-14 amendment exists precisely
// because those two are not the same incident: an RO initial sync is
// legitimately transient, which is why it got its own condition reason
// (DataMissingOnReadOnlyPods). An alert on `== 0` that cannot tell them apart
// either false-fires on every read-replica scale-up or is relaxed until it stops
// catching the thing it was written for.
func TestSuffixProbeCarriesTheRoleOfEveryPod(t *testing.T) {
	obs := DatabaseObservation{
		Namespace: "ns", Cluster: "c", Mesh: "m",
		Database: "d",
		Observed: time.Unix(1700000000, 0),
		Pods: []SuffixProbe{
			{Pod: "rw-healthy", Probed: true, Healthy: true},
			{Pod: "rw-bad", Probed: true, Healthy: false},
			{Pod: "ro-healthy", ReadOnly: true, Probed: true, Healthy: true},
			{Pod: "ro-syncing", ReadOnly: true, Probed: true, Healthy: false},
		},
	}
	want := map[string]string{
		"rw-healthy": RoleReadWrite,
		"rw-bad":     RoleReadWrite,
		"ro-healthy": RoleReadOnly,
		"ro-syncing": RoleReadOnly,
	}

	got := withName(RenderDatabase(obs), MetricSuffixProbeOK)
	if len(got) != len(want) {
		t.Fatalf("rendered %d probe samples, want %d: %v", len(got), len(want), rendered(got))
	}
	for _, s := range got {
		pod := s.Labels[LabelPod]
		role, present := s.Labels[LabelRole]
		if !present {
			t.Errorf("%s has no %q label; a rule cannot then give the read-only case a "+
				"longer fuse", s, LabelRole)
			continue
		}
		if role != want[pod] {
			t.Errorf("pod %q rendered role %q, want %q", pod, role, want[pod])
		}
	}
}

// TestSuffixGlueIsItsOwnFamilyWithPresenceAsTheSignal pins the other ADR-025
// distinction: a glue suffix is not a worse kind of "missing".
//
// A glue entry is hidden from ordinary searches, frozen at the winner's
// entryCSN so every CSN health reading comes back clean, makes every backup
// from that pod unrestorable, and NEVER heals — so its alert needs no `for:`,
// which is only expressible while it is a series of its own. A missing entry on
// a pod that is still syncing heals by itself. Collapsed onto one gauge value
// they are the same 0, and the alert has to be written for the weaker of them.
//
// Presence is the signal: there is no glue="false", because that would put a
// sample on every healthy pod of every database — noise on the one family that
// must never be noisy.
func TestSuffixGlueIsItsOwnFamilyWithPresenceAsTheSignal(t *testing.T) {
	obs := DatabaseObservation{
		Namespace: "ns", Cluster: "c", Mesh: "m",
		Database: "d",
		Observed: time.Unix(1700000000, 0),
		Pods: []SuffixProbe{
			{Pod: "healthy", Probed: true, Healthy: true},
			{Pod: "missing", Probed: true, Healthy: false},
			{Pod: testPodGlued, Probed: true, Healthy: false, Glue: true},
		},
	}
	got := RenderDatabase(obs)

	glue := withName(got, MetricSuffixGlue)
	if len(glue) != 1 {
		t.Fatalf("rendered %d %s samples, want exactly one — for the glued pod and nothing "+
			"else: %v", len(glue), MetricSuffixGlue, rendered(got))
	}
	if glue[0].Labels[LabelPod] != testPodGlued {
		t.Errorf("%s names pod %q, want the glued one", MetricSuffixGlue, glue[0].Labels[LabelPod])
	}
	if glue[0].Value != 1 {
		t.Errorf("%s = %v, want 1: presence is the signal", MetricSuffixGlue, glue[0].Value)
	}
	// `role` identifies the pod, it does not grade the finding: both families
	// are pod-scoped and the pod is the same pod, so carrying it on one and not
	// the other would make "glue on a read-write pod" a join instead of a
	// selector — and that is the query ADR-025's unrestorable-backup
	// consequence leads an operator to write.
	if got := glue[0].Labels[LabelRole]; got != RoleReadWrite {
		t.Errorf("%s rendered role %q, want %q for a read-write pod",
			MetricSuffixGlue, got, RoleReadWrite)
	}

	// Both bad pods still read 0 on the probe gauge. The glue family says which
	// of the two bad states it is; it does not replace the verdict.
	for _, pod := range []string{"missing", testPodGlued} {
		s, ok := has(got, MetricSuffixProbeOK, map[string]string{LabelPod: pod})
		if !ok || s.Value != 0 {
			t.Errorf("%s for pod %q: present=%v value=%v, want present with 0",
				MetricSuffixProbeOK, pod, ok, s.Value)
		}
	}
}

// TestSuffixGlueAbsentForAPodThatCouldNotBeProbed is decision 5 on the newest
// family. Probed false means the probe never reached a verdict, so there is no
// glue finding to report — and a glue claim is the most consequential thing
// this surface can say about a pod: it sends a human to a manual runbook and
// condemns every backup taken from it.
func TestSuffixGlueAbsentForAPodThatCouldNotBeProbed(t *testing.T) {
	obs := healthyDatabase()
	// Glue set on an unprobed pod is a caller bug, and the render must not
	// honour it. `Probed` is read first and the rest of the verdict is never
	// consulted without it.
	obs.Pods = []SuffixProbe{{Pod: "unreachable", Probed: false, Glue: true}}

	got := RenderDatabase(obs)
	if s := withName(got, MetricSuffixGlue); len(s) != 0 {
		t.Errorf("rendered %v for a pod that could not be probed; \"we could not look\" must "+
			"never become \"this pod is glued\"", rendered(s))
	}
	if s := withName(got, MetricSuffixProbeOK); len(s) != 0 {
		t.Errorf("rendered %v for a pod that could not be probed", rendered(s))
	}
}

// TestSuffixGlueOnAReadOnlyPodIsStillGlue is the case where the two reasons for
// the `role` label visibly diverge, and it pins that only one of them applies
// here.
//
// On the probe gauge, `role` buys the read-only case a longer fuse: an RO
// consumer mid-initial-sync is legitimately missing its suffix entry. On THIS
// family it buys nothing of the sort — a glue entry on an RO pod is as
// permanent, as invisible to ordinary searches and as fatal to a backup taken
// from it as one on a read-write pod. The label is here to say WHICH pod, not
// to grade the finding, so the sample must be present and the role must be
// readonly.
func TestSuffixGlueOnAReadOnlyPodIsStillGlue(t *testing.T) {
	obs := healthyDatabase()
	obs.Pods = []SuffixProbe{{Pod: "slapd-ro-0", ReadOnly: true, Probed: true, Healthy: false, Glue: true}}

	glue := withName(RenderDatabase(obs), MetricSuffixGlue)
	if len(glue) != 1 {
		t.Fatalf("rendered %d %s samples for a glued read-only pod, want 1 — a glue never "+
			"heals, whichever fleet the pod belongs to", len(glue), MetricSuffixGlue)
	}
	if got := glue[0].Labels[LabelRole]; got != RoleReadOnly {
		t.Errorf("%s rendered role %q, want %q", MetricSuffixGlue, got, RoleReadOnly)
	}
}
