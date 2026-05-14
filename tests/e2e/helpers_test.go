package e2e_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// ── Kubernetes helpers ────────────────────────────────────────────────────────

func newK8sClient() *kubernetes.Clientset {
	c, err := kubernetes.NewForConfig(restConfig())
	Expect(err).NotTo(HaveOccurred(), "failed to create k8s client")
	return c
}

func newCRDClient() client.Client {
	cfg := restConfig()
	s := runtime.NewScheme()
	Expect(ldapv1alpha1.AddToScheme(s)).To(Succeed(), "failed to add ldap scheme")
	c, err := client.New(cfg, client.Options{Scheme: s})
	Expect(err).NotTo(HaveOccurred(), "failed to create CRD client")
	return c
}

// restConfig returns the *rest.Config used by both newK8sClient and newCRDClient.
func restConfig() *rest.Config {
	var config *rest.Config
	var err error
	if kubeconfig := os.Getenv("KUBECONFIG"); kubeconfig != "" {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		config, err = rest.InClusterConfig()
		if err != nil {
			config, err = clientcmd.BuildConfigFromFlags("", os.ExpandEnv("$HOME/.kube/config"))
		}
	}
	Expect(err).NotTo(HaveOccurred(), "failed to build k8s config")
	return config
}

func statefulSetReady(c *kubernetes.Clientset, ns, name string) bool {
	sts, err := c.AppsV1().StatefulSets(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return false
	}
	desired := int32(1)
	if sts.Spec.Replicas != nil {
		desired = *sts.Spec.Replicas
	}
	return sts.Status.ReadyReplicas >= desired
}

// podReady returns true when pod <name> is in Running phase with every container ready.
func podReady(c *kubernetes.Clientset, ns, name string) bool {
	pod, err := c.CoreV1().Pods(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return false
	}
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	if len(pod.Status.ContainerStatuses) == 0 {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if !cs.Ready {
			return false
		}
	}
	return true
}

// slapdDatabaseRunning returns true when the named SlapdDatabase CR has phase=Running.
func slapdDatabaseRunning(c client.Client, ns, name string) bool {
	db := &ldapv1alpha1.SlapdDatabase{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: ns}, db); err != nil {
		return false
	}
	return db.Status.Phase == ldapv1alpha1.DatabasePhaseRunning
}

// slapdSchemaApplied returns true when the named SlapdSchema CR has applied=true.
func slapdSchemaApplied(c client.Client, ns, name string) bool {
	ss := &ldapv1alpha1.SlapdSchema{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: ns}, ss); err != nil {
		return false
	}
	return ss.Status.Applied
}

func jobSucceeded(c *kubernetes.Clientset, ns, name string) bool {
	job, err := c.BatchV1().Jobs(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return false
	}
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func deploymentReady(c *kubernetes.Clientset, ns, name string) bool {
	d, err := c.AppsV1().Deployments(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return false
	}
	return d.Status.ReadyReplicas >= 1
}

// ── LDAP helpers ──────────────────────────────────────────────────────────────

func connectLDAP(addr, base, password string) *ldap.Conn {
	conn, err := ldap.Dial("tcp", addr)
	Expect(err).NotTo(HaveOccurred(), "failed to dial LDAP at %s", addr)
	adminDN := fmt.Sprintf("cn=admin,%s", base)
	Expect(conn.Bind(adminDN, password)).To(Succeed(), "admin bind failed")
	return conn
}

// ldapExists returns true when a single entry exists at dn.
//
// Errors are folded into "false" because the typical caller is an Eventually
// loop polling for an entry to appear/disappear, where transient "not yet"
// states (LDAPResultNoSuchObject) are expected. But genuine failures
// (connection drops, referrals, ACL refusals, server errors) would otherwise
// be invisible — Expect-style callers see only "false" with no diagnostic.
// So: NoSuchObject is silent (the expected "doesn't exist"); every other
// error is logged to GinkgoWriter with the DN and the LDAP result code,
// which surfaces in the failure output for the spec that called us.
func ldapExists(conn *ldap.Conn, dn string) bool {
	req := ldap.NewSearchRequest(dn,
		ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		0, 0, false, "(objectClass=*)", []string{"dn"}, nil)
	result, err := conn.Search(req)
	if err != nil {
		if !ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
			fmt.Fprintf(GinkgoWriter, "ldapExists(%q): search returned non-NoSuchObject error: %v\n", dn, err)
		}
		return false
	}
	return len(result.Entries) == 1
}

