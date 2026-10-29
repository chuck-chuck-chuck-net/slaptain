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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// fleetName labels a subtest with the fleet it exercises.
func fleetName(readOnly bool) string {
	if readOnly {
		return "ro-fleet"
	}
	return "rw-fleet"
}

// ADR-043 decision 3: when the user says nothing about spread constraints the
// operator writes exactly ONE, per fleet, and it must select that fleet alone.
//
// The selector is the whole point. A constraint selecting every pod of the
// cluster constrains the per-node TOTAL and says nothing about the mix: with
// three nodes, two writers and four readers, 2/2/2 satisfies maxSkew 1 while
// both writers share a node — the catastrophe the feature exists to prevent.
// So the expected selector is built from the same helpers the StatefulSet
// selector uses, not re-spelled here: if those two ever disagree the constraint
// would select nothing at all, which looks exactly like a healthy spread.
func TestDefaultSpreadConstraint(t *testing.T) {
	for _, tc := range []struct {
		name     string
		readOnly bool
		want     map[string]string
	}{
		{"read-write fleet", false, selectorLabels("slapd")},
		{"read-only fleet", true, readOnlySelectorLabels("slapd")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := defaultSpreadConstraint("slapd", tc.readOnly)

			if got.MaxSkew != 1 {
				t.Errorf("maxSkew = %d, want 1", got.MaxSkew)
			}
			if got.TopologyKey != corev1.LabelHostname {
				t.Errorf("topologyKey = %q, want %q", got.TopologyKey, corev1.LabelHostname)
			}
			// ScheduleAnyway, never DoNotSchedule: a bound local PV pins the
			// pod to one node, so a hard constraint refusing that node leaves
			// it Pending with no reschedule that can help (ADR-043 decision 4).
			if got.WhenUnsatisfiable != corev1.ScheduleAnyway {
				t.Errorf("whenUnsatisfiable = %q, want %q", got.WhenUnsatisfiable, corev1.ScheduleAnyway)
			}
			if got.LabelSelector == nil {
				t.Fatalf("labelSelector is nil — the constraint would select every pod in the namespace")
			}
			if !reflect.DeepEqual(got.LabelSelector.MatchLabels, tc.want) {
				t.Errorf("labelSelector.matchLabels = %v, want %v", got.LabelSelector.MatchLabels, tc.want)
			}
			if len(got.LabelSelector.MatchExpressions) != 0 {
				t.Errorf("labelSelector carries matchExpressions %v, want none", got.LabelSelector.MatchExpressions)
			}
			// matchLabelKeys is the one non-GA field in the type and solves a
			// Deployment problem (ADR-043 decision 6).
			if len(got.MatchLabelKeys) != 0 {
				t.Errorf("matchLabelKeys = %v, want none", got.MatchLabelKeys)
			}
		})
	}
}

// The two fleets must not get the same constraint: that is option 3 of
// ADR-043, which looks right and is strictly weaker.
func TestDefaultSpreadConstraintDiffersPerFleet(t *testing.T) {
	rw := defaultSpreadConstraint("slapd", false)
	ro := defaultSpreadConstraint("slapd", true)
	if reflect.DeepEqual(rw, ro) {
		t.Fatalf("both fleets got the same constraint %+v — a combined group permits every writer "+
			"to share a node while the per-node totals still look even (ADR-043 option 3)", rw)
	}
}

