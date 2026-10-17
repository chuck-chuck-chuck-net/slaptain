/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package cmd

import (
	"strings"
	"testing"
)

// The syncrepl checks judge PER DATABASE — the same axis correction ADR-008's
// 2026-09-14 amendment made for contextCSN, applied to the checks that were
// left behind.
//
// Every one of these fixtures is a two-database cluster whose SECOND database
// is broken. Reading only the first data DB — what the checks did until now —
// passes all of them.

func readings(pods ...syncreplPodReading) []syncreplPodReading { return pods }

func rw(dbs map[string]syncreplDBReading) syncreplPodReading {
	return syncreplPodReading{Pod: testPod0, BySuffix: dbs}
}

func ro(name string, dbs map[string]syncreplDBReading) syncreplPodReading {
	return syncreplPodReading{Pod: name, RO: true, BySuffix: dbs}
}

func stanzas(s ...string) syncreplDBReading { return syncreplDBReading{Stanzas: s} }

var twoDBs = []dbIdentity{
	{Name: "db1", Suffix: "dc=one,dc=test", Replicated: true},
	{Name: "db2", Suffix: "dc=two,dc=test", Replicated: true},
}

func TestCheckSyncreplStanzaCount(t *testing.T) {
	good := map[string]syncreplDBReading{
		"dc=one,dc=test": stanzas("rid=101 provider=ldaps://slapd-1...", "rid=102 provider=ldaps://slapd-2..."),
		"dc=two,dc=test": stanzas("rid=201 provider=ldaps://slapd-1...", "rid=202 provider=ldaps://slapd-2..."),
	}
	if got := checkSyncreplStanzaCount(twoDBs, readings(rw(good)), 2, 3); got.Status != "pass" {
		t.Errorf("healthy two-database pod: %s — %s", got.Status, got.Detail)
	}

	// The whole point: db2 short by one, db1 perfect.
	short := map[string]syncreplDBReading{
		"dc=one,dc=test": good["dc=one,dc=test"],
		"dc=two,dc=test": stanzas("rid=201 provider=ldaps://slapd-1..."),
	}
	got := checkSyncreplStanzaCount(twoDBs, readings(rw(short)), 2, 3)
	if got.Status != "fail" {
		t.Fatalf("a database short one stanza must fail, got %s — %s", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "dc=two,dc=test") {
		t.Errorf("the failure must name the database it indicts: %q", got.Detail)
	}

	// A database with NO stanzas at all is the severe case, and the one a
	// first-database-only check is most likely to miss entirely.
	none := map[string]syncreplDBReading{"dc=one,dc=test": good["dc=one,dc=test"]}
	if got := checkSyncreplStanzaCount(twoDBs, readings(rw(none)), 2, 3); got.Status != "fail" {
		t.Errorf("a database with no stanzas must fail, got %s — %s", got.Status, got.Detail)
	}

	// A non-replicated database legitimately carries none, and must not be
	// judged against the replicated expectation.
	mixed := []dbIdentity{
		{Name: "db1", Suffix: "dc=one,dc=test", Replicated: true},
		{Name: "plain", Suffix: "dc=plain,dc=test", Replicated: false},
	}
	plain := map[string]syncreplDBReading{
		"dc=one,dc=test":   good["dc=one,dc=test"],
		"dc=plain,dc=test": stanzas(),
	}
	if got := checkSyncreplStanzaCount(mixed, readings(rw(plain)), 2, 3); got.Status != "pass" {
		t.Errorf("a non-replicated database must not be held to the stanza count: %s — %s", got.Status, got.Detail)
	}

	// RO pods consume from every RW pod, including for the second database.
	roShort := map[string]syncreplDBReading{
		"dc=one,dc=test": stanzas("a", "b", "c"),
		"dc=two,dc=test": stanzas("a", "b"),
	}
	if got := checkSyncreplStanzaCount(twoDBs, readings(ro("slapd-readonly-0", roShort)), 2, 3); got.Status != "fail" {
		t.Errorf("an RO pod short a stanza on db2 must fail, got %s — %s", got.Status, got.Detail)
	}
}

func TestCheckSyncreplSkipSelf(t *testing.T) {
	ok := map[string]syncreplDBReading{
		"dc=one,dc=test": stanzas("rid=101 provider=ldaps://slapd-1.slapd-headless.ns.svc:1025"),
		"dc=two,dc=test": stanzas("rid=201 provider=ldaps://slapd-1.slapd-headless.ns.svc:1025"),
	}
	if got := checkSyncreplSkipSelf(readings(rw(ok)), "slapd-headless"); got.Status != "pass" {
		t.Errorf("no self-reference must pass: %s — %s", got.Status, got.Detail)
	}

	// Self-reference on the SECOND database only.
	self := map[string]syncreplDBReading{
		"dc=one,dc=test": ok["dc=one,dc=test"],
		"dc=two,dc=test": stanzas("rid=201 provider=ldaps://slapd-0.slapd-headless.ns.svc:1025"),
	}
	got := checkSyncreplSkipSelf(readings(rw(self)), "slapd-headless")
	if got.Status != "fail" {
		t.Fatalf("a pod replicating from itself must fail, got %s — %s", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "dc=two,dc=test") {
		t.Errorf("the failure must name the database: %q", got.Detail)
	}
}

// RID uniqueness is the check with the sharpest blind spot: its entire purpose
// is a COLLISION BETWEEN databases (ADR-003 gives each database its own
// ridBase precisely so this cannot happen), and reading one database made it
// structurally incapable of seeing one.
func TestCheckRIDUniqueness(t *testing.T) {
	ok := map[string]syncreplDBReading{
		"dc=one,dc=test": stanzas("rid=101 provider=x", "rid=102 provider=y"),
		"dc=two,dc=test": stanzas("rid=201 provider=x", "rid=202 provider=y"),
	}
	if got := checkRIDUniqueness(readings(rw(ok))); got.Status != "pass" {
		t.Errorf("distinct ridBases must pass: %s — %s", got.Status, got.Detail)
	}

	// Two databases sharing a ridBase — the misconfiguration ADR-003 warns
	// about and docs/BACKLOG.md notes is documented but not enforced.
	clash := map[string]syncreplDBReading{
		"dc=one,dc=test": stanzas("rid=101 provider=x", "rid=102 provider=y"),
		"dc=two,dc=test": stanzas("rid=101 provider=x", "rid=102 provider=y"),
	}
	got := checkRIDUniqueness(readings(rw(clash)))
	if got.Status != "fail" {
		t.Fatalf("a RID reused across databases must fail, got %s — %s", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "101") {
		t.Errorf("the failure must name the colliding RID: %q", got.Detail)
	}

	// A duplicate WITHIN one database still fails, as before.
	dup := map[string]syncreplDBReading{
		"dc=one,dc=test": stanzas("rid=101 provider=x", "rid=101 provider=y"),
	}
	if got := checkRIDUniqueness(readings(rw(dup))); got.Status != "fail" {
		t.Errorf("a duplicate RID within one database must fail, got %s", got.Status)
	}
}

func TestCheckMultiProvider(t *testing.T) {
	both := map[string]syncreplDBReading{
		"dc=one,dc=test": {Stanzas: []string{"x"}, MultiProvider: "TRUE"},
		"dc=two,dc=test": {Stanzas: []string{"x"}, MultiProvider: "TRUE"},
	}
	if got := checkMultiProviderPerDB(twoDBs, readings(rw(both))); got.Status != "pass" {
		t.Errorf("TRUE on both databases must pass: %s — %s", got.Status, got.Detail)
	}

	// Missing on the second database: that database silently rejects writes.
	half := map[string]syncreplDBReading{
		"dc=one,dc=test": both["dc=one,dc=test"],
		"dc=two,dc=test": {Stanzas: []string{"x"}},
	}
	got := checkMultiProviderPerDB(twoDBs, readings(rw(half)))
	if got.Status != "fail" {
		t.Fatalf("multiProvider missing on a database must fail, got %s — %s", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "dc=two,dc=test") {
		t.Errorf("the failure must name the database: %q", got.Detail)
	}

	// An RO pod must carry it on no database.
	roBad := map[string]syncreplDBReading{
		"dc=one,dc=test": {MultiProvider: ""},
		"dc=two,dc=test": {MultiProvider: "TRUE"},
	}
	if got := checkMultiProviderPerDB(twoDBs, readings(ro("slapd-readonly-0", roBad))); got.Status != "fail" {
		t.Errorf("an RO pod with multiProvider on a database must fail, got %s — %s", got.Status, got.Detail)
	}
}

// A pod whose cn=config could not be read produces NO verdict about cn=config.
//
// Observed live 2026-09-28, walking docs/MULTI-SITE.md: a reinstall over
// surviving PVCs left the operator's freshly generated config password out of
// step with the olcRootPW already on disk, so every bind failed with err=49.
// slctl read zero syncrepl stanzas from zero readable pods and reported
// "0 stanzas (want 4)" eight times over — a confident, detailed, wrong claim
// about the cluster's configuration, which sent the reader to check their
// values file while the actual cause sat three lines above in the same output.
//
// Unreadable evidence must never become a verdict. ADR-008's 2026-09-14
// amendment established that for contextCSN ("unreadable evidence never counts
// toward True"); this is the same rule in the other direction, which is worse,
// because a False invents a defect that does not exist.
func TestChecksDoNotJudgeUnreadablePods(t *testing.T) {
	none := readings() // every pod's cn=config was unreadable, so nothing to judge

	for _, got := range []checkResult{
		checkSyncreplStanzaCount(twoDBs, none, 2, 3),
		checkSyncreplSkipSelf(none, "slapd-headless"),
		checkMultiProviderPerDB(twoDBs, none),
		checkRIDUniqueness(none),
	} {
		if got.Status == "fail" {
			t.Errorf("%s FAILED on no evidence: %s", got.Name, got.Detail)
		}
		if got.Status == "pass" {
			t.Errorf("%s PASSED on no evidence — a vacuous green is how a broken cluster reports clean: %s",
				got.Name, got.Detail)
		}
		if !strings.Contains(strings.ToLower(got.Detail), "not evaluated") {
			t.Errorf("%s does not say it was not evaluated, so the reader cannot tell "+
				"a verdict from an absence: %q", got.Name, got.Detail)
		}
	}
}

// And the cause itself has to be a check, not a line of prose above the checks.
func TestCheckConfigReadable(t *testing.T) {
	ok := checkConfigReadable(nil)
	if ok.Status != "pass" {
		t.Errorf("no config errors must pass, got %s — %s", ok.Status, ok.Detail)
	}

	bad := checkConfigReadable([]configReadFailure{
		{Pod: "slapd-0", Err: `cn=config bind failed: LDAP Result Code 49 "Invalid Credentials"`},
		{Pod: "slapd-1", Err: `cn=config bind failed: LDAP Result Code 49 "Invalid Credentials"`},
	})
	if bad.Status != "fail" {
		t.Fatalf("unreadable cn=config must fail, got %s", bad.Status)
	}
	for _, want := range []string{"slapd-0", "slapd-1", "49"} {
		if !strings.Contains(bad.Detail, want) {
			t.Errorf("detail %q does not mention %q", bad.Detail, want)
		}
	}
}
