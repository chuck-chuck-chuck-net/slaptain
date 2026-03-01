package e2e_test

import (
	"context"
	"os"
	"strings"
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
	rootPW    string
	readpwPWs map[string]string // username → plaintext; empty when not configured
	baseDN    string            // discovered from the server's rootDSE at suite start

	// NAMESPACE_TESTING — Kubernetes namespace to test in.
	namespace = envOrDefault("NAMESPACE_TESTING", "slaptain-testing")

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

	By("Reading admin password from slapd-passwords secret")
	pwSecret, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx, "slapd-passwords", metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	adminPW = string(pwSecret.Data["admin-password"])
	Expect(adminPW).NotTo(BeEmpty(), "admin-password must not be empty in slapd-passwords")

	By("Reading root password from slapd-config-password secret")
	cfgSecret, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx, "slapd-config-password", metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	rootPW = string(cfgSecret.Data["root-password"])
	Expect(rootPW).NotTo(BeEmpty(), "root-password must not be empty in slapd-config-password")

	By("Reading readpw passwords from slapd-test-passwords secret")
	testSecret, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx, "slapd-test-passwords", metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	readpwPWs = make(map[string]string)
	for k, v := range testSecret.Data {
		if user, ok := strings.CutPrefix(k, "readpw-"); ok {
			readpwPWs[user] = string(v)
		}
	}

	By("Starting kubectl port-forward to " + ldapSvc + ":389")
	pfCancel = startPortForward(namespace, ldapSvc, "13891", "389")
	time.Sleep(2 * time.Second)

	By("Discovering base DN from LDAP rootDSE")
	{
		conn, err := ldap.Dial("tcp", localLDAPAddr)
		Expect(err).NotTo(HaveOccurred())
		req := ldap.NewSearchRequest("",
			ldap.ScopeBaseObject, ldap.NeverDerefAliases,
			0, 0, false, "(objectClass=*)", []string{"namingContexts"}, nil)
		result, err := conn.Search(req)
		conn.Close()
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Entries).To(HaveLen(1))
		contexts := result.Entries[0].GetAttributeValues("namingContexts")
		Expect(contexts).NotTo(BeEmpty(), "rootDSE must advertise at least one namingContext")
		// Skip internal cn= suffixes (cn=accesslog, cn=config); pick the user data tree.
		for _, ctx := range contexts {
			if strings.HasPrefix(strings.ToLower(ctx), "dc=") {
				baseDN = ctx
				break
			}
		}
		Expect(baseDN).NotTo(BeEmpty(), "rootDSE must advertise a dc= naming context")
		GinkgoLogr.Info("discovered base DN", "baseDN", baseDN)
	}

	By("Connecting to LDAP as admin")
	ldapConn = connectLDAP(localLDAPAddr, baseDN, adminPW)

	By("Waiting for bootstrap data to be visible (replication convergence)")
	ouPeopleDN := "ou=People," + baseDN
	Eventually(ctx, func() bool {
		return ldapExists(ldapConn, ouPeopleDN)
	}).WithTimeout(2*time.Minute).WithPolling(5*time.Second).Should(BeTrue(),
		"ou=People not visible — bootstrap may not have run or replication has not converged")
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
