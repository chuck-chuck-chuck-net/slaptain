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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SlapdClusterPhase represents the lifecycle phase of the cluster.
type SlapdClusterPhase string

const (
	PhaseBootstrapping SlapdClusterPhase = "Bootstrapping"
	PhaseRunning       SlapdClusterPhase = "Running"
	PhaseDegraded      SlapdClusterPhase = "Degraded"
	PhaseError         SlapdClusterPhase = "Error"
	// PhaseRestoring indicates the cluster is held down (StatefulSets at 0
	// replicas) for an offline bootstrapFrom restore. See ADR-014. The
	// SlapdDatabase controller gates on phase==Running and so pauses while this
	// is set.
	PhaseRestoring SlapdClusterPhase = "Restoring"
)

// SlapdClusterRestorePhase is the sub-state of an in-progress restore (ADR-014).
type SlapdClusterRestorePhase string

const (
	// RestorePreflight: validating the backup source(s) are fetchable and valid
	// BEFORE scaling anything down (destroy-last). Does NOT hold the cluster
	// down — it keeps serving while preflight runs (ADR-014 amendment).
	RestorePreflight SlapdClusterRestorePhase = "Preflight"
	// RestoreScalingDown: scaling the StatefulSet(s) to 0; waiting for pods to
	// terminate and release the PVCs.
	RestoreScalingDown SlapdClusterRestorePhase = "ScalingDown"
	// RestoreInProgress: per-database restore Jobs are running (download → wipe
	// → slapadd) against pod-0's PVCs.
	RestoreInProgress SlapdClusterRestorePhase = "Restoring"
	// RestoreScalingUp: all Jobs succeeded; scaling back up to the original
	// replica counts. Peers initial-sync from pod-0.
	RestoreScalingUp SlapdClusterRestorePhase = "ScalingUp"
	// RestoreFailed: a restore Job failed; the cluster is held at 0 replicas for
	// human inspection rather than scaling up a half-restored DIT.
	RestoreFailed SlapdClusterRestorePhase = "Failed"
)

// SlapdImageConfig defines the image repository, tag, and pull policy for one image.
type SlapdImageConfig struct {
	// repository is the image repository (e.g. "ghcr.io/chuck-chuck-chuck-net/slaptain/slapd").
	// +required
	Repository string `json:"repository"`
	// tag is the image tag. When empty, the operator substitutes its own image
	// tag at reconcile time — so "I want slapd/init at the version that shipped
	// with this operator" is the implicit default. Set this explicitly only when
	// you need to pin slapd/init to a different version than the operator.
	// +optional
	Tag string `json:"tag,omitempty"`
	// pullPolicy is the image pull policy.
	// +kubebuilder:default=IfNotPresent
	PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`
}

// SlapdImages defines the images used by the slapd cluster.
type SlapdImages struct {
	// slapd is the main slapd runtime image.
	// +required
	Slapd SlapdImageConfig `json:"slapd"`
	// init is the slapd-init bootstrap container image.
	// +required
	Init SlapdImageConfig `json:"init"`
}

// SlapdTLSConfig configures TLS for slapd.
type SlapdTLSConfig struct {
	// enabled controls whether TLS/LDAPS is active.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`
	// secretName is the name of the TLS Secret. Required keys: tls.crt
	// (server certificate, full chain) and tls.key (private key). Optional
	// key: ca.crt — a separate CA bundle for verifying peer/client certs.
	//
	// Public-CA certs (Let's Encrypt, ZeroSSL, cert-manager with a public
	// Issuer) embed the chain in tls.crt and need no separate ca.crt; the
	// init container skips the TLSCACertificateFile directive and slapd
	// falls back to OpenSSL's system trust store, which is the right answer
	// for verifying peers signed by a public CA.
	//
	// Provide ca.crt when running against a private/self-signed PKI, when
	// requiring client certificate authentication, or when cross-site
	// syncrepl peers present certs from an internal CA.
	SecretName string `json:"secretName,omitempty"`
}

