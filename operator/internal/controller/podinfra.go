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
	"fmt"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// Independent per-pod convergence steps are INDEPENDENT.
//
// The rule exists because breaking it produced a permanent replication outage,
// measured on t3e 2026-09-15 while reproducing the mesh run's scale-up failure.
// A cluster with TLS disabled makes slapd refuse olcTLSProtocolMin outright —
// `LDAP Result Code 53 "Unwilling To Perform"` — and reconcilePodInfrastructure
// returned on that error, so ensureAuthDB, listed after it, never ran on that
// cluster. Not once, not late: never. The node-local auth database was
// therefore absent forever, and with ADR-027's cutover in place the identity
// deferred and the syncrepl stanzas were withheld for as long as the cluster
// existed.
//
// Both short-circuits predate this milestone and both were harmless while the
// auth database was unused decoration (ADR-027 milestone 1, additive). The
// cutover is what made one of them fatal, by turning that database into a hard
// precondition for replication. The lesson generalises past this instance:
// once anything becomes a precondition, every path that can silently skip it is
// a potential deadlock, and "best-effort, will retry" does not save a step that
// is never reached.

// convergenceStep is one named unit of per-pod work.
type convergenceStep struct {
	name string
	run  func() error
}

// runConvergenceSteps runs every step even when an earlier one fails, and
// reports all failures joined together.
//
// Deliberately NOT fail-fast. These steps share only a connection: a global
// tunable the server will not accept says nothing about whether the auth
// database can be created, so letting the first decide the second's fate is
// exactly the coupling that caused the outage above. Joining rather than
// returning the first also keeps the diagnosis honest — a log line naming only
// the TLS error would have sent a reader looking at TLS, when the consequence
// that mattered was a database that was never created.
func runConvergenceSteps(steps ...convergenceStep) error {
	var errs []error
	for _, s := range steps {
		if err := s.run(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.name, err))
		}
	}
	return errors.Join(errs...)
}

// tlsTunablesWritable reports whether this cluster's slapd will accept TLS
// settings on cn=config at all.
//
// It will not when the server has no TLS configured: slapd rejects
// olcTLSProtocolMin (and its siblings) with err=53, every reconcile, forever.
// That is ADR-024 R4's own rule turned on the operator's own default rather
// than on a user field — a value that cannot be honoured is not offered — and
// without it the operator retries a modify that can never succeed for the life
// of the cluster.
//
// Note this governs whether the attribute is TOUCHED, not what it is set to. A
// TLS-less cluster is left exactly as slapd booted it.
func tlsTunablesWritable(sc *ldapv1alpha1.SlapdCluster) bool {
	return sc != nil && sc.Spec.LDAP.TLS.Enabled
}

// globalTunable is one cn=config attribute the operator converges: its name,
// the value it should hold, and whether the operator writes it at all.
//
// write=false means "the operator does not want this attribute", and the loop
// DELETES it when present. That is distinct from "do not touch this attribute",
// which is expressed by leaving the entry out of the list entirely — the
// distinction the TLS attributes need on a server that refuses the set AND the
// delete for the same reason.
// frontendDN is the entry slapd wants password policy in. slaptest conversion
// creates it, so it is present on every pod we bootstrap — verified on a live
// three-site lab alongside olcDatabase={0}config and the data databases.
const frontendDN = "olcDatabase={-1}frontend,cn=config"

// tunableEntryDN says WHICH cn=config entry an attribute belongs in.
//
// Global is the default and the common case. The exception is olcPasswordHash,
// which slapd 2.7 warns about on every startup when it sits in the global
// entry:
//
//	setting password scheme in the global entry is deprecated. The server may
//	refuse to start if it is provided by a loadable module, please move it to
//	the frontend database instead
//
// "May refuse to start" is the part that matters. A built-in scheme like the
// default {SSHA} is fine today; a module-provided one ({ARGON2} via pw-argon2,
// the backlog item) is the configuration slapd is warning it may reject — and
// it would reject it at STARTUP, on a pod that was healthy a moment earlier.
// Placing it correctly now costs nothing and removes that from the path.
func tunableEntryDN(attr string) string {
	if attr == "olcPasswordHash" {
		return frontendDN
	}
	return "cn=config"
}

type globalTunable struct {
	attr  string
	value string
	write bool
}
