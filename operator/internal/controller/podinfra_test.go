/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"errors"
	"strings"
	"testing"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// Per-pod cn=config convergence must not let one surface's failure skip the
// others.
//
// Measured on t3e, 2026-09-15, reproducing the mesh run's scale-up failure. A
// cluster with spec.ldap.tls.enabled=false makes slapd refuse olcTLSProtocolMin:
//
//	set olcTLSProtocolMin on cn=config at slapd-scaleup-0…: LDAP Result Code 53
//	"Unwilling To Perform"
//
// reconcilePodInfrastructure returned on that error, so ensureAuthDB — listed
// after it — never ran on that cluster at all. The node-local auth database was
// therefore never created, and the identity deferred forever:
//
//	auth identity deferred: auth database not created yet   ×34 per pod
//	syncrepl configuration withheld …                       ×36
//
// Two nested short-circuits, both PRE-EXISTING and both harmless until now: the
// attribute loop inside ensureGlobalTunables abandons the remaining attributes,
// and reconcilePodInfrastructure abandons the remaining steps. ADR-027's cutover
// is what made the second one fatal — the auth database went from unused
// decoration to a hard precondition for replication, so a step skipped by an
// unrelated tunable became a permanent replication outage on any cluster whose
// tunables cannot be written.
//
// The rule these pin: independent convergence steps are INDEPENDENT. Each runs,
// each failure is reported, none is allowed to hide the others.
func TestRunConvergenceSteps(t *testing.T) {
	t.Run("a failing step does not skip the ones after it", func(t *testing.T) {
		var ran []string
		err := runConvergenceSteps(
			convergenceStep{"tunables", func() error { ran = append(ran, "tunables"); return errors.New("err 53") }},
			convergenceStep{"authdb", func() error { ran = append(ran, "authdb"); return nil }},
		)
		if len(ran) != 2 || ran[1] != "authdb" {
			t.Fatalf("every step must run; ran %v", ran)
		}
		if err == nil {
			t.Fatal("the failure must still be reported, not swallowed")
		}
		if !strings.Contains(err.Error(), "tunables") || !strings.Contains(err.Error(), "err 53") {
			t.Errorf("error must name the step that failed: %v", err)
		}
	})

	t.Run("several failures are all reported", func(t *testing.T) {
		err := runConvergenceSteps(
			convergenceStep{"a", func() error { return errors.New("boom-a") }},
			convergenceStep{"b", func() error { return errors.New("boom-b") }},
		)
		if err == nil {
			t.Fatal("expected an error")
		}
		for _, want := range []string{"boom-a", "boom-b"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error must mention %q, got %v", want, err)
			}
		}
	})

	t.Run("all-clean is nil", func(t *testing.T) {
		if err := runConvergenceSteps(
			convergenceStep{"a", func() error { return nil }},
			convergenceStep{"b", func() error { return nil }},
		); err != nil {
			t.Errorf("expected nil, got %v", err)
		}
	})
}

// And the attribute that provoked it: slapd rejects TLS settings outright when
// the server has no TLS configured, so writing them is a modify that can never
// succeed. ADR-024 R4's own rule — a value that cannot be honoured is not
// offered — applied to the operator's own default rather than to a user field.
func TestGlobalTunablesSkipTLSWhenDisabled(t *testing.T) {
	off := &ldapv1alpha1.SlapdCluster{}
	off.Spec.LDAP.TLS.Enabled = false
	on := &ldapv1alpha1.SlapdCluster{}
	on.Spec.LDAP.TLS.Enabled = true

	for _, tc := range []struct {
		name string
		sc   *ldapv1alpha1.SlapdCluster
		want bool
	}{
		{"TLS off: do not touch olcTLSProtocolMin", off, false},
		{"TLS on: converge it as before", on, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tlsTunablesWritable(tc.sc); got != tc.want {
				t.Errorf("tlsTunablesWritable = %v, want %v", got, tc.want)
			}
		})
	}
}
