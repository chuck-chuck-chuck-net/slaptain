package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	ldap "github.com/go-ldap/ldap/v3"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The pure decision seams behind the breaks-at-scale tunables (ADR-024). Each
// one decides; the LDAP half in slapddatabase_controller.go only executes.
//
// All of these are invisible in a fixture-sized directory, which is why they
// survived a green suite for so long — tests/e2e/scale_test.go is the
// many-entries vehicle that makes them observable (ADR-024 Consequences).

// ── Replication identity limits (ADR-024 R7, ADR-020 amendment) ─────────────

// replicationLimits is the olcLimits value exempting a database's replication
// identity from every search limit. It is written on BOTH the data database and
// that database's accesslog database, beside the ACL that grants the same
// identity read (accesslogACL / applyACLs).
//
// Why it is not optional and not a CRD field: slapd's default sizelimit is 500
// entries, and a consumer's syncrepl search is an ordinary search subject to it.
// Without this exemption a directory silently stops replicating past its
// 500th entry, and a delta-syncrepl journal stops being readable past its 500th
// record — on a cluster that reports itself healthy in every other respect. The
// DN, the ACL and the limits are one contract the operator owns end to end
// (ADR-003); a user who cannot change the DN has no business capping its
// searches (ADR-024 R7).
//
// The explicit soft/hard spelling is deliberate: `size=unlimited` sets both, but
// the four-attribute form is what appears in cn=config after slapd normalises
// it either way, so writing it lets the convergence comparison be a string
// compare rather than a limits parser.
func replicationLimits(dataSuffix string) string {
	return fmt.Sprintf(
		`dn.exact="cn=replication,%s" time.soft=unlimited time.hard=unlimited `+
			`size.soft=unlimited size.hard=unlimited`, dataSuffix)
}

// desiredLimits is the full olcLimits list a data database must carry: the
// operator-owned replication exemption first (when this cluster replicates),
// then whatever the user declared in spec.limits, in order.
//
// Ours goes first for the same reason the replication ACL does — slapd applies
// the first matching selector, so a user rule with a broad selector must not be
// able to shadow the replication identity's exemption.
//
// Never aliases sd.Spec.Limits: prepending onto the CR's own backing array is
// the bug class that would make a cached object grow a new rule per reconcile.
func desiredLimits(sd *ldapv1alpha1.SlapdDatabase, replicating bool) []string {
	var out []string
	if replicating {
		out = append(out, replicationLimits(sd.Spec.Suffix))
	}
	out = append(out, sd.Spec.Limits...)
	return out
}

// limitsMatch reports whether the stored olcLimits values (which carry slapd's
// {N} ordering prefixes) already equal the desired list. Same comparison as
// aclsMatch — both attributes are ordered, prefixed, multi-valued — so it
// delegates rather than growing a second copy that could drift.
func limitsMatch(current, desired []string) bool {
	return aclsMatch(current, desired)
}

// ── olcDbMaxSize (ADR-024 R1 + R4) ──────────────────────────────────────────

// The operator's map-size opinions. back-mdb's own default is ~10 MB, which is
// a demo value: a directory that outgrows its map size stops accepting writes
// with MDB_MAP_FULL, and a delta-syncrepl journal that fills stops advancing —
// the second is worse, because it fails on write *rate*, not data volume.
//
// The map size is an address-space reservation, not an allocation: LMDB grows
// the file sparsely inside it. The real bound on a database is therefore the
// PVC, not this number, so the default is set high enough that the volume is
// always the thing that runs out first — which is the limit an operator can
// actually see, alert on and expand.
//
// 32Gi data / 8Gi journal: the journal is purged on a schedule
// (spec.replication.accesslogPurge) so it only ever needs to hold a purge
// window's worth of change records, not a copy of the directory.
const (
	defaultDataMaxSizeBytes      int64 = 32 << 30 // 32Gi
	defaultAccesslogMaxSizeBytes int64 = 8 << 30  // 8Gi
)

