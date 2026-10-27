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
	"maps"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// ADR-044: the manager caches a Secret, ConfigMap, Service, StatefulSet, Job or
// Pod only when it carries LabelOwned, so an object the operator writes
// without it drops out of every Owns() watch and every cached read. These
// tests run the real create paths against a fake API server and look at what
// landed, rather than at the builders, because the sites that write inline
// are the ones a builder-level test cannot see.

// byoSecretName is a Secret the user brought, which the operator only reads.
const byoSecretName = "byo"

func assertOperatorLabels(t *testing.T, what string, got map[string]string) {
	t.Helper()
	if got[labelManagedBy] != managedBy {
		t.Errorf("%s: %s = %q, want %q", what, labelManagedBy, got[labelManagedBy], managedBy)
	}
	if got[LabelOwned] != "true" {
		t.Errorf("%s: %s = %q, want \"true\"", what, LabelOwned, got[LabelOwned])
	}
}

func labelsTestCluster() *ldapv1alpha1.SlapdCluster {
	sc := goldenCluster()
	sc.UID = "11111111-1111-1111-1111-111111111111"
	return sc
}

func TestClusterControllerLabelsEverythingItWrites(t *testing.T) {
	ctx := context.Background()
	s := ridScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	r := goldenReconciler()
	r.Client = c
	r.Scheme = s
	sc := labelsTestCluster()

	steps := []struct {
		name string
		run  func(context.Context, *ldapv1alpha1.SlapdCluster) error
	}{
		{"reconcileSecret", r.reconcileSecret},
		{"reconcileHeadlessService", r.reconcileHeadlessService},
		{"reconcileClusterIPService", r.reconcileClusterIPService},
		{"reconcileDatabaseDirsConfigMap", r.reconcileDatabaseDirsConfigMap},
		{"reconcileStartupConfigMap", r.reconcileStartupConfigMap},
		{"reconcileStatefulSet", r.reconcileStatefulSet},
		{"reconcileReadOnlyHeadlessService", r.reconcileReadOnlyHeadlessService},
		{"reconcileReadOnlyService", r.reconcileReadOnlyService},
		{"reconcileReadOnlyStatefulSet", r.reconcileReadOnlyStatefulSet},
	}
	for _, st := range steps {
		if err := st.run(ctx, sc); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
	}

	var secrets corev1.SecretList
	var cms corev1.ConfigMapList
	var svcs corev1.ServiceList
	var stss appsv1.StatefulSetList
	for _, l := range []client.ObjectList{&secrets, &cms, &svcs, &stss} {
		if err := c.List(ctx, l); err != nil {
			t.Fatalf("list %T: %v", l, err)
		}
	}
	// Counted so an empty list cannot pass: 1 Secret, 2 ConfigMaps, 4 Services, 2 StatefulSets.
	if len(secrets.Items) != 1 || len(cms.Items) != 2 || len(svcs.Items) != 4 || len(stss.Items) != 2 {
		t.Fatalf("wrote %d Secrets, %d ConfigMaps, %d Services, %d StatefulSets; want 1, 2, 4, 2",
			len(secrets.Items), len(cms.Items), len(svcs.Items), len(stss.Items))
	}
	for _, o := range secrets.Items {
		assertOperatorLabels(t, "Secret "+o.Name, o.Labels)
	}
	for _, o := range cms.Items {
		assertOperatorLabels(t, "ConfigMap "+o.Name, o.Labels)
	}
	for _, o := range svcs.Items {
		assertOperatorLabels(t, "Service "+o.Name, o.Labels)
		if _, ok := o.Spec.Selector[LabelOwned]; ok {
			t.Errorf("Service %s: selector carries %s; selectors stay as they were", o.Name, LabelOwned)
		}
	}
	for _, o := range stss.Items {
		assertOperatorLabels(t, "StatefulSet "+o.Name, o.Labels)
		assertOperatorLabels(t, "StatefulSet "+o.Name+" pod template", o.Spec.Template.Labels)
		// A StatefulSet's selector is immutable: adding a label to it would
		// fail every apply against a cluster created by an older operator.
		want := selectorLabels(sc.Name)
		if o.Name != sc.Name {
			want = readOnlySelectorLabels(sc.Name)
		}
		if !maps.Equal(o.Spec.Selector.MatchLabels, want) {
			t.Errorf("StatefulSet %s: selector = %v, want %v", o.Name, o.Spec.Selector.MatchLabels, want)
		}
	}

	w, ok := r.writeService(sc, 0, true)
	if !ok {
		t.Fatal("writeService built nothing")
	}
	assertOperatorLabels(t, "Service "+w.Name, w.Labels)
}

