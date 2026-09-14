/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import "strings"

// Accesslog hygiene helpers that are genuinely controller-local: deciding, from
// one pod's observed cn=config, what must not be there.
//
// How a per-database log is *named* lives in api/v1alpha1 (AccesslogSuffix /
// AccesslogDir / ExternalLogBase) — it is part of the observable contract.

// observedConfigEntry is one cn=config entry as read off a pod: its DN and its
// objectClass values.
type observedConfigEntry struct {
	DN      string
	Classes []string
}

// unwantedLogDBChildren returns the DNs of children of an accesslog database
// that must not exist there.
//
// An accesslog database legitimately carries exactly one child: the syncprov
// overlay that exposes its journal to consumers (ensureAccesslogDB adds it). An
// **accesslog overlay** on an accesslog database is never legitimate: it makes
// one journal journal into another, so a consumer of the second database
// receives log entries whose reqDN lives under the first database's accesslog
// suffix, submits them to its own backend, and gets NO_SUCH_OBJECT — ADR-019
// Fact 2, with the journals themselves as the two databases.
//
// That state is not hypothetical: it is what the renumbering class produces
// (ADR-026 R1). olcDatabase={N} is a POSITIONAL handle — slapd renumbers every
// database ordered after a deleted one — so a DN resolved before a delete and
// used after it names whatever slid into that slot, and an accesslog overlay
// written through such a DN lands on a journal. Every delete in this controller
// now reports itself and its caller re-resolves, but the live delete path that
// can renumber is still there (the ADR-010 peer → consumer-only demotion reaps
// a log DB, which usually but not always sits above its data DB), and a cluster
// mis-configured by an older build keeps the mis-attached overlay forever —
// nothing else would ever remove it — so ensureAccesslogDB reaps it.
//
// Anything else found under a log DB is left alone. The rule is "delete what
// this operator can positively attribute to a defect of its own", not "delete
// what this operator did not put here": cn=config is node-local and
// hand-editable (ADR-002), and a reaper that removes unrecognised entries is a
// footgun.
func unwantedLogDBChildren(children []observedConfigEntry) []string {
	var out []string
	for _, c := range children {
		for _, cls := range c.Classes {
			if strings.EqualFold(cls, "olcAccessLogConfig") {
				out = append(out, c.DN)
				break
			}
		}
	}
	return out
}
