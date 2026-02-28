package e2e_test

import (
	"context"
	"os"
	"testing"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ── Suite-wide variables ──────────────────────────────────────────────────────

var (
	k8sClient *kubernetes.Clientset
	ldapConn  *ldap.Conn
	pfCancel  context.CancelFunc
	adminPW   string

	// NAMESPACE_TESTING — Kubernetes namespace to test in.
	namespace = envOrDefault("NAMESPACE_TESTING", "slaptain-testing")

	// LDAP_DOMAIN — base DN of the LDAP tree.
	baseDN = envOrDefault("LDAP_DOMAIN", "dc=as8,dc=lab,dc=test")

	// LDAP_SVC — service to port-forward for plain LDAP access (port 389).
	ldapSvc = envOrDefault("LDAP_SVC", "svc/slapd")

	localLDAPAddr = "localhost:13891"
)

// ── Ginkgo bootstrap ──────────────────────────────────────────────────────────

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Slaptain E2E Suite")
}

var _ = BeforeSuite(func(ctx SpecContext) {
	By("Setting up Kubernetes client")
	k8sClient = newK8sClient()

	By("Waiting for slapd StatefulSet to be ready")
	Eventually(ctx, func() bool {
		return statefulSetReady(k8sClient, namespace, "slapd")
	}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(BeTrue())

	By("Waiting for bootstrap Job to complete")
	Eventually(ctx, func() bool {
		return jobSucceeded(k8sClient, namespace, "slapd-test")
	}).WithTimeout(5 * time.Minute).WithPolling(10 * time.Second).Should(BeTrue())

	By("Reading admin password from slapd-test-passwords secret")
	secret, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx, "slapd-test-passwords", metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	adminPW = string(secret.Data["admin-password"])
	Expect(adminPW).NotTo(BeEmpty(), "admin password must not be empty in slapd-test-passwords")

	By("Starting kubectl port-forward to " + ldapSvc + ":389")
	pfCancel = startPortForward(namespace, ldapSvc, "13891", "389")
	time.Sleep(2 * time.Second)

	By("Connecting to LDAP as admin")
	ldapConn = connectLDAP(localLDAPAddr, baseDN, adminPW)
}, NodeTimeout(12*time.Minute))

var _ = AfterSuite(func() {
	if ldapConn != nil {
		ldapConn.Close()
	}
	if pfCancel != nil {
		pfCancel()
	}
})

// ── Helpers ───────────────────────────────────────────────────────────────────

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
