package e2e_test

import (
	"fmt"
	"os"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// In-place restore e2e (ADR-014 amendment, BACKUP-PLAN Phase 7.3/7.4): a
// SlapdRestore rolls an EXISTING, populated database back to a backup. This
// exercises the imperative path — the SlapdCluster controller picks up the
// SlapdRestore (no separate reconciler) and drives the same scale-to-0 →
// slapadd-all-pods → scale-up machine as bootstrapFrom.
//
// Scenario: bootstrap a fresh cluster from a known backup (so its DIT is
// known), write a "mistake" entry, then SlapdRestore back to that same backup
// and assert the mistake is gone and the original DIT is intact. Single-replica
// (the multi-pod slapadd is covered by restore_replication_test.go); this spec
// focuses on the SlapdRestore CRD mechanics. Gated by E2E_BACKUP=1.
var _ = Describe("in-place restore", Label("restore"), Label("restore-inplace"), Ordered, func() {
	const (
		srcCluster     = "slapd"
		srcBackup      = "inplace-src"
		restoreCluster = "slapd-ip"
		restoreDB      = "ip-db"
		restoreNPSvc   = "slapd-ip-np"
		restoreReq     = "ip-rollback"
		bucket         = "slaptain-backups"
		credSecret     = "versitygw-creds"
	)
	endpoint := fmt.Sprintf("http://versitygw.%s.svc:7480", namespace)

	var (
		srcImages      ldapv1alpha1.SlapdImages
		srcPullSecrets []corev1.LocalObjectReference
		restoreSuffix  string
		sourceCount    int
		nodeIP         string
		markerDN       string
		anySpecFailed  bool
	)

	BeforeAll(func(ctx SpecContext) {
		if os.Getenv("E2E_BACKUP") != "1" {
			Skip("E2E_BACKUP not set; skipping in-place restore tests")
		}
		nodeIP = os.Getenv("E2E_NODE_IP")
		Expect(nodeIP).NotTo(BeEmpty(), "E2E_NODE_IP must be set to reach the restore cluster's NodePort")

		By("waiting for the versitygw S3 server to be ready")
		Eventually(ctx, func() bool {
			return deploymentReady(k8sClient, namespace, "versitygw")
		}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(BeTrue())

		By("cloning images + pull secrets + suffix from the source cluster")
		src := &ldapv1alpha1.SlapdCluster{}
		Expect(crdClient.Get(ctx, client.ObjectKey{Name: srcCluster, Namespace: namespace}, src)).To(Succeed())
		srcImages = src.Spec.Images
		srcPullSecrets = src.Spec.ImagePullSecrets

		sd := &ldapv1alpha1.SlapdDatabase{}
		Expect(crdClient.Get(ctx, client.ObjectKey{Name: dbCRName, Namespace: namespace}, sd)).To(Succeed())
		restoreSuffix = sd.Spec.Suffix
		Expect(restoreSuffix).NotTo(BeEmpty())
		markerDN = "uid=rollback-marker,ou=People," + restoreSuffix

		By("creating the known-good source backup and waiting for Completed")
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
		}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(Equal(ldapv1alpha1.BackupPhaseCompleted))
	})

	// See restore_test.go for why the autopsy runs here and not after cleanup.
	AfterEach(func(ctx SpecContext) {
		if os.Getenv("E2E_BACKUP") != "1" || !CurrentSpecReport().Failed() {
			return
		}
		anySpecFailed = true
		dumpRestoreAutopsy(ctx, restoreCluster, restoreDB)
	})

	AfterAll(func(ctx SpecContext) {
		if os.Getenv("E2E_BACKUP") != "1" {
			return
		}
		if (anySpecFailed || CurrentSpecReport().Failed()) && keepOnFailure() {
			reportKeptRestoreState(restoreCluster, restoreDB,
				[]string{restoreNPSvc}, []string{srcBackup})
			return
		}
		_ = k8sClient.CoreV1().Services(namespace).Delete(ctx, restoreNPSvc, metav1.DeleteOptions{})
		_ = crdClient.Delete(ctx, &ldapv1alpha1.SlapdRestore{ObjectMeta: metav1.ObjectMeta{Name: restoreReq, Namespace: namespace}})
		_ = crdClient.Delete(ctx, &ldapv1alpha1.SlapdDatabase{ObjectMeta: metav1.ObjectMeta{Name: restoreDB, Namespace: namespace}})
		_ = crdClient.Delete(ctx, &ldapv1alpha1.SlapdCluster{ObjectMeta: metav1.ObjectMeta{Name: restoreCluster, Namespace: namespace}})
		_ = crdClient.Delete(ctx, &ldapv1alpha1.SlapdBackup{ObjectMeta: metav1.ObjectMeta{Name: srcBackup, Namespace: namespace}})
		deleteRestorePVCs(ctx, restoreCluster)
	})

	It("rolls an existing database back to a backup via SlapdRestore", func(ctx SpecContext) {
		By("bootstrapping a fresh cluster from the known-good backup")
		disabled := false
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
		Expect(crdClient.Create(ctx, &ldapv1alpha1.SlapdDatabase{
			ObjectMeta: metav1.ObjectMeta{Name: restoreDB, Namespace: namespace},
			Spec: ldapv1alpha1.SlapdDatabaseSpec{
				ClusterRef:    restoreCluster,
				Suffix:        restoreSuffix,
				Replication:   ldapv1alpha1.DatabaseReplicationConfig{Enabled: &disabled},
				BootstrapFrom: &ldapv1alpha1.BootstrapSource{BackupRef: srcBackup},
			},
		})).To(Succeed())

		By("waiting for the bootstrap restore to complete")
		Eventually(ctx, func() bool {
			sd := &ldapv1alpha1.SlapdDatabase{}
			if err := crdClient.Get(ctx, client.ObjectKey{Name: restoreDB, Namespace: namespace}, sd); err != nil {
				return false
			}
			return sd.Status.RestoreApplied
		}).WithTimeout(8 * time.Minute).WithPolling(5 * time.Second).Should(BeTrue())
		Eventually(ctx, func() bool {
			sc := &ldapv1alpha1.SlapdCluster{}
			if err := crdClient.Get(ctx, client.ObjectKey{Name: restoreCluster, Namespace: namespace}, sc); err != nil {
				return false
			}
			return sc.Status.Phase == ldapv1alpha1.PhaseRunning && sc.Status.Restore == nil
		}).WithTimeout(3 * time.Minute).WithPolling(5 * time.Second).Should(BeTrue())

		addr := exposeRestoreNodePort(ctx, restoreCluster, restoreNPSvc, nodeIP)
		adminPWip := readSecretKey(ctx, restoreDB+"-credentials", "root-password")

		// The baseline has to be read from the restore cluster itself, after its
		// bootstrap completed: that is the state this rollback must return to.
		// Reading it from the shared source cluster instead compares two different
		// DITs — the artifact is a slapcat of the source's pod-0, while a
		// ClusterIP read lands on an arbitrary pod, so the two differ by any entry
		// that has not yet converged. That made this spec fail on the
		// "mistake entry landed" assertion, before the rollback ever ran, which
		// says nothing about SlapdRestore either way.
		By("counting the restored DIT on the restore cluster (the state to roll back to)")
		conn := retryConnectLDAP(ctx, addr, restoreSuffix, adminPWip)
		sourceCount = len(ldapSearch(conn, restoreSuffix, "(objectClass=*)", "dn"))
		Expect(sourceCount).To(BeNumerically(">", 0))

		By("writing a 'mistake' entry to the populated database")
		add := ldap.NewAddRequest(markerDN, nil)
		add.Attribute("objectClass", []string{"inetOrgPerson"})
		add.Attribute("cn", []string{"rollback marker"})
		add.Attribute("sn", []string{"marker"})
		Expect(conn.Add(add)).To(Succeed(), "add mistake entry")
		Expect(ldapExists(conn, markerDN)).To(BeTrue(), "mistake entry should be present before rollback")
		Expect(ldapSearch(conn, restoreSuffix, "(objectClass=*)", "dn")).To(HaveLen(sourceCount + 1))
		conn.Close()

		By("requesting an in-place rollback to the known-good backup via SlapdRestore")
		Expect(crdClient.Create(ctx, &ldapv1alpha1.SlapdRestore{
			ObjectMeta: metav1.ObjectMeta{Name: restoreReq, Namespace: namespace},
			Spec: ldapv1alpha1.SlapdRestoreSpec{
				DatabaseRef: restoreDB,
				Source:      ldapv1alpha1.RestoreSource{BackupRef: srcBackup},
			},
		})).To(Succeed())

		By("waiting for the SlapdRestore to reach Completed")
		Eventually(ctx, func() ldapv1alpha1.SlapdRestorePhase {
			sr := &ldapv1alpha1.SlapdRestore{}
			if err := crdClient.Get(ctx, client.ObjectKey{Name: restoreReq, Namespace: namespace}, sr); err != nil {
				return ""
			}
			return sr.Status.Phase
		}).WithTimeout(8*time.Minute).WithPolling(5*time.Second).Should(Equal(ldapv1alpha1.RestoreRequestCompleted),
			"SlapdRestore should complete; check the cluster status.restore and restore Job logs")

		By("verifying the mistake is gone and the original DIT is intact")
		// The cluster scaled down+up during the restore; reconnect.
		conn2 := retryConnectLDAP(ctx, addr, restoreSuffix, adminPWip)
		defer conn2.Close()
		Expect(ldapExists(conn2, markerDN)).To(BeFalse(), "mistake entry should be rolled back")
		Expect(ldapSearch(conn2, restoreSuffix, "(objectClass=*)", "dn")).To(HaveLen(sourceCount),
			"DIT should match the known-good backup after rollback")

		By("confirming the database's restoreApplied was NOT flipped by the in-place restore")
		sd := &ldapv1alpha1.SlapdDatabase{}
		Expect(crdClient.Get(ctx, client.ObjectKey{Name: restoreDB, Namespace: namespace}, sd)).To(Succeed())
		Expect(sd.Status.RestoreApplied).To(BeTrue(),
			"restoreApplied reflects the original bootstrapFrom; an in-place SlapdRestore must not change it")
	})
})
