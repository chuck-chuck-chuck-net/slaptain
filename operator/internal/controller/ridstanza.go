/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"fmt"
	"slices"

	"sigs.k8s.io/controller-runtime/pkg/client"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
	"github.com/chuck-chuck-chuck-net/slaptain/operator/internal/ridcoord"
)

// The consumption seam of ADR-031: where a coordinate stops being arithmetic
// in a package nothing calls and becomes the number on a syncrepl stanza.
//
// Everything here is pure except ridLayoutFor, which performs the single read
// the layout needs. The stanza builders are long and take a dozen arguments;
// the derivation deliberately does not live inside them, because a RID that is
// wrong by one is a stanza carrying another provider's number and slapd accepts
// a duplicate RID in silence (ADR-031, "What slapd actually requires").
//
// RID-PLAN hazard 1: one coordinate function, not two. Every number below comes
// from ridcoord.Layout.Local or ridcoord.Layout.Remote, and a refusal from
// ridcoord propagates verbatim — never a substituted, capped or improvised
// number.

// inClusterStanzaRIDs is the RID of every in-cluster provider's stanza, as seen
// from one pod: one entry per provider ordinal in [0, replicas), keyed by that
// ordinal, each value ridcoord.Layout.Local(segment, ordinal).
//
// selfOrdinal says who is asking, and it is a POINTER on purpose:
//
//   - non-nil — a read-write pod, which is itself a provider. Its own ordinal
//     is omitted from the result: a pod never replicates from itself. The slot
//     is NOT reclaimed or renumbered — reclaiming it would make the offset
//     depend on who is reading, which is the uniformity ADR-031 decision 1
//     trades two dead slots for.
//   - nil — the caller is not a provider at all, which is the read-only pod.
//     It receives the full set. That is the whole of "read replicas stay free":
//     an RO pod consumes from the same providers and therefore carries the same
//     RIDs the RW pods do (ADR-031 Consequences).
//
// A sentinel int32 was rejected for the second case. The RO caller would have
// had to remember to pass -1, and forgetting it — passing its own 0 — drops
// provider 0's stanza from an otherwise complete set, with no refusal anywhere:
// a short set under the atomic Replace is a deletion of that link (RID-PLAN
// hazard 2). A convention that must be remembered at a call site is the class
// of bug this scheme exists to make unexpressible, so the type carries it
// instead.
//
// A non-nil selfOrdinal outside [0, replicas) skips nothing and is not an
// error: there is no such provider to omit. The caller's own loop cannot
// produce one, and refusing would withhold a whole site's stanzas over a
// transient the ADR does not call a fault.
//
// Refuses with whatever ridcoord refused — an invalid layout, a segment outside
// 0..C-1, an ordinal outside 0..P-1 — and returns no partial map. A short
// stanza set is a deletion under the atomic Replace, so there is no such thing
// as "most of the RIDs".
func inClusterStanzaRIDs(layout ridcoord.Layout, segment, replicas int32, selfOrdinal *int32) (map[int32]int32, error) {
	// The layout and the segment are judged once, before the fleet is walked,
	// so that an empty fleet refuses a bad segment exactly as a populated one
	// does. Ordinal 0 is in range for every layout that validates at all
	// (podsPerSite is at least 1), so this probe can only answer for the two
	// axes it is here to answer for.
	if _, err := layout.Local(segment, 0); err != nil {
		return nil, err
	}

	rids := make(map[int32]int32, replicas)
	for ordinal := range replicas {
		if selfOrdinal != nil && *selfOrdinal == ordinal {
			continue
		}
		rid, err := layout.Local(segment, ordinal)
		if err != nil {
			// No partial map: a short stanza set is a deletion under the
			// atomic Replace (RID-PLAN hazard 2).
			return nil, err
		}
		rids[ordinal] = rid
	}
	return rids, nil
}

// externalStanzaRID is the RID of this pod's syncrepl link to peer site
// peerSiteIndex, for the database holding segment:
// ridcoord.Layout.Remote(segment, peerSiteIndex).
//
// The coordinate names the LINK, not the remote pod answering it. Which remote
// pod serves a link is a runtime selection that moves when the far end is
// replaced; the stanza's provider= says which pod, the RID says which link
// (ADR-031 decision 1). A pod holds at most one link per peer site per database
// (decision 9), which is what makes one slot per peer site enough.
//
// Refuses with whatever ridcoord refused. Nothing here caps or wraps a site
// index into range: the remote block is S wide, and a peer index past it has no
// slot, not a different one.
func externalStanzaRID(layout ridcoord.Layout, segment, peerSiteIndex int32) (int32, error) {
	rid, err := layout.Remote(segment, peerSiteIndex)
	if err != nil {
		// Zero rather than the number that was refused: rid = 0 is reserved
		// and never a coordinate, so a caller that drops the error writes
		// something obviously wrong rather than something plausibly wrong.
		return 0, err
	}
	return rid, nil
}

