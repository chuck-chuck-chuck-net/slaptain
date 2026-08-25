package e2e_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// Per-database accesslog (ADR-019) + accesslog access control (ADR-020).
//
// This is the two-replicated-database fixture ADR-019 names as its red-first
// artefact. It needs tests/resources/example/database2.yaml — a second
// SlapdDatabase at deltaSync: true — and skips itself when the resource set
// declares only one database.
//
// What the four specs are for, and which of them actually has teeth:
//
//	structural   — one log DB per replicated data DB, logbase ≡ olcAccessLogDB ≡
//	               log olcSuffix, no two databases sharing a log, no legacy
//	               cn=accesslog left. Deterministically red pre-ADR-019.
//	behavioural  — sustained writes to DB-A produce no `delta-sync lost sync` in
//	               any pod's log while DB-B sits idle. THIS is the spec that
//	               proves the mechanism (ADR-019 Facts 1-2).
//	convergence  — both databases converge on every RW pod. A guard, NOT a
//	               proof: a shared log converges too — by whole-DIT reload. It
//	               stays green against the buggy code, which is exactly why the
//	               defect was invisible for so long. Do not read a green
//	               convergence run as evidence that the bug is not real.
//	ADR-020       — anonymous read of a log is denied, cn=replication,<suffix>
//	               read succeeds, and the rootDSE still advertises both logs.

// lostSyncRE matches slapd's delta-syncrepl kill-switch line
// (syncrepl.c: "do_syncrep2: %s delta-sync lost sync on (%s), switching to
// REFRESH"). Requires -d sync in spec.logLevel; the fixture's 16640 = 16384
// (sync) + 256 (stats) provides it.
var lostSyncRE = regexp.MustCompile(`delta-sync lost sync on`)

