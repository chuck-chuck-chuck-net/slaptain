/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"strings"
	"testing"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// ADR-027 milestone 2: the cutover. Everything that authenticates on the
// replication path — syncrepl stanzas, the ACL grants and olcLimits that let
// the bound identity actually read, and the operator's CSN monitoring — names
// the node-local identity cn=repl-<db>,cn=slaptain-auth instead of the
// replicated-tree entry cn=replication,<suffix>.
//
// The contract these tests pin is ADDITIVE, which is the whole reason the
// migration is safe on a mesh whose sites upgrade one at a time: the legacy
// identity keeps every grant it had. A provider that has cut over still
// accepts a consumer that has not. Several tests below exist purely to fail if
// someone later "tidies away" a legacy grant — that removal is migration step 4
// and a separate release (ADR-027 Consequences).

func TestReplicationBindDN(t *testing.T) {
	for _, c := range []struct{ dbName, want string }{
		{"example-db", "cn=repl-example-db,cn=slaptain-auth"},
		{"default", "cn=repl-default,cn=slaptain-auth"},
	} {
		if got := replicationBindDN(c.dbName); got != c.want {
			t.Errorf("replicationBindDN(%q) = %q, want %q", c.dbName, got, c.want)
		}
	}
}

// Drift guard: the controller's notion of the bind identity must be the API
// package's, not a parallel spelling that can wander away from it.
func TestReplicationBindDNMatchesAPI(t *testing.T) {
	if got, want := replicationBindDN("example-db"), ldapv1alpha1.AuthIdentityDN("example-db"); got != want {
		t.Errorf("replicationBindDN = %q, API AuthIdentityDN = %q", got, want)
	}
}

func TestLegacyReplicationBindDN(t *testing.T) {
	if got, want := legacyReplicationBindDN("dc=example,dc=org"),
		"cn=replication,dc=example,dc=org"; got != want {
		t.Errorf("legacyReplicationBindDN = %q, want %q", got, want)
	}
}

// The data database's operator-owned grant. Both identities read; the rule ends
// in `by * break` so the user's own spec.acls still see the request.
func TestReplicationACLGrantsBothIdentities(t *testing.T) {
	const (
		dbName = "example-db"
		suffix = "dc=example,dc=org"
	)
	got := replicationACL(dbName, suffix)

	newDN := `dn.exact="cn=repl-example-db,cn=slaptain-auth" read`
	oldDN := `dn.exact="cn=replication,dc=example,dc=org" read`

	if !strings.Contains(got, newDN) {
		t.Errorf("data ACL must grant the node-local identity (ADR-027):\n  %q", got)
	}
	// Additivity. If this fails, the legacy grant was removed — that is
	// migration step 4, not this milestone, and it breaks every not-yet-upgraded
	// consumer in the mesh.
	if !strings.Contains(got, oldDN) {
		t.Errorf("data ACL must STILL grant the legacy identity (ADR-027 additive migration):\n  %q", got)
	}
	if strings.Index(got, newDN) > strings.Index(got, oldDN) {
		t.Errorf("the node-local identity should be named first:\n  %q", got)
	}
	if !strings.HasPrefix(got, "to * by ") {
		t.Errorf("data ACL must apply to all attributes: %q", got)
	}
	if !strings.HasSuffix(got, "by * break") {
		t.Errorf("data ACL must fall through to the user's ACLs (`by * break`): %q", got)
	}
}

// The journal's grant (ADR-020). Same two identities; the rule ends in
// `by * none`, because nothing else may read a change journal at all.
func TestAccesslogACLGrantsBothIdentities(t *testing.T) {
	got := accesslogACL("example-db", "dc=ex,dc=com")

	if !strings.Contains(got, `dn.exact="cn=repl-example-db,cn=slaptain-auth" read`) {
		t.Errorf("accesslog ACL must grant the node-local identity (ADR-020 as amended by ADR-027):\n  %q", got)
	}
	if !strings.Contains(got, `dn.exact="cn=replication,dc=ex,dc=com" read`) {
		t.Errorf("accesslog ACL must STILL grant the legacy identity (additive migration):\n  %q", got)
	}
	if !strings.HasSuffix(got, "by * none") {
		t.Errorf("accesslog ACL must deny everyone else outright: %q", got)
	}
}

// ADR-020's 2026-09-12 amendment: the ACL says WHO may read, olcLimits says HOW
// MUCH. slapd's default sizelimit of 500 silently capped every syncrepl search
// until that was found live, so an identity that is granted read without a
// matching limits exemption reproduces a defect this project already paid for.
// Limits select on the bind DN, so the new identity needs its own value.
func TestReplicationLimitsCoverBothIdentities(t *testing.T) {
	got := replicationLimits("example-db", "dc=example,dc=org")

	if len(got) != 2 {
		t.Fatalf("expected one olcLimits value per identity, got %d: %q", len(got), got)
	}
	wantNew := `dn.exact="cn=repl-example-db,cn=slaptain-auth" ` +
		`time.soft=unlimited time.hard=unlimited size.soft=unlimited size.hard=unlimited`
	wantOld := `dn.exact="cn=replication,dc=example,dc=org" ` +
		`time.soft=unlimited time.hard=unlimited size.soft=unlimited size.hard=unlimited`
	if got[0] != wantNew {
		t.Errorf("first limits value =\n  %q\nwant\n  %q", got[0], wantNew)
	}
	if got[1] != wantOld {
		t.Errorf("second limits value (legacy, additive) =\n  %q\nwant\n  %q", got[1], wantOld)
	}
}

// The operator's CSN monitoring binds to read contextCSN (ADR-008). It prefers
// the node-local identity, and keeps the legacy one as a fallback so a
// cross-site peer that has not upgraded yet still reports its lag instead of
// going Unreachable for the length of the migration window.
func TestCSNBindDNsPreferNodeLocal(t *testing.T) {
	got := csnBindDNs("example-db", "dc=example,dc=org")
	want := []string{
		"cn=repl-example-db,cn=slaptain-auth",
		"cn=replication,dc=example,dc=org",
	}
	if len(got) != len(want) {
		t.Fatalf("csnBindDNs = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("csnBindDNs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
