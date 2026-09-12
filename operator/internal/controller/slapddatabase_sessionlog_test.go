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

// ADR-022: the data DB's syncprov overlay carries an in-memory sessionlog,
// enabled by default. The decision lives in two pure functions — the value
// (desiredSessionlogOps) and the convergence step (planSessionlog) — so the
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

func TestDesiredSessionlogOps(t *testing.T) {
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
			ops, enabled := desiredSessionlogOps(sdWithSessionlog(tc.field))
			if enabled != tc.wantEnabled {
				t.Fatalf("enabled = %v, want %v", enabled, tc.wantEnabled)
			}
			if enabled && ops != tc.wantOps {
				t.Fatalf("ops = %d, want %d", ops, tc.wantOps)
			}
		})
	}
}

func TestDesiredSessionlogOps_DefaultIsNonZero(t *testing.T) {
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
			wantValue:  "5000",
		},
		{
			name:       "already at the wanted value: no write",
			current:    []string{"5000"},
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
			wantValue:  "5000",
		},
		{
			name:       "unparseable live value: replace it",
			current:    []string{"bogus"},
			ops:        5000,
			enabled:    true,
			wantAction: sessionlogSet,
			wantValue:  "5000",
		},
		{
			name:       "disabled with a live value: delete the attribute",
			current:    []string{"5000"},
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
			action, value := planSessionlog(tc.current, tc.ops, tc.enabled)
			if action != tc.wantAction {
				t.Fatalf("action = %v, want %v", action, tc.wantAction)
			}
			if tc.wantAction == sessionlogSet && value != tc.wantValue {
				t.Fatalf("value = %q, want %q", value, tc.wantValue)
			}
		})
	}
}