// Ordered for the BeforeAll (one credential/topology read for all four specs),
// ContinueOnFailure because the four are independent: a structural failure must
// not skip the behavioural one. On a shared-accesslog cluster that distinction
// is the whole point — spec 1 fails, spec 2 fails with the `delta-sync lost
// sync` evidence, and spec 3 passes anyway.
var _ = Describe("per-database accesslog", Label("accesslog"), Ordered, ContinueOnFailure, func() {

	var (
		db2Name     string
		db2Suffix   string
		db2AdminPW  string
		db1ReplPW   string
		db2ReplPW   string
		replicas    int32
		rwPods      []string
		dbNames     []string // CR names of both replicated databases
		dbSuffixes  map[string]string
		dbReplPWs   map[string]string
		logLevelSet bool
	)

	BeforeAll(func(ctx SpecContext) {
		db2Name = os.Getenv("DB2_CR_NAME")
		if db2Name == "" {
			Skip("DB2_CR_NAME not set — the resource set declares a single SlapdDatabase, " +
				"so the two-replicated-database ADR-019 coverage is not applicable")
		}

		By("reading both SlapdDatabase CRs")
		db1 := &ldapv1alpha1.SlapdDatabase{}
		Expect(crdClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: dbCRName}, db1)).To(Succeed())
		db2 := &ldapv1alpha1.SlapdDatabase{}
		Expect(crdClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: db2Name}, db2)).To(Succeed())
		db2Suffix = db2.Spec.Suffix
		Expect(db2Suffix).NotTo(BeEmpty())
		Expect(db2Suffix).NotTo(Equal(baseDN), "the second database must have its own suffix")

		Expect(db1.Spec.Replication.DeltaSync).NotTo(BeNil())
		Expect(*db1.Spec.Replication.DeltaSync).To(BeTrue(), "%s must be deltaSync for this fixture", dbCRName)
		Expect(db2.Spec.Replication.DeltaSync).NotTo(BeNil())
		Expect(*db2.Spec.Replication.DeltaSync).To(BeTrue(), "%s must be deltaSync for this fixture", db2Name)

		dbNames = []string{dbCRName, db2Name}
		dbSuffixes = map[string]string{dbCRName: baseDN, db2Name: db2Suffix}

		By("waiting for both SlapdDatabases to be Running")
		for _, n := range dbNames {
			name := n
			Eventually(ctx, func() bool {
				return slapdDatabaseRunning(crdClient, namespace, name)
			}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(BeTrue(),
				"SlapdDatabase %s never reached Running", name)
		}

		By("reading the second database's credentials")
		sec2Name := envOrDefault("DB2_CREDENTIALS_SECRET", db2Name+"-credentials")
		sec2, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx, sec2Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		db2AdminPW = string(sec2.Data["root-password"])
		db2ReplPW = string(sec2.Data["replication-password"])
		Expect(db2AdminPW).NotTo(BeEmpty())
		Expect(db2ReplPW).NotTo(BeEmpty())

		sec1Name := envOrDefault("DB_CREDENTIALS_SECRET", dbCRName+"-credentials")
		sec1, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx, sec1Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		db1ReplPW = string(sec1.Data["replication-password"])
		Expect(db1ReplPW).NotTo(BeEmpty())
		dbReplPWs = map[string]string{dbCRName: db1ReplPW, db2Name: db2ReplPW}

		By("reading the RW StatefulSet")
		sts, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, "slapd", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		replicas = 1
		if sts.Spec.Replicas != nil {
			replicas = *sts.Spec.Replicas
		}
		rwPods = nil
		for i := int32(0); i < replicas; i++ {
			rwPods = append(rwPods, fmt.Sprintf("slapd-%d", i))
		}

		// Sync debug is what makes the behavioural spec observable at all.
		sc := &ldapv1alpha1.SlapdCluster{}
		Expect(crdClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "slapd"}, sc)).To(Succeed())
		logLevelSet = sc.Spec.LogLevel&16384 != 0
	}, NodeTimeout(7*time.Minute))

	// ── 1. Structural ────────────────────────────────────────────────────────

	It("gives every replicated database its own accesslog DB, consistently referenced", func(ctx SpecContext) {
		for _, pod := range rwPods {
			By("inspecting cn=config on " + pod)
			conn := dialPodConfig(pod)
			assertPerDatabaseAccesslogLayout(conn, pod, dbNames, dbSuffixes, true)
			conn.Close()
		}
	}, NodeTimeout(5*time.Minute))

	// ── 2. Behavioural — the assertion that proves the mechanism ─────────────

	It("does not lose delta-sync on one database when the other is written to", func(ctx SpecContext) {
		if replicas < 2 {
			Skip("needs ≥2 RW replicas: delta-sync loss is a consumer-side symptom")
		}
		Expect(logLevelSet).To(BeTrue(),
			"SlapdCluster.spec.logLevel must include slapd's sync debug (16384) for this spec "+
				"to be able to observe `delta-sync lost sync`; tests/values.slapd-persistent.yaml sets 16640")

		// Put DB-B's consumers into log (delta) mode and make sure they are
		// caught up, so that anything they log afterwards is attributable to
		// the DB-A traffic below and not to their own initial refresh.
		By("warming up DB-B's consumers with one write of its own")
		conn2w := dialPodAs(rwPods[0], "cn=admin,"+db2Suffix, db2AdminPW)
		defer conn2w.Close()
		warm := addPersonEntry(conn2w, db2Suffix, fmt.Sprintf("aclog-warm-%d", GinkgoRandomSeed()))
		defer conn2w.Del(ldap.NewDelRequest(warm, nil)) //nolint:errcheck // best-effort cleanup
		conn2r := dialPodAs(rwPods[len(rwPods)-1], "cn=admin,"+db2Suffix, db2AdminPW)
		defer conn2r.Close()
		Eventually(ctx, func() bool { return ldapExists(conn2r, warm) }).
			WithTimeout(60 * time.Second).WithPolling(2 * time.Second).Should(BeTrue(),
			"DB-B warm-up entry %s must replicate before the window opens", warm)

		// Scope the assertion to exactly the writes below: record each pod's
		// current log length, and only ever look at what is appended after it.
		// Byte offsets rather than SinceTime because they need no agreement
		// between this runner's clock and the kubelet's, and because a
		// pre-existing `lost sync` from bootstrap must not be able to fail the
		// spec (nor a later one hide behind a coarse timestamp).
		By("recording each RW pod's current slapd log length")
		baseline := map[string]int{}
		for _, pod := range rwPods {
			baseline[pod] = len(podSlapdLog(ctx, pod))
		}

		By("writing sustained traffic to DB-A only")
		conn1 := dialPodAs(rwPods[0], "cn=admin,"+baseDN, adminPW)
		defer conn1.Close()
		const writes = 25
		var written []string
		for i := 0; i < writes; i++ {
			dn := addPersonEntry(conn1, baseDN, fmt.Sprintf("aclog-a-%d-%02d", GinkgoRandomSeed(), i))
			written = append(written, dn)
			time.Sleep(200 * time.Millisecond)
		}
		defer func() {
			for _, dn := range written {
				_ = conn1.Del(ldap.NewDelRequest(dn, nil)) //nolint:errcheck
			}
		}()

		By("waiting for DB-A to converge on every RW pod")
		for _, pod := range rwPods[1:] {
			c := dialPodAs(pod, "cn=admin,"+baseDN, adminPW)
			last := written[len(written)-1]
			Eventually(ctx, func() bool { return ldapExists(c, last) }).
				WithTimeout(90 * time.Second).WithPolling(2 * time.Second).Should(BeTrue(),
				"DB-A entry %s must reach %s", last, pod)
			c.Close()
		}

		By("letting DB-B's consumers settle")
		time.Sleep(20 * time.Second)

		By("asserting no pod lost delta-sync during the window")
		var offences []string
		for _, pod := range rwPods {
			window := podSlapdLog(ctx, pod)
			if len(window) >= baseline[pod] {
				window = window[baseline[pod]:]
			}
			for _, line := range strings.Split(window, "\n") {
				if lostSyncRE.MatchString(line) {
					offences = append(offences, pod+": "+strings.TrimSpace(line))
				}
			}
		}
		if len(offences) > 0 {
			// Report a sample, not the lot: against a shared log this runs into
			// the thousands (the failure oscillates, one line per retry per rid)
			// and Gomega truncates the dump mid-line, which is worse than a
			// deliberate excerpt.
			sample := offences
			if len(sample) > 10 {
				sample = sample[:10]
			}
			Fail(fmt.Sprintf(
				"writes to DB-A (%s) drove delta-sync loss while DB-B (%s) was idle — the "+
					"ADR-019 shared-accesslog mechanism (Facts 1-2). %d offending log line(s), "+
					"first %d:\n%s",
				baseDN, db2Suffix, len(offences), len(sample), strings.Join(sample, "\n")))
		}
	}, NodeTimeout(10*time.Minute))

	// ── 3. Convergence — a guard, not the proof ─────────────────────────────

	It("converges both databases on every RW pod", func(ctx SpecContext) {
		if replicas < 2 {
			Skip("needs ≥2 RW replicas")
		}
		// NOTE: this spec stays GREEN against a shared accesslog — convergence
		// then happens by repeated full-DIT refresh. It guards against outright
		// breakage; the behavioural spec above is what proves the mechanism.
		for _, dbName := range dbNames {
			suffix := dbSuffixes[dbName]
			pw := adminPW
			if dbName == db2Name {
				pw = db2AdminPW
			}

			By(fmt.Sprintf("writing to %s on %s", dbName, rwPods[0]))
			w := dialPodAs(rwPods[0], "cn=admin,"+suffix, pw)
			dn := addPersonEntry(w, suffix, fmt.Sprintf("aclog-conv-%s-%d", dbName, GinkgoRandomSeed()))

			for _, pod := range rwPods[1:] {
				r := dialPodAs(pod, "cn=admin,"+suffix, pw)
				Eventually(ctx, func() bool { return ldapExists(r, dn) }).
					WithTimeout(90 * time.Second).WithPolling(2 * time.Second).Should(BeTrue(),
					"%s must replicate to %s", dn, pod)
				r.Close()
			}
			_ = w.Del(ldap.NewDelRequest(dn, nil)) //nolint:errcheck
			w.Close()
		}
	}, NodeTimeout(8*time.Minute))

	// ── 4. ADR-020 access control ───────────────────────────────────────────

	It("restricts each accesslog to its database's replication identity", func(ctx SpecContext) {
		pod := rwPods[0]
		addr := podNodePortAddr(pod, "E2E_POD_NODEPORT_BASE")
		Expect(addr).NotTo(BeEmpty())

		for _, dbName := range dbNames {
			logSuffix := ldapv1alpha1.AccesslogSuffix(dbName)
			dataSuffix := dbSuffixes[dbName]
			pw := adminPW
			if dbName == db2Name {
				pw = db2AdminPW
			}

			// Guarantee the log has content: the accesslog overlay creates the
			// suffix entry on its first logged write.
			By("writing to " + dbName + " so its journal is non-empty")
			w := dialPodAs(pod, "cn=admin,"+dataSuffix, pw)
			dn := addPersonEntry(w, dataSuffix, fmt.Sprintf("aclog-acl-%s-%d", dbName, GinkgoRandomSeed()))
			_ = w.Del(ldap.NewDelRequest(dn, nil)) //nolint:errcheck
			w.Close()

			By("denying anonymous read of " + logSuffix)
			anon, err := ldap.Dial("tcp", addr)
			Expect(err).NotTo(HaveOccurred())
			res, err := anon.Search(ldap.NewSearchRequest(
				logSuffix, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
				0, 0, false, "(objectClass=*)", []string{"reqDN", "reqMod"}, nil))
			if err == nil {
				Expect(res.Entries).To(BeEmpty(),
					"anonymous read of %s must return nothing (ADR-020 R1); got %d entries",
					logSuffix, len(res.Entries))
			} else {
				GinkgoLogr.Info("anonymous accesslog search rejected", "suffix", logSuffix, "err", err.Error())
			}
			anon.Close()

			By("allowing cn=replication," + dataSuffix + " to read " + logSuffix)
			repl, err := ldap.Dial("tcp", addr)
			Expect(err).NotTo(HaveOccurred())
			Expect(repl.Bind("cn=replication,"+dataSuffix, dbReplPWs[dbName])).To(Succeed())
			Eventually(ctx, func() int {
				r, err := repl.Search(ldap.NewSearchRequest(
					logSuffix, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
					0, 0, false, "(objectClass=*)", []string{"dn"}, nil))
				if err != nil {
					fmt.Fprintf(GinkgoWriter, "replication read of %s: %v\n", logSuffix, err)
					return 0
				}
				return len(r.Entries)
			}).WithTimeout(60 * time.Second).WithPolling(3 * time.Second).Should(BeNumerically(">", 0),
				"cn=replication,%s must be able to read %s (ADR-020 R1/R2)", dataSuffix, logSuffix)
			repl.Close()
		}

		By("still advertising both accesslog suffixes in the rootDSE namingContexts")
		// Frontend-governed, so per-database ACLs must not hide them — and the
		// structural spec depends on it, so assert rather than assume (ADR-020
		// Consequences).
		anon, err := ldap.Dial("tcp", addr)
		Expect(err).NotTo(HaveOccurred())
		defer anon.Close()
		res, err := anon.Search(ldap.NewSearchRequest("",
			ldap.ScopeBaseObject, ldap.NeverDerefAliases,
			0, 0, false, "(objectClass=*)", []string{"namingContexts"}, nil))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Entries).To(HaveLen(1))
		ncs := res.Entries[0].GetAttributeValues("namingContexts")
		for _, dbName := range dbNames {
			Expect(ncs).To(ContainElement(BeEquivalentTo(ldapv1alpha1.AccesslogSuffix(dbName))),
				"rootDSE namingContexts must still list %s; got %v",
				ldapv1alpha1.AccesslogSuffix(dbName), ncs)
		}
	}, NodeTimeout(6*time.Minute))
})