// parseMaxSize turns a user-supplied map size into the bare byte count
// olcDbMaxSize takes. Accepts both the Kubernetes quantity the field's doc
// comment has always promised ("32Gi") and the bare byte count the operator has
// always actually passed through ("34359738368") — a plain integer is a valid
// quantity, so one parser serves both and no existing spec value changes
// meaning. Resolves the doc/implementation mismatch ADR-024 calls out, in the
// direction that keeps every previously-working value working.
//
// Zero and negative are errors rather than "unlimited": there is no such thing
// as an unbounded LMDB map, and accepting 0 would hand slapd a value it treats
// as "use the default" — silence where the user asked for something.
func parseMaxSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty map size")
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0, fmt.Errorf("parse map size %q (want a byte count or a quantity like 32Gi): %w", s, err)
	}
	v, ok := q.AsInt64()
	if !ok {
		return 0, fmt.Errorf("map size %q does not fit in an int64", s)
	}
	if v <= 0 {
		return 0, fmt.Errorf("map size %q must be positive", s)
	}
	return v, nil
}

// desiredDataMaxSize resolves spec.maxSize into bytes, falling back to the
// operator default when unset (ADR-024 R5). An unparseable value is an error,
// not a silent fallback — R4 forbids accepting a spec field and dropping it.
func desiredDataMaxSize(sd *ldapv1alpha1.SlapdDatabase) (int64, error) {
	if strings.TrimSpace(sd.Spec.MaxSize) == "" {
		return defaultDataMaxSizeBytes, nil
	}
	return parseMaxSize(sd.Spec.MaxSize)
}

// maxSizeVerdict is what comparing the live olcDbMaxSize against the desired
// value concluded.
type maxSizeVerdict int

const (
	// maxSizeMatches — cn=config already carries the desired value.
	maxSizeMatches maxSizeVerdict = iota
	// maxSizeNeedsGrow — cn=config has a smaller value, or none at all (which
	// means back-mdb's ~10 MB).
	maxSizeNeedsGrow
	// maxSizeNeedsShrink — the spec asks for a SMALLER map than the database
	// already has.
	maxSizeNeedsShrink
)

// compareMaxSize compares the live olcDbMaxSize against the desired byte count.
//
// NEITHER verdict is written to a running database. This is the one place where
// the live cluster overruled the design: ADR-024 named the map size as the
// worked example of a field that had no business being create-only, and growing
// it live looked safe on paper. It is not. An `ldapmodify` of olcDbMaxSize
// against a running back-mdb database **segfaults slapd** — observed on
// OpenLDAP 2.7.1, exit code 139, immediately after the MOD is logged:
//
//	conn=1011 op=6 MOD dn="olcDatabase={1}mdb,cn=config"
//	conn=1011 op=6 MOD attr=olcDbMaxSize
//	slap_get_csn: conn=1011 op=6 generated new csn=…
//	<process dies>
//
// The mechanism is LMDB's own contract: mdb_env_set_mapsize may only be called
// with no transactions active in the process, and a live slapd always has some.
// So the map size is set when the database is CREATED — where it is safe,
// because the backend is initialised with it and there is no live environment
// to resize — and on an existing database a mismatch is reported, never
// applied. ADR-024 R4 is still honoured: the divergence gets a status
// condition, which is R2's "documents itself as bootstrap-time and reports the
// pending-reload state", not R4's forbidden silence.
//
// An unparseable current value (a hand-edit) counts as "smaller than ours" so
// it is reported rather than quietly accepted.
func compareMaxSize(current string, desired int64) (maxSizeVerdict, string) {
	cur := strings.TrimSpace(current)
	if cur == "" {
		return maxSizeNeedsGrow, ""
	}
	curBytes, err := strconv.ParseInt(cur, 10, 64)
	if err != nil {
		return maxSizeNeedsGrow, cur
	}
	switch {
	case curBytes == desired:
		return maxSizeMatches, cur
	case curBytes > desired:
		return maxSizeNeedsShrink, cur
	default:
		return maxSizeNeedsGrow, cur
	}
}

// ── Search limits (ADR-024 R5) ──────────────────────────────────────────────

