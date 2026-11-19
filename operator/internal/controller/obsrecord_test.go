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
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/utils/ptr"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
	"github.com/chuck-chuck-chuck-net/slaptain/operator/internal/obsmetrics"
)

// These are the only tests that exercise BOTH layers of Surface 2 — the
// controller's reading and obsmetrics' render — and that is the point: each
// layer's own suite can be green while the wire between them is crossed.
//
// The absence rule they exist for used to be stated in two places — a nil
// Spread here, a PairsRead >= 2 gate in the render — and the correctness review
// executed the gap between them: a database with no measurement rendered 0.
// Both sides now carry a pointer and the render gates on it alone, so the rule
// is stated ONCE and this file checks that the crossing preserves it rather
// than that two statements still agree.
//
// Two of everything again (ridcoord_test.go:28-31): obsRecordCluster and
// obsRecordControlCluster share no value — not a namespace, not a name, not a
// mesh, not a peer, not a provider count — so a mapping that hardcoded any of
// them cannot pass both.

// ─── Fixtures ───────────────────────────────────────────────────────────────

// The read-only pods and the duplicated peer name these fixtures use.
const (
	obsROPod0 = "slapd-ro-0"
	obsROPod1 = "slapd-ro-1"
	// dupPeer names two externalPeers entries at once, which a spec can do.
	dupPeer = "dup"
)

// obsRecordCluster is a meshed three-pod site with two external peers: one in
// discovery mode that resolved two providers, one static `uri` peer that has no
// discovery result at all.
func obsRecordCluster() *ldapv1alpha1.SlapdCluster {
	return &ldapv1alpha1.SlapdCluster{
		Name: testClusterName, Namespace: testE2ENamespace,
		Spec: ldapv1alpha1.SlapdClusterSpec{
			MeshRef: testMeshName,
			Replication: ldapv1alpha1.SlapdReplicationConfig{
				Enabled: true,
				ExternalPeers: []ldapv1alpha1.ExternalPeer{
					{Name: testSite2, Discovery: &ldapv1alpha1.ExternalPeerDiscovery{}},
					{Name: testSite3, URI: "ldaps://peer.example:636"},
				},
			},
		},
		Status: ldapv1alpha1.SlapdClusterStatus{
			ExternalPeerStatuses: []ldapv1alpha1.ExternalPeerStatus{
				{
					Name: testSite2,
					DiscoveredProviders: []ldapv1alpha1.DiscoveredProvider{
						{Ordinal: 0, Address: "203.0.113.1"},
						{Ordinal: 1, Address: "203.0.113.2"},
					},
					ReplicationState: ldapv1alpha1.ReplicationSynced,
				},
				// A static peer. No DiscoveredProviders, and none is expected:
				// the absence here is configuration, not an outage.
				{Name: testSite3, ReplicationState: ldapv1alpha1.ReplicationSynced},
			},
		},
	}
}

// obsRecordControlCluster shares no value with the fixture above, and is
// degraded on both axes that matter here: NO meshRef (a standalone cluster,
// whose `mesh` must still be carried as the empty string) and a discovery peer
// whose discovery resolved to NOTHING — the shape ADR-033 decision 3 calls how
// a cross-site outage begins.
func obsRecordControlCluster() *ldapv1alpha1.SlapdCluster {
	return &ldapv1alpha1.SlapdCluster{
		Name: "ldap-b", Namespace: "other-ns",
		Spec: ldapv1alpha1.SlapdClusterSpec{
			Replication: ldapv1alpha1.SlapdReplicationConfig{
				Enabled: true,
				ExternalPeers: []ldapv1alpha1.ExternalPeer{
					{Name: testSite9, Discovery: &ldapv1alpha1.ExternalPeerDiscovery{}},
				},
			},
		},
		Status: ldapv1alpha1.SlapdClusterStatus{
			ExternalPeerStatuses: []ldapv1alpha1.ExternalPeerStatus{
				{Name: testSite9, ReplicationState: ldapv1alpha1.ReplicationUnreachable},
			},
		},
	}
}

// obsRecordReadouts is one pass's per-database measurement: one database fully
// read and converged, one readable on a single pod only — so the pair of them
// covers both sides of the spread rule in a single mapping.
func obsRecordReadouts() map[string]dbCSNReadout {
	return map[string]dbCSNReadout{
		sfxA: {PodsExpected: 3, PodsRead: 3, Spread: ptr.To(time.Duration(0))},
		sfxB: {PodsExpected: 3, PodsRead: 1, Spread: nil},
	}
}

// obsRecordNames is the suffix → SlapdDatabase name translation the `database`
// label needs: the label is what a reader takes back to `kubectl get sd`.
func obsRecordNames() map[string]string {
	return map[string]string{sfxA: testDefaultDB, sfxB: testDB2}
}

func obsRecordDatabase() *ldapv1alpha1.SlapdDatabase {
	return &ldapv1alpha1.SlapdDatabase{
		Name: testDefaultDB, Namespace: testE2ENamespace,
		Spec: ldapv1alpha1.SlapdDatabaseSpec{
			ClusterRef: testClusterName,
			Suffix:     sfxA,
		},
	}
}

var obsRecordObserved = time.Unix(1700000000, 0)

// ─── Helpers ────────────────────────────────────────────────────────────────

