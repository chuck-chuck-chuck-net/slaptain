package cmd

import (
	"strings"
	"testing"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// ── Derivation drift guards ───────────────────────────────────────────────────

// slctl must not spell the accesslog suffix literally (ADR-019 R5); the prefix and the
// legacy suffix it needs are derived from the one API helper. These guard that
// derivation against a change to AccesslogSuffix's shape.
func TestAccesslogDerivationDriftGuard(t *testing.T) {
	if got, want := accesslogSuffixPrefix+"default", ldapv1alpha1.AccesslogSuffix("default"); got != want {
		t.Errorf("accesslogSuffixPrefix+db = %q, want %q", got, want)
	}
	if got, want := legacySharedAccesslogSuffix+"-", accesslogSuffixPrefix; got != want {
		t.Errorf("legacySharedAccesslogSuffix+\"-\" = %q, want %q", got, want)
	}
	if !isAccesslogSuffix(ldapv1alpha1.AccesslogSuffix("default")) {
		t.Errorf("isAccesslogSuffix(%q) = false, want true", ldapv1alpha1.AccesslogSuffix("default"))
	}
	if isAccesslogSuffix("dc=example,dc=org") {
		t.Error("isAccesslogSuffix(data suffix) = true, want false")
	}
	// The per-database log of a database called "default" must never be
	// mistaken for the pre-ADR-019 cluster-shared log.
	if isLegacySharedAccesslog(ldapv1alpha1.AccesslogSuffix("default")) {
		t.Errorf("isLegacySharedAccesslog(%q) = true, want false",
			ldapv1alpha1.AccesslogSuffix("default"))
	}
	if !isLegacySharedAccesslog(legacySharedAccesslogSuffix) {
		t.Errorf("isLegacySharedAccesslog(%q) = false, want true", legacySharedAccesslogSuffix)
	}
}

// ── Fixture helpers ───────────────────────────────────────────────────────────

const (
	sufA = "dc=a,dc=example"
	sufB = "dc=b,dc=example"
)

var (
	logA = ldapv1alpha1.AccesslogSuffix("dbA")
	logB = ldapv1alpha1.AccesslogSuffix("dbB")
)

func idA() dbIdentity {
	return dbIdentity{Name: "dbA", Suffix: sufA, WantLog: true, ExternalLogBase: logA}
}

func idB() dbIdentity {
	return dbIdentity{Name: "dbB", Suffix: sufB, WantLog: true, ExternalLogBase: logB}
}

// dataDB builds the observed olcMdbConfig entry of a data database with one
// in-cluster delta-syncrepl stanza.
func dataDB(ordinal int, suffix, overlayLog, stanzaLogbase string) observedDB {
	return observedDB{
		DN:          dbDN(ordinal),
		Suffix:      suffix,
		AccessLogDB: overlayLog,
		SyncRepl: []string{
			`{0}rid=101 provider="ldaps://slapd-0.slapd-headless.ns.svc:1025" ` +
				`searchbase="` + suffix + `" type=refreshAndPersist ` +
				`syncdata=accesslog logbase="` + stanzaLogbase + `" ` +
				`logfilter="(&(objectClass=auditWriteObject)(reqResult=0))"`,
		},
	}
}

// logDB builds the observed olcMdbConfig entry of an accesslog database.
func logDB(ordinal int, suffix, dir string) observedDB {
	return observedDB{DN: dbDN(ordinal), Suffix: suffix, Dir: dir}
}

func dbDN(ordinal int) string {
	return "olcDatabase={" + itoa(ordinal) + "}mdb,cn=config"
}

func itoa(i int) string { return string(rune('0' + i)) }

// ── namingContexts check ──────────────────────────────────────────────────────

func TestCheckNamingContexts(t *testing.T) {
	cases := []struct {
		name       string
		dbs        []dbIdentity
		pods       []podAccesslogState
		wantStatus string
		wantDetail []string // substrings the detail must contain
	}{
		{
			name: "one replicated database: its log is recognised",
			dbs:  []dbIdentity{idA()},
			pods: []podAccesslogState{{
				Name:           "slapd-0",
				NamingContexts: []string{sufA, logA},
			}},
			wantStatus: "pass",
		},
		{
			name: "two replicated databases: two logs, no failure",
			dbs:  []dbIdentity{idA(), idB()},
			pods: []podAccesslogState{{
				Name:           "slapd-0",
				NamingContexts: []string{sufA, sufB, logA, logB},
			}},
			wantStatus: "pass",
		},
		{
			name: "a replicated database with no log of its own fails",
			dbs:  []dbIdentity{idA(), idB()},
			pods: []podAccesslogState{{
				Name:           "slapd-0",
				NamingContexts: []string{sufA, sufB, logA},
			}},
			wantStatus: "fail",
			wantDetail: []string{"slapd-0", "dbB", logB},
		},
		{
			name: "no data naming context at all fails",
			dbs:  []dbIdentity{idA()},
			pods: []podAccesslogState{{
				Name:           "slapd-0",
				NamingContexts: []string{logA},
			}},
			wantStatus: "fail",
			wantDetail: []string{"missing data namingContext"},
		},
		{
			name: "RO pod carrying any log naming context fails",
			dbs:  []dbIdentity{idA()},
			pods: []podAccesslogState{
				{Name: "slapd-0", NamingContexts: []string{sufA, logA}},
				{Name: "slapd-readonly-0", RO: true, NamingContexts: []string{sufA, logB}},
			},
			wantStatus: "fail",
			wantDetail: []string{"slapd-readonly-0", logB},
		},
		{
			name: "RO pod carrying the legacy shared log fails too",
			dbs:  []dbIdentity{idA()},
			pods: []podAccesslogState{
				{Name: "slapd-0", NamingContexts: []string{sufA, logA}},
				{Name: "slapd-readonly-0", RO: true, NamingContexts: []string{sufA, legacySharedAccesslogSuffix}},
			},
			wantStatus: "fail",
			wantDetail: []string{"slapd-readonly-0", legacySharedAccesslogSuffix},
		},
		{
			name: "legacy shared log mid-migration is reported as legacy, not as a failure",
			dbs:  []dbIdentity{idA()},
			pods: []podAccesslogState{{
				Name:           "slapd-0",
				NamingContexts: []string{sufA, legacySharedAccesslogSuffix, logA},
			}},
			wantStatus: "warn",
			wantDetail: []string{"legacy", legacySharedAccesslogSuffix},
		},
		{
			name: "a log with no database CR is reported as orphaned, not as a data suffix",
			dbs:  []dbIdentity{idA()},
			pods: []podAccesslogState{{
				Name:           "slapd-0",
				NamingContexts: []string{sufA, logA, logB},
			}},
			wantStatus: "warn",
			wantDetail: []string{"orphan", logB},
		},
		{
			name: "unreachable pods are skipped",
			dbs:  []dbIdentity{idA()},
			pods: []podAccesslogState{
				{Name: "slapd-0", NamingContexts: []string{sufA, logA}},
				{Name: "slapd-1", Skip: true},
			},
			wantStatus: "pass",
		},
		{
			name: "a database excluded from delta-sync is not expected to have a log",
			dbs: []dbIdentity{
				idA(),
				{Name: "dbB", Suffix: sufB, WantLog: false},
			},
			pods: []podAccesslogState{{
				Name:           "slapd-0",
				NamingContexts: []string{sufA, sufB, logA},
			}},
			wantStatus: "pass",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkNamingContexts(tc.dbs, tc.pods)
			if got.Name != "naming-contexts" {
				t.Errorf("check name = %q, want %q", got.Name, "naming-contexts")
			}
			if got.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q (detail: %s)", got.Status, tc.wantStatus, got.Detail)
			}
			for _, want := range tc.wantDetail {
				if !strings.Contains(got.Detail, want) {
					t.Errorf("detail %q does not contain %q", got.Detail, want)
				}
			}
		})
	}
}

