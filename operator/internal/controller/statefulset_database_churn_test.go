/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// Adding or removing a SlapdDatabase must not roll the fleet.
//
// The sibling of TestPodTemplateDoesNotDependOnReplicaCount, for the last
// pod-template roll trigger ADR-013 sanctioned and its 2026-09-28 amendment
// retires. slapd needs no restart to gain or lose a database — it is a live
// ldap-add against cn=config — but the directory list rode into the init
// container's env as DATABASE_DIRS, so the template hash moved with the
// database set and the StatefulSet controller replaced every pod. On a mesh
// that is one syncrepl address rediscovery per pod per peer (ADR-016:
// 130-268 s measured), for an operation that touches no pod at all.
//
// buildStatefulSetSpec no longer takes the database set at all, which makes
// the invariant structural rather than asserted — so what is left to guard is
// the way back in: a re-added env var, or a ConfigMap projected by content
// instead of referenced by name. Both would restore the roll while every other
// test stayed green.
func TestPodTemplateCarriesNoDatabaseSet(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		r := &SlapdClusterReconciler{ClusterDomain: testClusterDomain}
		sc := &ldapv1alpha1.SlapdCluster{}
		sc.Name = testClusterName
		sc.Namespace = testNamespace
		sc.Spec.Replicas = 3
		sc.Spec.ReadReplicas = 1
		sc.Spec.Replication.Enabled = true
		sc.Spec.LDAP.TLS.Enabled = true
		sc.Spec.LDAP.TLS.SecretName = testTLSSecretName
		tmpl := r.buildStatefulSetSpec(sc, readOnly).Template

		for _, c := range append(
			append([]corev1.Container{}, tmpl.Spec.InitContainers...),
			tmpl.Spec.Containers...,
		) {
			for _, e := range c.Env {
				if e.Name == "DATABASE_DIRS" {
					t.Errorf("readOnly=%v: container %q carries DATABASE_DIRS=%q again; "+
						"a database add or remove will roll the whole fleet (ADR-013 amendment)",
						readOnly, c.Name, e.Value)
				}
				if strings.Contains(e.Value, testDBName) {
					t.Errorf("readOnly=%v: container %q env %q leaks a database name into the pod template",
						readOnly, c.Name, e.Name)
				}
			}
		}

		// Referenced by name — the content lives in the ConfigMap, so changing
		// it leaves the template's hash alone. An inline projection here would
		// look identical from the pod's side and roll on every change.
		var found bool
		for _, v := range tmpl.Spec.Volumes {
			if v.Name != databaseDirsVolume {
				continue
			}
			found = true
			if v.ConfigMap == nil {
				t.Fatalf("readOnly=%v: %s is not a ConfigMap volume", readOnly, databaseDirsVolume)
			}
			if got, want := v.ConfigMap.Name, databaseDirsConfigMapName(sc.Name); got != want {
				t.Errorf("readOnly=%v: %s references ConfigMap %q, want %q", readOnly, databaseDirsVolume, got, want)
			}
			if len(v.ConfigMap.Items) > 0 {
				t.Errorf("readOnly=%v: %s projects explicit items; the key set would then be part of the template",
					readOnly, databaseDirsVolume)
			}
			if v.ConfigMap.Optional == nil || !*v.ConfigMap.Optional {
				t.Errorf("readOnly=%v: %s must be optional — a cluster with no databases yet has no ConfigMap, "+
					"and its pods must still start", readOnly, databaseDirsVolume)
			}
		}
		if !found {
			t.Errorf("readOnly=%v: pod template has no %s volume, so the init container cannot read the lists",
				readOnly, databaseDirsVolume)
		}
	}
}
