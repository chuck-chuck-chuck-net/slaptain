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

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// Pure decision seams for ADR-005's cleanupPolicy: Delete teardown — WHAT to
// delete in WHAT order, and on WHICH pods. The I/O that executes the plan lives
// in slapddatabase_controller.go (deleteFromAllPods / deleteDatabaseFromPod).

// planSubtreeDeletion orders a set of cn=config DNs so that nothing is deleted
// before its own descendants: deepest first, input order preserved within a
// depth.
//
// slapd does not cascade. config_back_delete refuses a delete whose target
// still has children with notAllowedOnNonLeaf (66) —
// servers/slapd/bconfig.c:6931 `else if ( ce->ce_kids )`, live because
// servers/slapd/slap.h:73 defines SLAP_CONFIG_DELETE unconditionally in the
// 2.7.1 tree we build (ADR-021). A replicated data database always has children
// (olcOverlay={0}syncprov and olcOverlay={1}accesslog), so a bare Del of it is
// a delete of a non-leaf and always fails. removeAccesslogDBAt has deleted
// children-first for exactly this reason since ADR-019; this is the same rule,
// generalised to arbitrary depth so an overlay that carries its own cn=config
// children needs no special case.
//
// Deleting the WHOLE subtree is right here and would be wrong in
// unwantedLogDBChildren. That reaper removes a child while KEEPING the parent,
// so an unrecognised entry is someone else's (ADR-002 sanctions hand edits) and
// must be left alone. Here the parent is going away on an explicit, opt-in
// request to remove this database — leaving a child behind does not preserve
// it, it only makes the requested delete fail.
func planSubtreeDeletion(dns []string) []string {
	if len(dns) == 0 {
		return nil
	}
	out := append([]string(nil), dns...)
	sort.SliceStable(out, func(i, j int) bool {
		return dnDepth(out[i]) > dnDepth(out[j])
	})
	return out
}

// dnDepth counts the RDNs in a DN. Only an unescaped comma separates RDNs; a
// comma inside an RDN value is backslash-escaped (RFC 4514 §2.4).
func dnDepth(dn string) int {
	if dn == "" {
		return 0
	}
	depth := 1
	escaped := false
	for i := 0; i < len(dn); i++ {
		switch {
		case escaped:
			escaped = false
		case dn[i] == '\\':
			escaped = true
		case dn[i] == ',':
			depth++
		}
	}
	return depth
}

// databasePodHost is one pod that carries a copy of a SlapdDatabase's
// cn=config, with the headless-service FQDN to reach it on (no port — callers
// append their own).
type databasePodHost struct {
	Name     string
	Host     string
	ReadOnly bool
	Ordinal  int32
}

// databasePodHosts enumerates every pod that carries the database: the RW
// StatefulSet, then the read-only one.
//
// cn=config is node-local (ADR-002), so "remove the database" means removing it
// once per pod — and an RO replica carries its own copy of the data database
// (it is a consumer of it), just without the accesslog DB and the overlays.
// ADR-005 says "each pod's cn=config" and means it; a teardown that walks only
// spec.replicas leaves the database served by the RO fleet forever, which is
// neither Delete nor Retain.
//
// NB the two per-pod convergence loops in slapddatabase_controller.go
// (reconcilePodDatabase, reconcileReplication) still enumerate pods inline with
// this same RW-then-RO shape. Unifying them is a behaviour-preserving refactor
// and deliberately not part of this fix.
func databasePodHosts(sc *ldapv1alpha1.SlapdCluster, clusterDomain string) []databasePodHost {
	replicas := sc.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}

	out := make([]databasePodHost, 0, replicas+sc.Spec.ReadReplicas)
	for i := int32(0); i < replicas; i++ {
		name := fmt.Sprintf("%s-%d", sc.Name, i)
		out = append(out, databasePodHost{
			Name: name,
			Host: fmt.Sprintf("%s.%s-headless.%s.svc.%s",
				name, sc.Name, sc.Namespace, clusterDomain),
			Ordinal: i,
		})
	}
	for i := int32(0); i < sc.Spec.ReadReplicas; i++ {
		name := fmt.Sprintf("%s-readonly-%d", sc.Name, i)
		out = append(out, databasePodHost{
			Name: name,
			Host: fmt.Sprintf("%s.%s-readonly-headless.%s.svc.%s",
				name, sc.Name, sc.Namespace, clusterDomain),
			ReadOnly: true,
			Ordinal:  i,
		})
	}
	return out
}
