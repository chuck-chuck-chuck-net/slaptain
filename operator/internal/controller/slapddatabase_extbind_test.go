/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import "testing"

// The bind identity of an external peer's syncrepl stanzas is a per-database
// value, exactly like logbase (ADR-019 R9's axis argument applied to the bind
// identity, amendment 2026-09-13): a cluster-level ExternalPeer.bindDN spans
// every SlapdDatabase, so with two replicated databases at most one of them
// can bind as its own replication identity. ADR-020's log ACL then denies the
// other database's log search (err=32 noSuchObject on the logbase) and its
// delta-syncrepl halts outright — captured live on a three-site mesh
// (reconcile-loop-fixes.md 2026-09-13).
//
// The rule under test: when the peer-level override fields are unset, the
// identity derives per database — since ADR-027's cutover that is the
// node-local cn=repl-<db>,cn=slaptain-auth with the database's own replication
// password, the same identity the in-cluster stanzas bind as (ADR-020 R2).
// Set fields win verbatim (the ADR-011 escape for foreign sources with their
// own bind accounts — and the migration-window escape for a cross-site peer
// that has not upgraded yet).
func TestExternalBindIdentity(t *testing.T) {
	const (
		suffix    = "dc=second,dc=example,dc=net"
		dbName    = "second-db"
		dbPW      = "per-db-replication-pw"
		derivedDN = "cn=repl-second-db,cn=slaptain-auth"
	)

	t.Run("no override: derives the database's replication identity", func(t *testing.T) {
		dn, pw := externalBindIdentity("", "", "", suffix, dbName, dbPW)
		if dn != derivedDN {
			t.Errorf("bindDN = %q, want derived %q", dn, derivedDN)
		}
		if pw != dbPW {
			t.Errorf("password = %q, want the database's replication password", pw)
		}
	})

	t.Run("override set: wins verbatim (ADR-011 foreign source)", func(t *testing.T) {
		dn, pw := externalBindIdentity(
			"cn=syncuser,ou=config,o=legacy", "legacy-creds", "legacy-pw", suffix, dbName, dbPW)
		if dn != "cn=syncuser,ou=config,o=legacy" {
			t.Errorf("bindDN = %q, want the override verbatim", dn)
		}
		if pw != "legacy-pw" {
			t.Errorf("password = %q, want the override verbatim", pw)
		}
	})

	t.Run("fields default independently: DN derived, override password kept", func(t *testing.T) {
		dn, pw := externalBindIdentity("", "shared-creds", "override-pw", suffix, dbName, dbPW)
		if dn != derivedDN {
			t.Errorf("bindDN = %q, want derived %q", dn, derivedDN)
		}
		if pw != "override-pw" {
			t.Errorf("password = %q, want the override secret's value", pw)
		}
	})

	t.Run("named but unreadable override secret must NOT borrow the per-DB password", func(t *testing.T) {
		// A secret that was explicitly named but yielded no password (missing
		// keys) is "could not read it", not "not set" — silently substituting
		// another credential would mask the user's broken override. The stanza
		// keeps the empty credential and fails loudly at the consumer.
		_, pw := externalBindIdentity("cn=x,o=y", "named-but-empty", "", suffix, dbName, dbPW)
		if pw == dbPW {
			t.Errorf("password fell back to the per-DB credential despite a named override secret")
		}
		if pw != "" {
			t.Errorf("password = %q, want empty (fail loudly at the consumer)", pw)
		}
	})
}
