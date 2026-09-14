/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package suffixprobe reads one database suffix's base entry off one pod and
// classifies what it finds (ADR-025).
//
// A multi-site seed race can demote a pod's suffix entry to a GLUE entry:
// slapd hides glue from ordinary searches at the frontend — not via ACLs, so
// even the rootDN sees nothing — and only a ManageDsaIT base search (RFC 3296)
// reveals it. The pod is then silently broken for every client doing a base
// search, while every CSN-based signal reads healthy (the glue carries the
// winner's entryCSN).
//
// This is the single implementation of that probe. It has three consumers,
// which previously carried two divergent copies between them:
//
//   - the SlapdDatabase controller's DataPresent condition (per RW pod),
//   - the SlapdBackup controller's SourceSuffixHealthy condition (source pod),
//   - slctl inspect's suffix-visibility / suffix-uuid-agreement checks (all pods).
//
// The contract they now share: positive evidence only. A search that fails for
// transport, bind or ACL reasons is OutcomeError — "could not read it" is never
// reported as "it is not there".
package suffixprobe

import (
	"strings"

	"github.com/go-ldap/ldap/v3"
)

// Outcome classifies one pod's suffix entry.
type Outcome int

const (
	// OutcomeVisible — an ordinary base search returned the entry: a real,
	// restorable suffix entry.
	OutcomeVisible Outcome = iota
	// OutcomeGlue — hidden from ordinary search, and a ManageDsaIT search
	// revealed a glue entry. The pod is silently broken for base searches and
	// any backup taken from it will fail restore preflight (ADR-025).
	OutcomeGlue
	// OutcomeMissing — hidden from ordinary search and ManageDsaIT found
	// nothing either: the suffix entry does not exist on this pod.
	OutcomeMissing
	// OutcomeError — the probe could not reach a verdict (search error, denied
	// read, or an entry that is hidden without being glue). Silence is not
	// evidence: never read this as an absent or a healthy entry.
	OutcomeError
)

func (o Outcome) String() string {
	switch o {
	case OutcomeVisible:
		return "visible"
	case OutcomeGlue:
		return "glue"
	case OutcomeMissing:
		return "missing"
	default:
		return "error"
	}
}

// Observation is one pod's answer for one suffix.
type Observation struct {
	// Suffix is the database suffix probed.
	Suffix string
	// Outcome is the classification.
	Outcome Outcome
	// UUID is the entry's entryUUID when readable (""), from whichever search
	// returned the entry. entryUUID is syncrepl's identity: two pods reporting
	// different UUIDs for the same DN hold different entries — the seed-race
	// residue CSN comparison cannot see.
	UUID string
	// Detail carries probe specifics (error text, the glue's entryUUID) for
	// messages. Never parsed.
	Detail string
}

// Visible reports whether an ordinary base search returned the entry.
func (o Observation) Visible() bool { return o.Outcome == OutcomeVisible }

// Glue reports whether a ManageDsaIT search positively identified a glue entry.
func (o Observation) Glue() bool { return o.Outcome == OutcomeGlue }

// Assessable reports whether the probe reached a verdict at all.
func (o Observation) Assessable() bool { return o.Outcome != OutcomeError }

// probeAttrs is what both searches ask for. entryUUID rides along on the
// ordinary search too so the UUID-agreement check can compare healthy pods
// against each other, not just against a glue.
var probeAttrs = []string{"objectClass", "structuralObjectClass", "entryUUID"}

// Probe reads suffix's base entry over an already-bound connection: an ordinary
// base search first, then — only when that comes up empty — a ManageDsaIT base
// search to tell a hidden glue from a genuinely absent entry.
//
// The caller owns the connection, its bind identity and its timeouts. Which
// identity binds matters for what a denied read looks like: a rootDN bind
// (DataPresent) cannot be ACL-denied, while an anonymous or replication-identity
// bind can — which is exactly why a denied read must come back as OutcomeError
// and not as OutcomeMissing.
func Probe(conn *ldap.Conn, suffix string) Observation {
	o := Observation{Suffix: suffix, Outcome: OutcomeError}
	if suffix == "" {
		o.Detail = "no suffix configured"
		return o
	}

	res, err := conn.Search(ldap.NewSearchRequest(
		suffix, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 5, false,
		"(objectClass=*)", probeAttrs, nil,
	))
	switch {
	case err == nil && len(res.Entries) > 0:
		o.Outcome = OutcomeVisible
		o.UUID = res.Entries[0].GetEqualFoldAttributeValue("entryUUID")
		return o
	case err != nil && !ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject):
		// A real failure (transport, timeout, denied). Not evidence of absence.
		o.Detail = err.Error()
		return o
	}

	// Not visible to an ordinary search. ManageDsaIT lifts slapd's frontend
	// hiding of glue entries (the control, not an ACL, is what hides them).
	mRes, err := conn.Search(ldap.NewSearchRequest(
		suffix, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 5, false,
		"(objectClass=*)", probeAttrs,
		[]ldap.Control{ldap.NewControlManageDsaIT(false)},
	))
	if err != nil {
		if ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
			o.Outcome = OutcomeMissing
			return o
		}
		o.Detail = err.Error()
		return o
	}
	if len(mRes.Entries) == 0 {
		o.Outcome = OutcomeMissing
		return o
	}
	return Classify(mRes.Entries[0], suffix)
}

// Classify decides what a ManageDsaIT-revealed entry is. Split out from Probe
// so the classification — the part that encodes ADR-025's signature — is unit
// testable without a server.
func Classify(e *ldap.Entry, suffix string) Observation {
	o := Observation{Suffix: suffix, UUID: e.GetEqualFoldAttributeValue("entryUUID")}

	glue := strings.EqualFold(e.GetEqualFoldAttributeValue("structuralObjectClass"), "glue")
	for _, oc := range e.GetEqualFoldAttributeValues("objectClass") {
		if strings.EqualFold(strings.TrimSpace(oc), "glue") {
			glue = true
		}
	}
	if glue {
		o.Outcome = OutcomeGlue
		if o.UUID != "" {
			o.Detail = "entryUUID " + o.UUID
		}
		return o
	}

	// Revealed by the control but hidden without it, and not glue: exotic
	// (a referral or subentry). Report "could not assess" rather than guess —
	// this probe's job is the glue class, and a wrong confident answer here
	// would be worse than none.
	o.Outcome = OutcomeError
	o.Detail = "suffix entry hidden from ordinary search but not a glue entry"
	return o
}
