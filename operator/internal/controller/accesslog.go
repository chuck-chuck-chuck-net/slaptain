/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"path"
	"strings"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// Convergence off the pre-ADR-019 cluster-shared accesslog (ADR-019 R8).
//
// How a per-database log is *named* lives in api/v1alpha1 (AccesslogSuffix /
// AccesslogDir / ExternalLogBase) — it is part of the observable contract.
// What lives here is genuinely controller-local: deciding, from one pod's
// observed cn=config, which teardown steps a legacy cluster still needs.
//
// olcSuffix and olcDbDirectory are not runtime-mutable, so there is no rename
// path — the shared log has to go and a per-database one has to take its place.
// The cost is one full refresh per consumer, which is the same SYNCLOG_FALLBACK
// path slapd takes for a purged log, so it is self-healing (ADR-019 R8).

// legacyAccesslogSuffix is the suffix of the single cluster-shared accesslog
// database this operator created before ADR-019. Its backing directory was the
// accesslog mount root itself (ldapv1alpha1.AccesslogRoot) rather than a
// per-database subdirectory, and the two together are what identify it.
//
// This is the ONE place the bare legacy suffix is spelled. ADR-019 R5's rule —
// nothing spells cn=accesslog literally — is about the *live* naming; a
// migration necessarily has to name the thing it is migrating away from.
const legacyAccesslogSuffix = "cn=accesslog"

// observedLogDB is one olcMdbConfig child of cn=config as read off a pod: its
// DN (carrying slapd's {N} ordering prefix), its olcSuffix and its
// olcDbDirectory.
type observedLogDB struct {
	DN     string
	Suffix string
	Dir    string
}

// observedAccesslogOverlay is one accesslog overlay found anywhere in this
// pod's cn=config, together with the log suffix its olcAccessLogDB names.
// Collected across *all* databases, not just the one being reconciled — which
// database still references the shared log is exactly what decides whether it
// is safe to delete.
type observedAccesslogOverlay struct {
	DN    string
	LogDB string
}

// accesslogMigrationPlan is what one pod needs for one SlapdDatabase. The zero
// value means "nothing to do", which is the answer on every cluster that has
// only per-database logs.
type accesslogMigrationPlan struct {
	// DropOverlay: this data DB's accesslog overlay names the legacy shared
	// log. Delete it, so ensureAccesslogOverlay re-adds it against this
	// database's own log. slapd validates olcAccessLogDB against an existing
	// database, which is why this is a delete-and-re-add rather than a modify —
	// the same reason reconcilePodDatabase already orders DB before overlay on
	// add and overlay before DB on remove.
	DropOverlay bool
	// DeleteLegacyDN: DN of the legacy shared log database to delete, or "" to
	// leave it alone.
	DeleteLegacyDN string
}

// Empty reports whether the plan requires no action at all.
func (p accesslogMigrationPlan) Empty() bool { return !p.DropOverlay && p.DeleteLegacyDN == "" }

// planAccesslogMigration decides the ADR-019 R8 convergence for one
// SlapdDatabase on one pod.
//
// dataDN is that database's own olcDatabase={N}mdb DN; mdbs and overlays are
// everything of the respective kind observed under cn=config on this pod.
//
// Two independent decisions, deliberately:
//
//   - DropOverlay is about *our* database only, and fires only when our overlay
//     names the legacy shared log exactly.
//   - DeleteLegacyDN is about the shared database, and fires only when nothing
//     will reference it once DropOverlay has been carried out.
//
// Keeping them independent is what makes the multi-database case safe and
// order-free. With DB-A and DB-B both journalling into one shared log:
//
//	A reconciles first — it drops A's overlay, sees B's overlay still naming the
//	shared log, and leaves the shared database alone. B keeps journalling
//	throughout; nothing is yanked out from under a live overlay.
//	B reconciles next — it drops B's overlay, sees no remaining reference, and
//	reaps the shared database.
//
// The reverse order is symmetric, and if the two ever observe each other
// concurrently and both decline to delete, the log is simply left orphaned and
// the next reconcile of either database reaps it — because DeleteLegacyDN does
// not depend on DropOverlay. It also reaps a shared log orphaned by a database
// being demoted out of delta-sync — but only while some database on the pod
// still wants an accesslog, because that is the only branch this runs under. A
// pod whose every database has been demoted keeps the orphan indefinitely: it is
// unreferenced and harmless, and reaping it would mean running this on the
// no-accesslog path purely to tidy up. cn=config is node-local (ADR-002), so
// pods converge independently with no cross-pod coordination.
//
// Idempotent by construction (ADR-001): after a successful migration the log DB
// is gone and the overlay names the per-database suffix, so both decisions read
// false on every subsequent reconcile.
func planAccesslogMigration(
	dataDN string,
	mdbs []observedLogDB,
	overlays []observedAccesslogOverlay,
) accesslogMigrationPlan {
	var plan accesslogMigrationPlan

	// Find the legacy shared log. BOTH the suffix and the backing directory
	// must match: cn=accesslog-<db> shares a prefix with the legacy suffix, so a
	// substring test here would tear the log out of every healthy cluster, and a
	// suffix-only test would delete a hand-made cn=accesslog living somewhere
	// this operator never put one.
	var legacyDN string
	for _, db := range mdbs {
		if isLegacyAccesslogDB(db) {
			legacyDN = db.DN
			break
		}
	}
	if legacyDN == "" {
		// The overwhelmingly common case, including every cluster created at or
		// after ADR-019 and every already-migrated one. Nothing is torn down
		// when there is no legacy log to migrate away from.
		return plan
	}

	remainingRefs := 0
	for _, ov := range overlays {
		if !namesLegacyAccesslog(ov.LogDB) {
			continue
		}
		if isChildDN(ov.DN, dataDN) {
			plan.DropOverlay = true
			continue
		}
		remainingRefs++
	}
	if remainingRefs == 0 {
		plan.DeleteLegacyDN = legacyDN
	}
	return plan
}

