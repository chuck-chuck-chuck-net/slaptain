package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	ldap "github.com/go-ldap/ldap/v3"
	"k8s.io/apimachinery/pkg/api/resource"
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

// maxSizeAction is what planMaxSize decided to do with olcDbMaxSize.
type maxSizeAction int

const (
	// maxSizeNoop — cn=config already carries the desired value.
	maxSizeNoop maxSizeAction = iota
	// maxSizeWrite — write the desired value (a fresh database, or a grow).
	maxSizeWrite
	// maxSizeShrinkRejected — the spec asks for a SMALLER map than the
	// database already has. Never written; reported instead (ADR-024 R4).
	maxSizeShrinkRejected
)

// planMaxSize compares the live olcDbMaxSize against the desired byte count.
//
// Growing is safe on a live database: slapd applies olcDbMaxSize to the running
// environment, and a larger map is a larger reservation over the same data.
// Shrinking is not — LMDB cannot map an environment smaller than the data it
// already holds, and a map size below the current one is either refused by
// slapd or, worse, accepted into cn=config while the running environment keeps
// the old size, which is exactly the spec/cn=config divergence ADR-024 R4
// exists to forbid. So a shrink is reported, never applied; the returned value
// in that case is the CURRENT size, for the message.
//
// An unparseable current value (a hand-edit) is treated as "not ours" and
// overwritten — convergence's whole job.
func planMaxSize(current string, desired int64) (maxSizeAction, string) {
	want := strconv.FormatInt(desired, 10)
	cur := strings.TrimSpace(current)
	if cur == "" {
		return maxSizeWrite, want
	}
	curBytes, err := strconv.ParseInt(cur, 10, 64)
	if err != nil {
		return maxSizeWrite, want
	}
	switch {
	case curBytes == desired:
		return maxSizeNoop, ""
	case curBytes > desired:
		return maxSizeShrinkRejected, cur
	default:
		return maxSizeWrite, want
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

// ensureMaxSize converges olcDbMaxSize on one database (data or accesslog).
//
// A grow is applied live. A SHRINK is refused with an error rather than
// silently dropped: the API accepted the edit, so the user gets a signal
// (the SlapdDatabase goes Degraded with this message) instead of a spec that
// says one thing while cn=config keeps another — ADR-024 R4, the rule this
// field's create-only past is the debt against.
func (r *SlapdDatabaseReconciler) ensureMaxSize(
	ctx context.Context,
	conn *ldap.Conn,
	host, dbDN string,
	desired int64,
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

	action, value := planMaxSize(cur, desired)
	switch action {
	case maxSizeNoop:
		return nil
	case maxSizeShrinkRejected:
		return fmt.Errorf(
			"refusing to shrink olcDbMaxSize on %s at %s from %s to %d bytes: LMDB cannot "+
				"map an environment smaller than the data it holds; raise spec.maxSize back to "+
				"at least %s, or recreate the database to shrink it (ADR-024 R4)",
			dbDN, host, value, desired, value)
	}

	log.Info("aligning olcDbMaxSize", "host", host, "dn", dbDN, "from", cur, "to", value)
	modReq := ldap.NewModifyRequest(dbDN, nil)
	modReq.Replace("olcDbMaxSize", []string{value})
	return conn.Modify(modReq)
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
