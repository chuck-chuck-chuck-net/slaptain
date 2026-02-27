package e2e_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var (
	k8sClient    *kubernetes.Clientset
	ldapConn     *ldap.Conn
	pfCancel     context.CancelFunc
	adminPW      string
	repoRoot     string

	namespace    = envOrDefault("NAMESPACE_TESTING", "slaptain-testing")
	baseDN       = envOrDefault("LDAP_DOMAIN", "dc=as8,dc=lab,dc=test")
	localLDAPAddr = "localhost:13891"
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Slaptain E2E Suite")
}

var _ = BeforeSuite(func(ctx SpecContext) {
	// tests/e2e/ is two levels below the repo root
	abs, err := filepath.Abs("../..")
	Expect(err).NotTo(HaveOccurred())
	repoRoot = abs

	if os.Getenv("SKIP_SETUP") == "" {
		By("Installing slapd chart")
		runMake("gencert")
		runMake("helm-install", "HELM_VALUES_SLAPD=-f tests/values.slapd.yaml")

		By("Installing slapd-test chart (bootstrap + toolkit)")
		runMake("testing-helm-install", "HELM_VALUES_SLAPD_TESTING=--set toolkit.enabled=true")
	}

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

	By("Reading admin password from secret")
	secret, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx, "slapd-test-passwords", metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	adminPW = string(secret.Data["admin-password"])
	Expect(adminPW).NotTo(BeEmpty())

	By("Starting kubectl port-forward to slapd:389")
	pfCancel = startPortForward(namespace, "svc/slapd", "13891", "389")
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
	if os.Getenv("SKIP_TEARDOWN") == "" {
		runMake("testing-helm-uninstall")
		runMake("helm-uninstall")
	}
})

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func runMake(args ...string) {
	cmd := exec.Command("make", args...)
	cmd.Dir = repoRoot
	cmd.Stdout = GinkgoWriter
	cmd.Stderr = GinkgoWriter
	Expect(cmd.Run()).To(Succeed(), fmt.Sprintf("make %v failed", args))
}
