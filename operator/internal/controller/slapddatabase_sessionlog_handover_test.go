/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"testing"
)

// The session log is HANDED OVER, never dropped (ADR-034 milestone 6).
//
// olcSpSessionlog is live-writable and olcSpSessionlogSource is not, so the two
// halves of a handover land at different moments: the operator can take the
// in-memory log away this second, while its replacement cannot arrive until the
// init container converges the configuration offline at the pod's next start —
// a restart a human schedules and may never take.
//
// Removing the in-memory log on the spec edit therefore opens a window, of
// unbounded length, in which the database has NO session log of any kind. Every
// consumer that falls behind in that window takes a full present-phase refresh
// instead of a replay: precisely the storm class ADR-022 exists to avoid.
//
// The rule, and the whole subject of this file: the database always has exactly
// one session log, and the operator never constructs the state where it has
// none or two. That makes the live value of olcSpSessionlogSource an INPUT to
// the decision — the pod's own state, not the spec alone, decides whether the
// in-memory log is still load-bearing.
//
// "Leave it alone" is consequently a verdict in its own right, distinct from
// "nothing to do": one means the attribute is already what it should be, the
// other means it is NOT what the spec asks for and must survive anyway.

// inMemoryDecision renders the pre-ADR-034 pair (ops, enabled) as the decision
// planSessionlog now takes, so the ADR-022 table above reads unchanged.
func inMemoryDecision(ops int32, enabled bool) sessionlogDecision {
	if !enabled {
		return sessionlogDecision{Kind: sessionlogKindNone}
	}
	return sessionlogDecision{Kind: sessionlogKindInMemory, Ops: ops}
}

func accesslogDecision() sessionlogDecision {
	return sessionlogDecision{
		Kind:     sessionlogKindAccesslog,
		SourceDN: "cn=accesslog-default",
	}
}

// TestPlanSessionlog_Handover is the four-row truth table of the rule: what the
// operator may do to olcSpSessionlog given what the spec declares and what the
// pod actually runs.
func TestPlanSessionlog_Handover(t *testing.T) {
	cases := []struct {
		name       string
		current    []string
		decision   sessionlogDecision
		sourceLive bool
		wantAction sessionlogAction
		wantValue  string
	}{
		// Row 1 — the defect. The spec asks for the sourced log, the pod does
		// not have it yet, and the in-memory log is the only one the database
		// has. Taking it away now buys nothing (the source cannot arrive until
		// the restart) and costs every reconnect a full refresh until then.
		{
			name:       "Accesslog declared, source not yet on the pod: leave the in-memory log standing",
			current:    []string{"5000"},
			decision:   accesslogDecision(),
			sourceLive: false,
			wantAction: sessionlogLeave,
		},
		{
			name:       "Accesslog declared, source not yet on the pod, no in-memory log either: still leave it",
			current:    nil,
			decision:   accesslogDecision(),
			sourceLive: false,
			wantAction: sessionlogLeave,
		},
		// Row 2 — the handover completed on this pod. The sourced log is
		// serving, so the in-memory one is the second log, and slapd itself
		// calls the pair wasteful.
		{
			name:       "Accesslog declared and live: remove the in-memory log",
			current:    []string{"5000"},
			decision:   accesslogDecision(),
			sourceLive: true,
			wantAction: sessionlogRemove,
		},
		{
			name:       "Accesslog declared and live, in-memory log already gone: nothing to do",
			current:    nil,
			decision:   accesslogDecision(),
			sourceLive: true,
			wantAction: sessionlogNoop,
		},
		// Row 3 — the handover BACK, before the restart. The field was dropped
		// but the sourced log is still on disk and still serving; adding an
		// in-memory one beside it makes two, and the admission rule, slapd and
		// ADR-034 all refuse that pair. The in-memory log returns when the
		// source goes, which is at the pod's restart.
		{
			name:       "field dropped but the sourced log is still live: do not add the in-memory one",
			current:    nil,
			decision:   inMemoryDecision(defaultSyncprovSessionlogOps, true),
			sourceLive: true,
			wantAction: sessionlogLeave,
		},
		{
			name:       "sessionlog 0 with the sourced log still live: do not remove what is serving nothing of ours",
			current:    []string{"5000"},
			decision:   sessionlogDecision{Kind: sessionlogKindNone},
			sourceLive: true,
			wantAction: sessionlogLeave,
		},
		// Row 4 — the ordinary world, unchanged. No source anywhere, so the
		// in-memory log is converged exactly as ADR-022 always had it.
		{
			name:       "no source anywhere: set the in-memory log as before",
			current:    nil,
			decision:   inMemoryDecision(5000, true),
			sourceLive: false,
			wantAction: sessionlogSet,
			wantValue:  "5000",
		},
		{
			name:       "no source anywhere and disabled: remove the in-memory log as before",
			current:    []string{"5000"},
			decision:   sessionlogDecision{Kind: sessionlogKindNone},
			sourceLive: false,
			wantAction: sessionlogRemove,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			action, value := planSessionlog(tc.current, tc.decision, tc.sourceLive)
			if action != tc.wantAction {
				t.Fatalf("action = %v, want %v", action, tc.wantAction)
			}
			if tc.wantAction == sessionlogSet && value != tc.wantValue {
				t.Fatalf("value = %q, want %q", value, tc.wantValue)
			}
		})
	}
}