// ldapSearch runs a subtree search and returns entries; fails the test on error.
func ldapSearch(conn *ldap.Conn, base, filter string, attrs ...string) []*ldap.Entry {
	req := ldap.NewSearchRequest(base,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
		0, 0, false, filter, attrs, nil)
	result, err := conn.Search(req)
	Expect(err).NotTo(HaveOccurred(), "ldapSearch(%s, %s) failed", base, filter)
	return result.Entries
}

// ldapAdd is idempotent: an "entry already exists" error is silently ignored.
func ldapAdd(conn *ldap.Conn, req *ldap.AddRequest) {
	err := conn.Add(req)
	if ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists) {
		return
	}
	Expect(err).NotTo(HaveOccurred(), "ldapAdd(%s) failed", req.DN)
}

// refreshLDAPConn checks if ldapConn is still alive and reconnects if needed.
// This guards against server-side connection resets caused by operator
// reconciliation (e.g. olcSyncRepl replacement restarts slapd's replication
// engine and can drop existing client connections).
func refreshLDAPConn() {
	if ldapConn == nil {
		ldapConn = retryConnectLDAP(context.Background(), localLDAPAddr, baseDN, adminPW)
		return
	}
	// Lightweight rootDSE search as a connection health check.
	req := ldap.NewSearchRequest("", ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		0, 0, false, "(objectClass=*)", []string{"1.1"}, nil)
	if _, err := ldapConn.Search(req); err != nil {
		ldapConn.Close()
		ldapConn = retryConnectLDAP(context.Background(), localLDAPAddr, baseDN, adminPW)
	}
}

// retryConnectLDAP retries dialing + binding as admin until success or the
// context deadline is reached. Use this instead of connectLDAP when a transient
// failure is expected — e.g. immediately after a pod restart when kubectl
// port-forward may briefly return EOF while it re-establishes to a new backend.
//
// The retry loop verifies the connection with a rootDSE search after binding.
// This catches connections that are TCP-established but about to be dropped
// (e.g. during operator reconciliation of syncrepl stanzas).
func retryConnectLDAP(ctx context.Context, addr, base, password string) *ldap.Conn {
	var conn *ldap.Conn
	Eventually(ctx, func() bool {
		c, err := ldap.Dial("tcp", addr)
		if err != nil {
			return false
		}
		adminDN := fmt.Sprintf("cn=admin,%s", base)
		if err := c.Bind(adminDN, password); err != nil {
			c.Close()
			return false
		}
		// Verify the connection is responsive, not just TCP-connected.
		req := ldap.NewSearchRequest("", ldap.ScopeBaseObject, ldap.NeverDerefAliases,
			0, 0, false, "(objectClass=*)", []string{"1.1"}, nil)
		if _, err := c.Search(req); err != nil {
			c.Close()
			return false
		}
		conn = c
		return true
	}).WithTimeout(30*time.Second).WithPolling(2*time.Second).Should(BeTrue(),
		"should reconnect to LDAP at %s within 30 s", addr)
	return conn
}

// refreshPodConn checks if a per-pod LDAP connection is still alive and
// re-establishes it via dialPodLDAP if dead. Call this before first use when
// the connection may have sat idle (e.g. while other connections were being
// established). The old cancel function is called on reconnect.
func refreshPodConn(conn *ldap.Conn, cancel context.CancelFunc, ns, podName, localPort string) (*ldap.Conn, context.CancelFunc) {
	req := ldap.NewSearchRequest("", ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		0, 0, false, "(objectClass=*)", []string{"1.1"}, nil)
	if _, err := conn.Search(req); err == nil {
		return conn, cancel // still alive
	}
	conn.Close()
	cancel()
	return dialPodLDAP(ns, podName, localPort)
}

