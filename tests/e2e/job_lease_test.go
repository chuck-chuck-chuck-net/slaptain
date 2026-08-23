package e2e_test

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// ADR-018: any pod object naming a PVC blocks that PVC's deletion for as long
// as the object exists — finished pods included. So every co-located Job the
// operator creates is a lease on slapd PVC lifecycle, and the operator must reap
// it as soon as the work is done (R1, R2). A leaked lease silently blocks the
// ADR-012 case-2 recovery path ("a pod lost its volumes; let syncrepl refill
// it"), whose only symptom is a StatefulSet FailedCreate event.
//
// Gated by E2E_BACKUP=1 (needs the versitygw S3 target).
var _ = Describe("job PVC leases (ADR-018)", Label("backup"), Label("job-lease"), Ordered, func() {
	const (
		backupName = "e2e-lease-backup"
		bucket     = "slaptain-backups"
		credSecret = "versitygw-creds"
	)
	endpoint := "http://versitygw." + namespace + ".svc:7480"
	jobName := backupName + "-backup"

	BeforeAll(func(ctx SpecContext) {
		if os.Getenv("E2E_BACKUP") != "1" {
			Skip("E2E_BACKUP not set; skipping ADR-018 job lease tests")
		}
		By("waiting for the versitygw S3 server to be ready")
		Eventually(ctx, func() bool {
			return deploymentReady(k8sClient, namespace, "versitygw")
		}).WithTimeout(2*time.Minute).WithPolling(5*time.Second).Should(BeTrue(),
			"versitygw Deployment not ready — apply tests/resources/versitygw.yaml")
	})

	AfterAll(func(ctx SpecContext) {
		if os.Getenv("E2E_BACKUP") != "1" {
			return
		}
		_ = crdClient.Delete(ctx, &ldapv1alpha1.SlapdBackup{
			ObjectMeta: metav1.ObjectMeta{Name: backupName, Namespace: namespace},
		})
	})

	It("reaps a completed backup Job and its pods", func(ctx SpecContext) {
		By("creating a SlapdBackup")
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
		Eventually(ctx, func() ldapv1alpha1.SlapdBackupPhase {
			var got ldapv1alpha1.SlapdBackup
			if err := crdClient.Get(ctx, client.ObjectKey{Name: backupName, Namespace: namespace}, &got); err != nil {
				return ""
			}
			return got.Status.Phase
		}).WithTimeout(5*time.Minute).WithPolling(5*time.Second).Should(Equal(ldapv1alpha1.BackupPhaseCompleted),
			"backup did not complete")

		By("asserting the operator reaped the Job once the backup was Completed")
		Eventually(ctx, func() bool {
			_, err := k8sClient.BatchV1().Jobs(namespace).Get(ctx, jobName, metav1.GetOptions{})
			return apierrors.IsNotFound(err)
		}).WithTimeout(90*time.Second).WithPolling(3*time.Second).Should(BeTrue(),
			"backup Job %s still exists after the backup reached Completed — its pod holds a "+
				"deletion lease on the slapd PVCs (ADR-018 R1/R2)", jobName)

		By("asserting the Job's pods are gone too (background propagation, not orphaned)")
		Eventually(ctx, func() int {
			pods, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: "job-name=" + jobName,
			})
			if err != nil {
				return -1
			}
			return len(pods.Items)
		}).WithTimeout(90*time.Second).WithPolling(3*time.Second).Should(BeZero(),
			"pods of Job %s survived the Job deletion — a Job deleted with Orphan propagation "+
				"leaves its pods, and the pod is what holds the PVC lease (ADR-018)", jobName)
	})

	// The invariant that generalises past backup: a pod that has finished has no
	// business holding a slapd PVC. In-flight (Pending/Running) pods legitimately
	// hold leases while they work, so only terminal pods are violations — which
	// also makes this check safe to run alongside other specs' live Jobs.
	It("leaves no terminated pod holding a slapd PVC", func(ctx SpecContext) {
		Eventually(ctx, func() []string {
			return terminatedPodsHoldingSlapdPVCs(ctx)
		}).WithTimeout(2*time.Minute).WithPolling(5*time.Second).Should(BeEmpty(),
			"terminated pods still reference slapd PVCs; each one blocks deletion of those "+
				"PVCs and therefore blocks ADR-012 case-2 recovery for that pod (ADR-018)")
	})
})

// slapdPVCPattern matches the StatefulSet volumeClaimTemplate PVC names
// (`<template>-<sts>-<ordinal>`) for the three slapd volumes.
var slapdPVCPattern = regexp.MustCompile(`^(config|data|accesslog)-.+-\d+$`)

// terminatedPodsHoldingSlapdPVCs returns "pod -> pvc" descriptions for every
// Succeeded/Failed pod that still names a slapd PVC. Reporting the holder by
// name is ADR-018 R6: a bare timeout is not an acceptable diagnostic for this
// class of failure.
func terminatedPodsHoldingSlapdPVCs(ctx SpecContext) []string {
	pods, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return []string{"list pods: " + err.Error()}
	}
	var found []string
	for _, p := range pods.Items {
		if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
			continue
		}
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim == nil {
				continue
			}
			if slapdPVCPattern.MatchString(v.PersistentVolumeClaim.ClaimName) {
				found = append(found, fmt.Sprintf("%s (%s) holds %s",
					p.Name, strings.ToLower(string(p.Status.Phase)), v.PersistentVolumeClaim.ClaimName))
			}
		}
	}
	return found
}

// podsReferencingPVC names every pod that currently references the PVC, with its
// phase. Any such pod — Running or long finished — holds
// `kubernetes.io/pvc-protection` on it, so this is the answer to "why is my PVC
// stuck in Terminating" (ADR-018 R6).
func podsReferencingPVC(ctx SpecContext, pvcName string) []string {
	pods, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return []string{"list pods: " + err.Error()}
	}
	var holders []string
	for _, p := range pods.Items {
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == pvcName {
				holders = append(holders, fmt.Sprintf("%s (%s)", p.Name, strings.ToLower(string(p.Status.Phase))))
			}
		}
	}
	if len(holders) == 0 {
		return []string{"no pod references it (finalizer should clear shortly)"}
	}
	return holders
}