// slaptainDefaultSearchLimit is what an unset spec.sizeLimit / spec.timeLimit
// resolves to. slapd's built-ins — 500 entries, 3600 seconds — are sized for a
// demo directory: a client enumerating a real subtree gets a silently truncated
// answer with a result code most client libraries do not surface. The way back
// to bare OpenLDAP behaviour is asking for it (`sizeLimit: "500"`), never
// silence (ADR-024 R5).
const slaptainDefaultSearchLimit = "unlimited"

// desiredSizeLimit resolves spec.sizeLimit into the olcSizeLimit value to write
// and whether to write one at all.
func desiredSizeLimit(sd *ldapv1alpha1.SlapdDatabase) (string, bool) {
	return resolveSearchLimit(sd.Spec.SizeLimit)
}

// desiredTimeLimit is the olcTimeLimit counterpart.
func desiredTimeLimit(sd *ldapv1alpha1.SlapdDatabase) (string, bool) {
	return resolveSearchLimit(sd.Spec.TimeLimit)
}

func resolveSearchLimit(v *string) (string, bool) {
	if v == nil {
		return slaptainDefaultSearchLimit, true
	}
	s := strings.TrimSpace(*v)
	if s == "" {
		return "", false
	}
	return s, true
}

// ── Baseline indices (ADR-024 R7) ───────────────────────────────────────────

// dataBaselineIndexAttrs is the equality index set every data database carries
// whatever the user declares in spec.indices.
//
// entryCSN and entryUUID are used by syncrepl itself: the consumer's refresh
// filter asserts on entryCSN, and out-of-order modify resolution looks entries
// up by entryUUID. Unindexed, each is a full scan of the database on a
// replication hot path — the same reasoning that already gave the accesslog DB
// its index set (accesslogIndexAttrs); the data DB simply never got it.
// objectClass is the set slapd itself assumes for almost every filter.
//
// Operator-set and not removable through the API (ADR-024 R7): these implement
// replication, which the operator owns end to end.
var dataBaselineIndexAttrs = []string{"objectClass", "entryCSN", "entryUUID"}

// planDataBaselineIndices returns the olcDbIndex values to ADD so a data DB
// covers dataBaselineIndexAttrs. Same seam, same rules as the accesslog
// planner — see planIndices.
func planDataBaselineIndices(current []string) []string {
	return planIndices(current, dataBaselineIndexAttrs)
}

