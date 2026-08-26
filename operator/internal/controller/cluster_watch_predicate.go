/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// slapdClusterChangeMatters reports whether an update to a SlapdCluster changes
// anything the SlapdDatabase controller actually consumes.
//
// Why this exists: the SlapdCluster CSN monitor rewrites LastChecked (and often
// ReplicationState / LagSeconds) on every monitoring interval, which bumps
// resourceVersion. The SlapdDatabase watch had no predicate, so every one of
// those writes enqueued EVERY SlapdDatabase in the namespace, and each of those
// reconciles dials and binds LDAP on every pod of the cluster. The steady-state
// cost of merely observing replication was therefore N_databases × N_pods LDAP
// sessions per interval, forever, with nothing to converge.
//
// WHAT IT KEYS ON, and why that is the complete set. Produced by sweeping every
// `sc.` read in slapddatabase_controller.go and following the cluster into every
// helper it is passed to (the syncrepl builders take primitives only, so nothing
// leaks past them):
//
//	metadata.generation  — proxy for ANY spec change. The API server bumps it on
//	                       every spec write, so no spec field needs enumerating
//	                       here; that is what makes this predicate safe to
//	                       maintain. spec.suspend, replicas, readReplicas,
//	                       ldap.*, replication.* and every externalPeers[] field
//	                       ride on it.
//	status.phase         — hard gate: the controller refuses to reconcile a
//	                       cluster that is not Running.
//	status.replicationNetworkIPs        — substituted into in-cluster syncrepl
//	                       stanzas under ADR-007 useForInCluster.
//	status.externalPeerStatuses[].name  — matched against spec peers.
//	status.externalPeerStatuses[].discoveredAddresses — the peer addresses the
//	                       stanza builder is handed (ADR-007 amendment / ADR-016).
//
// Everything else in status is deliberately filtered: readyReplicas, replicas,
// observedGeneration, conditions, restore, and the CSN-monitoring subfields of
// each peer status (replicationState, lagSeconds, lastChecked, lastError,
// connected). The controller reads none of them — verified, not assumed: there
// are exactly three `sc.Status.` reads in that file.
//
// IF YOU ADD A READ OF A NEW CLUSTER FIELD TO THE SlapdDatabase CONTROLLER, ADD
// IT HERE AND ADD A CASE TO TestSlapdClusterChangeMatters. An over-filtered
// watch does not fail loudly: stanzas, ACLs, overlays or serverIDs simply stop
// converging, and the cluster keeps reporting itself healthy.
//
// Create and Delete are not filtered (the mapper handles them); this governs
// Update only, which is where the churn is.
func slapdClusterChangeMatters(old, new *ldapv1alpha1.SlapdCluster) bool {
	if old == nil || new == nil {
		return true // cannot compare; be conservative
	}
	if old.Generation != new.Generation {
		return true
	}
	if old.Status.Phase != new.Status.Phase {
		return true
	}
	if !stringMapsEqual(old.Status.ReplicationNetworkIPs, new.Status.ReplicationNetworkIPs) {
		return true
	}
	return !peerAddressProjectionsEqual(old.Status.ExternalPeerStatuses, new.Status.ExternalPeerStatuses)
}

// peerAddressProjectionsEqual compares the (name, discoveredAddresses) pairs of
// two peer-status lists — the only projection the SlapdDatabase controller reads.
// Order-sensitive on both axes: the controller looks peers up by name, and
// discoverRemotePeerAddresses already sorts the addresses, so any difference in
// either is a real change rather than a re-ordering artefact.
func peerAddressProjectionsEqual(a, b []ldapv1alpha1.ExternalPeerStatus) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name {
			return false
		}
		if !stringSlicesEqual(a[i].DiscoveredAddresses, b[i].DiscoveredAddresses) {
			return false
		}
	}
	return true
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stringMapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
