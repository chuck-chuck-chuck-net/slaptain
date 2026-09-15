package e2e_test

import (
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// ADR-027 milestone 2 — the cutover, observed on a live cluster.
//
// Milestone 1 put a node-local identity on every pod and nothing bound as it.
// This milestone points the replication path at it. Four things have to hold at
// once, and only the live cluster can show all four:
//
//	stanzas  — every olcSyncRepl on every pod (RW and RO) names
//	           cn=repl-<db>,cn=slaptain-auth
//	grants   — the data DB's olcAccess and olcLimits, and the journal's
//	           olcAccess, name BOTH identities. The legacy one is not leftovers:
//	           it is what keeps a not-yet-upgraded peer replicating, and this
//	           spec fails if someone removes it early (ADR-027 migration step 4)
//	binds    — the identity actually authenticates on every pod, with the
//	           Secret's password
//	traffic  — a write on one pod reaches another. A stanza that names a
//	           plausible DN and a mesh that still replicates are different
//	           claims; only the second one matters
//
// The olcLimits half is not decoration either. An identity granted read without
// a matching limits exemption caps at slapd's default 500 entries — the
// ADR-020 2026-09-12 defect, invisible in a fixture-sized directory, which is
// why it is asserted structurally here rather than left to scale_test.
//
// Ungated: it needs nothing beyond the standard replicated fixture.
var _ = Describe("node-local replication identity", Label("auth-identity"), Ordered, func() {

	var (
		rwPods     []string
		roPods     []string
		dataSuffix string
		identityDN string
		replPW     string
		credSecret string
	)

	BeforeAll(func(ctx SpecContext) {
		sc := &ldapv1alpha1.SlapdCluster{}
		Expect(crdClient.Get(ctx, nsName(namespace, "slapd"), sc)).To(Succeed())
		if !sc.Spec.Replication.Enabled || sc.IsConsumerOnly() {
			Skip("cluster is not a syncrepl provider, so it writes no stanza to cut over")
		}

		sd := &ldapv1alpha1.SlapdDatabase{}
		Expect(crdClient.Get(ctx, nsName(namespace, dbCRName), sd)).To(Succeed())
		dataSuffix = sd.Spec.Suffix
		Expect(dataSuffix).NotTo(BeEmpty())
		identityDN = ldapv1alpha1.AuthIdentityDN(sd.Name)

		credSecret = sd.Name + "-credentials"
		if sd.Spec.Credentials.SecretName != "" {
			credSecret = sd.Spec.Credentials.SecretName
		}
		sec, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx, credSecret, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "reading %s", credSecret)
		replPW = string(sec.Data["replication-password"])
		Expect(replPW).NotTo(BeEmpty(), "%s must carry a replication-password", credSecret)

		Eventually(ctx, func() bool {
			return slapdDatabaseRunning(crdClient, namespace, dbCRName)
		}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(BeTrue(),
			"SlapdDatabase %s never reached Running", dbCRName)

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
		roPods = nil
		for i := int32(0); i < sc.Spec.ReadReplicas; i++ {
			roPods = append(roPods, fmt.Sprintf("slapd-readonly-%d", i))
		}
	}, NodeTimeout(7*time.Minute))

	It("points every syncrepl stanza at the node-local identity", func(ctx SpecContext) {
		want := fmt.Sprintf(`binddn="%s"`, identityDN)
		for _, pod := range append(append([]string{}, rwPods...), roPods...) {
			By("reading olcSyncRepl on " + pod)
			conn := dialPodConfigEventually(ctx, pod)
			dataDB, ok := findMdb(observedMdbDatabases(conn), dataSuffix)
			Expect(ok).To(BeTrue(), "pod %s has no data database %q", pod, dataSuffix)
			stanzas := configAttrValues(conn, dataDB.dn, "olcSyncrepl")
			conn.Close()

			Expect(stanzas).NotTo(BeEmpty(),
				"pod %s: %s carries no syncrepl stanza at all", pod, dataDB.dn)
			for _, s := range stanzas {
				Expect(s).To(ContainSubstring(want),
					"pod %s: stanza must bind as the node-local identity (ADR-027):\n  %s",
					pod, redactCredentials(s))
				Expect(s).NotTo(ContainSubstring(`binddn="cn=replication,`),
					"pod %s: stanza still binds as the replicated-tree identity:\n  %s",
					pod, redactCredentials(s))
			}
		}
	}, NodeTimeout(5*time.Minute))

	It("grants and exempts BOTH identities on the data database", func(ctx SpecContext) {
		legacyDN := "cn=replication," + dataSuffix
		for _, pod := range rwPods {
			By("reading olcAccess and olcLimits on " + pod)
			conn := dialPodConfigEventually(ctx, pod)
			dataDB, ok := findMdb(observedMdbDatabases(conn), dataSuffix)
			Expect(ok).To(BeTrue(), "pod %s has no data database %q", pod, dataSuffix)
			acls := strings.Join(configAttrValues(conn, dataDB.dn, "olcAccess"), "\n")
			limits := configAttrValues(conn, dataDB.dn, "olcLimits")
			conn.Close()

			Expect(acls).To(ContainSubstring(fmt.Sprintf(`dn.exact="%s" read`, identityDN)),
				"pod %s: the data DB must grant the node-local identity read (ADR-027)", pod)
			// The additivity contract. Removing this grant is migration step 4
			// and a separate release; doing it early kills every consumer in the
			// mesh that has not upgraded yet.
			Expect(acls).To(ContainSubstring(fmt.Sprintf(`dn.exact="%s" read`, legacyDN)),
				"pod %s: the legacy grant must REMAIN during the additive window (ADR-027)", pod)

			for _, dn := range []string{identityDN, legacyDN} {
				Expect(limits).To(ContainElement(MatchRegexp(
					`^(\{\d+\})?dn\.exact="`+regexp.QuoteMeta(dn)+`" .*size\.hard=unlimited`)),
					"pod %s: %s has no unlimited olcLimits — its syncrepl searches cap at "+
						"slapd's default 500 entries (ADR-020 amendment); got %v", pod, dn, limits)
			}
		}
	}, NodeTimeout(5*time.Minute))

	It("authenticates as the node-local identity on every pod", func(ctx SpecContext) {
		for _, pod := range append(append([]string{}, rwPods...), roPods...) {
			By("binding as " + identityDN + " on " + pod)
			conn := dialPodAs(pod, identityDN, replPW)
			// It can read the data tree — that is what a syncrepl search needs
			// and what the ACL grant above promises.
			res, err := conn.Search(ldap.NewSearchRequest(
				dataSuffix, ldap.ScopeBaseObject, ldap.NeverDerefAliases,
				0, 10, false, "(objectClass=*)", []string{"contextCSN"}, nil))
			conn.Close()
			Expect(err).NotTo(HaveOccurred(),
				"pod %s: %s must be able to read %s", pod, identityDN, dataSuffix)
			Expect(res.Entries).NotTo(BeEmpty())
		}
	}, NodeTimeout(5*time.Minute))

	It("still replicates a write across the mesh", func(ctx SpecContext) {
		if len(rwPods) < 2 {
			Skip("single RW pod: nothing to replicate to")
		}
		uid := fmt.Sprintf("adr027-cutover-%d", time.Now().UnixNano())
		dn := addReplTestUser(ldapConn, uid, 64027)
		DeferCleanup(func() { _ = ldapConn.Del(ldap.NewDelRequest(dn, nil)) })

		for _, pod := range rwPods[1:] {
			By("waiting for " + dn + " to appear on " + pod)
			expectEntryOnPod(ctx, pod, dn)
		}
	}, NodeTimeout(5*time.Minute))

	// The capability the whole ADR exists for. The old placement could not
	// rotate at all: the entry lived in the replicated tree, a corrective write
	// travelled by the very syncrepl link a wrong password had broken, and
	// ensureReplicationUser never compared the password anyway. Now the Secret
	// is the source of truth and the entry is a node-local projection, so
	// changing the Secret must converge every pod — and replication must
	// survive it, because the stanzas carry the same value.
	//
	// Destructive by nature, so it runs last and restores the original password
	// on the way out. It only ever touches this suite's own fixture Secret.
	It("rotates the replication password through the Secret", func(ctx SpecContext) {
		newPW := fmt.Sprintf("rotated-%d", time.Now().UnixNano())

		By("patching replication-password in " + credSecret)
		sec, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx, credSecret, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		original := string(sec.Data["replication-password"])
		setReplPassword(ctx, credSecret, newPW)
		DeferCleanup(func(ctx SpecContext) {
			setReplPassword(ctx, credSecret, original)
			// Leave the fixture converged for whatever runs next.
			for _, pod := range append(append([]string{}, rwPods...), roPods...) {
				expectBindEventually(ctx, pod, identityDN, original)
			}
			// And repair the LEGACY entry by hand.
			//
			// The round trip above should leave it untouched — it is create-only,
			// so it still holds whatever it was created with, which is `original`.
			// This is a belt: if a rotation ever half-completes, or someone rotates
			// the fixture out of band, the legacy entry is left holding a password
			// nothing else knows, and accesslog_test.go — which binds as it — fails
			// somewhere entirely unrelated. That failure mode is the real ADR-027
			// operational hazard (the entry does NOT follow the Secret), so the
			// suite performs exactly the manual repair the ADR documents rather
			// than leaving a landmine for the next spec.
			repairLegacyReplicationEntry(dataSuffix, original)
		})

		By("waiting for the operator to converge the projection on every pod")
		for _, pod := range append(append([]string{}, rwPods...), roPods...) {
			expectBindEventually(ctx, pod, identityDN, newPW)
		}

		By("writing through the rotated mesh")
		if len(rwPods) < 2 {
			Skip("single RW pod: nothing to replicate to")
		}
		uid := fmt.Sprintf("adr027-rotated-%d", time.Now().UnixNano())
		dn := addReplTestUser(ldapConn, uid, 64027)
		DeferCleanup(func() { _ = ldapConn.Del(ldap.NewDelRequest(dn, nil)) })
		for _, pod := range rwPods[1:] {
			expectEntryOnPod(ctx, pod, dn)
		}
	}, NodeTimeout(25*time.Minute))
})

// ── Spec-local helpers ──────────────────────────────────────────────────────

// configAttrValues reads one multi-valued attribute off a cn=config entry,
// keeping slapd's {N} ordering prefixes so order-sensitive attributes can be
// asserted as they are stored.
func configAttrValues(conn *ldap.Conn, dn, attr string) []string {
	res, err := conn.Search(ldap.NewSearchRequest(
		dn, ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		1, 10, false, "(objectClass=*)", []string{attr}, nil))
	Expect(err).NotTo(HaveOccurred(), "read %s on %s", attr, dn)
	Expect(res.Entries).NotTo(BeEmpty(), "no entry at %s", dn)
	return res.Entries[0].GetEqualFoldAttributeValues(attr)
}

// setReplPassword rewrites the replication-password key of a credentials
// Secret. The operator's projection follows within a reconcile.
func setReplPassword(ctx SpecContext, secretName, password string) {
	Eventually(ctx, func() error {
		sec, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		sec.Data["replication-password"] = []byte(password)
		_, err = k8sClient.CoreV1().Secrets(namespace).Update(ctx, sec, metav1.UpdateOptions{})
		return err
	}).WithTimeout(time.Minute).WithPolling(2 * time.Second).Should(Succeed(),
		"patching %s", secretName)
}

// authConvergenceBudget bounds how long the operator may take to carry a
// changed Secret to the pods.
//
// Sized off the mechanism, not a guess. A Secret is not an object the
// SlapdDatabase controller watches, so a rotation produces no event at all: it
// is picked up by the controller's resync floor (databaseRequeueAfter, 5 min on
// a healthy database), which is therefore the worst case. The first draft of
// this spec used 4 minutes and failed its own cleanup on t3e — the Secret was
// restored at 12:57:17, the spec gave up at 13:01:17, and the operator
// converged unattended at 13:02:16, on the tick due at 13:02:14. The budget
// must exceed the floor, with room for a slow pod.
const authConvergenceBudget = 8 * time.Minute

// expectBindEventually retries a simple bind until it succeeds — the shape a
// convergence assertion needs, since the operator writes the projection on its
// own schedule.
func expectBindEventually(ctx SpecContext, pod, bindDN, password string) {
	addr := podNodePortAddr(pod, "E2E_POD_NODEPORT_BASE")
	Expect(addr).NotTo(BeEmpty(), "E2E_NODE_IP and E2E_POD_NODEPORT_BASE must be set")
	Eventually(ctx, func() error {
		c, err := ldap.DialURL("ldap://"+addr, ldap.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}))
		if err != nil {
			return err
		}
		defer c.Close()
		c.SetTimeout(10 * time.Second)
		return c.Bind(bindDN, password)
	}).WithTimeout(authConvergenceBudget).WithPolling(5*time.Second).Should(Succeed(),
		"pod %s must accept a bind as %s with the Secret's current password", pod, bindDN)
}

