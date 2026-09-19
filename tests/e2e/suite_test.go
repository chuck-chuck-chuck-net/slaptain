package e2e_test

import (
	"os"
	"strings"
	"testing"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// ── Suite-wide variables ──────────────────────────────────────────────────────

var (
	k8sClient *kubernetes.Clientset
	crdClient client.Client // controller-runtime client for SlapdCluster CRD access
	ldapConn  *ldap.Conn
	adminPW   string
	rootPW    string
	readpwPWs map[string]string // username → plaintext; empty when not configured
	baseDN    string            // discovered from the server's rootDSE at suite start

	// NAMESPACE_TESTING — Kubernetes namespace to test in.
	namespace = envOrDefault("NAMESPACE_TESTING", "slaptain-testing")

	// LDAP_ADDR — direct LDAP address (host:port). REQUIRED. Set by the e2e
	// scripts to a NodePort (single-site: ${NODE_IP}:30389; multi-site: per
	// context). The previous port-forward fallback has been removed — see
	// commit log and CLAUDE.md.
	localLDAPAddr = os.Getenv("LDAP_ADDR")

	// READPW_OU — OU name for read-only service accounts (readpw users).
	readpwOU = envOrDefault("READPW_OU", "ServiceAccounts")

	// DB_CR_NAME — SlapdDatabase CR name. Used to read ridBase for RID assertions.
	dbCRName = envOrDefault("DB_CR_NAME", "slapd-db")
)

// ── Ginkgo bootstrap ──────────────────────────────────────────────────────────

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Slaptain E2E Suite")
}

var _ = BeforeSuite(func(ctx SpecContext) {
	Expect(localLDAPAddr).NotTo(BeEmpty(),
		"LDAP_ADDR is required (NodePort target host:port). The e2e scripts set this; if you "+
			"are running tests directly, export LDAP_ADDR=<NODE_IP>:<NodePort> before invoking go test.")

	By("Setting up Kubernetes client")
	k8sClient = newK8sClient()

	By("Setting up CRD client for SlapdCluster access")
	crdClient = newCRDClient()

	// The migration scenario (tests/e2e/migration_test.go) uses two
	// SlapdClusters in different namespaces with custom names; none of the
	// legacy "slapd" cluster setup below applies. The migration spec does its
	// own BeforeAll-scoped setup, so just hand it the k8sClient + crdClient
	// and bail out early.
	if os.Getenv("E2E_MIGRATION") == "1" {
		By("E2E_MIGRATION=1 — skipping legacy slapd suite setup")
		return
	}

	By("Waiting for slapd StatefulSet to be ready")
	Eventually(ctx, func() bool {
		return statefulSetReady(k8sClient, namespace, "slapd")
	}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(BeTrue())

	// Wait for SlapdDatabase to reach Running phase (replaces old bootstrap Job wait).
	By("Waiting for SlapdDatabase " + dbCRName + " to be Running")
	Eventually(ctx, func() bool {
		return slapdDatabaseRunning(crdClient, namespace, dbCRName)
	}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(BeTrue())

	// Read admin password from the database credentials secret.
	dbCredSecret := envOrDefault("DB_CREDENTIALS_SECRET", dbCRName+"-credentials")
	By("Reading admin password from " + dbCredSecret)
	pwSecret, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx, dbCredSecret, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	adminPW = string(pwSecret.Data["root-password"])
	Expect(adminPW).NotTo(BeEmpty(), "root-password must not be empty in "+dbCredSecret)

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

	By("Using LDAP address: " + localLDAPAddr)

	// baseDN is the *primary* database's suffix, read off its own CR rather
	// than picked out of the rootDSE. Picking the first dc= namingContext was
	// unambiguous while one SlapdDatabase was the only shape any fixture
	// declared; the ADR-019 fixture declares two (tests/resources/example/
	// database2.yaml), and namingContexts order is slapd's business, not ours —
	// so the whole suite's identity would have hinged on it. The rootDSE is
	// still consulted, as an assertion that the suffix we resolved is actually
	// served.
	By("Resolving base DN from SlapdDatabase " + dbCRName)
	{
		sd := &ldapv1alpha1.SlapdDatabase{}
		Expect(crdClient.Get(ctx, types.NamespacedName{Name: dbCRName, Namespace: namespace}, sd)).To(Succeed())
		baseDN = sd.Spec.Suffix
		Expect(baseDN).NotTo(BeEmpty(), "SlapdDatabase %s must declare spec.suffix", dbCRName)

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
		served := false
		for _, nc := range contexts {
			if strings.EqualFold(nc, baseDN) {
				served = true
				break
			}
		}
		Expect(served).To(BeTrue(),
			"rootDSE must advertise the primary database's suffix %q; got %v", baseDN, contexts)
		GinkgoLogr.Info("resolved base DN", "baseDN", baseDN, "namingContexts", contexts)
	}

	By("Connecting to LDAP as admin")
	ldapConn = connectLDAP(localLDAPAddr, baseDN, adminPW)

	By("Waiting for bootstrap data to be visible (replication convergence)")
	ouPeopleDN := "ou=People," + baseDN
	Eventually(ctx, func() bool {
		return ldapExists(ldapConn, ouPeopleDN)
	}).WithTimeout(2*time.Minute).WithPolling(5*time.Second).Should(BeTrue(),
		"ou=People not visible — bootstrap may not have run or replication has not converged")

	// The wait above proves LOCAL convergence and says nothing about the peers.
	// Under ADR-016 every peer's syncrepl stanzas name our pod IPs, so a mesh
	// whose pods were created shortly before the suite started is silently
	// one-directional until each peer rediscovers them — measured at 146-432 s
	// for a whole site (docs/BACKLOG.md, "Cross-site recovery after a pod-IP
	// change"). Nothing local detects that: the StatefulSet is ready, the
	// cluster is Running, CSNs converge among OUR pods.
	//
	// waitForCrossSiteReplication already exists for specs that replace pods,
	// on the principle "the spec that invalidates the addresses must wait for
	// them". This is the case that principle missed: SETUP invalidates them
	// too, and no spec is responsible. Observed 2026-09-19 — site-1's pods were
	// 62 s old when "a write on siteA propagates to siteB" gave up after 60 s.
	waitForCrossSiteReplication(ctx, "suite start (setup may have rolled the pods)")
}, NodeTimeout(20*time.Minute))

// Refresh the shared ldapConn before every test. Tests that restart pods or
// trigger operator reconciliation (syncrepl replacement) can cause the server
// to drop existing connections. A lightweight rootDSE ping catches this early
// and reconnects transparently instead of failing with "connection closed".
var _ = BeforeEach(func() {
	if baseDN != "" { // skip until BeforeSuite has completed
		refreshLDAPConn()
	}
})

var _ = AfterSuite(func() {
	if ldapConn != nil {
		ldapConn.Close()
	}
})

// ── Helpers ───────────────────────────────────────────────────────────────────

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
