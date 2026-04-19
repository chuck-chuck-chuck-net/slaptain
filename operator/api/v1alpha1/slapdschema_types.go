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

// SlapdSchemaSpec defines the desired state of SlapdSchema.
type SlapdSchemaSpec struct {
	// clusterRef is the name of the SlapdCluster this schema belongs to.
	// Must be in the same namespace.
	// +required
	ClusterRef string `json:"clusterRef"`
	// priority controls the order in which schemas are applied. Lower values
	// are applied first. Built-in schemas (core, cosine, nis, inetorgperson)
	// are loaded by the init container at priority 0-30. Custom schemas
	// should use values >= 100.
	// +kubebuilder:default=100
	// +kubebuilder:validation:Minimum=0
	Priority int32 `json:"priority,omitempty"`
	// attributeTypes is a list of attributeType definitions in RFC 4512 syntax.
	// Each string is a complete attributeType definition including the
	// surrounding parentheses.
	// Example: "( 1.3.6.1.4.1.99999.1.1.1 NAME 'myAttr' EQUALITY caseIgnoreMatch SYNTAX 1.3.6.1.4.1.1466.115.121.1.15 )"
	// +optional
	AttributeTypes []string `json:"attributeTypes,omitempty"`
	// objectClasses is a list of objectClass definitions in RFC 4512 syntax.
	// Each string is a complete objectClass definition including the
	// surrounding parentheses. Referenced attributeTypes must exist (either
	// built-in or declared in this or a lower-priority SlapdSchema).
	// Example: "( 1.3.6.1.4.1.99999.1.2.1 NAME 'myClass' SUP top STRUCTURAL MUST cn MAY myAttr )"
	// +optional
	ObjectClasses []string `json:"objectClasses,omitempty"`
}

// SlapdSchemaStatus defines the observed state of SlapdSchema.
type SlapdSchemaStatus struct {
	// applied indicates whether all declared schema elements exist on all
	// reachable pods in the referenced cluster.
	// +optional
	Applied bool `json:"applied,omitempty"`
	// appliedToPods lists pod names where all schema elements are present.
	// +optional
	AppliedToPods []string `json:"appliedToPods,omitempty"`
	// failedPods lists pod names where schema application failed.
	// +optional
	FailedPods []string `json:"failedPods,omitempty"`
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
// +kubebuilder:resource:scope=Namespaced,shortName=ss
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterRef`
// +kubebuilder:printcolumn:name="Priority",type=integer,JSONPath=`.spec.priority`
// +kubebuilder:printcolumn:name="Applied",type=boolean,JSONPath=`.status.applied`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SlapdSchema is the Schema for the slapdschemas API.
// It declares attributeTypes and objectClasses that must exist in
// cn=schema,cn=config on every pod of the referenced SlapdCluster.
// Schemas are global — visible to all databases on the slapd process.
// See ADR-006 for lifecycle semantics (additive-only, desired-minimum model).
type SlapdSchema struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired schema elements.
	// +required
	Spec SlapdSchemaSpec `json:"spec"`

	// status defines the observed state of the schema.
	// +optional
	Status SlapdSchemaStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SlapdSchemaList contains a list of SlapdSchema.
type SlapdSchemaList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SlapdSchema `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SlapdSchema{}, &SlapdSchemaList{})
}
