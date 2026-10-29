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
	// When empty, the operator derives it from its own image reference
	// (OPERATOR_IMAGE) by swapping the trailing path segment for "slapd"/"slapd-init",
	// so unpinned operand images live in the same registry/path as the operator.
	// Falls back to the canonical upstream location when OPERATOR_IMAGE is unset.
	// +optional
	Repository string `json:"repository,omitempty"`
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
//
// The whole block is optional: when omitted, the operator defaults both images
// to its own registry/path at its own tag (see SlapdImageConfig.repository and
// SlapdImageConfig.tag). This mirrors CloudNativePG-style operator-side image
// defaulting — the operand image tracks the operator release, and a fork/mirror
// only needs to set the operator's own image. Set fields here to override.
type SlapdImages struct {
	// slapd is the main slapd runtime image.
	// +optional
	Slapd SlapdImageConfig `json:"slapd,omitempty"`
	// init is the slapd-init bootstrap container image.
	// +optional
	Init SlapdImageConfig `json:"init,omitempty"`
	// exporter is the image the slapd exporter sidecar runs (spec.monitoring,
	// ADR-033 Surface 3). The exporter is its own image, slapd-exporter, with
	// its own version line (git tags exporter/vX.Y.Z, image tag X.Y.Z). When
	// the repository is empty it is derived from the operator's like slapd and
	// slapd-init (trailing path segment swapped for slapd-exporter); when the
	// tag is empty it is the exporter release compiled into the operator — NOT
	// the operator's own tag. An operator upgrade therefore leaves the sidecar,
	// and the pods, unchanged unless that compiled-in release moved. A tag
	// alone keeps the derived repository; repository and tag together are used
	// exactly. pullPolicy: once this object is present at all, the CRD default
	// stores IfNotPresent, and that is what the sidecar gets unless set
	// otherwise; only with the object absent is the pull policy left to the
	// API server's tag-based default (Always for :latest, IfNotPresent
	// otherwise).
	//
	// The operator passes the sidecar no command and no arguments: the image's
	// entrypoint and the exporter's flag defaults (ldapi socket in
	// /run/openldap, SASL EXTERNAL, :9330/metrics) are the whole contract, so
	// any slapd-exporter release can be pinned here. Pin it to run a different
	// exporter version; it is not pinned in lockstep with slapd and init. A
	// future operator that changes the sidecar's pod-side shape (mounts, port,
	// securityContext) still rolls a pinned cluster. Ignored while monitoring
	// is off.
	// +optional
	Exporter SlapdImageConfig `json:"exporter,omitempty"`
	// select picks WHICH data-plane build the slapd and slapd-init images are,
	// without naming a tag (ADR-040). It is the recommended way to choose, and
	// it drives BOTH images from one declaration: the matched pair ADR-021
	// requires ("pin both or neither") stops being a rule you can violate and
	// becomes a property of the API.
	//
	// It sits beside slapd and init rather than inside either, because it
	// governs both. An explicit tag on an individual image still wins and is
	// used verbatim — the selector picks a build, it does not take the pin away
	// from anyone who wants one.
	//
	// Unset means the defaults below: the current storage format, without our
	// patches. That cell resolves to the operator's own tag with no suffix.
	// +optional
	Select *SlapdImageSelector `json:"select,omitempty"`
}

// SlapdImageSelector names a data-plane build by what it GOVERNS rather than by
// a version number (ADR-040 amendment). Two axes, each chosen so that the thing
// it controls is the thing it is named after:
//
//   - storageFormat is the COMPATIBILITY axis — what can open your volumes. A
//     selector keyed on the format cannot silently strand a cluster the way a
//     version-keyed one can: "the current lmdb1.0 build" moving 2.7.1 → 2.7.2
//     is safe by construction, and moving across formats is impossible because
//     the format is the thing you named. So slaptain keeps shipping newer slapd
//     builds, security fixes included, and the only thing that can strand a
//     volume is the axis you pinned yourself.
//   - flavour is the TRUST axis — whether you run slaptain's own behaviour
//     patches on top of upstream OpenLDAP.
//
// The two are NOT as independent as the shape suggests, and the field
// documentation on flavour says where they interact.
type SlapdImageSelector struct {
	// storageFormat is the on-disk LMDB format the images can open.
	//
	// lmdb1.0 is OpenLDAP 2.7's format and the default. lmdb0.9 is 2.6's.
	// They are mutually unreadable: a volume written by one cannot be opened
	// by the other, which is why this axis exists and why changing it on a
	// populated cluster is a dump/reload, not an upgrade (docs/OPENLDAP-VERSIONS.md;
	// ADR-023 is the path that makes it cheaper).
	//
	// Whoever takes the default never migrates. lmdb0.9 is a deliberate choice
	// for interoperating with an existing 2.6 deployment (ADR-011 hot
	// migration) and is an ADR-039 tier-2 surface: functionally valid, honestly
	// reported, and not the line that gets a mesh cycle every release.
	// +kubebuilder:validation:Enum=lmdb1.0;lmdb0.9
	// +kubebuilder:default=lmdb1.0
	// +optional
	StorageFormat string `json:"storageFormat,omitempty"`
	// flavour is whether the build carries slaptain's own behaviour patches.
	//
	// pure is upstream OpenLDAP as our packaging builds it, with none of our
	// patches, and is the default: nobody has to run a patched slapd in
	// production to use slaptain. slaptain is the patched line; each patch has
	// a stated reason in images/openldap/debian/changelog, and the package
	// revision says which you are running (`slapd -VV` reports +pure<N> or
	// +slaptain<N>).
	//
	// WHERE THE TWO AXES INTERACT: spec.rotation's phase-staggering modes need
	// the accesslog-purge-phase patch, so they work only on flavour: slaptain.
	// A pure slapd refuses the three-value olcAccessLogPurge and the operator
	// reports AccesslogPurgeConverged=False/PurgePhaseUnsupported. Rotation's
	// WriteOnly mode needs no patch — it is a Service the operator repoints —
	// and works on pure. Measured, not assumed (ADR-040 amendment, cycle B).
	// +kubebuilder:validation:Enum=pure;slaptain
	// +kubebuilder:default=pure
	// +optional
	Flavour string `json:"flavour,omitempty"`
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
	// protocolMin is the minimum TLS protocol version slapd will negotiate
	// (olcTLSProtocolMin), in slapd's "<major>.<minor>" SSL/TLS version
	// spelling: 3.1 = TLS 1.0, 3.2 = TLS 1.1, 3.3 = TLS 1.2, 3.4 = TLS 1.3.
	//
	// Unset means the operator's default, "3.3" — a TLS 1.2 floor (ADR-024 R5).
	// Without it slapd's own default is 0.0, i.e. "whatever the runtime image's
	// OpenSSL happens to permit today", which is a policy that changes silently
	// with a base-image bump. Set "0.0" to ask for that behaviour explicitly.
	//
	// Converged per pod, and written whether or not TLS is enabled: it costs one
	// attribute and it means turning TLS on later cannot land on an unpinned
	// floor.
	// +kubebuilder:validation:Pattern=`^[0-9]+\.[0-9]+$`
	// +optional
	ProtocolMin *string `json:"protocolMin,omitempty"`
	// cipherSuite is the OpenSSL cipher specification slapd offers
	// (olcTLSCipherSuite), e.g. "HIGH:!aNULL:!MD5". Unset writes nothing and
	// leaves the OpenSSL default list in force.
	//
	// Deliberately NOT operator-defaulted: a cipher list is the one TLS
	// parameter where our opinion ages badly and OpenSSL's own default — which
	// tracks the distribution's crypto policy — is better maintained than a
	// string frozen in an operator release. The protocol floor above is the part
	// that is worth pinning. Converged per pod when set; clearing the field
	// removes the attribute.
	// +optional
	CipherSuite *string `json:"cipherSuite,omitempty"`
}

