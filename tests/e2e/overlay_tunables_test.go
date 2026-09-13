package e2e_test

import (
	"fmt"
	"strings"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The two overlay tunables the operator converges rather than writes once:
// olcAccessLogPurge on the accesslog overlay and olcSpCheckpoint on the data
// DB's syncprov overlay (ADR-024 R4, with an R5 default for the purge window).
//
// Both used to be written ONLY into the overlay-creation addReq, so a spec edit
// — or a newly introduced operator default — never reached a pod whose overlay
// already existed. That is invisible on a healthy fixture: replication works
// fine with no purge policy right up to the day the journal hits its map
// ceiling and, because the accesslog overlay sits in the DATA write path, takes
// data writes down with it. An assertion on cn=config is the only standing
// guard.
//
// cn=config is node-local (ADR-002), so every RW pod is checked independently:
// a purge window converged on two pods out of three leaves the third growing.
//
// Both attributes were verified live-modifiable on a running slapd before being
// converged at all (replace AND delete, no crash, no hang) — the precondition
// ADR-024's 2026-09-12 and 2026-09-13 amendments made mandatory for this class.
//
// Ungated: a bind and two searches per pod per database.
var _ = Describe("overlay tunables: purge window and checkpoint",
	Label("tunables"), Label("accesslog"), Ordered, ContinueOnFailure, func() {

		var (
			rwPods []string
			dbs    []ldapv1alpha1.SlapdDatabase
		)

		BeforeAll(func(ctx SpecContext) {
			By("reading the SlapdCluster")
			sc := &ldapv1alpha1.SlapdCluster{}
			Expect(crdClient.Get(ctx, nsName(namespace, "slapd"), sc)).To(Succeed())
			if !sc.Spec.Replication.Enabled || sc.IsConsumerOnly() {
				Skip("cluster is not a syncrepl provider, so it carries neither " +
					"an accesslog overlay nor a data-DB syncprov overlay")
			}

			By("listing the SlapdDatabase CRs in the namespace")
			list := &ldapv1alpha1.SlapdDatabaseList{}
			Expect(crdClient.List(ctx, list)).To(Succeed())
			for _, sd := range list.Items {
				if sd.Namespace == namespace && sd.Spec.ClusterRef == "slapd" {
					dbs = append(dbs, sd)
				}
			}
			Expect(dbs).NotTo(BeEmpty(), "no SlapdDatabase CRs found for cluster slapd")

			for _, sd := range dbs {
				name := sd.Name
				Eventually(ctx, func() bool {
					return slapdDatabaseRunning(crdClient, namespace, name)
				}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(BeTrue(),
					"SlapdDatabase %s never reached Running", name)
			}

			By("reading the RW StatefulSet")
			sts, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, "slapd", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			n := int32(1)
			if sts.Spec.Replicas != nil {
				n = *sts.Spec.Replicas
			}
			for i := int32(0); i < n; i++ {
				rwPods = append(rwPods, fmt.Sprintf("slapd-%d", i))
			}
		}, NodeTimeout(8*time.Minute))

		It("converges the accesslog purge window on every RW pod", func(ctx SpecContext) {
			for _, pod := range rwPods {
				By("inspecting cn=config on " + pod)
				conn := dialPodConfigEventually(ctx, pod)

				for _, sd := range dbs {
					if !(&sd).DeltaSyncEnabled() {
						continue
					}
					want := wantAccesslogPurge(sd)

					dataDB, ok := findMdb(observedMdbDatabases(conn), sd.Spec.Suffix)
					Expect(ok).To(BeTrue(), "pod %s has no data database %q", pod, sd.Spec.Suffix)

					dn, values := overlayAttrValues(conn, dataDB.dn,
						"(objectClass=olcAccessLogConfig)", "olcAccessLogPurge")
					Expect(dn).NotTo(BeEmpty(),
						"pod %s: data database %q has no accesslog overlay", pod, sd.Spec.Suffix)

					if want == "" {
						Expect(values).To(BeEmpty(),
							"pod %s: %s (db %s) has accesslogPurge \"none\", so it must carry "+
								"no olcAccessLogPurge", pod, dn, sd.Name)
						continue
					}
					Expect(values).To(ConsistOf(want),
						"pod %s: %s (db %s) must carry olcAccessLogPurge %q — an unpurged "+
							"journal grows to its map ceiling and then stops data writes (ADR-024 R4/R5)",
						pod, dn, sd.Name, want)
				}
				conn.Close()
			}
		}, NodeTimeout(6*time.Minute))

		It("converges the syncprov checkpoint on every RW pod's data database", func(ctx SpecContext) {
			for _, pod := range rwPods {
				By("inspecting cn=config on " + pod)
				conn := dialPodConfigEventually(ctx, pod)

				for _, sd := range dbs {
					want := strings.TrimSpace(sd.Spec.Replication.SyncprovCheckpoint)
					if strings.EqualFold(want, "none") {
						want = ""
					}

					dataDB, ok := findMdb(observedMdbDatabases(conn), sd.Spec.Suffix)
					Expect(ok).To(BeTrue(), "pod %s has no data database %q", pod, sd.Spec.Suffix)

					dn, values := overlayAttrValues(conn, dataDB.dn,
						"(objectClass=olcSyncProvConfig)", "olcSpCheckpoint")
					Expect(dn).NotTo(BeEmpty(),
						"pod %s: data database %q has no syncprov overlay", pod, sd.Spec.Suffix)

					if want == "" {
						Expect(values).To(BeEmpty(),
							"pod %s: %s (db %s) has no syncprovCheckpoint, so it must carry "+
								"no olcSpCheckpoint", pod, dn, sd.Name)
						continue
					}
					Expect(values).To(ConsistOf(want),
						"pod %s: %s (db %s) must carry olcSpCheckpoint %q, converged on every "+
							"reconcile and not only at overlay creation (ADR-024 R4)",
						pod, dn, sd.Name, want)
				}
				conn.Close()
			}
		}, NodeTimeout(6*time.Minute))
	})

// wantAccesslogPurge mirrors the operator's resolution of
// spec.replication.accesslogPurge: unset → the operator default, "none" → no
// attribute, anything else verbatim. Spelled out here rather than imported so
// the e2e states the contract instead of restating the code.
func wantAccesslogPurge(sd ldapv1alpha1.SlapdDatabase) string {
	v := strings.TrimSpace(sd.Spec.Replication.AccesslogPurge)
	switch {
	case v == "":
		return "7+00:00 1+00:00"
	case strings.EqualFold(v, "none"):
		return ""
	default:
		return v
	}
}

// overlayAttrValues returns the DN of the overlay under dbDN matching filter and
// that overlay's values for attr (trimmed). An empty DN means no such overlay.
func overlayAttrValues(conn *ldap.Conn, dbDN, filter, attr string) (string, []string) {
	res, err := conn.Search(ldap.NewSearchRequest(
		dbDN, ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
		0, 0, false, filter, []string{attr}, nil))
	Expect(err).NotTo(HaveOccurred(), "search %s under %s", filter, dbDN)
	if len(res.Entries) == 0 {
		return "", nil
	}
	var out []string
	for _, v := range res.Entries[0].GetEqualFoldAttributeValues(attr) {
		out = append(out, strings.TrimSpace(v))
	}
	return res.Entries[0].DN, out
}
