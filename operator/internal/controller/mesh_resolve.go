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
	"context"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// meshRef resolution (ADR-028 §4, MESH-PLAN Phase 4): the first consumer of the
// Phase 3 derivations.
//
// The shape is a pure decision (deriveClusterWiring) plus two thin shells — one
// that fetches the SlapdMesh (resolveMeshWiring) and one that writes the result
// onto the in-memory SlapdCluster the rest of the reconcile reads
// (applyMeshWiring). The decision is pure because the values it produces are
// baked irreversibly into replicated data: a serverID ends up inside every CSN
// the pod writes, and a peer NAME ends up as a directory component of the CA
// mount path and therefore inside every external syncrepl stanza's tls_cacert.
//
// Two properties are load-bearing and are asserted in mesh_resolve_test.go:
//
//   - meshRef unset derives NOTHING. Not defaults, not an empty peer set —
//     nothing, so there is no path by which mesh code can perturb any cluster
//     that exists today.
//   - The derivation is applied IN MEMORY, never written back to the spec.
//     Writing it back would make each site's SlapdCluster differ from its
//     neighbours', destroying the byte-identical property that ADR-028 §3 rests
//     the whole parity-checking design on.

const (
	// reasonMeshResolved is the condition type carrying the verdict.
	condMeshResolved = "MeshResolved"

	reasonMeshDerived           = "Derived"
	reasonNoMeshRef             = "NoMeshRef"
	reasonMeshNotFound          = "MeshNotFound"
	reasonMeshUnreadable        = "MeshUnreadable"
	reasonSpecConflictsWithMesh = "SpecConflictsWithMesh"
	reasonSiteIdentityMissing   = "SiteIdentityMissing"
	reasonSiteNotInMesh         = "SiteNotInMesh"
	reasonSiteNotSelected       = "SiteNotSelected"
	reasonMeshSitesInvalid      = "MeshSitesInvalid"
	reasonServerIDCeiling       = "ServerIDCeilingExceeded"
)

// meshWiring is what a mesh contributes to one cluster at one site. A nil
// *meshWiring means "no derivation took place" (no meshRef); a nil Network
// inside a non-nil wiring means "the mesh declares no network, keep the
// cluster's own".
type meshWiring struct {
	ServerIDBase  int32
	ExternalPeers []ldapv1alpha1.ExternalPeer
	Network       *ldapv1alpha1.ReplicationNetworkConfig
}

// meshResolveError carries a machine-readable condition Reason alongside the
// human message, so every distinct failure the Phase 3 derivations can produce
// reaches `kubectl describe` as its own reason rather than as one lump.
type meshResolveError struct {
	Reason  string
	Message string
	cause   error
}

func (e *meshResolveError) Error() string { return e.Message }
func (e *meshResolveError) Unwrap() error { return e.cause }

// deriveClusterWiring is the Phase 4 decision, pure.
//
// mesh is the referenced SlapdMesh's spec (never nil when spec.MeshRef is set —
// the shell has already established that), spec is the cluster's own, and
// identity is this operator's site name from its installation config.
//
// Order of judgement is deliberate: conflicts first, then identity, then the
// derivation. A conflicting spec is a user error that is true regardless of
// which site reads it, so reporting it uniformly at every site is more useful
// than having it masked at the one site whose SITE_NAME happens to be missing.
func deriveClusterWiring(mesh *ldapv1alpha1.SlapdMeshSpec, spec ldapv1alpha1.SlapdClusterSpec, identity string) (*meshWiring, error) {
	if spec.MeshRef == "" {
		// The compatibility invariant. Nothing below this line may run for a
		// cluster that did not ask for a mesh.
		return nil, nil
	}
	if mesh == nil {
		return nil, &meshResolveError{
			Reason:  reasonMeshUnreadable,
			Message: fmt.Sprintf("SlapdMesh %q resolved to no spec", spec.MeshRef),
		}
	}

	if err := checkMeshSpecConflicts(mesh, spec); err != nil {
		return nil, err
	}

	base, err := serverIDBaseForSite(mesh.Sites, identity)
	if err != nil {
		return nil, meshDerivationError(spec.MeshRef, err)
	}
	peers, err := externalPeersForSite(mesh.Sites, spec.Sites, identity)
	if err != nil {
		return nil, meshDerivationError(spec.MeshRef, err)
	}

	return &meshWiring{
		ServerIDBase:  base,
		ExternalPeers: peers,
		Network:       mesh.Network.DeepCopy(),
	}, nil
}

