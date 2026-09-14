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
	"encoding/json"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// TestApplyConfigurationBodyIsUnchanged pins the one thing the migration off the
// deprecated client.Apply patch type must not change: the bytes on the wire.
//
// Both the old and the new path send json.Marshal of the payload as an
// application/apply-patch+yaml body — the old one marshalled the typed object
// directly, the new one marshals the unstructured map applyConfiguration()
// produces. If those two ever diverge, the field set the API server records for
// our field manager diverges with them, which is exactly the SSA drift ADR-001's
// idempotency guarantee rests on.
func TestApplyConfigurationBodyIsUnchanged(t *testing.T) {
	replicas := int32(3)

	objs := map[string]runtime.Object{
		"Service": &corev1.Service{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
			ObjectMeta: metav1.ObjectMeta{
				Name:        "slapd-headless",
				Namespace:   "slaptain-testing",
				Annotations: map[string]string{"a": "b"},
			},
			Spec: corev1.ServiceSpec{
				ClusterIP: corev1.ClusterIPNone,
				Selector:  map[string]string{"app.kubernetes.io/name": "slapd"},
				Ports: []corev1.ServicePort{{
					Name:       "ldap",
					Port:       1024,
					TargetPort: intstr.FromString("ldap"),
					Protocol:   corev1.ProtocolTCP,
				}},
			},
		},
		"StatefulSet": &appsv1.StatefulSet{
			TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "slapd",
				Namespace: "slaptain-testing",
			},
			Spec: appsv1.StatefulSetSpec{
				Replicas:    &replicas,
				ServiceName: "slapd-headless",
				Selector:    &metav1.LabelSelector{MatchLabels: map[string]string{"x": "y"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"x": "y"}},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  "slapd",
							Image: "example.invalid/slapd:v0",
							Ports: []corev1.ContainerPort{{Name: "ldap", ContainerPort: 1024}},
						}},
					},
				},
			},
		},
		"SlapdClusterStatus": func() *ldapv1alpha1.SlapdCluster {
			sc := &ldapv1alpha1.SlapdCluster{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "ldap.chuck-chuck-chuck.net/v1alpha1",
					Kind:       "SlapdCluster",
				},
				ObjectMeta: metav1.ObjectMeta{Name: "slapd", Namespace: "slaptain-testing"},
			}
			sc.Status.Phase = ldapv1alpha1.PhaseRunning
			sc.Status.ReadyReplicas = 3
			sc.Status.Replicas = 3
			sc.Status.Conditions = []metav1.Condition{{
				Type:               "ReplicationConverged",
				Status:             metav1.ConditionTrue,
				Reason:             "CSNsMatch",
				Message:            "all databases converged",
				LastTransitionTime: metav1.Date(2026, 9, 14, 12, 0, 0, 0, metav1.Now().Location()),
			}}
			return sc
		}(),
		"SlapdDatabaseStatus": func() *ldapv1alpha1.SlapdDatabase {
			sd := &ldapv1alpha1.SlapdDatabase{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "ldap.chuck-chuck-chuck.net/v1alpha1",
					Kind:       "SlapdDatabase",
				},
				ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "slaptain-testing"},
			}
			sd.Status.RestoreApplied = true
			return sd
		}(),
	}

	for name, obj := range objs {
		t.Run(name, func(t *testing.T) {
			want, err := json.Marshal(obj)
			if err != nil {
				t.Fatalf("marshal typed object: %v", err)
			}

			ac, err := applyConfiguration(obj)
			if err != nil {
				t.Fatalf("applyConfiguration: %v", err)
			}
			got, err := json.Marshal(ac)
			if err != nil {
				t.Fatalf("marshal apply configuration: %v", err)
			}

			// Compare as decoded JSON: key order is not part of the payload.
			var wantAny, gotAny any
			if err := json.Unmarshal(want, &wantAny); err != nil {
				t.Fatalf("unmarshal typed body: %v", err)
			}
			if err := json.Unmarshal(got, &gotAny); err != nil {
				t.Fatalf("unmarshal apply body: %v", err)
			}
			if !reflect.DeepEqual(wantAny, gotAny) {
				t.Errorf("apply body differs from the typed SSA body\n typed: %s\n apply: %s", want, got)
			}
		})
	}
}
