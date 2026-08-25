/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package v1alpha1

// AccesslogRoot is the mount path of the single accesslog PVC (ADR-019 R3).
// It is the same value the SlapdCluster controller passes to the init container
// as ACCESSLOG_DIR and mounts into the slapd container; per-database LMDB
// directories live beneath it. It is the one definition of that path — the
// StatefulSet builder, the backup/restore Job builders and AccesslogDir all
// derive from it.
const AccesslogRoot = "/accesslog"

// Accesslog naming — the single place the string "cn=accesslog" is spelled.
//
// ADR-019 R5: the accesslog DB's own olcSuffix, the data DB's overlay
// olcAccessLogDB, and every delta-syncrepl stanza's logbase must never be able
// to drift apart, so all three derive from one helper keyed on the
// SlapdDatabase CR name (ADR-019 R1). Nothing else in the operator may spell
// cn=accesslog literally.
//
// ADR-019 R10: these are computed at the point of use. Nothing is written back
// into .spec.
//
// They live on the API package rather than in internal/controller because the
// derivation is part of the observable contract, not controller-internal
// bookkeeping: slctl reads it back off a live pod to check that logbase,
// olcAccessLogDB and the log DB's olcSuffix agree, and a CLI must not have to
// pull controller-runtime in to know how the operator names a log.

// AccesslogSuffix returns the LDAP suffix of the accesslog database that
// journals the given SlapdDatabase: cn=accesslog-<dbname> (ADR-019 R1). Keyed
// on the CR name, not on the data DB's LDAP suffix — the CR name is already
// the identity used for <dbname>-credentials and DATABASE_DIRS, and an LDAP
// suffix can carry characters that are awkward in the matching path.
func AccesslogSuffix(dbName string) string { return "cn=accesslog-" + dbName }

// AccesslogDir returns the LMDB backing directory of that database's accesslog:
// /accesslog/<dbname> (ADR-019 R1). One shared accesslog PVC, one directory per
// database beneath it (ADR-019 R3) — LMDB needs a directory, not a mount.
func AccesslogDir(dbName string) string { return AccesslogRoot + "/" + dbName }

// ExternalLogBase returns the accesslog suffix to use as `logbase` in the
// delta-syncrepl stanzas aimed at external peers.
//
// ADR-019 R9: `logbase` is a search base evaluated on the *provider*, so an
// external stanza must name the peer's accesslog suffix. The default — correct
// whenever the peer is another slaptain cluster running a SlapdDatabase of the
// same name — is this database's own derived suffix; spec.replication.
// externalAccesslogSuffix overrides it for a foreign provider (ADR-011 supports
// syncMode: delta against one, and its log need not be called cn=accesslog).
//
// ADR-019 R10: computed at the point of use; never written back to .spec.
func ExternalLogBase(sd *SlapdDatabase) string {
	if s := sd.Spec.Replication.ExternalAccesslogSuffix; s != "" {
		return s
	}
	return AccesslogSuffix(sd.Name)
}
