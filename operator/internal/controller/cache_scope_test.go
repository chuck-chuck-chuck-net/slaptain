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
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
func TestSecretReadsGoToTheAPIServerNotTheCache(t *testing.T) {
	s := cacheScopeScheme(t)
	sc := cacheScopeFixtureCluster()
	sc.Spec.LDAP.TLS = ldapv1alpha1.SlapdTLSConfig{Enabled: true, SecretName: "user-tls"}
	sc.Spec.LDAP.CnConfigCredentials.SecretName = "user-config"
	sd := &ldapv1alpha1.SlapdDatabase{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: sc.Namespace},
		Spec: ldapv1alpha1.SlapdDatabaseSpec{
			ClusterRef:  sc.Name,
			Suffix:      sfxA,
			Credentials: ldapv1alpha1.DatabaseCredentials{SecretName: "user-creds"},
		},
	}
	secret := func(name string, data map[string]string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: sc.Namespace},
			Data:       mapBytes(data),
		}
	}
	apiReader := fake.NewClientBuilder().WithScheme(s).WithObjects(
		secret("user-tls", map[string]string{"tls.crt": "c", "tls.key": "k"}),
		secret("user-config", map[string]string{secretKeyRootPassword: "cfg"}),
		secret("user-creds", map[string]string{secretKeyRootPassword: "root", secretKeyReplicationPassword: "repl"}),
		secret("user-s3", map[string]string{"access-key-id": "id", "secret-access-key": "sk"}),
		secret("user-peer", map[string]string{"password": cacheScopePeerPW}),
	).Build()
	// The cache: CRs only, no Secret at all.
	cache := fake.NewClientBuilder().WithScheme(s).WithObjects(sc, sd).WithStatusSubresource(sc, sd).Build()

	cr := &SlapdClusterReconciler{Client: cache, APIReader: apiReader, Scheme: s}
	dr := &SlapdDatabaseReconciler{Client: cache, APIReader: apiReader, Scheme: s}
	sr := &SlapdSchemaReconciler{Client: cache, APIReader: apiReader, Scheme: s}
	br := &SlapdBackupReconciler{Client: cache, APIReader: apiReader, Scheme: s}
	sbr := &SlapdScheduledBackupReconciler{Client: cache, APIReader: apiReader, Scheme: s}
	ctx := context.Background()

	reads := map[string]func() (string, error){
		"cluster: cn=config password": func() (string, error) { return cr.getConfigPassword(ctx, sc) },
		"cluster: TLS gate": func() (string, error) {
			ok, err := cr.reconcileTLSGate(ctx, sc)
			if err != nil {
				return "", err
			}
			if !ok {
				return "", fmt.Errorf("TLS gate withheld the StatefulSet: %+v", sc.Status.Conditions)
			}
			return "ok", nil
		},
		"cluster: CSN credentials": func() (string, error) {
			infos := cr.listDatabaseInfo(ctx, sc)
			if len(infos) != 1 || infos[0].bindPW == "" {
				return "", fmt.Errorf("no replication password read: %+v", infos)
			}
			return infos[0].bindPW, nil
		},
		"cluster: restore replication password": func() (string, error) { return cr.databaseReplPassword(ctx, sd) },
		"scheduled backup: S3 credentials": func() (string, error) {
			cfg, err := sbr.s3Config(ctx, &ldapv1alpha1.SlapdBackup{
				ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: sc.Namespace},
				Spec: ldapv1alpha1.SlapdBackupSpec{
					DatabaseRef: sd.Name,
					Storage:     ldapv1alpha1.S3StorageSpec{CredentialsSecretName: "user-s3"},
				},
			})
			return cfg.AccessKeyID, err
		},
		"database: root password":        func() (string, error) { return dr.getDatabaseRootPassword(ctx, sd) },
		"database: replication password": func() (string, error) { return dr.getDatabaseReplPassword(ctx, sd) },
		"database: cn=config password":   func() (string, error) { return dr.getClusterConfigPassword(ctx, sc) },
		"database: external peer password": func() (string, error) {
			return dr.externalPeerPassword(ctx, sc.Namespace, "user-peer")
		},
		"database: bring-your-own credentials check": func() (string, error) {
			return "ok", dr.reconcileCredentials(ctx, sd, sc)
		},
		"schema: cn=config password": func() (string, error) { return sr.getConfigPassword(ctx, sc) },
		"backup: replication password": func() (string, error) {
			return br.replicationPassword(ctx, sc.Namespace, sd)
		},
	}
	for what, read := range reads {
		got, err := read()
		if err != nil {
			t.Errorf("%s: read through the cache, which holds no user Secret: %v", what, err)
			continue
		}
		if got == "" {
			t.Errorf("%s: read nothing", what)
		}
	}
}

func mapBytes(m map[string]string) map[string][]byte {
	out := make(map[string][]byte, len(m))
	for k, v := range m {
		out[k] = []byte(v)
	}
	return out
}

// Every kind some controller Owns() or reads through the cache gets an
// informer, and an unfiltered informer holds the whole cluster's objects of
// that kind. The option set must name each of them with the selector.
func TestCacheOptionsFilterEveryCachedKind(t *testing.T) {
	sel, err := labels.Parse(cacheScopeSelector)
	if err != nil {
		t.Fatalf("parse selector: %v", err)
	}
	opts := CacheOptions()
	want := []client.Object{
		&corev1.Secret{}, &corev1.Service{}, &corev1.Pod{}, &appsv1.StatefulSet{}, &batchv1.Job{},
	}
	for _, kind := range want {
		var found bool
		for obj, by := range opts.ByObject {
			if fmt.Sprintf("%T", obj) != fmt.Sprintf("%T", kind) {
				continue
			}
			found = true
			if by.Label == nil || by.Label.String() != sel.String() {
				t.Errorf("%T: informer filtered by %v, want %q", kind, by.Label, cacheScopeSelector)
			}
		}
		if !found {
			t.Errorf("%T: no cache filter — the informer would hold every one in the cluster", kind)
		}
	}
	if opts.DefaultLabelSelector != nil {
		t.Errorf("a default selector would filter the CRDs too, which carry no label of ours: %v", opts.DefaultLabelSelector)
	}
}

// An object created by an operator that predates the label is absent from the
// filtered cache until this pass's apply labels it. The read must not take
// that absence for a first create.
func TestManagedGetAsksTheAPIServerWhenTheCacheSaysNotFound(t *testing.T) {
	s := cacheScopeScheme(t)
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "ldap", Namespace: cacheScopeNamespace}}
	sts.Spec.ServiceName = "ldap-headless"
	cache := fake.NewClientBuilder().WithScheme(s).Build()
	api := fake.NewClientBuilder().WithScheme(s).WithObjects(sts).Build()
	key := client.ObjectKeyFromObject(sts)

	got := &appsv1.StatefulSet{}
	if err := managedGet(context.Background(), cache, api, key, got); err != nil {
		t.Fatalf("an unlabelled, pre-existing StatefulSet read as missing: %v", err)
	}
	if got.Spec.ServiceName != sts.Spec.ServiceName {
		t.Errorf("read %+v, want the API server's copy", got.Spec)
	}

	// A genuinely missing object is still NotFound, from the API server.
	if err := managedGet(context.Background(), cache, api,
		client.ObjectKey{Name: "nope", Namespace: cacheScopeNamespace}, &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
		t.Errorf("missing object: got %v, want NotFound", err)
	}
}

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
