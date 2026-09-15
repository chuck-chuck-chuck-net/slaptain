package e2e_test

import (
	"context"
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

// findCondition returns the named condition, or nil.
func findCondition(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}

// artifactGrepExitCode greps the uploaded backup artifact for needle and returns
// the grep exit code (0 = found, 1 = absent, 3 = artifact missing) plus a
// human-readable detail string.
//
// It reads the object out of versitygw's POSIX backend rather than over S3: the
// backend is an emptyDir inside the versitygw pod, so an ephemeral container on
// that pod — busybox, the same image the Deployment's own init container uses —
// can mount it and stream the gzip. That keeps the e2e module free of an S3
// client and of AWS request signing, at the cost of being versitygw-specific,
// which the whole backup e2e already is.
func artifactGrepExitCode(ctx context.Context, bucket, objectKey, needle string) (int32, string) {
	GinkgoHelper()
	Expect(objectKey).NotTo(BeEmpty(), "status.path must name the artifact before it can be inspected")

	pods, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=versitygw",
	})
	Expect(err).NotTo(HaveOccurred(), "list versitygw pods")
	var podName string
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning {
			podName = p.Name
			break
		}
	}
	Expect(podName).NotTo(BeEmpty(), "no Running versitygw pod to read the artifact from")

	// versitygw's posix backend stores <root>/<bucket>/<key>; the root is /data.
	path := "/data/" + bucket + "/" + objectKey
	ecName := fmt.Sprintf("artifact-grep-%d", time.Now().UnixNano()%1000000)

	pod, err := k8sClient.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred(), "get versitygw pod")
	pod.Spec.EphemeralContainers = append(pod.Spec.EphemeralContainers, corev1.EphemeralContainer{
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name:  ecName,
			Image: "busybox:stable",
			// Distinguish "not in the artifact" (grep's 1) from "no artifact"
			// (3) — the second would mean the check itself is broken.
			Command: []string{"sh", "-c",
				`test -f "$F" || { echo "artifact not found: $F" >&2; exit 3; }; gzip -dc "$F" | grep -q -- "$N"`},
			Env: []corev1.EnvVar{
				{Name: "F", Value: path},
				{Name: "N", Value: needle},
			},
			VolumeMounts:             []corev1.VolumeMount{{Name: "data", MountPath: "/data", ReadOnly: true}},
			TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		},
	})
	_, err = k8sClient.CoreV1().Pods(namespace).UpdateEphemeralContainers(ctx, podName, pod, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred(), "attach artifact-inspection container %s to %s", ecName, podName)

	var term *corev1.ContainerStateTerminated
	Eventually(ctx, func() bool {
		p, err := k8sClient.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return false
		}
		for _, st := range p.Status.EphemeralContainerStatuses {
			if st.Name == ecName && st.State.Terminated != nil {
				term = st.State.Terminated
				return true
			}
		}
		return false
	}).WithTimeout(2*time.Minute).WithPolling(3*time.Second).Should(BeTrue(),
		"artifact-inspection container %s never terminated", ecName)

	return term.ExitCode, fmt.Sprintf("reason=%s message=%q path=%s", term.Reason, term.Message, path)
}

