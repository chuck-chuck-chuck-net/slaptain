package e2e_test

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// Syncprov in-memory sessionlog (ADR-022).
//
// Two assertions, one per half of the decision:
//
//	data DB      — the syncprov overlay on the *data* database carries
//	               olcSpSessionlog, sized from spec.replication.syncprovSessionlog
//	               (unset → the operator default of 5000). Red against any
//	               operator predating ADR-022, which wrote no such attribute.
//	accesslog DB — the syncprov overlay on an *accesslog* database carries no
//	               sessionlog at all. This is the placement rule, not a detail:
//	               a successful replay there displaces the minCSN guard and
//	               trades a loud REFRESH_REQUIRED for silent under-replication.
//
// Ungated: it needs nothing beyond the standard fixture, and the state it checks
// is what every replicated cluster must carry.
var _ = Describe("syncprov sessionlog", Label("sessionlog"), Ordered, func() {

	var (
		rwPods     []string
		dataSuffix string
		logSuffix  string
		wantOps    string // "" means: the attribute must be absent
	)

	BeforeAll(func(ctx SpecContext) {
		By("reading the SlapdCluster and its database")
		sc := &ldapv1alpha1.SlapdCluster{}
		Expect(crdClient.Get(ctx, nsName(namespace, "slapd"), sc)).To(Succeed())
		if !sc.Spec.Replication.Enabled || sc.IsConsumerOnly() {
			Skip("cluster is not a syncrepl provider (replication disabled or consumer-only), " +
				"so its data DB carries no syncprov overlay to hold a sessionlog")
		}

		sd := &ldapv1alpha1.SlapdDatabase{}
		Expect(crdClient.Get(ctx, nsName(namespace, dbCRName), sd)).To(Succeed())
		dataSuffix = sd.Spec.Suffix
		Expect(dataSuffix).NotTo(BeEmpty())
		logSuffix = ldapv1alpha1.AccesslogSuffix(sd.Name)

		// Mirror ADR-022's tristate: unset → the operator default, 0 →
		// disabled, >0 → verbatim. Spelled out here rather than imported so
		// the e2e states the contract instead of restating the code.
		switch v := sd.Spec.Replication.SyncprovSessionlog; {
		case v == nil:
			wantOps = "5000"
		case *v <= 0:
			wantOps = ""
		default:
			wantOps = strconv.Itoa(int(*v))
		}

		Eventually(ctx, func() bool {
			return slapdDatabaseRunning(crdClient, namespace, dbCRName)
		}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(BeTrue(),
			"SlapdDatabase %s never reached Running", dbCRName)

		By("reading the RW StatefulSet")
		sts, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, "slapd", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		n := int32(1)
		if sts.Spec.Replicas != nil {
			n = *sts.Spec.Replicas
		}
		rwPods = nil
		for i := int32(0); i < n; i++ {
			rwPods = append(rwPods, fmt.Sprintf("slapd-%d", i))
		}
	}, NodeTimeout(7*time.Minute))

	It("sizes the sessionlog on every RW pod's data database", func(ctx SpecContext) {
		for _, pod := range rwPods {
			By("inspecting cn=config on " + pod)
			conn := dialPodConfigEventually(ctx, pod)

			dataDB, ok := findMdb(observedMdbDatabases(conn), dataSuffix)
			Expect(ok).To(BeTrue(), "pod %s has no data database %q", pod, dataSuffix)

			dn, values := syncprovSessionlog(conn, dataDB.dn)
			conn.Close()

			Expect(dn).NotTo(BeEmpty(),
				"pod %s: data database %q has no syncprov overlay", pod, dataSuffix)

			if wantOps == "" {
				Expect(values).To(BeEmpty(),
					"pod %s: syncprovSessionlog is 0, so %s must carry no olcSpSessionlog", pod, dn)
				continue
			}
			Expect(values).To(ConsistOf(wantOps),
				"pod %s: %s must carry olcSpSessionlog %s (ADR-022)", pod, dn, wantOps)
		}
	}, NodeTimeout(5*time.Minute))

	It("leaves the accesslog database's syncprov without a sessionlog", func(ctx SpecContext) {
		for _, pod := range rwPods {
			By("inspecting cn=config on " + pod)
			conn := dialPodConfigEventually(ctx, pod)

			logDB, ok := findMdb(observedMdbDatabases(conn), logSuffix)
			if !ok {
				conn.Close()
				Skip(fmt.Sprintf("pod %s has no accesslog DB %q — the database is not delta-sync",
					pod, logSuffix))
			}

			dn, values := syncprovSessionlog(conn, logDB.dn)
			conn.Close()

			Expect(dn).NotTo(BeEmpty(),
				"pod %s: accesslog DB %q has no syncprov overlay", pod, logSuffix)
			Expect(values).To(BeEmpty(),
				"pod %s: %s must carry no olcSpSessionlog — on a log DB a successful "+
					"replay displaces the minCSN guard and under-replicates silently (ADR-022)",
				pod, dn)
		}
	}, NodeTimeout(5*time.Minute))
})

// syncprovSessionlog returns the DN of the syncprov overlay under dbDN and its
// olcSpSessionlog values (trimmed, {N}-prefix-free). An empty DN means the
// database carries no syncprov overlay at all.
func syncprovSessionlog(conn *ldap.Conn, dbDN string) (string, []string) {
	res, err := conn.Search(ldap.NewSearchRequest(
		dbDN, ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
		0, 0, false, "(objectClass=olcSyncProvConfig)",
		[]string{"olcSpSessionlog"}, nil))
	Expect(err).NotTo(HaveOccurred(), "search syncprov overlay under %s", dbDN)
	if len(res.Entries) == 0 {
		return "", nil
	}
	var out []string
	for _, v := range res.Entries[0].GetEqualFoldAttributeValues("olcSpSessionlog") {
		out = append(out, strings.TrimSpace(v))
	}
	return res.Entries[0].DN, out
}
