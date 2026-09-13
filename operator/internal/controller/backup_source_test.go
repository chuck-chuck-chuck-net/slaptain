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

// A backup records its circumstances; it never judges them (ADR-014 amendment
// 2026-09-12). sourceConvergedCondition therefore CONSUMES the SlapdCluster's
// ReplicationConverged condition — the signal regular operations already
// maintain — and must never re-derive convergence of its own.

func replicatedCluster(conds ...metav1.Condition) *ldapv1alpha1.SlapdCluster {
	return &ldapv1alpha1.SlapdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "slapd", Namespace: "ns"},
		Spec: ldapv1alpha1.SlapdClusterSpec{
			Replicas:    3,
			Replication: ldapv1alpha1.SlapdReplicationConfig{Enabled: true},
		},
		Status: ldapv1alpha1.SlapdClusterStatus{Conditions: conds},
	}
}

func TestSourceConvergedCondition(t *testing.T) {
	now := metav1.Now()

	tests := []struct {
		name       string
		cluster    *ldapv1alpha1.SlapdCluster
		wantStatus metav1.ConditionStatus
		wantReason string
		wantMsgHas string
	}{
		{
			name: "cluster reports converged",
			cluster: replicatedCluster(metav1.Condition{
				Type:    "ReplicationConverged",
				Status:  metav1.ConditionTrue,
				Reason:  "CSNsMatch",
				Message: "all local pods report identical contextCSN",
			}),
			wantStatus: metav1.ConditionTrue,
			wantReason: "ClusterConverged",
			wantMsgHas: "all local pods report identical contextCSN",
		},
		{
			// The incident shape: pod-0 lagged the write that had just been
			// ACKed on another pod. The backup still runs, but says so.
			name: "cluster reports divergence",
			cluster: replicatedCluster(metav1.Condition{
				Type:    "ReplicationConverged",
				Status:  metav1.ConditionFalse,
				Reason:  "CSNsDiverged",
				Message: "local CSN divergence: 3.0s lag across 3 pods",
			}),
			wantStatus: metav1.ConditionFalse,
			wantReason: "ClusterDiverged",
			wantMsgHas: "3.0s lag across 3 pods",
		},
		{
			name: "cluster reports unknown",
			cluster: replicatedCluster(metav1.Condition{
				Type:    "ReplicationConverged",
				Status:  metav1.ConditionUnknown,
				Reason:  "Whatever",
				Message: "no idea",
			}),
			wantStatus: metav1.ConditionUnknown,
			wantReason: "ConvergenceUnknown",
			wantMsgHas: "no idea",
		},
		{
			// Replication is on but the cluster has not produced the signal yet
			// (too few CSN sets, monitoring not yet run). Unknown, not True:
			// silence is not evidence of convergence.
			name:       "replicated cluster without the condition",
			cluster:    replicatedCluster(),
			wantStatus: metav1.ConditionUnknown,
			wantReason: "NoConvergenceSignal",
			wantMsgHas: "ReplicationConverged",
		},
		{
			// Nothing to converge WITH: pod-0 is the only writable copy, so the
			// artifact is by construction the whole truth.
			name: "replication disabled",
			cluster: &ldapv1alpha1.SlapdCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "slapd", Namespace: "ns"},
				Spec:       ldapv1alpha1.SlapdClusterSpec{Replicas: 1},
			},
			wantStatus: metav1.ConditionTrue,
			wantReason: "NotReplicated",
			wantMsgHas: "not replicated",
		},
		{
			name: "replication enabled but single replica, no peers",
			cluster: &ldapv1alpha1.SlapdCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "slapd", Namespace: "ns"},
				Spec: ldapv1alpha1.SlapdClusterSpec{
					Replicas:    1,
					Replication: ldapv1alpha1.SlapdReplicationConfig{Enabled: true},
				},
			},
			wantStatus: metav1.ConditionTrue,
			wantReason: "NotReplicated",
			wantMsgHas: "not replicated",
		},
		{
			// One replica but an external peer: writes can land elsewhere, so
			// this cluster IS a replication participant.
			name: "single replica with an external peer",
			cluster: &ldapv1alpha1.SlapdCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "slapd", Namespace: "ns"},
				Spec: ldapv1alpha1.SlapdClusterSpec{
					Replicas: 1,
					Replication: ldapv1alpha1.SlapdReplicationConfig{
						Enabled:       true,
						ExternalPeers: []ldapv1alpha1.ExternalPeer{{Name: "siteb"}},
					},
				},
			},
			wantStatus: metav1.ConditionUnknown,
			wantReason: "NoConvergenceSignal",
			wantMsgHas: "ReplicationConverged",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sourceConvergedCondition(tc.cluster, 7, now)
			if got.Type != backupSourceConvergedCondition {
				t.Errorf("Type = %q, want %q", got.Type, backupSourceConvergedCondition)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tc.wantReason)
			}
			if !strings.Contains(got.Message, tc.wantMsgHas) {
				t.Errorf("Message = %q, want it to contain %q", got.Message, tc.wantMsgHas)
			}
			if got.ObservedGeneration != 7 {
				t.Errorf("ObservedGeneration = %d, want 7", got.ObservedGeneration)
			}
			if got.LastTransitionTime.IsZero() {
				t.Error("LastTransitionTime is zero")
			}
		})
	}
}

