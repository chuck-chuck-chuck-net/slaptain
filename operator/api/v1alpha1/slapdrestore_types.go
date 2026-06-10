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

// SlapdRestorePhase is the lifecycle phase of an imperative in-place restore.
type SlapdRestorePhase string

const (
	// RestoreRequestPending: accepted, waiting to be picked up (e.g. another
	// restore is in progress on the cluster, or the operator image is unset).
	RestoreRequestPending SlapdRestorePhase = "Pending"
	// RestoreRequestPreflight: the source is being validated; the cluster is
	// still serving (nothing wiped yet).
	RestoreRequestPreflight SlapdRestorePhase = "Preflight"
	// RestoreRequestRestoring: the cluster is scaled to 0 and slapadd is loading
	// the artifact into every pod.
	RestoreRequestRestoring SlapdRestorePhase = "Restoring"
	// RestoreRequestCompleted: the DIT was restored and the cluster scaled back up.
	RestoreRequestCompleted SlapdRestorePhase = "Completed"
	// RestoreRequestFailed: a restore Job failed; the cluster is held at 0
	// replicas for inspection.
	RestoreRequestFailed SlapdRestorePhase = "Failed"
)

// RestoreSource selects the backup artifact a SlapdRestore loads. Exactly one of
// backupRef or s3 must be set (the same shape as SlapdDatabase.spec.bootstrapFrom).
//
// +kubebuilder:validation:XValidation:rule="has(self.backupRef) != has(self.s3)",message="source requires exactly one of backupRef or s3."
type RestoreSource struct {
	// backupRef is the name of a SlapdBackup (same namespace) to restore from.
	// +optional
	BackupRef string `json:"backupRef,omitempty"`
	// s3 points directly at a backup artifact in object storage — for restoring
	// from a backup with no SlapdBackup object (a legacy slapcat dump or a backup
	// produced by another cluster).
	// +optional
	S3 *BootstrapS3Source `json:"s3,omitempty"`
}

// SlapdRestoreSpec defines an imperative, in-place restore of one existing
// SlapdDatabase's data tree from a backup (ADR-014 amendment). Unlike
// SlapdDatabase.spec.bootstrapFrom (which seeds a *new* database at creation), a
// SlapdRestore replaces the DIT of a live, possibly-populated database — a
// rollback ("restore yesterday's backup"). It is a command, not desired state,
// so the spec is immutable: to restore again, create another SlapdRestore.
//
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="SlapdRestore spec is immutable; create a new SlapdRestore to run another restore."
type SlapdRestoreSpec struct {
	// databaseRef is the name of the SlapdDatabase to restore into. Must be in
	// the same namespace and already exist (a SlapdRestore does not create it).
	// +required
	DatabaseRef string `json:"databaseRef"`
	// source is the backup artifact to load.
	// +required
	Source RestoreSource `json:"source"`
	// skipReplicationPasswordCheck disables the preflight verification that the
	// cluster's replication-password matches the backup's cn=replication entry.
	// The check is default-deny (mismatch or unverifiable hash scheme fails). Set
	// true only when you accept that intra-cluster syncrepl may fail to
	// authenticate after the restore and you will repair cn=replication yourself.
	// See SlapdDatabase.spec.bootstrapFrom.skipReplicationPasswordCheck.
	// +optional
	SkipReplicationPasswordCheck bool `json:"skipReplicationPasswordCheck,omitempty"`
}

// SlapdRestoreStatus defines the observed state of SlapdRestore.
type SlapdRestoreStatus struct {
	// phase summarises the restore's lifecycle state.
	// +optional
	Phase SlapdRestorePhase `json:"phase,omitempty"`
	// startedAt is when the restore was picked up (preflight began).
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// completedAt is when the restore finished (success or failure).
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// message is a human-readable status detail (e.g. the failure reason).
	// +optional
	Message string `json:"message,omitempty"`
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
// +kubebuilder:resource:scope=Namespaced,shortName=sr
// +kubebuilder:printcolumn:name="Database",type=string,JSONPath=`.spec.databaseRef`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Completed",type=date,JSONPath=`.status.completedAt`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SlapdRestore is the Schema for the slapdrestores API. Each instance is one
// imperative in-place restore of a SlapdDatabase's data tree from S3. The
// SlapdCluster controller watches it and drives the same preflight →
// slapadd-all-pods machine as bootstrapFrom (ADR-014, Architecture A).
type SlapdRestore struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the requested restore.
	// +required
	Spec SlapdRestoreSpec `json:"spec"`

	// status defines the observed state of the restore.
	// +optional
	Status SlapdRestoreStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SlapdRestoreList contains a list of SlapdRestore.
type SlapdRestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SlapdRestore `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SlapdRestore{}, &SlapdRestoreList{})
}

// IsTerminal reports whether the restore has reached a terminal phase and should
// no longer be acted on.
func (sr *SlapdRestore) IsTerminal() bool {
	return sr.Status.Phase == RestoreRequestCompleted || sr.Status.Phase == RestoreRequestFailed
}
