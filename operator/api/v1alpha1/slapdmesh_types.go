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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MeshSite describes one site of a mesh: a Kubernetes cluster that carries
// slapd pods and exchanges syncrepl traffic with the other sites.
//
// A site entry is written from the point of view of every OTHER site — it says
// how to reach and trust THIS site — which is what keeps the SlapdMesh object
// byte-identical everywhere (ADR-028 §3). Nothing in here may differ per
// reader; the one per-site fact in the system, "which of these am I", lives in
// the operator's own installation config and never in a CR (ADR-028 §4, see
// ResolveSiteName).
//
// The site's position in spec.sites is deliberately NOT load-bearing. The
// serverID decade comes from the explicit serverIDIndex field below, so the
// list may be reordered, sorted or edited in the middle without touching any
// site's identity. See that field for why.
type MeshSite struct {
	// name identifies the site. It is matched verbatim — not case-folded, only
	// whitespace-trimmed — against the operator's own SITE_NAME, exactly as
	// SlapdDatabase.spec.seed.site is (ADR-028 §3 amendment). Two sites whose
	// names differ only in case are two sites.
	//
	// Names must be unique within the mesh: two sites claiming one identity
	// collide their serverID decades, which is the failure ADR-028 §4 calls out
	// as the first thing to verify.
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// serverIDIndex fixes this site's slot in the serverID space. The site's
	// serverID decade is serverIDIndex * 100, and ADR-017's
	// serverIDBase + ordinal + 1 then gives each pod its own: index 2, pod 3 →
	// 203. That reads straight out of a CSN, which is the point of the scheme.
	//
	// It is an EXPLICIT field rather than the site's position in spec.sites,
	// and that is the whole design of this field. olcServerID is baked into
	// every CSN a pod has ever written (timestamp#count#sid#mod), so renumbering
	// a live site splits its history across two sids and per-site CSN tracking
	// stops being comparable with itself (MESH-PLAN hazard 2). Deriving the
	// number from list position would mean that inserting a site in the middle,
	// or letting a formatter sort the list, silently renumbers every site after
	// it — and reordering a YAML list is something people do without thinking.
	// Binding the number to a field instead makes spec.sites freely reorderable
	// and the identity immovable.
	//
	// Required, deliberately: defaulting it to the position would reintroduce
	// exactly the fragility the field removes, and would do so invisibly on the
	// one edit that matters.
	//
	// Indices must be unique across the mesh — two sites on one index share one
	// decade, and slapd validates nothing: contextCSN keeps the highest CSN per
	// serverID, so colliding sites merge into one bucket and each other's
	// writes read as already-seen. Gaps are legal and expected: a decommissioned
	// site's index is retired, not reused, so 0/1/2 becoming 0/2 is the correct
	// way to remove a site.
	//
	// The ceiling is index 40: slapd caps olcServerID at 4095 and
	// SlapdCluster.spec.replication.serverIDBase at 4094, so index 40 (decade
	// 4000) is the last one a whole decade fits in.
	// +required
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=40
	ServerIDIndex *int32 `json:"serverIDIndex"`

	// endpoint is the site's Kubernetes API server URL, on the network the
	// other sites can reach it over (the replication network, not a public
	// ingress). Recorded for the bootstrap CLI and for `slctl mesh verify`,
	// which needs to reach peer API servers while replication is broken
	// (ADR-028 §6).
	//
	// The operator itself does not read this: it dials the address inside the
	// kubeconfig Secret below, which is authoritative because it is what the
	// client credentials were minted for. Keep the two consistent.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// kubeconfigSecret names the Secret, in the SlapdCluster's namespace at
	// every OTHER site, holding a kubeconfig for this site's API server. It is
	// what ADR-007's dynamic peer discovery binds with.
	//
	// Optional: when omitted the name defaults by convention to
	// "<site name>-kubeconfig" with the usual "kubeconfig" key. Set it when the
	// Secret is provisioned by something with its own naming opinion —
	// External Secrets, SOPS, a platform team's convention (ADR-028 §7 adopts
	// the ecosystem rather than growing our own). Use KubeconfigSecretFor to
	// resolve it; do not read this field directly.
	// +optional
	KubeconfigSecret *KubeconfigSecretRef `json:"kubeconfigSecret,omitempty"`

	// caSecretName names the Secret, in the SlapdCluster's namespace at every
	// OTHER site, holding this site's CA certificate under "ca.crt". Consumers
	// verify this site's slapd against it (ADR-007 TLS posture).
	//
	// Optional: defaults by convention to "<site name>-ca". Set it for the same
	// reason as kubeconfigSecret — cert-manager's trust-manager writes the
	// bundle where its Bundle resource says, not where we would like. Use
	// CASecretNameFor to resolve it.
	// +optional
	CASecretName string `json:"caSecretName,omitempty"`
}

// KubeconfigSecretFor resolves the Secret reference used to reach this site's
// Kubernetes API, applying the documented "<site>-kubeconfig" convention when
// the field is omitted. Pure; safe on a zero-valued site.
func (s MeshSite) KubeconfigSecretFor() KubeconfigSecretRef {
	ref := KubeconfigSecretRef{}
	if s.KubeconfigSecret != nil {
		ref = *s.KubeconfigSecret
	}
	if ref.Name == "" {
		ref.Name = s.Name + "-kubeconfig"
	}
	return ref
}