// SlapdTuningConfig is the server-global tuning family: attributes that live on
// cn=config itself rather than on a database (ADR-024 R6). All of them are
// converged per pod on every reconcile — verified live on OpenLDAP 2.7.1, each
// one takes an ldapmodify against a running slapd without incident.
//
// The two connection lifetimes the production-config review put at the head of
// this family — olcIdleTimeout and olcWriteTimeout — are deliberately ABSENT.
// They are a real gap (slapd's default for both is "never close", so a pod
// behind a stateful firewall accumulates dead connections until it runs out of
// descriptors) but they cannot be converged: an ldapmodify of either against a
// running slapd 2.7.1 HANGS the process outright. See ensureGlobalTunables for
// the captured evidence, and docs/BACKLOG.md for the bootstrap-time (ADR-024
// R2) path that remains open to them. Offering a field the operator cannot
// honour is what ADR-024 R4 forbids.
//
// The thread and buffer knobs the review also listed (olcThreads,
// olcListenerThreads, olcConcurrency, olcSockbufMaxIncoming,
// olcSockbufMaxIncomingAuth, olcConnMaxPending) are NOT here either:
// upstream's defaults are defensible, the reference production platform leaves
// them alone too, and a thread count we cannot measure is a knob we would be
// guessing at. They stay recorded in docs/BACKLOG.md as escape hatches to add
// when a measurement asks for one.
type SlapdTuningConfig struct {
	// toolThreads is how many threads slapd's offline tools use for indexing
	// (olcToolThreads). It is read by slapadd, which is how a SlapdRestore or a
	// bootstrapFrom restore loads its LDIF — with the cluster scaled to zero for
	// the whole window, so the load time is downtime. Unset means the operator's
	// default, 2: enough to overlap index building with entry parsing,
	// conservative enough not to thrash a pod whose CPU limit is small. slapd's
	// own default is 1.
	//
	// Written on cn=config and therefore converged per pod, even though only the
	// tools read it — the restore Job runs `slapadd -F /config/slapd.d` against
	// that same config directory, so this is where slapadd finds it.
	// +kubebuilder:validation:Minimum=1
	// +optional
	ToolThreads *int32 `json:"toolThreads,omitempty"`
	// noSync is the cluster-wide durability default every SlapdDatabase inherits
	// unless it sets spec.noSync of its own (ADR-024 R6): true disables the
	// per-write fsync (olcDbNoSync) on every database in the cluster, trading
	// durability on an unclean shutdown for write throughput, on the argument
	// that the replication mesh is the redundancy.
	//
	// Unset means false — fsync after every write. This is a posture, not a
	// tuning detail: turn it on for the whole cluster or not at all, and read
	// SlapdDatabase.spec.checkpoint before you do.
	// +optional
	NoSync *bool `json:"noSync,omitempty"`
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
	// passwordHash is the scheme slapd uses when it hashes a password on a
	// user's behalf — a userPassword written in clear by a client with the right
	// to do so, or one changed through the password-modify extended operation
	// (olcPasswordHash). It does NOT govern the root passwords the operator
	// generates: those are hashed by the operator itself before they ever reach
	// slapd.
	//
	// Unset means the operator's default, "{SSHA}" (ADR-024 R5). slapd's own
	// frontend default is also {SSHA}, so this pins rather than changes it — the
	// point is that the policy is stated and converged, instead of being
	// whatever the build happened to compile in. Converged per pod.
	//
	// It is written to olcDatabase={-1}frontend, NOT to the cn=config global
	// entry: slapd 2.7 deprecates the latter and warns that it "may refuse to
	// start" when the scheme comes from a loadable module — which is precisely
	// the {ARGON2} case below. The operator also retires a copy left in the
	// global entry by an older version.
	//
	// Stronger schemes ({ARGON2} in particular) need their module present in the
	// runtime image; slaptain's does not ship pw-argon2 today, so asking for one
	// here would produce a slapd that rejects every password write. That is why
	// this field is a small pinned default rather than a menu — see
	// docs/BACKLOG.md for the module question.
	// +optional
	PasswordHash *string `json:"passwordHash,omitempty"`
}

// SlapdPVCConfig holds sizing and storage class settings for a single PVC.
type SlapdPVCConfig struct {
	// size is the requested storage size. Unset means slaptain's default for
	// this volume: 1Gi for config and accesslog, 5Gi for data (see
	// internal/controller/persistence_defaults.go and docs/TUNING.md).
	//
	// Deliberately carries no +kubebuilder:default. A CRD default is written
	// into the stored object at admission and only when its parent object is
	// present, so it would fill `persistence: {}` while an omitted
	// `persistence:` fell through to the operator — two sources for one number,
	// and the CR would show a value nobody chose, frozen at creation time.
	// +optional
	Size string `json:"size,omitempty"`
	// storageClass is the storage class name. Defaults to the cluster default if empty.
	// +optional
	StorageClass string `json:"storageClass,omitempty"`
	// accessMode is the PVC access mode. Unset means ReadWriteOnce, resolved by
	// the operator for the same reason size is.
	// +optional
	AccessMode corev1.PersistentVolumeAccessMode `json:"accessMode,omitempty"`
}

