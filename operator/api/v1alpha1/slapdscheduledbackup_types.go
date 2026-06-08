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

// ScheduledBackupRetention defines how long emitted backups are kept. The two
// limits compose: a backup is pruned (object deleted from S3 and the
// SlapdBackup object removed) when it exceeds either limit. Both unset means
// keep everything.
type ScheduledBackupRetention struct {
	// maxCount keeps at most this many of the most recent successful backups.
	// 0 (default) means no count-based limit.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxCount int32 `json:"maxCount,omitempty"`
	// maxAge deletes backups older than this duration. Go duration string,
	// e.g. "720h" for 30 days. Empty means no age-based limit.
	// +optional
	MaxAge string `json:"maxAge,omitempty"`
}

// SlapdScheduledBackupSpec defines the desired state of SlapdScheduledBackup.
// On each cron tick the controller creates a SlapdBackup (owned by this
// resource) and enforces the retention policy. See ADR-014.
type SlapdScheduledBackupSpec struct {
	// databaseRef is the name of the SlapdDatabase to back up. Must be in the
	// same namespace.
	// +required
	DatabaseRef string `json:"databaseRef"`
	// schedule is a standard 5-field cron expression (minute hour day-of-month
	// month day-of-week) in the controller's timezone.
	// +required
	Schedule string `json:"schedule"`
	// suspend pauses scheduling without deleting the resource or its existing
	// backups. Default false.
	// +kubebuilder:default=false
	// +optional
	Suspend bool `json:"suspend,omitempty"`
	// immediate triggers one backup as soon as this resource is created, in
	// addition to the schedule. Default false.
	// +kubebuilder:default=false
	// +optional
	Immediate bool `json:"immediate,omitempty"`
	// storage configures the S3 target stamped onto each emitted SlapdBackup.
	// +required
	Storage S3StorageSpec `json:"storage"`
	// retention controls pruning of emitted backups (and their S3 objects).
	// +optional
	Retention ScheduledBackupRetention `json:"retention,omitempty"`
	// compression for emitted backups' LDIF artifacts. Only "gzip" in the MVP.
	// +kubebuilder:default=gzip
	// +kubebuilder:validation:Enum=gzip
	// +optional
	Compression string `json:"compression,omitempty"`
	// podAffinity is passed through to each emitted SlapdBackup; see
	// SlapdBackupSpec.podAffinity. Tristate (*bool); default true.
	// +kubebuilder:default=true
	// +optional
	PodAffinity *bool `json:"podAffinity,omitempty"`
}

// SlapdScheduledBackupStatus defines the observed state of SlapdScheduledBackup.
type SlapdScheduledBackupStatus struct {
	// lastScheduleTime is when the controller last created a backup from this schedule.
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`
	// nextScheduleTime is the next time a backup is due.
	// +optional
	NextScheduleTime *metav1.Time `json:"nextScheduleTime,omitempty"`
	// lastBackupRef is the name of the most recently created SlapdBackup.
	// +optional
	LastBackupRef string `json:"lastBackupRef,omitempty"`
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
// +kubebuilder:resource:scope=Namespaced,shortName=ssb
// +kubebuilder:printcolumn:name="Database",type=string,JSONPath=`.spec.databaseRef`
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Suspended",type=boolean,JSONPath=`.spec.suspend`,priority=1
// +kubebuilder:printcolumn:name="Last",type=date,JSONPath=`.status.lastScheduleTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SlapdScheduledBackup is the Schema for the slapdscheduledbackups API. Each
// instance produces SlapdBackup objects on a cron schedule and prunes old ones
// per its retention policy. See ADR-014.
type SlapdScheduledBackup struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired schedule.
	// +required
	Spec SlapdScheduledBackupSpec `json:"spec"`

	// status defines the observed state of the schedule.
	// +optional
	Status SlapdScheduledBackupStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SlapdScheduledBackupList contains a list of SlapdScheduledBackup.
type SlapdScheduledBackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SlapdScheduledBackup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SlapdScheduledBackup{}, &SlapdScheduledBackupList{})
}