// CnConfigCredentials references the Secret containing the cn=config admin password.
type CnConfigCredentials struct {
	// secretName references an existing Secret containing a "root-password" key
	// (plaintext password for cn=admin,cn=config). When empty, the operator
	// auto-generates a Secret named "<name>-config-password".
	//
	// The value MUST be plaintext, not {SSHA}/{ARGON2}/{CRYPT} — the operator
	// binds to cn=config using this password to manage schemas, ACLs, replication
	// stanzas, and topology. See docs/ONBOARDING.md §Secret and Credential Model.
	// +optional
	SecretName string `json:"secretName,omitempty"`
}

// SlapdLDAPConfig holds LDAP-specific configuration.
type SlapdLDAPConfig struct {
	// cnConfigCredentials references the Secret containing the cn=config admin
	// password. Used by the operator to manage per-pod cn=config (ACLs, schemas,
	// replication, database creation). See ADR-002.
	// +optional
	CnConfigCredentials CnConfigCredentials `json:"cnConfigCredentials,omitempty"`
	// tls configures TLS/LDAPS.
	// +optional
	TLS SlapdTLSConfig `json:"tls,omitempty"`
}

// SlapdPVCConfig holds sizing and storage class settings for a single PVC.
type SlapdPVCConfig struct {
	// size is the requested storage size.
	// +kubebuilder:default="1Gi"
	Size string `json:"size,omitempty"`
	// storageClass is the storage class name. Defaults to the cluster default if empty.
	// +optional
	StorageClass string `json:"storageClass,omitempty"`
	// accessMode is the PVC access mode.
	// +kubebuilder:default=ReadWriteOnce
	AccessMode corev1.PersistentVolumeAccessMode `json:"accessMode,omitempty"`
}

// SlapdPersistenceConfig configures persistent storage for config and data volumes.
//
// Per ADR-013, persistent storage is mandatory; there is no "disabled" mode.
// The `enabled` field was removed in v1alpha1 — CRs that still carry it will
// be rejected at admission as an unknown field.
type SlapdPersistenceConfig struct {
	// config is the PVC for the slapd configuration directory (/config).
	// +optional
	Config SlapdPVCConfig `json:"config,omitempty"`
	// data is the PVC for the slapd data directory (/data).
	// +optional
	Data SlapdPVCConfig `json:"data,omitempty"`
	// accesslog is the PVC for the delta-syncrepl access log (/accesslog).
	// Only provisioned for read-write pods that need to produce replication
	// events: spec.replication.enabled=true AND spec.replicas>1. Read-only
	// replicas and single-pod clusters never get this PVC.
	// +optional
	Accesslog SlapdPVCConfig `json:"accesslog,omitempty"`
}

// SlapdServiceConfig configures the client-facing Service for the read-write
// pods (the second StatefulSet's read-only Service inherits type and ports
// only; load-balancer fields apply to the read-write Service only).
type SlapdServiceConfig struct {
	// type is the Kubernetes Service type.
	// +kubebuilder:default=ClusterIP
	Type corev1.ServiceType `json:"type,omitempty"`
	// ldapPort is the external service port for LDAP.
	// +kubebuilder:default=389
	LDAPPort int32 `json:"ldapPort,omitempty"`
	// ldapsPort is the external service port for LDAPS.
	// +kubebuilder:default=636
	LDAPSPort int32 `json:"ldapsPort,omitempty"`
	// annotations is set on the Service's metadata. Useful for LB-controller
	// hints (MetalLB pool, Cilium IP pool, AWS NLB attributes, etc).
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
	// loadBalancerIP requests a specific IP from the LB provider. Only honored
	// when type=LoadBalancer. Deprecated upstream in favor of provider-specific
	// annotations, but still supported by MetalLB, Cilium LB-IPAM, and others.
	// +optional
	LoadBalancerIP string `json:"loadBalancerIP,omitempty"`
	// loadBalancerSourceRanges restricts traffic to the LB to the given CIDRs.
	// Only honored when type=LoadBalancer.
	// +optional
	LoadBalancerSourceRanges []string `json:"loadBalancerSourceRanges,omitempty"`
	// externalTrafficPolicy controls how the Service routes external traffic.
	// "Local" preserves the client source IP and avoids an extra hop; "Cluster"
	// load-balances across all nodes. Only honored when type=LoadBalancer or
	// type=NodePort.
	// +kubebuilder:validation:Enum=Cluster;Local
	// +optional
	ExternalTrafficPolicy corev1.ServiceExternalTrafficPolicy `json:"externalTrafficPolicy,omitempty"`
}