// renderedCluster runs the render and turns the not-implemented panic into a
// legible failure.
//
// The recover is not politeness: this package's test binary also carries the
// envtest suite, and a panic escaping here would abort all of it — every other
// red in the package would vanish behind one stack trace. Once RenderCluster is
// implemented the recover never fires.
func renderedCluster(t *testing.T, obs obsmetrics.ClusterObservation) []obsmetrics.Sample {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("obsmetrics.RenderCluster panicked: %v\n"+
				"  this seam asserts the controller's reading THROUGH the render, so it "+
				"stays red until the render exists", r)
		}
	}()
	return obsmetrics.RenderCluster(obs)
}

// renderedDatabase is renderedCluster for the database pass.
func renderedDatabase(t *testing.T, obs obsmetrics.DatabaseObservation) []obsmetrics.Sample {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("obsmetrics.RenderDatabase panicked: %v\n"+
				"  this seam asserts the controller's reading THROUGH the render, so it "+
				"stays red until the render exists", r)
		}
	}()
	return obsmetrics.RenderDatabase(obs)
}

// obsSampleFor finds the sample of one metric family carrying one label value,
// e.g. the csn_spread_seconds of database "db2".
func obsSampleFor(samples []obsmetrics.Sample, name, label, value string) (obsmetrics.Sample, bool) {
	for _, s := range samples {
		if s.Name == name && s.Labels[label] == value {
			return s, true
		}
	}
	return obsmetrics.Sample{}, false
}

// obsDatabaseLabels lists the `database` label values a family was rendered
// with, for failure messages that say what WAS there.
func obsDatabaseLabels(samples []obsmetrics.Sample, name string) []string {
	var out []string
	for _, s := range samples {
		if s.Name == name {
			out = append(out, s.Labels[obsmetrics.LabelDatabase])
		}
	}
	return out
}

// ─── The mapping: identity and the per-database measurement ─────────────────

// TestClusterObservationCarriesEveryAttemptedDatabase pins that the mapping is
// lossless in the direction that matters: every database the pass attempted
// reaches the observation, with its own counts, labelled by the SlapdDatabase
// name rather than by the suffix the readout is keyed on.
//
// Both numbers must survive, not just the spread: `read < expected` is the
// alertable comparison, and it is unwritable if either side is dropped at the
// mapping.
func TestClusterObservationCarriesEveryAttemptedDatabase(t *testing.T) {
	obs := clusterObservation(obsRecordCluster(), obsRecordObserved,
		obsRecordReadouts(), obsRecordNames(), nil)

	if obs.Namespace != testE2ENamespace || obs.Cluster != testClusterName || obs.Mesh != testMeshName {
		t.Errorf("Identity = %+v, want namespace/cluster/mesh slaptain-testing/slapd/ldap",
			obs.Identity)
	}
	if !obs.Observed.Equal(obsRecordObserved) {
		t.Errorf("Observed = %s, want %s — the freshness anchor is the only way a stalled "+
			"pass is detectable", obs.Observed, obsRecordObserved)
	}

	want := map[string]obsmetrics.DatabaseCSN{
		testDefaultDB: {Database: testDefaultDB, PairsExpected: 3, PairsRead: 3},
		testDB2:       {Database: testDB2, PairsExpected: 3, PairsRead: 1},
	}
	if len(obs.Databases) != len(want) {
		t.Fatalf("Databases = %+v, want one entry per attempted database (%d)",
			obs.Databases, len(want))
	}
	for _, got := range obs.Databases {
		w, ok := want[got.Database]
		if !ok {
			t.Errorf("unexpected database %q in the observation; the label must be the "+
				"SlapdDatabase name, not the suffix", got.Database)
			continue
		}
		if got.PairsExpected != w.PairsExpected || got.PairsRead != w.PairsRead {
			t.Errorf("database %q: read %d of %d, want %d of %d",
				got.Database, got.PairsRead, got.PairsExpected, w.PairsRead, w.PairsExpected)
		}
	}
}

// TestClusterObservationLabelsAnUnnamedDatabaseByItsSuffix pins the one
// judgement call in this layer. The readout is keyed by suffix; the label wants
// a CR name. When the translation has no entry — a database deleted mid-pass, or
// a caller that has not yet been taught to supply names — the reading is still
// evidence that was TAKEN, and dropping it would hide exactly the shape this
// family exists to report.
//
// So the suffix is used as the label and nothing is lost. It is visibly a DN, so
// a reader can tell which case they are looking at. The cost is that the series
// would be renamed if the name later becomes available, which is a one-time
// transition and strictly better than a silent hole.
func TestClusterObservationLabelsAnUnnamedDatabaseByItsSuffix(t *testing.T) {
	obs := clusterObservation(obsRecordCluster(), obsRecordObserved,
		obsRecordReadouts(), map[string]string{sfxA: testDefaultDB}, nil) // sfxB unnamed

	labels := make([]string, 0, len(obs.Databases))
	for _, db := range obs.Databases {
		labels = append(labels, db.Database)
	}
	if len(obs.Databases) != 2 {
		t.Fatalf("Databases = %v, want both attempted databases — an unnamed database must "+
			"not be dropped", labels)
	}
	if _, ok := obsSampleForDatabase(obs, sfxB); !ok {
		t.Errorf("databases = %v, want the unnamed one labelled by its suffix %q", labels, sfxB)
	}
}

// obsSampleForDatabase finds a DatabaseCSN by its rendered label.
func obsSampleForDatabase(obs obsmetrics.ClusterObservation, database string) (obsmetrics.DatabaseCSN, bool) {
	for _, db := range obs.Databases {
		if db.Database == database {
			return db, true
		}
	}
	return obsmetrics.DatabaseCSN{}, false
}

// ─── The cross-layer equivalence ────────────────────────────────────────────

