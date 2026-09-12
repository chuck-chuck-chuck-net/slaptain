package e2e_test

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The many-entries fixture class (ADR-024 Consequences).
//
// Every breaks-at-scale finding is structurally invisible to a single-digit-entry
// LDIF — which is exactly how a 500-entry replication cap survived a green
// suite for the whole life of the project. This spec is the standing
// verification vehicle for ADR-024's converged class: a GENERATED seed of well
// over a thousand entries, a churn loop that makes the change journal outgrow
// the same cap, and assertions that fail at the cap rather than at the fixture.
//
// The seed is generated rather than committed so the entry count is a knob
// (E2E_SCALE_ENTRIES) the slow lane can raise without a repository diff.
//
// Gated by E2E_SCALE=1: it writes thousands of entries to the shared fixture
// database and takes minutes. Everything it writes lives under its own OU and
// is removed in AfterAll.
//
// What each spec is red against on a pre-ADR-024 operator:
//
//	convergence  — syncrepl's search on the provider is capped at slapd's
//	               default 500 entries, so peers stop at 500. THE headline red.
//	client search— olcSizeLimit unset, so a client enumeration truncates at 500
//	               with a result code most callers never surface.
//	journal      — the ADR-020 spec's own read path: cn=replication reading its
//	               journal gets sizeLimitExceeded once the journal passes 500
//	               records, which is also how a consumer reads it.
//	structural   — olcDbMaxSize absent (→ back-mdb's ~10 MB) on both the data
//	               DB and the journal; entryCSN/entryUUID unindexed.
var _ = Describe("many entries", Label("scale"), Ordered, ContinueOnFailure, func() {

	var (
		scaleOU   string
		entries   int
		churn     int
		replicas  int32
		rwPods    []string
		replPW    string
		logSuffix string
		writeConn *ldap.Conn
		seededDNs []string
	)

	BeforeAll(func(ctx SpecContext) {
		if os.Getenv("E2E_SCALE") != "1" {
			Skip("E2E_SCALE not set; skipping the many-entries scale fixture " +
				"(writes thousands of entries, takes minutes)")
		}

		entries = envInt("E2E_SCALE_ENTRIES", 1200)
		Expect(entries).To(BeNumerically(">", 500),
			"the fixture must exceed slapd's default 500-entry sizelimit to be able to see the cap")
		churn = envInt("E2E_SCALE_CHURN", 700)

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

		By("reading the fixture database's replication credentials")
		sec, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx,
			envOrDefault("DB_CREDENTIALS_SECRET", dbCRName+"-credentials"), metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		replPW = string(sec.Data["replication-password"])
		Expect(replPW).NotTo(BeEmpty())
		logSuffix = ldapv1alpha1.AccesslogSuffix(dbCRName)

		scaleOU = fmt.Sprintf("ou=scale-%d,%s", GinkgoRandomSeed(), baseDN)

		By("seeding " + strconv.Itoa(entries) + " entries on " + rwPods[0])
		writeConn = dialPodAs(rwPods[0], "cn=admin,"+baseDN, adminPW)
		ouReq := ldap.NewAddRequest(scaleOU, nil)
		ouReq.Attribute("objectClass", []string{"organizationalUnit"})
		ouReq.Attribute("ou", []string{strings.TrimPrefix(strings.Split(scaleOU, ",")[0], "ou=")})
		ldapAdd(writeConn, ouReq)

		start := time.Now()
		for i := 0; i < entries; i++ {
			seededDNs = append(seededDNs, addScaleEntry(writeConn, scaleOU, i))
		}
		fmt.Fprintf(GinkgoWriter, "seeded %d entries in %s\n", entries, time.Since(start).Round(time.Second))
	}, NodeTimeout(20*time.Minute))

	AfterAll(func(ctx SpecContext) {
		if os.Getenv("E2E_SCALE") != "1" || writeConn == nil {
			return
		}
		By("removing the generated entries")
		for _, dn := range seededDNs {
			_ = writeConn.Del(ldap.NewDelRequest(dn, nil)) //nolint:errcheck // best-effort cleanup
		}
		_ = writeConn.Del(ldap.NewDelRequest(scaleOU, nil)) //nolint:errcheck
		writeConn.Close()
	}, NodeTimeout(20*time.Minute))

	// ── 1. Replication past the cap — the headline assertion ────────────────

	It("replicates every entry to every RW pod", func(ctx SpecContext) {
		if replicas < 2 {
			Skip("needs ≥2 RW replicas: a replication cap is a consumer-side symptom")
		}
		// Entry-exact, not "the last one arrived": a cap truncates the MIDDLE
		// of a refresh, so a spot check on one DN can pass while hundreds are
		// missing. Pre-ADR-024 this settles at exactly 500 and stays there.
		for _, pod := range rwPods[1:] {
			c := dialPodAs(pod, "cn=admin,"+baseDN, adminPW)
			var last int
			Eventually(ctx, func() int {
				last = countSubtree(c, scaleOU)
				return last
			}).WithTimeout(10*time.Minute).WithPolling(10*time.Second).Should(Equal(entries),
				"%s must hold all %d generated entries; a plateau at ~500 is slapd's default "+
					"sizelimit applying to the syncrepl search on the provider — the "+
					"replication identity needs an olcLimits exemption (ADR-020 amendment)",
				pod, entries)
			c.Close()
		}
	}, NodeTimeout(15*time.Minute))

	// ── 2. Client-facing search limits ──────────────────────────────────────

	It("returns every entry to an ordinary client search", func(ctx SpecContext) {
		// Bound as the replication identity would be too generous (it carries
		// its own exemption), and as rootDN too generous again (slapd exempts
		// it). An anonymous bind is what a plain client is: the fixture's ACLs
		// end in "to * by * read", so it can see the generated subtree, and it
		// is subject to whatever olcSizeLimit the database carries.
		addr := podNodePortAddr(rwPods[0], "E2E_POD_NODEPORT_BASE")
		Expect(addr).NotTo(BeEmpty())
		anon, err := ldap.Dial("tcp", addr)
		Expect(err).NotTo(HaveOccurred())
		defer anon.Close()

		res, err := anon.Search(ldap.NewSearchRequest(
			scaleOU, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
			0, 0, false, "(objectClass=inetOrgPerson)", []string{"dn"}, nil))
		Expect(err).NotTo(HaveOccurred(),
			"an unrestricted client enumeration of %d entries must not be refused; "+
				"sizeLimitExceeded here is slapd's default 500 (ADR-024 R5)", entries)
		Expect(res.Entries).To(HaveLen(entries),
			"a client enumeration must return all %d entries, not a silently truncated prefix", entries)
	}, NodeTimeout(6*time.Minute))

	// ── 3. Journal-heavy churn ──────────────────────────────────────────────

	It("keeps a journal-heavy churn readable and replicating", func(ctx SpecContext) {
		if replicas < 2 {
			Skip("needs ≥2 RW replicas")
		}

		By("recording each RW pod's current slapd log length")
		baseline := map[string]int{}
		for _, pod := range rwPods {
			baseline[pod] = len(podSlapdLog(ctx, pod))
		}

		By(fmt.Sprintf("modifying %d entries to push the change journal past the cap", churn))
		marker := fmt.Sprintf("churn-%d", GinkgoRandomSeed())
		n := churn
		if n > len(seededDNs) {
			n = len(seededDNs)
		}
		for i := 0; i < n; i++ {
			mod := ldap.NewModifyRequest(seededDNs[i], nil)
			mod.Replace("description", []string{marker})
			Expect(writeConn.Modify(mod)).To(Succeed(), "churn modify of %s", seededDNs[i])
		}

		By("reading the journal as the replication identity — the way a consumer does")
		repl := dialPodAs(rwPods[0], "cn=replication,"+baseDN, replPW)
		defer repl.Close()
		// A consumer reads the whole journal; if its search is capped the log
		// is unusable past the cap, which is a silent replication stall rather
		// than an error anyone sees. > 500 is the assertion because 500 is
		// exactly where the default lands.
		Eventually(ctx, func() int {
			r, err := repl.Search(ldap.NewSearchRequest(
				logSuffix, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
				0, 0, false, "(objectClass=auditWriteObject)", []string{"reqDN"}, nil))
			if err != nil {
				fmt.Fprintf(GinkgoWriter, "journal read of %s: %v\n", logSuffix, err)
				return 0
			}
			return len(r.Entries)
		}).WithTimeout(5*time.Minute).WithPolling(10*time.Second).Should(BeNumerically(">", 500),
			"cn=replication,%s must be able to read more than 500 records from %s; "+
				"a sizeLimitExceeded or a plateau at 500 is the replication identity's "+
				"missing olcLimits exemption (ADR-020 amendment)", baseDN, logSuffix)

		By("waiting for the churn to converge on every RW pod")
		for _, pod := range rwPods[1:] {
			c := dialPodAs(pod, "cn=admin,"+baseDN, adminPW)
			Eventually(ctx, func() int {
				return countMatching(c, scaleOU, fmt.Sprintf("(description=%s)", marker))
			}).WithTimeout(10*time.Minute).WithPolling(10*time.Second).Should(Equal(n),
				"all %d churned entries must carry the new description on %s", n, pod)
			c.Close()
		}

		By("asserting no pod lost delta-sync during the churn")
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
			Fail(fmt.Sprintf("a journal-heavy churn drove delta-sync loss on %d log line(s), first %d:\n%s",
				len(offences), len(sample), strings.Join(sample, "\n")))
		}
	}, NodeTimeout(25*time.Minute))

	// ── 4. Structural: the tunables that have no runtime symptom below scale ─

	It("carries the operator's scale tunables on every pod", func(ctx SpecContext) {
		db := &ldapv1alpha1.SlapdDatabase{}
		Expect(crdClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: dbCRName}, db)).To(Succeed())

		for _, pod := range rwPods {
			By("inspecting cn=config on " + pod)
			conn := dialPodConfigEventually(ctx, pod)

			dataDN := configDBDN(conn, baseDN)
			Expect(dataDN).NotTo(BeEmpty(), "pod %s: no data DB for %s", pod, baseDN)

			// Map size: absent means back-mdb's ~10 MB, i.e. a directory that
			// stops accepting writes the moment it outgrows a fixture.
			assertMapSize(conn, pod, dataDN, "data DB")

			// entryCSN/entryUUID: searched by syncrepl itself on every refresh
			// and by out-of-order modify resolution. Unindexed, each is a full
			// scan on a replication hot path.
			idx := strings.ToLower(strings.Join(
				ldapSearchAttr(conn, dataDN, "olcDbIndex"), " "))
			for _, attr := range []string{"objectclass", "entrycsn", "entryuuid"} {
				Expect(idx).To(ContainSubstring(attr),
					"pod %s: data DB %s must index %s (ADR-024 R7); olcDbIndex = %q",
					pod, dataDN, attr, idx)
			}

			// The replication identity's limits exemption, on the data DB…
			assertReplicationLimits(conn, pod, dataDN, baseDN)

			// …and on its journal, which is the other half of the same cap.
			logDN := configDBDN(conn, logSuffix)
			Expect(logDN).NotTo(BeEmpty(), "pod %s: no accesslog DB for %s", pod, logSuffix)
			assertMapSize(conn, pod, logDN, "accesslog DB")
			assertReplicationLimits(conn, pod, logDN, baseDN)

			conn.Close()
		}
	}, NodeTimeout(8*time.Minute))
})