// isLegacyAccesslogDB reports whether an observed mdb database is the
// pre-ADR-019 cluster-shared accesslog: suffix cn=accesslog backed by the
// accesslog mount root itself.
func isLegacyAccesslogDB(db observedLogDB) bool {
	if !namesLegacyAccesslog(db.Suffix) {
		return false
	}
	dir := path.Clean(strings.TrimSpace(db.Dir))
	return dir == path.Clean(ldapv1alpha1.AccesslogRoot)
}

// namesLegacyAccesslog reports whether an olcSuffix or olcAccessLogDB value is
// exactly the legacy shared suffix. Exact, case-folded, whitespace-trimmed:
// cn=config DN values are case-insensitive and slapd normalises them on its own
// schedule, but cn=accesslog-default must never match.
func namesLegacyAccesslog(v string) bool {
	return strings.EqualFold(strings.TrimSpace(v), legacyAccesslogSuffix)
}

// isChildDN reports whether child is a direct-or-deeper descendant of parent,
// comparing case-insensitively. Used to attribute an accesslog overlay to the
// data database it hangs under; slapd's {N} ordering prefixes make the overlay's
// own RDN unpredictable, but its parent DN is not.
func isChildDN(child, parent string) bool {
	return len(child) > len(parent)+1 &&
		strings.EqualFold(child[len(child)-len(parent):], parent) &&
		child[len(child)-len(parent)-1] == ','
}

// observedConfigEntry is one cn=config entry as read off a pod: its DN and its
// objectClass values.
type observedConfigEntry struct {
	DN      string
	Classes []string
}

// unwantedLogDBChildren returns the DNs of children of an accesslog database
// that must not exist there.
//
// An accesslog database legitimately carries exactly one child: the syncprov
// overlay that exposes its journal to consumers (ensureAccesslogDB adds it). An
// **accesslog overlay** on an accesslog database is never legitimate: it makes
// one journal journal into another, so a consumer of the second database
// receives log entries whose reqDN lives under the first database's accesslog
// suffix, submits them to its own backend, and gets NO_SUCH_OBJECT — ADR-019
// Fact 2, with the journals themselves as the two databases.
//
// That state is not hypothetical. It is what a cn=config database delete plus a
// cached olcDatabase={N} DN produces: slapd renumbers every database ordered
// after a deleted one, so an operator that resolved a DN before the delete and
// used it after would add the overlay to whatever database slid into that slot.
// The staleness is fixed at its root in reconcilePodDatabase, but a cluster
// migrated by a build that had the bug keeps the mis-attached overlay forever —
// nothing else would ever remove it — so ensureAccesslogDB reaps it.
//
// Anything else found under a log DB is left alone. The rule is "delete what
// this operator can positively attribute to its own bug", not "delete what this
// operator did not put here": cn=config is node-local and hand-editable
// (ADR-002), and a reaper that removes unrecognised entries is a footgun.
func unwantedLogDBChildren(children []observedConfigEntry) []string {
	var out []string
	for _, c := range children {
		for _, cls := range c.Classes {
			if strings.EqualFold(cls, "olcAccessLogConfig") {
				out = append(out, c.DN)
				break
			}
		}
	}
	return out
}