// refreshReadOnlyPodConn is the read-only variant of refreshPodConn.
func refreshReadOnlyPodConn(conn *ldap.Conn, cancel context.CancelFunc, ns, podName, localPort string) (*ldap.Conn, context.CancelFunc) {
	req := ldap.NewSearchRequest("", ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		0, 0, false, "(objectClass=*)", []string{"1.1"}, nil)
	if _, err := conn.Search(req); err == nil {
		return conn, cancel
	}
	conn.Close()
	cancel()
	return dialReadOnlyPodLDAP(ns, podName, localPort)
}

// podNodePortAddr returns the NodePort address for a specific pod if per-pod
// NodePort env vars are configured (E2E_NODE_IP + E2E_POD_NODEPORT_BASE).
// Returns "" if not configured. The ordinal is extracted from the pod name
// (e.g. "slapd-2" → 2).
func podNodePortAddr(podName, baseEnv string) string {
	nodeIP := os.Getenv("E2E_NODE_IP")
	baseStr := os.Getenv(baseEnv)
	if nodeIP == "" || baseStr == "" {
		return ""
	}
	// Extract ordinal from pod name: last segment after "-"
	parts := strings.Split(podName, "-")
	ordinalStr := parts[len(parts)-1]
	ordinal := 0
	fmt.Sscanf(ordinalStr, "%d", &ordinal)

	base := 0
	fmt.Sscanf(baseStr, "%d", &base)
	return fmt.Sprintf("%s:%d", nodeIP, base+ordinal)
}

// dialPodLDAP connects directly to a specific slapd pod as admin via its
// dedicated NodePort. E2E_NODE_IP and E2E_POD_NODEPORT_BASE must be set by
// the e2e setup scripts. The returned cancel func is a no-op (kept in the
// signature for callers that still hold per-pod connection lifecycle state);
// connections are torn down by ldap.Conn.Close().
func dialPodLDAP(ns, podName, _ string) (*ldap.Conn, context.CancelFunc) {
	addr := podNodePortAddr(podName, "E2E_POD_NODEPORT_BASE")
	Expect(addr).NotTo(BeEmpty(),
		"E2E_NODE_IP and E2E_POD_NODEPORT_BASE must be set to dial pod %s", podName)
	_ = ns
	return retryConnectLDAP(context.Background(), addr, baseDN, adminPW), func() {}
}

// dialReadOnlyPodLDAP connects directly to a specific read-only slapd pod via
// its dedicated NodePort. E2E_NODE_IP and E2E_RO_POD_NODEPORT_BASE must be set.
func dialReadOnlyPodLDAP(ns, podName, _ string) (*ldap.Conn, context.CancelFunc) {
	addr := podNodePortAddr(podName, "E2E_RO_POD_NODEPORT_BASE")
	Expect(addr).NotTo(BeEmpty(),
		"E2E_NODE_IP and E2E_RO_POD_NODEPORT_BASE must be set to dial RO pod %s", podName)
	_ = ns
	return retryConnectLDAP(context.Background(), addr, baseDN, adminPW), func() {}
}

// addReplTestUser adds a minimal posixAccount entry to ou=People for use in
// replication and resilience tests. Returns the DN. Caller is responsible for
// deleting the entry when done.
func addReplTestUser(conn *ldap.Conn, uid string, uidNum int) string {
	dn := fmt.Sprintf("uid=%s,ou=People,%s", uid, baseDN)
	req := ldap.NewAddRequest(dn, nil)
	req.Attribute("objectClass", []string{"posixAccount", "shadowAccount", "inetOrgPerson"})
	req.Attribute("cn", []string{"Replication Test"})
	req.Attribute("sn", []string{"Test"})
	req.Attribute("uid", []string{uid})
	req.Attribute("uidNumber", []string{fmt.Sprintf("%d", uidNum)})
	req.Attribute("gidNumber", []string{"65500"})
	req.Attribute("homeDirectory", []string{"/dev/null"})
	ldapAdd(conn, req)
	return dn
}
