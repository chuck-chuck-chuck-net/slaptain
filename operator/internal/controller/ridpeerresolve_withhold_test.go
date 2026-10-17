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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The spec that was blocked until reconcileReplication's return shape changed.
//
// judgePeerResolutions' unit table proves the VERDICT. It cannot prove that the
// verdict is reached, carried out of reconcileReplication, and published — and
// until M4b that was unprovable at any seam: reconcileReplication returned
// (bool, error), so a withhold and an ordinary pass that merely could not
// reach its pods were the same `(true, nil)`. The implementer widened it to
// (bool, *ridSegmentVerdict, error), which separates "nothing converged
// because a peer is unresolvable — go fix it" from "nothing converged because
// the pods are not up yet — wait". This spec is what holds that apart.
//
// WHAT IT DOES NOT PROVE, said plainly rather than implied by silence:
//
//   - That the pod's existing olcSyncRepl survives the withhold. That is the
//     actual hazard-2 property and it lives in a live cn=config. envtest runs
//     an API server and no slapd, so there is nothing to leave alone and an
//     assertion would be vacuously green. RID-PLAN M7.
//   - That the phase is Degraded. databasePhase ranks Error/NoPodsReachable
//     above every refusal, and the fixture has no pods at all, so every
//     database here ends Error whatever was judged. The phase claim is pinned
//     where it is assertable — TestDatabasePhasePrecedence, which is pure and
//     takes the pod tallies as arguments. Asserting Degraded here would mean
//     weakening the precedence to make a test pass, which is the wrong
//     direction entirely.
//
// What it DOES prove is the whole of the wiring: the Secret read fails, the
// resolution refuses, the refusal survives the return, and it lands on
// SyncreplStanzasReady with the reason that names the cause. Revert the
// verdict to a bare bool at the call site and this goes red.

// withExternalPeer puts one podAddresses-mode external peer on the fixture's
// cluster, binding with a password from `secretName`.
//
// podAddresses rather than discovery on purpose: it resolves to an address
// without any remote cluster, so the ONLY thing that can fail is the Secret
// read. A discovery peer would refuse for want of providers too, and the spec
// could not then tell which of the two causes it had actually observed.
//
// consumer-only (ADR-010) for a reason worth stating, because it is the one
// fixture choice here that is not cosmetic. Step 9 of the reconcile is
// ADR-027's identity gate: a peer-mode cluster writes no stanza at all until
// every READ-WRITE pod carries cn=repl-<db>,cn=slaptain-auth, and this fixture
// has no pods, so a peer-mode cluster never reaches the external-peer
// resolution to begin with — the spec would go red on a gate that fires first
// and prove nothing about M4b. A consumer-only cluster writes no in-cluster
// stanza that binds as the identity, so the gate is skipped by design, and its
// ONLY stanzas are the external ones. That makes it not a workaround but the
// sharpest case there is: every stanza this database has is at stake in the
// peer that cannot be resolved.
func (f *ridFixture) withExternalPeer(ctx context.Context, peerName, secretName string) {
	sc := &ldapv1alpha1.SlapdCluster{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.cluster, Namespace: f.ns}, sc)).To(Succeed())
	sc.Spec.Replication.Mode = replicationModeConsumerOnly
	sc.Spec.Replication.ExternalPeers = []ldapv1alpha1.ExternalPeer{{
		Name:                   peerName,
		SiteIndex:              new(int32(1)),
		PodAddresses:           []string{"192.0.2.10"},
		BindPasswordSecretName: secretName,
	}}
	Expect(k8sClient.Update(ctx, sc)).To(Succeed())

	// The cluster's phase lives on the status subresource and does not survive
	// a spec update round-trip in the fixture's own hand-written form, so it is
	// re-stamped: the database reconcile refuses to proceed past step 4 against
	// a cluster that is not Running, and would never reach the peer loop.
	sc.Status.Phase = ldapv1alpha1.PhaseRunning
	Expect(k8sClient.Status().Update(ctx, sc)).To(Succeed())
}

// createPeerBindSecret creates the Secret an external peer's
// bindPasswordSecretName names, in the form the resolution reads it.
func createPeerBindSecret(ctx context.Context, ns, name string) {
	Expect(k8sClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		StringData: map[string]string{"password": "not-a-real-password"},
	})).To(Succeed())
}

