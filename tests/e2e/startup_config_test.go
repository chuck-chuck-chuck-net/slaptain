package e2e_test

import (
	"context"
	"fmt"
	"strings"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// Startup-only cn=config, end to end (ADR-034).
//
// olcSpSessionlogSource cannot be written to a running slapd at all — the
// online path in sp_cf_gen frees c->value_ndn.bv_val and then stores the freed
// pointer, and the delete path frees it again. So the attribute reaches
// cn=config by exactly one route: the operator publishes a declared table into
// the <cluster>-startup-config ConfigMap, the init container's offline
// converger applies it with slapmodify at pod start, and the change lands at a
// RESTART rather than at the spec edit.
//
// That split is the whole behaviour, and it is what this spec pins, in the
// order a user meets it:
//
//	1. the table appears in the ConfigMap when a database asks for Accesslog,
//	   and the running pods do NOT yet carry the attribute — ADR-034's admitted
//	   first-generation edge, reported as StartupConfigConverged=False/
//	   RestartRequired rather than silently converged;
//	2. after a restart every RW pod's data DB carries olcSpSessionlogSource and
//	   no olcSpSessionlog, the accesslog DB's own syncprov still carries no
//	   session log (ADR-022 — a replay there displaces the minCSN guard), and
//	   the condition reads True;
//	3. dropping the field renders an EMPTY table (never an absent key, which
//	   means "unmanaged" to the converger), and one restart later the attribute
//	   is off the disk and the in-memory log is back.
//
// Ungated, like cleanup_policy_test.go: it needs nothing beyond the standard
// fixture. It DOES restart the read-write fleet twice, and it mutates the
// fixture database's spec — both are undone before it returns, so a spec
// scheduled after it is handed the cluster it would have found. Run it alone
// with E2E_LABEL_FILTER=startup-config.
var _ = Describe("startup-only configuration", Label("startup-config"), Ordered, func() {

	var (
		rwPods     []string
		dataSuffix string
		logSuffix  string
		// What the fixture declared before this spec touched it, restored in
		// AfterAll. Both halves, because the admission rule couples them.
		origSource string
		origOps    *int32
	)

	const (
		configMapKey = "startup-config"
		// The restart budget. A pod start now runs the converger, which
		// slapcats and (on the first boot after a change) slapmodifies the
		// configuration before slapd starts; ADR-030 gives the startup probe
		// 300 s on top of that.
		restartBudget = 8 * time.Minute
	)

	configMapName := func() string { return "slapd-" + configMapKey }

	BeforeAll(func(ctx SpecContext) {
		By("reading the SlapdCluster")
		sc := &ldapv1alpha1.SlapdCluster{}
		Expect(crdClient.Get(ctx, nsName(namespace, "slapd"), sc)).To(Succeed())
		if !sc.Spec.Replication.Enabled || sc.IsConsumerOnly() {
			Skip("cluster is not a syncrepl provider (replication disabled or consumer-only), " +
				"so its data DB carries no syncprov overlay to source a session log into")
		}

		By("reading the fixture SlapdDatabase " + dbCRName)
		sd := &ldapv1alpha1.SlapdDatabase{}
		Expect(crdClient.Get(ctx, nsName(namespace, dbCRName), sd)).To(Succeed())
		if sd.Spec.Replication.DeltaSync != nil && !*sd.Spec.Replication.DeltaSync {
			Skip("fixture database is not delta-sync, so it has no accesslog database " +
				"to source a session log from (ADR-034 admission rule)")
		}
		dataSuffix = sd.Spec.Suffix
		Expect(dataSuffix).NotTo(BeEmpty())
		logSuffix = ldapv1alpha1.AccesslogSuffix(sd.Name)
		origSource = sd.Spec.Replication.SyncprovSessionlogSource
		origOps = sd.Spec.Replication.SyncprovSessionlog

		Eventually(ctx, func() bool {
			return slapdDatabaseRunning(crdClient, namespace, dbCRName)
		}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(BeTrue(),
			"SlapdDatabase %s never reached Running", dbCRName)

		rwPods = rwPodNames(ctx, "slapd")
		Expect(rwPods).NotTo(BeEmpty())
	}, NodeTimeout(10*time.Minute))

	AfterAll(func(ctx SpecContext) {
		// Belt for a failed run: a green one has already put the field back in
		// the last spec. Best effort and deliberately non-asserting, so a
		// failure reports its own cause rather than a teardown error — but the
		// restart is NOT skipped, because leaving the fleet carrying an
		// attribute the fixture no longer declares would hand every later spec
		// a cluster in a state nothing else in the suite creates.
		sd := &ldapv1alpha1.SlapdDatabase{}
		if err := crdClient.Get(ctx, nsName(namespace, dbCRName), sd); err != nil {
			return
		}
		if sd.Spec.Replication.SyncprovSessionlogSource == origSource {
			return
		}
		GinkgoLogr.Info("restoring the fixture database's session-log source",
			"from", sd.Spec.Replication.SyncprovSessionlogSource, "to", origSource)
		setSessionlogSource(ctx, origSource, origOps)
		restartRWFleet(ctx, rwPods, restartBudget)
		waitClusterSettled(ctx)
	}, NodeTimeout(20*time.Minute))

	It("publishes the table and leaves the running pods alone until they restart", func(ctx SpecContext) {
		By("asking for the session log to come from the accesslog database")
		setSessionlogSource(ctx, ldapv1alpha1.SyncprovSessionlogSourceAccesslog, nil)

		wantLine := strings.Join(
			[]string{"olcSpSessionlogSource", dataSuffix, logSuffix}, "\t")

		By("waiting for the operator to publish " + configMapName())
		Eventually(ctx, func() []string {
			return startupConfigLines(ctx, configMapName())
		}).WithTimeout(3*time.Minute).WithPolling(5*time.Second).Should(ContainElement(wantLine),
			"%s must carry one tab-separated record naming the database and its journal "+
				"(ADR-034 wire format)", configMapName())

		By("asserting StartupConfigConverged reports the divergence and names the pods")
		// Waited for BEFORE the per-pod reads below, and on the CURRENT
		// generation: it is the proof that the SlapdDatabase controller has
		// seen the edit and finished a pass over every pod's syncprov overlay.
		// Reading the pods first would let a green run mean nothing more than
		// "the operator had not got to it yet".
		Eventually(ctx, func() string {
			return describeConditionAtGeneration(ctx, dbCRName)
		}).WithTimeout(6*time.Minute).WithPolling(10*time.Second).Should(
			HavePrefix("False/RestartRequired"),
			"the operator must report the declared table as not yet in place (ADR-034 R3)")

		By("asserting the attribute has NOT appeared on any running pod, and the " +
			"in-memory session log is STILL THERE")
		// Two halves of one rule, and the second is the one that was missing.
		//
		// The operator may not write olcSpSessionlogSource and does not: a pod
		// carrying it already would mean something put it on the wire, the
		// crash ADR-034 R2 exists to prevent.
		//
		// And because it cannot write the replacement, it must not take the
		// in-memory log away either. Doing so leaves the database with NO
		// session log of any kind — not briefly, but until a restart a human
		// schedules and may never take — so every consumer that falls behind
		// pays a full present-phase refresh instead of a replay, which is the
		// storm class ADR-022 exists to avoid. The log is handed over at the
		// restart, in one offline write; it is never dropped here.
		for _, pod := range rwPods {
			conn := dialPodConfigEventually(ctx, pod)
			dataDB, ok := findMdb(observedMdbDatabases(conn), dataSuffix)
			Expect(ok).To(BeTrue(), "pod %s has no data database %q", pod, dataSuffix)
			dn, src := syncprovAttr(conn, dataDB.dn, "olcSpSessionlogSource")
			_, inMem := syncprovAttr(conn, dataDB.dn, "olcSpSessionlog")
			conn.Close()
			Expect(dn).NotTo(BeEmpty(), "pod %s: data database has no syncprov overlay", pod)
			Expect(src).To(BeEmpty(),
				"pod %s: %s carries olcSpSessionlogSource before any restart — the operator "+
					"must never write this attribute to a running slapd (ADR-034 R2)", pod, dn)
			Expect(inMem).NotTo(BeEmpty(),
				"pod %s: %s has lost its in-memory olcSpSessionlog while olcSpSessionlogSource "+
					"is not yet on the pod — the database now has NO session log at all, and "+
					"will have none until somebody restarts it. The log is handed over at the "+
					"restart, never dropped at the spec edit (ADR-034, ADR-022)", pod, dn)
		}

		cond := startupConfigCondition(ctx, dbCRName)
		Expect(cond).NotTo(BeNil())
		for _, pod := range rwPods {
			Expect(cond.Message).To(ContainSubstring(pod),
				"the condition must name every diverging pod, so the reader knows what to "+
					"restart; got: %s", cond.Message)
		}
	}, NodeTimeout(15*time.Minute))

	It("converges the attribute offline when the pods restart", func(ctx SpecContext) {
		restartRWFleet(ctx, rwPods, restartBudget)
		waitClusterSettled(ctx)

		By("asserting every RW pod's data database now sources its session log from the journal")
		for _, pod := range rwPods {
			conn := dialPodConfigEventually(ctx, pod)
			mdbs := observedMdbDatabases(conn)

			dataDB, ok := findMdb(mdbs, dataSuffix)
			Expect(ok).To(BeTrue(), "pod %s has no data database %q", pod, dataSuffix)
			dn, src := syncprovAttr(conn, dataDB.dn, "olcSpSessionlogSource")
			_, inMem := syncprovAttr(conn, dataDB.dn, "olcSpSessionlog")

			logDB, haveLog := findMdb(mdbs, logSuffix)
			var logDN string
			var logSessionlog []string
			if haveLog {
				logDN, logSessionlog = syncprovAttr(conn, logDB.dn, "olcSpSessionlog")
			}
			conn.Close()

			Expect(dn).NotTo(BeEmpty(), "pod %s: data database has no syncprov overlay", pod)
			Expect(src).To(HaveLen(1),
				"pod %s: %s must carry exactly one olcSpSessionlogSource after the restart "+
					"(ADR-034 R1)", pod, dn)
			Expect(strings.EqualFold(strings.TrimSpace(src[0]), logSuffix)).To(BeTrue(),
				"pod %s: %s must source its session log from %q, got %q", pod, dn, logSuffix, src[0])
			Expect(inMem).To(BeEmpty(),
				"pod %s: %s must carry no olcSpSessionlog alongside the source — slapd calls "+
					"both at once wasteful and the admission rule refuses the combination", pod, dn)

			Expect(haveLog).To(BeTrue(), "pod %s has no accesslog database %q", pod, logSuffix)
			Expect(logDN).NotTo(BeEmpty(),
				"pod %s: accesslog database %q has no syncprov overlay", pod, logSuffix)
			Expect(logSessionlog).To(BeEmpty(),
				"pod %s: %s must carry no olcSpSessionlog of its own — being the SOURCE of the "+
					"data DB's session log is a different thing, and a log ON the journal "+
					"displaces the minCSN guard (ADR-022)", pod, logDN)
		}

		By("asserting StartupConfigConverged has gone True")
		Eventually(ctx, func() string {
			return describeCondition(startupConfigCondition(ctx, dbCRName))
		}).WithTimeout(6*time.Minute).WithPolling(10*time.Second).Should(
			HavePrefix("True/Converged"),
			"with the table in place on every pod the operator must report it converged")

		By("asserting the directory still answers and still replicates")
		refreshLDAPConn()
		dn := addReplTestUser(ldapConn, "startupcfg-after-restart", 44101)
		Expect(ldapExists(ldapConn, dn)).To(BeTrue())
		Expect(ldapConn.Del(ldap.NewDelRequest(dn, nil))).To(Succeed())
		waitForCrossSiteReplication(ctx, "the startup-config fleet restart")
	}, NodeTimeout(25*time.Minute))

	It("takes the attribute off the disk when the field is dropped", func(ctx SpecContext) {
		By("restoring the fixture database's declaration")
		setSessionlogSource(ctx, origSource, origOps)

		By("asserting the ConfigMap key is still present and now EMPTY")
		// An ABSENT key means "unmanaged" to the converger and would leave the
		// attribute on disk forever. Present-and-empty is how the last database
		// to drop the field gets it removed, so the distinction is asserted
		// rather than assumed.
		Eventually(ctx, func() (map[string]string, error) {
			cm, err := k8sClient.CoreV1().ConfigMaps(namespace).
				Get(ctx, configMapName(), metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			return cm.Data, nil
		}).WithTimeout(3*time.Minute).WithPolling(5*time.Second).Should(
			HaveKeyWithValue(configMapKey, ""),
			"%s must keep the %q key and render it empty — an absent key means "+
				"'unmanaged' and would strip nothing (ADR-034 wire format)",
			configMapName(), configMapKey)

		By("asserting the sourced log keeps serving until the restart")
		// The handover runs backwards exactly as it ran forwards. The field is
		// gone, but olcSpSessionlogSource is still on disk and still the log
		// this database answers from, so the operator must NOT put an
		// in-memory one beside it: that pair is what slapd calls wasteful and
		// what the admission rule refuses. The in-memory log returns when the
		// source goes, which is at the restart below.
		for _, pod := range rwPods {
			conn := dialPodConfigEventually(ctx, pod)
			dataDB, ok := findMdb(observedMdbDatabases(conn), dataSuffix)
			Expect(ok).To(BeTrue(), "pod %s has no data database %q", pod, dataSuffix)
			dn, src := syncprovAttr(conn, dataDB.dn, "olcSpSessionlogSource")
			_, inMem := syncprovAttr(conn, dataDB.dn, "olcSpSessionlog")
			conn.Close()
			Expect(src).To(HaveLen(1),
				"pod %s: %s must still source its session log from the journal until it "+
					"restarts — dropping the field cannot take the attribute off a running "+
					"server (ADR-034 R2)", pod, dn)
			Expect(inMem).To(BeEmpty(),
				"pod %s: %s has gained an in-memory olcSpSessionlog while the sourced log is "+
					"still live — two session logs, the combination slapd calls wasteful. The "+
					"in-memory one comes back when the source goes, at the restart", pod, dn)
		}

		By("restarting the fleet so the converger can remove it")
		restartRWFleet(ctx, rwPods, restartBudget)
		waitClusterSettled(ctx)

		By("asserting the attribute is gone and the in-memory session log is back")
		for _, pod := range rwPods {
			conn := dialPodConfigEventually(ctx, pod)
			dataDB, ok := findMdb(observedMdbDatabases(conn), dataSuffix)
			Expect(ok).To(BeTrue(), "pod %s has no data database %q", pod, dataSuffix)
			dn, src := syncprovAttr(conn, dataDB.dn, "olcSpSessionlogSource")
			_, inMem := syncprovAttr(conn, dataDB.dn, "olcSpSessionlog")
			conn.Close()

			Expect(src).To(BeEmpty(),
				"pod %s: %s must no longer carry olcSpSessionlogSource once the field is "+
					"dropped and the pod has restarted", pod, dn)
			Expect(inMem).NotTo(BeEmpty(),
				"pod %s: %s must carry the in-memory olcSpSessionlog again (ADR-022)", pod, dn)
		}

		By("asserting the condition is True for a database that declares nothing")
		Eventually(ctx, func() string {
			return describeCondition(startupConfigCondition(ctx, dbCRName))
		}).WithTimeout(6*time.Minute).WithPolling(10*time.Second).Should(
			HavePrefix("True/NoStartupOnlyAttributes"),
			"a database that declares no startup-only configuration must not keep a stale False")

		waitForCrossSiteReplication(ctx, "the startup-config restore restart")
	}, NodeTimeout(25*time.Minute))
})

// setSessionlogSource patches spec.replication.syncprovSessionlogSource (and
// the in-memory size the admission rule couples to it) on the fixture database.
//
// A Get/Update retry loop rather than a patch: the SlapdDatabase controller
// writes status on the same object, so a stale resourceVersion is normal.
func setSessionlogSource(ctx context.Context, source string, ops *int32) {
	Eventually(ctx, func() error {
		sd := &ldapv1alpha1.SlapdDatabase{}
		if err := crdClient.Get(ctx, nsName(namespace, dbCRName), sd); err != nil {
			return err
		}
		sd.Spec.Replication.SyncprovSessionlogSource = source
		// Accesslog excludes an in-memory log, and the CRD's CEL rule rejects
		// the pair — so the size must go on the way in and come back on the
		// way out, in the same update.
		if source == ldapv1alpha1.SyncprovSessionlogSourceAccesslog {
			sd.Spec.Replication.SyncprovSessionlog = nil
		} else {
			sd.Spec.Replication.SyncprovSessionlog = ops
		}
		return crdClient.Update(ctx, sd)
	}).WithTimeout(2*time.Minute).WithPolling(3*time.Second).Should(Succeed(),
		"setting syncprovSessionlogSource=%q on %s", source, dbCRName)
}

// startupConfigLines reads the published table as the converger would parse it:
// non-blank, non-comment lines.
func startupConfigLines(ctx context.Context, name string) []string {
	cm, err := k8sClient.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil // not written yet — the caller polls
	}
	var out []string
	for _, line := range strings.Split(cm.Data["startup-config"], "\n") {
		if s := strings.TrimRight(line, "\r"); strings.TrimSpace(s) != "" &&
			!strings.HasPrefix(s, "#") {
			out = append(out, s)
		}
	}
	return out
}

// startupConfigCondition returns the SlapdDatabase's StartupConfigConverged
// condition, or nil when the CR or the condition is not there.
func startupConfigCondition(ctx context.Context, name string) *metav1.Condition {
	db := &ldapv1alpha1.SlapdDatabase{}
	if err := crdClient.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, db); err != nil {
		return nil
	}
	for i := range db.Status.Conditions {
		if db.Status.Conditions[i].Type == "StartupConfigConverged" {
			return &db.Status.Conditions[i]
		}
	}
	return nil
}

// describeConditionAtGeneration is describeCondition, but only once the
// condition was computed from the CR's CURRENT generation.
//
// The difference matters whenever a spec edit is followed by a read of the
// pods: a condition left over from before the edit would let an Eventually
// return while the controller has not yet touched a single pod, and every
// assertion after it would be observing the OLD world and passing.
func describeConditionAtGeneration(ctx context.Context, name string) string {
	db := &ldapv1alpha1.SlapdDatabase{}
	if err := crdClient.Get(ctx, nsName(namespace, name), db); err != nil {
		return "<unreadable>"
	}
	cond := startupConfigCondition(ctx, name)
	if cond == nil {
		return "<absent>"
	}
	if cond.ObservedGeneration != db.Generation {
		return fmt.Sprintf("<stale: observedGeneration %d, generation %d> %s",
			cond.ObservedGeneration, db.Generation, describeCondition(cond))
	}
	return describeCondition(cond)
}

// describeCondition renders a condition as "Status/Reason: message" so an
// Eventually can match on the first two and still print the third on failure.
func describeCondition(cond *metav1.Condition) string {
	if cond == nil {
		return "<absent>"
	}
	return fmt.Sprintf("%s/%s: %s", cond.Status, cond.Reason, cond.Message)
}

// syncprovAttr returns the DN of the syncprov overlay under dbDN and the values
// of one attribute on it. An empty DN means there is no syncprov overlay.
func syncprovAttr(conn *ldap.Conn, dbDN, attr string) (string, []string) {
	res, err := conn.Search(ldap.NewSearchRequest(
		dbDN, ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
		0, 0, false, "(objectClass=olcSyncProvConfig)",
		[]string{attr}, nil))
	Expect(err).NotTo(HaveOccurred(), "search syncprov overlay under %s", dbDN)
	if len(res.Entries) == 0 {
		return "", nil
	}
	var out []string
	for _, v := range res.Entries[0].GetEqualFoldAttributeValues(attr) {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return res.Entries[0].DN, out
}

// restartRWFleet deletes every read-write pod, one at a time, waiting for each
// to come back Ready before touching the next.
//
// One at a time on purpose. The change path for a startup-only attribute is a
// restart (ADR-034 R3), and the way a user would take it without losing the
// cluster is the way a StatefulSet rolls: a provider leaving the fleet is an
// ADR-016 topology event at every peer, and taking all of them at once would
// make this spec a write outage rather than a configuration change.
func restartRWFleet(ctx SpecContext, pods []string, budget time.Duration) {
	for _, pod := range pods {
		By("restarting " + pod)
		old, err := k8sClient.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "reading %s before restart", pod)
		oldUID := old.UID

		Expect(k8sClient.CoreV1().Pods(namespace).Delete(ctx, pod, metav1.DeleteOptions{})).
			To(Succeed(), "deleting %s", pod)

		Eventually(ctx, func() bool {
			p, err := k8sClient.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
			if err != nil || p.UID == oldUID {
				return false
			}
			return podReady(k8sClient, namespace, pod)
		}).WithTimeout(budget).WithPolling(5*time.Second).Should(BeTrue(),
			"%s must be recreated and become Ready within %s — a converger that refuses "+
				"writes nothing and still exits 0, but one that fails keeps the pod down",
			pod, budget)
	}
}