// TestClusterObservationSpreadAbsenceSurvivesTheRender is the assertion this
// file was created for: `dbCSNReadout.Spread == nil` on one side and an omitted
// slaptain_csn_spread_seconds on the other are the SAME rule, written in two
// layers, and nothing else can see them drift.
//
// obsmetrics.RenderCluster is authoritative — it omits the sample while
// PairsRead < 2, because it cannot trust a caller — and dbCSNReadout.Spread
// states the rule at the source so that a non-metric consumer cannot be handed
// a fabricated zero either. This runs a reading all the way through, so a
// mapping that invented a 0 for an unmeasurable database, or dropped a measured
// 0 as if it were missing, fails here even though both layers' own suites stay
// green.
func TestClusterObservationSpreadAbsenceSurvivesTheRender(t *testing.T) {
	cases := []struct {
		name    string
		readout dbCSNReadout
		because string
	}{
		{
			name:    "three pods agreeing render a measured spread of zero",
			readout: dbCSNReadout{PodsExpected: 3, PodsRead: 3, Spread: ptr.To(time.Duration(0))},
			because: "0 over two or more readings is the healthiest reading on the surface, " +
				"and dropping it loses the only evidence of convergence there is",
		},
		{
			name: "a measured spread is carried through in seconds",
			readout: dbCSNReadout{
				PodsExpected: 3, PodsRead: 3, Spread: ptr.To(7500 * time.Millisecond),
			},
			because: "the gauge is seconds; a Duration passed through unconverted reads as " +
				"7.5 billion",
		},
		{
			// THE EXECUTED HOLE (2026-10-06). Before DatabaseCSN.Spread became
			// a pointer this input rendered `csn_spread_seconds 0` — a database
			// with no measurement at all, published as the healthiest reading
			// on the surface. The render's PairsRead >= 2 gate did not catch it
			// because PairsRead and the spread came from different places and
			// agreed only by convention.
			name:    "a reading with no measurement renders nothing even at three pairs read",
			readout: dbCSNReadout{PodsExpected: 3, PodsRead: 3, Spread: nil},
			because: "the absence now travels with the value; nothing between the probe " +
				"and the scrape may turn it back into a number",
		},
		{
			name:    "one readable pod renders no spread at all",
			readout: dbCSNReadout{PodsExpected: 3, PodsRead: 1, Spread: nil},
			because: "a spread needs two readings; 0 here is indistinguishable from two " +
				"pods that agree",
		},
		{
			name:    "no readable pod renders no spread, and says 0 of 3 was attempted",
			readout: dbCSNReadout{PodsExpected: 3, PodsRead: 0, Spread: nil},
			because: "the 2026-10-06 shape: the attempt is the observation, the spread is not",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := obsRecordCluster()
			obs := clusterObservation(sc, obsRecordObserved,
				map[string]dbCSNReadout{sfxA: tc.readout},
				map[string]string{sfxA: testDefaultDB}, nil)

			// Fail here rather than render a stub's zero value: the equivalence
			// below only means something once the reading reached the
			// observation.
			if len(obs.Databases) != 1 {
				t.Fatalf("Databases = %+v, want the one attempted database%s",
					obs.Databases, becauseTail(tc.because))
			}

			samples := renderedCluster(t, obs)
			got, present := obsSampleFor(samples,
				obsmetrics.MetricCSNSpread, obsmetrics.LabelDatabase, testDefaultDB)
			wantPresent := tc.readout.Spread != nil

			if present != wantPresent {
				t.Fatalf("%s rendered for database %q: %v, want %v\n"+
					"  readout: read %d of %d, spread %s\n"+
					"  rendered %s for: %v\n"+
					"  rule: the spread's absence is carried by the pointer, from "+
					"databaseCSNReadouts through this mapping to obsmetrics.RenderCluster, "+
					"which gates on it alone. Nothing in that path may invent, drop or "+
					"re-derive it%s",
					obsmetrics.MetricCSNSpread, testDefaultDB, present, wantPresent,
					tc.readout.PodsRead, tc.readout.PodsExpected, formatSpread(tc.readout.Spread),
					obsmetrics.MetricCSNSpread, obsDatabaseLabels(samples, obsmetrics.MetricCSNSpread),
					becauseTail(tc.because))
			}
			if wantPresent && got.Value != tc.readout.Spread.Seconds() {
				t.Errorf("%s = %v, want %v seconds%s", obsmetrics.MetricCSNSpread,
					got.Value, tc.readout.Spread.Seconds(), becauseTail(tc.because))
			}

			// The pair counts render unconditionally — including 0 read — because
			// presence in the observation is what means "attempted".
			for _, fam := range []string{obsmetrics.MetricCSNPairsRead, obsmetrics.MetricCSNPairsExpected} {
				if _, ok := obsSampleFor(samples, fam, obsmetrics.LabelDatabase, testDefaultDB); !ok {
					t.Errorf("%s missing for an attempted database; the attempt is the "+
						"observation%s", fam, becauseTail(tc.because))
				}
			}
		})
	}
}

// ─── Decision 5, end to end ─────────────────────────────────────────────────