// ── accesslog consistency check ───────────────────────────────────────────────

func TestCheckAccesslogConsistency(t *testing.T) {
	externalNeedles := []string{"ldaps://peer.example:1025"}

	externalStanza := func(suffix, logbase string) string {
		s := `{1}rid=151 provider="ldaps://peer.example:1025" searchbase="` + suffix +
			`" type=refreshAndPersist`
		if logbase != "" {
			s += ` syncdata=accesslog logbase="` + logbase + `"`
		}
		return s
	}

	cases := []struct {
		name       string
		dbs        []dbIdentity
		pods       []podAccesslogState
		wantStatus string
		wantDetail []string
	}{
		{
			name: "one replicated database: logbase, olcAccessLogDB and olcSuffix agree",
			dbs:  []dbIdentity{idA()},
			pods: []podAccesslogState{{
				Name: "slapd-0",
				DBs: []observedDB{
					dataDB(1, sufA, logA, logA),
					logDB(2, logA, ldapv1alpha1.AccesslogDir("dbA")),
				},
			}},
			wantStatus: "pass",
		},
		{
			name: "two replicated databases, one log each",
			dbs:  []dbIdentity{idA(), idB()},
			pods: []podAccesslogState{{
				Name: "slapd-0",
				DBs: []observedDB{
					dataDB(1, sufA, logA, logA),
					logDB(2, logA, ldapv1alpha1.AccesslogDir("dbA")),
					dataDB(3, sufB, logB, logB),
					logDB(4, logB, ldapv1alpha1.AccesslogDir("dbB")),
				},
			}},
			wantStatus: "pass",
		},
		{
			name: "two databases sharing one log fails (the ADR-019 condition)",
			dbs:  []dbIdentity{idA(), idB()},
			pods: []podAccesslogState{{
				Name: "slapd-0",
				DBs: []observedDB{
					dataDB(1, sufA, logA, logA),
					logDB(2, logA, ldapv1alpha1.AccesslogDir("dbA")),
					dataDB(3, sufB, logA, logA),
				},
			}},
			wantStatus: "fail",
			wantDetail: []string{"share", logA, "dbA", "dbB"},
		},
		{
			name: "logbase disagreeing with olcAccessLogDB fails",
			dbs:  []dbIdentity{idA()},
			pods: []podAccesslogState{{
				Name: "slapd-0",
				DBs: []observedDB{
					dataDB(1, sufA, logA, logB),
					logDB(2, logA, ldapv1alpha1.AccesslogDir("dbA")),
				},
			}},
			wantStatus: "fail",
			wantDetail: []string{"slapd-0", "logbase", logB},
		},
		{
			name: "olcAccessLogDB naming another database's log fails",
			dbs:  []dbIdentity{idA()},
			pods: []podAccesslogState{{
				Name: "slapd-0",
				DBs: []observedDB{
					dataDB(1, sufA, logB, logB),
					logDB(2, logA, ldapv1alpha1.AccesslogDir("dbA")),
				},
			}},
			wantStatus: "fail",
			wantDetail: []string{"olcAccessLogDB", logB},
		},
		{
			name: "a log database that is absent from cn=config fails",
			dbs:  []dbIdentity{idA()},
			pods: []podAccesslogState{{
				Name: "slapd-0",
				DBs:  []observedDB{dataDB(1, sufA, logA, logA)},
			}},
			wantStatus: "fail",
			wantDetail: []string{"slapd-0", logA, "absent"},
		},
		{
			name: "a data database with no accesslog overlay fails",
			dbs:  []dbIdentity{idA()},
			pods: []podAccesslogState{{
				Name: "slapd-0",
				DBs: []observedDB{
					dataDB(1, sufA, "", logA),
					logDB(2, logA, ldapv1alpha1.AccesslogDir("dbA")),
				},
			}},
			wantStatus: "fail",
			wantDetail: []string{"no accesslog overlay"},
		},
		{
			name: "an external-peer logbase that differs is informational, not a failure",
			dbs: []dbIdentity{
				{Name: "dbA", Suffix: sufA, WantLog: true, ExternalLogBase: "cn=log"},
			},
			pods: []podAccesslogState{{
				Name: "slapd-0",
				DBs: []observedDB{
					{
						DN:          dbDN(1),
						Suffix:      sufA,
						AccessLogDB: logA,
						SyncRepl: append(dataDB(1, sufA, logA, logA).SyncRepl,
							externalStanza(sufA, "cn=log")),
					},
					logDB(2, logA, ldapv1alpha1.AccesslogDir("dbA")),
				},
			}},
			wantStatus: "pass",
			wantDetail: []string{"cn=log", "peer"},
		},
		{
			name: "a plain-syncrepl external peer carries no logbase and is silent",
			dbs:  []dbIdentity{idA()},
			pods: []podAccesslogState{{
				Name: "slapd-0",
				DBs: []observedDB{
					{
						DN:          dbDN(1),
						Suffix:      sufA,
						AccessLogDB: logA,
						SyncRepl: append(dataDB(1, sufA, logA, logA).SyncRepl,
							externalStanza(sufA, "")),
					},
					logDB(2, logA, ldapv1alpha1.AccesslogDir("dbA")),
				},
			}},
			wantStatus: "pass",
		},
		{
			name: "an RO pod's stanza logbase must name the provider's log",
			dbs:  []dbIdentity{idA()},
			pods: []podAccesslogState{{
				Name: "slapd-readonly-0",
				RO:   true,
				DBs:  []observedDB{dataDB(1, sufA, "", logB)},
			}},
			wantStatus: "fail",
			wantDetail: []string{"slapd-readonly-0", "logbase", logB},
		},
		{
			name: "an RO pod with a correct stanza logbase and no log of its own passes",
			dbs:  []dbIdentity{idA()},
			pods: []podAccesslogState{{
				Name: "slapd-readonly-0",
				RO:   true,
				DBs:  []observedDB{dataDB(1, sufA, "", logA)},
			}},
			wantStatus: "pass",
		},
		{
			name: "a single database still on the legacy shared log is reported as legacy",
			dbs:  []dbIdentity{idA()},
			pods: []podAccesslogState{{
				Name: "slapd-0",
				DBs: []observedDB{
					dataDB(1, sufA, legacySharedAccesslogSuffix, legacySharedAccesslogSuffix),
					logDB(2, legacySharedAccesslogSuffix, ldapv1alpha1.AccesslogRoot),
				},
			}},
			wantStatus: "warn",
			wantDetail: []string{"legacy", legacySharedAccesslogSuffix},
		},
		{
			// The 2026-09-14 defect (ADR-026): the overlay names the legacy
			// shared log, but that database is GONE from this pod's cn=config.
			// slapd resolves logdb offline at accesslog_db_open, so this pod
			// serves fine until its next restart and then exits with
			// `accesslog: "logdb <suffix>" missing or invalid`. It must be an
			// issue, not a "legacy" note: the pod is already unbootable.
			name: "an olcAccessLogDB naming a database absent from the pod fails",
			dbs:  []dbIdentity{idA()},
			pods: []podAccesslogState{{
				Name: "slapd-0",
				DBs: []observedDB{
					dataDB(1, sufA, legacySharedAccesslogSuffix, logA),
					logDB(2, logA, ldapv1alpha1.AccesslogDir("dbA")),
				},
			}},
			wantStatus: "fail",
			wantDetail: []string{"slapd-0", legacySharedAccesslogSuffix, "not a database"},
		},
		{
			name: "two databases still sharing the legacy log is the forbidden condition and fails",
			dbs:  []dbIdentity{idA(), idB()},
			pods: []podAccesslogState{{
				Name: "slapd-0",
				DBs: []observedDB{
					dataDB(1, sufA, legacySharedAccesslogSuffix, legacySharedAccesslogSuffix),
					dataDB(3, sufB, legacySharedAccesslogSuffix, legacySharedAccesslogSuffix),
					logDB(2, legacySharedAccesslogSuffix, ldapv1alpha1.AccesslogRoot),
				},
			}},
			wantStatus: "fail",
			wantDetail: []string{"share", "dbA", "dbB"},
		},
		{
			name: "a database named default is not treated as the legacy shared log",
			dbs: []dbIdentity{{
				Name: "default", Suffix: sufA, WantLog: true,
				ExternalLogBase: ldapv1alpha1.AccesslogSuffix("default"),
			}},
			pods: []podAccesslogState{{
				Name: "slapd-0",
				DBs: []observedDB{
					dataDB(1, sufA,
						ldapv1alpha1.AccesslogSuffix("default"),
						ldapv1alpha1.AccesslogSuffix("default")),
					logDB(2, ldapv1alpha1.AccesslogSuffix("default"),
						ldapv1alpha1.AccesslogDir("default")),
				},
			}},
			wantStatus: "pass",
		},
		{
			name: "unreachable pods are skipped",
			dbs:  []dbIdentity{idA()},
			pods: []podAccesslogState{
				{
					Name: "slapd-0",
					DBs: []observedDB{
						dataDB(1, sufA, logA, logA),
						logDB(2, logA, ldapv1alpha1.AccesslogDir("dbA")),
					},
				},
				{Name: "slapd-1", Skip: true},
			},
			wantStatus: "pass",
		},
		{
			name: "a database excluded from delta-sync is not checked",
			dbs: []dbIdentity{
				{Name: "dbB", Suffix: sufB, WantLog: false},
			},
			pods: []podAccesslogState{{
				Name: "slapd-0",
				DBs:  []observedDB{{DN: dbDN(1), Suffix: sufB}},
			}},
			wantStatus: "pass",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkAccesslogConsistency(tc.dbs, tc.pods, externalNeedles)
			if got.Name != "accesslog-consistency" {
				t.Errorf("check name = %q, want %q", got.Name, "accesslog-consistency")
			}
			if got.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q (detail: %s)", got.Status, tc.wantStatus, got.Detail)
			}
			for _, want := range tc.wantDetail {
				if !strings.Contains(got.Detail, want) {
					t.Errorf("detail %q does not contain %q", got.Detail, want)
				}
			}
		})
	}
}

