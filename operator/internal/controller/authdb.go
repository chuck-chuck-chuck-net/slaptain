/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"crypto/sha1" // #nosec G505 -- {SSHA} is OpenLDAP's on-disk password scheme; the algorithm is dictated by what slapd verifies against, not chosen here.
	"crypto/subtle"
	"encoding/base64"
	"strings"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The decisions behind the node-local authentication database (ADR-027), kept
// pure and I/O-free: what the database's ACLs must be, what one identity entry
// must contain, and whether a stored password hash already matches the Secret.
// The LDAP traffic that applies them lives in slapdcluster_authdb.go (the
// database, owned by the SlapdCluster controller) and
// slapddatabase_authdb.go (the entries, owned by the SlapdDatabase
// controller) — ADR-027 decision 7.

// authDBACLs returns the olcAccess rules the auth database carries (ADR-027
// decision 5). It exists to answer binds, and the ACLs say exactly that and
// nothing more.
//
//	to attrs=userPassword by anonymous auth by * none
//	to * by * none
//
// `auth` rather than `read` on the first rule: slapd needs the auth right to
// verify a simple bind, and granting only that means a successful bind never
// implies the ability to read the hash back out. The catch-all is not
// redundant — a database with no matching rule falls through to the frontend
// default, which is *read*. That is the same trap ADR-020 documents for an
// accesslog database with no olcAccess, and this database holds every
// replication credential on the pod.
//
// No rootDN grant (ADR-020 R3's reasoning, unchanged): olcRootDN is
// cn=admin,cn=config and a rootDN bypasses ACLs, so the operator's own writes
// are unaffected. An ACL line restating a bypass reads later as a requirement.
//
// Order is load-bearing: olcAccess is evaluated in order and the first matching
// `to` clause wins, so the catch-all must come last or it shadows everything.
func authDBACLs() []string {
	return []string{
		`to attrs=userPassword by anonymous auth by * none`,
		`to * by * none`,
	}
}

// desiredAuthIdentityAttrs returns the complete attribute set of one
// SlapdDatabase's replication identity inside the auth database (ADR-027
// decision 2), given the SSHA hash of the password from that database's
// <dbname>-credentials Secret.
//
// The objectClass pair is the one ensureReplicationUser already uses for
// cn=replication,<suffix>: simpleSecurityObject carries userPassword,
// organizationalRole carries cn. The entry slapd verifies a bind against is
// structurally identical to the one it will eventually replace — only its
// placement changed, which is the whole of ADR-027.
func desiredAuthIdentityAttrs(dbName, hash string) map[string][]string {
	return map[string][]string{
		"objectClass":  {"simpleSecurityObject", "organizationalRole"},
		"cn":           {ldapv1alpha1.AuthIdentityCN(dbName)},
		"userPassword": {hash},
		"description": {
			"Syncrepl bind identity for SlapdDatabase " + dbName + " (ADR-027)",
		},
	}
}

// sshaMatches reports whether an {SSHA} value stored in a directory is the hash
// of the given password — the comparison that turns create-if-missing into
// convergence (ADR-027 decision 6).
//
// An SSHA hash is salted, so two hashes of the same password never compare
// equal and a naive string comparison would rewrite userPassword on every
// reconcile of every database on every pod. The salt is recoverable: slappasswd
// stores base64(sha1(password||salt) || salt) with the salt trailing the
// 20-byte digest, so re-hashing the candidate with the SALT FROM THE STORED
// VALUE reproduces it exactly when the password matches. This is what slapd
// itself does to verify a bind.
//
// Anything it cannot verify is a mismatch, deliberately: an absent attribute,
// an empty or truncated value, a cleartext or foreign-scheme value, or a value
// too short to carry both a digest and a salt. All of those must provoke a
// rewrite rather than be accepted — the password-stripped copy that satisfied
// the old existence check forever, while locking every consumer out, is
// precisely the class ADR-027 was written from.
func sshaMatches(stored, password string) bool {
	const prefix = "{SSHA}"
	if len(stored) < len(prefix) || !strings.EqualFold(stored[:len(prefix)], prefix) {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(stored[len(prefix):])
	if err != nil {
		return false
	}
	if len(raw) <= sha1.Size {
		// No room for a salt after the digest — unverifiable, so not a match.
		return false
	}
	digest, salt := raw[:sha1.Size], raw[sha1.Size:]

	h := sha1.New() // #nosec G401 -- see the import comment: the scheme is slapd's.
	h.Write([]byte(password))
	h.Write(salt)
	return subtle.ConstantTimeCompare(h.Sum(nil), digest) == 1
}