// TestEmptyPassMapsToAnObservationThatRendersNothing is decision 5 across both
// layers: a pass that observed nothing publishes nothing — not an identity with
// a 1970 timestamp, not a set of zeros. It is the first place the two layers
// are exercised together, and the cheapest one to get wrong, since a mapping
// that always stamps `Observed` would publish a freshness anchor for a pass
// that never ran.
func TestEmptyPassMapsToAnObservationThatRendersNothing(t *testing.T) {
	sc := &ldapv1alpha1.SlapdCluster{
		Name: testClusterName, Namespace: testE2ENamespace,
	}
	obs := clusterObservation(sc, time.Time{}, nil, nil, nil)

	if len(obs.Databases) != 0 || len(obs.Peers) != 0 {
		t.Fatalf("observation = %+v, want nothing observed", obs)
	}
	if samples := renderedCluster(t, obs); len(samples) != 0 {
		t.Errorf("RenderCluster = %v, want no samples at all for a pass that observed "+
			"nothing", samples)
	}

	dbObs := databaseObservation(&ldapv1alpha1.SlapdDatabase{
		Name: testDefaultDB, Namespace: testE2ENamespace,
		Spec: ldapv1alpha1.SlapdDatabaseSpec{ClusterRef: testClusterName, Suffix: sfxA},
	}, "", time.Time{}, nil)
	if samples := renderedDatabase(t, dbObs); len(samples) != 0 {
		t.Errorf("RenderDatabase = %v, want no samples at all for a pass that observed "+
			"nothing", samples)
	}
}

// TestMeshIsCarriedAsTheEmptyStringForAStandaloneCluster pins the non-mesh
// case. A cluster with no meshRef is a supported deployment, and the label must
// be present and empty rather than omitted or invented — obsmetrics pins the
// render half in TestMeshLabelIsPresentAndEmptyForAStandaloneCluster, and this
// is the mapping half: the identity must not acquire a value spec.meshRef does
// not have.
func TestMeshIsCarriedAsTheEmptyStringForAStandaloneCluster(t *testing.T) {
	sc := obsRecordControlCluster() // no meshRef
	obs := clusterObservation(sc, obsRecordObserved, obsRecordReadouts(), obsRecordNames(), nil)

	// Asserted together on purpose: the identity's other two fields prove the
	// mapping ran at all, so an empty Mesh cannot pass by accident.
	if obs.Namespace != "other-ns" || obs.Cluster != "ldap-b" {
		t.Fatalf("Identity = %+v, want namespace/cluster other-ns/ldap-b", obs.Identity)
	}
	if obs.Mesh != "" {
		t.Errorf("Mesh = %q for a cluster with no meshRef, want the empty string — a mesh "+
			"label nobody declared is a join key with two authorities", obs.Mesh)
	}
}

// TestNoSiteLabelSurvivesTheMapping is the ADR-033 boundary as a tripwire at
// the one place it is tempting to cross. Monitoring is site-local (ADR-033
// amendment 2026-10-09): each site's Prometheus scrapes only that site, so the
// Prometheus is the site and no surface carries `site` — a label on Surface 2
// alone would silently empty every join on it against Surfaces 1 and 3. The
// operator DOES know its own site (ADR-028's single per-site fact, SITE_NAME on
// its own chart), which is exactly why this is asserted here rather than left
// to obsmetrics.
//
// If you landed here having added it: which site a series came from is which
// Prometheus holds it. Nothing in the operator needs to say so.
func TestNoSiteLabelSurvivesTheMapping(t *testing.T) {
	t.Setenv("SITE_NAME", "site-sentinel")

	obs := clusterObservation(obsRecordCluster(), obsRecordObserved,
		obsRecordReadouts(), obsRecordNames(), nil)
	if len(obs.Databases) == 0 {
		t.Fatalf("observation carried no database, so there is nothing to check yet")
	}

	for _, s := range renderedCluster(t, obs) {
		for k, v := range s.Labels {
			if k == "site" {
				t.Errorf("sample %s carries a `site` label", s)
			}
			if v == "site-sentinel" {
				t.Errorf("sample %s carries SITE_NAME in label %q; the operator's own site "+
					"is never a label on Surface 2", s, k)
			}
		}
	}
}

// ─── Peers: a provider count only where discovery ran ───────────────────────

