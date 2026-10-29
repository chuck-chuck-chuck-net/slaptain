/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"fmt"
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// rwTemplate builds the read-write pod template for a given replica count and
// replication setting — the two spec fields a user changes when a standalone
// cluster becomes an HA one.
func rwTemplate(replicas int32, replication bool) corev1.PodTemplateSpec {
	r := &SlapdClusterReconciler{ClusterDomain: testClusterDomain}
	sc := &ldapv1alpha1.SlapdCluster{}
	sc.Name = testClusterName
	sc.Namespace = testNamespace
	sc.Spec.Replicas = replicas
	sc.Spec.ReadReplicas = 1
	sc.Spec.Replication.Enabled = replication
	sc.Spec.LDAP.TLS.Enabled = true
	sc.Spec.LDAP.TLS.SecretName = testTLSSecretName
	return r.buildStatefulSetSpec(sc, false, "").Template
}

// templateDiff names every pod-template difference between two builds in terms
// the reader can act on — which container, which env var, which mount — rather
// than dumping two pod specs and leaving them to diff it by eye. Keyed by name,
// not by index, so an inserted env var reads as one addition and not as every
// subsequent position changing.
func templateDiff(a, b corev1.PodTemplateSpec) []string {
	var diffs []string

	byName := func(cs []corev1.Container) map[string]corev1.Container {
		m := make(map[string]corev1.Container, len(cs))
		for _, c := range cs {
			m[c.Name] = c
		}
		return m
	}
	// keyDiff reports entries present in one map and not the other, plus
	// entries whose values disagree.
	keyDiff := func(what, container string, x, y map[string]string) {
		seen := make(map[string]bool, len(x)+len(y))
		keys := make([]string, 0, len(x)+len(y))
		for _, m := range []map[string]string{x, y} {
			for k := range m {
				if !seen[k] {
					seen[k] = true
					keys = append(keys, k)
				}
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			xv, inX := x[k]
			yv, inY := y[k]
			switch {
			case inX && !inY:
				diffs = append(diffs, fmt.Sprintf("container %q: %s %q present in A, absent in B", container, what, k))
			case !inX && inY:
				diffs = append(diffs, fmt.Sprintf("container %q: %s %q absent in A, present in B", container, what, k))
			case xv != yv:
				diffs = append(diffs, fmt.Sprintf("container %q: %s %q = %q in A, %q in B", container, what, k, xv, yv))
			}
		}
	}

	envMap := func(c corev1.Container) map[string]string {
		m := make(map[string]string, len(c.Env))
		for _, e := range c.Env {
			// A valueFrom env var has no literal value; its source is what
			// identifies it, and String() is stable enough to compare.
			if e.ValueFrom != nil {
				m[e.Name] = fmt.Sprintf("%v", e.ValueFrom)
				continue
			}
			m[e.Name] = e.Value
		}
		return m
	}
	mountMap := func(c corev1.Container) map[string]string {
		m := make(map[string]string, len(c.VolumeMounts))
		for _, v := range c.VolumeMounts {
			m[v.Name] = fmt.Sprintf("%s(subPath=%q,ro=%v)", v.MountPath, v.SubPath, v.ReadOnly)
		}
		return m
	}
	volumeMap := func(p corev1.PodTemplateSpec) map[string]string {
		m := make(map[string]string, len(p.Spec.Volumes))
		for _, v := range p.Spec.Volumes {
			m[v.Name] = fmt.Sprintf("%v", v.VolumeSource)
		}
		return m
	}

	// Pod-level placement (ADR-043). Everything else here is keyed on a
	// container or a volume, so a placement field that silently varied with
	// the replica count — a generated spread constraint that learned to read
	// spec.replicas, say — would satisfy every assertion in this file and be
	// caught only by the golden templates, which render ONE fixed cluster and
	// so cannot see a dependency on a number at all.
	placementMap := func(p corev1.PodTemplateSpec) map[string]string {
		return map[string]string{
			"nodeSelector":              fmt.Sprintf("%v", p.Spec.NodeSelector),
			"affinity":                  fmt.Sprintf("%v", p.Spec.Affinity),
			"tolerations":               fmt.Sprintf("%v", p.Spec.Tolerations),
			"priorityClassName":         p.Spec.PriorityClassName,
			"topologySpreadConstraints": fmt.Sprintf("%v", p.Spec.TopologySpreadConstraints),
		}
	}

	keyDiff("volume", "<pod>", volumeMap(a), volumeMap(b))
	keyDiff("placement", "<pod>", placementMap(a), placementMap(b))

	for _, set := range []struct{ x, y []corev1.Container }{
		{a.Spec.InitContainers, b.Spec.InitContainers},
		{a.Spec.Containers, b.Spec.Containers},
	} {
		xs, ys := byName(set.x), byName(set.y)
		names := make([]string, 0, len(xs))
		for n := range xs {
			names = append(names, n)
		}
		for n := range ys {
			if _, ok := xs[n]; !ok {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		for _, n := range names {
			xc, inX := xs[n]
			yc, inY := ys[n]
			if !inX || !inY {
				diffs = append(diffs, fmt.Sprintf("container %q exists in only one template", n))
				continue
			}
			keyDiff("env", n, envMap(xc), envMap(yc))
			keyDiff("mount", n, mountMap(xc), mountMap(yc))
			if xc.Image != yc.Image {
				diffs = append(diffs, fmt.Sprintf("container %q: image %q vs %q", n, xc.Image, yc.Image))
			}
			if fmt.Sprintf("%v", xc.Command) != fmt.Sprintf("%v", yc.Command) ||
				fmt.Sprintf("%v", xc.Args) != fmt.Sprintf("%v", yc.Args) {
				diffs = append(diffs, fmt.Sprintf("container %q: command/args differ", n))
			}
		}
	}
	return diffs
}

// Scaling a StatefulSet must not roll the pods that already exist.
//
// `spec.replicas` is a StatefulSet field, not a pod-template field, so changing
// it creates or removes a pod and leaves the rest alone — UNLESS the operator
// also lets the number leak into the pod template, whose hash is what the
// StatefulSet controller rolls on. Then a scale-up silently becomes a full
// fleet restart, and on a mesh each restarted pod costs every remote peer a
// syncrepl address rediscovery (ADR-016: 130-268 s measured).
//
// The 1 <-> 2 boundary is included deliberately. It used to be exempted, on the
// grounds that NeedsAccesslogVolume() flips there and "legitimately" changes the
// mounts — but ADR-013's amendment puts the count of sanctioned roll triggers at
// zero, and a user scaling a standalone cluster to HA has no way to read a pod
// replacement as legitimate. Observed live: pod-0 came back a minute younger
// than pods 1 and 2 after a 1 -> 3 scale-up, restartCount 0, with the cluster
// reporting Running then Degraded then Running.
func TestPodTemplateDoesNotDependOnReplicaCount(t *testing.T) {
	for _, tc := range []struct{ from, to int32 }{{3, 4}, {2, 5}, {4, 3}, {1, 2}, {1, 3}, {2, 1}} {
		diffs := templateDiff(rwTemplate(tc.from, true), rwTemplate(tc.to, true))
		for _, d := range diffs {
			t.Errorf("replicas %d -> %d changes the pod template: %s", tc.from, tc.to, d)
		}
		if len(diffs) > 0 {
			t.Errorf("replicas %d -> %d changed the pod template; the StatefulSet will roll every existing pod",
				tc.from, tc.to)
		}
	}
}

// Turning replication on must not roll the pods that already exist either.
//
// It is the same defect as the replica-count leak and it fires on the same
// apply: a user going from a standalone cluster to an HA one raises
// spec.replicas AND sets spec.replication.enabled in one edit. Everything
// replication needs at pod level — the accesslog mount, the module-loading env
// — is cheap enough to carry unconditionally, and the accesslog PVC already is
// (volumeClaimTemplates being immutable forced that hand first). What is left
// must not be allowed to change the template hash.
//
// The read-only template is not covered here because it cannot regress: an RO
// pod gets LDAP_REPLICATION_ENABLED=true unconditionally and never mounts the
// accesslog, and an RO StatefulSet does not exist at all until replication is on.
func TestPodTemplateDoesNotDependOnReplicationEnabled(t *testing.T) {
	for _, replicas := range []int32{1, 2, 3} {
		diffs := templateDiff(rwTemplate(replicas, false), rwTemplate(replicas, true))
		for _, d := range diffs {
			t.Errorf("replicas=%d: enabling replication changes the pod template: %s", replicas, d)
		}
		if len(diffs) > 0 {
			t.Errorf("replicas=%d: enabling replication changed the pod template; the StatefulSet will roll every existing pod",
				replicas)
		}
	}
}
