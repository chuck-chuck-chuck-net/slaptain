package e2e_test

import "testing"

// TestReplayCleanupNeeded pins the skip-path guard: when BeforeAll skipped
// before assigning markerDN, the AfterAll must do nothing. Without the guard it
// issued conn.Del("") — a delete of the root DSE — and slapd's err 53 was
// reported as "!!! CLEANUP FAILED ... The shared fixture is off baseline",
// which is a false alarm about a marker that was never written.
//
// A plain Go test, not a Ginkgo spec: it must run without a cluster, and it is
// about the scaffolding rather than about slapd.
func TestReplayCleanupNeeded(t *testing.T) {
	if replayCleanupNeeded("") {
		t.Error("replayCleanupNeeded(\"\") = true: the skip path would delete the root DSE and warn about fixture drift")
	}
	if !replayCleanupNeeded("uid=replay-marker,ou=People,dc=example,dc=org") {
		t.Error("replayCleanupNeeded(marker) = false: a marker that WAS written must still be cleaned up")
	}
}
