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

package cmd

import (
	"strings"
	"testing"
)

// contextCSN is a property of ONE database on ONE pod (ADR-008 amendment
// 2026-09-14). These rows assert that the CLI judges it on that axis: a
// verdict must cover every database, name the database it indicts, and never
// let a healthy database's readings stand in for an unread one.

const (
	db1 = "dc=example,dc=org"
	db2 = "dc=second,dc=example,dc=net"

	// db1's three-SID vector, as measured on a healthy three-pod cluster.
	db1v1 = "20260914190344.682483Z#000000#001#000000"
	db1v2 = "20260914190201.992144Z#000000#002#000000"
	db1v3 = "20260914190208.375499Z#000000#003#000000"
	// The same vector, one SID an hour behind: a pod that is really lagging.
	db1old = "20260914180344.682483Z#000000#001#000000"

	// db2's vector is a different length and different timestamps — two
	// databases are never comparable to each other.
	db2v1  = "20260914190344.711140Z#000000#001#000000"
	db2old = "20260914185344.711140Z#000000#001#000000"
)

func csnPod(name string, bySuffix map[string][]string) csnPodReading {
	return csnPodReading{Pod: name, BySuffix: bySuffix}
}

func TestCheckCSNConvergence(t *testing.T) {
	db1Healthy := []string{db1v1, db1v2, db1v3}
	db2Healthy := []string{db2v1}

	tests := []struct {
		name        string
		readings    []csnPodReading
		wantStatus  string
		wantContain []string
		wantAbsent  []string
	}{
		{
			// Positive control: the single-database cluster this check was
			// written for must behave exactly as before.
			name: "single database, all pods identical",
			readings: []csnPodReading{
				csnPod("slapd-0", map[string][]string{db1: db1Healthy}),
				csnPod("slapd-1", map[string][]string{db1: db1Healthy}),
			},
			wantStatus:  "pass",
			wantContain: []string{"2 pods"},
		},
		{
			// Positive control: a real single-database divergence must still
			// be reported, with the lag.
			name: "single database, one pod behind",
			readings: []csnPodReading{
				csnPod("slapd-0", map[string][]string{db1: db1Healthy}),
				csnPod("slapd-1", map[string][]string{db1: {db1old, db1v2, db1v3}}),
			},
			wantStatus:  "warn",
			wantContain: []string{db1, "slapd-1"},
		},
		{
			name: "two databases, both converged",
			readings: []csnPodReading{
				csnPod("slapd-0", map[string][]string{db1: db1Healthy, db2: db2Healthy}),
				csnPod("slapd-1", map[string][]string{db1: db1Healthy, db2: db2Healthy}),
			},
			wantStatus: "pass",
			// The verdict must state how many databases it covers — a pass
			// that says only "all pods agree" is the defect's own wording.
			wantContain: []string{"2 database"},
		},
		{
			// THE DEFECT: db1 is fine, db2 is behind on one pod. Reading only
			// the first suffix reports a clean pass.
			name: "two databases, the SECOND one diverged",
			readings: []csnPodReading{
				csnPod("slapd-0", map[string][]string{db1: db1Healthy, db2: db2Healthy}),
				csnPod("slapd-1", map[string][]string{db1: db1Healthy, db2: {db2old}}),
			},
			wantStatus:  "warn",
			wantContain: []string{db2, "slapd-1"},
			// The healthy database must not be dragged into the indictment.
			wantAbsent: []string{db1},
		},
		{
			// Strictness, mirroring the operator's CSNQueriesIncomplete: a
			// (pod, database) pair that could not be read never counts toward
			// the good verdict and never silently vanishes.
			name: "a pod with no readable contextCSN for the second database",
			readings: []csnPodReading{
				csnPod("slapd-0", map[string][]string{db1: db1Healthy, db2: db2Healthy}),
				csnPod("slapd-1", map[string][]string{db1: db1Healthy, db2: nil}),
			},
			wantStatus:  "fail",
			wantContain: []string{db2, "slapd-1"},
		},
		{
			// Severity order across databases: a failing database is not
			// diluted by a merely diverged one.
			name: "one database unreadable on a pod, another diverged",
			readings: []csnPodReading{
				csnPod("slapd-0", map[string][]string{db1: db1Healthy, db2: db2Healthy}),
				csnPod("slapd-1", map[string][]string{db1: {db1old, db1v2, db1v3}, db2: nil}),
			},
			wantStatus:  "fail",
			wantContain: []string{db1, db2},
		},
		{
			// "no contextCSN" is not positive evidence of "never synced": a
			// glued suffix entry hides contextCSN with the entry (ADR-025),
			// and this probe is anonymous, so an ACL can deny it.
			name: "the unreadable verdict does not claim to know why",
			readings: []csnPodReading{
				csnPod("slapd-0", map[string][]string{db1: db1Healthy}),
				csnPod("slapd-1", map[string][]string{db1: nil}),
			},
			wantStatus:  "fail",
			wantContain: []string{"glue", "ACL"},
			wantAbsent:  []string{"never synced?"},
		},
		{
			name: "nothing readable anywhere is not a failure",
			readings: []csnPodReading{
				csnPod("slapd-0", map[string][]string{db1: nil}),
				csnPod("slapd-1", map[string][]string{db1: nil}),
			},
			wantStatus: "warn",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := checkCSNConvergence(tc.readings)
			if got.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q (detail: %s)", got.Status, tc.wantStatus, got.Detail)
			}
			for _, want := range tc.wantContain {
				if !strings.Contains(got.Detail, want) {
					t.Errorf("detail %q does not mention %q", got.Detail, want)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(got.Detail, absent) {
					t.Errorf("detail %q must not mention %q", got.Detail, absent)
				}
			}
		})
	}
}
