/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// defaultSpreadConstraint is the one thing the operator generates for a user
// who said nothing about placement (ADR-043 decision 3): spread THIS fleet of
// THIS cluster evenly over the nodes, and never refuse to schedule for it.
//
// Three choices, each load-bearing:
//
//   - maxSkew 1 on kubernetes.io/hostname. Even is the point; the hostname is
//     the only topology key that means anything on a single site.
//   - ScheduleAnyway, not DoNotSchedule. A bound local PV pins a pod to one
//     node, so a hard constraint that refuses that node leaves the pod Pending
//     with no reschedule that can resolve it (ADR-043 Consequences).
//   - a labelSelector naming this fleet alone, built from the same helper that
//     builds the StatefulSet selector. ONE combined group over both fleets
//     constrains the per-node TOTAL and permits every writer to share a node
//     while the counts still look even (ADR-043 option 3), and re-spelling the
//     label map here is how the two would silently drift apart — a constraint
//     selecting nothing reads exactly like a healthy spread.
func defaultSpreadConstraint(clusterName string, readOnly bool) corev1.TopologySpreadConstraint {
	labels := selectorLabels(clusterName)
	if readOnly {
		labels = readOnlySelectorLabels(clusterName)
	}
	return corev1.TopologySpreadConstraint{
		MaxSkew:           1,
		TopologyKey:       corev1.LabelHostname,
		WhenUnsatisfiable: corev1.ScheduleAnyway,
		LabelSelector:     &metav1.LabelSelector{MatchLabels: labels},
	}
}

// spreadConstraintsFor returns the spread constraints for one fleet's pod
// template: the user's list where they supplied one, the generated default
// where they did not.
//
// REPLACE, never merge. Two constraints on one pod sharing a {topologyKey,
// whenUnsatisfiable} pair are rejected by the API — and the check ignores
// labelSelector, so ours and a user's hostname constraint collide however
// differently they select. Merging would therefore make a perfectly reasonable
// user constraint unschedulable from a value the user cannot see.
//
// Absent and empty are different answers. A nil list is "said nothing" and
// gets the default; an explicit [] is "said none", and is the only way to
// switch the default off.
func spreadConstraintsFor(sc *ldapv1alpha1.SlapdCluster, readOnly bool) []corev1.TopologySpreadConstraint {
	if sc.Spec.Placement.TopologySpreadConstraints != nil {
		return sc.Spec.Placement.TopologySpreadConstraints
	}
	return []corev1.TopologySpreadConstraint{defaultSpreadConstraint(sc.Name, readOnly)}
}

// placementNodeAffinity wraps the user's nodeAffinity in the PodSpec's
// affinity struct, or returns nil when they set none — so an unset
// spec.placement renders no affinity field at all rather than an empty one,
// which would roll every existing pod for nothing.
//
// Only the node half is ever filled: the pod half is reserved for the operator
// (ADR-043 decision 2).
func placementNodeAffinity(sc *ldapv1alpha1.SlapdCluster) *corev1.Affinity {
	if sc.Spec.Placement.NodeAffinity == nil {
		return nil
	}
	return &corev1.Affinity{NodeAffinity: sc.Spec.Placement.NodeAffinity}
}
