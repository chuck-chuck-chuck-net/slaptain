package e2e_test

import (
	"fmt"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// Backup e2e: exercises the SlapdBackup path (ADR-014, Phases 2–3) end-to-end
// against an in-cluster versitygw S3 server. Gated by E2E_BACKUP=1.
//
// Prerequisites (the e2e.sh E2E_BACKUP flow applies these):
//   - versitygw deployed in the test namespace (tests/resources/versitygw.yaml),
//     exposing Service "versitygw":7480 and a Secret "versitygw-creds" with
//     access-key-id / secret-access-key, bucket "slaptain-backups" pre-created.
//   - The standard "slapd" cluster + SlapdDatabase already Running (BeforeSuite).
//
// Restore (Phases 4–5) is verified separately — it needs a second cluster
// lifecycle and endpoint and is run as a focused, iterated scenario.
var _ = Describe("backup", Label("backup"), Ordered, func() {
	const (
		backupName = "e2e-backup"
		bucket     = "slaptain-backups"
		credSecret = "versitygw-creds"
	)
	// Fully-qualified so it resolves from any namespace (the backup Job runs in
	// the workload namespace; the operator's inline S3 ops run in the operator's).
	endpoint := "http://versitygw." + namespace + ".svc:7480"

	BeforeAll(func(ctx SpecContext) {
		if os.Getenv("E2E_BACKUP") != "1" {
			Skip("E2E_BACKUP not set; skipping backup tests")
		}
		By("waiting for the versitygw S3 server to be ready")
		Eventually(ctx, func() bool {
			return deploymentReady(k8sClient, namespace, "versitygw")
		}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(BeTrue(),
			"versitygw Deployment not ready — apply tests/resources/versitygw.yaml")
	})

	AfterAll(func(ctx SpecContext) {
		if os.Getenv("E2E_BACKUP") != "1" {
			return
		}
		// Delete the SlapdBackup; its Job is garbage-collected via the owner ref.
		_ = crdClient.Delete(ctx, &ldapv1alpha1.SlapdBackup{
			ObjectMeta: metav1.ObjectMeta{Name: backupName, Namespace: namespace},
		})
	})

	It("completes an on-demand backup to S3", func(ctx SpecContext) {
		By("creating a SlapdBackup pointing at versitygw")
		sb := &ldapv1alpha1.SlapdBackup{
			ObjectMeta: metav1.ObjectMeta{Name: backupName, Namespace: namespace},
			Spec: ldapv1alpha1.SlapdBackupSpec{
				DatabaseRef: dbCRName,
				Storage: ldapv1alpha1.S3StorageSpec{
					Bucket:                bucket,
					Endpoint:              endpoint,
					Region:                "us-east-1",
					CredentialsSecretName: credSecret,
				},
			},
		}
		Expect(crdClient.Create(ctx, sb)).To(Succeed())

		By("waiting for the backup to reach phase Completed")
		var got ldapv1alpha1.SlapdBackup
		Eventually(ctx, func() ldapv1alpha1.SlapdBackupPhase {
			if err := crdClient.Get(ctx, client.ObjectKey{Name: backupName, Namespace: namespace}, &got); err != nil {
				return ""
			}
			return got.Status.Phase
		}).WithTimeout(5*time.Minute).WithPolling(5*time.Second).Should(Equal(ldapv1alpha1.BackupPhaseCompleted),
			"backup did not complete; check the %s-backup Job logs", backupName)

		By("asserting the artifact path and size were recorded")
		Expect(got.Status.Path).NotTo(BeEmpty(), "status.path should be the S3 object key")
		Expect(got.Status.Path).To(HaveSuffix(".ldif.gz"))
		Expect(got.Status.SizeBytes).To(BeNumerically(">", 0), "status.sizeBytes should be the uploaded artifact size")

		By("asserting the backup recorded WHERE it read from (ADR-014 amendment 2026-09-12)")
		// A backup is one pod's view of the DIT. Which pod, and whether that pod
		// was current with its peers, is recorded unconditionally — on every
		// topology, replicated or not. The contextCSN vector is asserted only in
		// restore_replay_test.go, which is gated on a replicated cluster: an
		// unreplicated database has no syncprov overlay and therefore no
		// contextCSN to record.
		Expect(got.Status.SourcePod).To(Equal("slapd-0"),
			"status.sourcePod should name the pod the artifact was read from")
		conv := findCondition(got.Status.Conditions, "SourceConverged")
		Expect(conv).NotTo(BeNil(),
			"a backup must always carry a SourceConverged condition, whatever it says")
		Expect(conv.Reason).NotTo(BeEmpty())
		fmt.Fprintf(GinkgoWriter, "backup source: pod=%s contextCSN=%v SourceConverged=%s (%s: %s)\n",
			got.Status.SourcePod, got.Status.SourceContextCSN, conv.Status, conv.Reason, conv.Message)
	})
})