// assertPerDatabaseAccesslogLayout is the ADR-019/ADR-020 end state, checked on
// one pod. Shared by the standard structural spec and the gated legacy-migration
// spec, which must converge to exactly this and nothing weaker.
//
// checkStanzas is false while a migration is still settling: olcSyncRepl is
// rewritten by a different reconcile step than the accesslog layout, so the two
// are not guaranteed to be observed in the same instant.
func assertPerDatabaseAccesslogLayout(
	conn *ldap.Conn,
	pod string,
	dbNames []string,
	dbSuffixes map[string]string,
	checkStanzas bool,
) {
	mdbs := observedMdbDatabases(conn)
	GinkgoLogr.Info("observed mdb databases", "pod", pod, "dbs", mdbs)

	// No legacy cluster-shared log may remain (ADR-019 R8).
	for _, db := range mdbs {
		Expect(strings.EqualFold(db.suffix, "cn=accesslog")).To(BeFalse(),
			"pod %s still carries the legacy cluster-shared accesslog %q at %q "+
				"— ADR-019 R8 convergence did not happen", pod, db.dn, db.dir)
	}

	logOwners := map[string][]string{} // log suffix → databases referencing it

	for _, dbName := range dbNames {
		dataSuffix := dbSuffixes[dbName]
		wantLog := ldapv1alpha1.AccesslogSuffix(dbName)
		wantDir := ldapv1alpha1.AccesslogDir(dbName)

		// (a) the log DB exists, with its own backing directory.
		logDB, ok := findMdb(mdbs, wantLog)
		Expect(ok).To(BeTrue(),
			"pod %s has no accesslog database %q for %s; observed: %v",
			pod, wantLog, dbName, mdbs)
		Expect(logDB.dir).To(Equal(wantDir),
			"accesslog DB %q on %s must be backed by its own directory", wantLog, pod)

		// (a2) with syncprov, and the ADR-020 ACL — the two attributes that must
		// never lag behind the DB's own existence (ADR-020 R5).
		Expect(logDBHasSyncprov(conn, logDB.dn)).To(BeTrue(),
			"pod %s: accesslog DB %q has no syncprov overlay — consumers cannot pull it",
			pod, wantLog)
		Expect(logDBAccess(conn, logDB.dn)).To(ConsistOf(
			MatchRegexp(`^(\{\d+\})?to \* by dn\.exact="cn=replication,`+regexp.QuoteMeta(dataSuffix)+`" read by \* none$`)),
			"pod %s: accesslog DB %q must carry exactly the ADR-020 R1 rule for %q",
			pod, wantLog, dataSuffix)

		// (b) the data DB's overlay names it.
		dataDB, ok := findMdb(mdbs, dataSuffix)
		Expect(ok).To(BeTrue(), "pod %s has no data database %q", pod, dataSuffix)
		overlayLog := accesslogOverlayTarget(conn, dataDB.dn)
		Expect(overlayLog).To(Equal(wantLog),
			"olcAccessLogDB on %s (%s) must name %q", dataDB.dn, pod, wantLog)

		// (c) every delta-syncrepl stanza on the data DB names it.
		if checkStanzas {
			stanzas := syncreplStanzas(conn, dataDB.dn)
			Expect(stanzas).NotTo(BeEmpty(),
				"pod %s: database %s has no olcSyncRepl stanzas", pod, dbName)
			for _, st := range stanzas {
				lb := logbaseOf(st)
				Expect(lb).To(Equal(wantLog),
					"pod %s: stanza for %s must carry logbase=%q; stanza: %s",
					pod, dbName, wantLog, st)
			}
		}

		logOwners[strings.ToLower(wantLog)] = append(logOwners[strings.ToLower(wantLog)], dbName)
	}

	// (d) an accesslog overlay exists on the replicated data DBs and NOWHERE
	// else. In particular a *log* DB must not carry one: a log journalling into
	// another log re-creates the ADR-019 Fact 2 mechanism (its consumers receive
	// reqDNs under the foreign log's suffix and cannot apply them), and it is
	// what an olcDatabase={N} DN going stale across a database delete looks like
	// from outside.
	overlays := allAccesslogOverlays(conn)
	wantParents := map[string]bool{}
	for _, dbName := range dbNames {
		dataDB, ok := findMdb(mdbs, dbSuffixes[dbName])
		Expect(ok).To(BeTrue())
		wantParents[strings.ToLower(dataDB.dn)] = true
	}
	for _, ov := range overlays {
		Expect(wantParents).To(HaveKey(strings.ToLower(ov.parentDN)),
			"pod %s: accesslog overlay on %s (naming %q) does not belong to any "+
				"replicated data database — a log DB journalling into another log "+
				"reintroduces ADR-019 Fact 2", pod, ov.parentDN, ov.logDB)
	}
	Expect(overlays).To(HaveLen(len(dbNames)),
		"pod %s: expected exactly one accesslog overlay per replicated database; got %v",
		pod, overlays)

	// (e) no two databases share a log — the exact condition ADR-019 forbids.
	Expect(logOwners).To(HaveLen(len(dbNames)),
		"pod %s: databases share accesslog DBs: %v", pod, logOwners)
	for logSuffix, owners := range logOwners {
		Expect(owners).To(HaveLen(1),
			"pod %s: accesslog %q is shared by %v", pod, logSuffix, owners)
	}
}

