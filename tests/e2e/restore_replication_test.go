package e2e_test

import (
	"fmt"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// Replicated restore e2e (ADR-014 amendment, BACKUP-PLAN Phase 7.4): restores a
// backup into a REPLICATED cluster (replicas>1, replication enabled). This is
// the path the single-replica restore spec cannot reach — preflight's
// replication-password verification only runs when both the cluster and the
// database have replication enabled. It asserts:
//
//   1. Default-deny: a restore whose replication-password does NOT match the
//      backup's cn=replication hash fails preflight WITHOUT scaling down (no
//      downtime, no data touched).
//   2. Once the password matches, preflight passes and the slapadd-all-pods
//      machine loads the DIT onto EVERY RW pod directly (not via syncrepl).
//
// TLS note: the restore cluster reuses the source's TLS Secret, whose SANs cover
// only the source cluster's DNS — so cross-pod syncrepl will NOT verify and the
// cluster will not converge. That is deliberate and irrelevant here: preflight
// runs before scale-down (from S3 metadata + the credentials Secret), and
// slapadd-all-pods loads each pod independently, so both assertions hold without
// any replication convergence. Gated by E2E_BACKUP=1.
var _ = Describe("replicated restore", Label("restore"), Label("restore-replicated"), Ordered, func() {
	const (
		srcCluster     = "slapd"
		srcBackup      = "rrestore-src"
		restoreCluster = "slapd-rr"
		restoreDB      = "rrestore-db"
		credSecret     = "versitygw-creds"
		bucket         = "slaptain-backups"
		restoreRootPW  = "restore-root-pw-9f3a2b"
		wrongReplPW    = "intentionally-wrong-replication-password-000"
	)
	// Fully-qualified so it resolves from the operator's namespace (inline
	// preflight runs in the operator pod, not the workload namespace).
	endpoint := fmt.Sprintf("http://versitygw.%s.svc:7480", namespace)

	var (
		srcImages      ldapv1alpha1.SlapdImages
		srcPullSecrets []corev1.LocalObjectReference
		tlsSecretName  string
		restoreSuffix  string
		srcReplPW      string
		sourceCount    int
		nodeIP         string
	)

	BeforeAll(func(ctx SpecContext) {
		if os.Getenv("E2E_BACKUP") != "1" {
			Skip("E2E_BACKUP not set; skipping replicated restore tests")
		}
		nodeIP = os.Getenv("E2E_NODE_IP")
		Expect(nodeIP).NotTo(BeEmpty(), "E2E_NODE_IP must be set to reach the restore cluster's NodePorts")

		By("waiting for the versitygw S3 server to be ready")
		Eventually(ctx, func() bool {
			return deploymentReady(k8sClient, namespace, "versitygw")
		}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(BeTrue())

		By("cloning images, pull secrets, and TLS secret from the source cluster")
		src := &ldapv1alpha1.SlapdCluster{}
		Expect(crdClient.Get(ctx, client.ObjectKey{Name: srcCluster, Namespace: namespace}, src)).To(Succeed())
		srcImages = src.Spec.Images
		srcPullSecrets = src.Spec.ImagePullSecrets
		tlsSecretName = src.Spec.LDAP.TLS.SecretName
		Expect(tlsSecretName).NotTo(BeEmpty(), "source cluster must have TLS enabled for a replicated restore target")

		sd := &ldapv1alpha1.SlapdDatabase{}
		Expect(crdClient.Get(ctx, client.ObjectKey{Name: dbCRName, Namespace: namespace}, sd)).To(Succeed())
		restoreSuffix = sd.Spec.Suffix
		Expect(restoreSuffix).NotTo(BeEmpty())

		By("reading the source replication-password (the value the backup's hash was stamped from)")
		srcReplPW = readSecretKey(ctx, dbCRName+"-credentials", "replication-password")

		By("counting source DIT entries for later comparison")
		sourceCount = len(ldapSearch(ldapConn, restoreSuffix, "(objectClass=*)", "dn"))
		Expect(sourceCount).To(BeNumerically(">", 0))

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

		By("creating a replicated restore cluster + database with a DELIBERATELY WRONG replication-password")
		// Pre-create the credentials Secret so the operator adopts it instead of
		// generating a random password; this lets us control the value preflight
		// verifies against. Start with the wrong value for the default-deny case.
		_, err := k8sClient.CoreV1().Secrets(namespace).Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: restoreDB + "-credentials", Namespace: namespace},
			StringData: map[string]string{
				"root-password":        restoreRootPW,
				"replication-password": wrongReplPW,
			},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), "pre-create restore credentials Secret")

		Expect(crdClient.Create(ctx, &ldapv1alpha1.SlapdCluster{
			ObjectMeta: metav1.ObjectMeta{Name: restoreCluster, Namespace: namespace},
			Spec: ldapv1alpha1.SlapdClusterSpec{
				Images:           srcImages,
				ImagePullSecrets: srcPullSecrets,
				Replicas:         2,
				Replication: ldapv1alpha1.SlapdReplicationConfig{
					Enabled:      true,
					Mode:         "peer",
					ServerIDBase: 10,
				},
				LDAP: ldapv1alpha1.SlapdLDAPConfig{
					TLS: ldapv1alpha1.SlapdTLSConfig{Enabled: true, SecretName: tlsSecretName},
				},
				Persistence: ldapv1alpha1.SlapdPersistenceConfig{
					Config:    ldapv1alpha1.SlapdPVCConfig{Size: "1Gi"},
					Data:      ldapv1alpha1.SlapdPVCConfig{Size: "1Gi"},
					Accesslog: ldapv1alpha1.SlapdPVCConfig{Size: "1Gi"},
				},
			},
		})).To(Succeed())

		ridBase := int32(200)
		enabled := true
		Expect(crdClient.Create(ctx, &ldapv1alpha1.SlapdDatabase{
			ObjectMeta: metav1.ObjectMeta{Name: restoreDB, Namespace: namespace},
			Spec: ldapv1alpha1.SlapdDatabaseSpec{
				ClusterRef:  restoreCluster,
				Suffix:      restoreSuffix,
				Replication: ldapv1alpha1.DatabaseReplicationConfig{Enabled: &enabled, RIDBase: &ridBase},
				BootstrapFrom: &ldapv1alpha1.BootstrapSource{
					BackupRef: srcBackup,
				},
			},
		})).To(Succeed())
	})

	AfterAll(func(ctx SpecContext) {
		if os.Getenv("E2E_BACKUP") != "1" {
			return
		}
		_ = k8sClient.CoreV1().Services(namespace).Delete(ctx, restoreCluster+"-np-0", metav1.DeleteOptions{})
		_ = k8sClient.CoreV1().Services(namespace).Delete(ctx, restoreCluster+"-np-1", metav1.DeleteOptions{})
		_ = crdClient.Delete(ctx, &ldapv1alpha1.SlapdDatabase{ObjectMeta: metav1.ObjectMeta{Name: restoreDB, Namespace: namespace}})
		_ = crdClient.Delete(ctx, &ldapv1alpha1.SlapdCluster{ObjectMeta: metav1.ObjectMeta{Name: restoreCluster, Namespace: namespace}})
		_ = crdClient.Delete(ctx, &ldapv1alpha1.SlapdBackup{ObjectMeta: metav1.ObjectMeta{Name: srcBackup, Namespace: namespace}})
		_ = k8sClient.CoreV1().Secrets(namespace).Delete(ctx, restoreDB+"-credentials", metav1.DeleteOptions{})
		// volumeClaimTemplates PVCs are not garbage-collected with the StatefulSet.
		_ = k8sClient.CoreV1().PersistentVolumeClaims(namespace).DeleteCollection(ctx, metav1.DeleteOptions{},
			metav1.ListOptions{LabelSelector: "app.kubernetes.io/instance=" + restoreCluster})
	})

	It("default-denies a restore whose replication-password does not match the backup", func(ctx SpecContext) {
		By("waiting for preflight to reject the wrong replication-password")
		Eventually(ctx, func() string {
			sc := &ldapv1alpha1.SlapdCluster{}
			if err := crdClient.Get(ctx, client.ObjectKey{Name: restoreCluster, Namespace: namespace}, sc); err != nil || sc.Status.Restore == nil {
				return ""
			}
			return sc.Status.Restore.Message
		}).WithTimeout(6*time.Minute).WithPolling(5*time.Second).Should(ContainSubstring("could not verify the replication-password"),
			"preflight should default-deny the mismatched replication-password")

		By("confirming the cluster stays up — preflight never advances to scale-down")
		Consistently(ctx, func() ldapv1alpha1.SlapdClusterRestorePhase {
			sc := &ldapv1alpha1.SlapdCluster{}
			if err := crdClient.Get(ctx, client.ObjectKey{Name: restoreCluster, Namespace: namespace}, sc); err != nil || sc.Status.Restore == nil {
				return ""
			}
			return sc.Status.Restore.Phase
		}).WithTimeout(15*time.Second).WithPolling(3*time.Second).Should(Equal(ldapv1alpha1.RestorePreflight),
			"a failing preflight must not scale the cluster down (no downtime, no data loss)")
	})

	It("passes preflight and restores onto every RW pod once the password matches", func(ctx SpecContext) {
		By("patching the credentials Secret to the matching source replication-password")
		_, err := k8sClient.CoreV1().Secrets(namespace).Patch(ctx, restoreDB+"-credentials",
			types.MergePatchType,
			[]byte(fmt.Sprintf(`{"stringData":{"replication-password":%q}}`, srcReplPW)),
			metav1.PatchOptions{})
		Expect(err).NotTo(HaveOccurred(), "patch restore credentials to matching password")

		By("waiting for the restore to complete (preflight passes → scale-to-0 → slapadd-all-pods → scale-up)")
		Eventually(ctx, func() bool {
			sd := &ldapv1alpha1.SlapdDatabase{}
			if err := crdClient.Get(ctx, client.ObjectKey{Name: restoreDB, Namespace: namespace}, sd); err != nil {
				return false
			}
			return sd.Status.RestoreApplied
		}).WithTimeout(8*time.Minute).WithPolling(5*time.Second).Should(BeTrue(),
			"restore should reach restoreApplied=true once the password matches")

		By("waiting for the restore state machine to clear (cluster scaled back up)")
		Eventually(ctx, func() bool {
			sc := &ldapv1alpha1.SlapdCluster{}
			if err := crdClient.Get(ctx, client.ObjectKey{Name: restoreCluster, Namespace: namespace}, sc); err != nil {
				return false
			}
			return sc.Status.Restore == nil
		}).WithTimeout(3 * time.Minute).WithPolling(5 * time.Second).Should(BeTrue())

		// slapadd-all-pods: every RW pod is loaded directly from the artifact, so
		// each holds the full DIT independent of syncrepl convergence (which is
		// intentionally broken here by the reused TLS cert's SANs).
		By("verifying the DIT landed on EVERY RW pod directly (slapadd-all-pods, not syncrepl)")
		for _, ordinal := range []int{0, 1} {
			pod := fmt.Sprintf("%s-%d", restoreCluster, ordinal)
			addr := exposePodNodePort(ctx, restoreCluster, pod, fmt.Sprintf("%s-np-%d", restoreCluster, ordinal), nodeIP)
			conn := retryConnectLDAP(ctx, addr, restoreSuffix, restoreRootPW)
			func() {
				defer conn.Close()
				entries := ldapSearch(conn, restoreSuffix, "(objectClass=*)", "dn")
				Expect(entries).To(HaveLen(sourceCount), "%s should hold the full restored DIT", pod)
				Expect(ldapExists(conn, "ou=People,"+restoreSuffix)).To(BeTrue(), "ou=People should be on %s", pod)
			}()
		}
	})
})

// exposePodNodePort creates a NodePort Service targeting a single StatefulSet pod
// (by the well-known statefulset.kubernetes.io/pod-name label) and returns a
// host:port reachable via nodeIP. Unlike the cluster's per-pod NodePorts (which
// e2e.sh pre-creates only for the "slapd" cluster), the restore cluster needs
// these created on the fly to verify each pod independently.
func exposePodNodePort(ctx SpecContext, cluster, podName, svcName, nodeIP string) string {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: svcName, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeNodePort,
			Selector: map[string]string{
				"app.kubernetes.io/instance":         cluster,
				"statefulset.kubernetes.io/pod-name": podName,
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
	Expect(err).NotTo(HaveOccurred(), "create per-pod NodePort service for %s", podName)
	np := created.Spec.Ports[0].NodePort
	Expect(np).NotTo(BeZero(), "NodePort should be assigned for %s", podName)
	return fmt.Sprintf("%s:%d", nodeIP, np)
}
