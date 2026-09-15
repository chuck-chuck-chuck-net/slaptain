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
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The SlapdDatabase controller's half of ADR-027: this database's own identity
// entry inside the node-local authentication database, and nothing else.
//
// ADR-027 decision 7: the container is the SlapdCluster controller's (see
// slapdcluster_authdb.go), the entries are this controller's. Each entry is
// per-database and singly-owned, so no reconcile here ever needs evidence about
// another database — which is what keeps ADR-026 R2 off this path.
//
// The substantive difference from ensureReplicationUser, the entry this will
// eventually replace, is that this one is CONVERGED rather than
// create-if-missing (ADR-027 decision 6). That is the point of the ADR:
// rotation must work, and the old placement could not support it, because a
// corrective write into the replicated tree travels by the very syncrepl link a
// wrong password has broken. Convergence is safe HERE only because the target
// is node-local and has exactly one writer.
//
// ADR-027's migration is additive-then-subtractive across two releases. This is
// step 1: the entry appears, nothing consumes it yet. cn=replication,<suffix>,
// the ACL grants naming it, the syncrepl stanzas' binddn and the operator's CSN
// monitoring identity are all untouched.

// errAuthDBAbsent reports that a pod has no cn=slaptain-auth database yet.
//
// Expected rather than exceptional: the SlapdCluster controller creates the
// database, and on a pod that has just come up — or on a cluster whose operator
// was upgraded between the two controllers' passes — this controller can arrive
// first. ADR-027 decision 7 says defer and retry, so the caller distinguishes it
// from a real failure and simply asks for another pass.
var errAuthDBAbsent = errors.New("auth database not present on this pod yet")

// reconcileAuthIdentity converges this database's replication identity inside
// the auth database on every pod of the cluster.
//
// Still per-pod best-effort in the shape of reconcileTunables rather than of
// reconcilePodDatabase — a returned error there marks the pod unhealthy for
// every other purpose too, which is far more reach than this step needs. What
// changed at the cutover is what the RESULT is used for.
//
// Milestone 1's posture was "a failure here must not be able to touch
// replication", correct only while nothing bound as the identity (ADR-027
// amendment, deviation 3). Now that every stanza names it, a pod without the
// entry rejects every consumer that binds as it, so the result is returned as
// an authIdentityState and the caller gates stanza writes on it
// (authIdentityReadyForStanzas in replidentity.go). The failure semantics are
// therefore inverted deliberately: an incomplete pass no longer merely asks for
// a requeue, it withholds the configuration that would point at the missing
// entry.
//
// Visits read-only pods too. ADR-027 gives RO pods the database for uniformity
// and notes only RW pods verify incoming binds; populating both means a pod is
// never half-provisioned, and an RO fleet that is later promoted needs no
// backfill. The write is node-local and tiny. Their outcome is reported
// separately from the RW fleet's because only the RW fleet is a provider — see
// authIdentityReadyForStanzas for why an RO pod must not be able to gate.
func (r *SlapdDatabaseReconciler) reconcileAuthIdentity(
	ctx context.Context,
	sc *ldapv1alpha1.SlapdCluster,
	sd *ldapv1alpha1.SlapdDatabase,
	configPW string,
) authIdentityState {
	log := logf.FromContext(ctx)

	replPassword, err := r.getDatabaseReplPassword(ctx, sd)
	if err != nil {
		log.Info("auth identity convergence skipped: cannot read the replication password",
			"database", sd.Name, "err", err)
		return authIdentityState{}
	}
	if replPassword == "" {
		// No replication password in the Secret — the same condition under which
		// ensureReplicationUser declines to create its entry. Nothing to
		// project, and nothing a stanza could bind as either: report converged
		// so this cannot wedge a database that simply has no replication
		// credential (the stanza builder is not reached in that state anyway).
		return authIdentityState{AllRWPodsConverged: true, AllROPodsConverged: true}
	}

	replicas := sc.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}

	type target struct {
		name, headless string
		readOnly       bool
	}
	var targets []target
	for i := int32(0); i < replicas; i++ {
		targets = append(targets, target{
			name:     fmt.Sprintf("%s-%d", sc.Name, i),
			headless: sc.Name + "-headless",
		})
	}
	for i := int32(0); i < sc.Spec.ReadReplicas; i++ {
		targets = append(targets, target{
			name:     fmt.Sprintf("%s-readonly-%d", sc.Name, i),
			headless: sc.Name + "-readonly-headless",
			readOnly: true,
		})
	}

	state := authIdentityState{AllRWPodsConverged: true, AllROPodsConverged: true}
	for _, t := range targets {
		host := fmt.Sprintf("%s.%s.%s.svc.%s", t.name, t.headless, sc.Namespace, r.ClusterDomain)
		err := r.reconcilePodAuthIdentity(ctx, host, configPW, replPassword, sd)
		switch {
		case errors.Is(err, errAuthDBAbsent):
			// The SlapdCluster controller has not created it here yet. Not a
			// failure — just not this pass (ADR-027 decision 7).
			log.V(1).Info("auth identity deferred: auth database not created yet",
				"pod", t.name, "database", sd.Name)
		case err != nil:
			log.Info("auth identity convergence skipped for pod (will retry)",
				"pod", t.name, "database", sd.Name, "err", err)
		default:
			continue
		}
		if t.readOnly {
			state.AllROPodsConverged = false
		} else {
			state.AllRWPodsConverged = false
		}
	}
	return state
}

