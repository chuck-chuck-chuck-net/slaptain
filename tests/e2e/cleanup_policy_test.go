package e2e_test

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	ldap "github.com/go-ldap/ldap/v3"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// ADR-005's cleanupPolicy, end to end, on the shared fixture cluster.
//
// The defect this was written for: deleteDatabaseFromPod issued a bare Del of
// the data database's olcDatabase={N} DN. A replicated data DB always carries
// two children (olcOverlay={0}syncprov, olcOverlay={1}accesslog) and slapd does
// not cascade — config_back_delete refuses a non-leaf with notAllowedOnNonLeaf
// (66) — so cleanupPolicy: Delete could never remove the databases it exists
// for, and the CR's finalizer was released anyway, leaving the database served
// on every pod with no CR to manage it. The accesslog DB was never reaped at
// all.
//
// COST: creating or deleting a SlapdDatabase changes the StatefulSet's
// DATABASE_DIRS, which rolls the cluster (ADR-013's accepted UX wart). This
// container pays that four times, so it is deliberately ONE Ordered container
// with all three policies exercised on one set of fixtures. Run it alone with
// E2E_LABEL_FILTER=cleanup-policy.
//
// MULTI-SITE CAVEAT: these databases exist at this site only. On a mesh, the
// local cluster's external-peer CSN query asks every peer about every LOCAL
// database, so while these fixtures exist a peer legitimately has no readable
// contextCSN for them and reports PartiallyVerified (ADR-008 amendment). That
// clears within one 60 s tick of the last spec. Ginkgo runs serially, so no
// other spec observes it mid-container — but a peer-status assertion scheduled
// immediately afterwards could catch the tail.
var _ = Describe("cleanupPolicy", Label("cleanup-policy"), Ordered, func() {
	const (
		delDB     = "cleanup-del-db"   // replicated + Delete — the defect
		plainDB   = "cleanup-plain-db" // NOT replicated + Delete — positive control
		keepDB    = "cleanup-keep-db"  // replicated + Retain — positive control
		delSuffix = "dc=cleanupdel,dc=cleanup,dc=test"
		plainSfx  = "dc=cleanupplain,dc=cleanup,dc=test"
		keepSfx   = "dc=cleanupkeep,dc=cleanup,dc=test"

		// The suffixes deliberately live in their OWN tree, NOT under the
		// fixture's dc=example,dc=org. slapd refuses a database whose suffix
		// is already served by a preceding one ("already served by a preceding
		// mdb database", LDAP 80), and a subordinate naming context is served
		// by its parent database — so dc=cleanupdel,dc=example,dc=org can
		// never be created while example-db exists. Measured on t3e 2026-09-14.
		settle = 8 * time.Minute
	)

	BeforeAll(func(ctx SpecContext) {
		createCleanupDB(ctx, delDB, delSuffix, ldapv1alpha1.CleanupPolicyDelete, true, 300)
		createCleanupDB(ctx, plainDB, plainSfx, ldapv1alpha1.CleanupPolicyDelete, false, 0)
		createCleanupDB(ctx, keepDB, keepSfx, ldapv1alpha1.CleanupPolicyRetain, true, 400)

		for _, name := range []string{delDB, plainDB, keepDB} {
			By("waiting for " + name + " to reach Running")
			Eventually(ctx, func() bool {
				return slapdDatabaseRunning(crdClient, namespace, name)
			}).WithTimeout(settle).WithPolling(5*time.Second).Should(BeTrue(),
				"%s did not reach Running", name)
		}
		waitClusterSettled(ctx)
	})

	AfterAll(func(ctx SpecContext) {
		// Nothing should survive a green run: every fixture is deleted by one
		// of the specs below. This is the belt for a failed run — best effort,
		// and it deliberately does NOT assert, so a failure reports its own
		// cause rather than a teardown error.
		for _, name := range []string{delDB, plainDB, keepDB} {
			_ = crdClient.Delete(ctx, &ldapv1alpha1.SlapdDatabase{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			})
		}
	})

	// Runs first: it is the only spec whose fixture must still exist
	// afterwards, and it establishes that the default policy is untouched by
	// the Delete-path fix.
	It("Retain leaves the database in every pod's cn=config", func(ctx SpecContext) {
		By("deleting the Retain database's CR")
		Expect(crdClient.Delete(ctx, &ldapv1alpha1.SlapdDatabase{
			ObjectMeta: metav1.ObjectMeta{Name: keepDB, Namespace: namespace},
		})).To(Succeed())
		waitCRGone(ctx, keepDB)
		waitClusterSettled(ctx, keepDB)

		By("asserting the database is still configured on every pod")
		forEachPodConfigConn(ctx, func(pod string, conn *ldap.Conn) {
			Expect(configDBCount(conn, keepSfx)).To(Equal(1),
				"pod %s: Retain must leave olcSuffix=%s in cn=config", pod, keepSfx)
		})
	}, NodeTimeout(15*time.Minute))

	It("Delete removes the database, its overlays and its accesslog DB from every pod", func(ctx SpecContext) {
		By("deleting both Delete-policy CRs")
		for _, name := range []string{delDB, plainDB} {
			Expect(crdClient.Delete(ctx, &ldapv1alpha1.SlapdDatabase{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			})).To(Succeed())
		}
		// The finalizer is only released once every pod's teardown succeeded,
		// so the CRs disappearing is itself part of the assertion.
		waitCRGone(ctx, delDB)
		waitCRGone(ctx, plainDB)
		waitClusterSettled(ctx, delDB, plainDB)

		By("asserting nothing of either database is left on any pod")
		forEachPodConfigConn(ctx, func(pod string, conn *ldap.Conn) {
			// The replicated one — non-leaf, the database this defect hid on.
			Expect(configDBCount(conn, delSuffix)).To(Equal(0),
				"pod %s: olcSuffix=%s must be gone from cn=config", pod, delSuffix)
			// Its accesslog DB goes in the same pass: one log per data DB
			// (ADR-019) means exactly one referent, and it is the CR being
			// finalized — so the delete is authorised by state the operator
			// owns (ADR-026 R2).
			logSuffix := ldapv1alpha1.AccesslogSuffix(delDB)
			Expect(configDBCount(conn, logSuffix)).To(Equal(0),
				"pod %s: olcSuffix=%s must be gone from cn=config", pod, logSuffix)
			// Positive control: a database with no overlays was always
			// deletable and must stay so.
			Expect(configDBCount(conn, plainSfx)).To(Equal(0),
				"pod %s: olcSuffix=%s must be gone from cn=config", pod, plainSfx)
		})
	}, NodeTimeout(15*time.Minute))

	// ADR-005: "To re-adopt an unmanaged database, create a new SlapdDatabase
	// CR with the same suffix." This both asserts that and removes the Retain
	// spec's leftover, so the shared fixture is left as it was found.
	It("re-adopts the Retain leftover and can then delete it", func(ctx SpecContext) {
		By("re-creating the CR over the unmanaged database, this time with Delete")
		createCleanupDB(ctx, keepDB, keepSfx, ldapv1alpha1.CleanupPolicyDelete, true, 400)
		Eventually(ctx, func() bool {
			return slapdDatabaseRunning(crdClient, namespace, keepDB)
		}).WithTimeout(settle).WithPolling(5*time.Second).Should(BeTrue(),
			"%s did not reach Running after re-adoption", keepDB)
		waitClusterSettled(ctx)

		By("deleting it under the Delete policy")
		Expect(crdClient.Delete(ctx, &ldapv1alpha1.SlapdDatabase{
			ObjectMeta: metav1.ObjectMeta{Name: keepDB, Namespace: namespace},
		})).To(Succeed())
		waitCRGone(ctx, keepDB)
		waitClusterSettled(ctx, keepDB)

		forEachPodConfigConn(ctx, func(pod string, conn *ldap.Conn) {
			Expect(configDBCount(conn, keepSfx)).To(Equal(0),
				"pod %s: olcSuffix=%s must be gone after re-adoption + Delete", pod, keepSfx)
			logSuffix := ldapv1alpha1.AccesslogSuffix(keepDB)
			Expect(configDBCount(conn, logSuffix)).To(Equal(0),
				"pod %s: olcSuffix=%s must be gone after re-adoption + Delete", pod, logSuffix)
		})
	}, NodeTimeout(20*time.Minute))
})