// TestPeerProviderCountsAppearOnlyForDiscoveryPeers pins the join this layer
// has to perform, and why it needs the spec as well as the status. A discovery
// peer that resolved to nothing carries an EMPTY DiscoveredProviders — byte for
// byte what a `uri` peer carries — so status alone cannot tell "discovery found
// nothing", which ADR-033 calls how a cross-site outage begins, from "this peer
// is statically configured and working exactly as declared".
//
// Render 0 for the static peer and the one query this family exists to serve
// (`providers_discovered == 0`) fires on every static peer in the mesh. Drop it
// for the discovery peer and the query can never fire at all.
func TestPeerProviderCountsAppearOnlyForDiscoveryPeers(t *testing.T) {
	t.Run("a discovery peer carries its provider count", func(t *testing.T) {
		obs := clusterObservation(obsRecordCluster(), obsRecordObserved,
			obsRecordReadouts(), obsRecordNames(), nil)

		peers := map[string]obsmetrics.PeerObservation{}
		for _, p := range obs.Peers {
			peers[p.Peer] = p
		}
		got, ok := peers[testSite2]
		if !ok {
			t.Fatalf("Peers = %+v, want an entry for the discovery peer site-2", obs.Peers)
		}
		if !got.UsesDiscovery || !got.DiscoveryRan || got.ProvidersDiscovered != 2 {
			t.Errorf("site-2 = %+v, want UsesDiscovery and DiscoveryRan true with 2 "+
				"providers", got)
		}
		static, ok := peers[testSite3]
		if !ok {
			t.Fatalf("Peers = %+v, want an entry for the static peer site-3 too — it is a "+
				"configured peer, it simply has no discovery result", obs.Peers)
		}
		if static.UsesDiscovery {
			t.Errorf("site-3 = %+v, want UsesDiscovery false: a `uri` peer has no discovery "+
				"result, and 0 found would be an outage-shaped reading of a working "+
				"configuration", static)
		}
	})

	t.Run("a discovery peer that found nothing renders zero", func(t *testing.T) {
		obs := clusterObservation(obsRecordControlCluster(), obsRecordObserved,
			obsRecordReadouts(), obsRecordNames(), nil)
		if len(obs.Peers) != 1 {
			t.Fatalf("Peers = %+v, want the one configured peer", obs.Peers)
		}
		if !obs.Peers[0].UsesDiscovery || !obs.Peers[0].DiscoveryRan ||
			obs.Peers[0].ProvidersDiscovered != 0 {
			t.Fatalf("peer = %+v, want UsesDiscovery and DiscoveryRan true with 0 providers "+
				"— discovery RAN and resolved to nothing", obs.Peers[0])
		}

		s, ok := obsSampleFor(renderedCluster(t, obs),
			obsmetrics.MetricPeerProvidersDiscovered, obsmetrics.LabelPeer, testSite9)
		if !ok || s.Value != 0 {
			t.Errorf("%s{peer=\"site-9\"} present=%v value=%v, want present with 0: with no "+
				"companion `expected` family, `providers_discovered == 0` IS the alert",
				obsmetrics.MetricPeerProvidersDiscovered, ok, s.Value)
		}
	})

	t.Run("a static peer renders no provider sample", func(t *testing.T) {
		obs := clusterObservation(obsRecordCluster(), obsRecordObserved,
			obsRecordReadouts(), obsRecordNames(), nil)
		if len(obs.Peers) != 2 {
			t.Fatalf("Peers = %+v, want both configured peers", obs.Peers)
		}
		if _, ok := obsSampleFor(renderedCluster(t, obs),
			obsmetrics.MetricPeerProvidersDiscovered, obsmetrics.LabelPeer, testSite3); ok {
			t.Errorf("%s was rendered for the static peer site-3; there is no discovery "+
				"result to report, which is not a result of zero",
				obsmetrics.MetricPeerProvidersDiscovered)
		}
	})
}

// ─── The database pass ──────────────────────────────────────────────────────

// TestDatabaseObservationMapsProbeOutcomesHonestly pins suffixprobe's "positive
// evidence only" contract across the boundary. The two facts stay two facts: a
// pod that could not be probed and a pod probed to a bad verdict must not share
// a number, because the ADR-025 glue alert has no `for:` (glue never heals) and
// would fire on every transport blip if they did.
func TestDatabaseObservationMapsProbeOutcomesHonestly(t *testing.T) {
	probes := []podPresence{
		{pod: testPod0, assessed: true, present: true},
		{pod: testPod1, assessed: true, present: false, glued: true},
		{pod: testPod2, assessed: false},
		{pod: obsROPod0, readOnly: true, assessed: true, present: true},
	}
	obs := databaseObservation(obsRecordDatabase(), testMeshName, obsRecordObserved, probes)

	if obs.Namespace != testE2ENamespace || obs.Cluster != testClusterName ||
		obs.Mesh != testMeshName || obs.Database != testDefaultDB {
		t.Errorf("identity = %+v database %q, want slaptain-testing/slapd/ldap and "+
			"database default", obs.Identity, obs.Database)
	}

	want := map[string]obsmetrics.SuffixProbe{
		testPod0:  {Pod: testPod0, Probed: true, Healthy: true},
		testPod1:  {Pod: testPod1, Probed: true, Healthy: false},
		testPod2:  {Pod: testPod2, Probed: false},
		obsROPod0: {Pod: obsROPod0, Probed: true, Healthy: true},
	}
	if len(obs.Pods) != len(want) {
		t.Fatalf("Pods = %+v, want one entry per probed pod, read-only pods included (%d)",
			obs.Pods, len(want))
	}
	for _, got := range obs.Pods {
		w, ok := want[got.Pod]
		if !ok {
			t.Errorf("unexpected pod %q in the observation", got.Pod)
			continue
		}
		if got.Probed != w.Probed || (got.Probed && got.Healthy != w.Healthy) {
			t.Errorf("pod %q = probed %v healthy %v, want probed %v healthy %v\n"+
				"  rule: `assessed` becomes Probed and `present` becomes Healthy; a glue is "+
				"probed-and-bad (renders 0), an unreachable pod is not probed (renders nothing)",
				got.Pod, got.Probed, got.Healthy, w.Probed, w.Healthy)
		}
	}
}

