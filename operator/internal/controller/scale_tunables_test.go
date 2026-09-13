package controller

import (
	"strings"
	"testing"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

func ptrBool(b bool) *bool  { return &b }
func ptrI32(i int32) *int32 { return &i }

func clusterFor() *ldapv1alpha1.SlapdCluster {
	return &ldapv1alpha1.SlapdCluster{}
}

// ── Durability family: noSync / checkpoint (findings 6, 7) ───────────────────

func TestDesiredNoSync(t *testing.T) {
	cases := []struct {
		name    string
		cluster *bool
		db      *bool
		want    bool
	}{
		{"both unset is fsync-on", nil, nil, false},
		{"cluster posture inherited", ptrBool(true), nil, true},
		{"database overrides cluster off", ptrBool(false), ptrBool(true), true},
		{"database overrides cluster on", ptrBool(true), ptrBool(false), false},
		{"explicit false on both", ptrBool(false), ptrBool(false), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := clusterFor()
			sc.Spec.Tuning.NoSync = tc.cluster
			sd := &ldapv1alpha1.SlapdDatabase{}
			sd.Spec.NoSync = tc.db
			if got := desiredNoSync(sd, sc); got != tc.want {
				t.Errorf("desiredNoSync = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDesiredCheckpoint(t *testing.T) {
	cases := []struct {
		name      string
		spec      *string
		wantVal   string
		wantWrite bool
	}{
		{"unset takes the operator default", nil, defaultDataCheckpoint, true},
		{"explicit value is honoured", ptrStr("4096 15"), "4096 15", true},
		{"empty string means write nothing", ptrStr(""), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sd := &ldapv1alpha1.SlapdDatabase{}
			sd.Spec.Checkpoint = tc.spec
			gotVal, gotWrite := desiredCheckpoint(sd)
			if gotVal != tc.wantVal || gotWrite != tc.wantWrite {
				t.Errorf("desiredCheckpoint = (%q, %v), want (%q, %v)",
					gotVal, gotWrite, tc.wantVal, tc.wantWrite)
			}
		})
	}
}

// The unsafe combination the gap analysis names: noSync with no checkpoint at
// all loses an unbounded window of writes on an unclean shutdown. It must not be
// reachable by accident — ADR-024 R4's "rejected or converged, never ignored".
func TestValidateDurability(t *testing.T) {
	cases := []struct {
		name    string
		noSync  *bool
		ckpt    *string
		wantErr bool
	}{
		{"defaults are safe", nil, nil, false},
		{"noSync with the default checkpoint is safe", ptrBool(true), nil, false},
		{"noSync with an explicit checkpoint is safe", ptrBool(true), ptrStr("2048 10"), false},
		{"checkpoint disabled while fsync is on is fine", nil, ptrStr(""), false},
		{"noSync with checkpoint disabled is rejected", ptrBool(true), ptrStr(""), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := clusterFor()
			sd := &ldapv1alpha1.SlapdDatabase{}
			sd.Spec.NoSync = tc.noSync
			sd.Spec.Checkpoint = tc.ckpt
			err := validateDurability(sd, sc)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateDurability err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "checkpoint") {
				t.Errorf("error should name the field that fixes it, got %q", err)
			}
		})
	}
}

// The cluster-level posture must reach the validation too, otherwise turning
// noSync on cluster-wide silently bypasses the check a per-database noSync hits.
func TestValidateDurabilityInheritsClusterPosture(t *testing.T) {
	sc := clusterFor()
	sc.Spec.Tuning.NoSync = ptrBool(true)
	sd := &ldapv1alpha1.SlapdDatabase{}
	sd.Spec.Checkpoint = ptrStr("")
	if err := validateDurability(sd, sc); err == nil {
		t.Fatal("cluster-wide noSync + checkpoint disabled must be rejected")
	}
}

// ── olcDbRtxnSize (finding 9) ───────────────────────────────────────────────

func TestDesiredRtxnSize(t *testing.T) {
	sd := &ldapv1alpha1.SlapdDatabase{}
	if got := desiredRtxnSize(sd); got != defaultRtxnSize {
		t.Errorf("unset rtxnSize = %d, want the operator default %d", got, defaultRtxnSize)
	}
	sd.Spec.RtxnSize = ptrI32(0)
	if got := desiredRtxnSize(sd); got != 0 {
		t.Errorf("explicit 0 must survive as 0, got %d", got)
	}
	sd.Spec.RtxnSize = ptrI32(50000)
	if got := desiredRtxnSize(sd); got != 50000 {
		t.Errorf("explicit value = %d, want 50000", got)
	}
}

// ── olcDbEnvFlags (finding 8) — create-only, compared for reporting ──────────

func TestEnvFlagsMatch(t *testing.T) {
	cases := []struct {
		name             string
		current, desired []string
		want             bool
	}{
		{"both empty", nil, nil, true},
		{"same single flag", []string{"nometasync"}, []string{"nometasync"}, true},
		{"order does not matter", []string{"nometasync", "writemap"},
			[]string{"writemap", "nometasync"}, true},
		{"case does not matter", []string{"WriteMap"}, []string{"writemap"}, true},
		{"missing on the database", nil, []string{"writemap"}, false},
		{"extra on the database", []string{"writemap"}, nil, false},
		{"different flag", []string{"nosync"}, []string{"nometasync"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := envFlagsMatch(tc.current, tc.desired); got != tc.want {
				t.Errorf("envFlagsMatch(%v, %v) = %v, want %v",
					tc.current, tc.desired, got, tc.want)
			}
		})
	}
}

// ── Server-global tuning (finding 10) ───────────────────────────────────────

func TestGlobalTuningDefaults(t *testing.T) {
	sc := clusterFor()
	if got := desiredToolThreads(sc); got != defaultToolThreads {
		t.Errorf("toolThreads = %d, want %d", got, defaultToolThreads)
	}
	sc.Spec.Tuning.ToolThreads = ptrI32(8)
	if got := desiredToolThreads(sc); got != 8 {
		t.Errorf("explicit toolThreads = %d, want 8", got)
	}
}

// ── TLS posture (finding 15) ────────────────────────────────────────────────

func TestDesiredTLSPosture(t *testing.T) {
	sc := clusterFor()
	if got := desiredTLSProtocolMin(sc); got != defaultTLSProtocolMin {
		t.Errorf("protocolMin = %q, want the operator default %q", got, defaultTLSProtocolMin)
	}
	if defaultTLSProtocolMin != "3.3" {
		t.Errorf("the floor must be TLS 1.2 (3.3), got %q", defaultTLSProtocolMin)
	}
	if _, write := desiredTLSCipherSuite(sc); write {
		t.Error("cipherSuite must not be written when unset — OpenSSL's list is better maintained")
	}
	sc.Spec.LDAP.TLS.ProtocolMin = ptrStr("3.4")
	if got := desiredTLSProtocolMin(sc); got != "3.4" {
		t.Errorf("explicit protocolMin = %q, want 3.4", got)
	}
	sc.Spec.LDAP.TLS.CipherSuite = ptrStr("HIGH:!aNULL")
	v, write := desiredTLSCipherSuite(sc)
	if !write || v != "HIGH:!aNULL" {
		t.Errorf("explicit cipherSuite = (%q, %v)", v, write)
	}
}

// ── Password hash (finding 16) ──────────────────────────────────────────────

func TestDesiredPasswordHash(t *testing.T) {
	sc := clusterFor()
	if got := desiredPasswordHash(sc); got != defaultPasswordHash {
		t.Errorf("passwordHash = %q, want %q", got, defaultPasswordHash)
	}
	sc.Spec.LDAP.PasswordHash = ptrStr("{CRYPT}")
	if got := desiredPasswordHash(sc); got != "{CRYPT}" {
		t.Errorf("explicit passwordHash = %q", got)
	}
}

// ── Log level (finding 14) ──────────────────────────────────────────────────

func TestDesiredLogLevel(t *testing.T) {
	sc := clusterFor()
	got := desiredLogLevel(sc)
	if got != defaultLogLevel {
		t.Errorf("logLevel = %d, want %d", got, defaultLogLevel)
	}
	if got&256 == 0 || got&16384 == 0 {
		t.Errorf("the default %d must carry both stats (256) and sync (16384)", got)
	}
	if got&32768 != 0 || got == -1 {
		t.Errorf("the default %d must not include the provider-side firehose", got)
	}
	// The whole reason LogLevel is a pointer: silence must be expressible.
	sc.Spec.LogLevel = ptrI32(0)
	if got := desiredLogLevel(sc); got != 0 {
		t.Errorf("explicit logLevel 0 = %d, want 0", got)
	}
}

// ── Syncrepl stanza hardening (finding 11) ──────────────────────────────────

func TestDesiredKeepalive(t *testing.T) {
	sc := clusterFor()
	if got := desiredKeepalive(sc); got != defaultKeepalive {
		t.Errorf("keepalive = %q, want the operator default %q", got, defaultKeepalive)
	}
	if strings.Count(defaultKeepalive, ":") != 2 {
		t.Errorf("keepalive must be an idle:probes:interval triple, got %q", defaultKeepalive)
	}
	sc.Spec.Replication.Keepalive = "60:2:10"
	if got := desiredKeepalive(sc); got != "60:2:10" {
		t.Errorf("explicit keepalive = %q", got)
	}
	sc.Spec.Replication.Keepalive = "none"
	if got := desiredKeepalive(sc); got != "" {
		t.Errorf(`keepalive "none" must opt out entirely, got %q`, got)
	}
}
