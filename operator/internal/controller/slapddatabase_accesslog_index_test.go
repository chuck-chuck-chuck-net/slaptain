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
)

// The per-database accesslog DBs (ADR-019) must carry the upstream index set:
// entryCSN, objectClass, reqEnd, reqResult, reqStart, reqDN — all eq. reqDN is
// the hot one: multi-provider out-of-order modify resolution searches the local
// log with (&(entryCSN>=…)(reqDN=…)…) on every conflicting write.
//
// planAccesslogIndices is the pure seam: it decides which olcDbIndex values are
// missing, both for a fresh DB (current == nil) and for one created by an older
// operator. The LDAP half only executes the verdict.

func TestAccesslogIndexAttrs_UpstreamSet(t *testing.T) {
	want := []string{"entryCSN", "objectClass", "reqEnd", "reqResult", "reqStart", "reqDN"}
	if len(accesslogIndexAttrs) != len(want) {
		t.Fatalf("accesslogIndexAttrs = %v, want %v", accesslogIndexAttrs, want)
	}
	for i, a := range want {
		if accesslogIndexAttrs[i] != a {
			t.Fatalf("accesslogIndexAttrs[%d] = %q, want %q (%v)", i, accesslogIndexAttrs[i], a, accesslogIndexAttrs)
		}
	}
}

func TestPlanAccesslogIndices(t *testing.T) {
	full := "entryCSN,objectClass,reqEnd,reqResult,reqStart,reqDN eq"

	cases := []struct {
		name    string
		current []string
		want    []string
	}{
		{
			name:    "fresh DB gets the whole upstream set in one value",
			current: nil,
			want:    []string{full},
		},
		{
			name:    "already complete: no write",
			current: []string{full},
			want:    nil,
		},
		{
			name:    "complete but split across values: no write",
			current: []string{"entryCSN eq", "objectClass eq", "reqEnd,reqResult,reqStart eq", "reqDN eq"},
			want:    nil,
		},
		{
			name: "pre-fix log DB: add only what is missing, preserving upstream order",
			// What every operator up to this change wrote.
			current: []string{"default eq", "reqEnd,reqResult,reqStart eq"},
			want:    []string{"entryCSN,objectClass,reqDN eq"},
		},
		{
			name:    "attribute names are case-insensitive",
			current: []string{"ENTRYCSN,objectclass,reqend,REQRESULT,reqstart,reqdn eq"},
			want:    nil,
		},
		{
			name:    "surplus whitespace in a live value is not a difference",
			current: []string{"  entryCSN,objectClass,reqEnd,reqResult,reqStart,reqDN   eq  "},
			want:    nil,
		},
		{
			name: "an attribute indexed with another type counts as configured",
			// Adding a second definition for an already-indexed attribute is a
			// back-mdb config error, so a hand-set richer index must be left alone.
			current: []string{"reqDN sub", "reqEnd,reqResult,reqStart eq"},
			want:    []string{"entryCSN,objectClass eq"},
		},
		{
			name:    "a bare attribute with no explicit type still counts as configured",
			current: []string{"entryCSN", "objectClass,reqEnd,reqResult,reqStart,reqDN eq"},
			want:    nil,
		},
		{
			name:    "default eq alone covers nothing",
			current: []string{"default eq"},
			want:    []string{full},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planAccesslogIndices(tc.current)
			if len(got) != len(tc.want) {
				t.Fatalf("planAccesslogIndices(%v) = %v, want %v", tc.current, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("planAccesslogIndices(%v)[%d] = %q, want %q",
						tc.current, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// The plan must be idempotent: applying its output and re-planning yields nothing.
func TestPlanAccesslogIndices_Idempotent(t *testing.T) {
	for _, current := range [][]string{
		nil,
		{"default eq", "reqEnd,reqResult,reqStart eq"},
		{"reqDN sub"},
	} {
		after := append(append([]string{}, current...), planAccesslogIndices(current)...)
		if got := planAccesslogIndices(after); got != nil {
			t.Fatalf("second plan over %v (from %v) = %v, want nil",
				after, current, got)
		}
	}
}

// reqDN is the reason this exists (BACKLOG: "Accesslog index set is incomplete").
func TestPlanAccesslogIndices_CoversReqDN(t *testing.T) {
	got := planAccesslogIndices([]string{"default eq", "reqEnd,reqResult,reqStart eq"})
	if len(got) != 1 || !strings.Contains(strings.ToLower(got[0]), "reqdn") {
		t.Fatalf("plan over a pre-fix log DB = %v, want it to add reqDN", got)
	}
}
