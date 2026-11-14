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
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/types"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
	"github.com/chuck-chuck-chuck-net/slaptain/operator/internal/obsmetrics"
)

// This file is the boundary between what a reconcile pass observed and what
// Surface 2 exports (ADR-033 decision 3): the only place that TRANSLATES a
// reconcile reading into an obsmetrics type, and the direction is one-way on
// purpose — the controller's own verdict types (localVerdict, dbCSNReadout,
// podPresence) know nothing about metrics, so a rendering decision can never
// leak into a reconcile decision.
//
// Three other files in this package name obsmetrics types without reading one:
// obscollector.go (the Prometheus adapter) and the two controllers, which carry
// a *obsmetrics.Store field and call the functions below. None of them maps a
// reading, which is the duplication this boundary exists to prevent.
//
// Both functions here are pure: they read their arguments and allocate a
// value. No client, no clock, no environment. That is what makes the seam
// testable at all — and it is the whole seam, because everything downstream of
// an obsmetrics.Observation is already covered by obsmetrics' own tests.
//
// Why a mapping layer rather than letting the controllers build observations
// inline: this is where a reading stops being reconcile state and becomes an
// export, and decision 5's rules have to survive the crossing intact. Since
// 2026-10-06 the spread's absence survives it as a POINTER rather than as a
// convention the two sides agree on — this layer copies it and decides
// nothing — so what obsrecord_test.go pins is no longer two statements of one
// rule, but that nothing in the crossing invents, drops or re-derives a
// reading. The per-pod probe is the same story: role and glue are facts only
// this side holds, and losing them here is losing them everywhere.