// planIndices returns the olcDbIndex values to ADD so that `want` is covered,
// given what the database carries today. nil means nothing to do. Pure — the
// LDAP half only executes the verdict. Passing nil current plans a fresh DB's
// whole set, so creation and convergence share one seam.
//
// An attribute already named in any live value counts as configured, whatever
// its index types: back-mdb rejects a SECOND definition for an attribute that
// already has one, so a hand-set richer index (say "entryUUID eq,sub") must be
// left alone rather than duplicated.
//
// Converging an existing DB is safe on back-mdb: per slapd-mdb(5), changing
// index settings by LDAPModifying cn=config rebuilds the indices online in a
// background task — the slapindex(8) requirement applies to slapd.conf-era
// changes, not to a cn=config modify.
func planIndices(current, want []string) []string {
	indexed := make(map[string]bool, len(current)*2)
	for _, v := range current {
		fields := strings.Fields(v)
		if len(fields) == 0 {
			continue
		}
		// "<attrlist> [<types>]" — the attribute list is the whitespace-tolerant
		// remainder once a trailing type field is dropped.
		attrList := fields[0]
		if len(fields) > 1 {
			attrList = strings.Join(fields[:len(fields)-1], "")
		}
		for _, a := range strings.Split(attrList, ",") {
			if a = strings.TrimSpace(a); a != "" {
				indexed[strings.ToLower(a)] = true
			}
		}
	}

	var missing []string
	for _, a := range want {
		if !indexed[strings.ToLower(a)] {
			missing = append(missing, a)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return []string{strings.Join(missing, ",") + " eq"}
}

// ── back-mdb backend (ADR-024 R2) ───────────────────────────────────────────

// mdbIdlExponent resolves spec.backend.idlExponent into the value the init
// container should emit as the back-mdb backend's `idlexp` directive, and
// whether to emit one at all.
//
// Bootstrap-time, never converged: the backend entry is initialised before any
// database exists and the attribute governs on-disk index layout, so a late
// write would apply to only part of the database (ADR-024 R2). Unset emits
// nothing, leaving slapd's own default — see SlapdMdbBackendConfig for why this
// one field deliberately does not carry an operator-side default.
func mdbIdlExponent(sc *ldapv1alpha1.SlapdCluster) (int32, bool) {
	if sc == nil || sc.Spec.Backend == nil || sc.Spec.Backend.IDLExponent == nil {
		return 0, false
	}
	return *sc.Spec.Backend.IDLExponent, true
}

// ── Convergence (the LDAP half) ─────────────────────────────────────────────

// readConfigAttr reads one multi-valued attribute off a cn=config entry.
func readConfigAttr(conn *ldap.Conn, dn, attr string) ([]string, error) {
	sr, err := conn.Search(ldap.NewSearchRequest(
		dn, ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		1, 0, false, "(objectClass=*)", []string{attr}, nil,
	))
	if err != nil {
		return nil, fmt.Errorf("read %s on %s: %w", attr, dn, err)
	}
	if len(sr.Entries) == 0 {
		return nil, fmt.Errorf("no entry at %s", dn)
	}
	return sr.Entries[0].GetEqualFoldAttributeValues(attr), nil
}

// ensureLimits aligns a database's olcLimits to desired, modifying only when it
// differs. Same read-compare-write shape as applyACLs, and deliberately run on
// every reconcile whether or not the user declared any limits of their own: the
// replication identity's exemption is operator-owned (ADR-024 R7), so it must
// not ride on a user field being non-empty the way the replication ACL
// currently rides on spec.acls.
func (r *SlapdDatabaseReconciler) ensureLimits(
	ctx context.Context,
	conn *ldap.Conn,
	host, dbDN string,
	desired []string,
) error {
	log := logf.FromContext(ctx)

	current, err := readConfigAttr(conn, dbDN, "olcLimits")
	if err != nil {
		return err
	}
	if limitsMatch(current, desired) {
		log.V(1).Info("limits already up-to-date", "host", host, "dn", dbDN)
		return nil
	}

	log.Info("aligning olcLimits", "host", host, "dn", dbDN, "rules", len(desired))
	modReq := ldap.NewModifyRequest(dbDN, nil)
	if len(desired) == 0 {
		// Nothing desired but something stored: drop the attribute entirely.
		// Delete of an absent attribute would error, but limitsMatch already
		// proved there is something there.
		modReq.Delete("olcLimits", nil)
	} else {
		modReq.Replace("olcLimits", desired)
	}
	return conn.Modify(modReq)
}

// ensureSearchLimits converges olcSizeLimit and olcTimeLimit on a data DB.
//
// slapd normalises "none" to "unlimited" on read, so the comparison treats the
// two as equal; without that this would rewrite the attribute on every single
// reconcile.
func (r *SlapdDatabaseReconciler) ensureSearchLimits(
	ctx context.Context,
	conn *ldap.Conn,
	host, dataDN string,
	sd *ldapv1alpha1.SlapdDatabase,
) error {
	log := logf.FromContext(ctx)

	sizeValue, sizeWrite := desiredSizeLimit(sd)
	timeValue, timeWrite := desiredTimeLimit(sd)

	for _, spec := range []struct {
		attr  string
		value string
		write bool
	}{
		{"olcSizeLimit", sizeValue, sizeWrite},
		{"olcTimeLimit", timeValue, timeWrite},
	} {
		if !spec.write {
			continue
		}
		current, err := readConfigAttr(conn, dataDN, spec.attr)
		if err != nil {
			return err
		}
		if len(current) == 1 && searchLimitEqual(current[0], spec.value) {
			continue
		}
		log.Info("aligning search limit", "host", host, "attr", spec.attr,
			"from", current, "to", spec.value)
		modReq := ldap.NewModifyRequest(dataDN, nil)
		modReq.Replace(spec.attr, []string{spec.value})
		if err := conn.Modify(modReq); err != nil {
			return fmt.Errorf("set %s on %s: %w", spec.attr, dataDN, err)
		}
	}
	return nil
}

// searchLimitEqual compares two olcSizeLimit/olcTimeLimit values. "none",
// "unlimited" and "-1" are the same thing to slapd; everything else is a
// numeric comparison so "0500" and "500" do not oscillate.
func searchLimitEqual(a, b string) bool {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "none" || s == "-1" {
			return "unlimited"
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return strconv.FormatInt(n, 10)
		}
		return s
	}
	return norm(a) == norm(b)
}

// checkMaxSize reports — and never fixes — a divergence between the desired map
// size and what a live database carries. See compareMaxSize for why writing it
// is not an option (it segfaults slapd).
//
// The report is a status condition on the SlapdDatabase, not an error: the
// database is otherwise healthy, and failing its reconcile would wedge
// replication over a condition no reconcile can clear. The change path is a
// database recreate (restore into a fresh database, ADR-014), which is the same
// shape as every other bootstrap-time attribute's (ADR-024 R2).
func (r *SlapdDatabaseReconciler) checkMaxSize(
	ctx context.Context,
	conn *ldap.Conn,
	host, dbDN, what string,
	desired int64,
	sd *ldapv1alpha1.SlapdDatabase,
) error {
	log := logf.FromContext(ctx)

	current, err := readConfigAttr(conn, dbDN, "olcDbMaxSize")
	if err != nil {
		return err
	}
	var cur string
	if len(current) > 0 {
		cur = current[0]
	}

	verdict, curVal := compareMaxSize(cur, desired)
	if verdict == maxSizeMatches {
		return nil
	}

	shown := curVal
	if shown == "" {
		shown = "unset (back-mdb's ~10 MB default)"
	}
	var msg string
	if verdict == maxSizeNeedsShrink {
		msg = fmt.Sprintf(
			"%s on %s has olcDbMaxSize %s; spec asks for %d bytes. A map size is fixed "+
				"once the database exists — LMDB cannot be resized under a running slapd "+
				"(the modify segfaults it), and it cannot map an environment smaller than "+
				"the data it holds at all. Raise spec.maxSize back to %s, or recreate the "+
				"database to shrink it.", what, host, shown, desired, curVal)
	} else {
		msg = fmt.Sprintf(
			"%s on %s has olcDbMaxSize %s; spec asks for %d bytes. A map size is fixed once "+
				"the database exists — LMDB cannot be resized under a running slapd (the "+
				"modify segfaults it). Recreate the database (back up, delete, restore into "+
				"a fresh one) to apply the larger map.", what, host, shown, desired)
	}

	log.Info("olcDbMaxSize diverges from spec and cannot be applied to a live database",
		"host", host, "dn", dbDN, "current", shown, "desired", desired)
	setCondition(&sd.Status.Conditions, metav1.Condition{
		Type:               tunablesConvergedCondition,
		Status:             metav1.ConditionFalse,
		Reason:             "RecreateRequired",
		Message:            msg,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: sd.Generation,
	})
	return nil
}

// ensureBackendTunables converges the back-mdb tunables a running slapd will
// accept: the fsync mode, the checkpoint interval, the read-transaction bound
// and — when the cluster runs the monitor backend — this database's operation
// counters.
//
// All four were verified modifiable against a live OpenLDAP 2.7.1 database
// before being classified R1; olcDbEnvFlags was verified the same way and
// failed, which is why it is handled by checkEnvFlags instead.
//
// noSync is the one that closes an existing ADR-024 R4 hole: the field shipped
// create-only, so flipping it on a live SlapdDatabase used to be a silent no-op.
//
// checkpointDefault lets the caller pass the accesslog database's longer
// interval; pass "" to use whatever desiredCheckpoint resolves for the data
// database.
func (r *SlapdDatabaseReconciler) ensureBackendTunables(
	ctx context.Context,
	conn *ldap.Conn,
	host, dbDN string,
	sd *ldapv1alpha1.SlapdDatabase,
	sc *ldapv1alpha1.SlapdCluster,
	checkpointOverride string,
) error {
	log := logf.FromContext(ctx)

	noSync := "FALSE"
	if desiredNoSync(sd, sc) {
		noSync = "TRUE"
	}

	checkpoint, writeCheckpoint := desiredCheckpoint(sd)
	if checkpointOverride != "" {
		checkpoint, writeCheckpoint = checkpointOverride, true
	}

	monitoring := "FALSE"
	if monitoringEnabled(sc) {
		monitoring = "TRUE"
	}

	// Each attribute is read and compared before it is written. The read is the
	// point: an unconditional Replace would rewrite cn=config on every reconcile
	// of every pod, and every one of those writes is a cn=config modification
	// slapd logs and journals.
	type want struct {
		attr  string
		value string
		write bool
	}
	for _, w := range []want{
		{"olcDbNoSync", noSync, true},
		{"olcDbCheckpoint", checkpoint, writeCheckpoint},
		{"olcDbRtxnSize", strconv.FormatInt(int64(desiredRtxnSize(sd)), 10), true},
		{"olcMonitoring", monitoring, true},
	} {
		current, err := readConfigAttr(conn, dbDN, w.attr)
		if err != nil {
			return err
		}
		if !w.write {
			// Nothing desired. Only delete when something is actually stored —
			// a delete of an absent attribute is an error.
			if len(current) == 0 {
				continue
			}
			log.Info("removing tunable", "host", host, "dn", dbDN, "attr", w.attr)
			modReq := ldap.NewModifyRequest(dbDN, nil)
			modReq.Delete(w.attr, nil)
			if err := conn.Modify(modReq); err != nil {
				return fmt.Errorf("delete %s on %s: %w", w.attr, dbDN, err)
			}
			continue
		}
		if len(current) == 1 && strings.EqualFold(strings.TrimSpace(current[0]), w.value) {
			continue
		}
		log.Info("aligning tunable", "host", host, "dn", dbDN, "attr", w.attr,
			"from", current, "to", w.value)
		modReq := ldap.NewModifyRequest(dbDN, nil)
		modReq.Replace(w.attr, []string{w.value})
		if err := conn.Modify(modReq); err != nil {
			return fmt.Errorf("set %s on %s: %w", w.attr, dbDN, err)
		}
	}
	return nil
}

// checkEnvFlags reports — and never fixes — a divergence between spec.envFlags
// and what a live database carries. The twin of checkMaxSize, for the same
// reason: writing it can kill the process.
//
// The evidence is a captured crash rather than a manual's claim. On t3e, an
// ldapmodify replacing olcDbEnvFlags with "writemap nometasync" on a running
// 2.7.1 database produced `ldap_result: Can't contact LDAP server (-1)` on the
// client and exit code 139 on the pod, with the MOD as the last thing logged.
// MDB_WRITEMAP is an mdb_env_open flag; LMDB has no path to add it to an open
// environment, and back-mdb's config handler does not refuse the attempt.
//
// Like checkMaxSize this reports rather than errors: the database is otherwise
// healthy and failing its reconcile would wedge replication over a condition no
// reconcile can clear.
func (r *SlapdDatabaseReconciler) checkEnvFlags(
	ctx context.Context,
	conn *ldap.Conn,
	host, dbDN string,
	sd *ldapv1alpha1.SlapdDatabase,
) error {
	log := logf.FromContext(ctx)

	desired := desiredEnvFlags(sd)
	current, err := readConfigAttr(conn, dbDN, "olcDbEnvFlags")
	if err != nil {
		return err
	}
	if envFlagsMatch(current, desired) {
		return nil
	}

	shown := "none"
	if len(current) > 0 {
		shown = strings.Join(current, ",")
	}
	wanted := "none"
	if len(desired) > 0 {
		wanted = strings.Join(desired, ",")
	}
	msg := fmt.Sprintf(
		"the data database on %s has olcDbEnvFlags %s; spec.envFlags asks for %s. "+
			"LMDB environment flags are fixed once the database exists — adding "+
			"writemap to a running back-mdb database segfaults slapd, so the "+
			"operator will not write them. Recreate the database (back up, delete, "+
			"restore into a fresh one) to apply them.", host, shown, wanted)

	log.Info("olcDbEnvFlags diverges from spec and cannot be applied to a live database",
		"host", host, "dn", dbDN, "current", shown, "desired", wanted)
	setCondition(&sd.Status.Conditions, metav1.Condition{
		Type:               tunablesConvergedCondition,
		Status:             metav1.ConditionFalse,
		Reason:             "RecreateRequired",
		Message:            msg,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: sd.Generation,
	})
	return nil
}

// ensureDataBaselineIndices adds whatever planDataBaselineIndices finds missing
// on a data DB. Converged on every reconcile, independent of spec.indices:
// entryCSN and entryUUID implement replication, so they must not depend on the
// user having declared an index list at all (ADR-024 R7).
func (r *SlapdDatabaseReconciler) ensureDataBaselineIndices(
	ctx context.Context,
	conn *ldap.Conn,
	host, dataDN string,
) error {
	log := logf.FromContext(ctx)

	current, err := readConfigAttr(conn, dataDN, "olcDbIndex")
	if err != nil {
		return err
	}
	missing := planDataBaselineIndices(current)
	if len(missing) == 0 {
		return nil
	}

	log.Info("adding missing baseline indices", "host", host, "dn", dataDN, "add", missing)
	modReq := ldap.NewModifyRequest(dataDN, nil)
	modReq.Add("olcDbIndex", missing)
	return conn.Modify(modReq)
}

// tunablesConvergedCondition reports whether every operator-managed tunable on
// this database matches cn=config on every pod. False means a value the
// operator cannot apply to a live database has diverged — today only the map
// size (ADR-024 R4: a spec field that cannot be honoured is reported, never
// silently ignored).
const tunablesConvergedCondition = "TunablesConverged"

// splitIndexValue splits an olcDbIndex value "<attrlist> [<types>]" into its
// attribute list and its (possibly empty) type field. Whitespace-tolerant about
// the comma-separated list, the way slapd is.
func splitIndexValue(v string) (attrs []string, types string) {
	fields := strings.Fields(v)
	if len(fields) == 0 {
		return nil, ""
	}
	attrList := fields[0]
	if len(fields) > 1 {
		attrList = strings.Join(fields[:len(fields)-1], "")
		types = fields[len(fields)-1]
	}
	for _, a := range strings.Split(attrList, ",") {
		if a = strings.TrimSpace(a); a != "" {
			attrs = append(attrs, a)
		}
	}
	return attrs, types
}

// indexedAttrs is the set of attributes any of these olcDbIndex values covers,
// lowercased.
func indexedAttrs(values []string) map[string]bool {
	set := make(map[string]bool, len(values)*2)
	for _, v := range values {
		attrs, _ := splitIndexValue(v)
		for _, a := range attrs {
			set[strings.ToLower(a)] = true
		}
	}
	return set
}

// planUserIndices returns the user-declared olcDbIndex values still to be
// ADDED, with attributes that are already indexed subtracted out.
//
// Subtracting per ATTRIBUTE rather than per value is not a refinement, it is
// the only correct reading: back-mdb rejects a second definition for an
// attribute that already has one with "duplicate index definition for attr
// <x>" (LDAP result 80), and that error fails the whole modify. Comparing whole
// values — which is what this code used to do — misses it whenever the same
// attribute appears inside a differently-spelled value, and the operator's own
// baseline set is exactly such a value: one combined
// "objectClass,entryCSN,entryUUID eq" against a spec.indices that lists
// "entryCSN eq" separately. Observed live on t3e: every pod of both databases
// wedged in Error on the first reconcile after the database was created.
//
// The user's index types are preserved for whatever attributes survive, so
// "uid eq,sub" stays "uid eq,sub" and a partially-covered "objectClass,sn eq"
// becomes "sn eq".
func planUserIndices(current, desired []string) []string {
	indexed := indexedAttrs(current)

	var out []string
	for _, d := range desired {
		attrs, types := splitIndexValue(d)
		var remaining []string
		for _, a := range attrs {
			if !indexed[strings.ToLower(a)] {
				remaining = append(remaining, a)
				// A single modify must not name the same attribute twice
				// either, so claim it as we go.
				indexed[strings.ToLower(a)] = true
			}
		}
		if len(remaining) == 0 {
			continue
		}
		v := strings.Join(remaining, ",")
		if types != "" {
			v += " " + types
		}
		out = append(out, v)
	}
	return out
}
