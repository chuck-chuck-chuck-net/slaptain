package e2e_test

import (
	"fmt"
	"os"
	"strings"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// ADR-019 R8: converging a legacy cluster off its single cluster-shared
// cn=accesslog. Gated by E2E_ACCESSLOG_MIGRATION=1 — it rewrites cn=config on
// every RW pod by hand and is destructive to the cluster's journals, which is
// not something the standard suite should do to the shared fixture.
//
// The pre-state is MANUFACTURED rather than deployed from an old image: on a
// HEAD cluster, as the cn=config rootDN, drop each database's per-DB log and its
// overlay, insert a single cn=accesslog at /accesslog, and repoint both
// overlays at it. That is exactly the shape a pre-ADR-019 operator left behind.
//
// One detail carries the whole point of the spec. slapd's olcDatabase={N} index
// is POSITIONAL, and the legacy log on a real cluster was created during the
// FIRST replicated database's reconcile — so a database added later sits at a
// HIGHER index than the log. Deleting the log therefore renumbers that database
// downward, and an operator holding a DN it resolved before the delete would
// then write to the wrong database. Appending the manufactured log at the end
// (the natural result of an ldapadd) would NOT reproduce that, so the log is
// inserted at the lowest data database's index — slapd honours an explicit {N}
// and shifts the rest up. Verified live: inserting {2} moved
// cn=accesslog-example-db2 from {2} to {3} and dc=example,dc=org from {3} to {4}.
//
// Observed against the build before the fix: the migration left an accesslog
// overlay on the *other* database's accesslog DB, so every write to one database
// landed a foreign reqDN in the other's journal and its consumers logged
// thousands of `delta-sync lost sync` lines.
var _ = Describe("legacy shared accesslog migration", Label("accesslog-migration"), Ordered, func() {

	var (
		db2Name    string
		db2Suffix  string
		db2AdminPW string
		replicas   int32
		rwPods     []string
		dbNames    []string
		dbSuffixes map[string]string
	)

	BeforeAll(func(ctx SpecContext) {
		if os.Getenv("E2E_ACCESSLOG_MIGRATION") != "1" {
			Skip("set E2E_ACCESSLOG_MIGRATION=1 to run the ADR-019 R8 legacy-accesslog migration scenario " +
				"(it rewrites cn=config on every pod by hand)")
		}
		db2Name = os.Getenv("DB2_CR_NAME")
		if db2Name == "" {
			Skip("DB2_CR_NAME not set — the migration scenario needs two replicated databases " +
				"to exercise the reference-counted teardown and the renumbering hazard")
		}

		db2 := &ldapv1alpha1.SlapdDatabase{}
		Expect(crdClient.Get(ctx, nsName(namespace, db2Name), db2)).To(Succeed())
		db2Suffix = db2.Spec.Suffix
		dbNames = []string{dbCRName, db2Name}
		dbSuffixes = map[string]string{dbCRName: baseDN, db2Name: db2Suffix}

		sec2Name := envOrDefault("DB2_CREDENTIALS_SECRET", db2Name+"-credentials")
		sec2, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx, sec2Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		db2AdminPW = string(sec2.Data["root-password"])
		Expect(db2AdminPW).NotTo(BeEmpty())

		sts, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, "slapd", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		replicas = 1
		if sts.Spec.Replicas != nil {
			replicas = *sts.Spec.Replicas
		}
		if replicas < 2 {
			Skip("needs ≥2 RW replicas: the hazard is consumer-side")
		}
		rwPods = nil
		for i := int32(0); i < replicas; i++ {
			rwPods = append(rwPods, fmt.Sprintf("slapd-%d", i))
		}
	}, NodeTimeout(2*time.Minute))

	It("converges a hand-made legacy shared accesslog to per-database logs", func(ctx SpecContext) {
		for _, pod := range rwPods {
			By("manufacturing the legacy shared-accesslog layout on " + pod)
			makeLegacyAccesslogLayout(pod, dbNames, dbSuffixes)

			By("confirming the pre-state on " + pod)
			conn := dialPodConfig(pod)
			mdbs := observedMdbDatabases(conn)
			legacy, ok := findMdb(mdbs, "cn=accesslog")
			Expect(ok).To(BeTrue(), "pod %s: no legacy cn=accesslog was created; got %v", pod, mdbs)
			Expect(legacy.dir).To(Equal(ldapv1alpha1.AccesslogRoot))
			for _, dbName := range dbNames {
				// Not asserted: the absence of the per-database log. The
				// operator reconciles concurrently and may already have
				// re-created it — which changes nothing about the path under
				// test, because R8 is driven by the legacy log plus an overlay
				// naming it, and ensureAccesslogDB is a no-op on a log that
				// already exists.
				dataDB, ok := findMdb(mdbs, dbSuffixes[dbName])
				Expect(ok).To(BeTrue())
				Expect(accesslogOverlayTarget(conn, dataDB.dn)).To(Equal("cn=accesslog"),
					"pod %s: %s's overlay must name the shared log in the pre-state", pod, dbName)
			}
			// The renumbering hazard: at least one data database must be
			// ordered AFTER the legacy log, or deleting the log shifts nothing
			// and the scenario proves less than it claims.
			Expect(anyDataDBAbove(mdbs, legacy.dn, dbSuffixes)).To(BeTrue(),
				"pod %s: no data database is ordered after %s — the manufactured pre-state "+
					"does not reproduce the renumbering hazard; %v", pod, legacy.dn, mdbs)
			conn.Close()
		}

		By("writing to both databases so the shared log carries traffic from each")
		for _, dbName := range dbNames {
			pw := adminPW
			if dbName == db2Name {
				pw = db2AdminPW
			}
			w := dialPodAs(rwPods[0], "cn=admin,"+dbSuffixes[dbName], pw)
			dn := addPersonEntry(w, dbSuffixes[dbName], fmt.Sprintf("aclog-mig-%s-%d", dbName, GinkgoRandomSeed()))
			_ = w.Del(ldap.NewDelRequest(dn, nil)) //nolint:errcheck
			w.Close()
		}

		By("waiting for the operator to converge every pod onto per-database logs")
		for _, p := range rwPods {
			pod := p
			Eventually(ctx, func(g Gomega) {
				conn := dialPodConfig(pod)
				defer conn.Close()
				mdbs := observedMdbDatabases(conn)
				for _, db := range mdbs {
					g.Expect(strings.EqualFold(db.suffix, "cn=accesslog")).To(BeFalse(),
						"pod %s still carries the legacy shared log", pod)
				}
				for _, dbName := range dbNames {
					_, found := findMdb(mdbs, ldapv1alpha1.AccesslogSuffix(dbName))
					g.Expect(found).To(BeTrue(), "pod %s has no log for %s yet", pod, dbName)
				}
			}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
		}

		By("asserting the full ADR-019/ADR-020 end state on every pod")
		for _, pod := range rwPods {
			conn := dialPodConfig(pod)
			assertPerDatabaseAccesslogLayout(conn, pod, dbNames, dbSuffixes, false)
			conn.Close()
		}
	}, NodeTimeout(12*time.Minute))

	It("is idempotent across repeated reconciles", func(ctx SpecContext) {
		snapshot := func() map[string]string {
			out := map[string]string{}
			for _, pod := range rwPods {
				conn := dialPodConfig(pod)
				out[pod] = configFingerprint(conn)
				conn.Close()
			}
			return out
		}

		By("fingerprinting cn=config on every pod")
		before := snapshot()

		// The SlapdDatabase controller requeues on a ~10 s cadence while the
		// cluster is settling and re-reconciles on every CR/Secret event, so a
		// 45 s window is several passes. A migration that is not idempotent
		// shows up as a moving fingerprint (an overlay re-added, a log
		// re-created, a DN renumbering again).
		By("letting several reconciles run")
		time.Sleep(45 * time.Second)

		after := snapshot()
		for _, pod := range rwPods {
			Expect(after[pod]).To(Equal(before[pod]),
				"pod %s: the accesslog layout changed across repeated reconciles — "+
					"the R8 convergence is not idempotent", pod)
		}
	}, NodeTimeout(5*time.Minute))

	It("leaves delta-syncrepl intact after the migration", func(ctx SpecContext) {
		// The migration's own full refresh is expected (ADR-019 R8 accepts one
		// per consumer). What must NOT survive it is Fact 2: writes to one
		// database killing the other database's delta-sync. Same technique as
		// the standard behavioural spec, run after convergence.
		By("recording each RW pod's current slapd log length")
		baseline := map[string]int{}
		for _, pod := range rwPods {
			baseline[pod] = len(podSlapdLog(ctx, pod))
		}

		By("writing sustained traffic to DB-A only")
		conn1 := dialPodAs(rwPods[0], "cn=admin,"+baseDN, adminPW)
		defer conn1.Close()
		var written []string
		for i := 0; i < 15; i++ {
			dn := addPersonEntry(conn1, baseDN, fmt.Sprintf("aclog-migb-%d-%02d", GinkgoRandomSeed(), i))
			written = append(written, dn)
			time.Sleep(200 * time.Millisecond)
		}
		defer func() {
			for _, dn := range written {
				_ = conn1.Del(ldap.NewDelRequest(dn, nil)) //nolint:errcheck
			}
		}()

		By("waiting for DB-A to converge and DB-B to settle")
		for _, pod := range rwPods[1:] {
			c := dialPodAs(pod, "cn=admin,"+baseDN, adminPW)
			last := written[len(written)-1]
			Eventually(ctx, func() bool { return ldapExists(c, last) }).
				WithTimeout(90*time.Second).WithPolling(2*time.Second).Should(BeTrue(),
				"DB-A entry %s must reach %s", last, pod)
			c.Close()
		}
		time.Sleep(20 * time.Second)

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
			sample := offences
			if len(sample) > 10 {
				sample = sample[:10]
			}
			Fail(fmt.Sprintf(
				"after the R8 migration, writes to DB-A still drive delta-sync loss on DB-B — "+
					"the migration left an ADR-019 Fact 2 configuration behind. %d line(s), first %d:\n%s",
				len(offences), len(sample), strings.Join(sample, "\n")))
		}
	}, NodeTimeout(10*time.Minute))
})

// ── Pre-state manufacture ───────────────────────────────────────────────────

// makeLegacyAccesslogLayout rewrites one pod's cn=config into the pre-ADR-019
// shape: no per-database logs, one cn=accesslog at the accesslog mount root,
// both data databases' accesslog overlays pointing at it — and the shared log
// ordered BEFORE at least one data database, which is what makes its deletion
// renumber a DN the operator may be holding.
//
// Every operation dials its own connection. slapd drops client connections when
// databases are added to or removed from cn=config underneath them (observed:
// "unable to read LDAP response packet: EOF" mid-surgery), so a long-lived
// connection across this sequence is not viable — the same reason the suite
// carries refreshLDAPConn.
func makeLegacyAccesslogLayout(pod string, dbNames []string, dbSuffixes map[string]string) {
	// 1. Drop each database's accesslog overlay and its per-database log.
	for _, dbName := range dbNames {
		mdbs := cfgMdbDatabases(pod)
		dataDB, ok := findMdb(mdbs, dbSuffixes[dbName])
		Expect(ok).To(BeTrue(), "pod %s has no data database %q", pod, dbSuffixes[dbName])
		for _, e := range cfgSearch(pod, dataDB.dn, ldap.ScopeSingleLevel, "(objectClass=olcAccessLogConfig)") {
			cfgDel(pod, e.DN)
		}
		// Best effort: the operator reconciles concurrently and may re-create
		// the per-database log while this runs. That is harmless — what the
		// scenario needs is the legacy log below a data DB with both overlays
		// naming it, which is what actually drives the R8 path. Removing the
		// per-database logs too makes the pre-state a faithful pre-ADR-019
		// cn=config when it wins the race, so it is worth attempting.
		if logDB, ok := findMdb(mdbs, ldapv1alpha1.AccesslogSuffix(dbName)); ok {
			cfgDelSubtreeBestEffort(pod, logDB.dn)
		}
	}

	// 2. Insert the shared log at the lowest data database's index, so every
	//    data database is ordered after it. slapd honours an explicit {N} on
	//    add and shifts the rest up.
	idx := lowestDataDBIndex(cfgMdbDatabases(pod), dbSuffixes)
	Expect(idx).To(BeNumerically(">", 0), "pod %s: could not determine a data DB index", pod)
	legacyDN := fmt.Sprintf("olcDatabase={%d}mdb,cn=config", idx)
	add := ldap.NewAddRequest(legacyDN, nil)
	add.Attribute("objectClass", []string{"olcDatabaseConfig", "olcMdbConfig"})
	add.Attribute("olcDatabase", []string{fmt.Sprintf("{%d}mdb", idx)})
	add.Attribute("olcSuffix", []string{"cn=accesslog"})
	add.Attribute("olcRootDN", []string{"cn=admin,cn=config"})
	add.Attribute("olcDbDirectory", []string{ldapv1alpha1.AccesslogRoot})
	add.Attribute("olcDbIndex", []string{"default eq", "reqEnd,reqResult,reqStart eq"})
	// Deliberately NO olcAccess: a pre-ADR-020 log inherited the frontend
	// default, which is *read*. The end-state assertion requires the operator to
	// have written the ADR-020 rule, so the pre-state must not supply it.
	cfgAdd(pod, add)

	// 3. Point both databases' overlays at the shared log. Indices have shifted,
	//    so re-read before addressing anything.
	for _, dbName := range dbNames {
		dataDB, ok := findMdb(cfgMdbDatabases(pod), dbSuffixes[dbName])
		Expect(ok).To(BeTrue())
		ov := ldap.NewAddRequest("olcOverlay=accesslog,"+dataDB.dn, nil)
		ov.Attribute("objectClass", []string{"olcOverlayConfig", "olcAccessLogConfig"})
		ov.Attribute("olcOverlay", []string{"accesslog"})
		ov.Attribute("olcAccessLogDB", []string{"cn=accesslog"})
		ov.Attribute("olcAccessLogOps", []string{"writes"})
		ov.Attribute("olcAccessLogSuccess", []string{"TRUE"})
		cfgAdd(pod, ov)
	}
}

// cfgSearch runs one cn=config search on a pod over a fresh connection.
func cfgSearch(pod, base string, scope int, filter string) []*ldap.Entry {
	var out []*ldap.Entry
	Eventually(func() error {
		conn := dialPodConfig(pod)
		defer conn.Close()
		res, err := conn.Search(ldap.NewSearchRequest(
			base, scope, ldap.NeverDerefAliases, 0, 0, false, filter,
			[]string{"olcSuffix", "olcDbDirectory"}, nil))
		if err != nil {
			if ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
				out = nil
				return nil
			}
			return err
		}
		out = res.Entries
		return nil
	}).WithTimeout(60*time.Second).WithPolling(2*time.Second).Should(Succeed(),
		"search %s under %s on %s", filter, base, pod)
	return out
}