// ExternalPeer defines a cross-cluster peer for multi-site replication.
// Exactly one of uri, podAddresses, or discovery must be set.
type ExternalPeer struct {
	// name is a human-readable identifier for this peer.
	// +required
	Name string `json:"name"`
	// uri is the LDAP URI of the remote peer, e.g. "ldaps://ldap.remote-site.example.com:636".
	// Mutually exclusive with podAddresses and discovery.
	// +optional
	URI string `json:"uri,omitempty"`
	// podAddresses lists the replication-network IPs of individual remote pods.
	// Each address becomes a separate syncrepl stanza with its own RID.
	// Mutually exclusive with uri and discovery.
	// +optional
	PodAddresses []string `json:"podAddresses,omitempty"`
	// discovery configures dynamic peer discovery via a remote cluster's Kubernetes API.
	// The operator reads the remote cluster's pod annotations to discover Multus IPs.
	// Requires spec.replication.network to be configured. See ADR-007 amendment.
	// Mutually exclusive with uri and podAddresses.
	// +optional
	Discovery *ExternalPeerDiscovery `json:"discovery,omitempty"`
	// replicasPerPeer controls cross-site syncrepl fan-out when this peer resolves
	// to multiple remote pods (podAddresses or discovery mode). Each local pod connects
	// to this many remote pods using diagonal-first assignment: local pod ordinal i,
	// connection k → remote pod (i + k) % len(addresses). Default 1 gives the 1:1
	// diagonal (one connection per local pod, evenly distributed across remote pods);
	// setting it equal to or greater than the number of addresses degrades to a full
	// N×M mesh. Values exceeding the address count are silently capped. Ignored in
	// uri mode (single endpoint).
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	ReplicasPerPeer *int32 `json:"replicasPerPeer,omitempty"`
	// port is the remote slapd port when using podAddresses or discovery.
	// Defaults to 1025 (LDAPS container port). Ignored when uri is set.
	// +kubebuilder:default=1025
	// +optional
	Port int32 `json:"port,omitempty"`
	// tlsSecretName is the name of the Secret containing the CA cert for verifying the peer.
	// +optional
	TLSSecretName string `json:"tlsSecretName,omitempty"`
	// bindDN is the DN used to authenticate to the remote peer.
	// +optional
	BindDN string `json:"bindDN,omitempty"`
	// bindPasswordSecretName is the name of the Secret containing the bind password.
	// +optional
	BindPasswordSecretName string `json:"bindPasswordSecretName,omitempty"`
	// syncMode selects the syncrepl wire protocol for this peer.
	//
	//   delta — default. Delta-syncrepl using the accesslog DB. Requires the
	//     peer to run accesslog+syncprov overlays (a slaptain peer always does).
	//   plain — plain refreshAndPersist syncrepl, no accesslog. Use this to
	//     interoperate with a legacy OpenLDAP source that does not advertise an
	//     accesslog DB (e.g. prod VMs during a hot migration — see ADR-011).
	//
	// Both modes preserve operational attributes (entryUUID, entryCSN, etc.)
	// on the consumer side. The difference is reconnect/recovery cost: delta
	// resumes from the last CSN; plain re-evaluates the full DIT on reconnect.
	// +kubebuilder:validation:Enum=plain;delta
	// +kubebuilder:default=delta
	// +optional
	SyncMode string `json:"syncMode,omitempty"`
}

