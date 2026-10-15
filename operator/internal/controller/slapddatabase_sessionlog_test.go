/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"testing"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// wantDefaultSessionlog pins defaultSyncprovSessionlogOps as written.
const wantDefaultSessionlog = "5000"

// ADR-022: the data DB's syncprov overlay carries an in-memory sessionlog,
// enabled by default. The decision lives in two pure functions — the value
// (desiredSessionlog) and the convergence step (planSessionlog) — so the
// live-LDAP half stays a thin executor.

func sdWithSessionlog(v *int32) *ldapv1alpha1.SlapdDatabase {
	return &ldapv1alpha1.SlapdDatabase{
		Spec: ldapv1alpha1.SlapdDatabaseSpec{
			Replication: ldapv1alpha1.DatabaseReplicationConfig{
				SyncprovSessionlog: v,
			},
		},
	}
}

func TestDesiredSessionlogInMemoryOps(t *testing.T) {
	i := func(v int32) *int32 { return &v }

	cases := []struct {
		name        string
		field       *int32
		wantOps     int32
		wantEnabled bool
	}{
		{"unset falls back to the operator default", nil, defaultSyncprovSessionlogOps, true},
		{"zero disables the sessionlog", i(0), 0, false},
		{"explicit value is used verbatim", i(42), 42, true},
		{"a large explicit value is not clamped", i(100000), 100000, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ops, enabled := desiredSessionlog(sdWithSessionlog(tc.field)).inMemoryOps()
			if enabled != tc.wantEnabled {
				t.Fatalf("enabled = %v, want %v", enabled, tc.wantEnabled)
			}
			if enabled && ops != tc.wantOps {
				t.Fatalf("ops = %d, want %d", ops, tc.wantOps)
			}
		})
	}
}

func TestDefaultSyncprovSessionlogOps_IsNonZero(t *testing.T) {
	// The default is the whole point of ADR-022 (best config by default); a
	// zero here would silently mean "disabled everywhere".
	if defaultSyncprovSessionlogOps <= 0 {
		t.Fatalf("defaultSyncprovSessionlogOps = %d, want > 0", defaultSyncprovSessionlogOps)
	}
}

func TestPlanSessionlog(t *testing.T) {
	cases := []struct {
		name       string
		current    []string
		ops        int32
		enabled    bool
		wantAction sessionlogAction
		wantValue  string
	}{
		{
			name:       "absent and wanted: set it",
			current:    nil,
			ops:        5000,
			enabled:    true,
			wantAction: sessionlogSet,
			wantValue:  wantDefaultSessionlog,
		},
		{
			name:       "already at the wanted value: no write",
			current:    []string{wantDefaultSessionlog},
			ops:        5000,
			enabled:    true,
			wantAction: sessionlogNoop,
		},
		{
			name:       "whitespace around the live value is not a difference",
			current:    []string{" 5000 "},
			ops:        5000,
			enabled:    true,
			wantAction: sessionlogNoop,
		},
		{
			name:       "different live value: replace it",
			current:    []string{"100"},
			ops:        5000,
			enabled:    true,
			wantAction: sessionlogSet,
			wantValue:  wantDefaultSessionlog,
		},
		{
			name:       "unparseable live value: replace it",
			current:    []string{"bogus"},
			ops:        5000,
			enabled:    true,
			wantAction: sessionlogSet,
			wantValue:  wantDefaultSessionlog,
		},
		{
			name:       "disabled with a live value: delete the attribute",
			current:    []string{wantDefaultSessionlog},
			enabled:    false,
			wantAction: sessionlogRemove,
		},
		{
			name:       "disabled and absent: nothing to do",
			current:    nil,
			enabled:    false,
			wantAction: sessionlogNoop,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// No olcSpSessionlogSource live on the pod: the pre-ADR-034
			// situation, where the in-memory log is the only one there is.
			action, value := planSessionlog(tc.current, inMemoryDecision(tc.ops, tc.enabled), false)
			if action != tc.wantAction {
				t.Fatalf("action = %v, want %v", action, tc.wantAction)
			}
			if tc.wantAction == sessionlogSet && value != tc.wantValue {
				t.Fatalf("value = %q, want %q", value, tc.wantValue)
			}
		})
	}
}

// ADR-034: the session log also has a PLACE. syncprovSessionlogSource=Accesslog
// moves it out of RAM and onto the database's own change journal
// (olcSpSessionlogSource → cn=accesslog-<name>), and asking for it REPLACES the
// in-memory log — the two together are the combination slapd calls wasteful.
// One pure function answers all three outcomes; the LDAP half and the init
// container's offline converger are both executors of its verdict. WHEN the
// replacement happens is a second question, answered in
// slapddatabase_sessionlog_handover_test.go.

