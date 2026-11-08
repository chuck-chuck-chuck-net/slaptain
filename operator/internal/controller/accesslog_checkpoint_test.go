package controller

import (
	"testing"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The accesslog DB's syncprov checkpoint is the ITS#9398 storm precondition's
// only known cure (docs/ITS-9398-REPRODUCTION.md §Controls). Both checkpoints
// default on since ADR-045; this one defaults TIGHTER, because its absence is a
// permanent correctness cliff where the data DB's is bounded and self-healing.
//
// The value is not a dial. `chkops = 1` is load-bearing: in accesslog_response
// the log entry is added BEFORE minCSN is modified, so si_chkops=1 checkpoints
// on that very operation (syncprov.c:2607, :2615) and contextCSN reaches disk
// before minCSN gains the new serverID. Any larger value reopens the window
// between two adjacent operations, which is what the `100 5` control measured.
func TestDesiredAccesslogSyncprovCheckpoint(t *testing.T) {
	db := func(v string) *ldapv1alpha1.SlapdDatabase {
		sd := &ldapv1alpha1.SlapdDatabase{}
		sd.Spec.Replication.AccesslogSyncprovCheckpoint = v
		return sd
	}
	cases := []struct {
		name string
		sd   *ldapv1alpha1.SlapdDatabase
		want string
	}{
		{"unset takes the operator default", db(""), accesslogSyncprovCheckpointDefault},
		{"whitespace is still unset", db("   "), accesslogSyncprovCheckpointDefault},
		{"nil database takes the default too", nil, accesslogSyncprovCheckpointDefault},
		{"an explicit value is used verbatim", db("100 5"), "100 5"},
		{"the none sentinel opts out entirely", db("none"), ""},
		{"the sentinel is case-insensitive", db("NONE"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := desiredAccesslogSyncprovCheckpoint(tc.sd); got != tc.want {
				t.Errorf("desiredAccesslogSyncprovCheckpoint(%v) = %q, want %q", tc.sd, got, tc.want)
			}
		})
	}
}

// Both default on, and they must not default to the SAME value. Pinning that
// here because the two are one `grep olcSpCheckpoint` apart and the reason they
// differ is not visible at either call site: the log DB needs chkops = 1 to
// close the ITS#9398 window on the logging operation itself, while the data DB
// would pay that on the hot write path for a staleness that is bounded and
// self-healing (ADR-045 R4).
func TestTheTwoCheckpointsDefaultDifferently(t *testing.T) {
	sd := &ldapv1alpha1.SlapdDatabase{}
	data := desiredSyncprovCheckpoint(sd)
	accesslog := desiredAccesslogSyncprovCheckpoint(sd)
	if data == "" {
		t.Error("the DATA DB checkpoint must default ON — unset regenerates contextCSN ahead of content (ADR-045)")
	}
	if accesslog == "" {
		t.Error("the ACCESSLOG checkpoint must default ON — its absence is the ITS#9398 precondition")
	}
	if data == accesslog {
		t.Errorf("both checkpoints default to %q; the log DB's chkops=1 is not a value the data DB's hot path should pay", data)
	}
}

// An existing cluster's accesslog overlay is bare: the overlay is configured at
// ADD time only, so nothing has ever written olcSpCheckpoint to one. The
// default is therefore worth nothing unless it CONVERGES onto overlays that
// already exist.
func TestTheDefaultConvergesOntoAnExistingBareOverlay(t *testing.T) {
	action, value := planOverlayAttr(nil, desiredAccesslogSyncprovCheckpoint(nil))
	if action != overlayAttrSet {
		t.Fatalf("a bare overlay must be given the checkpoint, got action %v", action)
	}
	if value != accesslogSyncprovCheckpointDefault {
		t.Errorf("converged value = %q, want %q", value, accesslogSyncprovCheckpointDefault)
	}
	// And the opt-out must remove one that is present, not leave it.
	sd := &ldapv1alpha1.SlapdDatabase{}
	sd.Spec.Replication.AccesslogSyncprovCheckpoint = noneSentinel
	action, _ = planOverlayAttr([]string{accesslogSyncprovCheckpointDefault}, desiredAccesslogSyncprovCheckpoint(sd))
	if action != overlayAttrRemove {
		t.Errorf("the none sentinel must REMOVE a live checkpoint, got action %v", action)
	}
}

// The default's VALUE is pinned here as a literal, on purpose and exactly once.
// Every other reference in this file now uses the constant, so without this
// test a change to the constant would move both sides of every assertion
// together and none of them could catch it.
func TestAccesslogSyncprovCheckpointDefaultValue(t *testing.T) {
	if accesslogSyncprovCheckpointDefault != "1 1" {
		t.Errorf("accesslogSyncprovCheckpointDefault = %q, want \"1 1\" — a checkpoint every write and every second (ADR-037)",
			accesslogSyncprovCheckpointDefault)
	}
}