// ridLayoutFor resolves the RID layout in force for sc.
//
// The release gate M3 left open: SlapdMesh.spec.ridLayout ships as live API
// that nothing reads, so a declared layout is silently ignored — which ADR-024
// forbids outright (a spec field that cannot be honoured is refused or
// converged, never ignored).
//
// Contract:
//
//   - No meshRef: ridcoord.DefaultLayout(), and NO I/O at all. A cluster with
//     no mesh has only to be self-consistent; cross-site agreement is a
//     legibility property and not a correctness one (ADR-031 decision 5).
//   - Under meshRef: the mesh's spec.ridLayout, defaulted PER AXIS. A zero axis
//     reads as unset rather than as unusable, because API-server defaulting
//     runs only on the API server's path — a mesh unmarshalled from a manifest,
//     restored from a backup, or carrying only one of the two fields reaches
//     this code with a zero in the other.
//   - An absent ridLayout on a mesh that exists is not an error: it means the
//     defaults, exactly as omitting it documents.
//   - An unreadable mesh — not found, RBAC, a cache miss — means the layout is
//     UNKNOWN, and unknown withholds (RID-PLAN hazard 3). It returns an error,
//     and the Layout it returns alongside is deliberately NOT usable: a caller
//     that drops the error must still withhold rather than converge on
//     defaults, because defaulting a layout that the mesh may have narrowed
//     renumbers every stanza on every pod of this site and no other.
func ridLayoutFor(ctx context.Context, c client.Client, sc *ldapv1alpha1.SlapdCluster) (ridcoord.Layout, error) {
	if sc.Spec.MeshRef == "" {
		return ridcoord.DefaultLayout(), nil
	}

	var mesh ldapv1alpha1.SlapdMesh
	key := client.ObjectKey{Name: sc.Spec.MeshRef, Namespace: sc.Namespace}
	if err := c.Get(ctx, key, &mesh); err != nil {
		// The zero Layout is deliberately unusable: ridcoord.Validate refuses
		// it, so a caller that drops this error still withholds instead of
		// converging on the defaults.
		return ridcoord.Layout{}, fmt.Errorf(
			"RID layout unknown: cannot read SlapdMesh %q named by spec.meshRef: %w", key, err)
	}

	// Defaulted per axis. A zero axis is unset, not unusable: API-server
	// defaulting runs only on the API server's path, and a mesh unmarshalled
	// from a manifest or restored from a backup reaches this code with a zero
	// in the axis it never declared.
	layout := ridcoord.DefaultLayout()
	if rl := mesh.Spec.RIDLayout; rl != nil {
		if rl.PodsPerSite != 0 {
			layout.PodsPerSite = rl.PodsPerSite
		}
		if rl.MaxSites != 0 {
			layout.MaxSites = rl.MaxSites
		}
	}
	return layout, nil
}

// Reasons an external peer list cannot be turned into remote coordinates. They
// are condition reasons, so each names a different human response: declare an
// index, pick a free one, pick a smaller one, or stop asking for a second link.
const (
	// reasonPeerSiteIndexMissing — a peer declares no siteIndex and the
	// cluster is not under meshRef, so nothing derives one. The CEL rule
	// normally catches this; reaching it means a CR that predates the field or
	// one applied with validation off.
	reasonPeerSiteIndexMissing = "PeerSiteIndexMissing"
	// reasonPeerSiteIndexCollision — two peers declare one siteIndex, so they
	// share a remote slot: one RID for two providers, which is the shared
	// connid state slapd never rejects (RID-PLAN hazard 5).
	reasonPeerSiteIndexCollision = "PeerSiteIndexCollision"
	// reasonPeerSiteIndexOutOfRange — a siteIndex is negative or at or above
	// the layout's S. The remote block has no slot for it.
	reasonPeerSiteIndexOutOfRange = "PeerSiteIndexOutOfRange"
	// reasonReplicasPerPeerUnsupported — a fan-out above 1 asks one pod for a
	// second link to one peer site, and the remote block is S wide precisely
	// because that does not happen (ADR-031 decision 9). Refused until ADR-032
	// redefines the quantity as links per site PAIR.
	reasonReplicasPerPeerUnsupported = "ReplicasPerPeerUnsupported"
)

