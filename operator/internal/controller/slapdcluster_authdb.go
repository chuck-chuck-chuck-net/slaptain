/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"fmt"

	ldap "github.com/go-ldap/ldap/v3"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The SlapdCluster controller's half of ADR-027: the authentication database
// itself — the container, not its contents.
//
// ADR-027 decision 7 pins that split. The auth database is shared by every
// SlapdDatabase on a pod, so letting each database's reconcile create it
// if-missing would recreate exactly the shape ADR-026 exists to prevent: shared
// cn=config state with competing writers, and an olcDatabase={N} renumbering
// (R1) on every create. One creator instead — this controller, which already
// owns the pod's other per-pod infrastructure (modules, TLS, global tunables)
// and already visits every pod for it. The per-database identity entries are
// the SlapdDatabase controller's, in slapddatabase_authdb.go.
//
// Never replicated, by construction: no syncprov overlay, no syncrepl stanza,
// no accesslog overlay, no olcMultiProvider. That is the whole point — the
// credential's verifier copy must not be replicated, multi-writer state, because
// a store that converges mesh-wide can never be reconciled against a per-site
// Secret that does not.

// findAuthDBDN resolves the auth database's olcDatabase={N}mdb DN on one pod,
// returning ("", nil) when it does not exist.
//
// Thin alias over findDataDBDNOptional, which is a plain olcSuffix search and
// carries nothing data-specific; naming it here keeps the call sites honest
// about what they are resolving. Note the {N}: like every olcDatabase DN it is
// positional (ADR-026 R1), so it is resolved fresh on each pass and never
// cached across a delete.
func findAuthDBDN(conn *ldap.Conn) (string, error) {
	return findDataDBDNOptional(conn, ldapv1alpha1.AuthSuffix)
}

// ensureAuthDB creates the node-local authentication database on one pod when
// missing, and converges what the operator owns about it on every pass.
//
// Three cn=config/directory surfaces, in the order slapd requires:
//
//	olcDatabase=mdb cn=slaptain-auth  — the database, backed by
//	                                    /data/_slaptain-auth (ADR-027 d.1/d.3/d.4)
//	olcAccess on that database        — bind-only, two rules (ADR-027 d.5)
//	cn=slaptain-auth                  — the suffix entry, so identity entries
//	                                    have a parent to be added under
//
// The suffix entry is not decoration: LDAP rejects an add whose parent does not
// exist, so without it every SlapdDatabase's identity write would fail with
// noSuchObject forever. It is created here rather than by the database
// controller for the same single-creator reason as the database.
//
// olcDbMaxSize is deliberately not set. back-mdb's default map size holds
// orders of magnitude more than the handful of tiny entries this database will
// ever carry, and ADR-024 R4 makes maxSize create-only — modifying it under a
// running slapd segfaults the process — so a value written here could never be
// corrected without recreating the database. Nothing is gained by pinning one.
//
// Idempotent in the sibling's idiom (ensureAccesslogDB): search first so a
// steady-state reconcile is one read, treat EntryAlreadyExists as success on
// every add, and modify the ACL only when it differs.
func (r *SlapdClusterReconciler) ensureAuthDB(
	ctx context.Context,
	conn *ldap.Conn,
	host string,
) error {
	log := logf.FromContext(ctx)

	dbDN, err := findAuthDBDN(conn)
	if err != nil {
		return fmt.Errorf("search for auth DB %s at %s: %w", ldapv1alpha1.AuthSuffix, host, err)
	}

	if dbDN == "" {
		log.Info("creating node-local auth DB", "host", host, "suffix", ldapv1alpha1.AuthSuffix)
		addReq := ldap.NewAddRequest("olcDatabase=mdb,cn=config", nil)
		addReq.Attribute("objectClass", []string{"olcDatabaseConfig", "olcMdbConfig"})
		addReq.Attribute("olcDatabase", []string{"mdb"})
		addReq.Attribute("olcSuffix", []string{ldapv1alpha1.AuthSuffix})
		// cn=admin,cn=config as rootDN, the pattern ADR-020 already established
		// for accesslog databases: it lets the operator write this database with
		// a credential it already holds, and it is deliberately NOT a data
		// database's rootDN — ADR-027 rejected binding as the data rootDN
		// precisely because that identity bypasses every ACL on the data tree.
		addReq.Attribute("olcRootDN", []string{"cn=admin,cn=config"})
		// back-mdb does not create olcDbDirectory; bootstrap.sh provisions it
		// unconditionally at pod start (ADR-027 decision 3).
		addReq.Attribute("olcDbDirectory", []string{ldapv1alpha1.AuthDir})
		addReq.Attribute("olcAccess", authDBACLs())
		if err := conn.Add(addReq); err != nil {
			if !ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists) {
				return fmt.Errorf("add auth DB %s at %s: %w", ldapv1alpha1.AuthSuffix, host, err)
			}
		}
		// Re-find to learn the assigned {N} prefix.
		dbDN, err = findAuthDBDN(conn)
		if err != nil {
			return fmt.Errorf("re-find auth DB at %s after add: %w", host, err)
		}
		if dbDN == "" {
			return fmt.Errorf("auth DB %s not present at %s after add", ldapv1alpha1.AuthSuffix, host)
		}
	}

	// Converge the ACLs on every reconcile, the way ensureAccesslogACL does
	// (ADR-020 R5's reasoning, unchanged): a database with no matching rule
	// falls through to the frontend default of *read*, and this one holds every
	// replication credential on the pod.
	if err := r.ensureAuthDBACLs(ctx, conn, host, dbDN); err != nil {
		return err
	}

	return r.ensureAuthSuffixEntry(ctx, conn, host)
}