func TestDatabaseControllerLabelsEverythingItWrites(t *testing.T) {
	ctx := context.Background()
	s := ridScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	r := &SlapdDatabaseReconciler{
		Client:          c,
		Scheme:          s,
		ClusterDomain:   testClusterDomain,
		DefaultImageTag: testOperatorVersion,
		OperatorImage:   goldenOperatorImage,
	}
	sc := labelsTestCluster()
	sd := &ldapv1alpha1.SlapdDatabase{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: sc.Namespace}}

	if err := r.reconcileCredentials(ctx, sd, sc); err != nil {
		t.Fatalf("reconcileCredentials: %v", err)
	}
	var sec corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Name: "db-credentials", Namespace: sc.Namespace}, &sec); err != nil {
		t.Fatalf("get generated credentials: %v", err)
	}
	assertOperatorLabels(t, "Secret db-credentials", sec.Labels)

	job, err := r.buildDirJob(sc, sc.Name+"-0", "slapd-0-mkdir-x", []string{"/data/db"}, false)
	if err != nil {
		t.Fatalf("buildDirJob: %v", err)
	}
	assertJobLabels(t, job)
}

func TestBackupAndRestoreJobsCarryTheLabels(t *testing.T) {
	sc := labelsTestCluster()
	sd := &ldapv1alpha1.SlapdDatabase{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: sc.Namespace}}
	sb := &ldapv1alpha1.SlapdBackup{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: sc.Namespace}}
	sc.Status.Restore = &ldapv1alpha1.SlapdClusterRestoreStatus{ID: "r1"}

	assertJobLabels(t, buildBackupJob(sb, sd, sc, "init:1", goldenOperatorImage, "key"))
	assertJobLabels(t, buildRestoreJob(sc, sd, ldapv1alpha1.S3StorageSpec{}, "key", "init:1", goldenOperatorImage,
		restorePodTarget{jobName: "slapd-0-restore", dataPVC: "data-slapd-0", loadData: true}))
}

// assertJobLabels checks the Job and its pod template: jobPods lists a failed
// Job's pods through the Pod cache, which holds only labelled pods.
func assertJobLabels(t *testing.T, job *batchv1.Job) {
	t.Helper()
	assertOperatorLabels(t, "Job "+job.Name, job.Labels)
	assertOperatorLabels(t, "Job "+job.Name+" pod template", job.Spec.Template.Labels)
}

// The filter must cover exactly the core kinds. Filtering a slaptain CR kind
// would make every CR a user applied without the label stop reconciling.
func TestCacheOptionsFilterTheCoreKindsOnly(t *testing.T) {
	opts := CacheOptions()
	labelled := labels.Set{LabelOwned: "true"}
	unlabelled := labels.Set{labelManagedBy: managedBy}

	want := map[string]bool{
		"*v1.Secret": true, "*v1.ConfigMap": true, "*v1.Service": true,
		"*v1.StatefulSet": true, "*v1.Job": true, "*v1.Pod": true,
	}
	if len(opts.ByObject) != len(want) {
		t.Errorf("ByObject has %d kinds, want %d", len(opts.ByObject), len(want))
	}
	for obj, by := range opts.ByObject {
		kind := typeName(obj)
		if !want[kind] {
			t.Errorf("ByObject filters %s, which is not one of the six core kinds", kind)
			continue
		}
		if by.Label == nil || !by.Label.Matches(labelled) || by.Label.Matches(unlabelled) {
			t.Errorf("%s: selector %v must match %s=true and nothing without it", kind, by.Label, LabelOwned)
		}
		delete(want, kind)
	}
	for kind := range want {
		t.Errorf("ByObject does not filter %s", kind)
	}
	if opts.DefaultLabelSelector != nil {
		t.Errorf("DefaultLabelSelector = %v; it would filter the slaptain CRs too", opts.DefaultLabelSelector)
	}
}

// A user's Secret has no label, so the cache never holds it; the read has to
// reach the API server. CR kinds are cached unfiltered, so a miss there is a
// real NotFound and must not cost a second request.
func TestFallbackClientReadsAFilteredMissFromTheAPI(t *testing.T) {
	ctx := context.Background()
	s := ridScheme(t)
	userSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "user", Namespace: "ns"}}
	sc := &ldapv1alpha1.SlapdCluster{ObjectMeta: metav1.ObjectMeta{Name: "sc", Namespace: "ns"}}
	cached := fake.NewClientBuilder().WithScheme(s).Build()
	api := fake.NewClientBuilder().WithScheme(s).WithObjects(userSecret, sc).Build()
	c := NewCacheFallbackClient(cached, api)

	if err := c.Get(ctx, client.ObjectKeyFromObject(userSecret), &corev1.Secret{}); err != nil {
		t.Errorf("Get of an unlabelled Secret: %v, want it read from the API", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(sc), &ldapv1alpha1.SlapdCluster{}); !apierrors.IsNotFound(err) {
		t.Errorf("Get of an uncached SlapdCluster: %v, want the cache's NotFound", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: "absent", Namespace: "ns"}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("Get of a Secret that exists nowhere: %v, want NotFound", err)
	}
}

