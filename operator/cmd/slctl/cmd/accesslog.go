package cmd

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// Per-database accesslog inspection (ADR-019).
//
// Everything in this file is pure: it takes what was observed off a pod's
// rootDSE and cn=config plus the SlapdDatabase CRs that state the intent, and
// returns checkResults. No LDAP, no k8s, no I/O — inspect.go does the talking.
//
// ADR-019 R5: nothing here spells the accesslog suffix literally. Both the
// per-database prefix and the pre-ADR-019 shared suffix are derived from the one
// API helper that owns the naming, and TestAccesslogDerivationDriftGuard pins
// that derivation.

// accesslogSuffixPrefix is the common prefix of every per-database accesslog
// suffix ("<accesslog>-"), obtained by asking the naming helper for the suffix
// of a database with an empty name. Used to recognise *any* accesslog naming
// context, including one whose SlapdDatabase CR we do not know about.
var accesslogSuffixPrefix = ldapv1alpha1.AccesslogSuffix("")

// legacySharedAccesslogSuffix is the suffix of the single cluster-shared
// accesslog database the operator created before ADR-019: the per-database
// prefix without the per-database separator. slctl reads naming contexts off a
// live rootDSE, so it must be able to recognise a legacy log on a pod the
// operator has not converged yet (ADR-019 R8) and say so, rather than report it
// as an unknown database.
var legacySharedAccesslogSuffix = strings.TrimSuffix(accesslogSuffixPrefix, "-")

// isAccesslogSuffix reports whether an LDAP suffix names a per-database
// accesslog. Prefix match, case-folded: cn=config DN values are
// case-insensitive and slapd normalises them on its own schedule.
func isAccesslogSuffix(nc string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(nc)),
		strings.ToLower(accesslogSuffixPrefix))
}

// isLegacySharedAccesslog reports whether an LDAP suffix is exactly the
// pre-ADR-019 shared log's. Exact, so a database that happens to be called
// "default" is never mistaken for it.
func isLegacySharedAccesslog(nc string) bool {
	return strings.EqualFold(strings.TrimSpace(nc), legacySharedAccesslogSuffix)
}

// isAnyAccesslogSuffix covers both shapes — used where the question is "is this
// a change journal at all", as on a read-only replica, which must have none.
func isAnyAccesslogSuffix(nc string) bool {
	return isAccesslogSuffix(nc) || isLegacySharedAccesslog(nc)
}

// dbIdentity is the *intent* side of the checks: one SlapdDatabase CR reduced
// to what accesslog verification needs.
type dbIdentity struct {
	// Name is the CR name — the key the accesslog suffix and directory are
	// derived from (ADR-019 R1).
	Name string
	// Suffix is the data DB's LDAP suffix, used to find the matching
	// olcMdbConfig entry on a pod.
	Suffix string
	// WantLog is true when this database is expected to have an accesslog DB of
	// its own on every RW pod: the cluster needs an accesslog at all and this
	// database uses delta-syncrepl.
	WantLog bool
	// ExternalLogBase is the logbase the operator puts in this database's
	// external-peer stanzas (ADR-019 R9) — the peer's log suffix, possibly
	// overridden via spec.replication.externalAccesslogSuffix.
	ExternalLogBase string
}

// observedDB is one olcMdbConfig child of cn=config as read off a pod.
type observedDB struct {
	// DN carries slapd's {N} ordering prefix.
	DN string
	// Suffix is olcSuffix.
	Suffix string
	// Dir is olcDbDirectory.
	Dir string
	// AccessLogDB is the olcAccessLogDB of this database's accesslog overlay,
	// or "" when it has no accesslog overlay.
	AccessLogDB string
	// SyncRepl holds the raw olcSyncRepl stanza values.
	SyncRepl []string
}

// podAccesslogState is what one pod showed. Skip marks a pod that could not be
// queried — unreachable pods are reported by pod-readiness, not here.
type podAccesslogState struct {
	Name           string
	RO             bool
	Skip           bool
	NamingContexts []string
	DBs            []observedDB
}

