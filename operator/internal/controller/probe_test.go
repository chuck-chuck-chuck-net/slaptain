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

// "Ready" must mean slapd answers LDAP (ADR-030).
//
// Without a probe it meant only that the process exists. A slapd frozen for
// 874 s — accepting TCP, answering nothing, logging nothing — held
// readyReplicas: 3, phase=Running and its place in the Service endpoints for
// the whole freeze. Every wait built on readyReplicas, in the demo and in the
// e2e suite, inherited that blindness.
func TestSlapdContainerProbes(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		r := &SlapdClusterReconciler{ClusterDomain: "cluster.local"}
		sc := &ldapv1alpha1.SlapdCluster{}
		sc.Name = "slapd"
		sc.Namespace = "slaptain"
		sc.Spec.Replicas = 3
		sc.Spec.ReadReplicas = 1
		sc.Spec.Replication.Enabled = true
		sc.Spec.LDAP.TLS.Enabled = true
		sc.Spec.LDAP.TLS.SecretName = "slapd-tls"
		tmpl := r.buildStatefulSetSpec(sc, readOnly).Template

		var slapd *corev1.Container
		for i := range tmpl.Spec.Containers {
			if tmpl.Spec.Containers[i].Name == slapdContainerName {
				slapd = &tmpl.Spec.Containers[i]
			}
		}
		if slapd == nil {
			t.Fatalf("readOnly=%v: no slapd container", readOnly)
		}

		// An RO pod serves clients too, and its readiness is what keeps a
		// still-syncing replica out of the read Service. Same probe, no
		// exceptions.
		if slapd.ReadinessProbe == nil {
			t.Fatalf("readOnly=%v: slapd has no readinessProbe — readyReplicas means only that the process exists", readOnly)
		}
		if slapd.StartupProbe == nil {
			t.Fatalf("readOnly=%v: slapd has no startupProbe — a slow LMDB open would flap readiness instead of being waited out", readOnly)
		}

		// NO liveness probe, deliberately. Restarting a pod mid-freeze
		// invalidates every peer's syncrepl addresses under ADR-016 (130-268 s
		// measured), turning a recoverable stall into a topology event at every
		// site. A frozen pod must leave the Service, not the cluster.
		if slapd.LivenessProbe != nil {
			t.Errorf("readOnly=%v: slapd has a livenessProbe; ADR-030 rejects one — a restart is an ADR-016 topology event", readOnly)
		}

		for name, p := range map[string]*corev1.Probe{
			"readinessProbe": slapd.ReadinessProbe,
			"startupProbe":   slapd.StartupProbe,
		} {
			// Exec, because kubelet offers HTTPGet, TCPSocket or Exec: slapd
			// speaks no HTTP, and TCPSocket is PROVEN insufficient — the frozen
			// pod accepted connections throughout.
			if p.Exec == nil {
				t.Errorf("readOnly=%v: %s is not an exec probe; TCPSocket cannot see a frozen slapd", readOnly, name)
				continue
			}
			cmd := strings.Join(p.Exec.Command, " ")
			if !strings.Contains(cmd, "ldapwhoami") {
				t.Errorf("readOnly=%v: %s runs %q; it must be ldapwhoami — WhoAmI reads no entry, so no ACL can break it", readOnly, name, cmd)
			}
			if !strings.Contains(cmd, "-x") {
				t.Errorf("readOnly=%v: %s must bind anonymously (-x); a probe carrying credentials is a credential in the pod spec", readOnly, name)
			}
			if p.TimeoutSeconds <= 0 {
				t.Errorf("readOnly=%v: %s has no timeout, so a frozen slapd blocks the probe forever instead of failing it", readOnly, name)
			}
		}

		// The whole design question: a threshold well above a normal
		// sub-second cn=config pause, so the operator's own writes never flap
		// pods out of the Service.
		rp := slapd.ReadinessProbe
		if budget := rp.PeriodSeconds * rp.FailureThreshold; budget < 60 {
			t.Errorf("readOnly=%v: readiness gives up after %ds; a legitimate cn=config pause would evict a healthy pod",
				readOnly, budget)
		}

		// Startup must be generous: under OrderedReady a pod-0 that never goes
		// Ready means pod-1 is never CREATED, and the operator cannot help —
		// there is no pod to converge.
		sp := slapd.StartupProbe
		if budget := sp.PeriodSeconds * sp.FailureThreshold; budget < 300 {
			t.Errorf("readOnly=%v: startup gives up after %ds; on a production-sized LMDB that is a cluster that never forms",
				readOnly, budget)
		}

		// Probes belong to the server, not to bootstrap: an init container runs
		// to completion and kubelet does not probe it.
		for _, c := range tmpl.Spec.InitContainers {
			if c.ReadinessProbe != nil || c.LivenessProbe != nil || c.StartupProbe != nil {
				t.Errorf("readOnly=%v: init container %q carries a probe", readOnly, c.Name)
			}
		}
	}
}