// logDBHasSyncprov reports whether an accesslog DB carries a syncprov overlay.
func logDBHasSyncprov(conn *ldap.Conn, logDN string) bool {
	res, err := conn.Search(ldap.NewSearchRequest(
		logDN, ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
		0, 0, false, "(objectClass=olcSyncProvConfig)", []string{"dn"}, nil))
	Expect(err).NotTo(HaveOccurred(), "search syncprov under %s", logDN)
	return len(res.Entries) > 0
}

// logDBAccess returns the olcAccess values on a database entry.
func logDBAccess(conn *ldap.Conn, dn string) []string {
	res, err := conn.Search(ldap.NewSearchRequest(
		dn, ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		0, 0, false, "(objectClass=*)", []string{"olcAccess"}, nil))
	Expect(err).NotTo(HaveOccurred(), "read olcAccess on %s", dn)
	Expect(res.Entries).To(HaveLen(1))
	return res.Entries[0].GetEqualFoldAttributeValues("olcAccess")
}

// nsName is a shorthand for the controller-runtime object key.
func nsName(ns, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: ns, Name: name}
}

// ── Spec-local helpers ──────────────────────────────────────────────────────

// observedMdb is one olcMdbConfig child of cn=config as read off a pod.
type observedMdb struct {
	dn     string
	suffix string
	dir    string
}

