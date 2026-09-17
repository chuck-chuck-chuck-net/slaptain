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

// CleanupPolicy defines what happens to the database when the SlapdDatabase CR is deleted.
type CleanupPolicy string

const (
	// CleanupPolicyRetain keeps the database definition and data in slapd after CR deletion.
	// The database becomes unmanaged — it continues to function but the operator no longer
	// reconciles ACLs, indices, replication, or schema for it.
	CleanupPolicyRetain CleanupPolicy = "Retain"
	// CleanupPolicyDelete removes the database definition from cn=config on each pod after
	// CR deletion. Data files on disk are NOT deleted (the operator has no PVC access).
	CleanupPolicyDelete CleanupPolicy = "Delete"
)

// SlapdDatabasePhase represents the lifecycle phase of a database.
type SlapdDatabasePhase string

const (
	DatabasePhasePending  SlapdDatabasePhase = "Pending"
	DatabasePhaseRunning  SlapdDatabasePhase = "Running"
	DatabasePhaseDegraded SlapdDatabasePhase = "Degraded"
	DatabasePhaseError    SlapdDatabasePhase = "Error"
)

// DatabaseCredentials references the Secret containing passwords for this database.
type DatabaseCredentials struct {
	// secretName is the name of the Secret containing passwords for this database.
	//
	// Keys (all plaintext):
	//   - "root-password" — required. Password for cn=admin,<suffix>.
	//   - "replication-password" — required when the cluster needs replication
	//     (replicas > 1 OR externalPeers configured). Bind password for
	//     cn=replication,<suffix>. May be omitted when the cluster is a single
	//     standalone pod with no peers.
	//
	// When this field is empty, the operator auto-generates a Secret named
	// "<slapddatabase-name>-credentials" with both keys populated. When this
	// field is set, the Secret is used as-is — the operator does NOT patch
	// missing keys into it. A user-provided Secret missing a required key
	// fails reconciliation with phase=Error / reason=CredentialsInvalid so the
	// gap is surfaced at apply time rather than silently breaking replication.
	//
	// Values MUST be plaintext, not {SSHA}/{ARGON2}/{CRYPT} hashes — the operator
	// binds to slapd using these passwords for ACL, schema, seed, and replication
	// management. To preserve an admin password through a migration, pre-create
	// this Secret with the legacy plaintext (not its stored hash) before creating
	// this CR. See docs/ONBOARDING.md §Secret and Credential Model.
	// +optional
	SecretName string `json:"secretName,omitempty"`
}