// The status copy of the source's contextCSN vector must be stable across
// reconciles: the same vector read twice in a different order must not churn
// the status (and therefore must not re-trigger SSA writes).
// TestSourceSuffixHealthyCondition pins the ADR-025 C2 record: a backup taken
// from a pod whose suffix entry is a hidden glue produces an artifact that
// restore preflight will reject, and the SlapdBackup must say so — record-only,
// never refusing the backup. Silence (a failed probe) is Unknown, not True.
func TestSourceSuffixHealthyCondition(t *testing.T) {
	now := metav1.Now()
	tests := []struct {
		name       string
		outcome    suffixProbeOutcome
		detail     string
		wantStatus metav1.ConditionStatus
		wantReason string
		wantInMsg  string
	}{
		{"visible entry is healthy", suffixProbeVisible, "", metav1.ConditionTrue, "SuffixEntryVisible", ""},
		{"glue suffix recorded", suffixProbeGlue, "entryUUID 1a914f3a-43f0-1041-9ed3-896165d17446",
			metav1.ConditionFalse, "GlueSuffix", "1a914f3a"},
		{"glue message warns about restore", suffixProbeGlue, "", metav1.ConditionFalse, "GlueSuffix", "preflight"},
		{"missing suffix recorded", suffixProbeMissing, "", metav1.ConditionFalse, "SuffixMissing", ""},
		{"probe failure is unknown", suffixProbeError, "dial tcp: timeout",
			metav1.ConditionUnknown, "CheckFailed", "timeout"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cond := sourceSuffixHealthyCondition(tc.outcome, tc.detail, 3, now)
			if cond.Type != backupSuffixHealthyCondition {
				t.Errorf("condition type = %q, want %q", cond.Type, backupSuffixHealthyCondition)
			}
			if cond.Status != tc.wantStatus || cond.Reason != tc.wantReason {
				t.Errorf("condition = (%s, %s, %q), want (%s, %s)",
					cond.Status, cond.Reason, cond.Message, tc.wantStatus, tc.wantReason)
			}
			if tc.wantInMsg != "" && !strings.Contains(cond.Message, tc.wantInMsg) {
				t.Errorf("message %q does not mention %q", cond.Message, tc.wantInMsg)
			}
			if cond.ObservedGeneration != 3 {
				t.Errorf("observedGeneration = %d, want 3", cond.ObservedGeneration)
			}
		})
	}
}

func TestNormalizedCSNVector(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "nil", in: nil, want: nil},
		{name: "empty", in: []string{}, want: nil},
		{
			name: "sorted deterministically",
			in: []string{
				"20260912104500.000000Z#000000#003#000000",
				"20260912104300.000000Z#000000#001#000000",
			},
			want: []string{
				"20260912104300.000000Z#000000#001#000000",
				"20260912104500.000000Z#000000#003#000000",
			},
		},
		{
			name: "duplicates collapsed, blanks dropped",
			in: []string{
				"20260912104300.000000Z#000000#001#000000",
				"",
				"  ",
				"20260912104300.000000Z#000000#001#000000",
			},
			want: []string{"20260912104300.000000Z#000000#001#000000"},
		},
		{
			name: "surrounding whitespace trimmed",
			in:   []string{" 20260912104300.000000Z#000000#001#000000 "},
			want: []string{"20260912104300.000000Z#000000#001#000000"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizedCSNVector(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v (len %d), want %v (len %d)", got, len(got), tc.want, len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}

	// The input slice must not be mutated — the caller owns it (it comes
	// straight off an LDAP entry).
	in := []string{"b", "a"}
	_ = normalizedCSNVector(in)
	if in[0] != "b" || in[1] != "a" {
		t.Errorf("input slice was mutated: %v", in)
	}
}

// status.sourcePod must name the pod the Job actually reads. The two facts are
// derived independently (backupSourcePod for the record, buildBackupJob for the
// PVCs and the node pin), so pin them together: a future change that moves the
// backup off pod-0 and forgets the record fails here.
//
// Green from birth — it asserts agreement between two existing behaviours.
// Mutation-checked: returning sc.Name+"-1" from backupSourcePod fails all three
// assertions below (verified 2026-09-12).
func TestBackupSourcePodMatchesJobAffinity(t *testing.T) {
	sc := &ldapv1alpha1.SlapdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "slapd", Namespace: "ns"},
		Spec:       ldapv1alpha1.SlapdClusterSpec{Replicas: 3},
	}
	sd := &ldapv1alpha1.SlapdDatabase{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "ns"},
		Spec:       ldapv1alpha1.SlapdDatabaseSpec{Suffix: "dc=example,dc=org"},
	}
	sb := &ldapv1alpha1.SlapdBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: "ns"},
		Spec:       ldapv1alpha1.SlapdBackupSpec{DatabaseRef: "default"},
	}

	want := backupSourcePod(sc)
	job := buildBackupJob(sb, sd, sc, "init:latest", "operator:latest", "k/e/y.ldif.gz")

	terms := job.Spec.Template.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if len(terms) != 1 {
		t.Fatalf("expected exactly one pod affinity term, got %d", len(terms))
	}
	if got := terms[0].LabelSelector.MatchLabels["statefulset.kubernetes.io/pod-name"]; got != want {
		t.Errorf("Job is pinned to pod %q but status would report %q", got, want)
	}

	var claims []string
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			claims = append(claims, v.PersistentVolumeClaim.ClaimName)
		}
	}
	for _, prefix := range []string{"config-", "data-"} {
		wantClaim := prefix + want
		found := false
		for _, c := range claims {
			if c == wantClaim {
				found = true
			}
		}
		if !found {
			t.Errorf("Job mounts %v, expected the reported source pod's claim %q", claims, wantClaim)
		}
	}
}