// ── helpers ──────────────────────────────────────────────────────────────────

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		Expect(err).NotTo(HaveOccurred(), "%s must be an integer, got %q", key, v)
		return n
	}
	return def
}

// addScaleEntry writes one generated inetOrgPerson. Deterministic given the
// index, so a failure names an entry you can look up by hand.
func addScaleEntry(conn *ldap.Conn, ou string, i int) string {
	uid := fmt.Sprintf("scaleuser%05d", i)
	dn := fmt.Sprintf("uid=%s,%s", uid, ou)
	req := ldap.NewAddRequest(dn, nil)
	req.Attribute("objectClass", []string{"posixAccount", "shadowAccount", "inetOrgPerson"})
	req.Attribute("cn", []string{"Scale User " + strconv.Itoa(i)})
	req.Attribute("sn", []string{"User"})
	req.Attribute("uid", []string{uid})
	req.Attribute("uidNumber", []string{strconv.Itoa(70000 + i)})
	req.Attribute("gidNumber", []string{"65400"})
	req.Attribute("homeDirectory", []string{"/dev/null"})
	ldapAdd(conn, req)
	return dn
}

// countSubtree counts the generated people under a base, paging past any
// server-side size limit so the COUNT is honest even when the limit is the
// thing under test: a capped search is reported as the cap, not as an error
// that hides how far it got.
func countSubtree(conn *ldap.Conn, base string) int {
	return countMatching(conn, base, "(objectClass=inetOrgPerson)")
}