// DatabaseReplicationConfig holds per-database replication settings.
type DatabaseReplicationConfig struct {
	// enabled controls whether this database participates in cluster
	// replication. Default true. Set false to explicitly exclude this database
	// from the cluster's syncrepl topology — rare but legitimate cases include
	// partial-migration coexistence (one DB pulls from legacy while another is
	// brand-new and standalone), per-site local state, and static reference DBs.
	//
	// When the parent SlapdCluster has replication.enabled=false this setting
	// has no effect: the cluster-level gate short-circuits all replication
	// work regardless of per-database intent. The field is still required at
	// the CRD level (every SlapdDatabase must declare its replication intent
	// up-front, even in non-replicated clusters) — this is by design, to make
	// "I forgot to configure replication" loud at admission time.
	//
	// Tristate (*bool) for the same reason as DeltaSync: round-trip safety for
	// an explicit false through the API server's defaulting.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// ridBase is the base Replica ID for this database's syncrepl stanzas.
	// In-cluster peer i gets RID = ridBase + i + 1.
	// External peer j gets RID = ridBase + 50 + j + 1.
	// Must be unique across all SlapdDatabase CRs in the same cluster to avoid
	// RID collisions. NOT machine-enforced today: the operator validates only
	// that ridBase is present when replication is enabled (the CEL rule below);
	// cross-CR uniqueness is the deployer's responsibility. See docs/BACKLOG.md.
	//
	// Required when enabled=true (the default); may be omitted when
	// enabled=false. Encoded as *int32 so CEL's has() can distinguish
	// "user supplied a value" from "Go zero value."
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=949
	RIDBase *int32 `json:"ridBase,omitempty"`
	// deltaSync enables delta-syncrepl via the accesslog overlay for this database.
	// When true, the accesslog overlay is added to this database and syncrepl stanzas
	// use syncdata=accesslog. When false, plain syncrepl (full entry sync) is used.
	//
	// Tristate (*bool): nil falls back to the default (true). An explicit
	// false must round-trip from kubectl through the API server unchanged,
	// which a plain bool + omitempty + kubebuilder:default does NOT guarantee
	// (false is the bool zero value, gets stripped at marshal time, and the
	// API server fills the default back in).
	// +kubebuilder:default=true
	// +optional
	DeltaSync *bool `json:"deltaSync,omitempty"`
	// syncprovCheckpoint sets the syncprov overlay checkpoint interval on this
	// database's *data* DB (olcSpCheckpoint).
	// Format: "<ops> <minutes>", e.g. "500 15" means checkpoint the contextCSN
	// every 500 operations or 15 minutes.
	//
	// Empty (or the explicit sentinel "none") means no olcSpCheckpoint at all —
	// OpenLDAP's own default behaviour. Unlike accesslogPurge this carries no
	// operator default: an unwritten checkpoint is a performance trade, not a
	// correctness cliff.
	//
	// Converged on every reconcile: a later edit is applied to pods whose
	// syncprov overlay already exists, and clearing the field removes the
	// attribute. (It used to be written only when the overlay was first
	// created, so edits were silently ignored — ADR-024 R4.)
	// +optional
	SyncprovCheckpoint string `json:"syncprovCheckpoint,omitempty"`
	// syncprovSessionlog sizes the syncprov overlay's in-memory session log on
	// this database's *data* DB, in operations (olcSpSessionlog). The session
	// log lets a reconnecting consumer whose cookie is still inside the window
	// be answered from memory instead of a present-phase scan of the whole
	// database.
	//
	// Three states:
	//   - unset (nil) — the operator applies its default of 5000 operations
	//     whenever this database's data DB gets a syncprov overlay. This is the
	//     normal case; the default is on.
	//   - 0 — disabled. No olcSpSessionlog is written, and an existing one is
	//     removed. OpenLDAP's own out-of-the-box behaviour.
	//   - >0 — that operation count.
	//
	// Never applied to an accesslog database's syncprov overlay: there a
	// session log would displace the minCSN guard and turn a loud
	// REFRESH_REQUIRED into silent under-replication. See ADR-022 for the
	// placement rule, the cost model behind the default, and the limits (the
	// log is in-memory, so empty after a restart, and self-wiping on
	// refresh-phase traffic).
	// +optional
	// +kubebuilder:validation:Minimum=0
	SyncprovSessionlog *int32 `json:"syncprovSessionlog,omitempty"`
	// accesslogPurge sets the purge policy on this database's accesslog change
	// journal (olcAccessLogPurge; only used when deltaSync=true).
	// Format: "<maxage> <interval>", e.g. "2+00:00 1+00:00" means purge entries
	// older than 2 days, sweeping once a day.
	//
	// Three states:
	//   - unset ("") — the operator applies its default of "7+00:00 1+00:00":
	//     keep 7 days, sweep daily. This is the normal case.
	//   - "none" — no purging at all. The journal grows unbounded; you are
	//     taking responsibility for it.
	//   - any other value — used verbatim.
	//
	// Why there is a default at all: an unpurged journal grows until it reaches
	// its 8Gi map ceiling, and because the accesslog overlay sits in the DATA
	// database's write path, the moment the journal cannot take a write neither
	// can the data. That is a slow-motion outage, not a tuning wart, so "no
	// opinion" is not an acceptable default.
	//
	// Why 7 days: the window is how long a consumer may stay away and still
	// resume from a delta instead of a full refresh, so it must cover a
	// weekend-plus outage. On 2.7-default images the interaction between purging
	// and a dormant consumer (upstream ITS#9580) is mitigated by the cookie-flush
	// fix; clusters still on the -ol26 image pair carry elevated exposure and
	// should treat a long consumer outage as needing a checked resync.
	//
	// Converged on every reconcile: a later edit is applied to pods whose
	// accesslog overlay already exists, and "none" removes the attribute.
	// +optional
	AccesslogPurge string `json:"accesslogPurge,omitempty"`
	// externalAccesslogSuffix overrides the accesslog suffix used as `logbase`
	// in the delta-syncrepl stanzas pointing at spec.replication.externalPeers
	// on the parent SlapdCluster.
	//
	// `logbase` is a search base sent to the *provider*, so an external stanza
	// must spell the peer's accesslog suffix, not ours (ADR-019 R9). Empty —
	// the normal case — means derive it from this database's own accesslog
	// suffix (cn=accesslog-<name of this SlapdDatabase>), which is correct
	// whenever the peer is another slaptain cluster running a SlapdDatabase of
	// the same name. Set it only when the peer is not slaptain (ADR-011
	// supports syncMode: delta against a foreign provider, whose log may be
	// cn=log or anything else) or does not use the same SlapdDatabase name.
	//
	// Deliberately carries no CRD-level default (ADR-019 R10): a kubebuilder
	// default marker is a constant, while the correct value depends on this
	// CR's own name. The derivation happens at the point of use and is never
	// written back into the spec.
	//
	// Accepted limitation (ADR-019 R9): one value per database, so a database
	// consuming delta from two different foreign providers with differing log
	// suffixes is not expressible. The escape, per-peer-per-database, is
	// deliberately not pre-built — ExternalPeer lives on SlapdCluster and
	// carries no database selector, so a field there would be one value across
	// all databases, which is the wrong axis for a per-database journal.
	//
	// Ignored for peers with syncMode: plain — those stanzas emit no logbase
	// at all.
	// +optional
	ExternalAccesslogSuffix string `json:"externalAccesslogSuffix,omitempty"`
}