// judgeExternalPeers decides whether sc's external peer list can be given
// remote coordinates at all, under the layout in force.
//
// Everything it refuses is a RID collision or a RID that does not exist, so the
// answer is the same withhold ADR-031 decision 6 prescribes for a refused
// segment — hence the shared verdict type. Only OK, Reason and Message are
// meaningful here; Segment is not a peer-level quantity and stays zero.
//
// The checks, in the order a human can act on them:
//
//  1. replicasPerPeer >= 2, cluster-wide default or per peer. Refused naming
//     the peer and the value (ADR-031 decision 9).
//  2. A peer with no siteIndex. Under meshRef every peer is derived and carries
//     one; off-mesh it is the deployer's to declare.
//  3. A siteIndex outside 0..S-1 for the layout in force.
//  4. Two peers declaring one siteIndex, named BOTH — symmetric, because
//     neither of them may win the race to write a stanza over the other's.
//
// Pure: the peer list is already resolved on sc (mesh wiring is applied in
// memory before this is reached), so there is no read and no ordering question.
func judgeExternalPeers(sc *ldapv1alpha1.SlapdCluster, layout ridcoord.Layout) ridSegmentVerdict {
	peers := sc.Spec.Replication.ExternalPeers

	// 1. A second link to one peer site has no slot to live in.
	for i := range peers {
		rpp := resolveReplicasPerPeer(sc.Spec.Replication.ReplicasPerPeer, peers[i].ReplicasPerPeer)
		if rpp >= 2 {
			return ridSegmentVerdict{
				Reason: reasonReplicasPerPeerUnsupported,
				Message: fmt.Sprintf(
					"replicasPerPeer %d for external peer %q asks one pod for %d syncrepl links to one peer site, and the RID matrix has one slot per peer site (ADR-031 decision 9). Set replicasPerPeer to 1, or leave it unset.",
					rpp, peers[i].Name, rpp),
			}
		}
	}

	// 2. Declared at all. nil is absence; a positional fallback is exactly the
	//    inference ADR-031 decision 4 forbids.
	for i := range peers {
		if peers[i].SiteIndex == nil {
			return ridSegmentVerdict{
				Reason: reasonPeerSiteIndexMissing,
				Message: fmt.Sprintf(
					"external peer %q declares no siteIndex, so its cross-site syncrepl link has no coordinate in the RID matrix. Declare externalPeers[].siteIndex (0..%d), or put this cluster under a SlapdMesh, where it is the peer site's serverIDIndex.",
					peers[i].Name, layout.MaxSites-1),
			}
		}
	}

	// 3. In range for THIS layout. The ceiling is the mesh's S, so the same CR
	//    is judged differently under a layout the mesh narrowed.
	for i := range peers {
		idx := *peers[i].SiteIndex
		if idx < 0 || idx >= layout.MaxSites {
			return ridSegmentVerdict{
				Reason: reasonPeerSiteIndexOutOfRange,
				Message: fmt.Sprintf(
					"external peer %q declares siteIndex %d, outside 0..%d: the RID layout in force (podsPerSite=%d, maxSites=%d) has no remote slot for it. Declare an index in that range, or widen spec.ridLayout.maxSites on the SlapdMesh.",
					peers[i].Name, idx, layout.MaxSites-1, layout.PodsPerSite, layout.MaxSites),
			}
		}
	}

	// 4. One slot, one peer — named symmetrically, because neither of them may
	//    win the race to write a stanza over the other's.
	byIndex := make(map[int32]string, len(peers))
	for i := range peers {
		idx := *peers[i].SiteIndex
		if first, taken := byIndex[idx]; taken {
			return ridSegmentVerdict{
				Reason: reasonPeerSiteIndexCollision,
				Message: fmt.Sprintf(
					"external peers %q and %q both declare siteIndex %d, so both cross-site links would carry one RID — slapd loads both stanzas and shares one connection id between them. Give each peer site its own index; neither peer's stanzas are written until one of them moves.",
					first, peers[i].Name, idx),
			}
		}
		byIndex[idx] = peers[i].Name
	}

	return ridSegmentVerdict{OK: true, Reason: reasonRIDSegmentOK}
}

