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

// S3StorageSpec configures an S3 (or S3-compatible) object store for backup
// artifacts. Shared by SlapdBackup, SlapdScheduledBackup, and
// SlapdDatabase.spec.bootstrapFrom (see ADR-014).
type S3StorageSpec struct {
	// bucket is the S3 bucket name.
	// +required
	Bucket string `json:"bucket"`
	// prefix is an optional key prefix within the bucket. Backup objects are
	// written under "<prefix>/<cluster>/<database>/<timestamp>.ldif.gz".
	// +optional
	Prefix string `json:"prefix,omitempty"`
	// endpoint is the S3 endpoint URL. Leave empty for AWS S3; set it for an
	// S3-compatible store (Ceph RGW, versitygw), e.g. "https://s3.example:9000".
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// region is the S3 region. Some S3-compatible stores ignore it.
	// +optional
	Region string `json:"region,omitempty"`
	// credentialsSecretName references a Secret holding the S3 credentials.
	// Required keys (plaintext): "access-key-id" and "secret-access-key".
	// (IRSA / workload-identity credentials are a future addition; the MVP is
	// static key-based only.)
	// +required
	CredentialsSecretName string `json:"credentialsSecretName"`
	// insecureTLS disables TLS certificate verification against the endpoint.
	// Use only for testing against a self-signed S3-compatible server. Default false.
	// +kubebuilder:default=false
	// +optional
	InsecureTLS bool `json:"insecureTLS,omitempty"`
}

// SlapdBackupPhase represents the lifecycle phase of a backup.
type SlapdBackupPhase string

const (
	BackupPhasePending   SlapdBackupPhase = "Pending"
	BackupPhaseRunning   SlapdBackupPhase = "Running"
	BackupPhaseCompleted SlapdBackupPhase = "Completed"
	BackupPhaseFailed    SlapdBackupPhase = "Failed"
)

// SlapdBackupSpec defines the desired state of SlapdBackup. A SlapdBackup is an
// on-demand, run-once backup of one SlapdDatabase's data tree to S3, taken via
// slapcat in a co-located Job (see ADR-014). Spec fields are effectively
// immutable once the backup has run.
type SlapdBackupSpec struct {
	// databaseRef is the name of the SlapdDatabase to back up. Must be in the
	// same namespace.
	// +required
	DatabaseRef string `json:"databaseRef"`
	// storage configures the S3 target for the backup artifact.
	// +required
	Storage S3StorageSpec `json:"storage"`
	// compression for the LDIF artifact. Only "gzip" is supported in the MVP.
	// +kubebuilder:default=gzip
	// +kubebuilder:validation:Enum=gzip
	// +optional
	Compression string `json:"compression,omitempty"`
	// podAffinity controls whether the backup Job is pinned (required
	// PodAffinity, topologyKey kubernetes.io/hostname) to the node of the target
	// slapd pod, which it must co-locate with to mount the ReadWriteOnce data
	// PVC. Default true. Set false only when the cluster forbids co-scheduling
	// Jobs onto data nodes — a node able to mount the PVC must then be available.
	//
	// Tristate (*bool): an explicit false must round-trip through the API server
	// unchanged, which a plain bool + omitempty + kubebuilder:default does not
	// guarantee.
	// +kubebuilder:default=true
	// +optional
	PodAffinity *bool `json:"podAffinity,omitempty"`
}

// SlapdBackupStatus defines the observed state of SlapdBackup.
type SlapdBackupStatus struct {
	// phase summarises the backup's lifecycle state.
	// +optional
	Phase SlapdBackupPhase `json:"phase,omitempty"`
	// jobName is the name of the Job the controller created to run the backup.
	// +optional
	JobName string `json:"jobName,omitempty"`
	// startedAt is when the backup Job started running.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// completedAt is when the backup finished (success or failure).
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// path is the S3 object key of the completed artifact.
	// +optional
	Path string `json:"path,omitempty"`
	// sizeBytes is the size of the uploaded artifact in bytes.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
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
// +kubebuilder:resource:scope=Namespaced,shortName=sb
// +kubebuilder:printcolumn:name="Database",type=string,JSONPath=`.spec.databaseRef`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Completed",type=date,JSONPath=`.status.completedAt`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SlapdBackup is the Schema for the slapdbackups API. Each instance is one
// on-demand backup of a SlapdDatabase's data tree to S3. See ADR-014.
type SlapdBackup struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired backup.
	// +required
	Spec SlapdBackupSpec `json:"spec"`

	// status defines the observed state of the backup.
	// +optional
	Status SlapdBackupStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SlapdBackupList contains a list of SlapdBackup.
type SlapdBackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SlapdBackup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SlapdBackup{}, &SlapdBackupList{})
}

// PodAffinityEnabled reports whether the backup Job should be node-pinned to the
// target slapd pod, treating an unset (nil) PodAffinity as the documented
// default (true).
func (sb *SlapdBackup) PodAffinityEnabled() bool {
	if sb.Spec.PodAffinity == nil {
		return true
	}
	return *sb.Spec.PodAffinity
}