func (m observedMdb) String() string { return fmt.Sprintf("%s[%s @ %s]", m.dn, m.suffix, m.dir) }

// dialPodConfig connects to one pod and binds as the cn=config rootDN — the
// only identity that can read an accesslog DB's own configuration (and, per
// ADR-020 R3, the log's contents; the *data* rootDN is a different database's
// and is denied).
func dialPodConfig(pod string) *ldap.Conn {
	addr := podNodePortAddr(pod, "E2E_POD_NODEPORT_BASE")
	Expect(addr).NotTo(BeEmpty(), "E2E_NODE_IP and E2E_POD_NODEPORT_BASE must be set")
	conn, err := ldap.Dial("tcp", addr)
	Expect(err).NotTo(HaveOccurred(), "dial %s (%s)", pod, addr)
	Expect(conn.Bind("cn=admin,cn=config", rootPW)).To(Succeed(), "config bind on %s", pod)
	return conn
}

// dialPodAs connects to one pod and binds as an arbitrary DN. Needed because
// the second database has its own rootDN and its own password.
func dialPodAs(pod, bindDN, password string) *ldap.Conn {
	addr := podNodePortAddr(pod, "E2E_POD_NODEPORT_BASE")
	Expect(addr).NotTo(BeEmpty(), "E2E_NODE_IP and E2E_POD_NODEPORT_BASE must be set")
	var conn *ldap.Conn
	Eventually(func() bool {
		c, err := ldap.Dial("tcp", addr)
		if err != nil {
			return false
		}
		if err := c.Bind(bindDN, password); err != nil {
			c.Close()
			return false
		}
		conn = c
		return true
	}).WithTimeout(60 * time.Second).WithPolling(2 * time.Second).Should(BeTrue(),
		"bind as %s on %s (%s)", bindDN, pod, addr)
	return conn
}

