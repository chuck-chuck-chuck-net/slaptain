package e2e_test

import (
	"fmt"
	"strings"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// localPorts for read-only pod port-forwards (must not collide with other tests).
const (
	roPort0 = "13895"
)

var _ = Describe("read-only replicas", Label("readonly"), func() {

	BeforeEach(func(ctx SpecContext) {
		sts, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, "slapd-readonly", metav1.GetOptions{})
		if err != nil {
			Skip("slapd-readonly StatefulSet not found; skipping read-only tests")
		}
		replicas := int32(0)
		if sts.Spec.Replicas != nil {
			replicas = *sts.Spec.Replicas
		}
		if replicas == 0 {
			Skip("slapd-readonly has 0 replicas; skipping read-only tests")
		}
		// Wait for at least one RO pod to be ready.
		Eventually(ctx, func() bool {
			return statefulSetReady(k8sClient, namespace, "slapd-readonly")
		}).WithTimeout(5*time.Minute).WithPolling(5*time.Second).Should(BeTrue(),
			"slapd-readonly StatefulSet should become ready")
	}, NodeTimeout(6*time.Minute))

	It("read-only StatefulSet and services exist", func(ctx SpecContext) {
		_, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, "slapd-readonly", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "slapd-readonly StatefulSet should exist")

		_, err = k8sClient.CoreV1().Services(namespace).Get(ctx, "slapd-readonly", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "slapd-readonly ClusterIP service should exist")

		_, err = k8sClient.CoreV1().Services(namespace).Get(ctx, "slapd-readonly-headless", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "slapd-readonly-headless service should exist")
	}, NodeTimeout(30*time.Second))

	It("data replicates from RW to RO", func(ctx SpecContext) {
		// Write a test entry via the RW ClusterIP service.
		uid := fmt.Sprintf("ro-repl-%d", GinkgoRandomSeed())
		dn := addReplTestUser(ldapConn, uid, 65520)
		defer ldapConn.Del(ldap.NewDelRequest(dn, nil)) //nolint:errcheck

		// Connect to the RO pod and verify the entry appears.
		roConn, cancel := dialReadOnlyPodLDAP(namespace, "slapd-readonly-0", roPort0)
		defer cancel()
		defer roConn.Close()

		GinkgoLogr.Info("wrote test entry via RW, waiting for RO replica", "dn", dn)
		Eventually(ctx, func() bool {
			return ldapExists(roConn, dn)
		}).WithTimeout(60*time.Second).WithPolling(2*time.Second).Should(BeTrue(),
			"entry %s should replicate from RW to slapd-readonly-0 within 60 s", dn)
	}, NodeTimeout(2*time.Minute))

	It("write to RO pod is rejected", func(ctx SpecContext) {
		roConn, cancel := dialReadOnlyPodLDAP(namespace, "slapd-readonly-0", roPort0)
		defer cancel()
		defer roConn.Close()

		uid := fmt.Sprintf("ro-write-reject-%d", GinkgoRandomSeed())
		dn := fmt.Sprintf("uid=%s,ou=People,%s", uid, baseDN)
		req := ldap.NewAddRequest(dn, nil)
		req.Attribute("objectClass", []string{"posixAccount", "shadowAccount", "inetOrgPerson"})
		req.Attribute("cn", []string{"RO Write Test"})
		req.Attribute("sn", []string{"Test"})
		req.Attribute("uid", []string{uid})
		req.Attribute("uidNumber", []string{"65521"})
		req.Attribute("gidNumber", []string{"65500"})
		req.Attribute("homeDirectory", []string{"/dev/null"})

		err := roConn.Add(req)
		Expect(err).To(HaveOccurred(), "write to read-only replica should fail")
		GinkgoLogr.Info("write to RO pod correctly rejected", "err", err)
	}, NodeTimeout(30*time.Second))

	// ADR-025 D5, RO-fleet reach: the operator's standing DataPresent probe must
	// visit the read-only pods, not just the writable ones. A glue suffix
	// propagates to an RO consumer (ADR-025 evidence item 5 measured the same
	// glue with the same entryUUID on the incident site's RO pod), so an
	// RO-only glue would otherwise be invisible to the operator.
	//
	// The assertion is on the message rather than the status, because on a
	// healthy fleet both the RW-only and the all-pods probe report
	// True/RootEntryVisible — only the message discloses how many pods were
	// actually reached. Red by construction before the change: it read
	// "visible on all 3 reached RW pod(s)" with the RO pod never dialled.
	It("DataPresent covers the read-only fleet", func(ctx SpecContext) {
		var last *metav1.Condition
		Eventually(ctx, func() bool {
			last = dataPresentCondition(crdClient, namespace, dbCRName)
			return last != nil && last.Status == metav1.ConditionTrue &&
				strings.Contains(last.Message, "read-only pod(s)")
		}).WithTimeout(3*time.Minute).WithPolling(5*time.Second).Should(BeTrue(),
			"DataPresent should report the read-only pods as probed, got %s",
			describeDataPresent(last))
		GinkgoLogr.Info("DataPresent covers RO pods", "message", last.Message)
	}, NodeTimeout(4*time.Minute))
})