// checkMeshSpecConflicts refuses a cluster that both references a mesh and
// hand-writes something the mesh derives.
//
// The brief for this is "reject, never silently prefer one". Silently
// preferring the spec hands this site a decade the mesh gave to someone else;
// silently preferring the mesh discards a value the user believed was in force.
// Both end in a serverID collision, which slapd does not validate and whose
// only symptom is that some writes never propagate.
//
// What counts as "explicitly set" — the rule, field by field, because the three
// fields do not distinguish unset from zero-valued the same way:
//
//   - externalPeers: a non-empty list. The field has no CRD default, so empty
//     and unset are the same state and a non-empty list is unambiguous intent.
//   - network: a non-nil pointer carrying a mode or a NAD reference. `network:
//     {}` renders as a non-nil struct carrying only the CRD default
//     useForInCluster=false, which says nothing, so it is not a conflict.
//     Conflicts only when the MESH also declares a network: an optional mesh
//     field that is absent supplies no value for the cluster's to be ambiguous
//     with, and forcing network to nil there would silently disable peer
//     discovery.
//   - serverIDBase: a non-zero value that DIFFERS from the derived one. This
//     field is a plain int32 with a CRD default of 0, so an explicit 0 is
//     indistinguishable on the wire from an unset field and is therefore read
//     as unset. That is safe in the only direction that matters: the mesh's
//     value is the collision-free one by construction, so deriving it over a
//     0 that may or may not have been typed can only move the cluster toward
//     correctness. A value that AGREES with the mesh is likewise not a
//     conflict — refusing it would make the obvious upgrade path (declare the
//     mesh, keep the old field for one release, then delete it) impossible.
//
// Making serverIDBase a *int32 would remove the zero ambiguity outright. It is
// deliberately NOT done here: it changes an existing API field's serialisation
// and ripples through every reader, which is a separate decision from adding
// meshRef.
func checkMeshSpecConflicts(mesh *ldapv1alpha1.SlapdMeshSpec, spec ldapv1alpha1.SlapdClusterSpec) error {
	var conflicts []string

	if len(spec.Replication.ExternalPeers) > 0 {
		conflicts = append(conflicts, "spec.replication.externalPeers")
	}
	if n := spec.Replication.Network; mesh.Network != nil && n != nil && (n.Mode != "" || n.MultusNetwork != "") {
		conflicts = append(conflicts, "spec.replication.network")
	}
	// The serverIDBase comparison needs the derived value, and the derivation
	// can itself fail; when it does, that failure is the more fundamental one
	// and is reported by the caller. So a base we cannot derive simply does not
	// participate in conflict detection.
	if spec.Replication.ServerIDBase != 0 {
		// Identity is not available here, and must not be: a conflict is a
		// property of the spec, not of which site is reading it. Compare
		// against every decade the mesh hands out — if the typed value is one
		// of them it is at best redundant and at worst this site's neighbour's,
		// and only the exactly-right site may keep it.
		derived, err := serverIDBaseForSiteOfValue(mesh.Sites, spec.Replication.ServerIDBase)
		if err != nil || !derived {
			conflicts = append(conflicts, "spec.replication.serverIDBase")
		}
	}

	if len(conflicts) == 0 {
		return nil
	}
	return &meshResolveError{
		Reason: reasonSpecConflictsWithMesh,
		Message: fmt.Sprintf(
			"spec.meshRef=%q derives %s, but %s %s also set explicitly; "+
				"remove the explicit value or drop meshRef — resolving the ambiguity silently "+
				"is how a serverID collision gets in, and slapd validates nothing",
			spec.MeshRef,
			strings.Join(conflicts, ", "),
			strings.Join(conflicts, " and "),
			plural(len(conflicts), "is", "are"),
		),
	}
}