// checkNamingContexts verifies the rootDSE naming contexts against the declared
// databases: every replicated database has its own accesslog on every RW pod
// (ADR-019 R1/R2), every RW pod serves data, and no RO pod carries a change
// journal at all (a read-only consumer produces no changes, so a log there is a
// misconfiguration whatever it is called).
//
// A legacy shared log, or a log with no SlapdDatabase CR behind it, is reported
// as such and warns rather than fails: both are legitimate transient states of
// a cluster the operator has not finished converging (ADR-019 R8), and neither
// is by itself the condition ADR-019 forbids. What does fail is a *missing* log
// — that is a database with no journal, not a database mid-migration.
func checkNamingContexts(dbs []dbIdentity, pods []podAccesslogState) checkResult {
	var issues, notes []string

	// Log suffixes we can attribute to a declared database. Includes databases
	// that want no log, so that a leftover log of a demoted database is not
	// reported as an orphan of an unknown CR.
	known := make(map[string]string, len(dbs))
	wantLogs := 0
	for _, db := range dbs {
		known[strings.ToLower(ldapv1alpha1.AccesslogSuffix(db.Name))] = db.Name
		if db.WantLog {
			wantLogs++
		}
	}

	for _, ps := range pods {
		if ps.Skip {
			continue
		}
		if ps.RO {
			for _, nc := range ps.NamingContexts {
				if isAnyAccesslogSuffix(nc) {
					issues = append(issues,
						fmt.Sprintf("%s: RO pod should not have %s", ps.Name, nc))
				}
			}
			continue
		}

		present := make(map[string]bool, len(ps.NamingContexts))
		hasData := false
		for _, nc := range ps.NamingContexts {
			present[strings.ToLower(strings.TrimSpace(nc))] = true
			switch {
			case isLegacySharedAccesslog(nc):
				notes = append(notes, fmt.Sprintf(
					"%s: legacy cluster-shared log %s still present (pre-ADR-019; converges on reconcile)",
					ps.Name, nc))
			case isAccesslogSuffix(nc):
				if _, ok := known[strings.ToLower(nc)]; !ok {
					notes = append(notes, fmt.Sprintf(
						"%s: orphan accesslog %s (no SlapdDatabase declares it)", ps.Name, nc))
				}
			case !strings.HasPrefix(nc, "cn="):
				hasData = true
			}
		}
		for _, db := range dbs {
			if !db.WantLog {
				continue
			}
			want := ldapv1alpha1.AccesslogSuffix(db.Name)
			if !present[strings.ToLower(want)] {
				issues = append(issues, fmt.Sprintf("%s: missing %s for database %s",
					ps.Name, want, db.Name))
			}
		}
		if !hasData {
			issues = append(issues, fmt.Sprintf("%s: missing data namingContext", ps.Name))
		}
	}

	switch {
	case len(issues) > 0:
		return checkResult{Name: "naming-contexts", Status: "fail",
			Detail: strings.Join(append(issues, notes...), "; ")}
	case len(notes) > 0:
		return checkResult{Name: "naming-contexts", Status: "warn",
			Detail: strings.Join(notes, "; ")}
	default:
		return checkResult{Name: "naming-contexts", Status: "pass",
			Detail: fmt.Sprintf("RW: data + %d accesslog(s), RO: data only", wantLogs)}
	}
}

