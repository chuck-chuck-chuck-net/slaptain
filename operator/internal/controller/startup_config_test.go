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

	corev1 "k8s.io/api/core/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The wire format between the operator and the init container's offline
// converger (ADR-034 R1). It is a contract between two binaries that are
// released together but written apart, so it is pinned here in literal bytes —
// tabs spelled out — rather than asserted through the renderer's own helpers,
// which would agree with any format the renderer happened to produce.

func sdStartup(name, clusterRef, suffix, source string) *ldapv1alpha1.SlapdDatabase {
	sd := &ldapv1alpha1.SlapdDatabase{}
	sd.Name = name
	sd.Spec.ClusterRef = clusterRef
	sd.Spec.Suffix = suffix
	sd.Spec.Replication.SyncprovSessionlogSource = source
	return sd
}

func dbList(sds ...*ldapv1alpha1.SlapdDatabase) []ldapv1alpha1.SlapdDatabase {
	out := make([]ldapv1alpha1.SlapdDatabase, 0, len(sds))
	for _, sd := range sds {
		out = append(out, *sd)
	}
	return out
}

func TestStartupConfigRender(t *testing.T) {
	const acc = ldapv1alpha1.SyncprovSessionlogSourceAccesslog

	cases := []struct {
		name string
		dbs  []ldapv1alpha1.SlapdDatabase
		want string
	}{
		{
			// The key exists and its value is empty. Absent FILE means
			// unmanaged to the converger, so omitting the key would be
			// indistinguishable from "this operator predates ADR-034" — and
			// would make removing the last source impossible.
			name: "no database wants a source: the empty file, not a missing key",
			dbs:  dbList(sdStartup("db1", "slapd", "dc=example,dc=org", "")),
			want: "",
		},
		{
			name: "one database wants its journal",
			dbs:  dbList(sdStartup("db1", "slapd", "dc=example,dc=org", acc)),
			want: "olcSpSessionlogSource\tdc=example,dc=org\tcn=accesslog-db1\n",
		},
		{
			// Sorted by suffix, not by list order: the ConfigMap is written on
			// every pass and a List's order is not guaranteed stable, so an
			// unchanged database set must still render byte-identically or the
			// apply churns (ADR-001).
			name: "several, in a stable order regardless of list order",
			dbs: dbList(
				sdStartup("zeta", "slapd", "dc=zeta,dc=org", acc),
				sdStartup("alpha", "slapd", "dc=alpha,dc=org", acc),
				sdStartup("mid", "slapd", "dc=mid,dc=org", acc),
			),
			want: "olcSpSessionlogSource\tdc=alpha,dc=org\tcn=accesslog-alpha\n" +
				"olcSpSessionlogSource\tdc=mid,dc=org\tcn=accesslog-mid\n" +
				"olcSpSessionlogSource\tdc=zeta,dc=org\tcn=accesslog-zeta\n",
		},
		{
			name: "a database of another cluster is not ours to configure",
			dbs: dbList(
				sdStartup("ours", "slapd", "dc=ours,dc=org", acc),
				sdStartup("theirs", "other", "dc=theirs,dc=org", acc),
			),
			want: "olcSpSessionlogSource\tdc=ours,dc=org\tcn=accesslog-ours\n",
		},
		{
			// The in-memory log is a live-writable attribute the SlapdDatabase
			// controller owns; it has no business in a startup-only table.
			name: "unset and InMemory sources contribute nothing",
			dbs: dbList(
				sdStartup("unset", "slapd", "dc=unset,dc=org", ""),
				sdStartup("inmem", "slapd", "dc=inmem,dc=org",
					ldapv1alpha1.SyncprovSessionlogSourceInMemory),
			),
			want: "",
		},
		{
			name: "the sourced database among unsourced ones",
			dbs: dbList(
				sdStartup("inmem", "slapd", "dc=inmem,dc=org",
					ldapv1alpha1.SyncprovSessionlogSourceInMemory),
				sdStartup("sourced", "slapd", "dc=sourced,dc=org", acc),
			),
			want: "olcSpSessionlogSource\tdc=sourced,dc=org\tcn=accesslog-sourced\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := startupConfigFor(tc.dbs, "slapd").render()
			if got != tc.want {
				t.Fatalf("render() =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

// The ConfigMap always carries the key, under the name the converger's mount
// path resolves to. Separate from the rendering cases because "the key exists"
// is the half the file-absent semantics hang on.
func TestStartupConfigMapDataAlwaysCarriesTheKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		dbs  []ldapv1alpha1.SlapdDatabase
	}{
		{"no databases at all", nil},
		{"no database wants a source",
			dbList(sdStartup("db1", "slapd", "dc=example,dc=org", ""))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := startupConfigFor(tc.dbs, "slapd").configMapData()
			v, ok := data[startupConfigKey]
			if !ok {
				t.Fatalf("ConfigMap data has no %q key; an absent file means UNMANAGED to the "+
					"converger, so the attribute could never be removed", startupConfigKey)
			}
			if v != "" {
				t.Fatalf("%s = %q, want the empty string", startupConfigKey, v)
			}
			if len(data) != 1 {
				t.Fatalf("ConfigMap data has %d keys, want exactly 1", len(data))
			}
		})
	}
}

// Every line is exactly three tab-separated fields. The converger splits on a
// single tab, so a value that grew a tab — or a renderer that switched to
// spaces — breaks it silently at the far end.
func TestStartupConfigLinesAreThreeTabSeparatedFields(t *testing.T) {
	got := startupConfigFor(dbList(
		sdStartup("db1", "slapd", "dc=example,dc=org", ldapv1alpha1.SyncprovSessionlogSourceAccesslog),
		sdStartup("db2", "slapd", "ou=people,dc=example,dc=org", ldapv1alpha1.SyncprovSessionlogSourceAccesslog),
	), "slapd").render()

	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("rendered file does not end in a newline: %q", got)
	}
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), got)
	}
	for _, l := range lines {
		fields := strings.Split(l, "\t")
		if len(fields) != 3 {
			t.Errorf("line %q has %d tab-separated fields, want 3", l, len(fields))
		}
		if fields[0] != startupAttrSessionlogSource {
			t.Errorf("line %q names attribute %q, want %q", l, fields[0], startupAttrSessionlogSource)
		}
	}
}

