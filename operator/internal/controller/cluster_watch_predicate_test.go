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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// clusterFixture is a SlapdCluster carrying a value in every field the
// SlapdDatabase controller reads, so each case below can move exactly one.
func clusterFixture() *ldapv1alpha1.SlapdCluster {
	rpp := int32(2)
	accesslog := true
	sc := &ldapv1alpha1.SlapdCluster{}
	sc.Name = "slapd"
	sc.Namespace = "ns"
	sc.Generation = 7
	sc.Spec.Replicas = 3
	sc.Spec.ReadReplicas = 1
	sc.Spec.Suspend = false
	sc.Spec.LDAP.TLS.Enabled = true
	sc.Spec.LDAP.CnConfigCredentials.SecretName = "byo"
	sc.Spec.Replication.Enabled = true
	sc.Spec.Replication.Mode = "peer"
	sc.Spec.Replication.AccesslogEnabled = &accesslog
	sc.Spec.Replication.Retry = "10 +"
	sc.Spec.Replication.Keepalive = "300:10:60"
	sc.Spec.Replication.ServerIDBase = 0
	sc.Spec.Replication.Network = &ldapv1alpha1.ReplicationNetworkConfig{UseForInCluster: true}
	sc.Spec.Replication.ExternalPeers = []ldapv1alpha1.ExternalPeer{{
		Name:                   "siteB",
		BindDN:                 "cn=replication,dc=example,dc=org",
		BindPasswordSecretName: "peer-pw",
		TLSSecretName:          "peer-tls",
		Port:                   1025,
		SyncMode:               "delta",
		ReplicasPerPeer:        &rpp,
		PodAddresses:           []string{"10.0.0.1"},
	}}
	sc.Status.Phase = ldapv1alpha1.PhaseRunning
	sc.Status.ReplicationNetworkIPs = map[string]string{"slapd-0": "192.168.1.10"}
	checked := metav1.NewTime(time.Now())
	sc.Status.ExternalPeerStatuses = []ldapv1alpha1.ExternalPeerStatus{{
		Name:                "siteB",
		DiscoveredAddresses: []string{"10.0.0.1", "10.0.0.2"},
		ReplicationState:    ldapv1alpha1.ReplicationSynced,
		LastChecked:         &checked,
		Connected:           true,
	}}
	sc.Status.ReadyReplicas = 3
	sc.Status.Replicas = 3
	return sc
}