// serverIDBaseForSiteOfValue reports whether value is a decade the mesh
// actually hands out. It is the "agreeing value is not a conflict" half of the
// rule above, and it is deliberately identity-free: a typed serverIDBase is
// either one of the mesh's decades or it is wrong, and which site is asking
// does not change that.
//
// The narrower check ("is it THIS site's decade?") was considered and rejected:
// it makes the same object valid at one site and invalid at its neighbour,
// which destroys the byte-identical property (ADR-028 §3) for the one field
// most likely to be left behind during an upgrade.
func serverIDBaseForSiteOfValue(sites []ldapv1alpha1.MeshSite, value int32) (bool, error) {
	for _, s := range sites {
		if s.ServerIDIndex == nil {
			return false, errMeshSites
		}
		if *s.ServerIDIndex*serverIDDecade == value {
			return true, nil
		}
	}
	return false, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// meshDerivationError maps the Phase 3 sentinels onto condition reasons. Each
// sentinel gets its own reason because each has a different remedy: fix the
// operator chart's siteName, fix the mesh, fix the cluster's selector, or
// re-plan the serverID space.
func meshDerivationError(meshRef string, err error) error {
	reason := reasonMeshSitesInvalid
	switch {
	case errors.Is(err, errSiteIdentity):
		reason = reasonSiteIdentityMissing
	case errors.Is(err, errSiteNotSelected):
		reason = reasonSiteNotSelected
	case errors.Is(err, errSiteUnknown):
		reason = reasonSiteNotInMesh
	case errors.Is(err, errServerIDCeiling):
		reason = reasonServerIDCeiling
	case errors.Is(err, errMeshSites):
		reason = reasonMeshSitesInvalid
	}
	return &meshResolveError{
		Reason:  reason,
		Message: fmt.Sprintf("cannot derive wiring from SlapdMesh %q: %v", meshRef, err),
		cause:   err,
	}
}

// applyMeshWiring writes a derived wiring onto the in-memory SlapdCluster, so
// that every downstream reader — the StatefulSet builder's LDAP_SERVER_ID_BASE,
// the per-peer CA volume mounts, the SlapdDatabase controller's per-pod
// olcServerID and its syncrepl stanzas — sees the derived values through the
// fields they already read. No new read path, and nothing to keep in sync.
//
// It is an in-memory mutation and never a write to the API. The SlapdCluster
// object at every site stays byte-identical, which is the property ADR-028 §3
// makes cross-site parity checkable with.
//
// A nil wiring (no meshRef) is a no-op — the compatibility invariant. A nil
// Network inside a non-nil wiring leaves the cluster's own network alone; see
// checkMeshSpecConflicts.
func applyMeshWiring(sc *ldapv1alpha1.SlapdCluster, w *meshWiring) {
	if w == nil {
		return
	}
	sc.Spec.Replication.ServerIDBase = w.ServerIDBase
	sc.Spec.Replication.ExternalPeers = w.ExternalPeers
	if w.Network != nil {
		sc.Spec.Replication.Network = w.Network
	}
}

// resolveMeshWiring is the I/O shell: fetch the referenced SlapdMesh and apply
// its derivation to sc in memory. It is called by every controller that reads
// the derived fields off a SlapdCluster, immediately after fetching it.
//
// Returns nil and touches nothing when spec.meshRef is unset. On any failure it
// returns a *meshResolveError and, critically, leaves sc UNMODIFIED: a cluster
// whose mesh cannot be read must stall on the values it has, never fall back to
// defaults and never half-derive. A missing mesh silently yielding an empty
// peer set would tear down every cross-site stanza on the next reconcile.
func resolveMeshWiring(ctx context.Context, c client.Client, sc *ldapv1alpha1.SlapdCluster, identity string) error {
	if sc.Spec.MeshRef == "" {
		return nil
	}

	mesh := &ldapv1alpha1.SlapdMesh{}
	if err := c.Get(ctx, client.ObjectKey{Name: sc.Spec.MeshRef, Namespace: sc.Namespace}, mesh); err != nil {
		if apierrors.IsNotFound(err) {
			return &meshResolveError{
				Reason:  reasonMeshNotFound,
				Message: fmt.Sprintf("SlapdMesh %q not found in namespace %q", sc.Spec.MeshRef, sc.Namespace),
				cause:   err,
			}
		}
		return &meshResolveError{
			Reason:  reasonMeshUnreadable,
			Message: fmt.Sprintf("cannot read SlapdMesh %q: %v", sc.Spec.MeshRef, err),
			cause:   err,
		}
	}

	w, err := deriveClusterWiring(&mesh.Spec, sc.Spec, identity)
	if err != nil {
		return err
	}
	applyMeshWiring(sc, w)
	return nil
}

// meshResolveCondition renders the verdict of resolveMeshWiring as a condition
// on the SlapdCluster, so a misconfigured mesh is visible in
// `kubectl describe slapdcluster` rather than only in the operator's log.
func meshResolveCondition(sc *ldapv1alpha1.SlapdCluster, err error) {
	status := metav1.ConditionTrue
	reason := reasonMeshDerived
	msg := fmt.Sprintf("cross-site wiring derived from SlapdMesh %q", sc.Spec.MeshRef)

	switch {
	case sc.Spec.MeshRef == "":
		status = metav1.ConditionFalse
		reason = reasonNoMeshRef
		msg = "no spec.meshRef; cross-site wiring is taken from this spec verbatim"
	case err != nil:
		status = metav1.ConditionFalse
		reason = reasonMeshUnreadable
		msg = err.Error()
		var re *meshResolveError
		if errors.As(err, &re) {
			reason = re.Reason
			msg = re.Message
		}
	}

	setCondition(&sc.Status.Conditions, metav1.Condition{
		Type:               condMeshResolved,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: sc.Generation,
	})
}
