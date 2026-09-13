package controller

import (
	"fmt"
	"sort"
	"strings"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The pure decision seams behind the degrades-at-scale and hygiene tunables
// (ADR-024, production-config review findings 6-16). The LDAP halves —
// ensureBackendTunables on the database side, reconcilePodConfig on the cluster
// side — only execute what these decide.
//
// Every default here is slaptain's opinion, not slapd's (ADR-024 R5), and every
// one of them is reachable back to bare OpenLDAP behaviour by ASKING for it
// (`0`, `""`) rather than by leaving the field unset.
//
// Placement was settled empirically, not on paper: each attribute was modified
// on a running OpenLDAP 2.7.1 pod before it was classified. One of them failed
// that test — see desiredEnvFlags.

const (
	// defaultDataCheckpoint is the back-mdb disk-buffer flush interval for a
	// data database: flush after 1 MB written or 5 minutes elapsed. It is what
	// bounds the loss window noSync opens (slapd-mdb(5): checkpoint "is only
	// needed if the nosync ... option is used"), and it is written
	// unconditionally so that turning noSync on is a one-field change that is
	// already safe rather than a two-field change whose second half is easy to
	// forget.
	defaultDataCheckpoint = "1024 5"
	// defaultAccesslogCheckpoint is the same for a delta-syncrepl journal, on a
	// longer interval. The journal is a derived, purged artefact: losing its
	// tail costs a consumer a full refresh, not data, so it can afford to flush
	// a quarter as often as the database it journals. Operator-set and not a
	// CRD field, exactly like the journal's map size.
	defaultAccesslogCheckpoint = "2048 15"
	// defaultRtxnSize bounds how many entries one back-mdb read transaction
	// covers before it is broken up. This is also slapd's own value; we write it
	// explicitly so it is visible in cn=config and cannot move underneath a
	// running cluster when the base image's OpenLDAP changes.
	defaultRtxnSize int32 = 10000
	// defaultToolThreads is what slapadd uses for index building during a
	// restore, which happens with the cluster scaled to zero. 2 overlaps index
	// building with entry parsing without thrashing a small CPU limit.
	defaultToolThreads int32 = 2
	// defaultTLSProtocolMin pins a TLS 1.2 floor (slapd spells versions
	// <major>.<minor>: 3.3 is TLS 1.2). Without it the floor is whatever the
	// runtime image's OpenSSL permits, which is a policy that changes silently
	// on a base-image bump.
	defaultTLSProtocolMin = "3.3"
	// defaultPasswordHash pins the scheme slapd uses when IT hashes a password
	// for a client. Same value as slapd's own frontend default — the point is
	// that it is stated and converged rather than compiled in.
	defaultPasswordHash = "{SSHA}"
	// defaultLogLevel is stats (256) + consumer-side sync (16384). A replication
	// incident is diagnosed from what was logged while it was going wrong, and
	// at 256 that record does not exist.
	defaultLogLevel int32 = 16640
	// defaultKeepalive is the syncrepl TCP keepalive triple idle:probes:interval
	// — start probing after 4 minutes idle, 3 probes 30s apart. The idle time is
	// deliberately under the 5-minute mark where stateful firewalls and cloud
	// load balancers commonly drop an idle flow: a refreshAndPersist connection
	// is idle by design between writes, so without keepalive the first symptom
	// of a silently dropped connection is a consumer that has stopped consuming
	// and still reports itself Synced (ADR-008 amendment).
	defaultKeepalive = "240:3:30"
	// syncreplTimeoutOpts are the operator's stanza timeouts, appended to every
	// syncrepl stanza.
	//
	// `network-timeout` (slap_bindconf.sb_timeout_net → LDAP_OPT_NETWORK_TIMEOUT)
	// bounds the TCP connect and TLS handshake, so a provider whose node is gone
	// is noticed in 10 seconds instead of at the kernel's TCP timeout.
	//
	// DELIBERATELY NOT emitted, both proven harmful on a running pod:
	//
	// `timeout` (sb_timeout_api → LDAP_OPT_TIMEOUT). Supplying it flips slapd's
	// refresh-phase waiting discipline from a non-blocking peek to a BLOCKING
	// wait: do_syncrep2 polls with tout={0,0} only in the persist phase, and in
	// the refresh phase blocks a threadpool thread inside ldap_result for up to
	// the timeout (servers/slapd/syncrepl.c:1357-1362, OpenLDAP 2.7.1). The
	// task can only honour a cn=config pause between messages (syncrepl.c:2128),
	// and every external config MOD pauses the whole pool with listeners
	// suspended (bconfig.c:6512) — so while any consumer is in refresh phase,
	// every cn=config write the operator makes freezes the ENTIRE server for
	// the remainder of that blocking wait. Measured live at timeout=300: three
	// ~300s total-silence freezes per convergence pass, cascading across pods
	// and sites. And the wait detects nothing: expiry maps to SYNC_TIMEOUT
	// ("nothing to read, listen for more", syncrepl.c:2352), never an abort or
	// retry. Dead-provider detection is the job of the socket-level mechanisms
	// (network-timeout for connect/handshake, keepalive for established flows),
	// which act without occupying a pool thread. Accepted residual: the
	// synchronous syncrepl bind (config.c ldap_sasl_bind_s) is unbounded
	// against a provider that completes the TLS handshake and then hangs —
	// the pre-regression status quo; every observed hang state stalls the
	// handshake itself, which network-timeout bounds.
	//
	// `timelimit`, which maps to LDAP_OPT_TIMELIMIT — a server-side search
	// time limit that WOULD kill a persistent search.
	syncreplTimeoutOpts = " network-timeout=10"
)

// ── Durability: noSync + checkpoint (findings 6 and 7) ──────────────────────

// desiredNoSync resolves this database's olcDbNoSync: the per-database field
// when set, otherwise the cluster-wide posture, otherwise fsync-on.
//
// Two levels because durability is usually a cluster property — every member
// trading fsync for throughput on the argument that the mesh is the redundancy —
// but a single database (a high-churn one, or conversely one holding something
// that must survive a node loss) can legitimately differ (ADR-024 R6).
func desiredNoSync(sd *ldapv1alpha1.SlapdDatabase, sc *ldapv1alpha1.SlapdCluster) bool {
	if sd != nil && sd.Spec.NoSync != nil {
		return *sd.Spec.NoSync
	}
	if sc != nil && sc.Spec.Tuning.NoSync != nil {
		return *sc.Spec.Tuning.NoSync
	}
	return false
}

// desiredCheckpoint resolves spec.checkpoint into the olcDbCheckpoint value and
// whether to write one. An explicitly empty string means "no checkpoint at all",
// which validateDurability refuses to combine with noSync.
func desiredCheckpoint(sd *ldapv1alpha1.SlapdDatabase) (string, bool) {
	if sd == nil || sd.Spec.Checkpoint == nil {
		return defaultDataCheckpoint, true
	}
	v := strings.TrimSpace(*sd.Spec.Checkpoint)
	if v == "" {
		return "", false
	}
	return v, true
}

// validateDurability rejects the one combination the gap analysis names as
// unreachable-by-accident: fsync disabled AND no checkpoint, which loses an
// unbounded window of writes on an unclean shutdown — the database's on-disk
// state can trail its in-memory state by however much has been written since
// the last flush, and nothing schedules a flush.
//
// This is ADR-024 R4 in its "rejected" form: the API takes a value it cannot
// honour safely and says so, rather than writing it and leaving the operator to
// discover the trade at 3am.
func validateDurability(sd *ldapv1alpha1.SlapdDatabase, sc *ldapv1alpha1.SlapdCluster) error {
	if !desiredNoSync(sd, sc) {
		return nil
	}
	if _, write := desiredCheckpoint(sd); write {
		return nil
	}
	return fmt.Errorf(
		"noSync is enabled with spec.checkpoint explicitly disabled: without a "+
			"checkpoint slapd never flushes the disk buffers on its own, so an "+
			"unclean shutdown loses every write since the last one it happened to "+
			"make. Set spec.checkpoint (the default is %q) or turn noSync off",
		defaultDataCheckpoint)
}

// ── olcDbRtxnSize (finding 9) ───────────────────────────────────────────────

// desiredRtxnSize resolves spec.rtxnSize, with 0 kept as a meaningful "no
// limit" rather than treated as unset.
func desiredRtxnSize(sd *ldapv1alpha1.SlapdDatabase) int32 {
	if sd == nil || sd.Spec.RtxnSize == nil {
		return defaultRtxnSize
	}
	return *sd.Spec.RtxnSize
}

// ── olcDbEnvFlags (finding 8) ───────────────────────────────────────────────

// desiredEnvFlags is spec.envFlags, normalised.
//
// CREATE-ONLY, and reported thereafter — the gap analysis proposed this as a
// converged field and the live cluster refused. Adding "writemap" to
// olcDbEnvFlags on a running database SEGFAULTS slapd (OpenLDAP 2.7.1, exit
// 139, captured on a t3e pod mid-MOD; MDB_WRITEMAP is an mdb_env_open flag and
// LMDB cannot add it to an open environment). "nometasync" and "nosync" alone
// modify cleanly, but the attribute is a single multi-valued list and a later
// edit can put "writemap" into it, so the granularity slapd gives us is the
// attribute, and the attribute is not runtime-mutable.
//
// Same class and same handling as olcDbMaxSize (ADR-024's amendment of
// 2026-09-12, extended by the one of 2026-09-13): written at creation, compared
// afterwards, divergence reported through TunablesConverged.
func desiredEnvFlags(sd *ldapv1alpha1.SlapdDatabase) []string {
	if sd == nil {
		return nil
	}
	out := make([]string, 0, len(sd.Spec.EnvFlags))
	for _, f := range sd.Spec.EnvFlags {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, strings.ToLower(f))
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// envFlagsMatch compares a database's live olcDbEnvFlags against the desired
// set. Order- and case-insensitive: slapd stores the flags as it received them
// and nothing about them is ordered, so a comparison that cared would report a
// false divergence nobody can fix.
func envFlagsMatch(current, desired []string) bool {
	norm := func(in []string) []string {
		out := make([]string, 0, len(in))
		for _, v := range in {
			if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
				out = append(out, v)
			}
		}
		sort.Strings(out)
		return out
	}
	c, d := norm(current), norm(desired)
	if len(c) != len(d) {
		return false
	}
	for i := range c {
		if c[i] != d[i] {
			return false
		}
	}
	return true
}

// ── Server-global tuning (finding 10) ───────────────────────────────────────

func desiredToolThreads(sc *ldapv1alpha1.SlapdCluster) int32 {
	if sc == nil || sc.Spec.Tuning.ToolThreads == nil {
		return defaultToolThreads
	}
	return *sc.Spec.Tuning.ToolThreads
}

// ── TLS posture (finding 15) ────────────────────────────────────────────────

func desiredTLSProtocolMin(sc *ldapv1alpha1.SlapdCluster) string {
	if sc == nil || sc.Spec.LDAP.TLS.ProtocolMin == nil {
		return defaultTLSProtocolMin
	}
	return strings.TrimSpace(*sc.Spec.LDAP.TLS.ProtocolMin)
}

// desiredTLSCipherSuite has no operator default on purpose: a cipher list ages
// badly in an operator release, and OpenSSL's own default tracks the
// distribution's crypto policy. Unset writes nothing; cleared removes the
// attribute.
func desiredTLSCipherSuite(sc *ldapv1alpha1.SlapdCluster) (string, bool) {
	if sc == nil || sc.Spec.LDAP.TLS.CipherSuite == nil {
		return "", false
	}
	v := strings.TrimSpace(*sc.Spec.LDAP.TLS.CipherSuite)
	if v == "" {
		return "", false
	}
	return v, true
}

// ── Password hashing (finding 16) ───────────────────────────────────────────

func desiredPasswordHash(sc *ldapv1alpha1.SlapdCluster) string {
	if sc == nil || sc.Spec.LDAP.PasswordHash == nil {
		return defaultPasswordHash
	}
	return strings.TrimSpace(*sc.Spec.LDAP.PasswordHash)
}

// ── Log level (finding 14) ──────────────────────────────────────────────────

// desiredLogLevel resolves spec.logLevel. The field is a pointer so that an
// explicit 0 — "log nothing" — is expressible: with `omitempty` a plain int32 0
// serialises to nothing and a kubebuilder default would overwrite it with ours.
func desiredLogLevel(sc *ldapv1alpha1.SlapdCluster) int32 {
	if sc == nil || sc.Spec.LogLevel == nil {
		return defaultLogLevel
	}
	return *sc.Spec.LogLevel
}

// ── Syncrepl stanza hardening (finding 11) ──────────────────────────────────

// desiredKeepalive resolves spec.replication.keepalive. Unset gets the
// operator's triple rather than nothing, because "nothing" is the setting that
// lets a silently-dropped provider connection look healthy. "none" is the
// explicit way to ask for no keepalive at all.
func desiredKeepalive(sc *ldapv1alpha1.SlapdCluster) string {
	if sc == nil {
		return defaultKeepalive
	}
	v := strings.TrimSpace(sc.Spec.Replication.Keepalive)
	switch {
	case v == "":
		return defaultKeepalive
	case strings.EqualFold(v, "none"):
		return ""
	default:
		return v
	}
}
