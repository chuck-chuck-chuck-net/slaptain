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

// The per-database LMDB directories a cluster's pods must carry, and how the
// operator tells them apart from the pod template (ADR-013 amendment of
// 2026-09-28, option H).
//
// back-mdb does not create olcDbDirectory, so the directory has to exist before
// the database can be added to a pod's cn=config. That list used to ride as the
// DATABASE_DIRS env var on the init container — a pod-template value, so every
// database add or remove re-rendered the template and the StatefulSet replaced
// every pod for an operation that needs no restart at all. It now lives in a
// ConfigMap mounted into the init container: referenced by name, so the
// template is invariant under the database set, and read at pod start, where a
// minute of kubelet propagation lag is invisible.
//
// Already-running pods are served by the mkdir Job (dirjob.go), not by this.

const (
	// databaseDirsVolume is the name of the projected ConfigMap volume.
	databaseDirsVolume = "database-dirs"
	// databaseDirsMountPath is where the init container finds the two lists.
	databaseDirsMountPath = "/etc/slaptain/database-dirs"
	// databaseDirsDataKey and databaseDirsAccesslogKey are the ConfigMap keys,
	// and therefore the file names under the mount path.
	databaseDirsDataKey      = "data-dirs"
	databaseDirsAccesslogKey = "accesslog-dirs"
)

// databaseDirsConfigMapName is the ConfigMap holding a cluster's directory
// lists. One per SlapdCluster, owned by it, shared by both StatefulSets.
func databaseDirsConfigMapName(clusterName string) string {
	return clusterName + "-database-dirs"
}

// databaseDirs is the set of directories to create under /data and under
// /accesslog. Two lists rather than one, because they are not the same list:
// a database's data directory is spec.dataDirectory when set and the CR name
// otherwise, while its accesslog directory is always keyed on the CR name
// (ADR-019 R1, which needs it to match the derived cn=accesslog-<dbname>).
type databaseDirs struct {
	Data      []string
	Accesslog []string
}

// databaseDirsFor computes the lists for one cluster. Sorted and deduplicated,
// so the rendered ConfigMap is a stable fingerprint of the database set and an
// unchanged set produces a byte-identical write.
func databaseDirsFor(dbs []ldapv1alpha1.SlapdDatabase, clusterName string) databaseDirs {
	data := map[string]struct{}{}
	accesslog := map[string]struct{}{}
	for i := range dbs {
		sd := &dbs[i]
		if sd.Spec.ClusterRef != clusterName {
			continue
		}
		data[dataDirName(sd)] = struct{}{}
		accesslog[sd.Name] = struct{}{}
	}
	return databaseDirs{Data: sortedKeys(data), Accesslog: sortedKeys(accesslog)}
}

// dataDirName is the effective /data subdirectory of one database — the single
// definition, so the directory that gets created and the olcDbDirectory that
// gets written can never disagree.
func dataDirName(sd *ldapv1alpha1.SlapdDatabase) string {
	if sd.Spec.DataDirectory != "" {
		return sd.Spec.DataDirectory
	}
	return sd.Name
}

func sortedKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// configMapData renders the lists as the two files the init container reads.
// Newline-separated: a directory name is an RFC 1123 label, so it cannot
// contain one, and a line-oriented file is what a shell reads without parsing.
func (d databaseDirs) configMapData() map[string]string {
	render := func(v []string) string {
		if len(v) == 0 {
			return ""
		}
		return strings.Join(v, "\n") + "\n"
	}
	return map[string]string{
		databaseDirsDataKey:      render(d.Data),
		databaseDirsAccesslogKey: render(d.Accesslog),
	}
}

// reconcileDatabaseDirsConfigMap publishes the cluster's directory lists.
//
// Owned by the SlapdCluster, so it is garbage-collected with it, and applied
// with SSA like every other resource this controller owns. Content-addressed by
// nothing: the lists are sorted and deduplicated, so an unchanged database set
// produces a byte-identical object and the apply is a no-op (ADR-001).
func (r *SlapdClusterReconciler) reconcileDatabaseDirsConfigMap(
	ctx context.Context, sc *ldapv1alpha1.SlapdCluster,
) error {
	var dbList ldapv1alpha1.SlapdDatabaseList
	if err := r.List(ctx, &dbList, client.InNamespace(sc.Namespace)); err != nil {
		return err
	}

	cm := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      databaseDirsConfigMapName(sc.Name),
			Namespace: sc.Namespace,
			Labels:    managedLabels(),
		},
		Data: databaseDirsFor(dbList.Items, sc.Name).configMapData(),
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
