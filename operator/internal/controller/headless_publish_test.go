/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// A headless Service must publish NotReady addresses.
//
// Everything the operator does per pod resolves
// <name>-<ordinal>.<name>-headless.<ns>.svc, and a NotReady pod drops out of a
// headless Service's DNS by default. So the moment slapd gains a readiness
// probe, an unhealthy pod becomes an UNREACHABLE pod: the operator cannot dial
// it, cannot converge its node-local cn=config (ADR-002), and ADR-027's
// identity gate then withholds syncrepl for the whole site — the cluster's
// response to one sick pod would be to stop configuring all of them.
//
// Syncrepl peers address each other through the same names, and a consumer
// whose provider vanishes from DNS is worse off than one whose provider is
// merely slow: it cannot even retry.
//
// The client-facing ClusterIP Service is deliberately NOT included. That one
// SHOULD gate on readiness — it is how a frozen pod stops receiving client
// traffic, which is the entire point of having a probe.
//
// Landed on its own, BEFORE any probe exists, because today nothing is ever
// NotReady and this is therefore a literal no-op: shipping it together with the
// probe is how the deadlock above gets discovered the hard way. ADR-013 named
// this fix in May 2026 and it never shipped, because the probe it was written
// for never shipped.
func TestHeadlessServicesPublishNotReadyAddresses(t *testing.T) {
	r := &SlapdClusterReconciler{ClusterDomain: testClusterDomain}
	sc := &ldapv1alpha1.SlapdCluster{}
	sc.Name = testClusterName
	sc.Namespace = testNamespace
	sc.Spec.Replicas = 3
	sc.Spec.ReadReplicas = 1

	for _, tc := range []struct {
		name    string
		build   func() *corev1.Service
		want    bool
		because string
	}{
		{
			name:    testHeadlessSvc,
			build:   func() *corev1.Service { return r.headlessService(sc) },
			want:    true,
			because: "the operator dials RW pods through it, and so do their peers",
		},
		{
			name:    "slapd-readonly-headless",
			build:   func() *corev1.Service { return r.readOnlyHeadlessService(sc) },
			want:    true,
			because: "an RO pod is converged through it too, and its initial sync makes NotReady legitimately transient",
		},
		{
			name:    "slapd (ClusterIP)",
			build:   func() *corev1.Service { return r.clusterIPService(sc) },
			want:    false,
			because: "client traffic MUST stop at an unready pod; that is what the probe is for",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.build().Spec.PublishNotReadyAddresses
			if got != tc.want {
				t.Errorf("publishNotReadyAddresses = %v, want %v — %s", got, tc.want, tc.because)
			}
		})
	}
}