// observedMdbDatabases lists every mdb database configured on this pod.
func observedMdbDatabases(conn *ldap.Conn) []observedMdb {
	res, err := conn.Search(ldap.NewSearchRequest(
		"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
		0, 0, false, "(objectClass=olcMdbConfig)",
		[]string{"olcSuffix", "olcDbDirectory"}, nil))
	Expect(err).NotTo(HaveOccurred(), "search olcMdbConfig under cn=config")
	var out []observedMdb
	for _, e := range res.Entries {
		out = append(out, observedMdb{
			dn:     e.DN,
			suffix: e.GetEqualFoldAttributeValue("olcSuffix"),
			dir:    e.GetEqualFoldAttributeValue("olcDbDirectory"),
		})
	}
	return out
}

func findMdb(mdbs []observedMdb, suffix string) (observedMdb, bool) {
	for _, m := range mdbs {
		if strings.EqualFold(strings.TrimSpace(m.suffix), suffix) {
			return m, true
		}
	}
	return observedMdb{}, false
}

// accesslogOverlayTarget returns the olcAccessLogDB of the accesslog overlay
// hanging under dataDN, or "" when there is no such overlay.
func accesslogOverlayTarget(conn *ldap.Conn, dataDN string) string {
	res, err := conn.Search(ldap.NewSearchRequest(
		dataDN, ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
		0, 0, false, "(objectClass=olcAccessLogConfig)",
		[]string{"olcAccessLogDB"}, nil))
	Expect(err).NotTo(HaveOccurred(), "search accesslog overlay under %s", dataDN)
	if len(res.Entries) == 0 {
		return ""
	}
	return strings.TrimSpace(res.Entries[0].GetEqualFoldAttributeValue("olcAccessLogDB"))
}