// The value is the SAME accesslog DN the live half resolves, because both read
// one function. A second derivation here is the drift dataDirName's comment
// warns about, and it would point syncprov at a database that does not exist —
// which is an unbootable pod (ADR-034 R4).
func TestStartupConfigValueComesFromDesiredSessionlog(t *testing.T) {
	sd := sdStartup("payroll", "slapd", "dc=example,dc=org",
		ldapv1alpha1.SyncprovSessionlogSourceAccesslog)
	want := desiredSessionlog(sd).SourceDN

	got := startupConfigFor(dbList(sd), "slapd").render()
	if !strings.HasSuffix(got, "\t"+want+"\n") {
		t.Fatalf("render() = %q, want it to end in the desiredSessionlog SourceDN %q", got, want)
	}
}

// The mount is a ConfigMap referenced by NAME — the sibling of
// TestPodTemplateCarriesNoDatabaseSet, for the same reason: the content changes
// whenever a database's spec does, and an inline projection would put that
// content into the pod template's hash and roll the fleet for a value that only
// takes effect at the NEXT pod start anyway (ADR-013, ADR-034 Decision).
//
// Unconditional, read-only pods included. An RO pod runs no syncprov overlay,
// so its converger finds no target entry and does nothing — a no-op is cheaper
// than a second template shape.
func TestPodTemplateMountsStartupConfigByName(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		r := &SlapdClusterReconciler{ClusterDomain: "cluster.local"}
		sc := &ldapv1alpha1.SlapdCluster{}
		sc.Name = "slapd"
		sc.Namespace = "slaptain"
		sc.Spec.Replicas = 3
		sc.Spec.ReadReplicas = 1
		sc.Spec.Replication.Enabled = true
		tmpl := r.buildStatefulSetSpec(sc, readOnly).Template

		var found bool
		for _, v := range tmpl.Spec.Volumes {
			if v.Name != startupConfigVolume {
				continue
			}
			found = true
			if v.ConfigMap == nil {
				t.Fatalf("readOnly=%v: %s is not a ConfigMap volume", readOnly, startupConfigVolume)
			}
			if got, want := v.ConfigMap.Name, startupConfigMapName(sc.Name); got != want {
				t.Errorf("readOnly=%v: %s references ConfigMap %q, want %q",
					readOnly, startupConfigVolume, got, want)
			}
			if len(v.ConfigMap.Items) > 0 {
				t.Errorf("readOnly=%v: %s projects explicit items; the key set would then be part "+
					"of the pod template", readOnly, startupConfigVolume)
			}
			if v.ConfigMap.Optional == nil || !*v.ConfigMap.Optional {
				t.Errorf("readOnly=%v: %s must be optional — a cluster whose operator has not "+
					"rendered it yet must still start pods", readOnly, startupConfigVolume)
			}
		}
		if !found {
			t.Errorf("readOnly=%v: pod template has no %s volume, so the converger has nothing to read",
				readOnly, startupConfigVolume)
		}

		var init *corev1.Container
		for i := range tmpl.Spec.InitContainers {
			if tmpl.Spec.InitContainers[i].Name == "init" {
				init = &tmpl.Spec.InitContainers[i]
			}
		}
		if init == nil {
			t.Fatalf("readOnly=%v: no init container", readOnly)
		}

		var mounted bool
		for _, m := range init.VolumeMounts {
			if m.Name != startupConfigVolume {
				continue
			}
			mounted = true
			if m.MountPath != startupConfigMountPath {
				t.Errorf("readOnly=%v: %s mounted at %q, want %q",
					readOnly, startupConfigVolume, m.MountPath, startupConfigMountPath)
			}
			if !m.ReadOnly {
				t.Errorf("readOnly=%v: %s must be mounted read-only", readOnly, startupConfigVolume)
			}
			if m.SubPath != "" {
				t.Errorf("readOnly=%v: %s is subPath-mounted (%q); a subPath projection never updates",
					readOnly, startupConfigVolume, m.SubPath)
			}
		}
		if !mounted {
			t.Errorf("readOnly=%v: the init container does not mount %s", readOnly, startupConfigVolume)
		}

		var env string
		var hasEnv bool
		for _, e := range init.Env {
			if e.Name == "STARTUP_CONFIG_PATH" {
				env, hasEnv = e.Value, true
			}
		}
		if !hasEnv {
			t.Errorf("readOnly=%v: the init container has no STARTUP_CONFIG_PATH", readOnly)
		} else if env != startupConfigMountPath {
			t.Errorf("readOnly=%v: STARTUP_CONFIG_PATH = %q, want the mount path %q",
				readOnly, env, startupConfigMountPath)
		}

		// The path is a constant. A database name or count reaching it would
		// put the database set back into the template hash by another door.
		if strings.Contains(env, sc.Name+"-") {
			t.Errorf("readOnly=%v: STARTUP_CONFIG_PATH %q is derived, not constant", readOnly, env)
		}
	}
}