// ExternalPeerDiscovery configures dynamic peer discovery via a remote cluster's Kubernetes API.
// The operator queries the remote cluster (over the replication network) and extracts
// Multus IPs from pod network-status annotations. See ADR-007 amendment.
type ExternalPeerDiscovery struct {
	// kubeconfigSecret references a Secret containing a kubeconfig for the remote cluster.
	// The API server address in the kubeconfig should use the remote node's replication-network IP.
	// +required
	KubeconfigSecret KubeconfigSecretRef `json:"kubeconfigSecret"`
	// namespace is the namespace of the remote SlapdCluster. Defaults to the local
	// SlapdCluster's namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// clusterName is the name of the remote SlapdCluster CR. Defaults to the local
	// SlapdCluster's name.
	// +optional
	ClusterName string `json:"clusterName,omitempty"`
}

// KubeconfigSecretRef references a Secret containing a kubeconfig file.
type KubeconfigSecretRef struct {
	// name is the Secret name (must be in the same namespace as the SlapdCluster).
	// +required
	Name string `json:"name"`
	// key is the data key containing the kubeconfig YAML.
	// +kubebuilder:default="kubeconfig"
	// +optional
	Key string `json:"key,omitempty"`
}

// ReplicationNetworkConfig configures a dedicated replication network via Multus CNI.
// See ADR-007.
type ReplicationNetworkConfig struct {
	// multusNetwork is the NetworkAttachmentDefinition reference.
	// Supports cross-namespace format "namespace/name" (recommended) or plain "name"
	// (same namespace as SlapdCluster). The NAD must already exist.
	// +required
	MultusNetwork string `json:"multusNetwork"`
	// useForInCluster controls whether in-cluster syncrepl uses discovered Multus IPs
	// instead of headless DNS. Default false — cross-site always uses Multus when configured.
	// +kubebuilder:default=false
	// +optional
	UseForInCluster bool `json:"useForInCluster,omitempty"`
}

// SlapdReplicationConfig holds cluster-level replication configuration.
// Per-database replication settings (deltaSync, RID base, checkpoint, purge)
// are on SlapdDatabase. See ADR-003 and ADR-004.
type SlapdReplicationConfig struct {
	// enabled controls whether replication infrastructure is active.
	// When true, the init container sets up syncprov and accesslog overlays.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`
	// mode selects the cluster's replication role. See ADR-010.
	//
	//   peer (default) — every RW pod is a full multi-master peer. Accesslog
	//     and syncprov overlays are provisioned, in-cluster syncrepl mesh is
	//     established, and the cluster accepts writes from clients.
	//
	//   consumer-only — the cluster is read-only and consumes data from
	//     externalPeers without advertising itself as a write source. No
	//     accesslog DB, no syncprov/accesslog overlays, no in-cluster mesh,
	//     and the data DB has olcReadOnly=TRUE. Use this during a hot
	//     migration: slaptain pulls from a legacy source while clients still
	//     write to the source, then is promoted to "peer" in place at cutover
	//     without re-syncing data.
	//
	// consumer-only requires externalPeers and is incompatible with readReplicas>0
	// (the cluster is already read-only). Promotion/demotion is in-place — pod
	// identity and data on disk are preserved across mode transitions.
	// +kubebuilder:validation:Enum=peer;consumer-only
	// +kubebuilder:default=peer
	// +optional
	Mode string `json:"mode,omitempty"`
	// accesslogEnabled overrides the operator's automatic decision about whether
	// to bootstrap the accesslog DB + PVC + container mounts for delta-syncrepl.
	//
	// Tristate:
	//   nil   — derive (the default): on when replicas > 1 OR externalPeers are
	//           configured, off otherwise.
	//   true  — force on. Use when undeclared external consumers (replicas the
	//           operator doesn't manage, e.g. a third party's RO mirror)
	//           connect via delta-syncrepl and need an accesslog journal here.
	//   false — force off. Save the per-write LMDB amplification of accesslog
	//           writes. Setting this with replicas > 1 will break in-cluster
	//           delta-sync replication — use only when you really know.
	//
	// Honored only when enabled=true; otherwise no replication infrastructure
	// is provisioned at all.
	// +optional
	AccesslogEnabled *bool `json:"accesslogEnabled,omitempty"`
	// externalPeers lists cross-cluster peers for multi-site replication.
	// +optional
	ExternalPeers []ExternalPeer `json:"externalPeers,omitempty"`
	// network configures a dedicated replication network via Multus. When set,
	// the operator adds Multus annotations to slapd pods and discovers assigned IPs
	// from pod network-status annotations. See ADR-007.
	// +optional
	Network *ReplicationNetworkConfig `json:"network,omitempty"`
	// keepalive sets the TCP keepalive parameters for syncrepl connections.
	// Format: "idle:probes:interval" (seconds), e.g. "300:10:60".
	// +optional
	Keepalive string `json:"keepalive,omitempty"`
	// retry sets the syncrepl retry interval.
	// Format: "<interval> <count>" pairs, e.g. "10 +" (retry every 10s, indefinitely).
	// +kubebuilder:default="10 +"
	Retry string `json:"retry,omitempty"`
	// serverIDBase shifts slaptain's per-pod ServerIDs out of the 1..N range.
	// Each pod's ServerID is computed as serverIDBase + ordinal + 1 (so the
	// first pod with base=500 has ServerID 501, the second 502, etc.).
	//
	// **Required when this cluster participates in a multi-master mesh with
	// any other cluster** (other slaptain instances via externalPeers, or a
	// legacy prod cluster via plain-syncrepl interop). Slapd's CSN tracking
	// uses ServerID to attribute writes to their origin; two clusters using
	// overlapping ServerID ranges silently drop each other's writes because
	// contextCSN tracks "highest CSN per serverID" — colliding serverIDs
	// merge into a single bucket and updates appear already-seen. There is
	// no validation slapd performs at startup; the only symptom is "my
	// writes don't propagate," visible only via contextCSN inspection.
	//
	// Convention: assign each site/cluster its own decade or hundred. ADR-011
	// codifies a legacy two-site layout (site A 101-104, site B 201-204); slaptain's
	// e2e-multisite test uses site_idx*100. A standalone cluster (no
	// externalPeers, no plans to add any) can leave this at 0.
	//
	// Valid range is 0..4094 (slapd caps ServerID at 4095).
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4094
	// +optional
	ServerIDBase int32 `json:"serverIDBase,omitempty"`
}

