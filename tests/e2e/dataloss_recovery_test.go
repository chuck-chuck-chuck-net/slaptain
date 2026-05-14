package e2e_test

// Data-loss-via-replication recovery test. Validates ADR-012's "case 2":
// a multi-pod cluster loses one pod's data (PVC reset, node failure,
// emptyDir restart) — the empty pod rejoins the mesh and syncrepl restores
// the directory from surviving peers. The operator does NOT re-seed.
//
// This test runs only against the ephemeral fixture (label `ephemeral-only`),
// where pod deletion guarantees the new pod's volumes are blank. On the
// persistent fixture this scenario is covered indirectly by the existing
// resilience tests (slapd-0 / slapd-1 restart), where PVC reuse is the
// recovery path — the explicit "empty pod recovers via replication"
// guarantee only matters when persistence is actually disabled.
//
// Fixture selection is the gate — no E2E_RESILIENCE env-var check. If you've
// deployed the ephemeral fixture you want this test to run.

import (
	"fmt"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

var _ = Describe("ephemeral: data loss recovery via replication",
	Label("resilience", "ephemeral-only"), Ordered, func() {

	const targetPod = "slapd-1" // pick a non-seed pod to keep this orthogonal to seed semantics

	var addedUserDN string
	var preObservedGeneration int64

	BeforeAll(func(ctx SpecContext) {
		// Add an entry that we'll use as a recovery witness. Seed entries
		// alone would be insufficient — they prove only that the operator-
		// managed bootstrap path is intact. A real recovery should also
		// restore user-added data that came in *after* the initial seed.
		addedUserDN = fmt.Sprintf("uid=dataloss-witness,ou=People,%s", baseDN)
		req := ldap.NewAddRequest(addedUserDN, nil)
		req.Attribute("objectClass", []string{"posixAccount", "shadowAccount", "inetOrgPerson"})
		req.Attribute("cn", []string{"Dataloss Witness"})
		req.Attribute("sn", []string{"Witness"})
		req.Attribute("uid", []string{"dataloss-witness"})
		req.Attribute("uidNumber", []string{"60001"})
		req.Attribute("gidNumber", []string{"60000"})
		req.Attribute("homeDirectory", []string{"/home/dataloss-witness"})
		req.Attribute("loginShell", []string{"/bin/false"})
		ldapAdd(ldapConn, req)

		// Capture the SlapdDatabase's observedGeneration BEFORE the data-loss
		// event. ADR-012's contract: a successful recovery must NOT trigger a
		// re-reconcile of the seed step. If it did, the operator would touch
		// status (even idempotently) and bump observedGeneration. The post-
		// recovery check asserts this number is unchanged.
		var sd ldapv1alpha1.SlapdDatabase
		Expect(crdClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: dbCRName}, &sd)).
			To(Succeed())
		Expect(sd.Status.SeedApplied).To(BeTrue(),
			"precondition: SeedApplied must already be true before the data-loss event")
		preObservedGeneration = sd.Status.ObservedGeneration
	})

	AfterAll(func() {
		// Best-effort cleanup. The witness entry is on every pod via replication
		// by now; deleting from the LB-fronted connection propagates the same way.
		if addedUserDN != "" {
			_ = ldapConn.Del(ldap.NewDelRequest(addedUserDN, nil))
		}
	})

	It("a pod that loses its emptyDir volumes recovers the full DIT from peers", func(ctx SpecContext) {
		By("capturing the original " + targetPod + " UID")
		oldPod, err := k8sClient.CoreV1().Pods(namespace).Get(ctx, targetPod, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		oldUID := oldPod.UID

		By("deleting " + targetPod + " (emptyDir → new pod comes back blank)")
		Expect(k8sClient.CoreV1().Pods(namespace).Delete(ctx, targetPod, metav1.DeleteOptions{})).
			To(Succeed())

		By("waiting for the StatefulSet to recreate " + targetPod + " with a new UID + Ready")
		Eventually(ctx, func() bool {
			p, err := k8sClient.CoreV1().Pods(namespace).Get(ctx, targetPod, metav1.GetOptions{})
			if err != nil {
				return false
			}
			if p.UID == oldUID {
				return false
			}
			for _, c := range p.Status.Conditions {
				if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
					return true
				}
			}
			return false
		}).WithTimeout(3 * time.Minute).WithPolling(2 * time.Second).Should(BeTrue(),
			"%s should come back with a new UID and become Ready within 3 min", targetPod)

		By("connecting directly to the new " + targetPod + " via its per-pod NodePort")
		// dialPodLDAP requires E2E_NODE_IP + E2E_POD_NODEPORT_BASE — set by e2e.sh.
		podConn, podCancel := dialPodLDAP(namespace, targetPod, "")
		defer func() {
			podConn.Close()
			podCancel()
		}()

		By("verifying the seeded OUs converge on " + targetPod + " via replication")
		// Replication needs a moment to drain the change journal from peers
		// into the empty pod. The operator never re-seeds (ADR-012); whatever
		// shows up here came over syncrepl.
		for _, ou := range []string{"People", "Mail", readpwOU} {
			dn := fmt.Sprintf("ou=%s,%s", ou, baseDN)
			Eventually(ctx, func() bool {
				return ldapExists(podConn, dn)
			}).WithTimeout(2*time.Minute).WithPolling(2*time.Second).Should(BeTrue(),
				"ou=%s should converge to %s via syncrepl after volume reset", ou, targetPod)
		}

		By("verifying the user-added witness entry also recovers")
		// This is the load-bearing check. If the operator had re-seeded (the
		// wrong behaviour ADR-012 explicitly forbids), only the spec-defined
		// seed entries would come back — the witness wouldn't, because the
		// operator doesn't know about it. Witness convergence => replication
		// is the recovery mechanism, not seed.
		Eventually(ctx, func() bool {
			return ldapExists(podConn, addedUserDN)
		}).WithTimeout(2*time.Minute).WithPolling(2*time.Second).Should(BeTrue(),
			"%s should converge via syncrepl — confirms recovery is replication-driven, "+
				"not a fake re-seed", addedUserDN)
	}, NodeTimeout(8*time.Minute))

	It("SlapdDatabase observedGeneration is unchanged across the recovery", func(ctx SpecContext) {
		// Belt-and-braces check on the operator-side promise. ADR-012 says
		// SeedApplied is a one-way latch and the operator does not act on
		// "data missing" detection. If the operator had re-seeded during the
		// recovery, it would have called setStatus → patched status →
		// observedGeneration would tick (independently of spec.generation,
		// since observedGeneration tracks the most recent generation the
		// controller has reconciled). Unchanged means the operator stayed out.
		var sd ldapv1alpha1.SlapdDatabase
		Expect(crdClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: dbCRName}, &sd)).
			To(Succeed())

		Expect(sd.Status.SeedApplied).To(BeTrue(),
			"SeedApplied must remain true after recovery (one-way latch, ADR-012)")
		Expect(sd.Status.ObservedGeneration).To(Equal(preObservedGeneration),
			"ObservedGeneration changed (%d → %d) — suggests the operator re-reconciled "+
				"during the data-loss event, which violates ADR-012's one-shot seed contract",
			preObservedGeneration, sd.Status.ObservedGeneration)
	})
})