// CASecretNameFor resolves the Secret holding this site's CA certificate,
// applying the documented "<site>-ca" convention when the field is omitted.
func (s MeshSite) CASecretNameFor() string {
	if s.CASecretName != "" {
		return s.CASecretName
	}
	return s.Name + "-ca"
}

// MeshTrustConfig carries mesh-wide trust material that is not per-site.
//
// Per-site CA Secrets live on the site entries (MeshSite.caSecretName), because
// a CA belongs to a site and not to a cluster: site B's CA is site B's CA
// whichever LDAP cluster is talking to it (ADR-028 §1). What is left at mesh
// level is the issuer the per-cluster server certificates are minted from.
type MeshTrustConfig struct {
	// issuerRef names a cert-manager Issuer or ClusterIssuer from which each
	// site's slapd server certificate is issued.
	//
	// Declarative only at present: nothing in the operator consumes it, because
	// ADR-028 §7 puts certificate bootstrap in a CLI run once per site pair
	// rather than in the reconcile loop. It is carried here so the mesh remains
	// the single description of the fabric, and so `mesh verify` has something
	// to compare.
	// +optional
	IssuerRef *MeshIssuerRef `json:"issuerRef,omitempty"`
}

// MeshIssuerRef references a cert-manager issuer. Field names and defaults
// mirror cert-manager's own certificate.spec.issuerRef so the value can be
// copied across without translation.
type MeshIssuerRef struct {
	// name is the issuer's name.
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// kind is "Issuer" (namespaced) or "ClusterIssuer".
	// +kubebuilder:validation:Enum=Issuer;ClusterIssuer
	// +kubebuilder:default=Issuer
	// +optional
	Kind string `json:"kind,omitempty"`
	// group is the issuer's API group.
	// +kubebuilder:default="cert-manager.io"
	// +optional
	Group string `json:"group,omitempty"`
}

// SlapdMeshSpec describes a multi-site deployment: the sites, the fabric
// between them, and the trust that fabric runs on — and deliberately nothing
// else (ADR-028 §1).
//
// There are no databases and no schemas here. A mesh object may be referenced
// by several SlapdClusters, and "whose databases?" has no coherent answer in
// that case; SlapdDatabase and SlapdSchema keep their clusterRef and are
// transitively mesh-aware through it. The invariants they must hold across
// sites (identical name, suffix, ridBase, credentials) are checked by
// `mesh verify` (ADR-028 §6), not modelled here.
//
// Every field is mesh-scoped by construction: it has one value that is true at
// all sites simultaneously. Adding a field whose value differs per site breaks
// the byte-identical property the whole design rests on (ADR-028 §3) — and mesh
// spec changes must additionally be additive and tolerant, because adding a
// site means editing the mesh everywhere and the sites run different
// generations during that rollout.
type SlapdMeshSpec struct {
	// sites lists the mesh's sites. Order is presentation only: each site's
	// serverID decade comes from its own serverIDIndex field, so the list may
	// be reordered or edited in the middle freely. Order IS preserved in the
	// derived external peer set, purely so an unchanged mesh keeps producing an
	// unchanged cn=config.
	// +required
	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=name
	Sites []MeshSite `json:"sites"`

	// network selects how cross-site syncrepl peers are addressed:
	// "pod-routed" (ADR-016) or "multus" plus its NAD (ADR-007). This is the
	// same shape SlapdCluster.spec.replication.network has today, reused rather
	// than re-declared so the Phase 4 derivation is an assignment and cannot
	// drift from it.
	//
	// The transport is a property of the fabric between sites, which is why it
	// belongs to the mesh: two clusters on one mesh cannot sensibly disagree
	// about whether pod CIDRs are routed.
	// +optional
	Network *ReplicationNetworkConfig `json:"network,omitempty"`

	// trust carries mesh-wide trust material. Per-site CA Secrets live on the
	// site entries.
	// +optional
	Trust *MeshTrustConfig `json:"trust,omitempty"`
}

// SlapdMeshStatus is the observed state of the mesh.
//
// Empty of substance today: no controller reconciles SlapdMesh (ADR-028 §6's
// parity signal is Phase 6 work, and it is read-only permanently — a mesh has
// no single writer, so a repair write would be corrective action on unowned
// state, which ADR-026 R2 forbids). The subresource exists so that signal has
// somewhere to land without a CRD storage change later.
type SlapdMeshStatus struct {
	// observedGeneration is the .metadata.generation last processed.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// conditions holds standard Kubernetes condition entries.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sm
// +kubebuilder:printcolumn:name="Network",type=string,JSONPath=`.spec.network.mode`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SlapdMesh is the Schema for the slapdmeshes API — the top layer of ADR-028's
// four:
//
//	SlapdMesh ←meshRef— SlapdCluster ←clusterRef— SlapdDatabase / SlapdSchema
//
// It describes the sites of a multi-site deployment and the fabric between
// them. It is namespaced like everything else in this API group: uniform RBAC,
// and two teams can run independent meshes in one cluster.
//
// The object is meant to be applied UNCHANGED to every site — that is the
// property that makes cross-site parity checkable as existence plus a hash,
// instead of a field-by-field comparison somebody gets wrong (ADR-028 §3).
type SlapdMesh struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired mesh topology.
	// +required
	Spec SlapdMeshSpec `json:"spec"`

	// status defines the observed state of the mesh.
	// +optional
	Status SlapdMeshStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SlapdMeshList contains a list of SlapdMesh.
type SlapdMeshList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SlapdMesh `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SlapdMesh{}, &SlapdMeshList{})
}