// cfgMdbDatabases lists the pod's mdb databases over a fresh connection.
func cfgMdbDatabases(pod string) []observedMdb {
	var out []observedMdb
	for _, e := range cfgSearch(pod, "cn=config", ldap.ScopeSingleLevel, "(objectClass=olcMdbConfig)") {
		out = append(out, observedMdb{
			dn:     e.DN,
			suffix: e.GetEqualFoldAttributeValue("olcSuffix"),
			dir:    e.GetEqualFoldAttributeValue("olcDbDirectory"),
		})
	}
	return out
}

// cfgAdd adds one cn=config entry; an existing entry counts as success.
func cfgAdd(pod string, req *ldap.AddRequest) {
	Eventually(func() error {
		conn := dialPodConfig(pod)
		defer conn.Close()
		err := conn.Add(req)
		if err != nil && ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists) {
			return nil
		}
		return err
	}).WithTimeout(60*time.Second).WithPolling(2*time.Second).Should(Succeed(),
		"add %s on %s", req.DN, pod)
}

// cfgDel deletes one cn=config entry; a missing entry counts as success.
func cfgDel(pod, dn string) {
	Eventually(func() error {
		conn := dialPodConfig(pod)
		defer conn.Close()
		err := conn.Del(ldap.NewDelRequest(dn, nil))
		if err != nil && ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
			return nil
		}
		return err
	}).WithTimeout(60*time.Second).WithPolling(2*time.Second).Should(Succeed(),
		"delete %s on %s", dn, pod)
}