func countMatching(conn *ldap.Conn, base, filter string) int {
	res, err := conn.Search(ldap.NewSearchRequest(
		base, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
		0, 0, false, filter, []string{"dn"}, nil))
	if err != nil {
		if res != nil {
			fmt.Fprintf(GinkgoWriter, "search %s %s: %v (partial: %d)\n",
				base, filter, err, len(res.Entries))
			return len(res.Entries)
		}
		fmt.Fprintf(GinkgoWriter, "search %s %s: %v\n", base, filter, err)
		return 0
	}
	return len(res.Entries)
}

// configDBDN resolves the olcDatabase={N}mdb DN carrying a suffix.
func configDBDN(conn *ldap.Conn, suffix string) string {
	res, err := conn.Search(ldap.NewSearchRequest(
		"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
		0, 0, false, fmt.Sprintf("(&(objectClass=olcMdbConfig)(olcSuffix=%s))", suffix),
		[]string{"dn"}, nil))
	if err != nil || len(res.Entries) == 0 {
		return ""
	}
	return res.Entries[0].DN
}

func ldapSearchAttr(conn *ldap.Conn, dn, attr string) []string {
	res, err := conn.Search(ldap.NewSearchRequest(
		dn, ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		1, 0, false, "(objectClass=*)", []string{attr}, nil))
	Expect(err).NotTo(HaveOccurred(), "read %s on %s", attr, dn)
	Expect(res.Entries).To(HaveLen(1))
	return res.Entries[0].GetEqualFoldAttributeValues(attr)
}