// SlapdClusterSpec defines the desired state of SlapdCluster.
//
// These guardrails (ADR-010) fire only when the relevant fields are present, so
// a standalone cluster that omits the replication block — or sets only
// replication.enabled — validates cleanly. self.replication is optional
// (omitempty, no default); externalPeers has no default. Guard every access with
// has() so the API server never errors with a bare "no such key".
// +kubebuilder:validation:XValidation:rule="!has(self.replication) || self.replication.mode != 'consumer-only' || (has(self.replication.externalPeers) && size(self.replication.externalPeers) > 0)",message="replication.mode=consumer-only requires at least one replication.externalPeers entry"
// +kubebuilder:validation:XValidation:rule="!has(self.replication) || self.replication.mode != 'consumer-only' || !has(self.readReplicas) || self.readReplicas == 0",message="replication.mode=consumer-only is incompatible with readReplicas>0 (the whole cluster is already read-only)"
// +kubebuilder:validation:XValidation:rule="!has(self.replication) || self.replication.mode != 'consumer-only' || self.replication.enabled",message="replication.mode=consumer-only requires replication.enabled=true (otherwise the mode is silently ignored)"
// +kubebuilder:validation:XValidation:rule="!has(self.replication) || !has(self.replication.externalPeers) || size(self.replication.externalPeers) == 0 || self.replication.enabled",message="replication.externalPeers requires replication.enabled=true (otherwise the peers are silently ignored)"
type SlapdClusterSpec struct {
	// suspend pauses the operator's reconciliation of this resource. Existing
	// StatefulSets, Services, and Secrets are left in place; the operator stops
	// observing or mutating them. Use during manual interventions (e.g. running
	// slapadd against the data PVC, hand-editing cn=config) where operator
	// reconciliation would fight your changes. Resume by setting back to false.
	// +kubebuilder:default=false
	// +optional
	Suspend bool `json:"suspend,omitempty"`
	// images specifies the container images to use.
	// +required
	Images SlapdImages `json:"images"`
	// ldap contains LDAP-specific configuration (TLS, cn=config credentials).
	// +optional
	LDAP SlapdLDAPConfig `json:"ldap,omitempty"`
	// replicas is the number of slapd replicas. Set spec.replication.enabled=true for replicas>1.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	Replicas int32 `json:"replicas,omitempty"`
	// readReplicas is the number of read-only consumer replicas.
	// Requires replication.enabled=true and replicas>=1.
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	// +optional
	ReadReplicas int32 `json:"readReplicas,omitempty"`
	// logLevel is the slapd -d debug bitmask. Default 256 ("stats") logs the
	// startup banner, connection accept/close, bind, and operation results —
	// the bare minimum to know slapd is alive and to debug why a client got
	// rejected. Higher levels add detail (see slapd.conf(5) "loglevel"):
	//
	//   0   no debug output at all (slapd is healthy but kubectl logs is empty
	//       — useful only when log volume itself is the problem)
	//   1   trace function calls (very noisy)
	//   32  search filter processing
	//   64  configuration processing
	//   128 access control list processing
	//   256 stats — recommended default
	//   16384 sync replication (consumer side)
	//   32768 sync replication (provider side)
	//   -1  everything (firehose; only for one-off debugging)
	//
	// Bitmasks combine, e.g. 256+128=384 for stats+ACL.
	// +kubebuilder:default=256
	LogLevel int32 `json:"logLevel,omitempty"`
	// persistence configures persistent storage for config and data volumes.
	// +optional
	Persistence SlapdPersistenceConfig `json:"persistence,omitempty"`
	// service configures the ClusterIP service.
	// +optional
	Service SlapdServiceConfig `json:"service,omitempty"`
	// resources sets compute resource requests and limits for the slapd container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// securityContext overrides the pod-level security context.
	// When nil, defaults of runAsUser/runAsGroup/fsGroup=1024 are applied.
	// +optional
	SecurityContext *corev1.PodSecurityContext `json:"securityContext,omitempty"`
	// replication configures N-way multi-master delta-syncrepl replication (Phase 2+).
	// +optional
	Replication SlapdReplicationConfig `json:"replication,omitempty"`
	// imagePullSecrets references Secrets in the same namespace that the kubelet
	// uses to pull the slapd and slapd-init images. Each Secret must be of
	// type kubernetes.io/dockerconfigjson. Equivalent to setting imagePullSecrets
	// on the StatefulSet pod template directly.
	//
	// Alternative if you standardize pull secrets at the namespace level: skip
	// this field and patch the default ServiceAccount in the workload namespace
	// with its own imagePullSecrets; the kubelet inherits them automatically.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
}