// createCleanupDB creates one throwaway SlapdDatabase on the shared cluster.
// Seeded with just its suffix entry so it can reach Running; ridBase must not
// collide with the fixture databases (100, 200 are taken).
func createCleanupDB(
	ctx context.Context,
	name, suffix string,
	policy ldapv1alpha1.CleanupPolicy,
	replicated bool,
	ridBase int32,
) {
	dc := strings.TrimPrefix(strings.SplitN(suffix, ",", 2)[0], "dc=")

	spec := ldapv1alpha1.SlapdDatabaseSpec{
		ClusterRef:    "slapd",
		Suffix:        suffix,
		CleanupPolicy: policy,
		Replication: ldapv1alpha1.DatabaseReplicationConfig{
			Enabled: &replicated,
		},
		Seed: &ldapv1alpha1.DatabaseSeedConfig{
			Entries: []string{fmt.Sprintf(
				"dn: %s\nobjectClass: top\nobjectClass: dcObject\nobjectClass: organization\no: %s\ndc: %s",
				suffix, dc, dc)},
		},
	}
	if replicated {
		spec.Replication.RIDBase = &ridBase
	}

	Expect(crdClient.Create(ctx, &ldapv1alpha1.SlapdDatabase{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       spec,
	})).To(Succeed(), "create SlapdDatabase %s", name)
}