func assertMapSize(conn *ldap.Conn, pod, dn, what string) {
	vals := ldapSearchAttr(conn, dn, "olcDbMaxSize")
	Expect(vals).To(HaveLen(1),
		"pod %s: %s %s must carry an olcDbMaxSize; absent means back-mdb's ~10 MB default "+
			"and a database that stops accepting writes (ADR-024 R1)", pod, what, dn)
	n, err := strconv.ParseInt(vals[0], 10, 64)
	Expect(err).NotTo(HaveOccurred(),
		"pod %s: %s olcDbMaxSize = %q, which slapd takes as a bare byte count", pod, what, vals[0])
	Expect(n).To(BeNumerically(">=", int64(1)<<30),
		"pod %s: %s map size %d bytes is below a gigabyte", pod, what, n)
}

func assertReplicationLimits(conn *ldap.Conn, pod, dn, dataSuffix string) {
	vals := ldapSearchAttr(conn, dn, "olcLimits")
	joined := strings.Join(vals, " | ")
	Expect(joined).To(ContainSubstring(`dn.exact="cn=replication,`+dataSuffix+`"`),
		"pod %s: %s must grant the replication identity a limits exemption; olcLimits = %q "+
			"(ADR-020 amendment: the ACL says who may read the journal, the limits say how much)",
		pod, dn, joined)
	Expect(strings.ToLower(joined)).To(ContainSubstring("size.soft=unlimited"),
		"pod %s: %s replication limits must be unlimited in size; got %q", pod, dn, joined)
}