// ExternalPeerReplicationState summarises the replication health of one external peer.
// +kubebuilder:validation:Enum=Synced;Lagging;Unreachable
type ExternalPeerReplicationState string

const (
	// ReplicationSynced means the peer's newest contextCSN is within the sync threshold.
	ReplicationSynced ExternalPeerReplicationState = "Synced"
	// ReplicationLagging means the peer's newest contextCSN is behind the local newest.
	ReplicationLagging ExternalPeerReplicationState = "Lagging"
	// ReplicationUnreachable means the operator could not query contextCSN on the peer.
	ReplicationUnreachable ExternalPeerReplicationState = "Unreachable"
)

// ExternalPeerStatus reports the observed replication state of one external peer.
type ExternalPeerStatus struct {
	// name matches ExternalPeer.Name.
	Name string `json:"name"`
	// connected indicates whether the operator can reach this peer.
	// Derived from replicationState: true when not Unreachable.
	Connected bool `json:"connected"`
	// lastError is the last connection or discovery error, if any.
	// +optional
	LastError string `json:"lastError,omitempty"`
	// discoveredAddresses lists Multus IPs discovered from the remote cluster's pods.
	// Only populated for peers using discovery mode. The SlapdDatabase controller
	// consumes these the same way as static podAddresses.
	// +optional
	DiscoveredAddresses []string `json:"discoveredAddresses,omitempty"`
	// replicationState reports the CSN convergence state of this peer.
	// Synced: remote CSN is within threshold of local CSN.
	// Lagging: remote CSN is behind local CSN by more than the threshold.
	// Unreachable: operator could not query contextCSN on any remote pod.
	// +optional
	ReplicationState ExternalPeerReplicationState `json:"replicationState,omitempty"`
	// lagSeconds reports the max CSN timestamp delta between local and remote
	// as a decimal string (e.g. "3.2"). Only set when replicationState is Lagging.
	// +optional
	LagSeconds string `json:"lagSeconds,omitempty"`
	// lastChecked is the time the CSN check last ran.
	// +optional
	LastChecked *metav1.Time `json:"lastChecked,omitempty"`
}