// clusterObservation maps one SlapdCluster pass into the observation Surface 2
// exports for it: the per-database CSN measurement, and the per-peer discovery
// result.
//
// Identity comes from the cluster object itself — namespace, name, and
// spec.meshRef as the `mesh` label, which is the EMPTY STRING for a standalone
// cluster rather than an omitted or invented value (obsmetrics pins that in
// TestMeshLabelIsPresentAndEmptyForAStandaloneCluster). `site` is deliberately
// not derived here or anywhere: monitoring is site-local (ADR-033 amendment
// 2026-10-09), the Prometheus is the site, and a label only Surface 2 carried
// would silently empty any join on it against Surfaces 1 and 3.
//
// readouts is evaluateLocalConvergence's ReadoutBySuffix, keyed by database
// SUFFIX. databaseNames translates a suffix to the SlapdDatabase name, because
// the `database` label is the name a reader can take back to `kubectl get sd`
// and a suffix is not. A suffix absent from databaseNames falls back to the
// suffix itself: the reading is evidence that was taken, and dropping it over a
// naming gap would hide exactly the 2026-10-06 shape this family exists to
// report (a site that could read none of its own databases). The fallback is
// visibly a DN, so a reader can tell which case they are looking at.
//
// discoveryFailed names the peers whose discovery could not be ASKED this pass,
// mapped to the error that stopped it, and it is a PARAMETER because the fact
// does not survive into status. On that path `discoverRemotePeerProviders`
// returns no providers, so `checkPeerCSNConvergence` finds no addresses and
// overwrites `LastError` with "no remote addresses available" — byte for byte
// what a discovery that RAN and found nothing leaves behind. The two cases
// ADR-033 needs separated are indistinguishable in
// status.externalPeerStatuses by the time this function sees it, and
// `LastError` is additionally overwritten by the CSN verdict on every
// successful peer, so reading it here would call a merely-lagging peer's
// discovery "failed".
//
// The caller holds the error at the one place it exists — the discovery branch
// of the status loop, before anything overwrites it — and nothing else does.
// A nil map means no peer failed discovery.
//
// Peers are read from the SPEC, the STATUS and discoveryFailed together,
// because the three facts live in three places and no two of them answer the
// question. status.externalPeerStatuses cannot say which peers discover at all
// — a discovery peer that resolved to nothing carries an empty
// DiscoveredProviders, byte for byte like a `uri` peer that never discovers —
// and spec.replication.externalPeers is where `discovery` lives (it carries the
// mesh-DERIVED peers too by this point, since applyMeshWiring writes them
// there).
//
// discoveryFailed is the third, and it CANNOT be derived from either of the
// other two. A discovery that failed leaves no providers, so
// checkPeerCSNConvergence finds no addresses and overwrites status.lastError
// with "no remote addresses available" — the same text, byte for byte, that a
// discovery which ran and found nothing leaves behind — and on a healthy peer
// it overwrites lastError with the CSN verdict's. Only the caller holds the
// error at the moment it exists, which is why it is a parameter and not a
// lookup.
func clusterObservation(
	sc *ldapv1alpha1.SlapdCluster,
	observed time.Time,
	readouts map[string]dbCSNReadout,
	databaseNames map[string]string,
	discoveryFailed map[string]string,
) obsmetrics.ClusterObservation {
	obs := obsmetrics.ClusterObservation{
		Namespace: sc.Namespace,
		Cluster:   sc.Name,
		Mesh:      sc.Spec.MeshRef,
		Observed:  observed,
	}

	// Sorted so one pass's observation is byte-stable; the render sorts its own
	// output too, but a stable observation is what makes a store snapshot
	// diffable before it is ever rendered.
	suffixes := make([]string, 0, len(readouts))
	for suffix := range readouts {
		suffixes = append(suffixes, suffix)
	}
	sort.Strings(suffixes)

	for _, suffix := range suffixes {
		readout := readouts[suffix]
		database := databaseNames[suffix]
		if database == "" {
			database = suffix
		}
		// The spread crosses as a POINTER, unchanged. Both sides have carried
		// one since 2026-10-06, so the absence travels with the value and this
		// layer has nothing to decide: it cannot fabricate a measurement, and
		// it cannot drop one. The previous shape — nil flattened to 0 here,
		// with the render re-deriving absence from PairsRead — is the hole the
		// correctness review executed, and it is now unexpressible rather than
		// merely untaken.
		obs.Databases = append(obs.Databases, obsmetrics.DatabaseCSN{
			Database:      database,
			PairsExpected: readout.PodsExpected,
			PairsRead:     readout.PodsRead,
			Spread:        readout.Spread,
		})
	}

	// Three sources, because no two of them can answer the question alone:
	//
	//	spec             — whether this peer discovers at all (`discovery` is a
	//	                   spec field; a `uri` peer has no discovery result, not
	//	                   a result of zero).
	//	discoveryFailed  — whether discovery could be ASKED this pass. The
	//	                   caller holds the error at the moment it exists and
	//	                   nothing else does; see the note below.
	//	status           — how many providers it found, when it ran.
	//
	// JOINED BY INDEX, not by name. ExternalPeers carries no +listMapKey=name
	// and judgeExternalPeers refuses a duplicate siteIndex but not a duplicate
	// name, so two peers may legitimately reach here sharing one — and a
	// name-keyed lookup then serves one peer's provider count for the other,
	// which is a wrong reading rather than a missing one. The status list is
	// rebuilt from this same spec slice in this same order on every pass
	// (Reconcile's external-peer loop), so position is the reliable key.
	//
	// Length-checked anyway: a caller that has not populated status yet gets no
	// provider count rather than a fabricated zero, which is the direction
	// decision 5 asks for whenever the two lists could disagree.
	//
	// discoveryFailed stays keyed by NAME because that is the shape the caller
	// can build (it has `ep` in hand, not an index into a list it is still
	// appending to). Under duplicate names that key collapses and BOTH peers
	// read as failed — absence for a peer that was fine, which is the harmless
	// direction; the duplicate itself then shows up as a duplicate sample in
	// the collector, which logs it.
	statuses := sc.Status.ExternalPeerStatuses
	for i, ep := range sc.Spec.Replication.ExternalPeers {
		peer := obsmetrics.PeerObservation{
			Peer:          ep.Name,
			UsesDiscovery: ep.Discovery != nil,
		}
		_, failed := discoveryFailed[ep.Name]
		if peer.UsesDiscovery && !failed && i < len(statuses) {
			peer.DiscoveryRan = true
			peer.ProvidersDiscovered = len(statuses[i].DiscoveredProviders)
		}
		obs.Peers = append(obs.Peers, peer)
	}

	return obs
}