// BootstrapSource selects where a SlapdDatabase's data tree is restored from
// when spec.bootstrapFrom is set. Exactly one of backupRef or s3 must be set.
//
// +kubebuilder:validation:XValidation:rule="has(self.backupRef) != has(self.s3)",message="bootstrapFrom requires exactly one of backupRef or s3."
type BootstrapSource struct {
	// backupRef is the name of a SlapdBackup (same namespace) to restore from.
	// +optional
	BackupRef string `json:"backupRef,omitempty"`
	// s3 points directly at a backup artifact in object storage — for restoring
	// from a backup that has no SlapdBackup object (e.g. a legacy slapcat dump
	// or a backup produced by another cluster).
	// +optional
	S3 *BootstrapS3Source `json:"s3,omitempty"`
	// skipReplicationPasswordCheck disables the restore preflight's verification
	// that the cluster's replication-password matches the cn=replication entry in
	// the backup. The check is default-deny: it fails the restore on a password
	// mismatch OR an unverifiable hash scheme (anything other than {SSHA}). Set
	// this to true ONLY when you have accepted the consequence: if the passwords
	// don't actually match, intra-cluster syncrepl will silently fail to
	// authenticate after the restore and you must repair cn=replication yourself.
	// A backup with no replication-pw-hash metadata (a foreign/legacy dump) is
	// never checked and does not need this flag.
	// +optional
	SkipReplicationPasswordCheck bool `json:"skipReplicationPasswordCheck,omitempty"`
}

// BootstrapS3Source addresses a single backup artifact in object storage.
type BootstrapS3Source struct {
	// storage configures the S3 bucket, endpoint, region, and credentials.
	// +required
	Storage S3StorageSpec `json:"storage"`
	// key is the object key (within storage.prefix) of the gzipped LDIF
	// artifact to restore.
	// +required
	Key string `json:"key"`
}

// DatabaseSeedConfig defines initial data to populate in the database.
type DatabaseSeedConfig struct {
	// entries is a list of LDIF entries to add when the database is first created.
	// Each string is a complete LDIF entry (dn: line, attributes, separated by
	// blank lines). Applied once and tracked in status — not re-applied on
	// subsequent reconciles.
	// +optional
	Entries []string `json:"entries,omitempty"`
	// configMapRef references a ConfigMap containing LDIF data. The ConfigMap's
	// data keys are processed in lexicographic order. Each value is parsed as
	// one or more LDIF entries separated by blank lines.
	// +optional
	ConfigMapRef string `json:"configMapRef,omitempty"`
	// site names the mesh site whose operator is allowed to apply this seed —
	// the founder. Every other site reads the same value, finds it is not its
	// own site, withholds the seed, and receives the DIT by replication.
	//
	// This exists so that a multi-site SlapdDatabase can be byte-identical at
	// every site (ADR-028 §3). The alternative — carrying spec.seed at the
	// founder and stripping it everywhere else — is a per-site EDIT of an object
	// that must not differ per site, and the deployment procedure it requires is
	// exactly what ADR-025 says must not be relied upon. Naming a site turns
	// single-creator seeding from a procedure into a property of the spec.
	//
	// The operator's own site identity comes from its installation config (the
	// SITE_NAME env var on the operator Deployment), never from a CR — putting
	// it in a CR would destroy the byte-identical property (ADR-028 §4).
	//
	// Unset (the default) means "no site restriction": the seed is applied
	// wherever this database reconciles, which is today's behaviour and what
	// every single-site deployment wants.
	//
	// Set while the operator has NO site identity configured, the seed is
	// withheld and the database stays Degraded until SITE_NAME is set. That is
	// deliberate: an unreadable identity never counts as a match, because a
	// database that never seeds is loud and recoverable whereas a multi-site
	// seed race produces a permanent, silent glue suffix (ADR-025).
	// +optional
	Site string `json:"site,omitempty"`
}