// SlapdClusterStatus defines the observed state of SlapdCluster.
// SlapdClusterRestoreStatus tracks an in-progress bootstrapFrom restore window.
type SlapdClusterRestoreStatus struct {
	// phase is the restore sub-state.
	// +optional
	Phase SlapdClusterRestorePhase `json:"phase,omitempty"`
	// id is a short token unique to this restore window, generated when the
	// restore starts. It is woven into the per-pod restore Job names
	// (<db>-restore-<id>-rw-<i>) and labels so each restore waits only for its
	// own Jobs — a prior restore's Jobs (different id) are never mistaken for
	// this one's. See ADR-014 amendment.
	// +optional
	ID string `json:"id,omitempty"`
	// originalReplicas is spec.replicas captured before scaling down, restored on completion.
	// +optional
	OriginalReplicas int32 `json:"originalReplicas,omitempty"`
	// originalReadReplicas is spec.readReplicas captured before scaling down.
	// +optional
	OriginalReadReplicas int32 `json:"originalReadReplicas,omitempty"`
	// databases lists the SlapdDatabase names being restored in this window.
	// +optional
	Databases []string `json:"databases,omitempty"`
	// requestRef is the name of the SlapdRestore that triggered this restore, or
	// empty for a bootstrapFrom-driven restore. When set, the restore source is
	// the SlapdRestore's spec.source (not the database's bootstrapFrom), and the
	// machine reports completion on the SlapdRestore rather than setting the
	// database's restoreApplied. See ADR-014 amendment.
	// +optional
	RequestRef string `json:"requestRef,omitempty"`
	// startedAt is when the restore window began.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// message is a human-readable status detail (e.g. the failure reason).
	// +optional
	Message string `json:"message,omitempty"`
}

