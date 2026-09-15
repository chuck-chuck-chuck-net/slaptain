/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package v1alpha1

// Naming for the node-local authentication database (ADR-027) — the single
// place these strings are spelled.
//
// They live on the API package rather than in internal/controller for the same
// reason the accesslog names do: the derivation is part of the observable
// contract, not controller bookkeeping. A diagnostic that reads a pod's
// cn=config back must be able to name the database and its identities without
// pulling controller-runtime in.
//
// ADR-027's premise is that the replication credential's LDAP copy must not be
// replicated, multi-writer state. Everything named here is therefore per-pod:
// one database per pod, one identity entry per SlapdDatabase inside it,
// populated by the operator from the Kubernetes Secret, which is the single
// source of truth.

// AuthSuffix is the LDAP suffix of the node-local authentication database
// (ADR-027 decision 1). It carries no syncprov, no syncrepl and no accesslog
// overlay: it is never replicated, by construction.
const AuthSuffix = "cn=slaptain-auth"

// AuthDir is the LMDB backing directory of that database (ADR-027 decisions 3
// and 4): a fixed subdirectory of the existing data PVC, so no new volume, no
// new mount and no new PVC-deletion-lease surface (ADR-018).
//
// The leading underscore is load-bearing, not decoration. The init container
// creates /data/<name> for every entry in DATABASE_DIRS, and those entries are
// SlapdDatabase CR names; a Kubernetes object name is an RFC 1123 subdomain and
// must begin with a lowercase alphanumeric, so no CR can ever be named
// "_slaptain-auth". The reservation is therefore structural rather than a
// convention someone could violate. This directory is created unconditionally
// at bootstrap and never appears in DATABASE_DIRS, which is why it adds nothing
// to the ADR-013 rolling-restart wart.
const AuthDir = "/data/_slaptain-auth"

// AuthIdentityDN returns the syncrepl bind identity for one SlapdDatabase:
// cn=repl-<dbname>,cn=slaptain-auth (ADR-027 decision 2).
//
// Keyed on the CR name, like AccesslogSuffix and for the same reason — it is
// already the identity behind <dbname>-credentials and DATABASE_DIRS. Identities
// stay per-database so the isolation ADR-020 depends on survives the move: one
// database's replication identity must not be able to read another database's
// journal.
func AuthIdentityDN(dbName string) string {
	return "cn=" + AuthIdentityCN(dbName) + "," + AuthSuffix
}

// AuthIdentityCN returns the RDN value of AuthIdentityDN. slapd rejects an add
// whose naming attribute is absent from the entry, so the two must be derived
// from one place.
func AuthIdentityCN(dbName string) string { return "repl-" + dbName }
