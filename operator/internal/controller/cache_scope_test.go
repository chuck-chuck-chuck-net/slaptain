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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The operator's informers are filtered to the objects it manages (ADR-042),
// which is only sound if every object it creates is selectable by the filter.
// An object the operator creates but cannot see again is one it would create
// twice, or never adopt, or — for a Secret it watches — never react to.
//
// The selector is spelled out here rather than taken from the code, so that a
// misspelling in the code's constant cannot move both sides of the check.
const (
	cacheScopeSelector  = "app.kubernetes.io/name=slapd"
	cacheScopeNamespace = "tenant"
	cacheScopePeerPW    = "peer"
)

// cacheScopeFixtureCluster is a minimal cluster for building every object the
// SlapdCluster and SlapdDatabase controllers create.

// cacheScopeFixtureCluster is a minimal cluster for building every object the
// SlapdCluster and SlapdDatabase controllers create.
func cacheScopeFixtureCluster() *ldapv1alpha1.SlapdCluster {
	return &ldapv1alpha1.SlapdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "ldap", Namespace: cacheScopeNamespace},
		Spec: ldapv1alpha1.SlapdClusterSpec{
			Replicas:     2,
			ReadReplicas: 1,
			Replication:  ldapv1alpha1.SlapdReplicationConfig{Enabled: true},
		},
	}
}

// cacheScopeScheme is the core + ldap scheme the builders set owner
// references against.

// cacheScopeScheme is the core + ldap scheme the builders set owner
// references against.
func cacheScopeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatalf("build scheme: %v", err)
	}
	if err := ldapv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add ldap scheme: %v", err)
	}
	return s
}

func TestManagedObjectsCarryTheCacheLabel(t *testing.T) {
	sel, err := labels.Parse(cacheScopeSelector)
	if err != nil {
		t.Fatalf("parse selector: %v", err)
	}
	sc := cacheScopeFixtureCluster()
	sc.Status.Restore = &ldapv1alpha1.SlapdClusterRestoreStatus{ID: "r1"}
	enabled := true
	sd := &ldapv1alpha1.SlapdDatabase{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: sc.Namespace},
		Spec: ldapv1alpha1.SlapdDatabaseSpec{
			ClusterRef:  sc.Name,
			Suffix:      sfxA,
			Replication: ldapv1alpha1.DatabaseReplicationConfig{Enabled: &enabled},
		},
	}
	sb := &ldapv1alpha1.SlapdBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: sc.Namespace},
		Spec:       ldapv1alpha1.SlapdBackupSpec{DatabaseRef: sd.Name},
	}
	cr := &SlapdClusterReconciler{}
	dr := &SlapdDatabaseReconciler{Scheme: cacheScopeScheme(t)}

	writeSvc, ok := cr.writeService(sc, 0, true)
	if !ok {
		t.Fatal("writeService returned no Service for a rotation-enabled fixture")
	}
	dirJob, err := dr.buildDirJob(sc, "ldap-0", "ldap-0-mkdir-abc", []string{"/data/db"}, false)
	if err != nil {
		t.Fatalf("buildDirJob: %v", err)
	}
	backupJob := buildBackupJob(sb, sd, sc, "init:tag", "operator:tag", "k/e/y.ldif.gz")
	restoreJob := buildRestoreJob(sc, sd, ldapv1alpha1.S3StorageSpec{}, "k/e/y.ldif.gz",
		"init:tag", "operator:tag", restorePodTarget{jobName: "ldap-0-restore", dataPVC: "data-ldap-0", configPVC: "config-ldap-0"})

	objects := map[string]client.Object{
		"headless Service":           cr.headlessService(sc),
		"client Service":             cr.clusterIPService(sc),
		"read-only headless Service": cr.readOnlyHeadlessService(sc),
		"read-only Service":          cr.readOnlyService(sc),
		"write Service":              writeSvc,
		"read-write StatefulSet":     cr.statefulSet(sc, false, ""),
		"read-only StatefulSet":      cr.statefulSet(sc, true, ""),
		"config-password Secret":     configPasswordSecret(sc, "pw"),
		"credentials Secret":         credentialsSecret(sc.Namespace, "db-credentials", "root", "repl"),
		"mkdir Job":                  dirJob,
		"backup Job":                 backupJob,
		"restore Job":                restoreJob,
	}
	for what, obj := range objects {
		if !sel.Matches(labels.Set(obj.GetLabels())) {
			t.Errorf("%s %q is not selected by %q: labels %v — the operator's cache would never hold it",
				what, obj.GetName(), cacheScopeSelector, obj.GetLabels())
		}
	}

	// A Job's pods are what the operator reads back (job_reap.go), and a pod
	// takes its labels from the Job's template, not from the Job.
	for what, tpl := range map[string]metav1.ObjectMeta{
		"mkdir Job pod":   dirJob.Spec.Template.ObjectMeta,
		"backup Job pod":  backupJob.Spec.Template.ObjectMeta,
		"restore Job pod": restoreJob.Spec.Template.ObjectMeta,
	} {
		if !sel.Matches(labels.Set(tpl.Labels)) {
			t.Errorf("%s template is not selected by %q: labels %v", what, cacheScopeSelector, tpl.Labels)
		}
	}
	for what, tpl := range map[string]metav1.ObjectMeta{
		"slapd pod":           cr.statefulSet(sc, false, "").Spec.Template.ObjectMeta,
		"read-only slapd pod": cr.statefulSet(sc, true, "").Spec.Template.ObjectMeta,
	} {
		if !sel.Matches(labels.Set(tpl.Labels)) {
			t.Errorf("%s template is not selected by %q: labels %v", what, cacheScopeSelector, tpl.Labels)
		}
	}
}

// A Secret the user supplies carries no label of ours, so the filtered cache
// does not hold it. Every Secret read must therefore go to the API server.
// The cache here is a client holding NOTHING; the API reader holds the
// Secrets. A read that consults the cache fails with NotFound.

// The config-password Secret is the one Secret the operator watches, so one
// created before the label must be labelled — and only when it is ours.
func TestLabelOwnedSecret(t *testing.T) {
	s := cacheScopeScheme(t)
	sc := cacheScopeFixtureCluster()
	sc.UID = "cluster-uid"
	owned := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ldap-config-password", Namespace: sc.Namespace}}
	if err := controllerutil.SetControllerReference(sc, owned, s); err != nil {
		t.Fatal(err)
	}
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ldap-config-password", Namespace: "other"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(sc, owned, foreign).Build()
	r := &SlapdClusterReconciler{Client: c, Scheme: s}
	ctx := context.Background()

	if err := r.labelOwnedSecret(ctx, sc, owned.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	got := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(owned), got); err != nil {
		t.Fatal(err)
	}
	if !managedSelector.Matches(labels.Set(got.Labels)) {
		t.Errorf("owned Secret not labelled: %v", got.Labels)
	}

	if err := r.labelOwnedSecret(ctx, sc, foreign.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(foreign), got); err != nil {
		t.Fatal(err)
	}
	if len(got.Labels) != 0 {
		t.Errorf("a Secret the operator does not own was labelled: %v", got.Labels)
	}
}
