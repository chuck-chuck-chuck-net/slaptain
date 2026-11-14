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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The wiring the rotation needs beyond the pure decisions in writeservice.go:
// a per-pod readiness reading, and a requeue that actually reaches the
// reconciler's return.
//
// Both are places the feature can be built perfectly and still never fire.

// pod builds one read-write pod of the fixture cluster at the given ordinal.
func pod(ordinal int32, ready bool) corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return corev1.Pod{
		Name:      statefulSetPodName(testClusterName, ordinal),
		Namespace: testNamespace,
		Labels:    selectorLabels(testClusterName),
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
		},
	}
}

// Readiness is per pod, and the map's ABSENCES are as load-bearing as its
// entries: eligibleForWrites reads a missing pod as not ready, so anything this
// function declines to record is a pod the write endpoint will not be pointed
// at. The operator has only ever read sts.Status.ReadyReplicas, which is a
// count and names nobody.
func TestReadyOrdinals(t *testing.T) {
	terminating := pod(2, true)
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	terminating.Finalizers = []string{"test/hold"}

	noCondition := pod(3, true)
	noCondition.Status.Conditions = nil

	foreign := pod(0, true)
	foreign.Name = "someone-else-0"

	readOnly := pod(0, true)
	readOnly.Name = testClusterName + "-readonly-0"

	for _, tc := range []struct {
		name    string
		pods    []corev1.Pod
		want    map[int32]bool
		because string
	}{
		{
			name:    "a ready pod is recorded ready",
			pods:    []corev1.Pod{pod(0, true), pod(1, true)},
			want:    map[int32]bool{0: true, 1: true},
			because: "this is the ordinary fleet, and every slot of the rotation runs on it",
		},
		{
			name:    "an unready pod is recorded, as not ready",
			pods:    []corev1.Pod{pod(0, true), pod(1, false)},
			want:    map[int32]bool{0: true, 1: false},
			because: "a pod that does not answer LDAP is one writes must not be steered to (ADR-030)",
		},
		{
			name:    "a terminating pod is not ready however its condition reads",
			pods:    []corev1.Pod{pod(0, true), terminating},
			want:    map[int32]bool{0: true, 2: false},
			because: "a deleting pod keeps Ready=True for a while, and its endpoint is already gone",
		},
		{
			name:    "a pod with no Ready condition is not ready",
			pods:    []corev1.Pod{noCondition},
			want:    map[int32]bool{3: false},
			because: "absence of evidence is not evidence of readiness",
		},
		{
			name:    "a pod of another workload is not this cluster's",
			pods:    []corev1.Pod{foreign},
			want:    map[int32]bool{},
			because: "the ordinal is read against this cluster's name; another name's pod-7 is not our pod 7",
		},
		{
			name:    "a read-only replica is not pod 0 of the read-write fleet",
			pods:    []corev1.Pod{readOnly},
			want:    map[int32]bool{},
			because: "read from the right, slapd-readonly-0 parses as ordinal 0, and a consumer replica refuses every write it is given",
		},
		{
			name:    "no pods at all yields an empty map, never a nil one that reads as unknown",
			pods:    nil,
			want:    map[int32]bool{},
			because: "an empty map makes nobody eligible, which leaves the selector exactly where it was",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := readyOrdinals(tc.pods, testClusterName)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("readyOrdinals(...) = %v, want %v — %s", got, tc.want, tc.because)
			}
		})
	}
}