type SlapdClusterStatus struct {
	// phase summarises the current lifecycle state.
	// +optional
	Phase SlapdClusterPhase `json:"phase,omitempty"`
	// replicationMode reflects the observed replication mode (ADR-010). Equals
	// spec.replication.mode in steady state; may lag spec briefly during an
	// in-place promotion or demotion (3d, not yet implemented).
	// +optional
	ReplicationMode string `json:"replicationMode,omitempty"`
	// readyReplicas is the number of pods reporting Ready.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	// replicas is the total number of pods (ready or not).
	// +optional
	Replicas int32 `json:"replicas,omitempty"`
	// readOnlyReadyReplicas is the number of read-only pods reporting Ready.
	// +optional
	ReadOnlyReadyReplicas int32 `json:"readOnlyReadyReplicas,omitempty"`
	// readOnlyReplicas is the total number of read-only pods (ready or not).
	// +optional
	ReadOnlyReplicas int32 `json:"readOnlyReplicas,omitempty"`
	// observedGeneration is the .metadata.generation the controller last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// restore tracks an in-progress bootstrapFrom restore (ADR-014). Nil when no
	// restore is active. While set (and not yet ScalingUp) the operator holds the
	// StatefulSet(s) at 0 replicas for the offline slapadd window.
	// +optional
	Restore *SlapdClusterRestoreStatus `json:"restore,omitempty"`
	// replicationNetworkIPs reports discovered Multus IPs per pod on the replication network.
	// Key: pod name, Value: IP address. Only populated when spec.replication.network is set.
	// +optional
	ReplicationNetworkIPs map[string]string `json:"replicationNetworkIPs,omitempty"`
	// externalPeerStatuses reports per-peer replication connectivity.
	// +optional
	ExternalPeerStatuses []ExternalPeerStatus `json:"externalPeerStatuses,omitempty"`
	// conditions holds standard Kubernetes condition entries.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sc
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.replicas`
// +kubebuilder:printcolumn:name="RO-Ready",type=integer,JSONPath=`.status.readOnlyReadyReplicas`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SlapdCluster is the Schema for the slapdclusters API.
type SlapdCluster struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of SlapdCluster.
	// +required
	Spec SlapdClusterSpec `json:"spec"`

	// status defines the observed state of SlapdCluster.
	// +optional
	Status SlapdClusterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SlapdClusterList contains a list of SlapdCluster.
type SlapdClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SlapdCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SlapdCluster{}, &SlapdClusterList{})
}

// NeedsAccesslog reports whether the cluster needs the accesslog DB +
// PVC/volume + container mounts for delta-syncrepl. Single source of truth
// consulted by both the SlapdCluster controller (infrastructure provisioning)
// and the SlapdDatabase controller (ACL prepend, syncrepl stanza gating).
//
// Always false when spec.replication.enabled is false — without replication
// infrastructure as a whole, accesslog alone is meaningless.
//
// Always false in consumer-only mode (ADR-010) — the cluster does not produce
// writes, so there is nothing for an accesslog to record. The explicit
// accesslogEnabled override is intentionally not respected in consumer-only
// (forcing accesslog on a read-only DB makes no sense).
//
// Otherwise: honor spec.replication.accesslogEnabled if explicitly set, else
// derive — on when there is a consumer for the change journal (replicas > 1
// or externalPeers configured), off otherwise.
func (sc *SlapdCluster) NeedsAccesslog() bool {
	if !sc.Spec.Replication.Enabled {
		return false
	}
	if sc.IsConsumerOnly() {
		return false
	}
	if sc.Spec.Replication.AccesslogEnabled != nil {
		return *sc.Spec.Replication.AccesslogEnabled
	}
	return sc.Spec.Replicas > 1 || len(sc.Spec.Replication.ExternalPeers) > 0
}

// IsConsumerOnly reports whether the cluster is configured for consumer-only
// replication (ADR-010). True when spec.replication.mode == "consumer-only"
// AND replication is enabled — the mode is meaningless without replication.
func (sc *SlapdCluster) IsConsumerOnly() bool {
	return sc.Spec.Replication.Enabled && sc.Spec.Replication.Mode == "consumer-only"
}

// NeedsAccesslogVolume reports whether the cluster's RW pods should have the
// /accesslog volume + mount provisioned. True for any peer-eligible RW pod
// (replication enabled, not read-only, with at least one replication
// participant). Crucially this is INDEPENDENT of mode: consumer-only clusters
// also provision the volume so an in-place promotion to peer mode (ADR-010 3e)
// can add the accesslog DB at runtime without requiring a rolling restart to
// attach a new PVC.
//
// Pairs with NeedsAccesslog(): "volume exists" vs "DB exists." The operator
// creates/removes the DB at runtime via ldapmodify; the volume stays.
func (sc *SlapdCluster) NeedsAccesslogVolume() bool {
	if !sc.Spec.Replication.Enabled {
		return false
	}
	if sc.Spec.Replication.AccesslogEnabled != nil {
		return *sc.Spec.Replication.AccesslogEnabled
	}
	return sc.Spec.Replicas > 1 || len(sc.Spec.Replication.ExternalPeers) > 0
}

// RestoreHoldsDown reports whether an active bootstrapFrom restore requires the
// StatefulSet(s) to be held at 0 replicas for the offline slapadd window
// (ADR-014). True during ScalingDown, Restoring, and Failed; false during
// ScalingUp (so the cluster scales back up) and when no restore is active.
func (sc *SlapdCluster) RestoreHoldsDown() bool {
	r := sc.Status.Restore
	return r != nil && (r.Phase == RestoreScalingDown ||
		r.Phase == RestoreInProgress ||
		r.Phase == RestoreFailed)
}
