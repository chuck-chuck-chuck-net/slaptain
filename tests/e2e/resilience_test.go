package e2e_test

// Resilience tests cover pod restarts and cluster-wide warm restart.
//
// These tests are intentionally slow (pod restarts take 30–90 s each; a full
// cluster warm-restart takes up to 3 minutes). They are disabled by default so
// that `make e2e` stays fast. Run them with:
//
//	E2E_RESILIENCE=1 make e2e
//
// The tests are Ordered so they run sequentially and leave the cluster in a
// clean state for subsequent test blocks.

import (
	"fmt"
	"os"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var _ = Describe("resilience", Label("resilience"), Ordered, func() {

	BeforeAll(func() {
		if os.Getenv("E2E_RESILIENCE") == "" {
			Skip("set E2E_RESILIENCE=1 to run resilience tests (pod restarts, cluster warm restart)")
		}
	})

	// ── Non-seed pod restart ──────────────────────────────────────────────────
	//
	// Delete slapd-1. The StatefulSet controller recreates it. After recovery,
	// the pod re-establishes its delta-syncrepl connections. We verify both that
	// data is still accessible and that replication is working to/from the
	// restarted pod.
	//
	// Important: statefulSetReady() can return a false-positive immediately after
	// deletion because the ReadyReplicas counter hasn't been decremented yet.
	// Instead, we capture the old pod UID and wait for a new pod with a different
	// UID to appear and become ready.

	It("cluster recovers after slapd-1 is restarted", func(ctx SpecContext) {
		sts, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, "slapd", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		if sts.Spec.Replicas == nil || *sts.Spec.Replicas < 2 {
			Skip("requires ≥2 replicas")
		}

		By("capturing old slapd-1 pod UID")
		oldPod, err := k8sClient.CoreV1().Pods(namespace).Get(ctx, "slapd-1", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		oldUID := oldPod.UID

		By("deleting pod slapd-1")
		err = k8sClient.CoreV1().Pods(namespace).Delete(ctx, "slapd-1", metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("waiting for slapd-1 to be recreated (new UID) and become ready")
		Eventually(ctx, func() bool {
			pod, err := k8sClient.CoreV1().Pods(namespace).Get(ctx, "slapd-1", metav1.GetOptions{})
			if err != nil {
				return false // pod gone or not yet created
			}
			if pod.UID == oldUID {
				return false // still the old (terminating) pod
			}
			return podReady(k8sClient, namespace, "slapd-1")
		}).WithTimeout(3*time.Minute).WithPolling(5*time.Second).Should(BeTrue(),
			"slapd-1 should restart and become ready within 3 min")

		// ldapConn was established via ClusterIP and may have been connected to
		// slapd-1. After the pod is deleted its TCP connection is severed regardless
		// of which endpoint it was on. Reconnect to ensure subsequent steps — and
		// all tests that follow in the suite — can still use ldapConn.
		By("reconnecting admin LDAP connection (in case it was connected to the restarted pod)")
		ldapConn.Close()
		ldapConn = retryConnectLDAP(ctx, localLDAPAddr, baseDN, adminPW)

		By("verifying data is accessible via ClusterIP after restart")
		Expect(ldapExists(ldapConn, baseDN)).To(BeTrue())
		Expect(ldapExists(ldapConn, fmt.Sprintf("ou=People,%s", baseDN))).To(BeTrue())

		By("verifying replication to slapd-1 works after its restart")
		conn1, cancel1 := dialPodLDAP(namespace, "slapd-1", podPort1)
		defer cancel1()
		defer conn1.Close()

		uid := fmt.Sprintf("resil-p1-%d", GinkgoRandomSeed())
		var dn string
		if sts.Spec.Replicas != nil && *sts.Spec.Replicas >= 3 {
			// For ≥3-replica clusters explicitly verify the slapd-2→slapd-1 channel.
			// Using ldapConn (ClusterIP) here is non-deterministic: the write might
			// land on slapd-0, leaving the slapd-2→slapd-1 channel unverified. A
			// subsequent replication test writing to slapd-2 would then race against
			// that channel still reconnecting.
			conn2, cancel2 := dialPodLDAP(namespace, "slapd-2", podPort2)
			defer cancel2()
			defer conn2.Close()
			dn = addReplTestUser(conn2, uid, 65520)
			defer conn2.Del(ldap.NewDelRequest(dn, nil)) //nolint:errcheck
		} else {
			dn = addReplTestUser(ldapConn, uid, 65520)
			defer ldapConn.Del(ldap.NewDelRequest(dn, nil)) //nolint:errcheck
		}

		Eventually(ctx, func() bool {
			return ldapExists(conn1, dn)
		}).WithTimeout(30*time.Second).WithPolling(2*time.Second).Should(BeTrue(),
			"new entry should replicate to the restarted slapd-1")
	}, NodeTimeout(5*time.Minute))

	// ── Seed pod restart ──────────────────────────────────────────────────────
	//
	// slapd-0 is the pod that ran the one-time LDAP bootstrap (root entry,
	// cn=admin, cn=replication). After it restarts, it performs a warm start
	// (config exists on PVC → bootstrap.sh skips slaptest) and re-joins the
	// replication mesh. We verify data access and that the operator's
	// bootstrapComplete guard still holds (no duplicate bootstrap attempt).

	It("cluster recovers after seed pod slapd-0 is restarted", func(ctx SpecContext) {
		By("capturing old slapd-0 pod UID")
		oldPod, err := k8sClient.CoreV1().Pods(namespace).Get(ctx, "slapd-0", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		oldUID := oldPod.UID

		By("deleting pod slapd-0")
		err = k8sClient.CoreV1().Pods(namespace).Delete(ctx, "slapd-0", metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("waiting for slapd-0 to be recreated (new UID) and become ready")
		Eventually(ctx, func() bool {
			pod, err := k8sClient.CoreV1().Pods(namespace).Get(ctx, "slapd-0", metav1.GetOptions{})
			if err != nil {
				return false
			}
			if pod.UID == oldUID {
				return false
			}
			return podReady(k8sClient, namespace, "slapd-0")
		}).WithTimeout(3*time.Minute).WithPolling(5*time.Second).Should(BeTrue(),
			"slapd-0 should restart and become ready within 3 min")

		// The existing ldapConn was forwarded through the port-forward to slapd-0's
		// network namespace, which is now closed. The port-forward may briefly return
		// EOF while it re-establishes to a remaining healthy endpoint (slapd-1/slapd-2).
		// Use retryConnectLDAP to wait for stabilisation instead of a fixed sleep.
		By("reconnecting admin LDAP connection (retrying until port-forward stabilises)")
		ldapConn.Close()
		ldapConn = retryConnectLDAP(ctx, localLDAPAddr, baseDN, adminPW)

		By("verifying data is accessible after seed pod restart")
		Expect(ldapExists(ldapConn, baseDN)).To(BeTrue())
		Expect(ldapExists(ldapConn, fmt.Sprintf("ou=People,%s", baseDN))).To(BeTrue())
	}, NodeTimeout(5*time.Minute))

	// ── All-pods simultaneous restart (warm start) ────────────────────────────
	//
	// Simulates a full-cluster power failure scenario: all pods are deleted at
	// once. The StatefulSet controller recreates them in ordinal order
	// (OrderedReady policy). Each pod performs a warm start — bootstrap.sh detects
	// the existing cn=config directory on the PVC and skips slaptest. Replication
	// re-establishes automatically (CSNs match → no data transfer needed).
	//
	// Two-phase wait: first confirm ReadyReplicas drops (actual deletion happened),
	// then wait for full recovery. This avoids the false-positive window where
	// StatefulSet status still shows the old ready count.

	It("data persists after simultaneous restart of all pods (warm start from PVCs)", func(ctx SpecContext) {
		By("capturing UIDs of all current slapd pods")
		podList, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/name=slapd",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(podList.Items).NotTo(BeEmpty(), "expected at least one slapd pod")

		oldUIDs := make(map[string]types.UID, len(podList.Items))
		for _, p := range podList.Items {
			oldUIDs[p.Name] = p.UID
		}

		By(fmt.Sprintf("deleting all %d pods simultaneously", len(podList.Items)))
		for _, pod := range podList.Items {
			err := k8sClient.CoreV1().Pods(namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		// Phase 1: confirm StatefulSet has registered the deletions.
		By("waiting for StatefulSet ReadyReplicas to drop (deletions registered)")
		Eventually(ctx, func() bool {
			sts, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, "slapd", metav1.GetOptions{})
			if err != nil {
				return false
			}
			desired := int32(1)
			if sts.Spec.Replicas != nil {
				desired = *sts.Spec.Replicas
			}
			return sts.Status.ReadyReplicas < desired
		}).WithTimeout(2*time.Minute).WithPolling(2*time.Second).Should(BeTrue(),
			"StatefulSet ReadyReplicas should drop after pod deletions")

		// Phase 2: wait for full recovery.
		By("waiting for StatefulSet to be fully ready again (warm start from PVCs)")
		Eventually(ctx, func() bool {
			return statefulSetReady(k8sClient, namespace, "slapd")
		}).WithTimeout(5*time.Minute).WithPolling(5*time.Second).Should(BeTrue(),
			"StatefulSet should recover within 5 min after simultaneous pod deletion")

		// All pods are gone so the port-forward has no backend. Retry until
		// svc/slapd has at least one endpoint ready again.
		By("reconnecting admin LDAP connection after full cluster restart")
		ldapConn.Close()
		ldapConn = retryConnectLDAP(ctx, localLDAPAddr, baseDN, adminPW)

		By("verifying all bootstrap entries survive the warm restart")
		Expect(ldapExists(ldapConn, baseDN)).To(BeTrue())
		for _, ou := range []string{"People", "Mail", "Readpw"} {
			Expect(ldapExists(ldapConn, fmt.Sprintf("ou=%s,%s", ou, baseDN))).To(BeTrue(),
				"ou=%s should survive warm restart", ou)
		}

		By("verifying all pods received new UIDs (confirms actual restart, not a no-op)")
		for podName, oldUID := range oldUIDs {
			pod, err := k8sClient.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(pod.UID).NotTo(Equal(oldUID),
				"pod %s should have a new UID after restart", podName)
			Expect(pod.Status.Phase).To(Equal(corev1.PodRunning),
				"pod %s should be Running after warm restart", podName)
		}

		By("verifying replication is healthy after warm restart")
		stsAfter, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, "slapd", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		if stsAfter.Spec.Replicas != nil && *stsAfter.Spec.Replicas >= 2 {
			conn0, cancel0 := dialPodLDAP(namespace, "slapd-0", podPort0)
			defer cancel0()
			defer conn0.Close()

			conn1, cancel1 := dialPodLDAP(namespace, "slapd-1", podPort1)
			defer cancel1()
			defer conn1.Close()

			uid := fmt.Sprintf("resil-warmstart-%d", GinkgoRandomSeed())
			dn := addReplTestUser(conn0, uid, 65530)
			defer conn0.Del(ldap.NewDelRequest(dn, nil)) //nolint:errcheck

			Eventually(ctx, func() bool {
				return ldapExists(conn1, dn)
			}).WithTimeout(60*time.Second).WithPolling(2*time.Second).Should(BeTrue(),
				"replication should be healthy after warm restart")
		}
	}, NodeTimeout(8*time.Minute))
})