// TestPlanSessionlog_NeverLeavesTheDatabaseWithoutALog states the rule as an
// invariant rather than as a row: over every combination of declaration and
// live state, there is no case in which the operator removes the in-memory log
// while no sourced log is running.
//
// The table above would catch today's defect; this catches the next shape of
// it, including one introduced by a plausible-looking refactor.
func TestPlanSessionlog_NeverLeavesTheDatabaseWithoutALog(t *testing.T) {
	decisions := map[string]sessionlogDecision{
		"Accesslog": accesslogDecision(),
		"InMemory":  inMemoryDecision(defaultSyncprovSessionlogOps, true),
		"None":      {Kind: sessionlogKindNone},
	}
	currents := map[string][]string{
		"in-memory log present": {"5000"},
		"in-memory log absent":  nil,
	}

	for dName, d := range decisions {
		for cName, current := range currents {
			t.Run(dName+"/"+cName+"/source absent", func(t *testing.T) {
				action, _ := planSessionlog(current, d, false)
				// With no source live, a removal leaves the database with no
				// session log at all. Only an explicit "none wanted" may do
				// that, and only then because the user asked for it.
				if action == sessionlogRemove && d.Kind != sessionlogKindNone {
					t.Fatalf("action = sessionlogRemove with no source live: the database "+
						"would be left with no session log of any kind until a restart "+
						"nobody has scheduled (decision %s, %s)", dName, cName)
				}
			})
			t.Run(dName+"/"+cName+"/source live", func(t *testing.T) {
				action, _ := planSessionlog(current, d, true)
				// With the sourced log live, writing an in-memory one makes two.
				if action == sessionlogSet {
					t.Fatalf("action = sessionlogSet while olcSpSessionlogSource is live: "+
						"the database would carry two session logs, the pair slapd calls "+
						"wasteful (decision %s, %s)", dName, cName)
				}
			})
		}
	}
}

// TestOverlayCreationSessionlog is the other half of "never without a log", on
// the one path that has no live state to read: a syncprov overlay being created
// from nothing.
//
// The Add cannot carry olcSpSessionlogSource — every online path through
// sp_cf_gen is the double free, Add included, and the guard refuses it — so a
// brand-new pod whose database declares Accesslog would run with no session log
// at all until its first restart. It gets the in-memory default instead, which
// the converger then swaps for the sourced one offline.
func TestOverlayCreationSessionlog(t *testing.T) {
	cases := []struct {
		name        string
		decision    sessionlogDecision
		wantOps     int32
		wantEnabled bool
	}{
		{
			name:        "Accesslog: the in-memory default stands in until the first restart",
			decision:    accesslogDecision(),
			wantOps:     defaultSyncprovSessionlogOps,
			wantEnabled: true,
		},
		{
			name:        "InMemory: the declared size, as always",
			decision:    inMemoryDecision(42, true),
			wantOps:     42,
			wantEnabled: true,
		},
		{
			name:        "None: the user asked for no session log, and gets none",
			decision:    sessionlogDecision{Kind: sessionlogKindNone},
			wantEnabled: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ops, enabled := tc.decision.overlayCreationSessionlog()
			if enabled != tc.wantEnabled {
				t.Fatalf("enabled = %v, want %v", enabled, tc.wantEnabled)
			}
			if enabled && ops != tc.wantOps {
				t.Fatalf("ops = %d, want %d", ops, tc.wantOps)
			}
		})
	}
}
