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

// The degrades-at-scale and hygiene tunables (ADR-024, production-config review
// findings 6-16).
//
// Unlike the many-entries fixture in scale_test.go, none of these needs volume
// to be observable: they are attributes in cn=config, present or absent. What
// they have in common with it is that their ABSENCE has no symptom on a healthy
// fixture — a cluster with no connection timeouts, no checkpoint, no TLS floor
// and no monitor backend passes every other spec in this suite — so an assertion
// on cn=config is the only thing standing between "we decided this" and "we
// stopped setting it two releases ago".
//
// cn=config is node-local (ADR-002), so every assertion runs against every pod
// independently, read-only pods included: a tunable converged on two pods out of
// three is a pod that behaves differently from its peers under exactly the
// conditions nobody is watching.
//
// Ungated on purpose. These cost one bind and a handful of base searches per
// pod, which buys a standing guard on twelve decisions.
var _ = Describe("scale and operations tunables", Label("tunables"), Ordered, ContinueOnFailure, func() {

	var (
		pods   []string
		rwPods []string
	)

	BeforeAll(func(ctx SpecContext) {
		sts, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, "slapd", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		replicas := int32(1)
		if sts.Spec.Replicas != nil {
			replicas = *sts.Spec.Replicas
		}
		for i := int32(0); i < replicas; i++ {
			rwPods = append(rwPods, fmt.Sprintf("slapd-%d", i))
		}
		pods = append(pods, rwPods...)

		// Read-only replicas are separate pods with their own cn=config. They
		// serve clients, so the connection lifetimes and the TLS floor matter
		// there for exactly the same reasons.
		if roSts, err := k8sClient.AppsV1().StatefulSets(namespace).
			Get(ctx, "slapd-readonly", metav1.GetOptions{}); err == nil {
			ro := int32(0)
			if roSts.Spec.Replicas != nil {
				ro = *roSts.Spec.Replicas
			}
			for i := int32(0); i < ro; i++ {
				pods = append(pods, fmt.Sprintf("slapd-readonly-%d", i))
			}
		}
		Expect(pods).NotTo(BeEmpty())
	})

	// ── Server-global: connection lifetimes, tool threads, TLS, password hash ─

	It("pins the server-global tunables on every pod", func(ctx SpecContext) {
		for _, pod := range pods {
			By("inspecting cn=config on " + pod)
			conn := dialPodConfigEventually(ctx, pod)

			// NOT asserted: olcIdleTimeout and olcWriteTimeout. The
			// production-config review wanted both converged; writing either on
			// a running slapd 2.7.1 hangs the process (see
			// ensureGlobalTunables for the captured evidence), so the operator
			// does not write them and there is nothing here to guard. They stay
			// in docs/BACKLOG.md as a bootstrap-time item.

			// slapadd reads this during a restore, with the cluster at zero
			// replicas for the whole window (finding 12).
			Expect(atoi(singleConfigValue(conn, "cn=config", "olcToolThreads"))).
				To(BeNumerically(">=", 1), "pod %s: olcToolThreads", pod)

			// Without a floor, the minimum protocol version is whatever the
			// runtime image's OpenSSL permits — a policy that changes silently
			// on a base-image bump (finding 15). 3.3 is TLS 1.2 in slapd's
			// <major>.<minor> spelling.
			min := singleConfigValue(conn, "cn=config", "olcTLSProtocolMin")
			Expect(min).NotTo(Equal("0.0"),
				"pod %s: olcTLSProtocolMin is unpinned (0.0)", pod)
			Expect(min).To(BeElementOf("3.3", "3.4"),
				"pod %s: the TLS floor must be TLS 1.2 or better, got %q", pod, min)

			// The scheme slapd uses when IT hashes a password for a client
			// (finding 16).
			Expect(singleConfigValue(conn, "cn=config", "olcPasswordHash")).
				NotTo(BeEmpty(), "pod %s: olcPasswordHash is unset", pod)

			conn.Close()
		}
	}, NodeTimeout(8*time.Minute))

	// ── Per-database: durability and read-transaction bounds ────────────────

	It("converges the per-database durability tunables on every pod", func(ctx SpecContext) {
		for _, pod := range rwPods {
			By("inspecting the databases on " + pod)
			conn := dialPodConfigEventually(ctx, pod)

			dataDN := configDBDN(conn, baseDN)
			Expect(dataDN).NotTo(BeEmpty(), "pod %s: no data DB for %s", pod, baseDN)

			// A checkpoint is what bounds the write window noSync opens. It is
			// written unconditionally so that turning noSync on later is a
			// one-field change that is already safe (findings 6 and 7).
			ckpt := singleConfigValue(conn, dataDN, "olcDbCheckpoint")
			Expect(ckpt).To(MatchRegexp(`^\d+ \d+$`),
				"pod %s: data DB olcDbCheckpoint = %q, want \"<kbyte> <min>\"", pod, ckpt)

			// noSync is converged, not create-only: the attribute must be
			// present with an explicit value, which is what proves the operator
			// writes it rather than leaving whatever creation happened to set.
			Expect(strings.ToUpper(singleConfigValue(conn, dataDN, "olcDbNoSync"))).
				To(BeElementOf("TRUE", "FALSE"),
					"pod %s: data DB olcDbNoSync must be explicitly set", pod)

			// A long read transaction — a syncrepl full refresh, a bulk export —
			// pins free pages and grows the map (finding 9).
			Expect(atoi(singleConfigValue(conn, dataDN, "olcDbRtxnSize"))).
				To(BeNumerically(">", 0), "pod %s: data DB olcDbRtxnSize", pod)

			// The journal is written on the same hot path as the data, so it
			// carries the same posture on its own, longer interval.
			logDN := configDBDN(conn, accesslogSuffixFor(dbCRName))
			if logDN != "" {
				logCkpt := singleConfigValue(conn, logDN, "olcDbCheckpoint")
				Expect(logCkpt).To(MatchRegexp(`^\d+ \d+$`),
					"pod %s: accesslog olcDbCheckpoint = %q", pod, logCkpt)
			}

			conn.Close()
		}
	}, NodeTimeout(8*time.Minute))

	// ── Syncrepl stanza hardening (finding 11) ──────────────────────────────

	It("hardens every syncrepl stanza with timeouts and a keepalive", func(ctx SpecContext) {
		for _, pod := range rwPods {
			conn := dialPodConfigEventually(ctx, pod)
			dataDN := configDBDN(conn, baseDN)
			Expect(dataDN).NotTo(BeEmpty())

			stanzas := ldapSearchAttr(conn, dataDN, "olcSyncRepl")
			if len(stanzas) == 0 {
				conn.Close()
				Skip("this cluster has no syncrepl stanzas (single replica, no external peers)")
			}
			for _, s := range stanzas {
				Expect(s).To(ContainSubstring("network-timeout="),
					"pod %s: a stanza with no network-timeout notices a dead provider "+
						"only when TCP gives up:\n%s", pod, s)
				Expect(s).To(ContainSubstring("keepalive="),
					"pod %s: a refreshAndPersist connection is idle by design between "+
						"writes; without keepalive a silently dropped flow leaves a "+
						"consumer that has stopped consuming and still reports Synced:\n%s",
					pod, s)
				// timelimit= is a server-side search time limit and WOULD abort
				// the persistent search. timeout= (LDAP_OPT_TIMEOUT) is the safe
				// one and is what we emit.
				Expect(s).NotTo(ContainSubstring("timelimit="),
					"pod %s: timelimit= would kill the persistent search:\n%s", pod, s)
			}
			conn.Close()
		}
	}, NodeTimeout(8*time.Minute))

	// ── cn=monitor (finding 13) ─────────────────────────────────────────────

	It("runs the monitor backend on every pod, readable only by the replication identity", func(ctx SpecContext) {
		replPW := replicationPassword(ctx)

		for _, pod := range pods {
			By("checking the monitor database on " + pod)
			conn := dialPodConfigEventually(ctx, pod)
			res, err := conn.Search(ldap.NewSearchRequest(
				"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
				0, 0, false, "(objectClass=olcMonitorConfig)", []string{"dn"}, nil))
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Entries).To(HaveLen(1),
				"pod %s: no monitor database. Monitoring today is the operator's CSN "+
					"polling, which by ADR-008's own amendment cannot see an "+
					"idle-but-broken link", pod)
			conn.Close()

			// The identity that reads it is the EXISTING replication identity,
			// not a new one (ADR-008 reuse).
			mon := dialPodAs(pod, "cn=replication,"+baseDN, replPW)
			counters, err := mon.Search(ldap.NewSearchRequest(
				"cn=Monitor", ldap.ScopeBaseObject, ldap.NeverDerefAliases,
				0, 10, false, "(objectClass=*)", []string{"monitoredInfo"}, nil))
			Expect(err).NotTo(HaveOccurred(),
				"pod %s: the replication identity must be able to read cn=monitor", pod)
			Expect(counters.Entries).To(HaveLen(1))
			mon.Close()

			// And nobody else. cn=monitor exposes bind DNs and connection peers,
			// so it is at least as restrictive as the databases it reflects —
			// ADR-020's rule applied to a different tree.
			anon := dialPodAnonymous(pod)
			_, err = anon.Search(ldap.NewSearchRequest(
				"cn=Monitor", ldap.ScopeBaseObject, ldap.NeverDerefAliases,
				0, 10, false, "(objectClass=*)", []string{"monitoredInfo"}, nil))
			Expect(err).To(HaveOccurred(),
				"pod %s: cn=monitor must not be readable anonymously — it lists the "+
					"DN of every current bind", pod)
			anon.Close()
		}
	}, NodeTimeout(8*time.Minute))
})