// TestDatabaseObservationCarriesEachPodsRoleAndGlueVerdict pins the two ADR-025
// distinctions across the boundary. The controller holds both facts —
// podPresence.readOnly and podPresence.glued — and discarding either collapses
// a distinction the ADR exists to draw:
//
//   - an RO consumer mid-initial-sync renders identically to an RW pod with a
//     broken suffix entry, so an alert on `== 0` either false-fires on every
//     read-replica scale-up or is relaxed until it misses the real thing;
//   - a glue entry (never heals, backups from that pod unrestorable, manual
//     runbook) renders identically to a missing one (heals on the next sync).
//
// This is the mapping half; obsmetrics pins the render half.
func TestDatabaseObservationCarriesEachPodsRoleAndGlueVerdict(t *testing.T) {
	probes := []podPresence{
		{pod: testPod0, assessed: true, present: true},
		{pod: testPod1, assessed: true, present: false, glued: true},
		{pod: testPod2, assessed: true, present: false},
		{pod: obsROPod0, readOnly: true, assessed: true, present: false},
		{pod: obsROPod1, readOnly: true, assessed: false, glued: true},
	}
	obs := databaseObservation(obsRecordDatabase(), testMeshName, obsRecordObserved, probes)

	want := map[string]obsmetrics.SuffixProbe{
		testPod0:  {Pod: testPod0, Probed: true, Healthy: true},
		testPod1:  {Pod: testPod1, Probed: true, Healthy: false, Glue: true},
		testPod2:  {Pod: testPod2, Probed: true, Healthy: false},
		obsROPod0: {Pod: obsROPod0, ReadOnly: true, Probed: true, Healthy: false},
		// Unassessed: the verdict fields are never read, so a `glued` the probe
		// never established must not cross. "We could not look" becoming "this
		// pod is glued" would send a human to a runbook over a dial timeout.
		obsROPod1: {Pod: obsROPod1, ReadOnly: true, Probed: false},
	}
	if len(obs.Pods) != len(want) {
		t.Fatalf("Pods = %+v, want one entry per probed pod (%d)", obs.Pods, len(want))
	}
	for _, got := range obs.Pods {
		w, ok := want[got.Pod]
		if !ok {
			t.Errorf("unexpected pod %q", got.Pod)
			continue
		}
		if got != w {
			t.Errorf("pod %q = %+v, want %+v\n"+
				"  rule: readOnly→ReadOnly and glued→Glue, both only under assessed; "+
				"ADR-025 keeps `probed`, `healthy`, `glue` and `role` as four facts",
				got.Pod, got, w)
		}
	}
}

// TestDatabaseObservationRendersTheRoleAndGlueThroughTheRender is the mapping
// and the render together, which is where a crossed wire shows up: each layer
// can be green on its own fixtures while the controller feeds the wrong field.
func TestDatabaseObservationRendersTheRoleAndGlueThroughTheRender(t *testing.T) {
	obs := databaseObservation(obsRecordDatabase(), testMeshName, obsRecordObserved, []podPresence{
		{pod: testPod0, assessed: true, present: true},
		{pod: testPod1, assessed: true, present: false, glued: true},
		{pod: obsROPod0, readOnly: true, assessed: true, present: false},
	})
	if len(obs.Pods) != 3 {
		t.Fatalf("Pods = %+v, want three probed pods", obs.Pods)
	}

	samples := renderedDatabase(t, obs)
	roles := map[string]string{testPod0: obsmetrics.RoleReadWrite,
		testPod1: obsmetrics.RoleReadWrite, obsROPod0: obsmetrics.RoleReadOnly}
	for pod, role := range roles {
		s, ok := obsSampleFor(samples, obsmetrics.MetricSuffixProbeOK, obsmetrics.LabelPod, pod)
		if !ok {
			t.Errorf("no %s for pod %q (rendered: %s)", obsmetrics.MetricSuffixProbeOK, pod,
				obsSampleNames(samples))
			continue
		}
		if s.Labels[obsmetrics.LabelRole] != role {
			t.Errorf("pod %q rendered role %q, want %q — an RO pod mid-initial-sync and an "+
				"RW pod with a broken suffix must not be one alert",
				pod, s.Labels[obsmetrics.LabelRole], role)
		}
	}

	glue, ok := obsSampleFor(samples, obsmetrics.MetricSuffixGlue, obsmetrics.LabelPod, testPod1)
	if !ok || glue.Value != 1 {
		t.Errorf("%s{pod=\"slapd-1\"} present=%v value=%v, want present with 1: glue is its "+
			"own family because its alert needs no `for:` — it never heals",
			obsmetrics.MetricSuffixGlue, ok, glue.Value)
	}
	// Both pod-scoped families identify their subject the same way, so a glue
	// sample carries `role` as well — "glue on a read-write pod" is then a
	// selector rather than a join between two families.
	if got := glue.Labels[obsmetrics.LabelRole]; got != obsmetrics.RoleReadWrite {
		t.Errorf("%s{pod=\"slapd-1\"} rendered role %q, want %q",
			obsmetrics.MetricSuffixGlue, got, obsmetrics.RoleReadWrite)
	}
	for _, pod := range []string{testPod0, obsROPod0} {
		if _, present := obsSampleFor(samples, obsmetrics.MetricSuffixGlue, obsmetrics.LabelPod, pod); present {
			t.Errorf("%s rendered for pod %q, which is not glued; presence is the signal",
				obsmetrics.MetricSuffixGlue, pod)
		}
	}
}

// obsSampleNames renders a sample set's metric names for a failure message.
func obsSampleNames(samples []obsmetrics.Sample) string {
	names := make([]string, 0, len(samples))
	for _, s := range samples {
		names = append(names, fmt.Sprintf("%s=%g", s.Name, s.Value))
	}
	if len(names) == 0 {
		return "(nothing)"
	}
	return strings.Join(names, " ")
}

// ─── The adapter's help strings ─────────────────────────────────────────────

