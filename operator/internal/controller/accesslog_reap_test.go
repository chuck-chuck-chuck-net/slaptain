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
	"testing"
)

// The live shape this guards, observed on t3e after an in-place ADR-019 R8
// migration performed by a build that reused a cached olcDatabase={N} DN across
// the legacy log's deletion:
//
//	olcDatabase={3}mdb,cn=config                          cn=accesslog-example-db
//	olcOverlay={0}syncprov,olcDatabase={3}mdb,cn=config    legitimate
//	olcOverlay={1}accesslog,olcDatabase={3}mdb,cn=config   olcAccessLogDB: cn=accesslog-example-db2
//
// The third entry made every write to example-db land a foreign reqDN in
// example-db2's journal, and example-db2's consumers logged 2466 "delta-sync
// lost sync" lines in a 30-second window.
func TestUnwantedLogDBChildren(t *testing.T) {
	const logDN = "olcDatabase={3}mdb,cn=config"

	cases := []struct {
		name     string
		children []observedConfigEntry
		want     []string
	}{
		{
			name: "healthy log DB: syncprov only, nothing to reap",
			children: []observedConfigEntry{
				{DN: "olcOverlay={0}syncprov," + logDN, Classes: []string{"olcOverlayConfig", "olcSyncProvConfig"}},
			},
			want: nil,
		},
		{
			name:     "no children at all",
			children: nil,
			want:     nil,
		},
		{
			name: "mis-attached accesslog overlay is reaped",
			children: []observedConfigEntry{
				{DN: "olcOverlay={0}syncprov," + logDN, Classes: []string{"olcOverlayConfig", "olcSyncProvConfig"}},
				{DN: "olcOverlay={1}accesslog," + logDN, Classes: []string{"olcOverlayConfig", "olcAccessLogConfig"}},
			},
			want: []string{"olcOverlay={1}accesslog," + logDN},
		},
		{
			name: "objectClass matching is case-insensitive (slapd normalises on its own schedule)",
			children: []observedConfigEntry{
				{DN: "olcOverlay={1}accesslog," + logDN, Classes: []string{"olcoverlayconfig", "olcaccesslogconfig"}},
			},
			want: []string{"olcOverlay={1}accesslog," + logDN},
		},
		{
			name: "an unrecognised child is left alone, not reaped",
			children: []observedConfigEntry{
				{DN: "olcOverlay={1}memberof," + logDN, Classes: []string{"olcOverlayConfig", "olcMemberOf"}},
			},
			want: nil,
		},
		{
			name: "several mis-attached overlays are all reaped, in order",
			children: []observedConfigEntry{
				{DN: "olcOverlay={1}accesslog," + logDN, Classes: []string{"olcAccessLogConfig"}},
				{DN: "olcOverlay={2}accesslog," + logDN, Classes: []string{"olcAccessLogConfig"}},
			},
			want: []string{
				"olcOverlay={1}accesslog," + logDN,
				"olcOverlay={2}accesslog," + logDN,
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := unwantedLogDBChildren(c.children)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("unwantedLogDBChildren() = %v, want %v", got, c.want)
			}
		})
	}
}
