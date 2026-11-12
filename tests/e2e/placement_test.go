package e2e_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// ADR-043: placement is the user's, with ONE default the user does not have to
// know about — a single topology spread constraint per fleet, generated when
// spec.placement says nothing about spread constraints.
//
// It asserts the StatefulSet TEMPLATE rather than waiting on pods, for the same
// reason image_selector_test.go does: the behaviour in scope is "what did the
// operator write", which the template shows immediately. Whether the scheduler
// could honour it is a property of the node count, and a single-node fixture
// satisfies maxSkew 1 trivially — a spec that waited for pods would be green on
// a cluster where the constraint had never been written at all.
//
// THREE THINGS THIS SPEC IS BUILT TO CATCH, each of which looks healthy from
// outside:
//
//   - ONE COMBINED GROUP instead of two. A constraint selecting every pod of
//     the cluster constrains the per-node TOTAL and permits both writers to
//     share a node while the counts still look even (ADR-043 option 3). It
//     renders as a perfectly ordinary spread constraint, so the assertion is on
//     the SELECTORS: the two fleets' must name their own fleet and must DIFFER
//     from each other.
//   - A MERGE instead of a replace. Appending the generated constraint to a
//     user's list is rejected by the API only when the two share a
//     {topologyKey, whenUnsatisfiable} pair — so a merge bug survives most
//     hand-testing and detonates on somebody's hostname constraint.
//   - [] READ AS ABSENT. An explicit empty list is "no constraint"; an absent
//     key is "the operator's default". A predicate written len() > 0 instead of
//     != nil collapses them, and the result — the default silently back on — is
//     indistinguishable from a user who never set the field. The same collapse
//     once happened at the ENCODE end: spec.placement.topologySpreadConstraints
//     is a *[]T rather than a []T because omitempty drops an empty slice as
//     readily as a nil one, which made this very spec unable to express [] over
//     the typed client. Written as a value slice it would go quietly green here
//     again, so this case is also the pointer's regression guard.
//
// NOT VACUOUS AGAINST A PRUNED CRD. Helm never upgrades a chart's crds/ and
// tests/e2e.sh does not re-apply them, so a cluster whose CRD predates
// spec.placement prunes the field on write and every check that only ever
// inspected the generated default would stay green. Two of the three cases here
// would fail instead — but with a confusing message, so each mutating case
// reads the CR back and asserts the field survived the round trip, naming the
// pruned CRD by name when it did not.
var _ = Describe("ADR-043: pod placement", Label("placement"), Ordered, func() {
	const (
		rwSts = "slapd"
		roSts = "slapd-readonly"
		instanceLabel = "app.kubernetes.io/instance"
	)

	var (
		origPlacement ldapv1alpha1.PlacementConfig
		roPresent     bool
	)

	// fleets returns the StatefulSets this spec asserts over: the read-write
	// one always, the read-only one when the fixture has it.
	fleets := func() []string {
		if roPresent {
			return []string{rwSts, roSts}
		}
		return []string{rwSts}
	}

	constraintsOf := func(ctx SpecContext, sts string) []corev1.TopologySpreadConstraint {
		s, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, sts, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "read StatefulSet %s", sts)
		return s.Spec.Template.Spec.TopologySpreadConstraints
	}

	podSpecOf := func(ctx SpecContext, sts string) corev1.PodSpec {
		s, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, sts, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "read StatefulSet %s", sts)
		return s.Spec.Template.Spec
	}

	// setPlacement writes spec.placement and then READS IT BACK. The readback
	// is the pruned-CRD guard: an UPDATE against a CRD that does not know the
	// field succeeds and silently drops it, which would otherwise surface three
	// assertions later as "the operator ignored my list".
	setPlacement := func(ctx SpecContext, mutate func(*ldapv1alpha1.PlacementConfig)) {
		Eventually(ctx, func() error {
			sc := &ldapv1alpha1.SlapdCluster{}
			if err := crdClient.Get(ctx, nsName(namespace, rwSts), sc); err != nil {
				return err
			}
			mutate(&sc.Spec.Placement)
			return crdClient.Update(ctx, sc)
		}).WithTimeout(time.Minute).WithPolling(2 * time.Second).Should(Succeed())

		want := ldapv1alpha1.PlacementConfig{}
		mutate(&want)
		got := &ldapv1alpha1.SlapdCluster{}
		Expect(crdClient.Get(ctx, nsName(namespace, rwSts), got)).To(Succeed())
		Expect(got.Spec.Placement.TopologySpreadConstraints).To(Equal(want.TopologySpreadConstraints),
			"spec.placement.topologySpreadConstraints did not survive the write. The usual cause is a "+
				"STALE CRD: Helm never upgrades a chart's crds/ and tests/e2e.sh does not re-apply them, "+
				"so a cluster whose CRD predates ADR-043 prunes the field and every placement check below "+
				"would be testing nothing. Run `make operator-crd-apply CONTEXT=<ctx>`")
	}

	BeforeAll(func(ctx SpecContext) {
		sc := &ldapv1alpha1.SlapdCluster{}
		Expect(crdClient.Get(ctx, nsName(namespace, rwSts), sc)).To(Succeed())
		origPlacement = *sc.Spec.Placement.DeepCopy()

		_, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, roSts, metav1.GetOptions{})
		switch {
		case err == nil:
			roPresent = true
		case apierrors.IsNotFound(err):
			roPresent = false
		default:
			Expect(err).NotTo(HaveOccurred(), "read the read-only StatefulSet")
		}

		DeferCleanup(func(ctx SpecContext) {
			By("restoring the fixture's spec.placement, and waiting for the fleets to settle")
			setPlacement(ctx, func(p *ldapv1alpha1.PlacementConfig) { *p = *origPlacement.DeepCopy() })
			for _, sts := range fleets() {
				name := sts
				Eventually(ctx, func() bool {
					return statefulSetReady(k8sClient, namespace, name)
				}).WithTimeout(5*time.Minute).WithPolling(5*time.Second).Should(BeTrue(),
					"%s never came back ready after the placement specs", name)
			}
		})
	})

	It("generates ONE spread constraint per fleet, each selecting its own fleet", func(ctx SpecContext) {
		// FORCE A TRANSITION FIRST. The fixture already says nothing about
		// placement, so clearing the field changes nothing, nothing reconciles,
		// and the assertions below would pass against whatever the StatefulSet
		// happened to carry already — including constraints written by a
		// previous, correct operator. Measured: under a deliberately broken
		// operator this spec went green in 22 ms without one reconcile.
		//
		// So put a sentinel there, watch it land, and only then clear it.
		sentinel := []corev1.TopologySpreadConstraint{{
			MaxSkew:           7,
			TopologyKey:       "slaptain.example/sentinel",
			WhenUnsatisfiable: corev1.ScheduleAnyway,
		}}
		By("displacing whatever is there with a sentinel, so the default has to be re-derived")
		setPlacement(ctx, func(p *ldapv1alpha1.PlacementConfig) { p.TopologySpreadConstraints = &sentinel })
		Eventually(ctx, func() []corev1.TopologySpreadConstraint {
			return constraintsOf(ctx, rwSts)
		}).WithTimeout(2*time.Minute).WithPolling(5*time.Second).Should(Equal(sentinel),
			"the sentinel never reached the RW pod template, so nothing below would be measuring a reconcile")

		By("then saying nothing about spread constraints")
		setPlacement(ctx, func(p *ldapv1alpha1.PlacementConfig) { p.TopologySpreadConstraints = nil })

		selectors := map[string]map[string]string{}
		for _, sts := range fleets() {
			name := sts
			// Wait for the SENTINEL TO BE GONE, not merely for a constraint to
			// exist: the sentinel is itself one constraint, so a length check
			// is satisfied by the value we are waiting to see replaced.
			var got []corev1.TopologySpreadConstraint
			Eventually(ctx, func() []corev1.TopologySpreadConstraint {
				got = constraintsOf(ctx, name)
				return got
			}).WithTimeout(2*time.Minute).WithPolling(5*time.Second).ShouldNot(Equal(sentinel),
				"%s still carries the sentinel: clearing the list never re-derived the default", name)
			Expect(got).To(HaveLen(1), "%s must carry exactly the one generated constraint", name)

			c := got[0]
			Expect(c.MaxSkew).To(BeEquivalentTo(1), "%s: maxSkew", name)
			Expect(c.TopologyKey).To(Equal(corev1.LabelHostname), "%s: topologyKey", name)
			// ScheduleAnyway is not a style choice: once a pod's PV is bound it
			// may use exactly one node, and a hard constraint refusing that node
			// leaves the pod Pending with no reschedule that can fix it.
			Expect(c.WhenUnsatisfiable).To(Equal(corev1.ScheduleAnyway), "%s: whenUnsatisfiable", name)
			Expect(c.LabelSelector).NotTo(BeNil(), "%s: a constraint with no selector spreads nothing", name)
			Expect(c.LabelSelector.MatchLabels).To(HaveKeyWithValue(instanceLabel, name),
				"%s: the constraint must select THIS fleet", name)
			selectors[name] = c.LabelSelector.MatchLabels
		}

		if roPresent {
			// The assertion that separates two spread groups from one. A single
			// combined group over both fleets keeps the per-node TOTAL even and
			// still permits every writer to share a node (ADR-043 option 3) —
			// and from the StatefulSet it looks like a healthy constraint.
			Expect(selectors[rwSts]).NotTo(Equal(selectors[roSts]),
				"both fleets carry the SAME selector, which is one combined spread group: it constrains "+
					"the per-node total and says nothing about the mix of writers and readers")
		}
	})

	It("lets a user's list REPLACE the default, on both fleets, and passes the rest through", func(ctx SpecContext) {
		mine := []corev1.TopologySpreadConstraint{{
			MaxSkew:           3,
			TopologyKey:       "topology.kubernetes.io/zone",
			WhenUnsatisfiable: corev1.ScheduleAnyway,
			LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "slapd"}},
		}}
		tolerations := []corev1.Toleration{{
			Key: "slaptain.example/dedicated", Operator: corev1.TolerationOpEqual,
			Value: "ldap", Effect: corev1.TaintEffectNoSchedule,
		}}

		By("supplying a list of my own, plus one pass-through field")
		setPlacement(ctx, func(p *ldapv1alpha1.PlacementConfig) {
			p.TopologySpreadConstraints = &mine
			p.Tolerations = tolerations
		})

		for _, sts := range fleets() {
			name := sts
			Eventually(ctx, func() []corev1.TopologySpreadConstraint {
				return constraintsOf(ctx, name)
			}).WithTimeout(2*time.Minute).WithPolling(5*time.Second).Should(Equal(mine),
				"%s: the user's list must REPLACE the generated constraint, never be merged with it — "+
					"the API rejects two constraints sharing a {topologyKey, whenUnsatisfiable} pair and "+
					"ignores labelSelector when deciding, so a merge makes the user's own constraint "+
					"unschedulable from a value they cannot see", name)

			spec := podSpecOf(ctx, name)
			Expect(spec.Tolerations).To(Equal(tolerations), "%s: tolerations must reach the pod template verbatim", name)
			// The node half only; the pod half is reserved for the operator, and
			// an unset nodeAffinity must render no affinity struct at all.
			Expect(spec.Affinity).To(BeNil(), "%s: an unset nodeAffinity must render no affinity field", name)
		}
	})

	It("writes NO constraint for an explicit empty list", func(ctx SpecContext) {
		By("saying none, which is not the same as saying nothing")
		setPlacement(ctx, func(p *ldapv1alpha1.PlacementConfig) {
			p.TopologySpreadConstraints = &[]corev1.TopologySpreadConstraint{}
		})

		for _, sts := range fleets() {
			name := sts
			Eventually(ctx, func() int {
				return len(constraintsOf(ctx, name))
			}).WithTimeout(2*time.Minute).WithPolling(5*time.Second).Should(BeZero(),
				"%s: an explicit [] is the ONLY way to switch the generated default off. Getting the "+
					"default back here means absent and empty were collapsed — a predicate written "+
					"len() > 0 where it must be != nil", name)
		}
	})
})
