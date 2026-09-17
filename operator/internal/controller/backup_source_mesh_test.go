/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// isReplicationParticipant on a mesh-driven cluster (MESH-PLAN Phase 6a).
//
// The predicate answers one question: can writes reach this cluster's data
// anywhere other than the pod a backup reads? When it says no, the backup
// records SourceConverged=True/NotReplicated — a confident claim that pod-0's
// view IS the whole truth.
//
// It answered from spec.replicas and spec.replication.externalPeers. A
// mesh-driven cluster hand-writes neither: the operator derives the peers in
// memory and never writes them back (ADR-028 §4). So a single-replica mesh
// member — a perfectly ordinary site in a three-site mesh — read as "not
// replicated", and every backup taken there was stamped with a convergence
// claim that no evidence supported.
//
// Reachability, which is what makes this a defect rather than a theoretical
// gap: ReplicationConverged (the condition that would otherwise pre-empt this
// fallback) is only computed once at least two local pods are Ready. At
// replicas=1 it is NEVER set, so the fallback is not merely reachable at that
// size, it is the only path.
func TestIsReplicationParticipantHonoursMeshRef(t *testing.T) {
	tests := []struct {
		name string
		sc   *ldapv1alpha1.SlapdCluster
		want bool
	}{
		{
			// The defect, minimal. One local pod, peers declared only by
			// reference to a mesh.
			name: "single replica, peers come from a mesh",
			sc: &ldapv1alpha1.SlapdCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "slapd", Namespace: "ns"},
				Spec: ldapv1alpha1.SlapdClusterSpec{
					Replicas:    1,
					MeshRef:     "lab",
					Replication: ldapv1alpha1.SlapdReplicationConfig{Enabled: true},
				},
			},
			want: true,
		},
		{
			// A meshRef whose derivation has not been applied to this copy of
			// the object is still a declaration that this cluster spans a
			// mesh. Unreadable evidence never counts toward the confident
			// answer (ADR-008's rule), and the confident answer here is
			// "not replicated".
			name: "single replica, mesh unresolvable from this reader",
			sc: &ldapv1alpha1.SlapdCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "slapd", Namespace: "ns"},
				Spec: ldapv1alpha1.SlapdClusterSpec{
					Replicas:    1,
					MeshRef:     "gone",
					Replication: ldapv1alpha1.SlapdReplicationConfig{Enabled: true},
				},
				Status: ldapv1alpha1.SlapdClusterStatus{
					Conditions: []metav1.Condition{{
						Type:   "MeshResolved",
						Status: metav1.ConditionFalse,
						Reason: "MeshNotFound",
					}},
				},
			},
			want: true,
		},
		{
			// Replication switched off dominates everything. Without
			// replication infrastructure a meshRef configures nothing, so the
			// answer stays the confident one.
			name: "meshRef but replication disabled",
			sc: &ldapv1alpha1.SlapdCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "slapd", Namespace: "ns"},
				Spec: ldapv1alpha1.SlapdClusterSpec{
					Replicas: 1,
					MeshRef:  "lab",
				},
			},
			want: false,
		},
		{
			// The compatibility invariant: no meshRef, no peers, one replica.
			// This is every standalone cluster and its answer must not move.
			name: "no meshRef, genuinely standalone",
			sc: &ldapv1alpha1.SlapdCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "slapd", Namespace: "ns"},
				Spec: ldapv1alpha1.SlapdClusterSpec{
					Replicas:    1,
					Replication: ldapv1alpha1.SlapdReplicationConfig{Enabled: true},
				},
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isReplicationParticipant(tc.sc); got != tc.want {
				t.Errorf("isReplicationParticipant = %v, want %v", got, tc.want)
			}
		})
	}
}

// The condition the predicate feeds. A single-replica mesh member must NOT be
// stamped with the confident NotReplicated claim; it must say convergence is
// unknown, which is the truth.
func TestSourceConvergedOnSingleReplicaMeshMember(t *testing.T) {
	sc := &ldapv1alpha1.SlapdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "slapd", Namespace: "ns"},
		Spec: ldapv1alpha1.SlapdClusterSpec{
			Replicas:    1,
			MeshRef:     "lab",
			Replication: ldapv1alpha1.SlapdReplicationConfig{Enabled: true},
		},
	}

	cond := sourceConvergedCondition(sc, 1, metav1.Now())

	if cond.Status != metav1.ConditionUnknown {
		t.Errorf("Status = %s, want %s — a mesh member's convergence is unknown, not proven",
			cond.Status, metav1.ConditionUnknown)
	}
	if cond.Reason != "NoConvergenceSignal" {
		t.Errorf("Reason = %q, want %q", cond.Reason, "NoConvergenceSignal")
	}
	if strings.Contains(cond.Message, "not replicated") {
		t.Errorf("Message claims the cluster is not replicated: %q", cond.Message)
	}
}
