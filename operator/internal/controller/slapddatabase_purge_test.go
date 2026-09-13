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

// ADR-024 R4/R5: the accesslog purge window and the syncprov checkpoint are
// converged on every reconcile, not written once at overlay creation. The
// decision lives in pure functions — the resolved value
// (desiredAccesslogPurge / desiredSyncprovCheckpoint) and the convergence step
// (planOverlayAttr) — so the live-LDAP half stays a thin executor, exactly as
// ADR-022 did it for the sessionlog.
//
// Both attributes were verified live-modifiable on a running slapd before being
// classed R4 (the precondition the ADR-024 amendments of 2026-09-12 and
// 2026-09-13 made mandatory): replace and delete, both attributes, instantaneous,
// pod still serving and still accepting data writes afterwards.

func sdWithPurge(v string) *ldapv1alpha1.SlapdDatabase {
	return &ldapv1alpha1.SlapdDatabase{
		Spec: ldapv1alpha1.SlapdDatabaseSpec{
			Replication: ldapv1alpha1.DatabaseReplicationConfig{
				AccesslogPurge: v,
			},
		},
	}
}

func sdWithCheckpoint(v string) *ldapv1alpha1.SlapdDatabase {
	return &ldapv1alpha1.SlapdDatabase{
		Spec: ldapv1alpha1.SlapdDatabaseSpec{
			Replication: ldapv1alpha1.DatabaseReplicationConfig{
				SyncprovCheckpoint: v,
			},
		},
	}
}

func TestDesiredAccesslogPurge(t *testing.T) {
	cases := []struct {
		name  string
		field string
		want  string
	}{
		{"unset falls back to the operator default", "", defaultAccesslogPurge},
		{"whitespace-only is still unset", "   ", defaultAccesslogPurge},
		{"none opts out of purging entirely", "none", ""},
		{"none is matched case-insensitively", "None", ""},
		{"none tolerates surrounding whitespace", "  none  ", ""},
		{"an explicit window is used verbatim", "2+00:00 1+00:00", "2+00:00 1+00:00"},
		{"an explicit window is trimmed", "  14+00:00 1+00:00 ", "14+00:00 1+00:00"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := desiredAccesslogPurge(sdWithPurge(tc.field)); got != tc.want {
				t.Fatalf("desiredAccesslogPurge(%q) = %q, want %q", tc.field, got, tc.want)
			}
		})
	}
}

// The default is the whole point of the R5 half of this batch: an unpurged
// journal grows to its map ceiling and then stops taking writes — and because
// the accesslog overlay sits in the DATA write path, data writes stop with it.
// An empty default here would silently restore exactly that failure mode.
func TestDefaultAccesslogPurge_IsAWindowNotNothing(t *testing.T) {
	if defaultAccesslogPurge == "" {
		t.Fatal("defaultAccesslogPurge is empty — unset would mean no purge at all")
	}
	if want := "7+00:00 1+00:00"; defaultAccesslogPurge != want {
		t.Fatalf("defaultAccesslogPurge = %q, want %q (7-day window, daily sweep)",
			defaultAccesslogPurge, want)
	}
}

func TestDesiredSyncprovCheckpoint(t *testing.T) {
	cases := []struct {
		name  string
		field string
		want  string
	}{
		{"unset means no checkpoint attribute", "", ""},
		{"whitespace-only is still unset", "  ", ""},
		{"none opts out explicitly", "none", ""},
		{"an explicit checkpoint is used verbatim", "500 15", "500 15"},
		{"an explicit checkpoint is trimmed", " 1000 10 ", "1000 10"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := desiredSyncprovCheckpoint(sdWithCheckpoint(tc.field)); got != tc.want {
				t.Fatalf("desiredSyncprovCheckpoint(%q) = %q, want %q", tc.field, got, tc.want)
			}
		})
	}
}

func TestPlanOverlayAttr(t *testing.T) {
	cases := []struct {
		name       string
		current    []string
		want       string
		wantAction overlayAttrAction
		wantValue  string
	}{
		{
			name:       "absent and wanted: set it",
			current:    nil,
			want:       "7+00:00 1+00:00",
			wantAction: overlayAttrSet,
			wantValue:  "7+00:00 1+00:00",
		},
		{
			name:       "already at the wanted value: no write",
			current:    []string{"7+00:00 1+00:00"},
			want:       "7+00:00 1+00:00",
			wantAction: overlayAttrNoop,
		},
		{
			name:       "whitespace around the live value is not a difference",
			current:    []string{"  7+00:00 1+00:00  "},
			want:       "7+00:00 1+00:00",
			wantAction: overlayAttrNoop,
		},
		{
			name:       "internal respacing by slapd is not a difference",
			current:    []string{"500\t15"},
			want:       "500 15",
			wantAction: overlayAttrNoop,
		},
		{
			name:       "a different live value: replace it",
			current:    []string{"2+00:00 1+00:00"},
			want:       "7+00:00 1+00:00",
			wantAction: overlayAttrSet,
			wantValue:  "7+00:00 1+00:00",
		},
		{
			name:       "an unexpectedly multi-valued attribute is replaced",
			current:    []string{"2+00:00 1+00:00", "7+00:00 1+00:00"},
			want:       "7+00:00 1+00:00",
			wantAction: overlayAttrSet,
			wantValue:  "7+00:00 1+00:00",
		},
		{
			name:       "not wanted with a live value: delete the attribute",
			current:    []string{"500 15"},
			want:       "",
			wantAction: overlayAttrRemove,
		},
		{
			name:       "not wanted and absent: nothing to do",
			current:    nil,
			want:       "",
			wantAction: overlayAttrNoop,
		},
		{
			name:       "not wanted and present but blank: nothing to do",
			current:    []string{"  "},
			want:       "",
			wantAction: overlayAttrNoop,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			action, value := planOverlayAttr(tc.current, tc.want)
			if action != tc.wantAction {
				t.Fatalf("action = %v, want %v", action, tc.wantAction)
			}
			if tc.wantAction == overlayAttrSet && value != tc.wantValue {
				t.Fatalf("value = %q, want %q", value, tc.wantValue)
			}
		})
	}
}
