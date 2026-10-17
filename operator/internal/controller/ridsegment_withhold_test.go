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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// ADR-031 decision 6 is M3's headline claim: a database whose ridSegment is
// refused writes NO syncrepl stanzas and says why. judgeRIDSegment's unit
// tests pin the verdict; these specs pin that the verdict reaches a real
// object through a real reconcile — the wiring, which is the half a pure test
// cannot see.
//
// WHAT THESE SPECS CANNOT PROVE, stated here rather than implied by silence.
// RID-PLAN hazard 2's actual property is "a withhold leaves the pod's existing
// olcSyncRepl alone", because olcSyncRepl is installed with one atomic
// Replace and a short set is a deletion of live replication. envtest runs an
// API server and no slapd, so there is no cn=config anywhere to leave alone
// and no stanza to count. An assertion here would be vacuously green. That
// half belongs to an e2e spec against real pods (docs/RID-PLAN.md M7) and is
// recorded as untested until then.
//
// What these specs DO pin, and it is the regression most likely to be
// introduced: the refusal is computed and published on a cluster whose pods
// are unreachable. Every one of these clusters has zero live pods, so if the
// check is ever moved inside reconcileReplication — i.e. behind the healthy-pod
// gate — the condition vanishes in exactly the situation where withholding
// matters most, and these specs go red.

// ridFixture is one namespace holding a Running SlapdCluster and the
// reconciler under test. No pods exist; the cluster's phase is written
// directly, which is what lets the SlapdDatabase reconcile proceed past step 4
// without a StatefulSet.
type ridFixture struct {
	ns         string
	cluster    string
	reconciler *SlapdDatabaseReconciler
}

var ridFixtureSeq int

func newRIDFixture(ctx context.Context) *ridFixture {
	ridFixtureSeq++
	f := &ridFixture{
		ns:      fmt.Sprintf("rid-withhold-%d", ridFixtureSeq),
		cluster: "slapd",
		reconciler: &SlapdDatabaseReconciler{
			Client:        k8sClient,
			Scheme:        k8sClient.Scheme(),
			ClusterDomain: "cluster.local",
		},
	}

	Expect(k8sClient.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: f.ns},
	})).To(Succeed())

	// The cn=config admin password the reconcile reads at step 6. Pre-created
	// because the SlapdCluster controller, which normally generates it, is not
	// running here.
	Expect(k8sClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: f.cluster + "-config-password", Namespace: f.ns},
		StringData: map[string]string{"root-password": "not-a-real-password"},
	})).To(Succeed())

	sc := &ldapv1alpha1.SlapdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: f.cluster, Namespace: f.ns},
		Spec: ldapv1alpha1.SlapdClusterSpec{
			Replicas: 1,
			Replication: ldapv1alpha1.SlapdReplicationConfig{
				Enabled: true,
			},
		},
	}
	Expect(k8sClient.Create(ctx, sc)).To(Succeed())

	sc.Status.Phase = ldapv1alpha1.PhaseRunning
	Expect(k8sClient.Status().Update(ctx, sc)).To(Succeed())

	return f
}

// createDB declares a replicating SlapdDatabase on the fixture's cluster.
// segment nil means "declare none", which the CEL rule on SlapdDatabaseSpec is
// expected to refuse — createDB returns the Create error rather than asserting
// on it, so a spec can pin the refusal.
func (f *ridFixture) createDB(ctx context.Context, name string, segment *int32) error {
	enabled := true
	return k8sClient.Create(ctx, &ldapv1alpha1.SlapdDatabase{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns},
		Spec: ldapv1alpha1.SlapdDatabaseSpec{
			ClusterRef: f.cluster,
			Suffix:     fmt.Sprintf("dc=%s,dc=test", name),
			Replication: ldapv1alpha1.DatabaseReplicationConfig{
				Enabled:    &enabled,
				RIDSegment: segment,
			},
		},
	})
}

func (f *ridFixture) reconcile(ctx context.Context, name string) {
	_, err := f.reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: f.ns},
	})
	Expect(err).NotTo(HaveOccurred(),
		"a refused RID segment is a reported state, not a controller error: returning an error "+
			"would retry on the exponential backoff and bury the reason in operator logs")
}

func (f *ridFixture) get(ctx context.Context, name string) *ldapv1alpha1.SlapdDatabase {
	sd := &ldapv1alpha1.SlapdDatabase{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: f.ns}, sd)).To(Succeed())
	return sd
}

// ridCondition returns the SyncreplStanzasReady condition, or nil when the reconcile
// never published one.
func ridCondition(sd *ldapv1alpha1.SlapdDatabase) *metav1.Condition {
	for i := range sd.Status.Conditions {
		if sd.Status.Conditions[i].Type == ridSegmentConditionType {
			return &sd.Status.Conditions[i]
		}
	}
	return nil
}

