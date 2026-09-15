/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"reflect"
	"sort"
	"testing"
)

// ADR-027 decision 5: the auth database's ACLs are minimal — it exists to
// answer binds and nothing else.
//
//	to attrs=userPassword by anonymous auth by * none
//	to * by * none
//
// Pinned literally, the way accesslogACL's rule is (ADR-020): the exact string
// is the security property, not an implementation detail. Two things matter and
// each has its own assertion below: `auth` (not `read`) on userPassword, so a
// successful bind never implies the ability to READ the hash; and a catch-all
// `by * none`, so the frontend's default of `read` cannot leak the rest.
func TestAuthDBACLs(t *testing.T) {
	want := []string{
		`to attrs=userPassword by anonymous auth by * none`,
		`to * by * none`,
	}
	got := authDBACLs()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("authDBACLs() =\n  %q\nwant\n  %q", got, want)
	}
}

func TestAuthDBACLsOrderIsLoadBearing(t *testing.T) {
	got := authDBACLs()
	if len(got) < 2 {
		t.Fatalf("authDBACLs() = %q, want at least the userPassword rule and a catch-all", got)
	}
	// olcAccess is evaluated in order and the first matching `to` clause wins.
	// A catch-all `to *` placed first would shadow the userPassword rule and
	// make every bind fail — the database would be inert in the wrong direction.
	if got[len(got)-1] != `to * by * none` {
		t.Errorf("the catch-all must be LAST, got %q as the final rule", got[len(got)-1])
	}
	for i, rule := range got[:len(got)-1] {
		if rule == `to * by * none` {
			t.Errorf("catch-all found at index %d, shadowing %d later rule(s)", i, len(got)-1-i)
		}
	}
}

// ADR-027 decision 2: one identity per SlapdDatabase, holding exactly the
// attributes a simple bind needs. The objectClass pair is the same one
// ensureReplicationUser uses for cn=replication,<suffix> — simpleSecurityObject
// carries userPassword, organizationalRole carries cn — so the entry slapd
// verifies against is structurally identical to the one it replaces; only its
// placement changed.
func TestDesiredAuthIdentityAttrs(t *testing.T) {
	got := desiredAuthIdentityAttrs("example-db", "{SSHA}deadbeef")

	want := map[string][]string{
		"objectClass":  {"simpleSecurityObject", "organizationalRole"},
		"cn":           {"repl-example-db"},
		"userPassword": {"{SSHA}deadbeef"},
		"description":  {"Syncrepl bind identity for SlapdDatabase example-db (ADR-027)"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("desiredAuthIdentityAttrs() =\n  %#v\nwant\n  %#v", got, want)
	}

	// The cn value must be the RDN of AuthIdentityDN, or slapd rejects the add
	// with "value of naming attribute not present in entry".
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(got["cn"]) != 1 || got["cn"][0] != "repl-example-db" {
		t.Errorf("cn must equal the DN's RDN value, got %q (attrs: %v)", got["cn"], keys)
	}
}

// ADR-027 decision 6: the entry is CONVERGED to the Secret, not created once.
// Convergence needs a comparison, and an SSHA hash is salted — so two hashes of
// the same password never compare equal. sshaMatches re-hashes the candidate
// password with the SALT recovered from the stored value, which is exactly what
// slapd does when it verifies a bind.
//
// Getting this wrong in either direction is a live defect: too eager and the
// operator rewrites userPassword on every reconcile (a write storm on every pod
// of every cluster); too lax and a rotated Secret never reaches the directory,
// which is the failure ADR-027 exists to remove.
func TestSSHAMatches(t *testing.T) {
	// Fixtures with known salts, in slappasswd's exact encoding:
	// base64(sha1(password||salt) || salt), 8-byte salt.
	const (
		hashOfReplpass = "{SSHA}o0WYxcuJec7SLIU+7Xb7XrpKvUIBAgMEBQYHCA=="
		hashOfOther    = "{SSHA}DVSav6fnvF4j9XdL5FJVzMPRal1hYmNkZWZnaA=="
	)

	cases := []struct {
		name     string
		stored   string
		password string
		want     bool
	}{
		{"matching password", hashOfReplpass, "replpass123", true},
		{"rotated password", hashOfReplpass, "replpass124", false},
		{"different fixture, its own password", hashOfOther, "other", true},
		{"different fixture, wrong password", hashOfOther, "replpass123", false},
		{"empty stored value (attribute absent)", "", "replpass123", false},
		// The password-stripped copy measured live (ADR-027 Context): an entry
		// whose userPassword arrived empty satisfies an existence check forever.
		// It must NOT satisfy a convergence check.
		{"stripped userPassword", "{SSHA}", "replpass123", false},
		// Not our scheme: a cleartext or {CRYPT} value must be reported as a
		// mismatch so the operator replaces it, never silently accepted.
		{"cleartext value", "replpass123", "replpass123", false},
		{"foreign scheme", "{CRYPT}abcdefg", "replpass123", false},
		{"truncated base64", "{SSHA}!!!not-base64!!!", "replpass123", false},
		// Shorter than one SHA-1 digest: there is no salt to recover, so the
		// value cannot be verified and must not be trusted.
		{"too short to carry a salt", "{SSHA}YWJj", "replpass123", false},
	}

	for _, c := range cases {
		if got := sshaMatches(c.stored, c.password); got != c.want {
			t.Errorf("%s: sshaMatches(%q, %q) = %v, want %v",
				c.name, c.stored, c.password, got, c.want)
		}
	}
}

// Round-trip against the hash generator the operator actually writes with —
// the two halves of convergence must agree, or every reconcile rewrites.
func TestSSHAMatchesAgreesWithGenerateSSHAHash(t *testing.T) {
	for _, pw := range []string{"replpass123", "", "ünïcödé-påss", "a"} {
		h, err := generateSSHAHash(pw)
		if err != nil {
			t.Fatalf("generateSSHAHash(%q): %v", pw, err)
		}
		if !sshaMatches(h, pw) {
			t.Errorf("sshaMatches(generateSSHAHash(%q), %q) = false, want true", pw, pw)
		}
		if sshaMatches(h, pw+"x") {
			t.Errorf("sshaMatches accepted a wrong password for %q", pw)
		}
	}
}