// cfgDelSubtreeBestEffort deletes a cn=config entry and its children, re-listing
// the children on every attempt: slapd refuses to delete a non-leaf ("Not
// Allowed On Non Leaf"), and the operator can re-add a child between the listing
// and the delete. Gives up quietly rather than failing the spec — see the call
// site for why absence is not load-bearing.
func cfgDelSubtreeBestEffort(pod, dn string) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		conn := dialPodConfig(pod)
		children, err := conn.Search(ldap.NewSearchRequest(
			dn, ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
			0, 0, false, "(objectClass=*)", []string{"dn"}, nil))
		if err == nil {
			for _, c := range children.Entries {
				_ = conn.Del(ldap.NewDelRequest(c.DN, nil)) //nolint:errcheck // retried
			}
			err = conn.Del(ldap.NewDelRequest(dn, nil))
		}
		conn.Close()
		if err == nil || ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
			return
		}
		fmt.Fprintf(GinkgoWriter, "cfgDelSubtree(%s on %s): %v — retrying\n", dn, pod, err)
		time.Sleep(2 * time.Second)
	}
	fmt.Fprintf(GinkgoWriter, "cfgDelSubtree(%s on %s): gave up; the operator keeps re-creating it\n", dn, pod)
}

// configDBIndex extracts N from an olcDatabase={N}<backend>,cn=config DN.
// Returns 0 when the DN carries no index.
func configDBIndex(dn string) int {
	lo := strings.Index(dn, "{")
	hi := strings.Index(dn, "}")
	if lo < 0 || hi < lo {
		return 0
	}
	n := 0
	if _, err := fmt.Sscanf(dn[lo+1:hi], "%d", &n); err != nil {
		return 0
	}
	return n
}