// A Secret an older operator generated has no label. The operator adds the
// labels only where ownership is on record, a controller reference to the CR.
// <db>-credentials carries none, and mesh-share-credentials.sh creates one under
// the same name, so an existing one is left alone and read through the fallback.
func TestGeneratedSecretsGetLabelledUserSecretsDoNot(t *testing.T) {
	ctx := context.Background()
	s := ridScheme(t)
	sc := labelsTestCluster()
	old := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: sc.Name + "-config-password", Namespace: sc.Namespace},
		Data:       map[string][]byte{secretKeyRootPassword: []byte("kept")},
	}
	oldDB := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: sc.Namespace},
		Data:       map[string][]byte{secretKeyRootPassword: []byte("kept")},
	}
	user := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: byoSecretName, Namespace: sc.Namespace},
		Data: map[string][]byte{
			secretKeyRootPassword:        []byte("mine"),
			secretKeyReplicationPassword: []byte("mine"),
		},
	}
	if err := controllerutil.SetControllerReference(sc, old, s); err != nil {
		t.Fatalf("own config-password: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(old, oldDB, user).Build()

	cr := goldenReconciler()
	cr.Client, cr.Scheme = c, s
	if err := cr.reconcileSecret(ctx, sc); err != nil {
		t.Fatalf("reconcileSecret: %v", err)
	}
	dr := &SlapdDatabaseReconciler{Client: c, Scheme: s}
	if err := dr.reconcileCredentials(ctx, &ldapv1alpha1.SlapdDatabase{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: sc.Namespace},
	}, sc); err != nil {
		t.Fatalf("reconcileCredentials (generated): %v", err)
	}
	byo := &ldapv1alpha1.SlapdDatabase{ObjectMeta: metav1.ObjectMeta{Name: "db2", Namespace: sc.Namespace}}
	byo.Spec.Credentials.SecretName = byoSecretName
	if err := dr.reconcileCredentials(ctx, byo, sc); err != nil {
		t.Fatalf("reconcileCredentials (user): %v", err)
	}
	scBYO := labelsTestCluster()
	scBYO.Spec.LDAP.CnConfigCredentials.SecretName = byoSecretName
	if err := cr.reconcileSecret(ctx, scBYO); err != nil {
		t.Fatalf("reconcileSecret (user): %v", err)
	}

	var got corev1.Secret
	if err := c.Get(ctx, client.ObjectKeyFromObject(old), &got); err != nil {
		t.Fatalf("get %s: %v", old.Name, err)
	}
	assertOperatorLabels(t, "Secret "+old.Name, got.Labels)
	if string(got.Data[secretKeyRootPassword]) != "kept" {
		t.Errorf("Secret %s: root-password changed; labelling must not touch data", old.Name)
	}
	for _, o := range []*corev1.Secret{oldDB, user} {
		var got corev1.Secret
		if err := c.Get(ctx, client.ObjectKeyFromObject(o), &got); err != nil {
			t.Fatalf("get %s: %v", o.Name, err)
		}
		if len(got.Labels) != 0 {
			t.Errorf("Secret %s gained labels %v; it has no controller reference to say the operator owns it",
				o.Name, got.Labels)
		}
	}
}

// A <name>-config-password the user created by hand, without setting
// cnConfigCredentials, has no controller reference and must stay untouched.
func TestUnownedConfigPasswordIsNotLabelled(t *testing.T) {
	ctx := context.Background()
	s := ridScheme(t)
	sc := labelsTestCluster()
	handMade := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: sc.Name + "-config-password", Namespace: sc.Namespace}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(handMade).Build()
	r := goldenReconciler()
	r.Client, r.Scheme = c, s
	if err := r.reconcileSecret(ctx, sc); err != nil {
		t.Fatalf("reconcileSecret: %v", err)
	}
	var got corev1.Secret
	if err := c.Get(ctx, client.ObjectKeyFromObject(handMade), &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Labels) != 0 {
		t.Errorf("hand-made %s gained labels %v", handMade.Name, got.Labels)
	}
}

func typeName(obj client.Object) string {
	switch obj.(type) {
	case *corev1.Secret:
		return "*v1.Secret"
	case *corev1.ConfigMap:
		return "*v1.ConfigMap"
	case *corev1.Service:
		return "*v1.Service"
	case *appsv1.StatefulSet:
		return "*v1.StatefulSet"
	case *batchv1.Job:
		return "*v1.Job"
	case *corev1.Pod:
		return "*v1.Pod"
	}
	return "other"
}