// waitCRGone waits for a SlapdDatabase to actually disappear — i.e. for the
// controller to have released its cleanup finalizer.
func waitCRGone(ctx context.Context, name string) {
	By("waiting for the " + name + " CR to be released by its finalizer")
	Eventually(ctx, func() bool {
		sd := &ldapv1alpha1.SlapdDatabase{}
		err := crdClient.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, sd)
		return err != nil
	}).WithTimeout(6*time.Minute).WithPolling(5*time.Second).Should(BeTrue(),
		"SlapdDatabase %s still exists — its finalizer was never released", name)
}

// waitClusterSettled waits out the rolling restart that a DATABASE_DIRS change
// triggers (ADR-013), so the per-pod assertions dial pods that are actually up.
//
// Readiness alone is not a settled cluster: right after a CR is deleted the
// operator has not yet rewritten DATABASE_DIRS, so every pod is still ready
// from BEFORE the roll. The wait is therefore keyed on the change itself —
// each named database absent from both StatefulSets' DATABASE_DIRS, the
// StatefulSet controller caught up with that generation, and every pod updated
// and ready.
func waitClusterSettled(ctx context.Context, absent ...string) {
	By("waiting for the cluster to settle after the DATABASE_DIRS roll")
	Eventually(ctx, func() bool {
		for _, sts := range []string{"slapd", "slapd-readonly"} {
			s, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, sts, metav1.GetOptions{})
			if err != nil {
				// A cluster with readReplicas=0 has no RO StatefulSet.
				if sts == "slapd-readonly" && apierrors.IsNotFound(err) {
					continue
				}
				return false
			}
			dirs := databaseDirsOf(s)
			for _, name := range absent {
				for _, d := range dirs {
					if d == name {
						return false
					}
				}
			}
			desired := int32(1)
			if s.Spec.Replicas != nil {
				desired = *s.Spec.Replicas
			}
			if s.Status.ObservedGeneration != s.Generation ||
				s.Status.UpdatedReplicas != desired ||
				s.Status.ReadyReplicas != desired {
				return false
			}
		}
		return true
	}).WithTimeout(10*time.Minute).WithPolling(5*time.Second).Should(BeTrue(),
		"cluster did not settle (databases still in DATABASE_DIRS, or pods not rolled)")
}

// databaseDirsOf reads the init container's DATABASE_DIRS list off a
// StatefulSet's pod template.
func databaseDirsOf(sts *appsv1.StatefulSet) []string {
	for _, c := range sts.Spec.Template.Spec.InitContainers {
		for _, e := range c.Env {
			if e.Name == "DATABASE_DIRS" {
				if e.Value == "" {
					return nil
				}
				return strings.Split(e.Value, ",")
			}
		}
	}
	return nil
}

// forEachPodConfigConn runs fn against cn=config on every pod that carries the
// database — the RW StatefulSet and the RO one. cn=config is node-local
// (ADR-002), so "removed from the cluster" is only true once it is true on each
// of them; an RO replica carries its own copy of the data database.
func forEachPodConfigConn(ctx context.Context, fn func(pod string, conn *ldap.Conn)) {
	sc := &ldapv1alpha1.SlapdCluster{}
	Expect(crdClient.Get(ctx, client.ObjectKey{Name: "slapd", Namespace: namespace}, sc)).To(Succeed())

	replicas := sc.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}
	for i := int32(0); i < replicas; i++ {
		pod := fmt.Sprintf("slapd-%d", i)
		conn, cancel := dialPodLDAP(namespace, pod, "")
		Expect(conn.Bind("cn=admin,cn=config", rootPW)).To(Succeed(),
			"bind cn=admin,cn=config on %s", pod)
		fn(pod, conn)
		conn.Close()
		cancel()
	}
	for i := int32(0); i < sc.Spec.ReadReplicas; i++ {
		pod := fmt.Sprintf("slapd-readonly-%d", i)
		conn, cancel := dialReadOnlyPodLDAP(namespace, pod, "")
		Expect(conn.Bind("cn=admin,cn=config", rootPW)).To(Succeed(),
			"bind cn=admin,cn=config on %s", pod)
		fn(pod, conn)
		conn.Close()
		cancel()
	}
}

// configDBCount counts the databases in one pod's cn=config carrying a suffix.
func configDBCount(conn *ldap.Conn, suffix string) int {
	sr, err := conn.Search(ldap.NewSearchRequest(
		"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
		0, 0, false,
		fmt.Sprintf("(olcSuffix=%s)", ldap.EscapeFilter(suffix)),
		[]string{"dn"}, nil,
	))
	Expect(err).NotTo(HaveOccurred(), "search cn=config for olcSuffix=%s", suffix)
	return len(sr.Entries)
}
