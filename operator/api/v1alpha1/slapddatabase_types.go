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
	// RID collisions. The operator validates this.
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
	// syncprovCheckpoint sets the syncprov overlay checkpoint interval.
	// Format: "<ops> <minutes>", e.g. "500 15" means checkpoint every 500 operations
	// or 15 minutes. Empty means no explicit checkpoint (OpenLDAP default).
	// +optional
	SyncprovCheckpoint string `json:"syncprovCheckpoint,omitempty"`
	// accesslogPurge sets the accesslog purge interval (only used when deltaSync=true).
	// Format: "<maxage> <interval>", e.g. "2+00:00 1+00:00" means purge entries older
	// than 2 days, checking every day. Empty means no purge (accesslog grows unbounded).
	// +optional
	AccesslogPurge string `json:"accesslogPurge,omitempty"`
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
}

// SlapdDatabaseSpec defines the desired state of SlapdDatabase.
//
// +kubebuilder:validation:XValidation:rule="self.replication.enabled == false || has(self.replication.ridBase)",message="spec.replication.ridBase is required when replication.enabled=true (the default). Set replication.enabled=false to explicitly exclude this database from cluster replication."
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
	// maxSize is the maximum database size (olcDbMaxSize) in bytes. Accepts
	// standard Kubernetes quantity format (e.g. "1Gi", "32Gi").
	// +optional
	MaxSize string `json:"maxSize,omitempty"`
	// noSync disables fsync after each write (olcDbNoSync). Improves write
	// performance at the cost of durability on unclean shutdown. Replicas
	// provide redundancy. Default false (fsync enabled).
	// +kubebuilder:default=false
	NoSync bool `json:"noSync,omitempty"`
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