// ── logbase parsing / stanza classification ───────────────────────────────────

func TestStanzaLogbase(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"quoted", `rid=101 logbase="` + logA + `" logfilter="(x=y)"`, logA},
		{"unquoted", `rid=101 logbase=cn=log type=refreshAndPersist`, "cn=log"},
		{"absent", `rid=101 provider="ldap://x" type=refreshOnly`, ""},
		{"empty", ``, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stanzaLogbase(tc.in); got != tc.want {
				t.Errorf("stanzaLogbase(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestStanzaIsExternal(t *testing.T) {
	needles := []string{"ldaps://peer.example:1025", "10.0.0.7"}
	if !stanzaIsExternal(`rid=151 provider="ldaps://peer.example:1025"`, needles) {
		t.Error("URI peer stanza not classified as external")
	}
	if !stanzaIsExternal(`rid=152 provider="ldaps://10.0.0.7:1025"`, needles) {
		t.Error("pod-address peer stanza not classified as external")
	}
	if stanzaIsExternal(`rid=101 provider="ldaps://slapd-1.slapd-headless.ns.svc:1025"`, needles) {
		t.Error("in-cluster stanza classified as external")
	}
	if stanzaIsExternal(`rid=101 provider="ldaps://slapd-1"`, nil) {
		t.Error("no needles: nothing is external")
	}
}