// The requeue bug this wiring exists to avoid.
//
// The controller's steady-state requeue is GATED on spec.replication.enabled:
// a Running cluster without replication returns a bare ctrl.Result{} and
// nothing schedules another pass. A rotation requeue returned from inside that
// branch would work on replicated clusters and silently never fire on the
// others — and "silently never happened" is the failure mode this whole ADR
// keeps running into.
func TestRequeueAfterFor(t *testing.T) {
	running := func(replication bool) *ldapv1alpha1.SlapdCluster {
		sc := &ldapv1alpha1.SlapdCluster{}
		sc.Status.Phase = ldapv1alpha1.PhaseRunning
		sc.Spec.Replication.Enabled = replication
		return sc
	}
	bootstrapping := func() *ldapv1alpha1.SlapdCluster {
		sc := &ldapv1alpha1.SlapdCluster{}
		sc.Status.Phase = ldapv1alpha1.PhaseBootstrapping
		return sc
	}

	for _, tc := range []struct {
		name     string
		sc       *ldapv1alpha1.SlapdCluster
		rotation time.Duration
		want     time.Duration
		because  string
	}{
		{
			name: "a Running cluster WITHOUT replication still gets the rotation requeue",
			sc:   running(false), rotation: 5 * time.Minute, want: 5 * time.Minute,
			because: "the steady-state requeue is gated on replication; a rotation requeue inside that gate never fires on a standalone cluster",
		},
		{
			name: "a Running cluster with replication takes whichever comes sooner",
			sc:   running(true), rotation: 5 * time.Minute, want: csnCheckInterval,
			because: "the CSN tick is the sooner of the two; the rotation is served by the next one after it",
		},
		{
			name: "a rotation sooner than the CSN tick wins",
			sc:   running(true), rotation: 3 * time.Second, want: 3 * time.Second,
			because: "the boundary is the instant the writer moves, and missing it is a slot spent on the wrong pod",
		},
		{
			name: "no rotation leaves the replicated cadence exactly as it was",
			sc:   running(true), rotation: 0, want: csnCheckInterval,
			because: "rotation off must be byte-identical to the behaviour before this feature existed",
		},
		{
			name: "no rotation and no replication leaves no requeue at all",
			sc:   running(false), rotation: 0, want: 0,
			because: "the same: an off feature changes nothing",
		},
		{
			name: "a cluster that is not yet Running keeps its 10s cadence",
			sc:   bootstrapping(), rotation: 5 * time.Minute, want: 10 * time.Second,
			because: "the bootstrap cadence is the sooner one and the rotation has nothing to point at yet",
		},
		{
			name: "a rotation sooner than the bootstrap cadence wins there too",
			sc:   bootstrapping(), rotation: 2 * time.Second, want: 2 * time.Second,
			because: "whichever boundary comes first is the one the pass must wake for",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := requeueAfterFor(tc.sc, tc.rotation); got != tc.want {
				t.Fatalf("requeueAfterFor(..., %s) = %s, want %s — %s",
					tc.rotation, got, tc.want, tc.because)
			}
		})
	}
}

// Rotation off does not mean "point the write endpoint nowhere" and does not
// mean "delete it". A Service clients were told to follow must keep answering;
// what it must stop doing is pinning every write onto one pod whose purge is no
// longer phased by anybody.
func TestFleetWriteServiceSelectsTheWholeReadWriteFleet(t *testing.T) {
	r, sc := writeFixture()

	svc := r.fleetWriteService(sc)
	if svc == nil {
		t.Fatal("fleetWriteService returned nil — there is nothing to repoint the retired endpoint at")
		return
	}
	if svc.Name != testWriteSvcName {
		t.Fatalf("Name = %q, want %q — it must be the same object, not a second one", svc.Name, testWriteSvcName)
	}
	if got := svc.Spec.Selector[labelStatefulSetPodName]; got != "" {
		t.Fatalf("selector still carries %s=%q — with the rotation off nothing phases that pod's "+
			"purge any more, so pinning every write to it is the 30 s stall ADR-037 measured, "+
			"arriving when the user turns the feature OFF", labelStatefulSetPodName, got)
	}
	if !reflect.DeepEqual(svc.Spec.Selector, selectorLabels(sc.Name)) {
		t.Fatalf("Selector = %v, want %v — the retired endpoint is an alias of the client Service",
			svc.Spec.Selector, selectorLabels(sc.Name))
	}
	if ref := r.clusterIPService(sc); !reflect.DeepEqual(svc.Spec.Ports, ref.Spec.Ports) {
		t.Fatalf("Ports = %+v, want %+v — clients keep dialling the port they were given",
			svc.Spec.Ports, ref.Spec.Ports)
	}
}
