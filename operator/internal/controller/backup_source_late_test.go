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
)

// ADR-014's source-honesty amendment grants a specific property: recorded just
// before the Job is created, status.sourceContextCSN is a LOWER bound —
// everything in it is certainly in the dump. A record written after the Job
// exists (the ADR-026 R3 repair path) cannot promise that: the source kept
// replicating while the dump was taken. The guarantee must be retracted in the
// open, not silently weakened.
func TestRecordedSourceConvergedConditionCaveatsLateRecording(t *testing.T) {
	now := metav1.Now()
	sc := replicatedCluster(metav1.Condition{
		Type:    "ReplicationConverged",
		Status:  metav1.ConditionTrue,
		Reason:  "CSNsMatch",
		Message: "all local pods report identical contextCSN",
	})

	timely := recordedSourceConvergedCondition(sc, 1, now, false)
	if strings.Contains(timely.Message, "NOT a lower bound") {
		t.Errorf("a timely record must not carry the late caveat: %q", timely.Message)
	}

	late := recordedSourceConvergedCondition(sc, 1, now, true)
	if !strings.Contains(late.Message, "NOT a lower bound") {
		t.Errorf("a late record must say sourceContextCSN is not a lower bound for the artifact: %q", late.Message)
	}
	// The verdict itself is unchanged — the caveat is about the CSN vector, not
	// about convergence, and the reason vocabulary is a published contract.
	if late.Status != timely.Status || late.Reason != timely.Reason {
		t.Errorf("late recording must not change the verdict: got %s/%s, want %s/%s",
			late.Status, late.Reason, timely.Status, timely.Reason)
	}
	if !strings.HasPrefix(late.Message, timely.Message) {
		t.Errorf("the late message must extend the timely one, not replace it:\n timely=%q\n late=%q", timely.Message, late.Message)
	}
}
