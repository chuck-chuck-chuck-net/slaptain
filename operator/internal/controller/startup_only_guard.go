/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"fmt"
	"strings"

	"github.com/go-ldap/ldap/v3"
)

// The one place a cn=config write leaves this operator (ADR-034 R2).
//
// A minority of cn=config cannot be written to a RUNNING slapd at all. The
// project found every member of that minority by killing or hanging a live
// server — olcDbMaxSize and olcDbEnvFlags segfault it, olcIdleTimeout and
// olcWriteTimeout hang it (ADR-024 amendment), and olcSpSessionlogSource is a
// double free in sp_cf_gen: the online path frees c->value_ndn.bv_val and then
// stores the freed pointer in si_logbase, which the delete path frees again
// (ADR-034 context). Reproduced 60/60 under AddressSanitizer, byte-identical in
// every release from 2.5.20 to master, and it has already cost two live
// providers.
//
// For olcSpSessionlogSource the change path is the init container's offline
// converger: slapmodify against slapd.d at pod start, when nothing holds the
// configuration open. The operator's part is to observe (see
// startup_config_converged.go) and never to write. This guard is what makes
// "never" a property of the code rather than a promise in a comment, because
// the function that reintroduces the write does not exist yet and will not be
// written by anyone reading the ADR.
//
// Both verbs are guarded. An LDAP Add of the syncprov overlay entry carrying
// the attribute takes the same path as a Modify: CONFIG_ONLINE_ADD(ca) is
// (!(ca)->lineno) and config_add_internal sets lineno = 0 for ANY LDAP
// operation against cn=config, reserving 1 for the configuration-directory read
// at startup (bconfig.c:5497-5505). Guarding Modify alone would be a false
// comfort.
//
// Deletes are refused too, though the delete path alone is survivable. The
// operator never needs one: removal is the converger's job, offline, and a
// deny-list with an exception is a deny-list somebody will reason their way
// around.

// startupOnlyAttributes is the declared deny-list, as DATA so the next member
// of the class is one line here and one line in the converger's table — not a
// new mechanism. Keys are lowercase attribute names.
//
// Today's single entry is ADR-034 R5's "one customer per change":
// olcDbMaxSize and olcDbEnvFlags belong to the same class but are segfaults
// rather than this double free, and moving them onto the vehicle is a separate
// decision per tunable with its own live evidence.
var startupOnlyAttributes = map[string]struct{}{
	strings.ToLower(startupAttrSessionlogSource): {},
}

// ldapWriter is the slice of *ldap.Conn this guard interposes on. An interface
// rather than the concrete type for one reason: it lets the guard's own test
// assert that a refused request never reached a connection, which is the whole
// assertion — "returned an error" is not it.
type ldapWriter interface {
	Modify(*ldap.ModifyRequest) error
	Add(*ldap.AddRequest) error
}

// normalizeAttrName renders an attribute description the way a comparison
// against the deny-list must see it: an LDAP attribute description is
// case-insensitive and may carry ;options (RFC 4512 §2.5), and slapd accepts
// both spellings into the same configuration handler. A guard that only
// matched the camel-case spelling would be bypassed by the exact request it
// exists to stop.
func normalizeAttrName(name string) string {
	n := strings.TrimSpace(name)
	if i := strings.IndexByte(n, ';'); i >= 0 {
		n = n[:i]
	}
	return strings.ToLower(n)
}

// startupOnlyAttrViolation returns the FIRST denied attribute name, verbatim as
// the caller spelled it, or "" when the request is ordinary. Verbatim because
// the error has to be greppable against the code that produced it.
func startupOnlyAttrViolation(names []string) string {
	for _, name := range names {
		if _, denied := startupOnlyAttributes[normalizeAttrName(name)]; denied {
			return name
		}
	}
	return ""
}

// startupOnlyError is the refusal, phrased so the reader lands on the ADR
// rather than on a workaround.
func startupOnlyError(attr, dn string) error {
	return fmt.Errorf("refusing to write startup-only attribute %q to %s on a running slapd: "+
		"it is converged offline by the init container at pod start (ADR-034 R2); "+
		"writing it online crashes the server", attr, dn)
}

// guardedModify is the cn=config Modify every controller in this package uses.
// Behaviourally a pass-through: the request is forwarded unchanged and the
// connection's error is returned verbatim, so the ldap.IsErrorWithCode checks
// at the call sites keep working.
func guardedModify(conn ldapWriter, req *ldap.ModifyRequest) error {
	names := make([]string, 0, len(req.Changes))
	for _, c := range req.Changes {
		names = append(names, c.Modification.Type)
	}
	if attr := startupOnlyAttrViolation(names); attr != "" {
		return startupOnlyError(attr, req.DN)
	}
	return conn.Modify(req)
}

// guardedAdd is the cn=config Add counterpart. See the package comment above
// for why an Add is as fatal as a Modify.
func guardedAdd(conn ldapWriter, req *ldap.AddRequest) error {
	names := make([]string, 0, len(req.Attributes))
	for _, a := range req.Attributes {
		names = append(names, a.Type)
	}
	if attr := startupOnlyAttrViolation(names); attr != "" {
		return startupOnlyError(attr, req.DN)
	}
	return conn.Add(req)
}