// TestEveryFamilyHasAHelpString closes a gap with no other failure channel.
//
// obscollector looks help up by metric name in obsMetricHelp and hands the
// result to prometheus.NewDesc, which validates fqName and the label names and
// does NOT validate help. A family added without a map entry therefore exports
// with an EMPTY help string: no error at registration, no error at scrape, no
// error in the test suite — just a series nobody can interpret, discovered by
// whoever is reading a dashboard at the time.
//
// It lives in this package rather than in obsmetrics because obsMetricHelp is
// here, and obsmetrics cannot import the controller (the dependency runs one
// way, which is the point of obsrecord.go). obsmetrics.Families is the shared
// list both ends check against, so neither side can hold a private copy that
// drifts.
func TestEveryFamilyHasAHelpString(t *testing.T) {
	for _, name := range obsmetrics.Families {
		help, present := obsMetricHelp[name]
		if !present {
			t.Errorf("no help string for %q. prometheus.NewDesc accepts an empty help "+
				"silently, so nothing else in this repository will tell you", name)
			continue
		}
		if strings.TrimSpace(help) == "" {
			t.Errorf("help string for %q is blank", name)
		}
		// A help string that only restates the name teaches nothing, and the
		// cheapest check for that is length: every real one here says what the
		// absence rule is.
		if len(help) < 40 {
			t.Errorf("help string for %q is %d characters (%q); say what the quantity is "+
				"AND when it is absent, which is the half a reader cannot guess",
				name, len(help), help)
		}
	}

	// A stale entry is the other direction: a family removed, its help left
	// behind, and the next family to take that name inherits a description of
	// something else.
	for name := range obsMetricHelp {
		if !slices.Contains(obsmetrics.Families, name) {
			t.Errorf("help string for %q, which obsmetrics no longer renders; remove it "+
				"with the family", name)
		}
	}
}

// TestClusterObservationMarksAPeerWhoseDiscoveryCouldNotBeAsked is the mapping
// half of the pair obsmetrics keeps adjacent: discovery that RAN and found
// nothing renders 0, discovery that could not be ASKED renders nothing at all.
//
// This is the layer where the distinction rots, and it already nearly did. The
// fact does NOT survive into status: a failed discovery leaves no providers, so
// checkPeerCSNConvergence finds no addresses and overwrites LastError with "no
// remote addresses available" — exactly what a discovery that ran and found
// nothing leaves behind — and on every healthy peer it overwrites LastError
// with the CSN verdict's. So the mapping cannot derive it, and must be told by
// the one caller that holds the error at the moment it exists.
func TestClusterObservationMarksAPeerWhoseDiscoveryCouldNotBeAsked(t *testing.T) {
	sc := obsRecordCluster() // site-2 uses discovery, site-3 is a static uri peer
	// What the status loop leaves behind on BOTH paths, to make the point that
	// the status alone cannot tell them apart.
	for i := range sc.Status.ExternalPeerStatuses {
		sc.Status.ExternalPeerStatuses[i].DiscoveredProviders = nil
		sc.Status.ExternalPeerStatuses[i].LastError = noRemoteAddressesMsg
	}
	discoveryFailed := map[string]string{
		testSite2: `kubeconfig secret "peer-site-2" not found`,
	}

	obs := clusterObservation(sc, obsRecordObserved, obsRecordReadouts(), obsRecordNames(),
		discoveryFailed)

	peers := map[string]obsmetrics.PeerObservation{}
	for _, p := range obs.Peers {
		peers[p.Peer] = p
	}
	failed, ok := peers[testSite2]
	if !ok {
		t.Fatalf("Peers = %+v, want an entry for site-2", obs.Peers)
	}
	if failed.DiscoveryRan {
		t.Errorf("site-2 = %+v, want DiscoveryRan false: discovery could not be asked, and a "+
			"provider count of 0 there reads as \"the remote site has no pods\" for a fault "+
			"that is entirely local", failed)
	}
	if _, present := obsSampleFor(renderedCluster(t, obs),
		obsmetrics.MetricPeerProvidersDiscovered, obsmetrics.LabelPeer, testSite2); present {
		t.Errorf("%s was rendered for site-2; Surface 1 already reports that peer Unreachable "+
			"(slaptain_cluster_peer_connected), and this surface must not invent a count to "+
			"say it again", obsmetrics.MetricPeerProvidersDiscovered)
	}
}

// TestClusterObservationKeepsDiscoveryThatRanApartFromDiscoveryThatFailed is
// the sibling, and the control for the test above: with the SAME status — no
// providers, the same LastError text — a peer absent from discoveryFailed ran
// its discovery and found nothing, which is the outage this family exists to
// report. A mapping that read the status instead of its argument would make
// these two identical, and would do it in the safe-looking direction.
func TestClusterObservationKeepsDiscoveryThatRanApartFromDiscoveryThatFailed(t *testing.T) {
	sc := obsRecordCluster()
	for i := range sc.Status.ExternalPeerStatuses {
		sc.Status.ExternalPeerStatuses[i].DiscoveredProviders = nil
		sc.Status.ExternalPeerStatuses[i].LastError = noRemoteAddressesMsg
	}

	obs := clusterObservation(sc, obsRecordObserved, obsRecordReadouts(), obsRecordNames(), nil)

	var ran obsmetrics.PeerObservation
	for _, p := range obs.Peers {
		if p.Peer == testSite2 {
			ran = p
		}
	}
	if !ran.DiscoveryRan || ran.ProvidersDiscovered != 0 {
		t.Fatalf("site-2 = %+v, want DiscoveryRan true with 0 providers — no caller reported "+
			"a discovery failure, so discovery ran and resolved to nothing", ran)
	}
	s, present := obsSampleFor(renderedCluster(t, obs),
		obsmetrics.MetricPeerProvidersDiscovered, obsmetrics.LabelPeer, testSite2)
	if !present || s.Value != 0 {
		t.Errorf("%s{peer=\"site-2\"} present=%v value=%v, want present with 0: `== 0` is "+
			"this family's whole alert", obsmetrics.MetricPeerProvidersDiscovered, present, s.Value)
	}
}

// ─── The spec→status join ───────────────────────────────────────────────────

