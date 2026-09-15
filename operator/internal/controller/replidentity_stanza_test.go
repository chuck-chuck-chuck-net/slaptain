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

// Every syncrepl stanza the operator writes binds as the node-local identity
// (ADR-027 migration step 3). Three builders produce them — in-cluster RW
// peers, external peers, and the read-only fleet — and all three named the
// legacy DN, two by hardcoding it and one by defaulting to it.
func TestSyncReplStanzasBindAsNodeLocalIdentity(t *testing.T) {
	const (
		cluster  = "slapd"
		headless = "slapd-headless"
		ns       = "slaptain-testing"
		domain   = "cluster.local"
		suffix   = "dc=example,dc=org"
		dbName   = "example-db"
	)
	wantBind := `binddn="cn=repl-example-db,cn=slaptain-auth"`

	extDN, _ := externalBindIdentity("", "", "", suffix, dbName, "pw")
	rw := buildDatabaseSyncRepl(
		cluster, headless, ns, domain, suffix, dbName,
		3, 0, "pw", false, 100, "60 +", "", true,
		ldapv1alpha1.AccesslogSuffix(dbName),
		[]resolvedExternalPeer{{
			Name: "site-b", URIs: []string{"ldap://10.0.0.9:1024"},
			ReplicasPerPeer: 1, BindDN: extDN, Password: "pw",
		}},
		nil, false,
	)
	ro := buildDatabaseSyncReplRO(
		cluster, headless, ns, domain, suffix, dbName,
		3, "pw", false, 200, "60 +", "", true, nil,
	)

	if len(rw) != 3 {
		t.Fatalf("expected 2 in-cluster + 1 external stanza, got %d: %q", len(rw), rw)
	}
	for _, group := range []struct {
		what    string
		stanzas []string
	}{{"RW", rw}, {"RO", ro}} {
		for i, s := range group.stanzas {
			if !strings.Contains(s, wantBind) {
				t.Errorf("%s stanza %d does not bind as the node-local identity (ADR-027):\n  %s",
					group.what, i, s)
			}
			// The plaintext credential source is unchanged — that half of the
			// two-store problem is inherent to simple bind (ADR-027 Consequences).
			if !strings.Contains(s, "credentials=pw") {
				t.Errorf("%s stanza %d lost its credentials=: %s", group.what, i, s)
			}
		}
	}
}

// An explicit externalPeers[].bindDN still wins verbatim — the ADR-011 escape
// for a foreign provider with its own bind account, and during ADR-027's
// migration window also how an operator pins a cross-site stanza back to the
// legacy identity until the remote site upgrades.
func TestExternalBindIdentityOverrideStillWins(t *testing.T) {
	got, _ := externalBindIdentity("cn=syncuser,dc=legacy,dc=net", "", "", "dc=example,dc=org", "example-db", "pw")
	if got != "cn=syncuser,dc=legacy,dc=net" {
		t.Errorf("explicit bindDN must win verbatim, got %q", got)
	}
}

// ── The ordering gate (ADR-027 amendment, deviation 3 coming due) ───────────
//
// While nothing bound as the node-local identity, failing to write it was
// harmless and the step was best-effort. Once a stanza names it, a provider
// that lacks the entry rejects every consumer binding as it (err=49) and that
// link is dead until the entry appears. So the entry must exist on every pod
// acting as a PROVIDER before any consumer is told to bind as it. In an
// in-cluster mesh every RW pod is both at once, which collapses the rule to:
// write no stanzas at all until every RW pod carries the entry.
func TestAuthIdentityReadyForStanzas(t *testing.T) {
	cases := []struct {
		name         string
		rwIdentityOK bool
		roIdentityOK bool
		want         bool
	}{
		{"every provider carries the identity", true, true, true},
		// The RO fleet is never a provider — nothing binds to it — so an RO pod
		// the operator could not reach must not withhold the mesh's stanzas.
		// Without this case the gate deadlocks a cluster on a broken RO replica.
		{"a read-only pod is behind", true, false, true},
		// One RW pod short is enough: every other pod's stanzas point at it.
		{"an RW provider is missing the identity", false, true, false},
		{"nothing converged", false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := authIdentityReadyForStanzas(authIdentityState{
				AllRWPodsConverged: tc.rwIdentityOK,
				AllROPodsConverged: tc.roIdentityOK,
			})
			if got != tc.want {
				t.Errorf("authIdentityReadyForStanzas = %v, want %v", got, tc.want)
			}
		})
	}
}