// reconcilePodAuthIdentity converges the identity entry on one pod.
//
// Binds as cn=admin,cn=config: that is the auth database's olcRootDN (ADR-027
// decision 1), the same credential-reuse ADR-020 established for accesslog
// databases, and a rootDN bypasses the bind-only ACLs the database carries.
func (r *SlapdDatabaseReconciler) reconcilePodAuthIdentity(
	ctx context.Context,
	host, configPW, replPassword string,
	sd *ldapv1alpha1.SlapdDatabase,
) error {
	addr := host + ":" + strconv.Itoa(int(ldapContainerPort))
	conn, err := ldap.DialURL("ldap://"+addr,
		ldap.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}),
	)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()
	conn.SetTimeout(ldapRequestTimeout)

	if err := conn.Bind("cn=admin,cn=config", configPW); err != nil {
		return fmt.Errorf("bind cn=admin,cn=config at %s: %w", host, err)
	}

	// The database must exist before its entries can. Checked explicitly rather
	// than inferred from a noSuchObject on the add, because the two are
	// genuinely different situations: no database is "wait for the other
	// controller", while a missing parent inside an existing database would be a
	// defect worth surfacing.
	dbDN, err := findAuthDBDN(conn)
	if err != nil {
		return fmt.Errorf("search for auth DB at %s: %w", host, err)
	}
	if dbDN == "" {
		return errAuthDBAbsent
	}

	return r.ensureAuthIdentity(ctx, conn, host, replPassword, sd)
}

// ensureAuthIdentity creates or converges cn=repl-<dbname>,cn=slaptain-auth on
// one pod, projecting the replication-password key of <dbname>-credentials.
//
// The Secret is the single source of truth and this entry is a projection of it
// (ADR-027 option A). So:
//
//   - absent      → add it;
//   - password up to date → do nothing, and specifically do NOT rewrite. An
//     SSHA hash is salted, so a blind Replace every reconcile would be a
//     permanent write on every pod of every cluster;
//   - anything else → Replace userPassword. "Anything else" deliberately
//     includes an empty or unparseable value: the password-stripped copy
//     measured live (ADR-027 Context) satisfied the old existence check forever
//     while locking every consumer out, and the whole reason convergence
//     replaces create-if-missing is that such a copy must be repaired.
//
// The repair is unconditionally safe here in a way it never was at the old
// placement: the target is node-local with exactly one writer, so there is no
// mesh to converge against, no ADR-026 R2 shared-state question, and no write
// storm from N sites each asserting their own Secret.
func (r *SlapdDatabaseReconciler) ensureAuthIdentity(
	ctx context.Context,
	conn *ldap.Conn,
	host, replPassword string,
	sd *ldapv1alpha1.SlapdDatabase,
) error {
	log := logf.FromContext(ctx)

	dn := ldapv1alpha1.AuthIdentityDN(sd.Name)

	sr, err := conn.Search(ldap.NewSearchRequest(
		dn, ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		1, 0, false, "(objectClass=*)", []string{"userPassword"}, nil,
	))
	if err != nil {
		if !ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
			return fmt.Errorf("read %s at %s: %w", dn, host, err)
		}
		hash, err := generateSSHAHash(replPassword)
		if err != nil {
			return fmt.Errorf("hash replication password: %w", err)
		}
		log.Info("creating node-local replication identity",
			"host", host, "dn", dn)
		addReq := ldap.NewAddRequest(dn, nil)
		for attr, values := range desiredAuthIdentityAttrs(sd.Name, hash) {
			addReq.Attribute(attr, values)
		}
		if err := conn.Add(addReq); err != nil {
			if ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists) {
				return nil
			}
			if ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
				// The database exists but its suffix entry does not — the
				// SlapdCluster controller creates both, and this pass caught it
				// between the two. Same deferral, not a failure.
				return fmt.Errorf("%w: suffix entry missing at %s", errAuthDBAbsent, host)
			}
			return fmt.Errorf("add %s at %s: %w", dn, host, err)
		}
		return nil
	}
	if len(sr.Entries) == 0 {
		return fmt.Errorf("no entry at %s on %s", dn, host)
	}

	for _, stored := range sr.Entries[0].GetAttributeValues("userPassword") {
		if sshaMatches(stored, replPassword) {
			return nil
		}
	}

	hash, err := generateSSHAHash(replPassword)
	if err != nil {
		return fmt.Errorf("hash replication password: %w", err)
	}
	log.Info("converging node-local replication identity to the Secret",
		"host", host, "dn", dn)
	modReq := ldap.NewModifyRequest(dn, nil)
	modReq.Replace("userPassword", []string{hash})
	if err := conn.Modify(modReq); err != nil {
		return fmt.Errorf("set userPassword on %s at %s: %w", dn, host, err)
	}
	return nil
}
