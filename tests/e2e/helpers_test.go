package e2e_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// ── Kubernetes helpers ────────────────────────────────────────────────────────

func newK8sClient() *kubernetes.Clientset {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = os.ExpandEnv("$HOME/.kube/config")
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
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
