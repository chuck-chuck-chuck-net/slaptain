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

package controller

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// applyConfiguration adapts a typed API object to the runtime.ApplyConfiguration
// that client.Client.Apply() and SubResource("status").Apply() take, replacing the
// deprecated client.Apply patch type.
//
// The object must carry its TypeMeta: the apply path resolves the REST mapping
// from apiVersion/kind in the payload, not from the scheme.
//
// Why unstructured and not a generated apply configuration: controller-runtime's
// new API wants k8s.io/client-go/applyconfigurations types, which exist for the
// core kinds but not for our CRDs, and adopting them for the StatefulSet would
// mean rewriting every builder. Both paths ultimately send json.Marshal of the
// payload as an application/apply-patch+yaml body, and the unstructured form
// marshals byte-for-byte the same JSON as the typed object it was converted from
// — apply_test.go pins that. So this is a call-shape migration, not a change to
// what the API server records for our field manager.
//
// The caveat in client.ApplyConfigurationFromUnstructured's doc — an unstructured
// object built from an API object cannot distinguish "unset" from "explicit zero"
// — is inherent to server-side-applying typed structs and applied equally to the
// deprecated client.Apply path this replaces. Nothing about it changed here.
func applyConfiguration(obj runtime.Object) (runtime.ApplyConfiguration, error) {
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, fmt.Errorf("convert %T to unstructured for apply: %w", obj, err)
	}
	return client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{Object: u}), nil
}