// spreadConstraintsFor resolves the one decision this feature makes: the
// user's list when they supplied one, otherwise the generated default.
// REPLACE, never merge — merging would put two hostname constraints with the
// same whenUnsatisfiable on one pod, which the API rejects outright
// (ValidateSpreadConstraintNotRepeat ignores labelSelector), from a value the
// user cannot see.
func TestSpreadConstraintsFor(t *testing.T) {
	userConstraint := corev1.TopologySpreadConstraint{
		MaxSkew:           2,
		TopologyKey:       "topology.kubernetes.io/zone",
		WhenUnsatisfiable: corev1.DoNotSchedule,
		LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{"owner": "operator-team"}},
	}

	for _, tc := range []struct {
		name     string
		readOnly bool
		set      func(*ldapv1alpha1.SlapdCluster)
		want     []corev1.TopologySpreadConstraint
	}{
		{
			name: "unset generates the read-write default",
			set:  func(*ldapv1alpha1.SlapdCluster) {},
			want: []corev1.TopologySpreadConstraint{defaultSpreadConstraint("slapd", false)},
		},
		{
			name:     "unset generates the read-only default",
			readOnly: true,
			set:      func(*ldapv1alpha1.SlapdCluster) {},
			want:     []corev1.TopologySpreadConstraint{defaultSpreadConstraint("slapd", true)},
		},
		{
			name: "a user list replaces the default, it is not merged into",
			set: func(sc *ldapv1alpha1.SlapdCluster) {
				sc.Spec.Placement.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{userConstraint}
			},
			want: []corev1.TopologySpreadConstraint{userConstraint},
		},
		{
			name:     "a user list replaces the default on the read-only fleet too",
			readOnly: true,
			set: func(sc *ldapv1alpha1.SlapdCluster) {
				sc.Spec.Placement.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{userConstraint}
			},
			want: []corev1.TopologySpreadConstraint{userConstraint},
		},
		{
			// An explicit [] is the opt-out. "Replace, never merge" applied
			// honestly: the user DID say something about spread constraints,
			// and what they said was "none". Absent is nil; empty is not.
			name: "an explicit empty list means no constraint at all",
			set: func(sc *ldapv1alpha1.SlapdCluster) {
				sc.Spec.Placement.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{}
			},
			want: []corev1.TopologySpreadConstraint{},
		},
		{
			// The rest of the block is independent: setting a nodeSelector is
			// not saying anything about spreading.
			name: "another placement field set still generates the default",
			set: func(sc *ldapv1alpha1.SlapdCluster) {
				sc.Spec.Placement.NodeSelector = map[string]string{"role": "directory"}
			},
			want: []corev1.TopologySpreadConstraint{defaultSpreadConstraint("slapd", false)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := &ldapv1alpha1.SlapdCluster{}
			sc.Name = "slapd"
			sc.Namespace = testNamespace
			tc.set(sc)

			got := spreadConstraintsFor(sc, tc.readOnly)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d constraints, want %d\n got: %+v\nwant: %+v", len(got), len(tc.want), got, tc.want)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("constraints differ (got nil=%v, want nil=%v)\n got: %+v\nwant: %+v",
					got == nil, tc.want == nil, got, tc.want)
			}
		})
	}
}

// The generated constraint names the cluster, so two clusters in one namespace
// spread independently rather than against each other.
func TestDefaultSpreadConstraintIsPerCluster(t *testing.T) {
	a := defaultSpreadConstraint("alpha", false)
	b := defaultSpreadConstraint("beta", false)
	if reflect.DeepEqual(a.LabelSelector, b.LabelSelector) {
		t.Fatalf("two clusters got the same labelSelector %+v — every SlapdCluster in the namespace "+
			"would be spread against every other", a.LabelSelector)
	}
}

// Setting spec.placement must move the pod-level placement fields and NOTHING
// else: not a container, not a volume, not an annotation. Same shape as
// TestMonitoringEnabledAddsOnlyTheExporterContainer, and for the same reason —
// "the roll happens when the user asks for placement" must not quietly become
// "the roll also changes slapd".
func TestPlacementMovesOnlyThePlacementFields(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		t.Run(fleetName(readOnly), func(t *testing.T) {
			placementMovesOnlyThePlacementFields(t, readOnly)
		})
	}
}

