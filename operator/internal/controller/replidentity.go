/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"fmt"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// Who the replication path authenticates as, in one place (ADR-027 milestone 2,
// migration steps 2 and 3).
//
// Three surfaces name a bind identity and they must never disagree: the
// syncrepl stanza that PRESENTS the credential, the olcAccess rule that lets
// the bound identity READ, and the olcLimits value that decides HOW MUCH it may
// read. A DN that appears in two of the three is a silent replication defect —
// this project has already paid for that twice, once as an ACL nobody granted
// and once as a 500-entry limit nobody set (ADR-020's 2026-09-12 amendment). So
// every spelling of every identity originates here, and the milestone-4
// subtraction is a deletion inside this file rather than a hunt.
//
// ── The cutover is ADDITIVE, deliberately ──────────────────────────────────
//
// Stanzas now bind as the node-local cn=repl-<db>,cn=slaptain-auth (ADR-027
// decision 2). The ACLs and limits grant BOTH that and the legacy
// cn=replication,<suffix>, and the legacy entry is still written. That is
// ADR-027's migration steps 2+3, and the reason is a mesh whose sites upgrade
// one at a time: a provider that has cut over still accepts a consumer that has
// not.
//
// The converse does NOT hold and cannot be made to — a stanza carries exactly
// one binddn, so an upgraded CONSUMER pointed at a not-yet-upgraded PROVIDER
// binds as an identity that does not exist there and gets err=49 until the
// provider upgrades. In-cluster that window does not exist (one operator writes
// both ends, and the gate below orders them); cross-site it does, and the
// documented handle is externalPeers[].bindDN, which still wins verbatim — pin
// it to legacyReplicationBindDN(<remote suffix>) for the length of the window.

// replicationBindDN is the identity every syncrepl stanza and the operator's
// own CSN monitoring bind as: this database's node-local entry inside the
// per-pod authentication database.
//
// Delegates to the API package rather than re-spelling the DN: the derivation
// is observable contract (ADR-027 amendment 6), and a controller-local copy is
// exactly the drift this file exists to prevent.
func replicationBindDN(dbName string) string {
	return ldapv1alpha1.AuthIdentityDN(dbName)
}

// legacyReplicationBindDN is the pre-ADR-027 identity in the replicated data
// tree. Still granted, still written, no longer bound as.
//
// It is kept for the migration window only. Removing it is ADR-027 migration
// step 4 — after which the ENTRY still remains, because deleting it is a write
// into the replicated tree that ADR-026 R2 says needs ownership we do not have;
// dropping its grants is cn=config, node-local and ours, and that is what makes
// it inert.
func legacyReplicationBindDN(suffix string) string {
	return "cn=replication," + suffix
}

// replicationACL is the operator-owned olcAccess rule prepended to a replicated
// data database: read on everything, for both replication identities.
//
// One rule with two `by` clauses rather than two rules. The clauses select
// disjoint DNs so their order is cosmetic, but keeping them in one rule means
// the trailing `by * break` — which is what lets the user's own spec.acls see
// the request at all — has exactly one spelling that can go wrong instead of
// two chained ones.
func replicationACL(dbName, suffix string) string {
	return fmt.Sprintf(`to * by dn.exact="%s" read by dn.exact="%s" read by * break`,
		replicationBindDN(dbName), legacyReplicationBindDN(suffix))
}

// accesslogACL is the single olcAccess rule an accesslog database carries
// (ADR-020 R1): read for the replication identities of the database it
// journals, nothing for anyone else.
//
// `by * none` rather than `by * break`, unchanged: a journal with no matching
// rule falls through to the frontend default of *read*, and a change journal
// carries the attribute values the data DB's own ACLs deny — the bypass was
// captured live before ADR-020 (an anonymous read returned a reqMod with a
// userPassword the data DB hides).
//
// No explicit rootDN grant (ADR-020 R3): olcRootDN is cn=admin,cn=config and a
// rootDN bypasses ACLs, so an ACL line restating that is noise a later reader
// mistakes for a requirement.
//
// Offline paths are unaffected (ADR-020 R4): slapcat/slapadd read the LMDB
// files directly and never evaluate ACLs.
func accesslogACL(dbName, dataSuffix string) string {
	return fmt.Sprintf(`to * by dn.exact="%s" read by dn.exact="%s" read by * none`,
		replicationBindDN(dbName), legacyReplicationBindDN(dataSuffix))
}

// csnBindDNs returns the identities the operator's CSN monitoring tries, in
// order, to read a database's contextCSN (ADR-008).
//
// The node-local identity first, because that is what this release grants and
// what every pod it manages carries. The legacy identity stays as a fallback
// for one reason: the same query runs against CROSS-SITE peers, and a peer
// running a pre-ADR-027 operator has no node-local entry to bind as. Without
// the fallback every such peer would read Unreachable for the length of the
// migration window — a monitoring blackout exactly when the mesh is mid-change
// and being watched. The fallback costs one extra dial, and only on a peer
// where the first bind already failed.
func csnBindDNs(dbName, suffix string) []string {
	return []string{replicationBindDN(dbName), legacyReplicationBindDN(suffix)}
}

// ── The ordering gate ───────────────────────────────────────────────────────

// authIdentityState summarises one pass of identity convergence across a
// cluster's pods: whether every pod of each fleet now carries this database's
// entry in its node-local auth database.
type authIdentityState struct {
	// AllRWPodsConverged — every read-write pod's identity entry is present and
	// matches the Secret.
	AllRWPodsConverged bool
	// AllROPodsConverged — same for the read-only fleet. Recorded, but not part
	// of the gate; see below.
	AllROPodsConverged bool
}

// authIdentityReadyForStanzas decides whether syncrepl stanzas may be written
// this pass — the failure-semantics revision ADR-027's amendment (deviation 3)
// scheduled for exactly this milestone.
//
// While nothing bound as the node-local identity, failing to write it was
// harmless and the step was deliberately best-effort so it could never withhold
// a stanza. That posture inverts the moment a stanza names it: a provider
// without the entry rejects every consumer binding as it with err=49, and the
// link stays dead until the entry appears. So the entry must exist on every pod
// that acts as a PROVIDER before any consumer is told to bind as it.
//
// In an in-cluster mesh every RW pod is provider and consumer at once, which
// collapses the ordering requirement to a single fleet-wide predicate: write no
// stanzas at all until every RW pod carries the entry. Withholding is safe in
// both directions — on an upgrade the pods keep the stanzas they already have,
// which still name the legacy identity and still work; on a fresh cluster there
// are no stanzas yet, so the only cost is a requeue.
//
// The read-only fleet is excluded on purpose. Nothing ever binds TO an RO pod,
// so its entry is provisioning uniformity (ADR-027 amendment 2), not a
// precondition for anyone's stanza. Including it would let one unreachable RO
// replica withhold the entire mesh's replication configuration — a deadlock on
// a pod that is not on the path.
//
// What this does NOT cover, and cannot: an EXTERNAL peer. The operator has no
// authority over a remote site's auth database and no way to observe it, so a
// cross-site stanza is retry-only — the consumer binds, fails, and syncrepl
// retries until the remote site upgrades. That is the migration window ADR-027
// documents, and externalPeers[].bindDN is the handle for shortening it.
func authIdentityReadyForStanzas(s authIdentityState) bool {
	return s.AllRWPodsConverged
}