// expectRefusal asserts the database carries a False SyncreplStanzasReady with the
// given reason, and that its message contains every one of `naming`.
func expectRefusal(sd *ldapv1alpha1.SlapdDatabase, reason string, naming ...string) {
	c := ridCondition(sd)
	ExpectWithOffset(1, c).NotTo(BeNil(),
		"no %s condition on %q: the refusal was never published, so a human has nowhere to read it",
		ridSegmentConditionType, sd.Name)
	ExpectWithOffset(1, string(c.Status)).To(Equal(string(metav1.ConditionFalse)),
		"%s on %q is %s; a refused segment must report False", ridSegmentConditionType, sd.Name, c.Status)
	ExpectWithOffset(1, c.Reason).To(Equal(reason),
		"%s on %q carries reason %q, message %q", ridSegmentConditionType, sd.Name, c.Reason, c.Message)
	for _, want := range naming {
		ExpectWithOffset(1, c.Message).To(ContainSubstring(want),
			"the refusal on %q does not name %q: %q", sd.Name, want, c.Message)
	}
	ExpectWithOffset(1, sd.Status.Phase).NotTo(Equal(ldapv1alpha1.DatabasePhaseRunning),
		"%q reports Running while its RID segment is refused", sd.Name)
}

func expectAccepted(sd *ldapv1alpha1.SlapdDatabase) {
	c := ridCondition(sd)
	ExpectWithOffset(1, c).NotTo(BeNil(), "no %s condition on %q", ridSegmentConditionType, sd.Name)
	ExpectWithOffset(1, string(c.Status)).To(Equal(string(metav1.ConditionTrue)),
		"%s on %q is %s/%s: %q", ridSegmentConditionType, sd.Name, c.Status, c.Reason, c.Message)
	ExpectWithOffset(1, c.Reason).To(Equal(reasonRIDSegmentOK))
}