// SlapdDatabaseSpec defines the desired state of SlapdDatabase.
//
// +kubebuilder:validation:XValidation:rule="self.replication.enabled == false || has(self.replication.ridBase)",message="spec.replication.ridBase is required when replication.enabled=true (the default). Set replication.enabled=false to explicitly exclude this database from cluster replication."
// +kubebuilder:validation:XValidation:rule="!(has(self.seed) && has(self.bootstrapFrom))",message="spec.seed and spec.bootstrapFrom are mutually exclusive: a restore replaces seeding. Set at most one."
type SlapdDatabaseSpec struct {
	// suspend pauses the operator's reconciliation of this resource. The data
	// database, ACLs, syncrepl stanzas, and seed entries are left in place; the
	// operator stops observing or mutating cn=config or the data tree on behalf
	// of this CR. Use during manual interventions (e.g. external syncrepl setup
	// before ADR-010 lands, hand-editing ACLs) where operator reconciliation
	// would fight your changes. Resume by setting back to false.
	// +kubebuilder:default=false
	// +optional
	Suspend bool `json:"suspend,omitempty"`
	// clusterRef is the name of the SlapdCluster this database belongs to.
	// Must be in the same namespace.
	// +required
	ClusterRef string `json:"clusterRef"`
	// suffix is the LDAP suffix (base DN) for this database, e.g. "o=myapp" or
	// "dc=example,dc=org". Must be unique within the cluster.
	// +required
	Suffix string `json:"suffix"`
	// rootDN is the administrative DN for this database. Defaults to
	// "cn=admin,<suffix>" when empty.
	// +optional
	RootDN string `json:"rootDN,omitempty"`
	// credentials references the Secret containing the root password for this database.
	// +optional
	Credentials DatabaseCredentials `json:"credentials,omitempty"`
	// dataDirectory is the subdirectory under /ldap-data/ where this database's
	// LMDB files are stored. Defaults to a sanitized form of the suffix
	// (e.g. "o=myapp" → "myapp"). Must be unique within the cluster.
	// +optional
	DataDirectory string `json:"dataDirectory,omitempty"`
	// maxSize is the LMDB map size (olcDbMaxSize) for this database. Accepts
	// either a Kubernetes quantity ("32Gi", "1G") or a bare byte count
	// ("34359738368"); the operator converts to the bare byte count slapd
	// wants. Unset means the operator's default (32Gi), NOT back-mdb's own
	// ~10 MB — a database that outgrows its map size stops accepting writes
	// (MDB_MAP_FULL). See ADR-024 R5.
	//
	// The map size is an address-space reservation, not an allocation: LMDB
	// sparsely grows the file within it, so the REAL bound on this database is
	// the size of the /data volume (spec.persistence.data on the SlapdCluster).
	// Sizing the map far above the volume is therefore cheap and normal; the
	// volume, not this field, is what you grow to make room.
	//
	// SET WHEN THE DATABASE IS CREATED, and fixed from then on. slapd cannot
	// take this at runtime: an ldapmodify of olcDbMaxSize against a running
	// back-mdb database SEGFAULTS slapd (observed on OpenLDAP 2.7.1), because
	// LMDB's mdb_env_set_mapsize may not be called while transactions are
	// active and a live slapd always has some. Editing this field on an
	// existing SlapdDatabase therefore does NOT resize anything — it sets the
	// TunablesConverged condition to False with the current value, the desired
	// value and the change path, which is to recreate the database (back up,
	// delete, restore into a fresh one — ADR-014). Reported, never silently
	// dropped: ADR-024 R4 with R2's change-path documentation.
	// +optional
	MaxSize string `json:"maxSize,omitempty"`
	// sizeLimit is the maximum number of entries a search against this database
	// may return (olcSizeLimit). Unset means the operator's default,
	// "unlimited" — NOT slapd's built-in 500, which silently truncates any
	// enumeration of a directory larger than a fixture (ADR-024 R5). Set an
	// explicit number to restore a cap; per-identity exemptions go in
	// spec.limits. Converged per pod.
	// +kubebuilder:validation:Pattern=`^(unlimited|none|[0-9]+)$`
	// +optional
	SizeLimit *string `json:"sizeLimit,omitempty"`
	// timeLimit is the maximum number of seconds slapd spends answering a
	// search against this database (olcTimeLimit). Unset means the operator's
	// default, "unlimited" — slapd's built-in 3600 aborts exactly the bulk
	// sweeps a large directory exists to serve (ADR-024 R5). Set an explicit
	// number of seconds to restore a cap. Converged per pod.
	// +kubebuilder:validation:Pattern=`^(unlimited|none|[0-9]+)$`
	// +optional
	TimeLimit *string `json:"timeLimit,omitempty"`
	// limits is a list of per-identity search-limit exemptions (olcLimits
	// values) in slapd.conf "limits" format, without the {N} index prefix —
	// e.g. `dn.exact="cn=bulkreader,dc=example,dc=org" size=unlimited
	// time=unlimited size.prtotal=unlimited`. Modelled like spec.acls: the
	// operator numbers them and converges them per pod.
	//
	// The replication identity's own exemption is NOT configurable here: it is
	// part of the replication contract the operator owns end to end and is
	// always prepended to this list (ADR-024 R7, ADR-020 amendment). A
	// replication identity capped at slapd's default 500 entries caps
	// replication itself at 500 entries.
	// +optional
	Limits []string `json:"limits,omitempty"`
	// noSync disables fsync after each write (olcDbNoSync). Improves write
	// throughput at the cost of durability on an unclean shutdown; replicas and
	// the delta-syncrepl journal provide the redundancy that makes the trade
	// defensible.
	//
	// Unset inherits SlapdCluster.spec.tuning.noSync, whose own default is false
	// (fsync after every write). Durability posture is usually a cluster-wide
	// property — every member trading fsync for throughput because replication
	// provides the redundancy — so the cluster field is the normal place to set
	// it and this one is the per-database override (ADR-024 R6).
	//
	// Converged per pod on every reconcile: unlike maxSize, slapd takes
	// olcDbNoSync at runtime (verified live on 2.7.1 — the modify is accepted
	// and the process survives), so flipping this field applies. It used to be
	// silently dropped on an existing database, which is the ADR-024 R4
	// violation this field's convergence pays off.
	//
	// noSync without a checkpoint loses an unbounded window of writes. The
	// operator will not let you reach that silently: spec.checkpoint defaults to
	// a value, and explicitly disabling it (checkpoint: "") while noSync is on
	// is rejected.
	// +optional
	NoSync *bool `json:"noSync,omitempty"`
	// checkpoint is the back-mdb disk-buffer flush interval (olcDbCheckpoint),
	// spelled "<kbyte> <min>" — flush after that many kilobytes have been
	// written or that many minutes have passed, whichever comes first. It is
	// what bounds the write window noSync exposes; per slapd-mdb(5) it only
	// takes effect when noSync is in force, but it is written unconditionally so
	// that turning noSync on is a one-field change that is safe by construction.
	//
	// Unset means the operator's default, "1024 5" (ADR-024 R5). Set to the
	// empty string to write no checkpoint at all — which the operator rejects
	// while noSync is on, because that combination is the unbounded-loss one.
	//
	// This database's accesslog journal gets its own, longer interval; it is
	// operator-set and not configurable, like the journal's map size.
	// Converged per pod.
	// +kubebuilder:validation:Pattern=`^$|^[0-9]+ [0-9]+$`
	// +optional
	Checkpoint *string `json:"checkpoint,omitempty"`
	// rtxnSize bounds how many entries one back-mdb read transaction covers
	// before it is broken up and restarted (olcDbRtxnSize). A long-running read
	// transaction — a syncrepl full refresh or a bulk export is exactly that —
	// pins free pages for its whole duration and forces the map to grow rather
	// than reuse them.
	//
	// Unset means the operator's default, 10000, which is also slapd's own: we
	// write it explicitly so the value is visible in cn=config and cannot move
	// underneath a cluster on a base-image bump. Converged per pod. 0 restores
	// "no limit".
	// +kubebuilder:validation:Minimum=0
	// +optional
	RtxnSize *int32 `json:"rtxnSize,omitempty"`
	// envFlags is the list of LMDB environment flags for this database
	// (olcDbEnvFlags). Valid values: "writemap", "nometasync", "nosync",
	// "mapasync". They are the standard high-write-rate levers for back-mdb;
	// "writemap" in particular changes the write-path cost profile materially on
	// a large map. Unset means none, which is slapd's own behaviour — this is a
	// knob for a measured problem, not a default we hold an opinion about.
	//
	// SET WHEN THE DATABASE IS CREATED, and fixed from then on — the same class
	// as maxSize, for the same reason and on the same evidence. An ldapmodify of
	// olcDbEnvFlags adding "writemap" against a running database SEGFAULTS slapd
	// (observed on OpenLDAP 2.7.1, exit 139, reproduced on a live pod):
	// MDB_WRITEMAP is an mdb_env_open flag and LMDB cannot add it to an open
	// environment. "nometasync"/"nosync" alone modify cleanly, but the attribute
	// is one multi-valued list and nothing stops a later edit from adding
	// "writemap" to it, so the whole attribute is create-only.
	//
	// Editing it on an existing SlapdDatabase therefore changes nothing: it sets
	// TunablesConverged=False with the current value, the desired value and the
	// change path (recreate the database — back up, delete, restore into a fresh
	// one, ADR-014). Reported, never silently dropped: ADR-024 R4 with R2's
	// change-path documentation. See the ADR-024 amendment of 2026-09-13.
	// +kubebuilder:validation:items:Enum=writemap;nometasync;nosync;mapasync
	// +optional
	EnvFlags []string `json:"envFlags,omitempty"`
	// acls is the list of OpenLDAP ACL rules (olcAccess entries) to apply to
	// this database on every pod. Rules are in standard slapd.conf "access to ..."
	// format, without the {N} index prefix. The operator numbers them and applies
	// them per-pod (cn=config is node-local).
	//
	// When empty or omitted, no olcAccess attribute is written and slapd uses
	// its built-in default: "to * by * read" (anonymous + authenticated users
	// read all attributes). The rootdn (cn=admin,<suffix>) always bypasses ACLs
	// regardless of this list. To replicate legacy slapd's "no access rules"
	// behavior, leave this field unset.
	// +optional
	ACLs []string `json:"acls,omitempty"`
	// indices is the list of index directives for this database. Each string is
	// an index specification in slapd.conf format, e.g. "objectClass eq" or
	// "uid eq,sub". The operator applies these as olcDbIndex entries.
	// +optional
	Indices []string `json:"indices,omitempty"`
	// replication holds per-database replication settings. Always required —
	// every SlapdDatabase must declare its replication intent at the CR level,
	// even if the parent cluster doesn't replicate. Set replication.enabled=false
	// to explicitly exclude this database from cluster replication; otherwise
	// supply replication.ridBase. See DatabaseReplicationConfig for the rationale.
	// +required
	Replication DatabaseReplicationConfig `json:"replication"`
	// seed defines initial data to populate in the database on first creation.
	// Applied once and tracked in status.
	// +optional
	Seed *DatabaseSeedConfig `json:"seed,omitempty"`
	// bootstrapFrom restores this database's data tree from a backup on first
	// creation, instead of seeding it. Mutually exclusive with seed. One-shot,
	// tracked via status.restoreApplied. Restore runs as a cluster-coordinated
	// offline slapadd (scale-to-0) — a deliberate, documented cluster-wide
	// interruption (see ADR-014). Valid only for a newly created database (its
	// peer copies must be empty); it never overwrites a populated database.
	// +optional
	BootstrapFrom *BootstrapSource `json:"bootstrapFrom,omitempty"`
	// cleanupPolicy controls what happens when this CR is deleted.
	// Retain (default): database stays in slapd, becomes unmanaged.
	// Delete: database definition is removed from cn=config (data files are NOT deleted).
	// See ADR-005.
	// +kubebuilder:default=Retain
	// +kubebuilder:validation:Enum=Retain;Delete
	CleanupPolicy CleanupPolicy `json:"cleanupPolicy,omitempty"`
}

