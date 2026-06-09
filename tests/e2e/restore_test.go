package e2e_test

import (
	"fmt"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// Restore e2e: exercises bootstrapFrom — restoring a backup into a FRESH cluster
// (ADR-014, Phases 4–5), including the cluster-coordinated scale-to-0 → offline
// slapadd → scale-up state machine. Gated by E2E_BACKUP=1 (shares the versitygw
// S3 target the backup specs deploy).
//
// Restoring into an *existing/populated* cluster (in-place rollback) is a
// separate, deferred feature (ADR-014) and is NOT covered here.
//
// The restore cluster (slapd-restore) is a single-replica, replication-off
// target — the minimal restore scenario. Data is verified over a NodePort the
// spec creates for it (E2E_NODE_IP is set by tests/e2e.sh).
var _ = Describe("restore", Label("restore"), Ordered, func() {
	const (
		srcBackup      = "restore-e2e-src"
		restoreCluster = "slapd-restore"
		restoreDB      = "restore-db"
		restoreNPSvc   = "slapd-restore-np"
		bucket         = "slaptain-backups"
		endpoint       = "http://versitygw:7480"
		credSecret     = "versitygw-creds"
	)

	var (
		srcImages      ldapv1alpha1.SlapdImages
		srcPullSecrets []corev1.LocalObjectReference
		restoreSuffix  string
		sourceCount    int
		nodeIP         string
	)

	BeforeAll(func(ctx SpecContext) {
		if os.Getenv("E2E_BACKUP") != "1" {
			Skip("E2E_BACKUP not set; skipping restore tests")
		}
		nodeIP = os.Getenv("E2E_NODE_IP")
		Expect(nodeIP).NotTo(BeEmpty(), "E2E_NODE_IP must be set to reach the restore cluster's NodePort")

		By("waiting for the versitygw S3 server to be ready")
		Eventually(ctx, func() bool {
			return deploymentReady(k8sClient, namespace, "versitygw")
		}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(BeTrue())

		By("cloning images + pull secrets + suffix from the source cluster")
		src := &ldapv1alpha1.SlapdCluster{}
		Expect(crdClient.Get(ctx, client.ObjectKey{Name: "slapd", Namespace: namespace}, src)).To(Succeed())
		srcImages = src.Spec.Images
		srcPullSecrets = src.Spec.ImagePullSecrets

		sd := &ldapv1alpha1.SlapdDatabase{}
		Expect(crdClient.Get(ctx, client.ObjectKey{Name: dbCRName, Namespace: namespace}, sd)).To(Succeed())
		restoreSuffix = sd.Spec.Suffix
		Expect(restoreSuffix).NotTo(BeEmpty())

		By("counting source DIT entries for later comparison")
		sourceCount = len(ldapSearch(ldapConn, restoreSuffix, "(objectClass=*)", "dn"))
		Expect(sourceCount).To(BeNumerically(">", 0))
	})

	AfterAll(func(ctx SpecContext) {
		if os.Getenv("E2E_BACKUP") != "1" {
			return
		}
		// Best-effort teardown of everything this spec created.
		_ = k8sClient.CoreV1().Services(namespace).Delete(ctx, restoreNPSvc, metav1.DeleteOptions{})
		_ = crdClient.Delete(ctx, &ldapv1alpha1.SlapdDatabase{ObjectMeta: metav1.ObjectMeta{Name: restoreDB, Namespace: namespace}})
		_ = crdClient.Delete(ctx, &ldapv1alpha1.SlapdCluster{ObjectMeta: metav1.ObjectMeta{Name: restoreCluster, Namespace: namespace}})
		_ = crdClient.Delete(ctx, &ldapv1alpha1.SlapdBackup{ObjectMeta: metav1.ObjectMeta{Name: srcBackup, Namespace: namespace}})
	})

	It("restores a backup into a fresh cluster via bootstrapFrom", func(ctx SpecContext) {
		By("creating a source backup and waiting for Completed")
		Expect(crdClient.Create(ctx, &ldapv1alpha1.SlapdBackup{
			ObjectMeta: metav1.ObjectMeta{Name: srcBackup, Namespace: namespace},
			Spec: ldapv1alpha1.SlapdBackupSpec{
				DatabaseRef: dbCRName,
				Storage: ldapv1alpha1.S3StorageSpec{
					Bucket: bucket, Endpoint: endpoint, Region: "us-east-1",
					CredentialsSecretName: credSecret,
				},
			},
		})).To(Succeed())
		Eventually(ctx, func() ldapv1alpha1.SlapdBackupPhase {
			b := &ldapv1alpha1.SlapdBackup{}
			if err := crdClient.Get(ctx, client.ObjectKey{Name: srcBackup, Namespace: namespace}, b); err != nil {
				return ""
			}
			return b.Status.Phase
		}).WithTimeout(5*time.Minute).WithPolling(5*time.Second).Should(Equal(ldapv1alpha1.BackupPhaseCompleted))

		By("creating a fresh restore cluster + database with bootstrapFrom")
		Expect(crdClient.Create(ctx, &ldapv1alpha1.SlapdCluster{
			ObjectMeta: metav1.ObjectMeta{Name: restoreCluster, Namespace: namespace},
			Spec: ldapv1alpha1.SlapdClusterSpec{
				Images:           srcImages,
				ImagePullSecrets: srcPullSecrets,
				Replicas:         1,
				Replication:      ldapv1alpha1.SlapdReplicationConfig{Enabled: false},
				LDAP:             ldapv1alpha1.SlapdLDAPConfig{TLS: ldapv1alpha1.SlapdTLSConfig{Enabled: false}},
				Persistence: ldapv1alpha1.SlapdPersistenceConfig{
					Config: ldapv1alpha1.SlapdPVCConfig{Size: "1Gi"},
					Data:   ldapv1alpha1.SlapdPVCConfig{Size: "1Gi"},
				},
			},
		})).To(Succeed())

		disabled := false
		Expect(crdClient.Create(ctx, &ldapv1alpha1.SlapdDatabase{
			ObjectMeta: metav1.ObjectMeta{Name: restoreDB, Namespace: namespace},
			Spec: ldapv1alpha1.SlapdDatabaseSpec{
				ClusterRef:  restoreCluster,
				Suffix:      restoreSuffix,
				Replication: ldapv1alpha1.DatabaseReplicationConfig{Enabled: &disabled},
				BootstrapFrom: &ldapv1alpha1.BootstrapSource{
					BackupRef: srcBackup,
				},
			},
		})).To(Succeed())

		By("waiting for the restore state machine to complete (scale-to-0 → slapadd → scale-up)")
		Eventually(ctx, func() bool {
			sd := &ldapv1alpha1.SlapdDatabase{}
			if err := crdClient.Get(ctx, client.ObjectKey{Name: restoreDB, Namespace: namespace}, sd); err != nil {
				return false
			}
			return sd.Status.RestoreApplied
		}).WithTimeout(8*time.Minute).WithPolling(5*time.Second).Should(BeTrue(),
			"restore-db should reach restoreApplied=true")

		By("waiting for the cluster to return to Running with restore cleared")
		Eventually(ctx, func() bool {
			sc := &ldapv1alpha1.SlapdCluster{}
			if err := crdClient.Get(ctx, client.ObjectKey{Name: restoreCluster, Namespace: namespace}, sc); err != nil {
				return false
			}
			return sc.Status.Phase == ldapv1alpha1.PhaseRunning && sc.Status.Restore == nil
		}).WithTimeout(3*time.Minute).WithPolling(5*time.Second).Should(BeTrue())

		By("verifying the restored DIT matches the source")
		addr := exposeRestoreNodePort(ctx, restoreCluster, restoreNPSvc, nodeIP)
		credPW := readSecretKey(ctx, restoreDB+"-credentials", "root-password")
		conn := retryConnectLDAP(ctx, addr, restoreSuffix, credPW)
		defer conn.Close()

		entries := ldapSearch(conn, restoreSuffix, "(objectClass=*)", "dn")
		Expect(entries).To(HaveLen(sourceCount), "restored entry count should equal the source")
		Expect(ldapExists(conn, "ou=People,"+restoreSuffix)).To(BeTrue(), "ou=People should be restored")
	})
})

// exposeRestoreNodePort creates a NodePort Service for the restore cluster's RW
// pods and returns a host:port reachable via E2E_NODE_IP.
func exposeRestoreNodePort(ctx SpecContext, cluster, svcName, nodeIP string) string {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: svcName, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeNodePort,
			Selector: map[string]string{
				"app.kubernetes.io/name":     "slapd",
				"app.kubernetes.io/instance": cluster,
			},
			Ports: []corev1.ServicePort{{
				Name:       "ldap",
				Port:       389,
				TargetPort: intstr.FromInt(1024),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
	created, err := k8sClient.CoreV1().Services(namespace).Create(ctx, svc, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "create restore NodePort service")
	np := created.Spec.Ports[0].NodePort
	Expect(np).NotTo(BeZero(), "NodePort should be assigned")
	return fmt.Sprintf("%s:%d", nodeIP, np)
}

// readSecretKey reads a single key from a Secret in the test namespace.
func readSecretKey(ctx SpecContext, name, key string) string {
	s, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred(), "get secret %s", name)
	v := string(s.Data[key])
	Expect(v).NotTo(BeEmpty(), "secret %s key %s must not be empty", name, key)
	return v
}