// Regression for the "stale accesslog replay undoes an in-place restore" bug
// (ADR-014; docs/reconcile-loop-fixes.md). Under multi-master delta-syncrepl an
// in-place SlapdRestore reloads the main DB but MUST also wipe the accesslog
// journal. If a pre-restore delta survives — most damagingly a delete at a CSN
// newer than the restored contextCSN — syncrepl replays it on scale-up and
// silently undoes the restore.
//
// This runs against the primary (replicated, TLS, working syncrepl) cluster —
// the only place in the suite where the syncrepl mesh actually converges, so the
// replay path is real (the dedicated restore clusters reuse the source TLS cert
// and deliberately do NOT converge). Scenario:
//
//	add marker → backup (marker IS in the artifact) → delete marker (accesslog
//	gets a delete at a newer CSN, propagated to all RW pods) → in-place
//	SlapdRestore to that backup → marker must be present on EVERY RW pod AND
//	stay present across a syncrepl settle window.
//
// Both marker writes go DIRECTLY to the backup's source pod (pod-0), not through
// the cluster Service. A backup slapcats pod-0, so a write that the Service
// routed to another pod need not be in the artifact yet — on 2026-09-12 exactly
// that happened on a freshly created cluster (the write landed on slapd-2 while
// pod-0's consumer sessions were still in their boot retry window), the artifact
// came out marker-less, the restore faithfully restored it, and this spec failed
// for a reason that has nothing to do with what it guards. Writing to the source
// pod removes replication freshness from the premise; the premise is then
// asserted anyway (marker present on every RW pod, and present in the artifact)
// before the spec proceeds. See ADR-014 amendment 2026-09-12.
//
// Against the buggy code the marker reappears then vanishes within seconds as
// the surviving accesslog delete replays. Gated by E2E_BACKUP=1 (needs versitygw)
// and skipped unless the primary cluster is actually replicated.
var _ = Describe("in-place restore under replication (accesslog replay)", Label("restore"), Label("restore-replay"), Ordered, func() {
	const (
		backupName = "replay-src"
		restoreReq = "replay-rollback"
		bucket     = "slaptain-backups"
		credSecret = "versitygw-creds"
	)
	endpoint := fmt.Sprintf("http://versitygw.%s.svc:7480", namespace)

	var (
		markerDN      string
		rwPods        []string
		anySpecFailed bool
	)

	BeforeAll(func(ctx SpecContext) {
		if os.Getenv("E2E_BACKUP") != "1" {
			Skip("E2E_BACKUP not set; skipping accesslog-replay regression")
		}

		By("requiring a replicated primary cluster (the replay path needs an accesslog + working syncrepl)")
		sc := &ldapv1alpha1.SlapdCluster{}
		Expect(crdClient.Get(ctx, client.ObjectKey{Name: "slapd", Namespace: namespace}, sc)).To(Succeed())
		if sc.Spec.Replicas < 2 || !sc.Spec.Replication.Enabled {
			Skip("primary cluster is not replicated; accesslog-replay regression is not applicable")
		}
		// This spec asserts rollback semantics, which SlapdRestore only provides
		// when no replica outside the restore's scope holds post-backup deltas.
		// With external peers it does not: slapadd restores each entry with its
		// original CSN, so the peers' newer changes win and are replayed back as
		// each pod rejoins the mesh — the cluster converges to the mesh's state
		// and the restore is a local re-seed, by design (ADR-014, amendment
		// 2026-08-24). Asserting a rollback here would be asserting something we
		// deliberately do not promise, so skip rather than fail.
		//
		// The regression this spec guards (the local accesslog wipe) is real and
		// still needs permanent coverage on an N>=2 cluster with no external
		// peers. That is blocked on the e2e framework refactor — see
		// docs/BACKLOG.md, "Permanent e2e coverage for in-place restore
		// topologies". Verified manually in the meantime.
		if len(sc.Spec.Replication.ExternalPeers) > 0 {
			Skip(fmt.Sprintf("primary cluster has %d external peer(s): in-place restore is a local "+
				"re-seed there, not a rollback (ADR-014 amendment) — this spec needs a cluster "+
				"with no external peers; see docs/BACKLOG.md",
				len(sc.Spec.Replication.ExternalPeers)))
		}
		rwPods = nil
		for i := int32(0); i < sc.Spec.Replicas; i++ {
			rwPods = append(rwPods, fmt.Sprintf("slapd-%d", i))
		}

		By("waiting for the versitygw S3 server to be ready")
		Eventually(ctx, func() bool {
			return deploymentReady(k8sClient, namespace, "versitygw")
		}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(BeTrue())

		markerDN = "uid=replay-marker,ou=People," + baseDN
	})

	// See restore_test.go. This spec runs against the SHARED primary cluster, so
	// the autopsy targets it — but note the difference in what "keep" means here:
	// keeping means the marker entry and the SlapdRestore stay behind on the
	// shared fixture, which is deliberate (the marker's presence/absence IS the
	// evidence) but does leave the fixture off baseline.
	AfterEach(func(ctx SpecContext) {
		if os.Getenv("E2E_BACKUP") != "1" || !CurrentSpecReport().Failed() {
			return
		}
		anySpecFailed = true
		dumpRestoreAutopsy(ctx, "slapd", dbCRName)
	})

	AfterAll(func(ctx SpecContext) {
		if os.Getenv("E2E_BACKUP") != "1" {
			return
		}
		// BeforeAll assigns markerDN LAST, after three Skip exits (not
		// replicated; external peers present — ADR-014 amendment). On any of
		// those paths nothing was created, and this AfterAll — gated only on
		// E2E_BACKUP — used to run anyway with markerDN == "": conn.Del("") is
		// a delete of the ROOT DSE, slapd answers err 53, and the handler
		// printed "!!! CLEANUP FAILED ... The shared fixture is off baseline;
		// delete it by hand" — sending a human hunting for fixture drift that
		// does not exist. Observed on every multi-site run (the external-peers
		// skip), most recently 2026-09-14.
		if !replayCleanupNeeded(markerDN) {
			return
		}
		if (anySpecFailed || CurrentSpecReport().Failed()) && keepOnFailure() {
			fmt.Fprintf(GinkgoWriter,
				"\n=== KEEPING FAILED REPLAY STATE: marker %s, SlapdRestore %s, SlapdBackup %s "+
					"left on the SHARED cluster (E2E_KEEP_ON_FAILURE=0 to tear down) ===\n"+
					"  slctl debug-dump -n %s slapd\n",
				markerDN, restoreReq, backupName, namespace)
			return
		}
		// Remove the marker so the shared cluster returns to baseline.
		//
		// NOT via the suite's ldapConn: the restore's scale-to-0 tore every pod
		// down under it, so that connection is dead by now and the delete would
		// be a silent no-op (its error was also discarded, which is how the
		// marker survived unnoticed into later specs). Re-dial pod-0 and report
		// what happened — a leaked marker is fixture drift, not a detail.
		conn, cancel := dialPodLDAP(namespace, "slapd-0", "")
		defer cancel()
		defer conn.Close()
		if err := conn.Del(ldap.NewDelRequest(markerDN, nil)); err != nil &&
			!ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
			fmt.Fprintf(GinkgoWriter,
				"\n!!! CLEANUP FAILED: could not delete the replay marker %s: %v\n"+
					"    The shared fixture is off baseline; delete it by hand.\n", markerDN, err)
		}
		_ = crdClient.Delete(ctx, &ldapv1alpha1.SlapdRestore{ObjectMeta: metav1.ObjectMeta{Name: restoreReq, Namespace: namespace}})
		_ = crdClient.Delete(ctx, &ldapv1alpha1.SlapdBackup{ObjectMeta: metav1.ObjectMeta{Name: backupName, Namespace: namespace}})
	})

	It("keeps a restored entry after syncrepl settles", func(ctx SpecContext) {
		By("adding a marker entry DIRECTLY to the backup's source pod (slapd-0)")
		srcConn, srcCancel := dialPodLDAP(namespace, "slapd-0", "")
		defer srcCancel()
		defer srcConn.Close()
		add := ldap.NewAddRequest(markerDN, nil)
		add.Attribute("objectClass", []string{"inetOrgPerson"})
		add.Attribute("cn", []string{"replay marker"})
		add.Attribute("sn", []string{"marker"})
		Expect(srcConn.Add(add)).To(Succeed(), "add marker entry on slapd-0")
		Expect(ldapExists(srcConn, markerDN)).To(BeTrue(), "marker should be present on slapd-0 before the backup")

		By("waiting for the marker to reach every RW pod (the premise: the mesh carries it)")
		for _, pod := range rwPods {
			pod := pod
			Eventually(ctx, func() bool {
				conn, cancel := dialPodLDAP(namespace, pod, "")
				defer cancel()
				defer conn.Close()
				return ldapExists(conn, markerDN)
			}).WithTimeout(2*time.Minute).WithPolling(3*time.Second).Should(BeTrue(),
				"marker should replicate to %s before the backup", pod)
		}

		By("backing up the database (the marker is captured in the artifact)")
		Expect(crdClient.Create(ctx, &ldapv1alpha1.SlapdBackup{
			ObjectMeta: metav1.ObjectMeta{Name: backupName, Namespace: namespace},
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
			if err := crdClient.Get(ctx, client.ObjectKey{Name: backupName, Namespace: namespace}, b); err != nil {
				return ""
			}
			return b.Status.Phase
		}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(Equal(ldapv1alpha1.BackupPhaseCompleted))

		By("asserting the backup recorded its source circumstances (ADR-014 amendment 2026-09-12)")
		done := &ldapv1alpha1.SlapdBackup{}
		Expect(crdClient.Get(ctx, client.ObjectKey{Name: backupName, Namespace: namespace}, done)).To(Succeed())
		Expect(done.Status.SourcePod).To(Equal("slapd-0"),
			"status.sourcePod must name the pod the artifact was read from")
		Expect(done.Status.SourceContextCSN).NotTo(BeEmpty(),
			"status.sourceContextCSN must pin the artifact's place in the replication timeline")
		conv := findCondition(done.Status.Conditions, "SourceConverged")
		Expect(conv).NotTo(BeNil(), "a backup must record whether its source was converged")
		fmt.Fprintf(GinkgoWriter, "backup source: pod=%s contextCSN=%v SourceConverged=%s (%s: %s)\n",
			done.Status.SourcePod, done.Status.SourceContextCSN, conv.Status, conv.Reason, conv.Message)

		By("asserting the ARTIFACT itself contains the marker (not just the live DIT)")
		// Promoted from the 2026-09-12 forensics: the artifact, not the source
		// pod's current state, is what a restore replays. If this fails, nothing
		// downstream in this spec means anything.
		code, detail := artifactGrepExitCode(ctx, bucket, done.Status.Path, "uid=replay-marker")
		Expect(code).To(BeEquivalentTo(0),
			"the backup artifact %s must contain the marker (grep exit %d: %s); "+
				"exit 1 = absent, 3 = artifact not found in the S3 backend",
			done.Status.Path, code, detail)

		By("deleting the marker on slapd-0 (accesslog records a delete at a CSN newer than the backup)")
		Expect(srcConn.Del(ldap.NewDelRequest(markerDN, nil))).To(Succeed(), "delete marker entry on slapd-0")

		By("waiting for the delete to propagate to every RW pod (so a stale accesslog exists on all)")
		for _, pod := range rwPods {
			pod := pod
			Eventually(ctx, func() bool {
				conn, cancel := dialPodLDAP(namespace, pod, "")
				defer cancel()
				defer conn.Close()
				return ldapExists(conn, markerDN)
			}).WithTimeout(2*time.Minute).WithPolling(3*time.Second).Should(BeFalse(),
				"delete of the marker should replicate to %s before the restore", pod)
		}

		By("requesting an in-place SlapdRestore back to the marker-containing backup")
		Expect(crdClient.Create(ctx, &ldapv1alpha1.SlapdRestore{
			ObjectMeta: metav1.ObjectMeta{Name: restoreReq, Namespace: namespace},
			Spec: ldapv1alpha1.SlapdRestoreSpec{
				DatabaseRef: dbCRName,
				Source:      ldapv1alpha1.RestoreSource{BackupRef: backupName},
			},
		})).To(Succeed())

		By("waiting for the SlapdRestore to reach Completed")
		awaitRestoreRequest(ctx, "slapd", dbCRName, restoreReq, 0)

		By("waiting for the restore state machine to clear and the cluster to be Running")
		Eventually(ctx, func() bool {
			sc := &ldapv1alpha1.SlapdCluster{}
			if err := crdClient.Get(ctx, client.ObjectKey{Name: "slapd", Namespace: namespace}, sc); err != nil {
				return false
			}
			return sc.Status.Phase == ldapv1alpha1.PhaseRunning && sc.Status.Restore == nil
		}).WithTimeout(3 * time.Minute).WithPolling(5 * time.Second).Should(BeTrue())

		By("verifying the marker is restored on EVERY RW pod")
		for _, pod := range rwPods {
			pod := pod
			Eventually(ctx, func() bool {
				conn, cancel := dialPodLDAP(namespace, pod, "")
				defer cancel()
				defer conn.Close()
				return ldapExists(conn, markerDN)
			}).WithTimeout(2*time.Minute).WithPolling(3*time.Second).Should(BeTrue(),
				"marker should be present on %s immediately after restore", pod)
		}

		By("confirming it STAYS restored across a syncrepl settle window (stale accesslog delete must not replay)")
		Consistently(ctx, func() bool {
			for _, pod := range rwPods {
				conn, cancel := dialPodLDAP(namespace, pod, "")
				present := ldapExists(conn, markerDN)
				conn.Close()
				cancel()
				if !present {
					return false
				}
			}
			return true
		}).WithTimeout(25*time.Second).WithPolling(5*time.Second).Should(BeTrue(),
			"marker must remain on all RW pods; if it vanishes, a stale accesslog delta was replayed")
	})
})

// replayCleanupNeeded reports whether this spec's AfterAll has anything to
// clean up. It is false exactly when BeforeAll took one of its Skip exits
// before assigning markerDN — nothing was created, so deleting anything (in
// particular the empty DN, which slapd reads as the root DSE) is both wrong and
// a source of false "cleanup failed" alarms.
func replayCleanupNeeded(markerDN string) bool { return markerDN != "" }
