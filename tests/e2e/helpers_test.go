package e2e_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// ── Kubernetes helpers ────────────────────────────────────────────────────────

func newK8sClient() *kubernetes.Clientset {
	var config *rest.Config
	var err error
	if kubeconfig := os.Getenv("KUBECONFIG"); kubeconfig != "" {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		// Try in-cluster config (ServiceAccount token inside a pod).
		config, err = rest.InClusterConfig()
		if err != nil {
			// Fall back to local kubeconfig for developer workstations.
			config, err = clientcmd.BuildConfigFromFlags("", os.ExpandEnv("$HOME/.kube/config"))
		}
	}
	Expect(err).NotTo(HaveOccurred(), "failed to build k8s config")
	client, err := kubernetes.NewForConfig(config)
	Expect(err).NotTo(HaveOccurred(), "failed to create k8s client")
	return client
}

func statefulSetReady(client *kubernetes.Clientset, ns, name string) bool {
	sts, err := client.AppsV1().StatefulSets(ns).Get(context.Background(), name, metav1.GetOptions{})
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
func podReady(client *kubernetes.Clientset, ns, name string) bool {
	pod, err := client.CoreV1().Pods(ns).Get(context.Background(), name, metav1.GetOptions{})
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

func jobSucceeded(client *kubernetes.Clientset, ns, name string) bool {
	job, err := client.BatchV1().Jobs(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return false
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func deploymentReady(client *kubernetes.Clientset, ns, name string) bool {
	d, err := client.AppsV1().Deployments(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return false
	}
	return d.Status.ReadyReplicas >= 1
}

// ── Port-forward ──────────────────────────────────────────────────────────────

func startPortForward(ns, resource, localPort, remotePort string) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "kubectl", "port-forward",
		"-n", ns,
		resource,
		fmt.Sprintf("%s:%s", localPort, remotePort),
	)
	cmd.Stdout = GinkgoWriter
	cmd.Stderr = GinkgoWriter
	Expect(cmd.Start()).To(Succeed(), "failed to start port-forward")
	return cancel
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
func ldapExists(conn *ldap.Conn, dn string) bool {
	req := ldap.NewSearchRequest(dn,
		ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		0, 0, false, "(objectClass=*)", []string{"dn"}, nil)
	result, err := conn.Search(req)
	if err != nil {
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

// retryConnectLDAP retries dialing + binding as admin until success or the
// context deadline is reached. Use this instead of connectLDAP when a transient
// failure is expected — e.g. immediately after a pod restart when kubectl
// port-forward may briefly return EOF while it re-establishes to a new backend.
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
		conn = c
		return true
	}).WithTimeout(30*time.Second).WithPolling(2*time.Second).Should(BeTrue(),
		"should reconnect to LDAP at %s within 30 s", addr)
	return conn
}

// dialPodLDAP connects directly to a specific slapd pod as admin.
//
// In in-cluster mode (LDAP_ADDR set) the pod is reachable via headless-service
// DNS and the returned cancel is a no-op.  In local mode a kubectl port-forward
// is started on localPort and must be torn down by the caller via cancel().
func dialPodLDAP(ns, podName, localPort string) (*ldap.Conn, context.CancelFunc) {
	if os.Getenv("LDAP_ADDR") != "" {
		// In-cluster: reach the pod directly over the headless service DNS.
		addr := fmt.Sprintf("%s.%s.%s.svc.cluster.local:1024", podName, ldapHeadlessSvc, ns)
		return connectLDAP(addr, baseDN, adminPW), func() {}
	}
	// Local: open a kubectl port-forward tunnel.
	cancel := startPortForward(ns, "pod/"+podName, localPort, "1024")
	time.Sleep(2 * time.Second)
	return connectLDAP("localhost:"+localPort, baseDN, adminPW), cancel
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