func placementMovesOnlyThePlacementFields(t *testing.T, readOnly bool) {
	t.Helper()
	baseline := goldenReconciler().buildStatefulSetSpec(goldenCluster(), readOnly, "").Template

	nodeAffinity := &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key:      "ldap.example/tier",
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{"storage"},
				}},
			}},
		},
	}
	tolerations := []corev1.Toleration{{
		Key:      "dedicated",
		Operator: corev1.TolerationOpEqual,
		Value:    "ldap",
		Effect:   corev1.TaintEffectNoSchedule,
	}}
	nodeSelector := map[string]string{"role": "directory"}
	userSpread := []corev1.TopologySpreadConstraint{{
		MaxSkew:           3,
		TopologyKey:       "topology.kubernetes.io/zone",
		WhenUnsatisfiable: corev1.ScheduleAnyway,
		LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{"owner": "operator-team"}},
	}}

	sc := goldenCluster()
	sc.Spec.Placement = ldapv1alpha1.PlacementConfig{
		TopologySpreadConstraints: userSpread,
		NodeSelector:              nodeSelector,
		NodeAffinity:              nodeAffinity,
		Tolerations:               tolerations,
		PriorityClassName:         "slaptain-critical",
	}
	got := goldenReconciler().buildStatefulSetSpec(sc, readOnly, "").Template

	// Every field arrives verbatim — a pass-through that quietly rewrote its
	// input would be the one failure nobody reads the CRD for.
	if !reflect.DeepEqual(got.Spec.NodeSelector, nodeSelector) {
		t.Errorf("readOnly=%v: nodeSelector = %v, want %v", readOnly, got.Spec.NodeSelector, nodeSelector)
	}
	if got.Spec.Affinity == nil || !reflect.DeepEqual(got.Spec.Affinity.NodeAffinity, nodeAffinity) {
		t.Errorf("readOnly=%v: affinity.nodeAffinity = %+v, want %+v", readOnly, got.Spec.Affinity, nodeAffinity)
	}
	// The pod half of the affinity struct is the operator's (ADR-043
	// decision 2) and nothing may put a user value there.
	if got.Spec.Affinity != nil &&
		(got.Spec.Affinity.PodAffinity != nil || got.Spec.Affinity.PodAntiAffinity != nil) {
		t.Errorf("readOnly=%v: pod (anti)affinity set from spec.placement: %+v", readOnly, got.Spec.Affinity)
	}
	if !reflect.DeepEqual(got.Spec.Tolerations, tolerations) {
		t.Errorf("readOnly=%v: tolerations = %+v, want %+v", readOnly, got.Spec.Tolerations, tolerations)
	}
	if got.Spec.PriorityClassName != "slaptain-critical" {
		t.Errorf("readOnly=%v: priorityClassName = %q, want %q", readOnly, got.Spec.PriorityClassName, "slaptain-critical")
	}
	if !reflect.DeepEqual(got.Spec.TopologySpreadConstraints, userSpread) {
		t.Errorf("readOnly=%v: topologySpreadConstraints = %+v, want the user's %+v",
			readOnly, got.Spec.TopologySpreadConstraints, userSpread)
	}

	// And nothing else moved. Zero the five fields on both sides and the two
	// templates must be identical.
	gotPod, wantPod := got.Spec, baseline.Spec
	for _, p := range []*corev1.PodSpec{&gotPod, &wantPod} {
		p.NodeSelector, p.Affinity, p.Tolerations = nil, nil, nil
		p.PriorityClassName, p.TopologySpreadConstraints = "", nil
	}
	if !reflect.DeepEqual(gotPod, wantPod) {
		t.Errorf("readOnly=%v: setting spec.placement changed something other than the placement "+
			"fields\n got: %+v\nwant: %+v", readOnly, gotPod, wantPod)
	}
	if !reflect.DeepEqual(got.ObjectMeta, baseline.ObjectMeta) {
		t.Errorf("readOnly=%v: pod template metadata changed when spec.placement was set\n got: %+v\nwant: %+v",
			readOnly, got.ObjectMeta, baseline.ObjectMeta)
	}
}
