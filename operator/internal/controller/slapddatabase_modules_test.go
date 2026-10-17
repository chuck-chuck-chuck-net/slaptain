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

func TestModuleBaseName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{overlayAccesslog, overlayAccesslog},
		{"{0}back_mdb", "back_mdb"},
		{"{12}syncprov", overlaySyncprov},
		{"accesslog.la", overlayAccesslog},
		{"syncprov.so", overlaySyncprov},
		{"syncprov.so.2", overlaySyncprov},
		{"{1}/usr/lib/ldap/accesslog.so.2.0.200", overlayAccesslog},
		{"/usr/lib/ldap/back_mdb.la", "back_mdb"},
		// A leading "{" without "}" is not an ordering prefix.
		{"{oddname", "{oddname"},
	}
	for _, c := range cases {
		if got := moduleBaseName(c.in); got != c.want {
			t.Errorf("moduleBaseName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMissingModules(t *testing.T) {
	cases := []struct {
		name   string
		loaded []string
		wanted []string
		want   []string
	}{
		{
			// The standalone→HA transition that motivated this helper:
			// pod bootstrapped without replication has only back_mdb.
			name:   "standalone pod missing both",
			loaded: []string{"{0}back_mdb"},
			wanted: []string{overlayAccesslog, overlaySyncprov},
			want:   []string{overlayAccesslog, overlaySyncprov},
		},
		{
			name:   "fresh replicated pod has everything",
			loaded: []string{"{0}back_mdb", "{1}accesslog", "{2}syncprov"},
			wanted: []string{overlayAccesslog, overlaySyncprov},
			want:   nil,
		},
		{
			name:   "partial: syncprov missing",
			loaded: []string{"{0}back_mdb", "{1}accesslog"},
			wanted: []string{overlayAccesslog, overlaySyncprov},
			want:   []string{overlaySyncprov},
		},
		{
			name:   "extension and path variants still match",
			loaded: []string{"{0}back_mdb.la", "{1}/usr/lib/ldap/accesslog.so.2"},
			wanted: []string{overlayAccesslog, overlaySyncprov},
			want:   []string{overlaySyncprov},
		},
		{
			name:   "empty loaded list",
			loaded: nil,
			wanted: []string{overlaySyncprov},
			want:   []string{overlaySyncprov},
		},
		{
			name:   "empty wanted list",
			loaded: []string{"{0}back_mdb"},
			wanted: nil,
			want:   nil,
		},
		{
			name:   "order of wanted preserved",
			loaded: []string{"{0}back_mdb"},
			wanted: []string{overlaySyncprov, overlayAccesslog},
			want:   []string{overlaySyncprov, overlayAccesslog},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := missingModules(c.loaded, c.wanted); !reflect.DeepEqual(got, c.want) {
				t.Errorf("missingModules(%v, %v) = %v, want %v",
					c.loaded, c.wanted, got, c.want)
			}
		})
	}
}
