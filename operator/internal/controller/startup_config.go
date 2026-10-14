/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The startup-only cn=config attributes a cluster's pods must carry, and how
// the operator hands them to the init container's offline converger (ADR-034).
//
// A minority of cn=config cannot be written to a running slapd at all:
// olcSpSessionlogSource is a double free in sp_cf_gen on every online path,
// Add included, so the value can only ever enter through slapd.d as read at
// startup. The converger runs in the init container, at the one moment in a
// pod's life when the configuration is on disk and nothing holds it open, and
// it reads its instructions from here.
//
// A ConfigMap mounted BY NAME, for the reason database_dirs.go gives: the
// content moves whenever a database's spec does, and an inline projection would
// put that content into the pod template's hash — rolling the fleet for a value
// that only takes effect at the next pod start anyway (ADR-013's 2026-09-28
// amendments). The volume itself is structural: it appears once, when the
// operator that writes it is first deployed.

const (
	// startupConfigVolume is the name of the ConfigMap volume.
	startupConfigVolume = "startup-config"
	// startupConfigMountPath is where the init container finds the table. A
	// constant, not a derived path: anything cluster- or database-specific here
	// would be back in the pod template.
	startupConfigMountPath = "/etc/slaptain/startup-config"
	// startupConfigKey is the ConfigMap key, and therefore the file name under
	// the mount path.
	startupConfigKey = "startup-config"
	// startupConfigFieldSep separates a record's three fields. A tab, because a
	// DN contains spaces freely and neither an attribute name nor a DN can
	// contain a tab.
	startupConfigFieldSep = "\t"

	// startupAttrSessionlogSource is the first and, for now, only member of the
	// declared table (ADR-034 R5). WHICH cn=config entry carries it is a
	// property of the attribute and is compiled into the converger — the file
	// names the attribute, the database and the value, leaving the shape of
	// cn=config to the side that reads it.
	startupAttrSessionlogSource = "olcSpSessionlogSource"
)

// startupConfigMapName is the ConfigMap holding a cluster's startup-only table.
// One per SlapdCluster, owned by it, shared by both StatefulSets.
func startupConfigMapName(clusterName string) string {
	return clusterName + "-startup-config"
}

// startupConfigRecord is one line: an attribute, the olcSuffix of the mdb
// database it belongs to, and the value wanted there. The suffix rather than a
// cn=config DN, because an olcDatabase={N} prefix is positional and assigned by
// slapd (ADR-026 R1) — the converger matches on olcSuffix, case-insensitively,
// in the dump it just read.
type startupConfigRecord struct {
	Attribute string
	Suffix    string
	Value     string
}

// startupConfig is one cluster's table.
type startupConfig struct {
	Records []startupConfigRecord
}

// startupConfigFor computes the table for one cluster. Sorted and deduplicated,
// so an unchanged database set renders byte-identically and the apply is a
// no-op (ADR-001), exactly as databaseDirsFor arranges for the directory lists.
//
// The desired value is not derived here: desiredSessionlog is the single
// resolution of spec.replication.syncprovSessionlogSource, shared with the live
// LDAP half, so the two cannot disagree about which database is sourced or
// about the journal's DN. A second derivation would be the drift dataDirName's
// comment warns about, and here it would point syncprov at a database that does
// not exist — a pod that refuses to start (ADR-034 R4).
func startupConfigFor(dbs []ldapv1alpha1.SlapdDatabase, clusterName string) startupConfig {
	var recs []startupConfigRecord
	for i := range dbs {
		sd := &dbs[i]
		if sd.Spec.ClusterRef != clusterName {
			continue
		}
		if d := desiredSessionlog(sd); d.Kind == sessionlogKindAccesslog {
			recs = append(recs, startupConfigRecord{
				Attribute: startupAttrSessionlogSource,
				Suffix:    sd.Spec.Suffix,
				Value:     d.SourceDN,
			})
		}
	}
	return startupConfig{Records: sortedStartupRecords(recs)}
}

// sortedStartupRecords orders by attribute, then suffix, then value, and drops
// exact duplicates. Two SlapdDatabase CRs naming the same suffix is a conflict
// the SlapdDatabase controller owns; whatever it decides, this file must not
// carry the same line twice.
func sortedStartupRecords(recs []startupConfigRecord) []startupConfigRecord {
	if len(recs) == 0 {
		return nil
	}
	sort.Slice(recs, func(i, j int) bool {
		a, b := recs[i], recs[j]
		if a.Attribute != b.Attribute {
			return a.Attribute < b.Attribute
		}
		if a.Suffix != b.Suffix {
			return a.Suffix < b.Suffix
		}
		return a.Value < b.Value
	})
	out := recs[:1]
	for _, r := range recs[1:] {
		if r != out[len(out)-1] {
			out = append(out, r)
		}
	}
	return out
}

// render produces the file the converger reads: one record per line, three
// tab-separated fields, trailing newline. Empty table, empty file — NOT an
// absent one, which is how the converger is told it has nothing to manage.
func (c startupConfig) render() string {
	if len(c.Records) == 0 {
		return ""
	}
	var b strings.Builder
	for _, r := range c.Records {
		b.WriteString(r.Attribute)
		b.WriteString(startupConfigFieldSep)
		b.WriteString(r.Suffix)
		b.WriteString(startupConfigFieldSep)
		b.WriteString(r.Value)
		b.WriteString("\n")
	}
	return b.String()
}

// configMapData renders the table as the one file the init container reads.
//
// The key is ALWAYS present, even empty. An absent file means "unmanaged" to
// the converger — the state a pre-ADR-034 operator leaves — so omitting the key
// when nothing wants a source would be indistinguishable from that, and would
// make removing the last source impossible: the attribute would simply stay on
// disk forever.
func (c startupConfig) configMapData() map[string]string {
	return map[string]string{startupConfigKey: c.render()}
}

// reconcileStartupConfigMap publishes the cluster's startup-only table.
//
// Owned by the SlapdCluster, so it is garbage-collected with it, and applied
// with SSA like every other resource this controller owns. Written before the
// StatefulSet, so a pod created by the same pass finds current content — and
// writing it is the operator's whole involvement: it never puts one of these
// attributes on the wire (ADR-034 R2).
func (r *SlapdClusterReconciler) reconcileStartupConfigMap(
	ctx context.Context, sc *ldapv1alpha1.SlapdCluster,
) error {
	var dbList ldapv1alpha1.SlapdDatabaseList
	if err := r.List(ctx, &dbList, client.InNamespace(sc.Namespace)); err != nil {
		return err
	}

	cm := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      startupConfigMapName(sc.Name),
			Namespace: sc.Namespace,
		},
		Data: startupConfigFor(dbList.Items, sc.Name).configMapData(),
	}
	if err := controllerutil.SetControllerReference(sc, cm, r.Scheme); err != nil {
		return err
	}
	ac, err := applyConfiguration(cm)
	if err != nil {
		return err
	}
	return r.Apply(ctx, ac, client.ForceOwnership, client.FieldOwner(fieldManager))
}