func sdWithSessionlogSource(name, source string, v *int32) *ldapv1alpha1.SlapdDatabase {
	sd := sdWithSessionlog(v)
	sd.Name = name
	sd.Spec.Replication.SyncprovSessionlogSource = source
	return sd
}

func TestDesiredSessionlog(t *testing.T) {
	i := func(v int32) *int32 { return &v }

	cases := []struct {
		name     string
		dbName   string
		source   string
		field    *int32
		wantKind sessionlogKind
		wantOps  int32
		wantDN   string
	}{
		{
			name:     "both unset: the in-memory log at the operator default",
			dbName:   "default",
			source:   "",
			field:    nil,
			wantKind: sessionlogKindInMemory,
			wantOps:  defaultSyncprovSessionlogOps,
		},
		{
			name:     "explicit InMemory is the same as unset",
			dbName:   "default",
			source:   ldapv1alpha1.SyncprovSessionlogSourceInMemory,
			field:    i(42),
			wantKind: sessionlogKindInMemory,
			wantOps:  42,
		},
		{
			name:     "zero ops with no source: no session log at all",
			dbName:   "default",
			source:   "",
			field:    i(0),
			wantKind: sessionlogKindNone,
		},
		{
			name:     "Accesslog sources it from this database's own journal",
			dbName:   "default",
			source:   ldapv1alpha1.SyncprovSessionlogSourceAccesslog,
			field:    nil,
			wantKind: sessionlogKindAccesslog,
			wantDN:   "cn=accesslog-default",
		},
		{
			name:     "Accesslog names the journal of THIS database, not a fixed DN",
			dbName:   "people",
			source:   ldapv1alpha1.SyncprovSessionlogSourceAccesslog,
			field:    nil,
			wantKind: sessionlogKindAccesslog,
			wantDN:   "cn=accesslog-people",
		},
		{
			name:     "Accesslog with an explicit 0 is still Accesslog",
			dbName:   "default",
			source:   ldapv1alpha1.SyncprovSessionlogSourceAccesslog,
			field:    i(0),
			wantKind: sessionlogKindAccesslog,
			wantDN:   "cn=accesslog-default",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := desiredSessionlog(sdWithSessionlogSource(tc.dbName, tc.source, tc.field))
			if got.Kind != tc.wantKind {
				t.Fatalf("kind = %v, want %v", got.Kind, tc.wantKind)
			}
			if tc.wantKind == sessionlogKindInMemory && got.Ops != tc.wantOps {
				t.Fatalf("ops = %d, want %d", got.Ops, tc.wantOps)
			}
			if got.SourceDN != tc.wantDN {
				t.Fatalf("sourceDN = %q, want %q", got.SourceDN, tc.wantDN)
			}
			if tc.wantKind != sessionlogKindInMemory && got.Ops != 0 {
				t.Fatalf("ops = %d, want 0 when the in-memory log is not wanted", got.Ops)
			}
		})
	}
}

// The DN must come from the one definition of the accesslog suffix (ADR-019
// R1), never from a second string literal that can drift from it.
func TestDesiredSessionlog_SourceDNIsTheAccesslogSuffix(t *testing.T) {
	got := desiredSessionlog(sdWithSessionlogSource("payroll",
		ldapv1alpha1.SyncprovSessionlogSourceAccesslog, nil))
	if want := ldapv1alpha1.AccesslogSuffix("payroll"); got.SourceDN != want {
		t.Fatalf("sourceDN = %q, want %q (AccesslogSuffix)", got.SourceDN, want)
	}
}

// Asking for the accesslog source must take the in-memory log away eventually,
// not merely recommend against it: the two together are what slapd calls
// wasteful. WHEN it goes is planSessionlog's call and not this one's — the
// removal waits until the sourced log is actually live on the pod, which is the
// pod's next start (see slapddatabase_sessionlog_handover_test.go).
func TestDesiredSessionlog_AccesslogExcludesTheInMemoryLog(t *testing.T) {
	i := func(v int32) *int32 { return &v }

	for _, field := range []*int32{nil, i(0), i(5000)} {
		sd := sdWithSessionlogSource("default",
			ldapv1alpha1.SyncprovSessionlogSourceAccesslog, field)
		if _, enabled := desiredSessionlog(sd).inMemoryOps(); enabled {
			t.Fatalf("syncprovSessionlog=%v with source=Accesslog: enabled = true, want false", field)
		}
	}
}
