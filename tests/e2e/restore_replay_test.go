package e2e_test

import (
	"fmt"
	"os"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

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
		if (anySpecFailed || CurrentSpecReport().Failed()) && keepOnFailure() {
			fmt.Fprintf(GinkgoWriter,
				"\n=== KEEPING FAILED REPLAY STATE: marker %s, SlapdRestore %s, SlapdBackup %s "+
					"left on the SHARED cluster (E2E_KEEP_ON_FAILURE=0 to tear down) ===\n"+
					"  slctl debug-dump -n %s slapd\n",
				markerDN, restoreReq, backupName, namespace)
			return
		}
		// Best-effort: remove the marker so the shared cluster returns to baseline.
		_ = ldapConn.Del(ldap.NewDelRequest(markerDN, nil))
		_ = crdClient.Delete(ctx, &ldapv1alpha1.SlapdRestore{ObjectMeta: metav1.ObjectMeta{Name: restoreReq, Namespace: namespace}})
		_ = crdClient.Delete(ctx, &ldapv1alpha1.SlapdBackup{ObjectMeta: metav1.ObjectMeta{Name: backupName, Namespace: namespace}})
	})

	It("keeps a restored entry after syncrepl settles", func(ctx SpecContext) {
		By("adding a marker entry to the primary database")
		add := ldap.NewAddRequest(markerDN, nil)
		add.Attribute("objectClass", []string{"inetOrgPerson"})
		add.Attribute("cn", []string{"replay marker"})
		add.Attribute("sn", []string{"marker"})
		Expect(ldapConn.Add(add)).To(Succeed(), "add marker entry")
		Expect(ldapExists(ldapConn, markerDN)).To(BeTrue(), "marker should be present before backup")

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

		By("deleting the marker (accesslog records a delete at a CSN newer than the backup)")
		Expect(ldapConn.Del(ldap.NewDelRequest(markerDN, nil))).To(Succeed(), "delete marker entry")

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
		Eventually(ctx, func() ldapv1alpha1.SlapdRestorePhase {
			sr := &ldapv1alpha1.SlapdRestore{}
			if err := crdClient.Get(ctx, client.ObjectKey{Name: restoreReq, Namespace: namespace}, sr); err != nil {
				return ""
			}
			return sr.Status.Phase
		}).WithTimeout(10*time.Minute).WithPolling(5*time.Second).Should(Equal(ldapv1alpha1.RestoreRequestCompleted),
			"SlapdRestore should complete; check cluster status.restore and restore Job logs")

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