var _ = Describe("External peer withhold (ADR-031 M4b, RID-PLAN hazard 2)", func() {
	ctx := context.Background()

	It("publishes a peer refusal on SyncreplStanzasReady, naming the peer and the Secret", func() {
		f := newRIDFixture(ctx)
		// No Secret of this name is ever created. That is the whole fixture:
		// a peer whose bind-password Secret cannot be read is one of the three
		// paths that used to `continue`, handing the atomic Replace a stanza
		// set with this peer's link missing — a deletion of live cross-site
		// replication, reported nowhere.
		f.withExternalPeer(ctx, "site-2", "site-2-repl")
		Expect(f.createDB(ctx, "alpha", new(int32(3)))).To(Succeed())

		f.reconcile(ctx, "alpha")

		expectRefusal(f.get(ctx, "alpha"), reasonPeerBindPasswordUnreadable,
			"site-2",      // which peer could not be resolved
			"site-2-repl", // and which Secret a human goes and looks at
		)
	})

	It("does not refuse a peer whose Secret is readable, on an equally podless pass", func() {
		// The over-refusal guard, and the contrast the old return shape could
		// not express. Both of these passes reach exactly zero pods and both
		// converge nothing; before M4b both left reconcileReplication as the
		// same `(true, nil)` and were reported identically, so a human whose
		// cross-site link had just been deleted read the same "still pending"
		// they read on every bringup.
		//
		// The two fixtures below differ in ONE fact — whether the Secret the
		// peer names exists — and that fact alone must change what is
		// published. Note what is deliberately not asserted: the phase.
		// Neither fixture has a pod, so both end Error/NoPodsReachable, which
		// is correct (databasePhase rank 1: a database no pod answers on has a
		// louder problem than its spec) and is pinned where it is assertable,
		// in TestDatabasePhasePrecedence.
		readable := newRIDFixture(ctx)
		createPeerBindSecret(ctx, readable.ns, "site-2-repl")
		readable.withExternalPeer(ctx, "site-2", "site-2-repl")
		Expect(readable.createDB(ctx, "alpha", new(int32(3)))).To(Succeed())
		readable.reconcile(ctx, "alpha")

		broken := newRIDFixture(ctx)
		broken.withExternalPeer(ctx, "site-2", "site-2-repl")
		Expect(broken.createDB(ctx, "alpha", new(int32(3)))).To(Succeed())
		broken.reconcile(ctx, "alpha")

		ok := ridCondition(readable.get(ctx, "alpha"))
		Expect(ok).NotTo(BeNil(), "no %s condition on the database whose peer resolves",
			ridSegmentConditionType)
		Expect(ok.Reason).NotTo(Equal(reasonPeerBindPasswordUnreadable),
			"a peer whose Secret was read reports it unreadable: the refusal is being "+
				"manufactured rather than observed, and every healthy mesh would stop converging")

		bad := ridCondition(broken.get(ctx, "alpha"))
		Expect(bad).NotTo(BeNil(), "no %s condition on the database whose peer is unresolvable",
			ridSegmentConditionType)
		Expect(bad.Reason).To(Equal(reasonPeerBindPasswordUnreadable),
			"the two passes report the same reason %q, though one of them silently stopped "+
				"replicating a site. An unresolvable peer withholds every stanza of this database — "+
				"here that is ALL of them — and that is not something a human waits out",
			bad.Reason)
		Expect(string(bad.Status)).To(Equal(string(metav1.ConditionFalse)))
	})

	It("lifts the withhold once the bind-password Secret can be read", func() {
		f := newRIDFixture(ctx)
		f.withExternalPeer(ctx, "site-2", "site-2-repl")
		Expect(f.createDB(ctx, "alpha", new(int32(3)))).To(Succeed())

		f.reconcile(ctx, "alpha")
		expectRefusal(f.get(ctx, "alpha"), reasonPeerBindPasswordUnreadable, "site-2")

		// A withhold that never lifts is as bad as one that writes. Nothing is
		// stored, so there is nothing to release — this is what proves it for
		// the peer half, as the collision spec does for the segment half.
		createPeerBindSecret(ctx, f.ns, "site-2-repl")

		f.reconcile(ctx, "alpha")

		c := ridCondition(f.get(ctx, "alpha"))
		Expect(c).NotTo(BeNil(), "no %s condition after the Secret appeared", ridSegmentConditionType)
		Expect(c.Reason).NotTo(Equal(reasonPeerBindPasswordUnreadable),
			"%s still reads %s — %q — after the Secret the message named was created. "+
				"The human did exactly what they were told and the mesh stayed frozen",
			ridSegmentConditionType, c.Reason, c.Message)
	})
})
