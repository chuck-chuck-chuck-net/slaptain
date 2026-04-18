package e2e_test

import (
	"fmt"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// localPorts used for per-pod port-forwards in this file.
// Must not collide with the suite-wide 13891.
const (
	podPort0 = "13892"
	podPort1 = "13893"
	podPort2 = "13894"
)

var _ = Describe("replication", Label("replication"), func() {

	var replicas int32

	BeforeEach(func(ctx SpecContext) {
		sts, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, "slapd", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		replicas = 1
		if sts.Spec.Replicas != nil {
			replicas = *sts.Spec.Replicas
		}
		if replicas < 2 {
			Skip("replication tests require ≥2 replicas")
		}
	}, NodeTimeout(15*time.Second))

	// ── Directed write → read ─────────────────────────────────────────────────
	//
	// Each test opens two port-forwards to individual pods (bypassing the ClusterIP
	// load-balancer) and verifies that an ADD on one pod propagates to the other
	// within the delta-syncrepl convergence window (~5 s in practice, 30 s timeout).

	It("a write to slapd-0 is visible on slapd-1", func(ctx SpecContext) {
		conn0, cancel0 := dialPodLDAP(namespace, "slapd-0", podPort0)
		defer func() { cancel0(); conn0.Close() }()

		conn1, cancel1 := dialPodLDAP(namespace, "slapd-1", podPort1)
		defer func() { cancel1(); conn1.Close() }()

		// conn0 may have gone stale while conn1 was being established.
		conn0, cancel0 = refreshPodConn(conn0, cancel0, namespace, "slapd-0", podPort0)

		uid := fmt.Sprintf("rtest0to1-%d", GinkgoRandomSeed())
		dn := addReplTestUser(conn0, uid, 65510)
		defer conn0.Del(ldap.NewDelRequest(dn, nil)) //nolint:errcheck // best-effort cleanup

		GinkgoLogr.Info("wrote test entry to slapd-0, waiting for slapd-1", "dn", dn)
		Eventually(ctx, func() bool {
			return ldapExists(conn1, dn)
		}).WithTimeout(30 * time.Second).WithPolling(2 * time.Second).Should(BeTrue(),
			"entry %s should replicate from slapd-0 to slapd-1 within 30 s", dn)
	}, NodeTimeout(2*time.Minute))

	It("a write to slapd-1 is visible on slapd-0", func(ctx SpecContext) {
		conn0, cancel0 := dialPodLDAP(namespace, "slapd-0", podPort0)
		defer func() { cancel0(); conn0.Close() }()

		conn1, cancel1 := dialPodLDAP(namespace, "slapd-1", podPort1)
		defer func() { cancel1(); conn1.Close() }()

		conn0, cancel0 = refreshPodConn(conn0, cancel0, namespace, "slapd-0", podPort0)

		uid := fmt.Sprintf("rtest1to0-%d", GinkgoRandomSeed())
		dn := addReplTestUser(conn1, uid, 65511)
		defer conn1.Del(ldap.NewDelRequest(dn, nil)) //nolint:errcheck

		GinkgoLogr.Info("wrote test entry to slapd-1, waiting for slapd-0", "dn", dn)
		Eventually(ctx, func() bool {
			return ldapExists(conn0, dn)
		}).WithTimeout(30 * time.Second).WithPolling(2 * time.Second).Should(BeTrue(),
			"entry %s should replicate from slapd-1 to slapd-0 within 30 s", dn)
	}, NodeTimeout(2*time.Minute))

	It("a write to slapd-2 is visible on slapd-0 and slapd-1", func(ctx SpecContext) {
		if replicas < 3 {
			Skip("requires ≥3 replicas")
		}

		conn0, cancel0 := dialPodLDAP(namespace, "slapd-0", podPort0)
		defer func() { cancel0(); conn0.Close() }()

		conn1, cancel1 := dialPodLDAP(namespace, "slapd-1", podPort1)
		defer func() { cancel1(); conn1.Close() }()

		conn2, cancel2 := dialPodLDAP(namespace, "slapd-2", podPort2)
		defer func() { cancel2(); conn2.Close() }()

		// Earlier connections may have gone stale while later ones were established.
		conn0, cancel0 = refreshPodConn(conn0, cancel0, namespace, "slapd-0", podPort0)
		conn1, cancel1 = refreshPodConn(conn1, cancel1, namespace, "slapd-1", podPort1)

		uid := fmt.Sprintf("rtest2toall-%d", GinkgoRandomSeed())
		dn := addReplTestUser(conn2, uid, 65512)
		defer conn2.Del(ldap.NewDelRequest(dn, nil)) //nolint:errcheck

		GinkgoLogr.Info("wrote test entry to slapd-2, waiting for slapd-0 and slapd-1", "dn", dn)
		// 60 s timeout: if the resilience tests ran first and slapd-1 recently
		// restarted, the slapd-2→slapd-1 syncrepl channel may need extra time to
		// re-establish (retry="5 10 60 +" means up to ~50 s for 10 retries at 5 s).
		Eventually(ctx, func() bool {
			return ldapExists(conn0, dn) && ldapExists(conn1, dn)
		}).WithTimeout(60 * time.Second).WithPolling(2 * time.Second).Should(BeTrue(),
			"entry %s should propagate from slapd-2 to both slapd-0 and slapd-1 within 60 s", dn)
	}, NodeTimeout(2*time.Minute))

	// ── Delete propagation ────────────────────────────────────────────────────

	It("a delete on slapd-0 is propagated to slapd-1", func(ctx SpecContext) {
		conn0, cancel0 := dialPodLDAP(namespace, "slapd-0", podPort0)
		defer func() { cancel0(); conn0.Close() }()

		conn1, cancel1 := dialPodLDAP(namespace, "slapd-1", podPort1)
		defer func() { cancel1(); conn1.Close() }()

		conn0, cancel0 = refreshPodConn(conn0, cancel0, namespace, "slapd-0", podPort0)

		// Write via the ClusterIP so cleanup is not pod-specific.
		uid := fmt.Sprintf("rtest-del-%d", GinkgoRandomSeed())
		dn := addReplTestUser(ldapConn, uid, 65513)

		// Wait for the entry to appear on both pods before testing deletion.
		Eventually(ctx, func() bool {
			return ldapExists(conn0, dn) && ldapExists(conn1, dn)
		}).WithTimeout(30 * time.Second).WithPolling(2 * time.Second).Should(BeTrue(),
			"entry should be present on both pods before deletion test")

		// Delete on pod-0, verify disappears from pod-1.
		Expect(conn0.Del(ldap.NewDelRequest(dn, nil))).To(Succeed())

		GinkgoLogr.Info("deleted entry on slapd-0, waiting for slapd-1 to reflect deletion", "dn", dn)
		Eventually(ctx, func() bool {
			return !ldapExists(conn1, dn)
		}).WithTimeout(30 * time.Second).WithPolling(2 * time.Second).Should(BeTrue(),
			"deletion of %s should propagate from slapd-0 to slapd-1 within 30 s", dn)
	}, NodeTimeout(2*time.Minute))
})