// ── helpers ──────────────────────────────────────────────────────────────────

// singleConfigValue reads one attribute expected to carry exactly one value,
// returning "" when the attribute is absent. Absence is a legitimate assertion
// target here — most of these findings ARE an absent attribute — so it is
// returned rather than failed on.
func singleConfigValue(conn *ldap.Conn, dn, attr string) string {
	res, err := conn.Search(ldap.NewSearchRequest(
		dn, ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		1, 0, false, "(objectClass=*)", []string{attr}, nil))
	Expect(err).NotTo(HaveOccurred(), "read %s on %s", attr, dn)
	Expect(res.Entries).To(HaveLen(1))
	vals := res.Entries[0].GetEqualFoldAttributeValues(attr)
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}

func atoi(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	Expect(err).NotTo(HaveOccurred(), "not an integer: %q", s)
	return n
}

func accesslogSuffixFor(db string) string { return "cn=accesslog-" + db }

func replicationPassword(ctx SpecContext) string {
	sec, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx,
		envOrDefault("DB_CREDENTIALS_SECRET", dbCRName+"-credentials"), metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	pw := string(sec.Data["replication-password"])
	Expect(pw).NotTo(BeEmpty())
	return pw
}

// dialPodAnonymous connects to one pod without binding. Separate from dialPodAs
// because go-ldap's simple bind with an empty password is an unauthenticated
// bind, which some servers treat differently from no bind at all.
func dialPodAnonymous(pod string) *ldap.Conn {
	addr := podNodePortAddr(pod, "E2E_POD_NODEPORT_BASE")
	Expect(addr).NotTo(BeEmpty(), "E2E_NODE_IP and E2E_POD_NODEPORT_BASE must be set")
	var conn *ldap.Conn
	Eventually(func() error {
		c, err := ldap.Dial("tcp", addr)
		if err != nil {
			return err
		}
		conn = c
		return nil
	}).WithTimeout(time.Minute).WithPolling(3 * time.Second).Should(Succeed())
	return conn
}

// effectiveLogLevel resolves what slapd is actually running at: spec.logLevel
// when set, the operator's default otherwise. The field is a pointer precisely
// so that an explicit 0 — "log nothing" — is expressible, which means a reader
// cannot treat nil and 0 as the same thing (finding 14).
//
// Kept in step with the operator's own default by TestEffectiveLogLevelDefault
// in the operator module; duplicated here rather than imported because the
// constant is unexported.
func effectiveLogLevel(sc *ldapv1alpha1.SlapdCluster) int32 {
	if sc == nil || sc.Spec.LogLevel == nil {
		return 16640
	}
	return *sc.Spec.LogLevel
}
