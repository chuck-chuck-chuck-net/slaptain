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

// Package v1alpha1 contains API Schema definitions for the ldap v1alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=ldap.chuck-chuck-chuck.net
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "ldap.chuck-chuck-chuck.net", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &objectRegistry{}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// objectRegistry keeps the Register(objects...) call shape of controller-runtime's
// scheme.Builder, which kubebuilder scaffolds into every *_types.go, on apimachinery
// alone: an API package should not pull controller-runtime into its importers.
type objectRegistry struct {
	runtime.SchemeBuilder
}

// Register adds objects to this group-version when the scheme is built.
func (r *objectRegistry) Register(objects ...runtime.Object) {
	r.SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, objects...)
		metav1.AddToGroupVersion(s, GroupVersion)
		return nil
	})
}