// checkAccesslogConsistency is the ADR-019 diagnostic: per replicated database
// and per pod, the accesslog overlay's olcAccessLogDB, every in-cluster
// syncrepl stanza's logbase and the log database's own olcSuffix must be the
// same string, and no two databases may share a log.
//
// Two kinds of finding, deliberately separated:
//
//   - **Local disagreement** — our own three values do not match, or two
//     databases name one log. That is a real defect: the shared-log case is the
//     exact condition ADR-019 forbids (every write to one database kicks the
//     other's consumers into a permanent full refresh), and a logbase naming a
//     log we do not have degrades this pod's consumers the same way. Fails.
//   - **An external peer's logbase** — evaluated on the *provider* (ADR-019 R9),
//     so it names the peer's suffix by design and may be overridden per database
//     via spec.replication.externalAccesslogSuffix. It cannot be validated from
//     here at all, so it is surfaced as information and never fails: a
//     correctly-configured cross-site mesh with a foreign log suffix must not
//     turn slctl inspect red. It is reported precisely because a logbase the
//     peer lacks is otherwise silent — the remote search finds nothing and the
//     consumer degrades to full-refresh syncrepl with everything still looking
//     healthy.
//
// A value still naming the legacy cluster-shared log warns instead of failing
// (mid-migration is legitimate — ADR-019 R8 — and one database on one log is
// behaviourally correct, just misnamed), *unless* two databases share it, which
// is the forbidden condition and fails whatever the log is called.
func checkAccesslogConsistency(dbs []dbIdentity, pods []podAccesslogState, externalNeedles []string) checkResult {
	var issues, legacyNotes, infoNotes []string
	checked := 0

	for _, ps := range pods {
		if ps.Skip {
			continue
		}

		observed := make(map[string]observedDB, len(ps.DBs))
		logsPresent := make(map[string]bool, len(ps.DBs))
		for _, od := range ps.DBs {
			observed[strings.ToLower(strings.TrimSpace(od.Suffix))] = od
			if isAnyAccesslogSuffix(od.Suffix) {
				logsPresent[strings.ToLower(strings.TrimSpace(od.Suffix))] = true
			}
		}

		// log suffix (lower-cased) -> databases whose overlay names it.
		sharers := make(map[string][]string)

		for _, db := range dbs {
			if !db.WantLog {
				continue
			}
			od, ok := observed[strings.ToLower(strings.TrimSpace(db.Suffix))]
			if !ok {
				// The data DB itself is absent from this pod's cn=config;
				// naming-contexts and the bootstrap checks own that.
				continue
			}
			checked++
			want := ldapv1alpha1.AccesslogSuffix(db.Name)
			legacy := false

			// RO pods run no accesslog overlay and hold no log of their own
			// (they never produce changes), but their stanzas still carry a
			// logbase — naming the RW provider's log.
			if !ps.RO {
				switch {
				case od.AccessLogDB == "":
					issues = append(issues, fmt.Sprintf("%s/%s: no accesslog overlay",
						ps.Name, db.Name))
				case isLegacySharedAccesslog(od.AccessLogDB):
					legacy = true
					legacyNotes = append(legacyNotes, fmt.Sprintf(
						"%s/%s: olcAccessLogDB still names the legacy cluster-shared log %s",
						ps.Name, db.Name, od.AccessLogDB))
				case !strings.EqualFold(od.AccessLogDB, want):
					issues = append(issues, fmt.Sprintf("%s/%s: olcAccessLogDB=%s (want %s)",
						ps.Name, db.Name, od.AccessLogDB, want))
				}
				if od.AccessLogDB != "" {
					k := strings.ToLower(strings.TrimSpace(od.AccessLogDB))
					sharers[k] = append(sharers[k], db.Name)
				}
				if !legacy && od.AccessLogDB != "" && !logsPresent[strings.ToLower(want)] {
					issues = append(issues, fmt.Sprintf("%s/%s: log database %s absent from cn=config",
						ps.Name, db.Name, want))
				}
			}

			for _, sr := range od.SyncRepl {
				lb := stanzaLogbase(sr)
				if lb == "" {
					// Plain syncrepl (ADR-011) emits no logbase at all.
					continue
				}
				if stanzaIsExternal(sr, externalNeedles) {
					if !strings.EqualFold(lb, want) {
						infoNotes = append(infoNotes, fmt.Sprintf("%s→%s", db.Name, lb))
					}
					continue
				}
				switch {
				case isLegacySharedAccesslog(lb):
					legacyNotes = append(legacyNotes, fmt.Sprintf(
						"%s/%s: in-cluster logbase still names the legacy cluster-shared log %s",
						ps.Name, db.Name, lb))
				case !strings.EqualFold(lb, want):
					issues = append(issues, fmt.Sprintf("%s/%s: logbase=%s (want %s)",
						ps.Name, db.Name, lb, want))
				}
			}
		}

		for _, log := range sortedKeys(sharers) {
			names := uniqueStrings(sharers[log])
			if len(names) < 2 {
				continue
			}
			sort.Strings(names)
			issues = append(issues, fmt.Sprintf(
				"%s: databases %s share accesslog %s (ADR-019: every write to one forces the other's consumers into full refresh)",
				ps.Name, strings.Join(names, ","), log))
		}
	}

	external := ""
	if len(infoNotes) > 0 {
		external = "; external logbase (evaluated on the peer, not verifiable locally): " +
			strings.Join(uniqueStrings(infoNotes), ", ")
	}

	switch {
	case len(issues) > 0:
		return checkResult{Name: "accesslog-consistency", Status: "fail",
			Detail: strings.Join(append(issues, legacyNotes...), "; ") + external}
	case len(legacyNotes) > 0:
		return checkResult{Name: "accesslog-consistency", Status: "warn",
			Detail: strings.Join(legacyNotes, "; ") + external}
	default:
		return checkResult{Name: "accesslog-consistency", Status: "pass",
			Detail: fmt.Sprintf("%d database/pod pair(s): logbase = olcAccessLogDB = log olcSuffix%s",
				checked, external)}
	}
}

// logbaseRe extracts the logbase of one olcSyncRepl stanza. The operator always
// quotes it; an unquoted value is accepted for a hand-edited config.
var logbaseRe = regexp.MustCompile(`logbase=(?:"([^"]*)"|(\S+))`)

// stanzaLogbase returns a stanza's logbase, or "" when it has none — which is
// the correct state for a plain-syncrepl peer (ADR-011).
func stanzaLogbase(stanza string) string {
	m := logbaseRe.FindStringSubmatch(stanza)
	if m == nil {
		return ""
	}
	if m[1] != "" {
		return m[1]
	}
	return m[2]
}

// stanzaIsExternal reports whether a stanza points at an external peer, by the
// same needle match printInspectResult uses to label cross-site stanzas: the
// peer's URI, its static Multus podAddresses, or its discovered addresses.
func stanzaIsExternal(stanza string, needles []string) bool {
	for _, n := range needles {
		if n != "" && strings.Contains(stanza, n) {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
