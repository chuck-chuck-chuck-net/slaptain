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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

const (
	// wantDefaultPurge is a representative composed purge value, used as an
	// opaque string by the generic overlay-attribute convergence table below.
	wantDefaultPurge   = "7+00:00 1+00:00"
	explicitPurge      = "2+00:00 1+00:00"
	explicitCheckpoint = "500 15"
)

// ADR-024 R4/R5: the accesslog purge window and the syncprov checkpoint are
// converged on every reconcile, not written once at overlay creation. The
// decision lives in pure functions — the resolved value
// (resolveRetention + planPodPurge / desiredSyncprovCheckpoint) and the
// convergence step (planOverlayAttr) — so the live-LDAP half stays a thin
// executor, exactly as ADR-022 did it for the sessionlog.
//
// Both attributes were verified live-modifiable on a running slapd before being
// classed R4 (the precondition the ADR-024 amendments of 2026-09-12 and
// 2026-09-13 made mandatory): replace and delete, both attributes, instantaneous,
// pod still serving and still accepting data writes afterwards.
//
// The purge DIRECTIVE's own resolution moved to accesslog_purge_converged_test.go
// when ADR-024 R6 decomposed it into typed fields with three owners.

// slapdDatabaseFixture declares the three states of the decomposed purge block
// without a table having to build a CR by hand. A nil fixture is a nil
// database; a zero fixture is a database with no accesslogPurge at all, which
// is what every existing CR looks like and must keep meaning "the operator's
// defaults".
//
// retentionSet exists because zero is a REAL declared value here — "purge
// everything immediately" — and must stay distinguishable from "not declared".
// That is the same distinction the pointer in the API carries, reproduced in
// the fixture so a table can assert on it.
type slapdDatabaseFixture struct {
	enabled      *bool
	retention    time.Duration
	retentionSet bool
}

func (f *slapdDatabaseFixture) build() *ldapv1alpha1.SlapdDatabase {
	if f == nil {
		return nil
	}
	sd := &ldapv1alpha1.SlapdDatabase{}
	if f.enabled == nil && f.retention == 0 && !f.retentionSet {
		return sd
	}
	block := &ldapv1alpha1.AccesslogPurgeConfig{Enabled: f.enabled}
	if f.retention != 0 || f.retentionSet {
		block.Retention = &metav1.Duration{Duration: f.retention}
	}
	sd.Spec.Replication.AccesslogPurge = block
	return sd
}

// sdPurgeDisabled is the explicit "never purge, keep forever" opt-out that
// replaced the "none" sentinel.
func sdPurgeDisabled() *ldapv1alpha1.SlapdDatabase {
	return (&slapdDatabaseFixture{enabled: new(false)}).build()
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

// ADR-045: the DATA database's checkpoint defaults to "100 10". Unset is not
// "slapd's own behaviour" — it is slapd's behaviour minus the startup repair
// upstream assumes is there, because FIND_MAXCSN is gated !SLAP_DBCLEAN and
// back-mdb is always clean (syncprov.c:4096, back-mdb/init.c:117). The
// unflushed contextCSN is then regenerated AHEAD of content on a
// non-replicated cluster, where no consumer side exists to repair it.
//
// Whitespace resolves to the default rather than to "", mirroring
// desiredAccesslogSyncprovCheckpoint: a field a user left blank in any
// spelling is a field they did not fill in, and only the explicit sentinel
// means "I really want none".
func TestDesiredSyncprovCheckpoint(t *testing.T) {
	cases := []struct {
		name  string
		field string
		want  string
	}{
		{"unset takes the data DB default", "", syncprovCheckpointDefault},
		{"whitespace-only is still unset", "  ", syncprovCheckpointDefault},
		{"none opts out explicitly", noneSentinel, ""},
		{"the sentinel is case-insensitive", "NONE", ""},
		{"an explicit checkpoint is used verbatim", explicitCheckpoint, explicitCheckpoint},
		{"an explicit checkpoint is trimmed", " 1000 10 ", "1000 10"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := desiredSyncprovCheckpoint(sdWithCheckpoint(tc.field)); got != tc.want {
				t.Fatalf("desiredSyncprovCheckpoint(%q) = %q, want %q", tc.field, got, tc.want)
			}
		})
	}

	t.Run("a nil database takes the default too", func(t *testing.T) {
		if got := desiredSyncprovCheckpoint(nil); got != syncprovCheckpointDefault {
			t.Fatalf("desiredSyncprovCheckpoint(nil) = %q, want %q", got, syncprovCheckpointDefault)
		}
	})
}

// The default's VALUE is pinned once, as a literal, for the reason the
// accesslog's equivalent is: every other assertion here uses the constant, so
// a change to it would move both sides together and none of them could catch
// it. "100 10" is the ecosystem's number (Symas, Ubuntu, Bitnami), not the man
// page's "500 15" example that reached our own fixtures.
func TestSyncprovCheckpointDefaultValue(t *testing.T) {
	if syncprovCheckpointDefault != "100 10" {
		t.Errorf("syncprovCheckpointDefault = %q, want \"100 10\" (ADR-045 option D)",
			syncprovCheckpointDefault)
	}
}

// The default is worth nothing unless it CONVERGES onto a syncprov overlay
// that already exists — every overlay on every cluster predating this release
// is bare, and ensureSyncprovTunables is what reaches them (ADR-024 R4).
func TestTheDataCheckpointDefaultConvergesOntoAnExistingBareOverlay(t *testing.T) {
	action, value := planOverlayAttr(nil, desiredSyncprovCheckpoint(nil))
	if action != overlayAttrSet {
		t.Fatalf("a bare syncprov overlay must be given the checkpoint, got action %v", action)
	}
	if value != syncprovCheckpointDefault {
		t.Errorf("converged value = %q, want %q", value, syncprovCheckpointDefault)
	}

	// And the opt-out must REMOVE one that is present, not merely decline to
	// write one — the sentinel's whole job is restoring slapd's own behaviour.
	action, _ = planOverlayAttr([]string{syncprovCheckpointDefault},
		desiredSyncprovCheckpoint(sdWithCheckpoint(noneSentinel)))
	if action != overlayAttrRemove {
		t.Errorf("the none sentinel must REMOVE a live checkpoint, got action %v", action)
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
			want:       wantDefaultPurge,
			wantAction: overlayAttrSet,
			wantValue:  wantDefaultPurge,
		},
		{
			name:       "already at the wanted value: no write",
			current:    []string{wantDefaultPurge},
			want:       wantDefaultPurge,
			wantAction: overlayAttrNoop,
		},
		{
			name:       "whitespace around the live value is not a difference",
			current:    []string{"  7+00:00 1+00:00  "},
			want:       wantDefaultPurge,
			wantAction: overlayAttrNoop,
		},
		{
			name:       "internal respacing by slapd is not a difference",
			current:    []string{"500\t15"},
			want:       explicitCheckpoint,
			wantAction: overlayAttrNoop,
		},
		{
			name:       "a different live value: replace it",
			current:    []string{explicitPurge},
			want:       wantDefaultPurge,
			wantAction: overlayAttrSet,
			wantValue:  wantDefaultPurge,
		},
		{
			name:       "an unexpectedly multi-valued attribute is replaced",
			current:    []string{explicitPurge, wantDefaultPurge},
			want:       wantDefaultPurge,
			wantAction: overlayAttrSet,
			wantValue:  wantDefaultPurge,
		},
		{
			name:       "not wanted with a live value: delete the attribute",
			current:    []string{explicitCheckpoint},
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