// observedAccesslogOverlay is one accesslog overlay anywhere under cn=config:
// the DN of the database it hangs on, and the log suffix it names.
type observedAccesslogOverlay struct {
	parentDN string
	logDB    string
}

func (o observedAccesslogOverlay) String() string {
	return fmt.Sprintf("%s→%s", o.parentDN, o.logDB)
}

// allAccesslogOverlays lists every accesslog overlay on the pod, regardless of
// which database it hangs on. Deliberately unscoped: the interesting failure is
// an overlay on a database that should not have one.
func allAccesslogOverlays(conn *ldap.Conn) []observedAccesslogOverlay {
	res, err := conn.Search(ldap.NewSearchRequest(
		"cn=config", ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
		0, 0, false, "(objectClass=olcAccessLogConfig)",
		[]string{"olcAccessLogDB"}, nil))
	Expect(err).NotTo(HaveOccurred(), "search accesslog overlays under cn=config")
	var out []observedAccesslogOverlay
	for _, e := range res.Entries {
		parent := e.DN
		if i := strings.Index(parent, ","); i >= 0 {
			parent = parent[i+1:]
		}
		out = append(out, observedAccesslogOverlay{
			parentDN: parent,
			logDB:    strings.TrimSpace(e.GetEqualFoldAttributeValue("olcAccessLogDB")),
		})
	}
	return out
}

// syncreplStanzas returns the olcSyncRepl values on a database entry.
func syncreplStanzas(conn *ldap.Conn, dataDN string) []string {
	res, err := conn.Search(ldap.NewSearchRequest(
		dataDN, ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		0, 0, false, "(objectClass=*)", []string{"olcSyncRepl"}, nil))
	Expect(err).NotTo(HaveOccurred(), "read olcSyncRepl on %s", dataDN)
	if len(res.Entries) == 0 {
		return nil
	}
	return res.Entries[0].GetEqualFoldAttributeValues("olcSyncRepl")
}

var logbaseRE = regexp.MustCompile(`logbase="([^"]*)"`)

// logbaseOf extracts the logbase of a syncrepl stanza, or "" when it carries none.
func logbaseOf(stanza string) string {
	m := logbaseRE.FindStringSubmatch(stanza)
	if m == nil {
		return ""
	}
	return m[1]
}

// podSlapdLog returns the whole current slapd container log of one pod.
func podSlapdLog(ctx context.Context, pod string) string {
	stream, err := k8sClient.CoreV1().Pods(namespace).
		GetLogs(pod, &corev1.PodLogOptions{Container: "slapd"}).Stream(ctx)
	Expect(err).NotTo(HaveOccurred(), "stream logs of %s", pod)
	defer stream.Close() //nolint:errcheck
	b, err := io.ReadAll(stream)
	Expect(err).NotTo(HaveOccurred(), "read logs of %s", pod)
	return string(b)
}

// addPersonEntry adds a minimal entry under ou=People,<suffix> and returns its DN.
// Like addReplTestUser, but for an arbitrary suffix — the second database has
// its own.
func addPersonEntry(conn *ldap.Conn, suffix, uid string) string {
	dn := fmt.Sprintf("uid=%s,ou=People,%s", uid, suffix)
	req := ldap.NewAddRequest(dn, nil)
	req.Attribute("objectClass", []string{"posixAccount", "shadowAccount", "inetOrgPerson"})
	req.Attribute("cn", []string{"Accesslog Test"})
	req.Attribute("sn", []string{"Test"})
	req.Attribute("uid", []string{uid})
	req.Attribute("uidNumber", []string{"65400"})
	req.Attribute("gidNumber", []string{"65400"})
	req.Attribute("homeDirectory", []string{"/dev/null"})
	ldapAdd(conn, req)
	return dn
}