// Every field the SlapdDatabase controller reads must still enqueue it. The
// enumeration behind this list was produced by sweeping every sc.* read in
// slapddatabase_controller.go and every helper the cluster is passed to; if
// someone adds a read of a new cluster field, this table is where they must add
// a case, and the comment on slapdClusterChangeMatters says so.
func TestSlapdClusterChangeMatters(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ldapv1alpha1.SlapdCluster)
		want   bool
	}{
		{name: "nothing changed", mutate: func(*ldapv1alpha1.SlapdCluster) {}, want: false},

		// ── spec, via generation ────────────────────────────────────────────
		{name: "spec.replicas (generation bumps)", mutate: func(sc *ldapv1alpha1.SlapdCluster) {
			sc.Spec.Replicas = 5
			sc.Generation++
		}, want: true},
		{name: "spec.suspend (generation bumps)", mutate: func(sc *ldapv1alpha1.SlapdCluster) {
			sc.Spec.Suspend = true
			sc.Generation++
		}, want: true},
		{name: "spec.replication.externalPeers (generation bumps)", mutate: func(sc *ldapv1alpha1.SlapdCluster) {
			sc.Spec.Replication.ExternalPeers = nil
			sc.Generation++
		}, want: true},
		{name: "generation alone (any spec edit)", mutate: func(sc *ldapv1alpha1.SlapdCluster) {
			sc.Generation++
		}, want: true},

		// ── status fields the controller reads ──────────────────────────────
		{name: "status.phase", mutate: func(sc *ldapv1alpha1.SlapdCluster) {
			sc.Status.Phase = ldapv1alpha1.PhaseDegraded
		}, want: true},
		{name: "status.replicationNetworkIPs value", mutate: func(sc *ldapv1alpha1.SlapdCluster) {
			sc.Status.ReplicationNetworkIPs["slapd-0"] = "192.168.1.99"
		}, want: true},
		{name: "status.replicationNetworkIPs new pod", mutate: func(sc *ldapv1alpha1.SlapdCluster) {
			sc.Status.ReplicationNetworkIPs["slapd-2"] = "192.168.1.12"
		}, want: true},
		{name: "status.replicationNetworkIPs cleared", mutate: func(sc *ldapv1alpha1.SlapdCluster) {
			sc.Status.ReplicationNetworkIPs = nil
		}, want: true},
		{name: "status.externalPeerStatuses[].discoveredAddresses added", mutate: func(sc *ldapv1alpha1.SlapdCluster) {
			sc.Status.ExternalPeerStatuses[0].DiscoveredAddresses = []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
		}, want: true},
		{name: "status.externalPeerStatuses[].discoveredAddresses replaced", mutate: func(sc *ldapv1alpha1.SlapdCluster) {
			sc.Status.ExternalPeerStatuses[0].DiscoveredAddresses = []string{"10.0.0.1", "10.0.0.9"}
		}, want: true},
		{name: "status.externalPeerStatuses[].name", mutate: func(sc *ldapv1alpha1.SlapdCluster) {
			sc.Status.ExternalPeerStatuses[0].Name = "siteC"
		}, want: true},
		{name: "status.externalPeerStatuses peer added", mutate: func(sc *ldapv1alpha1.SlapdCluster) {
			sc.Status.ExternalPeerStatuses = append(sc.Status.ExternalPeerStatuses,
				ldapv1alpha1.ExternalPeerStatus{Name: "siteC", DiscoveredAddresses: []string{"10.1.0.1"}})
		}, want: true},
		{name: "status.externalPeerStatuses emptied", mutate: func(sc *ldapv1alpha1.SlapdCluster) {
			sc.Status.ExternalPeerStatuses = nil
		}, want: true},

		// ── status fields the controller does NOT read ──────────────────────
		// These are what the predicate exists to filter: the CSN monitor rewrites
		// them every interval, and each rewrite used to fan out to an LDAP dial
		// per pod per database.
		{name: "status.externalPeerStatuses[].lastChecked", mutate: func(sc *ldapv1alpha1.SlapdCluster) {
			t := metav1.NewTime(time.Now().Add(time.Minute))
			sc.Status.ExternalPeerStatuses[0].LastChecked = &t
		}, want: false},
		{name: "status.externalPeerStatuses[].replicationState", mutate: func(sc *ldapv1alpha1.SlapdCluster) {
			sc.Status.ExternalPeerStatuses[0].ReplicationState = ldapv1alpha1.ReplicationLagging
		}, want: false},
		{name: "status.externalPeerStatuses[].lagSeconds", mutate: func(sc *ldapv1alpha1.SlapdCluster) {
			sc.Status.ExternalPeerStatuses[0].LagSeconds = "12.5"
		}, want: false},
		{name: "status.externalPeerStatuses[].lastError", mutate: func(sc *ldapv1alpha1.SlapdCluster) {
			sc.Status.ExternalPeerStatuses[0].LastError = "transient"
		}, want: false},
		{name: "status.externalPeerStatuses[].connected", mutate: func(sc *ldapv1alpha1.SlapdCluster) {
			sc.Status.ExternalPeerStatuses[0].Connected = false
		}, want: false},
		{name: "status.readyReplicas", mutate: func(sc *ldapv1alpha1.SlapdCluster) {
			sc.Status.ReadyReplicas = 2
		}, want: false},
		{name: "status.replicas", mutate: func(sc *ldapv1alpha1.SlapdCluster) {
			sc.Status.Replicas = 2
		}, want: false},
		{name: "status.conditions", mutate: func(sc *ldapv1alpha1.SlapdCluster) {
			setCondition(&sc.Status.Conditions, metav1.Condition{
				Type: "ReplicationConverged", Status: metav1.ConditionFalse,
				Reason: "CSNsDiverged", LastTransitionTime: metav1.Now(),
			})
		}, want: false},
		{name: "status.observedGeneration", mutate: func(sc *ldapv1alpha1.SlapdCluster) {
			sc.Status.ObservedGeneration = 99
		}, want: false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old := clusterFixture()
			new := clusterFixture()
			c.mutate(new)
			if got := slapdClusterChangeMatters(old, new); got != c.want {
				t.Errorf("slapdClusterChangeMatters() = %v, want %v", got, c.want)
			}
		})
	}
}
