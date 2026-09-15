/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package v1alpha1

import (
	"regexp"
	"strings"
	"testing"
)

// ADR-027 decisions 1-3: the names of the node-local authentication database.
// Same shape as accesslog_test.go — these strings are the observable contract
// (slctl reads them back off a live pod), so they are pinned literally rather
// than re-derived from the same helper they test.

func TestAuthSuffixAndDir(t *testing.T) {
	if got, want := AuthSuffix, "cn=slaptain-auth"; got != want {
		t.Errorf("AuthSuffix = %q, want %q", got, want)
	}
	if got, want := AuthDir, "/data/_slaptain-auth"; got != want {
		t.Errorf("AuthDir = %q, want %q", got, want)
	}
}

func TestAuthIdentityDN(t *testing.T) {
	cases := []struct {
		dbName string
		want   string
	}{
		{"example-db", "cn=repl-example-db,cn=slaptain-auth"},
		{"default", "cn=repl-default,cn=slaptain-auth"},
		{"example-db2", "cn=repl-example-db2,cn=slaptain-auth"},
	}
	for _, c := range cases {
		if got := AuthIdentityDN(c.dbName); got != c.want {
			t.Errorf("AuthIdentityDN(%q) = %q, want %q", c.dbName, got, c.want)
		}
	}
}

// The identity DN must be a child of the auth suffix — the ACLs in
// internal/controller are written against the suffix, so an identity that fell
// outside it would be governed by nothing.
func TestAuthIdentityDNIsUnderAuthSuffix(t *testing.T) {
	if !strings.HasSuffix(AuthIdentityDN("example-db"), ","+AuthSuffix) {
		t.Errorf("AuthIdentityDN must live under %q, got %q",
			AuthSuffix, AuthIdentityDN("example-db"))
	}
}

// ADR-027 decision 3: the reserved directory name cannot collide with a user
// database's /data/<dbname>, and the guarantee is STRUCTURAL, not a naming
// convention: DATABASE_DIRS entries are SlapdDatabase CR names, and a
// Kubernetes object name is an RFC 1123 subdomain — it must start with a
// lowercase alphanumeric, so no CR can ever be called "_slaptain-auth".
//
// This test pins the property the ADR rests on. If AuthDir's base name were
// ever changed to something a CR name could match, the auth database and a
// user database would share an LMDB environment.
func TestAuthDirBaseNameIsUnreachableByAnyCRName(t *testing.T) {
	base := AuthDir[strings.LastIndex(AuthDir, "/")+1:]

	// RFC 1123 subdomain, as enforced by apimachinery for object names.
	rfc1123 := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	if rfc1123.MatchString(base) {
		t.Errorf("AuthDir base name %q is a legal Kubernetes object name — a "+
			"SlapdDatabase of that name would collide with the auth database", base)
	}
	if !strings.HasPrefix(base, "_") {
		t.Errorf("AuthDir base name %q must start with '_' — that leading "+
			"underscore is what RFC 1123 forbids and therefore what makes the "+
			"reservation collision-proof", base)
	}
}