// databaseObservation maps one SlapdDatabase pass into the observation
// Surface 2 exports for it: the per-pod ADR-025 suffix probe, with each pod's
// fleet and glue verdict intact.
//
// mesh is a parameter rather than something read off sd, because a
// SlapdDatabase has no meshRef at all: under ADR-028 the mesh is the cluster's
// fact and a database reaches it only through clusterRef. The caller has the
// cluster in hand already.
//
// probes are the controller's own per-pod probe results, read-write and
// read-only pods alike, and the mapping is exactly suffixprobe's "positive
// evidence only" contract:
//
//	assessed == false  → Probed false. Nothing is rendered for that pod; the
//	                     probe could not be taken, which is never evidence.
//	present            → Healthy. A glue or a missing entry is `assessed and
//	                     not present`, so it renders 0 — probed, and the answer
//	                     was bad (ADR-025).
//	glued              → Glue, and only under assessed. ADR-025's worse state,
//	                     which never heals: it must not arrive at the surface as
//	                     an ordinary `Healthy: false`, because then its alert
//	                     has to be written for the state that DOES heal.
//	readOnly           → ReadOnly. An RO consumer mid-initial-sync is
//	                     legitimately missing its suffix entry (ADR-025's
//	                     2026-09-14 amendment gave that its own condition
//	                     reason), and the controller is the only layer that
//	                     knows which fleet a pod belongs to. Dropping it here is
//	                     what made the two indistinguishable downstream.
//
// Withheld syncrepl convergence is deliberately NOT mapped here. It is the
// SyncreplStanzasReady condition, so Surface 1 already exports it as
// slaptain_database_condition{condition="SyncreplStanzasReady",reason=…} —
// including the Unknown case this layer could not express — and ADR-033
// decision 1 gives a quantity to the vantage point that observes it directly.
// This function took the condition as an argument until 2026-10-06; it no
// longer does, and re-adding the parameter is the thing to notice.
func databaseObservation(
	sd *ldapv1alpha1.SlapdDatabase,
	mesh string,
	observed time.Time,
	probes []podPresence,
) obsmetrics.DatabaseObservation {
	obs := obsmetrics.DatabaseObservation{
		Namespace: sd.Namespace,
		Cluster:   sd.Spec.ClusterRef,
		Mesh:      mesh,
		Database:  sd.Name,
		Observed:  observed,
	}

	for _, p := range probes {
		// ReadOnly is identity, not verdict: which fleet the pod belongs to is
		// known whether or not the probe reached it, and the controller is the
		// only layer that knows at all.
		probe := obsmetrics.SuffixProbe{
			Pod:      p.pod,
			ReadOnly: p.readOnly,
			Probed:   p.assessed,
		}
		// The verdict fields are read ONLY under `assessed`. An unassessed
		// pod's `present` is meaningless, and its `glued` is worse than
		// meaningless — crossing it would turn a dial timeout into a claim that
		// sends a human to ADR-025's manual runbook and condemns every backup
		// taken from that pod.
		if p.assessed {
			probe.Healthy = p.present
			probe.Glue = p.glued
		}
		obs.Pods = append(obs.Pods, probe)
	}

	return obs
}

// ── Recording: one point per pass, one eviction per deletion ─────────────────

// recordObservation publishes this SlapdCluster pass's readings for the
// Prometheus collector to render on the next scrape.
//
// Called from ONE place in the cluster reconcile — immediately before the
// end-of-pass status apply — and the single call site is the design, not an
// accident of where it fit. See the comment there.
//
// Builds a FRESH observation and hands it over whole. Nothing already in the
// store is ever mutated: Snapshot returns value copies whose slice fields share
// backing arrays, and that documented read-only contract holds only because the
// writer swaps observations rather than editing them.
//
// A nil store records nothing, which is how every unit test in this package
// runs without one.
func (r *SlapdClusterReconciler) recordObservation(
	sc *ldapv1alpha1.SlapdCluster,
	readouts map[string]dbCSNReadout,
	dbInfos []databaseCSNInfo,
	discoveryFailed map[string]string,
) {
	if r.Observations == nil {
		return
	}
	names := make(map[string]string, len(dbInfos))
	for _, db := range dbInfos {
		names[db.suffix] = db.name
	}
	r.Observations.Record(
		obsmetrics.Key{Namespace: sc.Namespace, Name: sc.Name},
		clusterObservation(sc, time.Now(), readouts, names, discoveryFailed),
	)
}

// evictObservation stops this cluster's Surface 2 series. Called from the
// reconcile that saw the object gone.
func (r *SlapdClusterReconciler) evictObservation(key types.NamespacedName) {
	if r.Observations == nil {
		return
	}
	r.Observations.Evict(obsmetrics.Key{Namespace: key.Namespace, Name: key.Name})
}

// recordObservation publishes this SlapdDatabase pass's readings. Keyed by the
// SLAPDDATABASE's name: one cluster has many databases, and keying on the
// cluster would have each database's pass overwrite its siblings' (see
// obsmetrics.Key).
//
// Same single-call-site and fresh-observation rules as the cluster's.
func (r *SlapdDatabaseReconciler) recordObservation(
	sd *ldapv1alpha1.SlapdDatabase,
	mesh string,
	probes []podPresence,
) {
	if r.Observations == nil {
		return
	}
	r.Observations.Record(
		obsmetrics.Key{Namespace: sd.Namespace, Name: sd.Name},
		databaseObservation(sd, mesh, time.Now(), probes),
	)
}

// evictObservation stops this database's Surface 2 series.
func (r *SlapdDatabaseReconciler) evictObservation(key types.NamespacedName) {
	if r.Observations == nil {
		return
	}
	r.Observations.Evict(obsmetrics.Key{Namespace: key.Namespace, Name: key.Name})
}