// duplicateNamePeers is two discovery-mode peers SHARING one name, whose
// statuses carry different provider counts.
//
// Yes, it is a misconfiguration — and nothing in the operator refuses it today,
// which is the entire reason the join has to survive it rather than assume it
// away: `ExternalPeers` carries no `+listMapKey=name`, and `judgeExternalPeers`
// rejects a duplicate `siteIndex` but says nothing about a duplicate name. So
// two peers reach this mapping sharing one, and a name-keyed lookup serves the
// first one's count for both. That is a WRONG reading rather than a missing
// one — the reviewer measured it by hand: the peer with three discovered
// providers reported 0 — and a wrong reading is the one failure mode this
// surface has no way to look suspicious while committing.
//
// The counts are deliberately 3 and 0, so a collapsed join cannot pass by
// accident whichever of the two it happens to keep.
func duplicateNamePeers() *ldapv1alpha1.SlapdCluster {
	sc := obsRecordCluster()
	sc.Spec.Replication.ExternalPeers = []ldapv1alpha1.ExternalPeer{
		{Name: dupPeer, Discovery: &ldapv1alpha1.ExternalPeerDiscovery{}},
		{Name: dupPeer, Discovery: &ldapv1alpha1.ExternalPeerDiscovery{}},
	}
	sc.Status.ExternalPeerStatuses = []ldapv1alpha1.ExternalPeerStatus{
		{Name: dupPeer, DiscoveredProviders: []ldapv1alpha1.DiscoveredProvider{
			{Ordinal: 0, Address: "203.0.113.1"},
			{Ordinal: 1, Address: "203.0.113.2"},
			{Ordinal: 2, Address: "203.0.113.3"},
		}},
		{Name: dupPeer},
	}
	return sc
}

// TestClusterObservationJoinsPeersByIndexNotByName pins the fix for the
// non-injective join. The status list is rebuilt from the same spec slice in
// the same order on every pass, so POSITION is the reliable key and the name is
// not a key at all.
//
// Asserted positionally for that reason: an assertion that looked peers up by
// name would be the bug, written as a test.
func TestClusterObservationJoinsPeersByIndexNotByName(t *testing.T) {
	obs := clusterObservation(duplicateNamePeers(), obsRecordObserved,
		obsRecordReadouts(), obsRecordNames(), nil)

	if len(obs.Peers) != 2 {
		t.Fatalf("Peers = %+v, want one observation per SPEC peer (2) — a name-keyed join "+
			"collapses two peers onto one", obs.Peers)
	}
	for i, want := range []int{3, 0} {
		got := obs.Peers[i]
		if got.Peer != dupPeer {
			t.Errorf("Peers[%d].Peer = %q, want %q verbatim from the spec", i, got.Peer, dupPeer)
		}
		if !got.UsesDiscovery || !got.DiscoveryRan {
			t.Errorf("Peers[%d] = %+v, want a discovery peer whose discovery ran", i, got)
		}
		if got.ProvidersDiscovered != want {
			t.Errorf("Peers[%d].ProvidersDiscovered = %d, want %d — this is the join: spec "+
				"position i must read status position i, never the first status whose name "+
				"matches (the reviewer measured the 3-provider peer reporting 0)",
				i, got.ProvidersDiscovered, want)
		}
	}

	// The duplicate survives into the render as two samples with an identical
	// label set, which is what makes the misconfiguration VISIBLE: the
	// collector logs the collision at Error. Dropping one here would hide a
	// broken spec behind a plausible-looking series.
	samples := renderedCluster(t, obs)
	n := 0
	for _, s := range samples {
		if s.Name == obsmetrics.MetricPeerProvidersDiscovered {
			n++
		}
	}
	if n != 2 {
		t.Errorf("rendered %d %s samples, want 2 — one per spec peer, duplicate labels "+
			"included (rendered: %s)", n, obsmetrics.MetricPeerProvidersDiscovered,
			obsSampleNames(samples))
	}
}

// TestDuplicateNamesMakeOneDiscoveryFailureSilenceBothPeers pins the asymmetry
// the join deliberately keeps, rather than leaving it to a comment.
//
// `discoveryFailed` is keyed by NAME and stays that way, because that is the
// shape its producer can build: the status loop holds `ep`, not an index into a
// list it is still appending to. Under duplicate names that key collapses, so
// ONE failed discovery silences BOTH peers.
//
// That is the harmless direction and the reason it is tolerable: absence for a
// peer that was fine costs a series Surface 1 still covers
// (slaptain_cluster_peer_connected), whereas the provider-count join collapsing
// would publish one peer's count as the other's. Absence over a wrong number,
// every time — and the duplicate itself surfaces through the render, pinned
// above.
func TestDuplicateNamesMakeOneDiscoveryFailureSilenceBothPeers(t *testing.T) {
	obs := clusterObservation(duplicateNamePeers(), obsRecordObserved,
		obsRecordReadouts(), obsRecordNames(),
		map[string]string{dupPeer: `kubeconfig secret "peer-dup" not found`})

	if len(obs.Peers) != 2 {
		t.Fatalf("Peers = %+v, want one observation per spec peer (2)", obs.Peers)
	}
	for i, got := range obs.Peers {
		if got.DiscoveryRan {
			t.Errorf("Peers[%d] = %+v, want DiscoveryRan false: the name key collapses, so a "+
				"failure reported for \"dup\" silences both — chosen because absence for a "+
				"healthy peer is cheaper than a wrong count for a broken one", i, got)
		}
	}
	for _, s := range renderedCluster(t, obs) {
		if s.Name == obsmetrics.MetricPeerProvidersDiscovered {
			t.Errorf("rendered %s under a collapsed discovery failure; neither peer may "+
				"carry a count that was not taken", s)
		}
	}
}