// lowestDataDBIndex returns the smallest olcDatabase={N} index held by one of
// the fixture's data databases.
func lowestDataDBIndex(mdbs []observedMdb, dbSuffixes map[string]string) int {
	lowest := 0
	for _, suffix := range dbSuffixes {
		db, ok := findMdb(mdbs, suffix)
		if !ok {
			continue
		}
		idx := configDBIndex(db.dn)
		if idx > 0 && (lowest == 0 || idx < lowest) {
			lowest = idx
		}
	}
	return lowest
}

// anyDataDBAbove reports whether at least one data database is ordered after
// legacyDN — the precondition for the deletion of legacyDN to renumber a data
// database's DN.
func anyDataDBAbove(mdbs []observedMdb, legacyDN string, dbSuffixes map[string]string) bool {
	legacyIdx := configDBIndex(legacyDN)
	for _, suffix := range dbSuffixes {
		if db, ok := findMdb(mdbs, suffix); ok && configDBIndex(db.dn) > legacyIdx {
			return true
		}
	}
	return false
}

// configFingerprint renders the accesslog-relevant parts of one pod's cn=config
// as a stable string: every mdb database with its suffix and directory, and
// every accesslog overlay with its parent and target.
func configFingerprint(conn *ldap.Conn) string {
	var b strings.Builder
	for _, db := range observedMdbDatabases(conn) {
		fmt.Fprintf(&b, "db %s suffix=%s dir=%s\n", db.dn, db.suffix, db.dir)
	}
	for _, ov := range allAccesslogOverlays(conn) {
		fmt.Fprintf(&b, "overlay parent=%s log=%s\n", ov.parentDN, ov.logDB)
	}
	return b.String()
}