// Reasons a cluster's own shape — not its databases, not its peer list —
// cannot be given coordinates.
const (
	// reasonRIDPodsPerSiteExceeded — spec.replicas is larger than the layout's
	// local block, so the fleet's highest ordinals have no slot. ADR-031
	// decision 5: exceeding an axis is a refusal naming which one and what the
	// ceiling is, never a wrap and never a silent collision.
	reasonRIDPodsPerSiteExceeded = "PodsPerSiteExceeded"
	// reasonConsumerOnlyReadReplicas — a consumer-only cluster has no
	// in-cluster mesh (ADR-010), so its read-only pods have no provider to
	// consume from: no local block, and the read-only builder writes no
	// external stanzas at all. The combination is already refused by a CEL rule
	// on SlapdClusterSpec; it is refused again here because an operator that
	// trusts admission writes an EMPTY olcSyncRepl Replace to every read-only
	// pod when the CR got in another way (RID-PLAN hazard 2).
	reasonConsumerOnlyReadReplicas = "ConsumerOnlyReadReplicasUnsupported"
)

// judgeClusterFleet decides whether the cluster's own pod fleet fits the layout
// and can be addressed at all.
//
// Two refusals, both of which withhold exactly as a refused segment does
// (ADR-031 decision 6), and both of which are currently SILENT:
//
//  1. spec.replicas above the layout's podsPerSite. The withhold already
//     happens — inClusterStanzaRIDs refuses the out-of-range ordinal — but it
//     happens inside reconcileReplication, so SyncreplStanzasReady stays True, the
//     phase reads PartiallyApplied, and the only mention of podsPerSite is a
//     log line. Decision 5 requires a refusal naming the axis and its ceiling.
//     Read replicas are NOT counted: an RO pod is never a provider, so it
//     occupies no slot in the local block (ADR-031 Consequences).
//  2. consumer-only with readReplicas > 0. Its read-only pods would be handed
//     an empty RID set, every lookup would miss, and the pod's whole stanza set
//     would be Replace()d with nothing.
//
// Pure. Only OK, Reason and Message are meaningful; Segment is not a
// cluster-level quantity and stays zero.
func judgeClusterFleet(sc *ldapv1alpha1.SlapdCluster, layout ridcoord.Layout) ridSegmentVerdict {
	// 1. The fleet against the local block. Zero reads as one, the same way
	//    every other reader of spec.replicas treats it.
	replicas := sc.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}
	if replicas > layout.PodsPerSite {
		return ridSegmentVerdict{
			Reason: reasonRIDPodsPerSiteExceeded,
			Message: fmt.Sprintf(
				"spec.replicas is %d and the RID layout in force admits podsPerSite=%d, so pod ordinals %d..%d have no slot in the local block. Every syncrepl stanza of every database is withheld until the fleet fits: scale back to at most %d read-write pods, or widen spec.ridLayout.podsPerSite on the SlapdMesh (which renumbers the whole mesh once). Read replicas are not counted — an RO pod is never a provider.",
				replicas, layout.PodsPerSite, layout.PodsPerSite, replicas-1, layout.PodsPerSite),
		}
	}

	// 2. Read-only pods on a consumer-only cluster. They would be handed an
	//    empty local block — no provider to consume from, and the read-only
	//    builder writes no external stanzas — and an empty stanza set is
	//    installed with one atomic Replace, which deletes whatever the pod had.
	//    Admission already refuses the combination; so does this, because a CR
	//    can reach a controller without passing admission and the cost of
	//    trusting it here is that deletion.
	if sc.IsConsumerOnly() && sc.Spec.ReadReplicas > 0 {
		return ridSegmentVerdict{
			Reason: reasonConsumerOnlyReadReplicas,
			Message: fmt.Sprintf(
				"spec.readReplicas is %d on a consumer-only cluster: a consumer-only cluster runs no in-cluster mesh (ADR-010), so its read-only pods have no provider to consume from and no stanza this operator could write for them. Set readReplicas to 0, or give the cluster a replication mode that makes its read-write pods providers.",
				sc.Spec.ReadReplicas),
		}
	}

	return ridSegmentVerdict{OK: true, Reason: reasonRIDSegmentOK}
}

