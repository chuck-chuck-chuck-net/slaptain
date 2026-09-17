package cmd

import (
	"fmt"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// Where the diagnostics get a mesh-driven cluster's external peer set
// (MESH-PLAN Phase 6a).
//
// The problem. ADR-028 §4 derives a cluster's cross-site wiring from a
// SlapdMesh plus the operator's own site identity, and MESH-PLAN Phase 4
// applies that derivation IN MEMORY only — never written back to the spec,
// because a materialised spec would differ from site to site and destroy the
// byte-identical property the whole mesh design rests on. The consequence is
// that `kubectl get slapdcluster -o yaml` on a mesh member shows NO external
// peers while cn=config carries the stanzas, and slctl, which reads the same
// object, showed the same nothing.
//
// The design call: slctl does not re-derive. It reads what the operator
// published.
//
// Re-deriving would need three inputs — the cluster, the mesh, and which site
// this is — and slctl has only the first two. The third lives in the operator
// Deployment's SITE_NAME env, so a re-deriving slctl would have to find that
// Deployment (whose namespace is a convention, not a fact), read it, and hope
// the running operator pod matches the Deployment spec it just read. That
// makes slctl a SECOND authority on the derivation, free to disagree with the
// operator it is being used to debug — the failure mode this codebase already
// refuses elsewhere (sourceConvergedCondition consumes the cluster's verdict
// rather than re-deriving one; ADR-026 R2 says report, do not act on state you
// do not own).
//
// It is also not sufficient. The `external-syncrepl` check matches stanzas
// against each peer's DISCOVERED addresses, and those come from the operator's
// live queries to remote API servers. No amount of local derivation produces
// them. status.externalPeerStatuses is the only place they exist.
//
// And status is a complete answer, because the operator builds
// status.externalPeerStatuses by looping the peer list AFTER applying the mesh
// derivation (slapdcluster_controller.go step 6). The resolved peer set is
// therefore already published, by name, with health — no re-derivation, no
// SITE_NAME, no extra RBAC, and impossible to disagree with the operator by
// construction.
//
// What this costs, stated plainly: the peer set shown for a mesh cluster is as
// fresh as the operator's last successful status write, and it carries only
// what status carries. Both are recorded in the Note.
//
// The one rule that must not be broken: an empty peer list must never mean "I
// could not tell". That silence is indistinguishable from a healthy standalone
// cluster, and it is the exact defect this file exists to remove. Every path
// that cannot produce a trustworthy peer set returns a non-empty Note, and the
// callers print it.

// condMeshResolved is the SlapdCluster condition carrying the operator's
// verdict on spec.meshRef. Mirrored from internal/controller, which slctl
// cannot import a value from (the constant is unexported). Drift is contained:
// a rename there makes this read as "operator published no verdict", which is
// loud and conservative rather than silently wrong.
const condMeshResolved = "MeshResolved"

// peerSourceKind says where a displayed peer set came from, which is the piece
// of context a reader needs before trusting it.
type peerSourceKind string

const (
	// peerSourceSpec — the cluster hand-writes its peers. This is every
	// pre-mesh cluster, and its output is byte-identical to what it has always
	// been.
	peerSourceSpec peerSourceKind = "spec"
	// peerSourceMesh — the peers were derived by the operator from a SlapdMesh
	// and recovered here from the status it published.
	peerSourceMesh peerSourceKind = "mesh"
	// peerSourceUnknown — the cluster references a mesh but the operator has
	// not published a usable verdict, so the peer set shown (if any) is not
	// authoritative.
	peerSourceUnknown peerSourceKind = "unknown"
)

// peerResolution is the answer: the peers to display, where they came from,
// and — whenever that provenance needs explaining — a Note the caller prints.
type peerResolution struct {
	Source  peerSourceKind
	MeshRef string
	Peers   []ldapv1alpha1.ExternalPeer
	// Note is empty ONLY for peerSourceSpec. Anything else has something the
	// reader must know before believing the list.
	Note string
}

// OK reports whether the peer set can be trusted as the cluster's real one.
func (r peerResolution) OK() bool { return r.Source != peerSourceUnknown }

// resolveExternalPeersForDisplay is the Phase 6a decision, pure.
//
// Order of judgement: no meshRef short-circuits first and derives nothing, so
// there is no path by which mesh code can perturb a cluster that never asked
// for a mesh. Everything below that line runs only for spec.meshRef != "".
func resolveExternalPeersForDisplay(sc *ldapv1alpha1.SlapdCluster) peerResolution {
	if sc.Spec.MeshRef == "" {
		return peerResolution{
			Source: peerSourceSpec,
			Peers:  sc.Spec.Replication.ExternalPeers,
		}
	}

	ref := sc.Spec.MeshRef
	recovered := peersFromStatus(sc.Status.ExternalPeerStatuses)
	cond := apimeta.FindStatusCondition(sc.Status.Conditions, condMeshResolved)

	// No verdict at all: an operator that has not reconciled this cluster yet,
	// one that is wedged, or one too old to know about meshes. Whatever the
	// cause, nothing here is evidence about peers.
	if cond == nil {
		return peerResolution{
			Source:  peerSourceUnknown,
			MeshRef: ref,
			Peers:   recovered,
			Note: fmt.Sprintf(
				"spec.meshRef=%q, but the operator has published no %s condition, so the "+
					"derived peer set cannot be read from this object — %s. "+
					"Check that the operator is running and has reconciled this cluster.",
				ref, condMeshResolved, describeRecovered(recovered)),
		}
	}

	if cond.Status != metav1.ConditionTrue {
		tail := "no peers could be recovered from status either, so the peer set here is " +
			"UNKNOWN, not empty"
		if len(recovered) > 0 {
			tail = fmt.Sprintf("the %d peer(s) below are the LAST KNOWN set from "+
				"status.externalPeerStatuses, not necessarily the current one", len(recovered))
		}
		return peerResolution{
			Source:  peerSourceUnknown,
			MeshRef: ref,
			Peers:   recovered,
			Note: fmt.Sprintf(
				"spec.meshRef=%q: the operator reports %s=%s (%s), so it is NOT currently "+
					"deriving this cluster's cross-site wiring; %s. Operator message: %s",
				ref, condMeshResolved, cond.Status, cond.Reason, tail, cond.Message),
		}
	}

	// Derived, and the operator published the result. This is the good path.
	note := fmt.Sprintf(
		"external peers are DERIVED from SlapdMesh %q (ADR-028 §4) and do not appear in "+
			"spec.replication.externalPeers; the %d peer(s) below are read back from "+
			"status.externalPeerStatuses, as of the operator's last status write.",
		ref, len(recovered))
	if len(recovered) == 0 {
		note = fmt.Sprintf(
			"external peers are DERIVED from SlapdMesh %q (ADR-028 §4); the operator derived "+
				"0 peers, so this cluster spans a single site of the mesh. "+
				"This is an answer, not missing data.",
			ref)
	}
	return peerResolution{
		Source:  peerSourceMesh,
		MeshRef: ref,
		Peers:   recovered,
		Note:    note,
	}
}

// describeRecovered renders the "and here is what we do have" clause, so an
// empty list is always accompanied by words saying it is empty for a reason.
func describeRecovered(peers []ldapv1alpha1.ExternalPeer) string {
	if len(peers) == 0 {
		return "no peers could be recovered from status either: the peer set shown is " +
			"UNKNOWN, not empty"
	}
	return fmt.Sprintf("%d peer(s) were recovered from status.externalPeerStatuses", len(peers))
}

// peersFromStatus reconstructs the peer list from what the operator published.
//
// Only Name and shape are reconstructed. Everything the display and the checks
// go on to need — discovered addresses, connectivity, replication state, lag —
// they already read out of status.externalPeerStatuses by name, so it would be
// duplication to copy it here.
//
// The shape is discovery mode, inferred rather than assumed: the mesh
// derivation (externalPeersForSite) produces discovery peers and nothing else —
// it sets neither uri nor podAddresses, and leaves replicasPerPeer nil so the
// CRD default applies. Reconstructing anything else would relabel a cross-site
// stanza as a single-URI one in every check that branches on the shape.
func peersFromStatus(statuses []ldapv1alpha1.ExternalPeerStatus) []ldapv1alpha1.ExternalPeer {
	if len(statuses) == 0 {
		return nil
	}
	peers := make([]ldapv1alpha1.ExternalPeer, 0, len(statuses))
	for _, s := range statuses {
		peers = append(peers, ldapv1alpha1.ExternalPeer{
			Name:      s.Name,
			Discovery: &ldapv1alpha1.ExternalPeerDiscovery{},
		})
	}
	return peers
}

// applyResolvedPeers writes the resolved peer set onto the in-memory
// SlapdCluster, mirroring the operator's own applyMeshWiring: every existing
// reader of sc.Spec.Replication.ExternalPeers then sees the right list through
// the field it already reads, with no second read path to keep in sync.
//
// It is an in-memory mutation in a read-only CLI and never a write to the API.
//
// A cluster without meshRef is left strictly untouched — not re-assigned to an
// equal value, not normalised. That is the compatibility invariant, and it is
// structural here rather than argued.
func applyResolvedPeers(sc *ldapv1alpha1.SlapdCluster) peerResolution {
	res := resolveExternalPeersForDisplay(sc)
	if res.Source == peerSourceSpec {
		return res
	}
	sc.Spec.Replication.ExternalPeers = res.Peers
	return res
}