var _ = Describe("RID segment withhold (ADR-031 decision 6)", func() {
	ctx := context.Background()

	It("refuses BOTH databases of a colliding pair, each naming the other", func() {
		f := newRIDFixture(ctx)
		Expect(f.createDB(ctx, "alpha", new(int32(4)))).To(Succeed())
		Expect(f.createDB(ctx, "bravo", new(int32(4)))).To(Succeed())

		f.reconcile(ctx, "alpha")
		f.reconcile(ctx, "bravo")

		// The symmetry is the point. Whichever database reconciles first must
		// NOT win: the loser's stanzas are already on the pods, and a winner
		// would write its own set over a cn=config that still carries them.
		expectRefusal(f.get(ctx, "alpha"), reasonRIDSegmentCollision, "bravo", "4")
		expectRefusal(f.get(ctx, "bravo"), reasonRIDSegmentCollision, "alpha", "4")
	})

	It("refuses the pair in the reverse reconcile order too", func() {
		f := newRIDFixture(ctx)
		Expect(f.createDB(ctx, "alpha", new(int32(4)))).To(Succeed())
		Expect(f.createDB(ctx, "bravo", new(int32(4)))).To(Succeed())

		f.reconcile(ctx, "bravo")
		f.reconcile(ctx, "alpha")

		expectRefusal(f.get(ctx, "bravo"), reasonRIDSegmentCollision, "alpha")
		expectRefusal(f.get(ctx, "alpha"), reasonRIDSegmentCollision, "bravo")
	})

	It("accepts a pair declaring distinct segments", func() {
		f := newRIDFixture(ctx)
		Expect(f.createDB(ctx, "alpha", new(int32(1)))).To(Succeed())
		Expect(f.createDB(ctx, "bravo", new(int32(2)))).To(Succeed())

		f.reconcile(ctx, "alpha")
		f.reconcile(ctx, "bravo")

		expectAccepted(f.get(ctx, "alpha"))
		expectAccepted(f.get(ctx, "bravo"))
	})

	It("lifts the withhold on both when the collision is resolved", func() {
		f := newRIDFixture(ctx)
		Expect(f.createDB(ctx, "alpha", new(int32(4)))).To(Succeed())
		Expect(f.createDB(ctx, "bravo", new(int32(4)))).To(Succeed())

		f.reconcile(ctx, "alpha")
		f.reconcile(ctx, "bravo")
		expectRefusal(f.get(ctx, "alpha"), reasonRIDSegmentCollision, "bravo")

		// A withhold that never lifts is as bad as one that writes: the mesh
		// stays frozen after the human has done exactly what the message asked.
		// ADR-031 decision 3 stores nothing, so there is nothing to release —
		// this spec is what proves that claim end to end.
		bravo := f.get(ctx, "bravo")
		bravo.Spec.Replication.RIDSegment = new(int32(5))
		Expect(k8sClient.Update(ctx, bravo)).To(Succeed())

		f.reconcile(ctx, "alpha")
		f.reconcile(ctx, "bravo")

		expectAccepted(f.get(ctx, "alpha"))
		expectAccepted(f.get(ctx, "bravo"))
	})

	// R2. The condition is published inside the replication branch, so a
	// database that STOPS replicating keeps whatever verdict it last had.
	// That is the documented escape from a collision — "set
	// replication.enabled=false to exclude this database" is the text of the
	// refusal itself and of the CEL message — so this is the path the message
	// sends people down, and it leaves them with a False condition naming a
	// database they already stopped colliding with, beside Ready=True and
	// phase=Running. The only tell is a stale observedGeneration.
	//
	// tunablesConvergedCondition is set unconditionally at the top of every
	// pass for exactly this reason; this condition has to follow it.
	It("does not leave a stale refusal on a database that stops replicating", func() {
		f := newRIDFixture(ctx)
		Expect(f.createDB(ctx, "alpha", new(int32(4)))).To(Succeed())
		Expect(f.createDB(ctx, "bravo", new(int32(4)))).To(Succeed())

		f.reconcile(ctx, "alpha")
		expectRefusal(f.get(ctx, "alpha"), reasonRIDSegmentCollision, "bravo")

		// Take the escape the refusal itself recommends.
		alpha := f.get(ctx, "alpha")
		disabled := false
		alpha.Spec.Replication.Enabled = &disabled
		Expect(k8sClient.Update(ctx, alpha)).To(Succeed())

		f.reconcile(ctx, "alpha")

		alpha = f.get(ctx, "alpha")
		c := ridCondition(alpha)
		if c == nil {
			// Clearing the condition outright is an acceptable answer: a
			// database that does not replicate has no segment to judge, and
			// absence cannot be stale.
			return
		}
		Expect(string(c.Status)).NotTo(Equal(string(metav1.ConditionFalse)),
			"%s still reads False/%s — %q — on a database that no longer replicates. "+
				"The refusal names a collision that cannot exist any more, beside Ready=%v and phase=%s",
			ridSegmentConditionType, c.Reason, c.Message,
			readyStatus(alpha), alpha.Status.Phase)
		Expect(c.ObservedGeneration).To(Equal(alpha.Generation),
			"%s was not re-evaluated this pass: it carries generation %d while the object is at %d, "+
				"which is the only trace a stale condition leaves",
			ridSegmentConditionType, c.ObservedGeneration, alpha.Generation)
	})

	It("refuses a segment the CRD admits but the layout does not", func() {
		f := newRIDFixture(ctx)
		// 60 passes the CRD's Maximum=499 — proven by the Create succeeding —
		// and is outside the default layout's C = 49. The two bounds are
		// deliberately different layers: the API server cannot know the mesh's
		// ridLayout, so the operator is the only place this can be caught.
		Expect(f.createDB(ctx, "wide", new(int32(60)))).To(Succeed())

		f.reconcile(ctx, "wide")

		expectRefusal(f.get(ctx, "wide"), reasonRIDSegmentOutOfRange, "60", "48")
	})

	It("refuses a replicating database that declares no segment, at admission", func() {
		f := newRIDFixture(ctx)
		// The CEL rule on SlapdDatabaseSpec. judgeRIDSegment still handles the
		// missing case because an object can predate the field or be applied
		// with validation off — but the ordinary path must never get that far.
		err := f.createDB(ctx, "nosegment", nil)
		Expect(err).To(HaveOccurred(),
			"the API server accepted a replicating SlapdDatabase with no ridSegment; "+
				"the CEL rule on SlapdDatabaseSpec is missing or names the wrong field")
		Expect(strings.ToLower(err.Error())).To(ContainSubstring("ridsegment"),
			"the admission refusal does not name the field to fix: %v", err)
	})

	It("accepts a non-replicating database that declares no segment", func() {
		f := newRIDFixture(ctx)
		disabled := false
		Expect(k8sClient.Create(ctx, &ldapv1alpha1.SlapdDatabase{
			ObjectMeta: metav1.ObjectMeta{Name: "quiet", Namespace: f.ns},
			Spec: ldapv1alpha1.SlapdDatabaseSpec{
				ClusterRef:  f.cluster,
				Suffix:      "dc=quiet,dc=test",
				Replication: ldapv1alpha1.DatabaseReplicationConfig{Enabled: &disabled},
			},
		})).To(Succeed(),
			"replication.enabled=false must remain the documented escape from declaring a segment")
	})
})

//go:fix inline
func ptrInt32(v int32) *int32 { return new(v) }

// readyStatus is for failure messages: the point of R2 is that a stale refusal
// sits NEXT TO a healthy Ready, which is what makes it unreadable.
func readyStatus(sd *ldapv1alpha1.SlapdDatabase) string {
	for i := range sd.Status.Conditions {
		if sd.Status.Conditions[i].Type == "Ready" {
			return fmt.Sprintf("%s/%s", sd.Status.Conditions[i].Status, sd.Status.Conditions[i].Reason)
		}
	}
	return "<absent>"
}