// reasonRIDLayoutUnresolved — the layout could not be READ, so it is unknown
// rather than wrong.
//
// Distinct from reasonRIDLayoutInvalid on purpose, and for the reason
// reasonRIDSegmentUnverified exists in the opposite direction: a reason is a
// diagnosis, and alerting and slctl key on it. RIDLayoutInvalid says "spec.
// ridLayout on the SlapdMesh admits no segment — go fix that field", which when
// the mesh simply could not be read sends a human to a field that may be
// perfectly correct, while the API server, the operator's RBAC or its cache is
// what actually failed.
const reasonRIDLayoutUnresolved = "RIDLayoutUnresolved"

// judgeRIDLayout turns ridLayoutFor's two return values into the verdict that
// must precede every segment judgement.
//
// It exists because the two failure modes are indistinguishable downstream: a
// failed read yields a zero Layout, and a zero Layout is also what a mesh
// declaring an unusable ridLayout produces, so a caller that passes the layout
// on and drops the error gets the WRONG diagnosis published — which is what
// shipped, and what this closes.
//
// Returns nil when the layout is usable and the judgement may proceed:
//
//   - err != nil → a refusal with reasonRIDLayoutUnresolved, naming the mesh
//     and carrying the underlying error.
//   - err == nil but the layout does not validate → a refusal with
//     reasonRIDLayoutInvalid, which is the declared-but-unusable case
//     judgeRIDSegment already words.
//   - otherwise → nil.
//
// Wiring, because a pure function cannot assert its own call site: it belongs
// at the top of reconcileRIDSegment, which takes ridLayoutFor's error alongside
// the layout, and its refusal is published and returned exactly like any other.
func judgeRIDLayout(sc *ldapv1alpha1.SlapdCluster, layout ridcoord.Layout, err error) *ridSegmentVerdict {
	if err != nil {
		return &ridSegmentVerdict{
			Reason: reasonRIDLayoutUnresolved,
			Message: fmt.Sprintf(
				"the RID layout could not be read from SlapdMesh %q, so it is unknown rather than wrong: %v. Syncrepl stanzas are withheld until the read succeeds — defaulting a layout the mesh may have narrowed would renumber every stanza at this site and no other. Nothing here says a CR is wrong: look at the API server, the operator's RBAC and its cache.",
				sc.Spec.MeshRef, err),
		}
	}
	if verr := layout.Validate(); verr != nil {
		return &ridSegmentVerdict{
			Reason: reasonRIDLayoutInvalid,
			Message: fmt.Sprintf(
				"the RID layout in force admits no segment: %v. Fix spec.ridLayout on the SlapdMesh (podsPerSite and maxSites must each be at least 1, and their sum at most %d).",
				verr, ridcoord.MaxRID),
		}
	}
	return nil
}

// stanzaRIDFor is the one lookup into a resolved RID map, and it REFUSES a
// miss.
//
// Both stanza builders currently read the map with `coord, known := rids[i]`
// and `continue` when the ordinal is absent, on the reasoning that the caller
// already decided which providers to address. That reasoning is a convention
// held at a distance, and it has already failed once: a consumer-only cluster
// hands the read-only builder an empty map, every lookup misses, every stanza
// is skipped, and the pod's whole set is Replace()d with nothing — a deletion
// of live replication with no error anywhere (RID-PLAN hazard 2).
//
// It is the same class of bug selfOrdinal's pointer type exists to make
// unexpressible, one level down: a caller that must remember something.
// Whichever provider a builder intends to address, it asks for that provider's
// RID here, and a miss ends the build.
//
// The error names the ordinal and the map it was not in, because the question a
// human then has is "who was supposed to put it there".
func stanzaRIDFor(rids map[int32]int32, ordinal int32) (int32, error) {
	rid, known := rids[ordinal]
	if !known {
		return 0, fmt.Errorf(
			"no RID for provider ordinal %d: the resolved set holds %d ordinal(s) %v. "+
				"A stanza set short of one provider is a deletion of that link under olcSyncRepl's atomic Replace, so the build stops here rather than emitting the rest",
			ordinal, len(rids), sortedOrdinals(rids))
	}
	return rid, nil
}

// sortedOrdinals renders a RID map's keys in a stable order, so a refusal
// message does not depend on Go's map iteration.
func sortedOrdinals(rids map[int32]int32) []int32 {
	out := make([]int32, 0, len(rids))
	for ordinal := range rids {
		out = append(out, ordinal)
	}
	slices.Sort(out)
	return out
}