// ensureAuthDBACLs aligns the auth database's olcAccess to authDBACLs,
// modifying only when it differs. Same shape as ensureAccesslogACL.
func (r *SlapdClusterReconciler) ensureAuthDBACLs(
	ctx context.Context,
	conn *ldap.Conn,
	host, dbDN string,
) error {
	log := logf.FromContext(ctx)

	desired := authDBACLs()

	sr, err := conn.Search(ldap.NewSearchRequest(
		dbDN, ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		1, 0, false, "(objectClass=*)", []string{"olcAccess"}, nil,
	))
	if err != nil {
		return fmt.Errorf("read olcAccess on %s at %s: %w", dbDN, host, err)
	}
	if len(sr.Entries) == 0 {
		return fmt.Errorf("no entry at %s on %s", dbDN, host)
	}
	if aclsMatch(sr.Entries[0].GetEqualFoldAttributeValues("olcAccess"), desired) {
		return nil
	}

	log.Info("setting auth DB ACLs", "host", host, "dn", dbDN)
	modReq := ldap.NewModifyRequest(dbDN, nil)
	modReq.Replace("olcAccess", desired)
	if err := conn.Modify(modReq); err != nil {
		return fmt.Errorf("set olcAccess on %s at %s: %w", dbDN, host, err)
	}
	return nil
}

// ensureAuthSuffixEntry adds the cn=slaptain-auth entry itself — the parent
// every identity entry hangs under.
//
// organizationalRole is the same structural class the identities use and the
// same one cn=replication,<suffix> has always used; it is in core.schema, which
// bootstrap.sh includes on every pod, and it takes cn as its naming attribute.
//
// The entry carries no attribute the operator would ever want to change, so
// this is existence-only rather than a read-compare-write. That is not the
// create-if-missing ADR-027 rejects: what must converge is the PASSWORD, and
// that lives on the identity entries, which are converged.
func (r *SlapdClusterReconciler) ensureAuthSuffixEntry(
	ctx context.Context,
	conn *ldap.Conn,
	host string,
) error {
	log := logf.FromContext(ctx)

	exists, err := ldapEntryExists(conn, ldapv1alpha1.AuthSuffix)
	if err != nil {
		return fmt.Errorf("check auth suffix entry at %s: %w", host, err)
	}
	if exists {
		return nil
	}

	log.Info("creating auth DB suffix entry", "host", host, "dn", ldapv1alpha1.AuthSuffix)
	addReq := ldap.NewAddRequest(ldapv1alpha1.AuthSuffix, nil)
	addReq.Attribute("objectClass", []string{"organizationalRole"})
	addReq.Attribute("cn", []string{"slaptain-auth"})
	addReq.Attribute("description", []string{
		"Node-local replication identities — never replicated (ADR-027)",
	})
	if err := conn.Add(addReq); err != nil {
		if ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists) {
			return nil
		}
		return fmt.Errorf("add auth suffix entry at %s: %w", host, err)
	}
	return nil
}