// SlapdMdbBackendConfig configures the back-mdb BACKEND entry
// (olcBackend={0}mdb), which is initialised before any database exists and is
// therefore BOOTSTRAP-TIME configuration in the ADR-024 R2 sense: the init
// container writes it into the generated base config, and the operator never
// converges it at runtime.
//
// Change path — read this before setting anything here. These attributes are
// applied when a pod's /config volume is first bootstrapped and NEVER again:
// this script only runs when cn=config does not yet exist. Editing the field on
// a live SlapdCluster therefore changes nothing on existing pods, and any pod
// bootstrapped afterwards (a scale-out, a replaced PVC) picks up the NEW value
// while its peers keep the old one. To change it cluster-wide you recreate the
// config volumes: back up (SlapdBackup), delete the StatefulSet's config PVCs
// pod by pod letting each pod re-bootstrap, or rebuild the cluster and restore.
// See docs/adrs/adr-024-tunable-placement.md §R2.
type SlapdMdbBackendConfig struct {
	// idlExponent is the power of two bounding how many entry IDs one index
	// slot holds before back-mdb degrades that slot to a range (the `idlexp`
	// backend directive, olcBkMdbIdlExp). Valid range 16-30; slapd's default
	// is 16 (65536 IDs), which is also slaptain's — see the note below on why
	// this one does NOT get an opinionated operator default.
	//
	// Raising it a few steps is the standard large-directory adjustment: on a
	// directory where a common index slot exceeds the cap, every search using
	// that slot falls back to a range and reads far more candidates than it
	// needs. The cost of raising it is memory per index page.
	//
	// Deliberately NOT operator-defaulted away from slapd's value (an
	// exception to ADR-024 R5, whose corollary "defaults live in the operator
	// so measurement can move them" assumes a converged attribute): this one
	// governs on-disk index layout and never converges, so moving the default
	// later would silently split a cluster into pods bootstrapped before and
	// after the change, with nothing able to repair it. An opt-in field whose
	// value is recorded in the CR is the honest shape for a knob nobody can
	// re-apply.
	// +kubebuilder:validation:Minimum=16
	// +kubebuilder:validation:Maximum=30
	// +optional
	IDLExponent *int32 `json:"idlExponent,omitempty"`
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
	// siteIndex is this peer's coordinate in the RID matrix's remote block:
	// a syncrepl link to this peer takes RID 1 + segment*(P+S) + P + siteIndex
	// (ADR-031 decision 1). It names the LINK, not the remote pod answering it,
	// so reselecting the far end moves provider= and leaves the RID alone.
	//
	// DERIVED when spec.meshRef is set — it is the peer site's serverIDIndex,
	// which ADR-028 already requires to be declared and unique — and setting
	// externalPeers at all alongside meshRef is refused. REQUIRED on a
	// hand-written peer of a replicating cluster: nothing else can supply a
	// peer site's identity, and a positional fallback is exactly the inference
	// ADR-031 forbids.
	//
	// Must be unique within one peer list, and must be below the mesh's
	// maxSites (S). The bound here is the loosest the band can admit; the
	// operator refuses it again against the layout actually in force.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=998
	// +optional
	SiteIndex *int32 `json:"siteIndex,omitempty"`
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

// Replication network modes. See ADR-007 (multus) and ADR-016 (pod-routed).
const (
	// NetworkModeMultus addresses cross-cluster peers via a dedicated Multus
	// secondary network; peer IPs come from the net1 interface (ADR-007).
	NetworkModeMultus = "multus"
	// NetworkModePodRouted addresses cross-cluster peers via their primary pod IP,
	// on clusters whose pod network is natively routed across sites (ADR-016).
	NetworkModePodRouted = "pod-routed"
)

// ReplicationNetworkConfig configures how cross-cluster replication peers are
// addressed. See ADR-007 (Multus) and ADR-016 (pod-routed native pod IPs).
type ReplicationNetworkConfig struct {
	// mode selects how cross-cluster peer IPs are obtained:
	//   "pod-routed" — the primary pod network is natively routed across sites; peer
	//                  IPs are the pods' primary IPs (needs no multusNetwork/NAD, and
	//                  no Multus attachment on the operator). Requires routed pod CIDRs.
	//   "multus"     — a dedicated Multus secondary network; peer IPs come from the
	//                  net1 interface (requires multusNetwork).
	// When unset, defaults to pod-routed — unless multusNetwork is set, which infers
	// multus. See ADR-016.
	// +kubebuilder:validation:Enum=multus;pod-routed
	// +optional
	Mode string `json:"mode,omitempty"`
	// multusNetwork is the NetworkAttachmentDefinition reference. Required when
	// mode is "multus"; leave empty for "pod-routed".
	// Supports cross-namespace format "namespace/name" (recommended) or plain "name"
	// (same namespace as SlapdCluster). The NAD must already exist.
	// +optional
	MultusNetwork string `json:"multusNetwork,omitempty"`
	// useForInCluster controls whether in-cluster syncrepl uses discovered Multus IPs
	// instead of headless DNS. Default false — cross-site always uses Multus when configured.
	// Only meaningful in "multus" mode; ignored in "pod-routed" (in-cluster stays on DNS).
	// +kubebuilder:default=false
	// +optional
	UseForInCluster bool `json:"useForInCluster,omitempty"`
}

// NetworkMode returns the effective replication network mode
// (NetworkModeMultus or NetworkModePodRouted). Returns "" when no replication
// network is configured. When mode is unset it defaults to pod-routed, unless a
// multusNetwork is named (which infers multus). See ADR-016.
func (s *SlapdCluster) NetworkMode() string {
	n := s.Spec.Replication.Network
	if n == nil {
		return ""
	}
	if n.Mode != "" {
		return n.Mode
	}
	if n.MultusNetwork != "" {
		return NetworkModeMultus
	}
	return NetworkModePodRouted
}

// SlapdReplicationConfig holds cluster-level replication configuration.
// Per-database replication settings (deltaSync, RID base, checkpoint, purge)
// are on SlapdDatabase. See ADR-003 and ADR-004.
type SlapdReplicationConfig struct {
	// enabled controls whether replication infrastructure is active. When true,
	// the operator writes the syncrepl stanzas, the accesslog and syncprov
	// overlays and the replication identity into each pod's cn=config, per
	// database (ADR-003, ADR-004). Combined with replicas > 1 it makes the
	// cluster an N-way multi-master; it is also the prerequisite for
	// readReplicas and for externalPeers.
	//
	// Turning it ON is a supported live transition and restarts no pod. Every RW
	// pod carries the accesslog mount and loads the accesslog/syncprov modules
	// whether or not replication is on, precisely so this can be flipped on a
	// running cluster — gating those on it put them in the pod template, where
	// turning replication on replaced pod-0 (ADR-013 amendment 2026-10-07).
	//
	// Turning it OFF again is NOT a supported transition and is not a way to
	// pause replication. It is a one-way door: the operator removes the syncprov
	// and accesslog overlays and DROPS each database's accesslog DB (the change
	// journal is destroyed), but it never clears the olcSyncRepl stanzas or
	// olcMultiProvider — those are only ever written on the enabled path. Each
	// pod is left consuming from peers that no longer serve syncrepl. To demote
	// a replicating cluster, use mode: consumer-only, which is the designed
	// transition and converges the stanzas (ADR-010).
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
	//
	// DERIVED when spec.meshRef is set: the operator computes the peer set from
	// the SlapdMesh's site list and setting this field alongside meshRef is
	// REFUSED, not merged (see checkMeshSpecConflicts). If you are looking for
	// where your peers come from on a mesh, they come from there — and
	// replicasPerPeer below is the one per-peer knob that survives, because it
	// is an input to the derivation rather than an output of it.
	// Two peers must never declare one siteIndex: a peer's RID slot is
	// P + siteIndex, so a shared index is a shared slot — one RID for two
	// providers, which is the shared-connid state slapd never rejects
	// (ADR-031 decision 4, RID-PLAN hazard 5). The rule below refuses it at
	// the API server; the operator refuses it again, because a CR applied
	// with validation off reaches the stanza builder either way.
	//
	// The list is bounded because the uniqueness rule below is O(n^2) — an
	// `all` over a `filter` — and the API server refuses a CEL rule whose cost
	// it cannot bound, rejecting THE WHOLE CRD rather than the rule. The bound
	// is a true constraint rather than an arbitrary one: a peer list longer
	// than the layout's maxSites - 1 has no remote slot to put the surplus
	// peers in and the operator would refuse it anyway (ADR-031 decision 1).
	// 24 is double the default maxSites of 12 — far above any mesh this
	// project contemplates — and leaves the rule comfortably inside the
	// budget.
	// +kubebuilder:validation:MaxItems=24
	// +kubebuilder:validation:XValidation:rule="self.all(p, !has(p.siteIndex) || self.filter(q, has(q.siteIndex) && q.siteIndex == p.siteIndex).size() == 1)",message="two externalPeers declare the same siteIndex: a peer's RID slot is derived from it, so a shared index gives two providers one syncrepl RID (ADR-031). Give every peer its own siteIndex."
	// +optional
	ExternalPeers []ExternalPeer `json:"externalPeers,omitempty"`

	// replicasPerPeer is the default cross-site fan-out for every external peer
	// of this cluster: how many remote pods each local pod opens a syncrepl
	// connection to, per peer. Unset means 1.
	//
	// It exists because a mesh cluster could not reach the per-peer field at
	// all. externalPeers[].replicasPerPeer is refused alongside meshRef (it is
	// part of a derived field), and the derivation never set one — so every
	// mesh was pinned at 1 with no way to say otherwise. Missed rather than
	// decided; see the ADR-028 amendment of 2026-09-28.
	//
	// What it buys is cross-site link REDUNDANCY, not coverage. At 1 each local
	// pod has a single path to each peer site, and losing the remote pod on the
	// far end of it costs that pod its cross-site path until discovery
	// repoints — the 130-268 s ADR-016 window. Data still arrives, via this
	// pod's in-cluster peers; what is gone is the second route. Raising this
	// buys the second route at the price of more connections and more refresh
	// traffic on every peer.
	//
	// Capped at the peer's discovered address count: asking for 3 against a
	// two-pod site silently gets 2, because there is no third pod to attach to.
	// Per-peer externalPeers[].replicasPerPeer still wins where it is set,
	// which is the non-mesh path only.
	// +optional
	// +kubebuilder:validation:Minimum=1
	ReplicasPerPeer *int32 `json:"replicasPerPeer,omitempty"`
	// network configures a dedicated replication network via Multus. When set,
	// the operator adds Multus annotations to slapd pods and discovers assigned IPs
	// from pod network-status annotations. See ADR-007.
	// +optional
	// DERIVED when spec.meshRef is set and the SlapdMesh declares a network:
	// the transport is a property of the fabric, so the mesh owns it. Setting
	// it on both is REFUSED.
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
	// Convention: assign each site/cluster its own stride slot of 16, i.e.
	// site_index*16, so the sid reads as site-digit + pod-digit in the hex
	// field of a CSN (ADR-028 amendment 2026-10-05). ADR-011 codifies a legacy
	// two-site layout on the older decimal convention (site A 101-104, site B
	// 201-204), which remains perfectly legal here — any base is, this field
	// is a bare integer. A standalone cluster (no externalPeers, no plans to
	// add any) can leave this at 0.
	//
	// Valid range is 0..4094 (slapd caps ServerID at 4095).
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4094
	// +optional
	// DERIVED when spec.meshRef is set, from the site's declared serverIDIndex
	// (ADR-028 §4); setting it alongside meshRef is REFUSED unless it happens
	// to equal this exact site's stride slot.
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
// +kubebuilder:validation:XValidation:rule="!has(self.sites) || size(self.sites) == 0 || has(self.meshRef)",message="spec.sites is a subset selector over spec.meshRef's site list and is meaningless without it"
type SlapdClusterSpec struct {
	// suspend pauses the operator's reconciliation of this resource. Existing
	// StatefulSets, Services, and Secrets are left in place; the operator stops
	// observing or mutating them. Use during manual interventions (e.g. running
	// slapadd against the data PVC, hand-editing cn=config) where operator
	// reconciliation would fight your changes. Resume by setting back to false.
	// +kubebuilder:default=false
	// +optional
	Suspend bool `json:"suspend,omitempty"`
	// meshRef names a SlapdMesh in this namespace whose site list describes the
	// multi-site deployment this cluster belongs to (ADR-028 §1).
	//
	// When set, the operator DERIVES the cross-site wiring — spec.replication.
	// serverIDBase, spec.replication.externalPeers and spec.replication.network
	// — from that mesh plus its own site identity (the SITE_NAME on the
	// operator's Deployment, the single per-site fact in the system, ADR-028
	// §4). Those fields then drop out of the user-facing spec, which is what
	// lets one chart be applied byte-identically to every site.
	//
	// The derived fields are spec.replication.serverIDBase, .externalPeers and
	// .network; each carries a pointer back here. Inputs to the derivation —
	// spec.replication.replicasPerPeer, for one — are NOT derived and stay
	// yours to set.
	//
	// Setting any of the derived fields explicitly alongside meshRef is
	// REFUSED rather than silently resolved in either direction: the cluster
	// reports a MeshResolved=False condition and reconciles nothing. Ambiguity
	// about serverIDBase is how a CSN-eating serverID collision gets in
	// quietly, and slapd validates nothing.
	//
	// Unset — the default, and what every existing deployment has — changes
	// nothing at all: the mesh code never runs and the spec is used verbatim.
	// +optional
	MeshRef string `json:"meshRef,omitempty"`
	// sites narrows this cluster to a SUBSET of the mesh's sites, so that
	// cluster X may span sites A+B while cluster Y on the same mesh spans
	// A+B+C. Names are matched against SlapdMesh.spec.sites[].name verbatim
	// (trimmed, never case-folded — ADR-028 §3 amendment).
	//
	// Unset means every site in the mesh, which is what a cluster gets when it
	// says nothing. The selector narrows the derived PEER set only; it never
	// touches the serverID stride slot, which is mesh-wide by construction — two
	// clusters spanning different subsets must still agree about who owns
	// which slot.
	//
	// A name the mesh does not declare, or a selector that excludes this
	// operator's own site, is an error condition rather than a silently
	// shrunken peer set: a half-connected topology looks healthy from both
	// ends of every link that does exist.
	//
	// Only meaningful with meshRef.
	// +optional
	Sites []string `json:"sites,omitempty"`
	// images specifies the container images to use. Optional: when omitted, the
	// operator defaults both slapd and slapd-init to its own registry/path at its
	// own tag (see SlapdImages). Set to override the repository, tag, or pull policy.
	// +optional
	Images SlapdImages `json:"images,omitempty"`
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
	// logLevel is the slapd -d debug bitmask. Unset means the operator's default,
	// 16640 = 256 ("stats") + 16384 ("sync"): operation results plus the
	// consumer-side replication trace. Higher levels add detail (see
	// slapd.conf(5) "loglevel"):
	//
	//   0   no debug output at all (slapd is healthy but kubectl logs is empty
	//       — useful only when log volume itself is the problem)
	//   1   trace function calls (very noisy)
	//   32  search filter processing
	//   64  configuration processing
	//   128 access control list processing
	//   256 stats
	//   16384 sync replication (consumer side)
	//   32768 sync replication (provider side)
	//   -1  everything (firehose; only for one-off debugging)
	//
	// Bitmasks combine, e.g. 256+128=384 for stats+ACL.
	//
	// Why the sync bit is in the DEFAULT and not just documented: a replication
	// incident is diagnosed from what slapd logged while it was going wrong, and
	// at 256 that record does not exist. "Restart it with sync logging on" both
	// loses the history and changes the state you were trying to observe — the
	// restart re-establishes every syncrepl connection. The volume is
	// per-replication-event, not per-operation; the firehose is 32768 (provider
	// side) and -1, and those stay opt-in. This is the alpha
	// best-config-by-default stance (ADR-024 R5, ADR-022): the value that is
	// right for every deployment we can name is the one we ship.
	//
	// A pointer, not a plain int, precisely so that 0 is reachable: with
	// `omitempty` an explicit `logLevel: 0` serialises to nothing, and a
	// kubebuilder default would then silently overwrite it with ours — the way
	// back to silence has to be asking for it, never being unable to ask.
	//
	// Changing this field rewrites the StatefulSet's argument list and therefore
	// rolls the pods; slapd takes -d at startup only.
	// +optional
	LogLevel *int32 `json:"logLevel,omitempty"`
	// tuning holds the server-global tuning family — connection lifetimes, tool
	// threads, the cluster-wide durability posture. Converged per pod; see
	// SlapdTuningConfig.
	// +optional
	Tuning SlapdTuningConfig `json:"tuning,omitempty"`
	// backend configures the back-mdb BACKEND (olcBackend={0}mdb), as opposed
	// to the individual databases. Bootstrap-time only — see SlapdMdbBackendConfig.
	// +optional
	Backend *SlapdMdbBackendConfig `json:"backend,omitempty"`
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
	// uses to pull the slapd and slapd-init images — and, with spec.monitoring
	// enabled, the exporter sidecar's image (slapd-exporter, beside the operator's image by default), so
	// list a Secret for that registry too when it is private. Each Secret must
	// be of type kubernetes.io/dockerconfigjson. Equivalent to setting
	// imagePullSecrets on the StatefulSet pod template directly.
	//
	// Alternative if you standardize pull secrets at the namespace level: skip
	// this field and patch the default ServiceAccount in the workload namespace
	// with its own imagePullSecrets; the kubelet inherits them automatically.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
	// monitoring configures Surface 3 of ADR-033: the slapd exporter sidecar,
	// which reads cn=Monitor over the pod's ldapi socket and serves it as
	// Prometheus metrics. Unset adds nothing to the pod.
	// +optional
	Monitoring SlapdMonitoringConfig `json:"monitoring,omitempty"`
	// accesslogPurge is FORM A of the accesslog purge schedule: one interval,
	// every pod purging together (ADR-024 R6, ADR-037 ninth amendment).
	//
	// The schedule is the cluster's because the cost is the POD's — all of a
	// pod's databases purge in one window, so the thing being scheduled is that
	// pod's contention. What each database KEEPS is still the database's, on
	// SlapdDatabase.spec.replication.accesslogPurge.retention.
	//
	// Form B is spec.rotation, which staggers the pods and moves the write
	// endpoint with them. THE TWO FORMS ARE MUTUALLY EXCLUSIVE: setting an
	// interval here while spec.rotation.enabled is true is REFUSED with a
	// condition, never silently preferred (the shape ADR-028 uses for meshRef
	// beside an explicitly set derived field).
	//
	// Unset means the operator's default interval of 24h, which is what every
	// cluster has always had.
	// +optional
	AccesslogPurge *SlapdAccesslogPurgeConfig `json:"accesslogPurge,omitempty"`
	// rotation is FORM B of the accesslog purge schedule (ADR-037): each pod
	// purges at its own phase on an absolute time grid, and the write endpoint
	// moves in lockstep so the pod taking writes is never the pod purging. The
	// interval becomes spec.replicas x slotLength. Unset changes nothing — see
	// SlapdRotationConfig.
	//
	// EXPERIMENTAL, and off by default (ADR-039 tier 2). Implemented, measured
	// and honestly reported; NOT yet asserted fit for production — what is
	// missing is operating experience, not code. It requires a slapd carrying
	// our accesslog-purge-phase patch, which upstream OpenLDAP does not have: a
	// stock build refuses the three-value olcAccessLogPurge, the operator falls
	// back to the two-value form, and AccesslogPurgeConverged reports
	// PurgePhaseUnsupported naming the version you need — so nothing breaks,
	// but nothing rotates either. It also couples your write endpoint to each
	// provider's own purge schedule, which little field experience anywhere
	// covers, and its benefit is conditional on short client connection
	// lifetimes (see leadTime): turn it on without them and you measure WORSE
	// than leaving it off. Known use cases are narrow — migrations, and bulk
	// provisioning with short-lived write clients. Feedback wanted, positive or
	// negative. User guide: docs/PURGE-ROTATION.md.
	// +optional
	Rotation SlapdRotationConfig `json:"rotation,omitempty"`
}

// SlapdAccesslogPurgeConfig is form A of the purge schedule: one interval for
// the whole fleet, every pod purging at the same instants.
//
// It is the simple form and the one every cluster has implicitly run since
// before ADR-037 — the operator's default interval is 24h, which is what the
// old two-token default "7+00:00 1+00:00" spelled. Form B (spec.rotation)
// staggers the pods and is what removes the write stall ADR-037 measured;
// form A does not pretend to.
//
// The CEL rule carries interval's GRAMMAR check for the same reason
// SlapdRotationConfig's does: controller-gen renders metav1.Duration as a bare
// `type: string` and drops field-level validation markers on it.
// +kubebuilder:validation:XValidation:rule="!has(self.interval) || self.interval.matches('^([0-9]+([.][0-9]+)?(ns|us|µs|ms|s|m|h))+$')",message="accesslogPurge.interval must be a Go duration such as 24h, 6h or 90m"
type SlapdAccesslogPurgeConfig struct {
	// interval is how often every pod sweeps its accesslog journals — slapd's
	// <interval>, the second field of olcAccessLogPurge.
	//
	// Every pod purges on the same absolute grid, so every provider's purge
	// window lands at the same instant, including the one taking writes. That
	// is the configuration ADR-037 measured at a 30 s write stall per cycle and
	// a 3,384 ms peak p99; spec.rotation is what moves it off the write path.
	// Shortening this interval does NOT help — it was measured and refuted
	// (15 s vs 5 min: 18x disk read, p99 2.1 ms to 143-230 ms, 2.2 % of load
	// dropped, no throughput gained).
	//
	// Unset means the operator's default of 24h. A Go duration string, whole
	// seconds only: olcAccessLogPurge's fields are [ddd+]hh:mm[:ss], so an
	// interval finer than a second has no representation on the server and is
	// reported by the operator rather than by the API server. A POINTER so an
	// unset field does not serialise as "0s".
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`
}

// SlapdMonitoringConfig configures the slapd exporter sidecar (ADR-033 Surface 3).
//
// The monitor database itself is always present on every pod, with no switch
// (ADR-033 amendment 2026-10-08); only its exposure — the sidecar — is opt-in.
type SlapdMonitoringConfig struct {
	// enabled adds the exporter sidecar to every slapd pod, read-write and
	// read-only alike. It runs the slapd-exporter image (spec.images.exporter;
	// by default the exporter release compiled into the operator) with no
	// command or arguments, so the exporter's defaults apply: it binds SASL
	// EXTERNAL over ldapi:///run/openldap/slapd.ldapi — no password, no
	// Secret — and serves /metrics on port 9330 (named "metrics").
	//
	// Opt-in: unset means false. Off or unset adds NOTHING to the pod template,
	// so upgrading the operator onto an existing cluster rolls no pod. Flipping
	// it changes the pod template and therefore rolls the pods once, when the
	// user flips it and only then (ADR-013 amendment 2026-10-08: a field whose
	// purpose is to change the pod is not a roll trigger).
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// resources sets the exporter sidecar's compute requests and limits. No
	// default: the exporter is small and collects only on scrape. The field
	// exists for namespaces whose ResourceQuota or LimitRange rejects, or
	// silently defaults, a container that declares nothing — there the sidecar
	// would otherwise block or skew the whole pod. Separate from spec.resources,
	// which belongs to the slapd container.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// SlapdRotationConfig configures the purge/write rotation (ADR-037).
//
// EXPERIMENTAL — ADR-039 tier 2. Off by default. It is implemented, measured
// and honestly reported, and the project does NOT yet assert it is fit for
// production: what is missing is operating experience, not code. Three things
// make it unlike a conventional slapd deployment. It requires a slapd carrying
// our accesslog-purge-phase patch, which upstream does not have — a stock build
// refuses the three-value olcAccessLogPurge, the operator falls back to the
// two-value form, and AccesslogPurgeConverged reports PurgePhaseUnsupported
// naming the version you need. It couples the write endpoint to each provider's
// own purge schedule, which little field experience anywhere covers. And its
// benefit is conditional on client connection lifetime (see leadTime). Known
// use cases are narrow: migrations, and bulk provisioning with short-lived
// write clients. Feedback wanted. User guide: docs/PURGE-ROTATION.md.
//
// The problem it solves: accesslog_purge() deletes expired journal entries in
// an unpaced loop holding the accesslog environment's single LMDB writer, and
// every client write appends to that same environment. A provider's purge
// therefore blocks that provider's WRITES while reads keep flowing — measured
// at a 30 s write stall per cycle on the pod taking writes, and at zero on a
// pod that is not. Scheduling cannot fix it: the duty cycle is
// write rate / delete rate and the purge interval does not appear in it. The
// only lever is moving the purge off the write path.
//
// THE GRID. With n read-write pods and a slot length t, the rotation period is
// T = n·t and pod k purges at phase k·t, counted from the epoch; the write
// endpoint moves every t, in lockstep. T is the interval the operator writes
// into every pod's olcAccessLogPurge, and k·t is that pod's phase — a third
// value in the attribute (<age> <interval> <phase>), anchored on the epoch so
// that nothing drifts and a restarted pod rejoins its own slot.
//
// That third value is OURS: it comes from a local patch to the accesslog
// overlay, not from upstream, so it needs an image carrying it. A stock slapd
// rejects the three-field value outright ("extra cruft after <age> <interval>"),
// which the operator reports as a condition naming the pod rather than looping
// on it (ADR-037 first amendment).
//
// n IS spec.replicas — DECLARED, NEVER OBSERVED. This is deliberate and it is
// not a bug to fix. n fixes T, and T is persisted into every pod's cn=config:
// deriving n from readiness would rewrite the whole fleet's configuration on a
// single transient restart, which is the churn ADR-013 exists to prevent.
// Readiness belongs in writer SELECTION, which persists nothing.
//
// t IS FIXED AND T = n·t VARIES, not the reverse. Scaling then leaves every
// existing pod's phase where it was and gives the new pod phase n·t; only the
// interval moves, and changing an interval does not move a phase. Holding T
// fixed and recomputing t = T/n would re-phase the entire fleet on every scale
// event.
//
// WHERE THE RETENTION LIVES. olcAccessLogPurge has three owners, decomposed by
// ADR-024 R6: the retention age is the database's
// (SlapdDatabase.spec.replication.accesslogPurge.retention), the interval is
// the pod's and therefore the cluster's, and the phase is derived and nobody
// declares it. The operator composes the per-(pod, database) value from them.
// There is no declared interval on a database to conflict with this schedule,
// which is what dissolves the defect the ninth amendment records rather than
// patching it: with nothing to refuse there is no withhold, and with no
// withhold there is no way for a database to end up with no purge value at all.
//
// MUTUALLY EXCLUSIVE WITH spec.accesslogPurge (form A). Declaring an interval
// there while this block is enabled is refused with a condition, never silently
// preferred.
//
// Databases ALIGN, they do not spread: a pod purges all of its accesslogs at
// its own phase, concentrating its disturbance into the one window where it is
// not taking writes. Spreading them over n·D slots would disturb every pod
// throughout the period and defeat the purpose.
//
// WHAT IS MEASURED, AND WHAT THE ROTATION DOES NOT REMOVE. The rotation has
// been measured under load on a two-provider cluster at 100,000 ops/s, and the
// result turns on one thing: with the endpoint moving AT the slot boundary the
// rotation COSTS throughput, and with a leadTime set above the clients'
// connection lifetime it does not. ADR-037's eleventh amendment separates the
// two halves — staggering the purges is free, moving the writes is what costs
// — and its twelfth measures a lead removing the per-boundary stall rather
// than shrinking it: the loss folds flat across the slot with no boundary
// structure left in it.
//
// What stays is the purge itself. The pod that is NOT taking writes still
// stalls its own syncrepl replay while it deletes, so the disturbance is moved
// off the write path and the residue is replication lag — which is what
// decision 1 predicted and the twelfth amendment observed. Size retention well
// above the purge duration, not just above anything else.
//
// THE CLIENT CONNECTION LIFETIME IS A PRECONDITION, NOT A RISK. A Kubernetes
// Service steers NEW connections and never re-wires an established one. That
// is how the mechanism works, it was known when the rotation was chosen, and
// ADR-037's seventh amendment withdraws the earlier framing that treated it as
// an open risk the design's validity waited on. The consequence is a
// requirement on the deployment, not an uncertainty in the feature: a client
// that holds a connection for the life of its process keeps writing to the pod
// it first reached and gains nothing here. Bound your clients' maximum
// connection lifetime τ, set leadTime above it, and send writes to
// <cluster>-write. A deployment that turns this on without doing so measures
// WORSE than one that leaves it off.
//
// The rule below carries slotLength's GRAMMAR check, which cannot live on the
// field itself (see SlotLength). It accepts Go's unsigned duration syntax and
// nothing else, so an unparseable or negative value is refused at admission
// instead of becoming a decode error much later. It says nothing about the
// value being usable — "0s" matches it — and its message is worded to claim no
// more than it checks.
// +kubebuilder:validation:XValidation:rule="!has(self.slotLength) || self.slotLength.matches('^([0-9]+([.][0-9]+)?(ns|us|µs|ms|s|m|h))+$')",message="rotation.slotLength must be a Go duration such as 6h, 90m or 12h30m"
// +kubebuilder:validation:XValidation:rule="!has(self.leadTime) || self.leadTime.matches('^([0-9]+([.][0-9]+)?(ns|us|µs|ms|s|m|h))+$')",message="rotation.leadTime must be a Go duration such as 30s, 1m or 90s"
type SlapdRotationConfig struct {
	// enabled turns the rotation on: per-pod purge phases plus a write endpoint
	// that moves with them.
	//
	// Opt-in: unset means false. ADR-037 decision 6 leaves "defaults on or off"
	// open, and opt-in is what makes an operator upgrade a no-op for every
	// existing cluster — nothing about an installed cluster's purge schedule or
	// its endpoints changes until somebody sets this field.
	//
	// A pointer rather than a plain bool for the same reason spec.logLevel is
	// one: with omitempty an explicit `enabled: false` serialises to nothing,
	// and "the user turned it off" would then be indistinguishable from "the
	// user said nothing" — which matters the day the default moves.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// mode selects WHICH of the rotation's two halves run. Unset is
	// PurgeAndWrite, which is what this field's absence has always meant: a
	// cluster upgraded onto a version that has this field, and not touching it,
	// is byte-identical to one running the version that did not (ADR-013).
	//
	// EXPERIMENTAL in every mode — ADR-039 tier 2 — and the evidence differs,
	// which is why it is stated here rather than in a document:
	//
	//   PurgeAndWrite  The only combination measured end to end. n=2, t=600s,
	//                  100k ops/s: 93,686/s and 16.0M dropped at leadTime 0,
	//                  against 99,245/s and 57,167 dropped at leadTime 180s.
	//                  This is the shape the Helm charts offer.
	//   PurgeOnly      MEASURED INERT for writer throughput (ADR-037 third
	//                  amendment, n=2 at 100k ops/s): a pinned writer purges
	//                  once per interval either way, for the same duration, so
	//                  staggering changes only WHICH pod is degraded at a given
	//                  instant. That quantity — fleet READ capacity, with reads
	//                  running ~75 % on a purging provider — is UNMEASURED, and
	//                  is the only reason to choose this mode.
	//   WriteOnly      NEVER MEASURED as its own arm; every arm of the
	//                  2026-10-20 run had purge staggering on. Its rationale is
	//                  ADR-036 / ITS#9580 dormancy: rotating the writer keeps
	//                  every provider's own serverID advancing, which is
	//                  independent of purging. Reasoned, not run.
	//
	// THE MODES HAVE DIFFERENT IMAGE REQUIREMENTS, measured on a three-site
	// mesh 2026-10-22. A phase is a third value of olcAccessLogPurge that only
	// a slapd carrying our accesslog-purge-phase patch accepts, so:
	//
	//   PurgeAndWrite, PurgeOnly   need the patched image; on a pure build the
	//                              operator writes the two-field fallback and
	//                              reports PurgePhaseUnsupported, and nothing
	//                              is staggered.
	//   WriteOnly                  needs NO patch. The endpoint is a Service
	//                              the operator repoints, so this mode works on
	//                              a stock upstream slapd.
	//
	// In PurgeOnly and WriteOnly, slotsFromWriteToPurge and leadTime have no
	// referent — there is no purge slot to displace from, and no
	// boundary-purge to arrive ahead of. Setting either is REPORTED on
	// RotationConfigured rather than ignored (ADR-024 R4); the operator will
	// not quietly drop a field you set.
	//
	// +kubebuilder:validation:Enum=PurgeAndWrite;PurgeOnly;WriteOnly
	// +optional
	Mode string `json:"mode,omitempty"`
	// slotLength is t, the time each pod spends as the write target, and the
	// spacing between neighbouring pods' purge phases. The resulting purge
	// interval written to every pod is T = spec.replicas × t.
	//
	// REQUIRED when enabled is true, and there is NO DEFAULT. ADR-037 fixes no
	// number for t on purpose: its only guidance is an expectation about
	// deployments — OpenLDAP's own default purges roughly daily, and "a
	// production-shaped deployment is likelier to sit at 6 or 12 hours" — which
	// is not a value the operator may invent. A default here would be a schedule
	// imposed on every cluster that said nothing, and changing it later would
	// re-phase every fleet that had accepted it.
	//
	// Sizing is yours (ADR-037 decision 6): the operator will not refuse a
	// rotation whose phases are too long for your write rate, and it will not
	// shorten one to keep a purge small. The arithmetic is the capacity
	// condition write rate ≤ delete rate × (n−1)/n against your own storage —
	// every provider logs every write, including the ones it receives by
	// syncrepl, while only the inactive ones purge. The hazard is measured:
	// draining ~250,000 entries in one cycle restarted both providers and cost
	// 255 s of replication lag.
	//
	// Set it no shorter than you can afford a purge to take: a purge still
	// running when its next fire comes due makes the daemon skip that cycle, so
	// the pod whose purge ran long is also the pod whose phase has moved. The
	// bound at that end is (n−v)·t ≥ P — see slotsFromWriteToPurge.
	//
	// Since leadTime landed, the binding constraint on t is the LEAD WINDOW
	// rather than the bare purge duration. A feasible lead needs
	// τ ≤ d ≤ (n−1)·t − P — for client connection lifetime τ, purge duration
	// P = w·n·t / r, write rate w and delete rate r — and rearranging that for
	// t gives a floor:
	//
	//	t  ≥  τ / ( (n−1) − n·w/r )
	//
	// which is where the margin actually lives: at the bottom of a measured
	// delete rate it can approach the slot length itself. At n = 2 the upper
	// bound reduces to t − P, and n = 2 is the only shape anyone has measured.
	// See leadTime, and docs/PURGE-ROTATION.md for the arithmetic worked
	// through with a table.
	//
	// The operator derives the whole schedule from this one value — interval
	// T = spec.replicas × t, phase ordinal × t — so there is no second number to
	// keep consistent with it, and no interval of yours for slapd to fire on
	// part-way through a phase (ADR-037's second amendment moves the interval off
	// SlapdDatabase for exactly that reason).
	//
	// WHAT ADMISSION CHECKS IS THE GRAMMAR, AND ONLY THE GRAMMAR. The CEL rule
	// on this struct says the string parses as a Go duration; it does not say
	// the duration is usable, and "0s" satisfies it. A zero or negative slot
	// length, and one finer than a whole second, are rejected by the OPERATOR
	// with a condition naming this cluster, not by the API server.
	//
	// Why that matters rather than being a technicality: with t = 0 every pod's
	// phase k·t is zero, so the whole fleet purges at the same instant — which
	// is precisely the synchronised purge this feature exists to remove, and it
	// would arrive silently, behind a value the API server had just pronounced
	// valid. A rule that promises more than it checks is the failure ADR-037
	// names in its own closing argument: a record of an intention is not a
	// record of a state.
	//
	// One limit the API cannot express: olcAccessLogPurge's fields are
	// [ddd+]hh:mm[:ss], whole seconds, so a slot length that does not land on a
	// whole number of seconds — or whose derived interval or phase does not —
	// has no representation on the server. The admission rule accepts Go's full
	// duration grammar, including "500ms"; refusing what cannot be written is
	// the operator's job, not the API server's.
	//
	// A Go duration string: "6h", "90m", "12h30m". A POINTER because
	// metav1.Duration is a struct and omitempty does not apply to one — the
	// value form would serialise an unset field as "0s", claiming a slot length
	// nobody chose and erasing the distinction between "not set" and "set to
	// zero" that "required when enabled, no default" rests on.
	//
	// The shape is checked by the CEL rule on this struct rather than by a
	// Pattern marker here, and that is forced rather than chosen: controller-gen
	// renders metav1.Duration as a bare `type: string` and DROPS field-level
	// validation markers on it (verified against controller-gen v0.20.0 — the
	// same Pattern marker renders on a *string field and vanishes on this one).
	// Without some rule `slotLength: banana` is admitted and the error surfaces
	// later as a decode failure in whatever typed client reads the object — a
	// rejection in an operator log instead of in the reply to the apply that
	// caused it.
	// +optional
	SlotLength *metav1.Duration `json:"slotLength,omitempty"`
	// slotsFromWriteToPurge is v, the SIGNED displacement in slots from a pod's
	// write slot to its own purge slot, taken mod spec.replicas. So +1 means
	// "purge the slot after writing", -1 means "purge the slot before writing",
	// and -1 is the same answer as n-1 — which is the point of the sign: a bare
	// count like "2 for a three-pod cluster" means n-1 only until somebody adds
	// a pod, while -1 stays correct across every scale event.
	//
	// Unset means 1: purge the slot after writing. That is the configuration
	// ADR-037 prefers and the one being measured first — the writer hands off
	// and then immediately works its journal down, having just stopped taking
	// writes. -1 is the other experiment, and 2 is the deferred cooldown one
	// (a slot between writing and purging), so the ladder is 1, 2, 3 and reads
	// correctly for a run nobody has done.
	//
	// THE RULE IS `v mod n != 0`, NOT a minimum. Zero and every multiple of n
	// put the write on the pod that is purging, which is the one selection this
	// whole feature exists to forbid. The rule needs spec.replicas, so it is
	// the OPERATOR's to report on the RotationConfigured condition, not the API
	// server's to refuse — and the operator names the slot the value RESOLVED
	// to rather than echoing what was typed, since -1 and n-1 are the same
	// answer and only one of them is what you wrote.
	//
	// What v costs: ADR-037 bounds it at both ends, and both ends now have
	// values. A pod must have finished purging before it writes again
	// ((n−v)·t ≥ P, for purge duration P = w·n·t / r), and writes must have
	// drained off a pod before it purges. The drain time the second bound needs
	// is the CLIENT CONNECTION LIFETIME τ — ADR-037's eleventh amendment names
	// it as such, correcting this ADR's own earlier letter for it, and measures
	// it at 60 s in the load arms. It is a property of the client population,
	// not of the cluster, and the operator cannot see it.
	//
	// Which is why the drain half is not v's to satisfy. With the endpoint
	// moving AT the slot boundary the drain window is ZERO whatever t and v
	// are, so the lever is leadTime, not this field: set a lead above τ and the
	// drain bound is met, leaving v carrying only the purge half.
	//
	// v THEREFORE STAYS AT ITS DEFAULT 1. The eleventh amendment considered -1
	// for the lagging hazard and declined it — it trades a client-lifetime
	// problem for a replication-freshness one and inverts the intended
	// write-then-purge ordering, while the lead attacks the cause instead. -1
	// and the deferred cooldown 2 remain experiments, not recommendations.
	//
	// A name stating the two endpoints and the direction of travel is the only
	// construction that survives a negative: "writeSlotsBeforePurge: -1" has to
	// be parsed as "minus one slots before", every time. Here, after is
	// positive and before is negative, and no convention paragraph is needed.
	// +optional
	SlotsFromWriteToPurge *int32 `json:"slotsFromWriteToPurge,omitempty"`
	// leadTime is d, how long BEFORE a slot boundary the write endpoint moves
	// to the pod that will hold the next slot.
	//
	// Why it exists, measured. With no lead the endpoint moves AT the boundary,
	// so the pod that stops taking writes starts purging at the same instant and
	// every connection that has not yet aged out delivers its writes into an
	// active purge. A two-provider run with rotation on and writes following the
	// endpoint lost 2.8 % of throughput against the same rotation with writes
	// pinned, and 99.9 % of that loss landed in the first 30 s after a boundary,
	// 69 % of it inside the client's 20 s connection lifetime. Staggering the
	// purges was free; moving the writes was what cost, and it cost only at the
	// handover.
	//
	// And measured again with the lead in place, one variable changed: the
	// unleaded arm lost 100 % of its dropped iterations in one contiguous block
	// after every boundary, while the leaded arm showed flat background across
	// the whole slot with no boundary structure at all, at a worst-case write
	// latency two orders of magnitude lower. The per-boundary stall is GONE,
	// not smaller (ADR-037 twelfth amendment).
	//
	// So set it above your clients' maximum connection lifetime τ: with d >= τ,
	// no connection still points at the old writer when that pod begins purging.
	// THE OPERATOR CANNOT DERIVE τ — it is a property of your client, not of the
	// cluster — so there is no default and no recommended value here. Unset
	// means zero, which is the endpoint moving at the boundary: exactly the
	// behaviour of every cluster that had no such field.
	//
	// It shifts WHO takes writes and WHEN the endpoint moves, and nothing else.
	// The purge grid is untouched: the interval stays spec.replicas × slotLength
	// and pod k still purges at k × slotLength from the epoch, so turning a lead
	// on or off never rewrites a pod's olcAccessLogPurge.
	//
	// THE ONE BOUND, and the operator reports it rather than the API server
	// refusing it, because it needs spec.replicas: d must stay below
	// (spec.replicas − 1) × slotLength. At or beyond that the lead reaches back
	// past the instant the incoming writer STARTED purging, so the endpoint
	// would hand writes to a pod that is going to purge during its own write
	// slot — nonsense independent of any measurement. Nothing else about the
	// value is checked: a lead that is too short for your clients is a
	// configuration the operator will not second-guess.
	//
	// That is the operator's guard, and it is weaker than the FEASIBILITY
	// bound, which is τ ≤ d ≤ (n−1)·t − P for purge duration P = w·n·t / r.
	// At n = 2 — the only shape measured — the upper bound reduces to t − P,
	// which is why docs/PURGE-ROTATION.md states it in that form; it is the
	// reduction, not a second rule. Both ends need numbers the operator does
	// not have (τ is the client's, w and r are the deployment's), so neither
	// is enforced. Note that the upper bound CLOSES as the write rate rises:
	// a lead sized once and then grown into stops protecting silently, and the
	// symptom is the unleaded failure returning.
	//
	// A Go duration string: "30s", "1m", "90s". A POINTER for the reason
	// slotLength is one — metav1.Duration is a struct, omitempty does not apply,
	// and the value form would serialise an unset field as "0s". The grammar is
	// checked by the CEL rule on this struct rather than by a Pattern marker
	// here: controller-gen renders metav1.Duration as a bare `type: string` and
	// drops field-level validation markers on it.
	// +optional
	LeadTime *metav1.Duration `json:"leadTime,omitempty"`
}

// Rotation modes (ADR-037 seventeenth amendment). The two halves of the
// rotation — staggering each pod's accesslog purge, and moving the write
// endpoint off the pod that is purging — are one feature today and two
// decisions in principle. These name them separately.
//
// All three are ADR-039 tier 2. The evidence differs per mode and is stated on
// the Mode field, because under ADR-039 the evidence travels with the knob.
const (
	// RotationModePurgeAndWrite staggers the purges AND moves the endpoint.
	// The default, and the only combination measured end to end.
	RotationModePurgeAndWrite = "PurgeAndWrite"
	// RotationModePurgeOnly staggers the purges and leaves the write endpoint
	// alone. slotsFromWriteToPurge and leadTime have no referent here.
	RotationModePurgeOnly = "PurgeOnly"
	// RotationModeWriteOnly moves the endpoint and declares no purge schedule,
	// so spec.accesslogPurge.interval is its companion rather than a conflict.
	// slotsFromWriteToPurge and leadTime have no referent here either.
	RotationModeWriteOnly = "WriteOnly"
)

// ExternalPeerReplicationState summarises the replication health of one external peer.
// +kubebuilder:validation:Enum=Synced;Lagging;PartiallyVerified;Unreachable
type ExternalPeerReplicationState string

const (
	// ReplicationSynced means every database's newest contextCSN on the peer is
	// within the sync threshold — and every database was actually read on BOTH
	// sides of the comparison, here as well as there.
	ReplicationSynced ExternalPeerReplicationState = "Synced"
	// ReplicationLagging means at least one database's newest contextCSN on the
	// peer is behind the local newest for that same database.
	ReplicationLagging ExternalPeerReplicationState = "Lagging"
	// ReplicationPartiallyVerified means the peer answered and every database
	// that could be compared is within the sync threshold, but at least one
	// database yielded no readable evidence on one side of the comparison or the
	// other (lastError names it).
	//
	// EITHER side. The missing evidence is usually the peer's, and that is what
	// the state was introduced for: CSN queries are per-database LDAP operations
	// with per-database bind identities (ADR-008), so one database can fail while
	// another on the same host succeeds — measured on 2026-09-13, where db1 bound
	// and answered while db2's bind returned err=49 against the same pod. But a
	// missing LOCAL baseline is the same absence, and reading the state as
	// "the peer is at fault" sends a debugger to the wrong end of the link: when
	// THIS site cannot read its own contextCSN for a database, it has no
	// comparison basis either, and claiming health it cannot see is the defect
	// (ADR-008 amendment 2026-10-06). Check lastError, then check this site's
	// own ReplicationConverged and PodsResponsive before suspecting the peer.
	//
	// It exists because "we know nothing" and "we know some of it" are different
	// states. Folding it into Unreachable would state something false about the
	// peer (it WAS reached) and seed a wrong decision in anything acting on the
	// field; folding it into Synced is a verdict computed from whichever database
	// happened to answer.
	//
	// Named for what is known rather than what is not: "Unknown" would discard
	// the verified half, which is the informative half.
	ReplicationPartiallyVerified ExternalPeerReplicationState = "PartiallyVerified"
	// ReplicationUnreachable means no database on the peer yielded any readable
	// contextCSN — nothing about it could be verified.
	ReplicationUnreachable ExternalPeerReplicationState = "Unreachable"
)

// DiscoveredProvider is one remote pod that peer discovery found: the
// StatefulSet ordinal that is its stable identity, and the address it
// currently answers on.
//
// The two are deliberately separate facts. An address is what changes — a pod
// is replaced, an IP is reassigned (ADR-016) — while the ordinal is declared
// identity and does not. ADR-031 keys a syncrepl RID on the identity, so a
// discovery result that carried only addresses would force the RID to move
// every time the far end was rebuilt.
type DiscoveredProvider struct {
	// ordinal is the remote pod's StatefulSet ordinal, parsed from its name.
	// +required
	Ordinal int32 `json:"ordinal"`
	// address is the IP the remote pod currently answers replication on:
	// its primary pod IP in pod-routed mode, its Multus net1 IP otherwise.
	// +required
	Address string `json:"address"`
}

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
	// discoveredProviders lists the remote pods discovery found, each as an
	// (ordinal, address) pair, ascending by ordinal. Only populated for peers
	// using discovery mode. Replaced discoveredAddresses, a bare IP list whose
	// order carried no provider identity (ADR-031).
	// +optional
	DiscoveredProviders []DiscoveredProvider `json:"discoveredProviders,omitempty"`
	// replicationState reports the CSN convergence state of this peer, judged
	// per database (a contextCSN vector is only comparable within one suffix),
	// and from evidence read on BOTH sides of each comparison.
	// Synced: every database was read here and there, and each is within
	// threshold of ours.
	// Lagging: a database that WAS read on both sides is behind ours by more
	// than the threshold.
	// PartiallyVerified: the peer answered and everything comparable is current,
	// but at least one database yielded no readable contextCSN — on the peer, or
	// on THIS site, which has no comparison basis without its own baseline
	// (lastError names the databases, not the side). Before suspecting the peer,
	// read this cluster's own ReplicationConverged and PodsResponsive
	// conditions.
	// Unreachable: no database on the peer could be read at all.
	// +optional
	ReplicationState ExternalPeerReplicationState `json:"replicationState,omitempty"`
	// lagSeconds reports the max per-database CSN timestamp delta between local
	// and remote as a decimal string (e.g. "3.2"). Only set when replicationState
	// is Lagging.
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
