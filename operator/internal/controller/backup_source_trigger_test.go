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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// ADR-026 R3: a convergence step is triggered by the condition it repairs,
// never by a sibling artifact. "Record the backup's source circumstances"
// (ADR-014 amendment 2026-09-12: unconditionally) repairs *the record being
// absent* — so the trigger is the absence of the record, not the absence of the
// backup Job.
//
// Keying it on the Job made the state absorbing: one lost status write after
// Create (patchStatus logs its Apply error and returns), an operator restart
// between Create and that write, or the deliberately tolerated
// IsAlreadyExists path, and the Job exists forever after while the record never
// arrives. Observed 2026-09-15 on a live mesh: a Completed SlapdBackup with a
// nil SourceConverged condition, which tests/e2e/backup_test.go asserts against.

func recordedStatus(condTypes ...string) ldapv1alpha1.SlapdBackupStatus {
	st := ldapv1alpha1.SlapdBackupStatus{SourcePod: "slapd-0"}
	for _, t := range condTypes {
		st.Conditions = append(st.Conditions, metav1.Condition{
			Type:               t,
			Status:             metav1.ConditionTrue,
			Reason:             "Recorded",
			LastTransitionTime: metav1.Now(),
		})
	}
	return st
}

func TestShouldRecordSourceCircumstances(t *testing.T) {
	tests := []struct {
		name      string
		status    ldapv1alpha1.SlapdBackupStatus
		jobExists bool
		want      bool
		why       string
	}{
		{
			name:      "first pass: nothing recorded, no Job yet",
			status:    ldapv1alpha1.SlapdBackupStatus{},
			jobExists: false,
			want:      true,
			why:       "positive control — the normal path must still record, before the Job is created",
		},
		{
			name:      "the incident: nothing recorded, Job already exists",
			status:    ldapv1alpha1.SlapdBackupStatus{},
			jobExists: true,
			want:      true,
			why:       "a lost status write after Create must not make the record unreachable (ADR-026 R3)",
		},
		{
			name:      "already recorded, Job exists",
			status:    recordedStatus(backupSourceConvergedCondition, backupSuffixHealthyCondition),
			jobExists: true,
			want:      false,
			why:       "recording does live LDAP work; once the circumstances are on the record it never repeats",
		},
		{
			name:      "already recorded, Job not observed yet",
			status:    recordedStatus(backupSourceConvergedCondition, backupSuffixHealthyCondition),
			jobExists: false,
			want:      false,
			why:       "the tolerated IsAlreadyExists / racing-cache pass must not re-probe and flap the record",
		},
		{
			name:      "partial record: SourceConverged only",
			status:    recordedStatus(backupSourceConvergedCondition),
			jobExists: true,
			want:      true,
			why:       "the circumstances are one record; half of it is not recorded",
		},
		{
			name:      "partial record: SourceSuffixHealthy only",
			status:    recordedStatus(backupSuffixHealthyCondition),
			jobExists: true,
			want:      true,
			why:       "same, the other way round",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldRecordSourceCircumstances(tc.status, tc.jobExists)
			if got != tc.want {
				t.Errorf("shouldRecordSourceCircumstances(jobExists=%v) = %v, want %v — %s",
					tc.jobExists, got, tc.want, tc.why)
			}
		})
	}
}
