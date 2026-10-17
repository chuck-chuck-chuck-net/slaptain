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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// ADR-034: syncprovSessionlogSource=Accesslog has two preconditions that are
// cheap to state and expensive to discover later.
//
// The in-memory log must be gone, because the two together are what slapd
// itself calls wasteful — and because the field is STARTUP-ONLY: the operator
// never writes olcSpSessionlogSource to a running server, so a spec that asks
// for both resolves to one of them and the user would have no way to see which
// until a restart.
//
// And there must BE an accesslog database to source from. replication.enabled
// plus deltaSync is exactly the condition under which the operator creates
// cn=accesslog-<name>; without it the attribute would name a backend that is
// absent at startup, and syncprov_db_open returns -1 on that — an UNBOOTABLE
// pod (ADR-034 R4, the failure shape ADR-026 exists for). Admission is the only
// place to catch it where the cost is a rejected kubectl apply rather than a
// pod that comes back from its next restart dead.
//
// These specs run against the REAL CRD envtest installed, so they also answer
// whether the rules install at all: a CEL expression the API server rejects
// takes the whole SlapdDatabase schema down with it, and that shows up here as
// the VALID create failing.

var sessionlogSourceAdmissionSeq int

func sessionlogSourceNamespace(ctx context.Context) string {
	sessionlogSourceAdmissionSeq++
	ns := fmt.Sprintf("sessionlog-source-%d", sessionlogSourceAdmissionSeq)
	Expect(k8sClient.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	})).To(Succeed())
	return ns
}

// dbWithSessionlogSource builds an otherwise-valid SlapdDatabase carrying the
// given source, in-memory size (nil for omitted) and replication switches (nil
// for omitted, i.e. the CRD defaults of true).
func dbWithSessionlogSource(
	ns, source string, sessionlog *int32, enabled, deltaSync *bool,
) *ldapv1alpha1.SlapdDatabase {
	segment := int32(0)
	return &ldapv1alpha1.SlapdDatabase{
		ObjectMeta: metav1.ObjectMeta{Name: testDefaultDB, Namespace: ns},
		Spec: ldapv1alpha1.SlapdDatabaseSpec{
			ClusterRef: "slapd",
			Suffix:     "dc=example,dc=org",
			Replication: ldapv1alpha1.DatabaseReplicationConfig{
				Enabled:                  enabled,
				DeltaSync:                deltaSync,
				RIDSegment:               &segment,
				SyncprovSessionlog:       sessionlog,
				SyncprovSessionlogSource: source,
			},
		},
	}
}

var _ = Describe("syncprovSessionlogSource admission (ADR-034)", func() {
	ctx := context.Background()
	i := func(v int32) *int32 { return &v }
	b := func(v bool) *bool { return &v }

	It("admits a database that names no source at all", func() {
		// The proof that the rules INSTALLED: every existing SlapdDatabase in
		// the world omits this field, and a CEL expression the API server
		// refused would take this create down with it.
		ns := sessionlogSourceNamespace(ctx)
		Expect(k8sClient.Create(ctx, dbWithSessionlogSource(ns, "", nil, nil, nil))).To(Succeed())
	})

	It("admits an absent source alongside an in-memory sessionlog", func() {
		// The combination the first rule forbids is legal in every shape that
		// does not ask for the accesslog source — a rule that tripped on an
		// ABSENT field would break the common case.
		ns := sessionlogSourceNamespace(ctx)
		Expect(k8sClient.Create(ctx, dbWithSessionlogSource(ns, "", i(5000), nil, nil))).To(Succeed())
	})

	It("admits InMemory alongside an in-memory sessionlog", func() {
		ns := sessionlogSourceNamespace(ctx)
		Expect(k8sClient.Create(ctx, dbWithSessionlogSource(ns,
			ldapv1alpha1.SyncprovSessionlogSourceInMemory, i(5000), nil, nil))).To(Succeed())
	})

	It("admits Accesslog on a replicated delta-sync database", func() {
		ns := sessionlogSourceNamespace(ctx)
		Expect(k8sClient.Create(ctx, dbWithSessionlogSource(ns,
			ldapv1alpha1.SyncprovSessionlogSourceAccesslog, nil, nil, nil))).To(Succeed())
	})

	It("admits Accesslog with an explicit sessionlog of 0", func() {
		// 0 means "no in-memory log", which is precisely what the source
		// requires — refusing it would make the explicit spelling illegal
		// while the implicit one passes.
		ns := sessionlogSourceNamespace(ctx)
		Expect(k8sClient.Create(ctx, dbWithSessionlogSource(ns,
			ldapv1alpha1.SyncprovSessionlogSourceAccesslog, i(0), nil, nil))).To(Succeed())
	})

	It("admits Accesslog with replication and deltaSync set explicitly true", func() {
		ns := sessionlogSourceNamespace(ctx)
		Expect(k8sClient.Create(ctx, dbWithSessionlogSource(ns,
			ldapv1alpha1.SyncprovSessionlogSourceAccesslog, nil, b(true), b(true)))).To(Succeed())
	})

	It("refuses an unknown source value", func() {
		ns := sessionlogSourceNamespace(ctx)
		err := k8sClient.Create(ctx, dbWithSessionlogSource(ns, "accesslog", nil, nil, nil))
		Expect(err).To(HaveOccurred(),
			"the enum is the first line of defence: a typo must not resolve to the default silently")
	})

	It("refuses Accesslog together with a non-zero in-memory sessionlog", func() {
		ns := sessionlogSourceNamespace(ctx)
		err := k8sClient.Create(ctx, dbWithSessionlogSource(ns,
			ldapv1alpha1.SyncprovSessionlogSourceAccesslog, i(5000), nil, nil))
		Expect(err).To(HaveOccurred(),
			"asking for the accesslog source means the in-memory log is gone, not that it is also kept")
		Expect(err.Error()).To(ContainSubstring("syncprovSessionlog"),
			"the refusal must name the field to clear, not merely say the object is invalid")
	})

	It("refuses Accesslog on a database excluded from replication", func() {
		ns := sessionlogSourceNamespace(ctx)
		err := k8sClient.Create(ctx, dbWithSessionlogSource(ns,
			ldapv1alpha1.SyncprovSessionlogSourceAccesslog, nil, b(false), nil))
		Expect(err).To(HaveOccurred(),
			"a non-replicated database has no accesslog DB, so the source would name an absent backend")
		Expect(err.Error()).To(ContainSubstring("deltaSync"),
			"the refusal must name the two switches that produce the accesslog database")
	})

	It("refuses Accesslog on a database with deltaSync off", func() {
		// The half that the enabled check alone would miss: plain syncrepl
		// replicates happily and creates no change journal at all.
		ns := sessionlogSourceNamespace(ctx)
		err := k8sClient.Create(ctx, dbWithSessionlogSource(ns,
			ldapv1alpha1.SyncprovSessionlogSourceAccesslog, nil, nil, b(false)))
		Expect(err).To(HaveOccurred(),
			"plain syncrepl writes no accesslog, so there is nothing to source the session log from")
	})
})