// SlapdDatabaseStatus defines the observed state of SlapdDatabase.
type SlapdDatabaseStatus struct {
	// phase summarises the current lifecycle state of the database.
	// +optional
	Phase SlapdDatabasePhase `json:"phase,omitempty"`
	// appliedToPods lists pod names where the database has been created and
	// is fully reconciled (ACLs, indices, replication all applied).
	// +optional
	AppliedToPods []string `json:"appliedToPods,omitempty"`
	// failedPods lists pod names where database creation or reconciliation failed.
	// +optional
	FailedPods []string `json:"failedPods,omitempty"`
	// seedApplied indicates whether the seed data has been successfully applied.
	// Once true, seed data is never re-applied.
	// +optional
	SeedApplied bool `json:"seedApplied,omitempty"`
	// restoreApplied indicates whether the bootstrapFrom restore has completed
	// successfully. Once true, the restore is never re-run. Analogous to
	// seedApplied (ADR-012 one-shot discipline).
	// +optional
	RestoreApplied bool `json:"restoreApplied,omitempty"`
	// dataObserved records that the suffix's root entry has at some point been
	// seen on at least one pod of this database. A one-way latch, set by
	// positive evidence only and never cleared.
	//
	// It exists so the DataPresent condition can tell "data has not arrived
	// yet" from "data was lost" on databases that are never seeded — a
	// non-founder site of a mesh (ADR-025 decision 1 tells peers to omit
	// spec.seed), a hot-migration cluster fed by the legacy provider
	// (ADR-011), or a consumer-only cluster (ADR-010). For seeded and restored
	// databases seedApplied and restoreApplied already answer that question.
	//
	// Observability only (ADR-012): the reconciler never reads this to decide
	// whether to write anything — in particular it is NOT an input to seeding.
	// +optional
	DataObserved bool `json:"dataObserved,omitempty"`
	// observedGeneration is the .metadata.generation the controller last reconciled.
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
// +kubebuilder:resource:scope=Namespaced,shortName=sd
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterRef`
// +kubebuilder:printcolumn:name="Suffix",type=string,JSONPath=`.spec.suffix`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Cleanup",type=string,JSONPath=`.spec.cleanupPolicy`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SlapdDatabase is the Schema for the slapddatabases API.
// Each instance represents one MDB backend database within a SlapdCluster.
// The controller creates the database via ldapmodify on each pod's cn=config,
// then manages ACLs, indices, replication, and seed data per-database.
// See ADR-004 for the multi-resource architecture and ADR-005 for cleanup policy.
type SlapdDatabase struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired database configuration.
	// +required
	Spec SlapdDatabaseSpec `json:"spec"`

	// status defines the observed state of the database.
	// +optional
	Status SlapdDatabaseStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SlapdDatabaseList contains a list of SlapdDatabase.
type SlapdDatabaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SlapdDatabase `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SlapdDatabase{}, &SlapdDatabaseList{})
}

// ReplicationEnabled reports whether this database participates in cluster
// replication. Treats an unset (nil) Enabled as the documented default (true).
// Independent of the parent cluster's replication.enabled — callers gate on
// both when deciding whether to perform replication work.
func (sd *SlapdDatabase) ReplicationEnabled() bool {
	if sd.Spec.Replication.Enabled == nil {
		return true
	}
	return *sd.Spec.Replication.Enabled
}

// DeltaSyncEnabled reports whether delta-syncrepl should be used for this
// database, treating an unset (nil) DeltaSync as the documented default
// (true). Returns false when the database is excluded from replication
// (ReplicationEnabled()==false) so callers that gate accesslog overlay setup
// on this helper don't bootstrap an accesslog DB on an excluded database.
func (sd *SlapdDatabase) DeltaSyncEnabled() bool {
	if !sd.ReplicationEnabled() {
		return false
	}
	if sd.Spec.Replication.DeltaSync == nil {
		return true
	}
	return *sd.Spec.Replication.DeltaSync
}