// expectEntryOnPod waits for one DN to be visible on one pod, read as the data
// rootDN so no ACL can hide it.
//
// The connection is made OUTSIDE the Eventually on purpose. dialPodLDAP asserts
// with the default Gomega, and a default-Gomega failure inside a retry closure
// panics straight through the envelope and aborts the spec instead of feeding
// the poll — the hazard tryDialPodConfig's comment documents, which has bitten
// this suite twice.
func expectEntryOnPod(ctx SpecContext, pod, dn string) {
	conn, cancel := dialPodLDAP(namespace, pod, "")
	defer cancel()
	defer conn.Close()
	Eventually(ctx, func() error {
		_, err := conn.Search(ldap.NewSearchRequest(
			dn, ldap.ScopeBaseObject, ldap.NeverDerefAliases,
			1, 10, false, "(objectClass=*)", []string{"dn"}, nil))
		return err
	}).WithTimeout(2*time.Minute).WithPolling(3*time.Second).Should(Succeed(),
		"%s must replicate to %s", dn, pod)
}

// repairLegacyReplicationEntry rewrites cn=replication,<suffix>'s userPassword
// as the data rootDN.
//
// This is the manual repair ADR-027 documents, not something the operator does:
// the legacy entry lives in the REPLICATED tree, and converging it there is the
// shared-state write ADR-026 R2 forbids. That is the whole reason for "do not
// rotate during the migration window" — and the reason the one spec that
// rotates carries this belt.
//
// Best-effort: it runs in a cleanup path, so a failure is reported rather than
// allowed to mask whatever the spec was actually asserting.
func repairLegacyReplicationEntry(dataSuffix, password string) {
	req := ldap.NewModifyRequest("cn=replication,"+dataSuffix, nil)
	req.Replace("userPassword", []string{password})
	if err := ldapConn.Modify(req); err != nil {
		AddReportEntry("legacy replication entry not repaired", err.Error())
	}
}
