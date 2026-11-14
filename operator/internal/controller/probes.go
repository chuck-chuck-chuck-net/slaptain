/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"strconv"

	corev1 "k8s.io/api/core/v1"
)

// What "Ready" means for a slapd pod — ADR-030.
//
// It used to mean only that the process exists. A slapd frozen for 874 s —
// accepting TCP, answering nothing, logging nothing — held readyReplicas: 3,
// phase=Running and its place in the Service endpoints for the entire freeze,
// so every wait built on readyReplicas inherited that blindness.
//
// kubelet offers HTTPGet, TCPSocket or Exec. slapd speaks no HTTP; TCPSocket is
// proven insufficient by exactly the incident above; so Exec, which is why the
// runtime image carries one LDAP client.

// slapdProbeCommand asks slapd the only question a readiness probe should ask:
// does it answer at all?
//
// ldapwhoami, not ldapsearch: WhoAmI is an extended operation that reads no
// entry, so no ACL a user writes can turn it into a failure. "Does the server
// answer" stays independent of "may this identity read anything" — the
// distinction ADR-025 makes when it treats uniform invisibility as normal
// rather than as divergence.
//
// Anonymous (-x) and over plain LDAP on loopback: the probe must carry no
// credential, and it exercises the same listener the ClusterIP Service routes
// to rather than a side channel. A frozen slapd accepts the connection and then
// never answers, so the probe blocks and kubelet fails it on TimeoutSeconds —
// which is the whole point, and the reason every probe here sets one.
//
// NO liveness probe is derived from this, and that is a decision rather than an
// omission: restarting a pod mid-freeze invalidates every peer's syncrepl
// addresses under ADR-016 (130-268 s measured, per pod, per site), turning a
// recoverable stall into a topology event across the mesh. A frozen pod should
// leave the Service, not the cluster.
func slapdProbeCommand() []string {
	return []string{
		"/usr/bin/ldapwhoami",
		"-x",
		"-H", "ldap://localhost:" + strconv.Itoa(int(ldapContainerPort)),
	}
}

// slapdStartupProbe absorbs a slow start so the readiness threshold does not
// have to.
//
// Generous on purpose. Under the default OrderedReady policy a pod-0 that never
// reaches Ready means pod-1 is never CREATED, and the operator cannot help
// because there is no pod to converge — a probe that is too tight does not
// degrade the cluster, it prevents one. 5 minutes is far beyond any fixture
// (which open a few-dozen-entry LMDB in well under a second) and is a guess for
// a production-sized one: nothing in this project has ever measured that, and
// the big-DIT e2e lane that would (docs/BACKLOG.md) is still unbuilt. Tuning it
// upward later is a pod-template change and therefore a roll; tuning it upward
// on a cluster that failed to form is a support call.
func slapdStartupProbe() *corev1.Probe {
	return &corev1.Probe{
		Exec:             &corev1.ExecAction{Command: slapdProbeCommand()},
		PeriodSeconds:    5,
		TimeoutSeconds:   5,
		FailureThreshold: 60, // 300s
	}
}

// slapdReadinessProbe decides whether this pod receives client traffic.
//
// 6 failures at 10 s is a full minute of not answering before the pod leaves
// the Service. That threshold is the design question this ADR exists for: it
// must sit well above a normal cn=config write, which pauses slapd for
// sub-second periods and which the operator performs routinely — ACLs, schemas,
// syncrepl stanzas, per-pod identity convergence. A threshold near those pauses
// would have the operator evicting healthy pods as a side effect of configuring
// them. It must also sit far below the freeze this exists to catch, which ran
// 874 s. A minute is comfortably between, on evidence rather than symmetry.
func slapdReadinessProbe() *corev1.Probe {
	return &corev1.Probe{
		Exec:             &corev1.ExecAction{Command: slapdProbeCommand()},
		PeriodSeconds:    10,
		TimeoutSeconds:   5,
		FailureThreshold: 6, // 60s
		SuccessThreshold: 1,
	}
}
