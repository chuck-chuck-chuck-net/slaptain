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
)

// SlapdImageConfig defines the image repository, tag, and pull policy for one image.
type SlapdImageConfig struct {
	// repository is the image repository (e.g. "ghcr.io/chuck-chuck-chuck-net/slaptain/slapd").
	// +required
	Repository string `json:"repository"`
	// tag is the image tag.
	// +kubebuilder:default="latest"
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
	// secretName is the name of the TLS Secret containing tls.crt, tls.key, and ca.crt.
	SecretName string `json:"secretName,omitempty"`
}

// CnConfigCredentials references the Secret containing the cn=config admin password.
type CnConfigCredentials struct {
	// secretName references an existing Secret containing a "root-password" key
	// (plaintext). When empty, the operator auto-generates a Secret named
	// "<name>-config-password".
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
	// forceRebootstrap instructs the init container to re-run bootstrap even if
	// data already exists. Handle with care — this will overwrite existing data.
	// +kubebuilder:default=false
	ForceRebootstrap bool `json:"forceRebootstrap,omitempty"`
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
type SlapdPersistenceConfig struct {
	// enabled controls whether PVCs are created. When false, emptyDir is used.
	// +kubebuilder:default=true
	Enabled bool `json:"enabled,omitempty"`
	// config is the PVC for the slapd configuration directory (/ldap-config).
	// +optional
	Config SlapdPVCConfig `json:"config,omitempty"`
	// data is the PVC for the slapd data directory (/ldap-data).
	// +optional
	Data SlapdPVCConfig `json:"data,omitempty"`
	// accesslog is the PVC for the delta-syncrepl access log (/ldap-accesslog).
	// Only provisioned when replication is enabled.
	// +optional
	Accesslog SlapdPVCConfig `json:"accesslog,omitempty"`
}

// SlapdServiceConfig configures the ClusterIP service exposed by the operator.
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
}

// SlapdClusterSpec defines the desired state of SlapdCluster.
type SlapdClusterSpec struct {
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
	// logLevel is the slapd -d debug level. 0 disables debug output.
	// +kubebuilder:default=0
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
	// imagePullSecrets is a list of references to secrets for pulling container images.
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
type SlapdClusterStatus struct {
	// phase summarises the current lifecycle state.
	// +optional
	Phase SlapdClusterPhase `json:"phase,omitempty"`
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
