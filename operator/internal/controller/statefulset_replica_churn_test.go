/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// Scaling a StatefulSet must not roll the pods that already exist.
//
// `spec.replicas` is a StatefulSet field, not a pod-template field, so changing
// it creates or removes a pod and leaves the rest alone — UNLESS the operator
// also lets the number leak into the pod template, whose hash is what the
// StatefulSet controller rolls on. Then a scale-up silently becomes a full
// fleet restart, and on a mesh each restarted pod costs every remote peer a
// syncrepl address rediscovery (ADR-016: 130-268 s measured).
//
// The boundary this does NOT assert is 1 <-> 2: NeedsAccesslogVolume() flips
// there, which legitimately changes the volumes and mounts. Above it, the
// template is a function of everything except the count.
func TestPodTemplateDoesNotDependOnReplicaCount(t *testing.T) {
	build := func(replicas int32) corev1.PodTemplateSpec {
		r := &SlapdClusterReconciler{ClusterDomain: testClusterDomain}
		sc := &ldapv1alpha1.SlapdCluster{}
		sc.Name = testClusterName
		sc.Namespace = testNamespace
		sc.Spec.Replicas = replicas
		sc.Spec.ReadReplicas = 1
		sc.Spec.Replication.Enabled = true
		sc.Spec.LDAP.TLS.Enabled = true
		sc.Spec.LDAP.TLS.SecretName = testTLSSecretName
		return r.buildStatefulSetSpec(sc, false).Template
	}

	for _, tc := range []struct{ from, to int32 }{{3, 4}, {2, 5}, {4, 3}} {
		a, b := build(tc.from), build(tc.to)
		if reflect.DeepEqual(a, b) {
			continue
		}
		// Name the offending env var rather than dumping two pod specs.
		for _, c := range [][2][]corev1.Container{
			{a.Spec.InitContainers, b.Spec.InitContainers},
			{a.Spec.Containers, b.Spec.Containers},
		} {
			for i := range c[0] {
				if i >= len(c[1]) {
					break
				}
				for j, e := range c[0][i].Env {
					if j < len(c[1][i].Env) && c[1][i].Env[j].Value != e.Value {
						t.Errorf("replicas %d -> %d changes the pod template: container %q env %q = %q vs %q",
							tc.from, tc.to, c[0][i].Name, e.Name, e.Value, c[1][i].Env[j].Value)
					}
				}
			}
		}
		t.Errorf("replicas %d -> %d changed the pod template; the StatefulSet will roll every existing pod",
			tc.from, tc.to)
	}
}
