package e2e_test

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// External replication tests verify cross-cluster delta-syncrepl between two
// independent SlapdClusters. Gated by E2E_EXTERNAL_REPL=1.
//
// Prerequisites — both clusters must be fully deployed BEFORE running tests:
//   - Operator installed on both siteA and siteB
//   - SlapdCluster deployed on both with externalPeers pointing at each other
//   - SlapdDatabase + SlapdSchema deployed on both (data replicates automatically)
//   - Both sites' LDAP services reachable from the test machine
//
// Required env vars:
//
//	E2E_EXTERNAL_REPL=1         — enable these tests
//	E2E_REMOTE_LDAP_ADDR        — LDAP address of siteB (e.g. "10.0.1.50:30389")
//	E2E_REMOTE_ADMIN_PW         — plaintext admin password for siteB
//
// Optional env vars:
//
//	NAMESPACE_TESTING            — siteA namespace (default: slaptain-testing)
//
// The siteA cluster uses the default kubeconfig (or KUBECONFIG env var).
// See tests/README.md § "Cross-cluster replication tests" for full setup instructions.

var _ = Describe("external replication", Label("external-replication"), Ordered, func() {

	var (
		remoteAddr    string
		remoteAdminPW string
		remoteRootPW  string // siteB's OWN cn=config admin password — see BeforeEach
		replicas      int32
		extPeerRID    string // expected RID for the first external peer (ridBase + 51)
	)

	BeforeEach(func(ctx SpecContext) {
		if os.Getenv("E2E_EXTERNAL_REPL") != "1" {
			Skip("E2E_EXTERNAL_REPL not set; skipping external replication tests")
		}

		remoteAddr = os.Getenv("E2E_REMOTE_LDAP_ADDR")
		Expect(remoteAddr).NotTo(BeEmpty(), "E2E_REMOTE_LDAP_ADDR must be set")

		remoteAdminPW = os.Getenv("E2E_REMOTE_ADMIN_PW")
		if remoteAdminPW == "" {
			// Fallback: if both clusters share the same credentials secret
			// (e.g. pre-seeded via credentialsSecretName), read from the local
			// cluster's slapd-passwords.
			remoteAdminPW = adminPW
		}
		Expect(remoteAdminPW).NotTo(BeEmpty(), "E2E_REMOTE_ADMIN_PW must be set (or siteA admin password is used as fallback)")

		// The cn=config admin password is per-cluster. The database credentials
		// ARE shared across sites (e2e.sh pre-creates the same secret
		// everywhere), which is why remoteAdminPW can fall back to the local
		// one — but each SlapdCluster auto-generates its own
		// <name>-config-password, so the suite-wide rootPW is siteA's and only
		// siteA's. Anything binding cn=admin,cn=config against siteB must use
		// this. e2e.sh reads it off the remote cluster and exports it; when it
		// is empty, cross-site cn=config reads are skipped rather than attempted
		// with a password that cannot work.
		remoteRootPW = os.Getenv("E2E_REMOTE_ROOT_PW")

		sts, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, "slapd", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		replicas = 1
		if sts.Spec.Replicas != nil {
			replicas = *sts.Spec.Replicas
		}

		// Read ridBase from SlapdDatabase CR to compute expected external peer RID.
		// External peer j → RID = ridBase + 50 + j + 1 (first peer: ridBase + 51).
		sd := &ldapv1alpha1.SlapdDatabase{}
		Expect(crdClient.Get(ctx, types.NamespacedName{Name: dbCRName, Namespace: namespace}, sd)).To(Succeed())
		Expect(sd.Spec.Replication.RIDBase).NotTo(BeNil(), "test fixture must set spec.replication.ridBase")
		extPeerRID = fmt.Sprintf("%d", *sd.Spec.Replication.RIDBase+51)
	}, NodeTimeout(30*time.Second))

	// ── 1. Syncrepl stanzas applied ──────────────────────────────────────────

	It("external peer syncrepl stanzas are present in cn=config on all RW pods", func(ctx SpecContext) {
		for i := int32(0); i < replicas; i++ {
			podName := fmt.Sprintf("slapd-%d", i)
			localPort := fmt.Sprintf("%d", 14000+i)
			conn, cancel := dialPodLDAP(namespace, podName, localPort)
			defer cancel()
			defer conn.Close()

			// Re-bind as cn=admin,cn=config to read olcSyncRepl. dialPodLDAP
			// targets a LOCAL (siteA) pod, so the suite-wide rootPW — siteA's own
			// config password — is the right credential here. Remote pods need
			// remoteRootPW instead; see the note in BeforeEach.
			Expect(conn.Bind("cn=admin,cn=config", rootPW)).To(Succeed())

			sr, err := conn.Search(ldap.NewSearchRequest(
				"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
				0, 0, false, fmt.Sprintf("(olcSuffix=%s)", baseDN),
				[]string{"olcSyncRepl"}, nil))
			Expect(err).NotTo(HaveOccurred())
			Expect(sr.Entries).NotTo(BeEmpty(), "data DB entry not found on %s", podName)

			syncreplVals := sr.Entries[0].GetEqualFoldAttributeValues("olcSyncRepl")
			hasExternal := false
			for _, v := range syncreplVals {
				if containsRID(v, extPeerRID) {
					hasExternal = true
					break
				}
			}
			Expect(hasExternal).To(BeTrue(),
				"pod %s should have external peer syncrepl stanza (rid=%s), got: %v", podName, extPeerRID, syncreplVals)
		}
	}, NodeTimeout(3*time.Minute))

	// ── 1b. The remote site's cn=config is actually readable ─────────────────
	//
	// Regression guard for a diagnostic that could never authenticate: the
	// cross-site dump bound cn=admin,cn=config with the LOCAL cluster's
	// password, so it always failed with "Invalid Credentials" and never printed
	// siteB's syncrepl stanzas — precisely the state you need when cross-site
	// replication stalls. Asserting the bind here keeps the credential wiring
	// honest: if e2e.sh stops exporting E2E_REMOTE_ROOT_PW, or exports the wrong
	// site's, this spec fails instead of a diagnostic silently going blank.

	It("can read the remote site's cn=config with the remote config password", func(ctx SpecContext) {
		Expect(remoteRootPW).NotTo(BeEmpty(),
			"E2E_REMOTE_ROOT_PW must be exported for multi-site runs — without it the "+
				"cross-site diagnostics cannot read siteB's syncrepl stanzas")
		Expect(remoteRootPW).NotTo(Equal(rootPW),
			"E2E_REMOTE_ROOT_PW looks like the LOCAL config password; each SlapdCluster "+
				"auto-generates its own, so this would be the same bug in a new disguise")

		// Dial and bind with explicit timeouts, and retry. A bare
		// ldap.DialURL + Bind has no deadline at all: a NodePort connection can
		// establish against a backend that is gone (stale conntrack, a pod
		// replaced moments earlier by the resilience specs) and then wait for a
		// bind response forever. That is exactly what happened on the first run
		// of this spec — the goroutine sat in readPacket for the full 90 s node
		// timeout. Every other cross-pod path in the suite retries for the same
		// reason (retryConnectLDAP); this one must too.
		var stanzas []string
		Eventually(ctx, func() error {
			c, err := ldap.DialURL("ldap://"+remoteAddr, ldap.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}))
			if err != nil {
				return fmt.Errorf("dial %s: %w", remoteAddr, err)
			}
			defer c.Close()
			c.SetTimeout(10 * time.Second)

			if err := c.Bind("cn=admin,cn=config", remoteRootPW); err != nil {
				return fmt.Errorf("bind cn=admin,cn=config on %s: %w", remoteAddr, err)
			}
			sr, err := c.Search(ldap.NewSearchRequest(
				"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
				0, 0, false, fmt.Sprintf("(olcSuffix=%s)", baseDN),
				[]string{"olcSyncRepl"}, nil))
			if err != nil {
				return fmt.Errorf("search cn=config on %s: %w", remoteAddr, err)
			}
			if len(sr.Entries) == 0 {
				return fmt.Errorf("remote data DB entry not found in cn=config")
			}
			stanzas = sr.Entries[0].GetEqualFoldAttributeValues("olcSyncRepl")
			return nil
		}).WithTimeout(90*time.Second).WithPolling(5*time.Second).Should(Succeed(),
			"the remote site's cn=config must be readable with E2E_REMOTE_ROOT_PW")
		Expect(stanzas).NotTo(BeEmpty(),
			"the remote site should have syncrepl stanzas for the data DB")

		// Prove the fixed diagnostic actually renders them — retried, because it
		// opens its own connections and a single unlucky cross-site dial made
		// this assertion fail on an otherwise-healthy mesh ("bind error:
		// connection timed out"). What is being asserted is the credential
		// wiring, not the reachability of one TCP connection.
		Eventually(ctx, func() error {
			dump := dumpReplDiagnostics(remoteAddr, remoteAdminPW, remoteRootPW)
			if !strings.Contains(dump, "olcSyncRepl:") {
				return fmt.Errorf("diagnostic did not dump the remote syncrepl stanzas:\n%s", dump)
			}
			if strings.Contains(dump, "config bind error") {
				return fmt.Errorf("diagnostic failed its config bind:\n%s", dump)
			}
			return nil
		}).WithTimeout(90*time.Second).WithPolling(5*time.Second).Should(Succeed(),
			"the cross-site diagnostic must render the remote site's syncrepl stanzas")
	}, NodeTimeout(4*time.Minute))

	// ── 2. Write on siteA propagates to siteB ────────────────────────────────

	It("a write on siteA propagates to siteB", func(ctx SpecContext) {
		uid := fmt.Sprintf("ext-repl-a2b-%d", GinkgoRandomSeed())
		dn := addReplTestUser(ldapConn, uid, 65600)
		defer ldapConn.Del(ldap.NewDelRequest(dn, nil)) //nolint:errcheck

		remoteConn := connectLDAP(remoteAddr, baseDN, remoteAdminPW)
		defer remoteConn.Close()

		GinkgoLogr.Info("wrote entry on siteA, waiting for siteB", "dn", dn)
		Eventually(ctx, func() bool {
			return ldapExists(remoteConn, dn)
		}).WithTimeout(60 * time.Second).WithPolling(3 * time.Second).Should(BeTrue(),
			"entry %s should replicate from siteA to siteB within 60 s\n%s", dn,
			dumpReplDiagnostics(remoteAddr, remoteAdminPW, remoteRootPW))
	}, NodeTimeout(3*time.Minute))

	// ── 3. Write on siteB propagates to siteA ────────────────────────────────

	It("a write on siteB propagates to siteA", func(ctx SpecContext) {
		remoteConn := connectLDAP(remoteAddr, baseDN, remoteAdminPW)
		defer remoteConn.Close()

		uid := fmt.Sprintf("ext-repl-b2a-%d", GinkgoRandomSeed())
		dn := addReplTestUser(remoteConn, uid, 65601)
		defer remoteConn.Del(ldap.NewDelRequest(dn, nil)) //nolint:errcheck

		GinkgoLogr.Info("wrote entry on siteB, waiting for siteA", "dn", dn)
		Eventually(ctx, func() bool {
			return ldapExists(ldapConn, dn)
		}).WithTimeout(60 * time.Second).WithPolling(3 * time.Second).Should(BeTrue(),
			"entry %s should replicate from siteB to siteA within 60 s\n%s", dn,
			dumpReplDiagnostics(localLDAPAddr, adminPW, rootPW))
	}, NodeTimeout(3*time.Minute))

	// ── 4. Removing external peer removes stanza ─────────────────────────────

	It("removing an external peer from the spec removes its syncrepl stanza", func(ctx SpecContext) {
		// Read the current SlapdCluster CR.
		sc := &ldapv1alpha1.SlapdCluster{}
		Expect(crdClient.Get(ctx, types.NamespacedName{Name: "slapd", Namespace: namespace}, sc)).To(Succeed())

		// Save original peers for restoration.
		originalPeers := sc.Spec.Replication.ExternalPeers
		Expect(originalPeers).NotTo(BeEmpty(), "test requires at least one external peer")

		// Remove all external peers.
		patch := client.MergeFrom(sc.DeepCopy())
		sc.Spec.Replication.ExternalPeers = nil
		Expect(crdClient.Patch(ctx, sc, patch)).To(Succeed())

		// Wait for the operator to remove the syncrepl stanza from all RW pods.
		// Transient bind/search errors are expected while the operator
		// reconciles cn=config (slapd's replication engine drops connections
		// when olcSyncRepl is rewritten) — fold them into "not ready yet,
		// retry" rather than letting them fail the test.
		Eventually(ctx, func() bool {
			for i := int32(0); i < replicas; i++ {
				podName := fmt.Sprintf("slapd-%d", i)
				localPort := fmt.Sprintf("%d", 14000+i)
				conn, cancel := dialPodLDAP(namespace, podName, localPort)
				defer cancel()
				defer conn.Close()

				// Local pod (dialPodLDAP), so rootPW is correct — see BeforeEach.
				if err := conn.Bind("cn=admin,cn=config", rootPW); err != nil {
					return false
				}
				sr, err := conn.Search(ldap.NewSearchRequest(
					"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
					0, 0, false, fmt.Sprintf("(olcSuffix=%s)", baseDN),
					[]string{"olcSyncRepl"}, nil))
				if err != nil {
					return false
				}
				if len(sr.Entries) == 0 {
					return false
				}
				syncreplVals := sr.Entries[0].GetEqualFoldAttributeValues("olcSyncRepl")
				for _, v := range syncreplVals {
					if containsRID(v, extPeerRID) {
						return false // stanza still present
					}
				}
			}
			return true
		}).WithTimeout(60 * time.Second).WithPolling(5 * time.Second).Should(BeTrue(),
			"external peer syncrepl stanza (rid=%s) should be removed from all RW pods", extPeerRID)

		// Restore original peers.
		Expect(crdClient.Get(ctx, types.NamespacedName{Name: "slapd", Namespace: namespace}, sc)).To(Succeed())
		patch = client.MergeFrom(sc.DeepCopy())
		sc.Spec.Replication.ExternalPeers = originalPeers
		Expect(crdClient.Patch(ctx, sc, patch)).To(Succeed())

		// Wait for the stanza to reappear on ALL RW pods. Checking only one pod
		// is insufficient: the controller reconciles pods sequentially, and pods
		// still being updated have their replication engine restarting — subsequent
		// tests that open new connections through the ClusterIP service can hit
		// those unsettled pods and get transient auth failures.
		// Same transient-error tolerance as the removal Eventually above.
		Eventually(ctx, func() bool {
			for i := int32(0); i < replicas; i++ {
				podName := fmt.Sprintf("slapd-%d", i)
				localPort := fmt.Sprintf("%d", 14000+i)
				conn, cancel := dialPodLDAP(namespace, podName, localPort)
				defer cancel()
				defer conn.Close()

				// Local pod (dialPodLDAP), so rootPW is correct — see BeforeEach.
				if err := conn.Bind("cn=admin,cn=config", rootPW); err != nil {
					return false
				}
				sr, err := conn.Search(ldap.NewSearchRequest(
					"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
					0, 0, false, fmt.Sprintf("(olcSuffix=%s)", baseDN),
					[]string{"olcSyncRepl"}, nil))
				if err != nil {
					return false
				}
				if len(sr.Entries) == 0 {
					return false
				}
				found := false
				for _, v := range sr.Entries[0].GetEqualFoldAttributeValues("olcSyncRepl") {
					if containsRID(v, extPeerRID) {
						found = true
						break
					}
				}
				if !found {
					return false
				}
			}
			return true
		}).WithTimeout(150 * time.Second).WithPolling(5 * time.Second).Should(BeTrue(),
			"external peer syncrepl stanza (rid=%s) should be restored on all RW pods after re-adding peer", extPeerRID)
		// 150 s, not 60: re-adding the peer reconciles immediately, but the
		// stanza needs the peer's addresses, and address discovery rides the
		// 60 s CSN-monitoring tick (ADR-016 amendment 2026-08-26; the M8
		// negative result). Worst case is a full tick plus the follow-up
		// SlapdDatabase reconcile on every pod — a 60 s budget fails on tick
		// phase alone, which is exactly how this spec flaked.
	}, NodeTimeout(4*time.Minute))

	// ── 5. External peer status reported in CR ───────────────────────────────

	It("external peer status is reported in the SlapdCluster status", func(ctx SpecContext) {
		sc := &ldapv1alpha1.SlapdCluster{}
		Expect(crdClient.Get(ctx, types.NamespacedName{Name: "slapd", Namespace: namespace}, sc)).To(Succeed())

		// Classify peers by mode.
		var hasURIPeers, hasDiscoveryPeers, hasPodAddrPeers bool
		for _, ep := range sc.Spec.Replication.ExternalPeers {
			if ep.Discovery != nil {
				hasDiscoveryPeers = true
			} else if len(ep.PodAddresses) > 0 {
				hasPodAddrPeers = true
			} else if ep.URI != "" {
				hasURIPeers = true
			}
		}

		if hasURIPeers {
			Expect(sc.Status.ExternalPeerStatuses).NotTo(BeEmpty(),
				"status.externalPeerStatuses should be populated for URI-based peers")
			for _, ps := range sc.Status.ExternalPeerStatuses {
				Expect(ps.Name).NotTo(BeEmpty(), "peer status should have a name")
				Expect(ps.Connected).To(BeTrue(),
					"external peer %q should be connected (got lastError: %s)", ps.Name, ps.LastError)
			}
		} else if hasDiscoveryPeers {
			// Discovery peers appear in ExternalPeerStatuses with DiscoveredAddresses.
			Expect(sc.Status.ExternalPeerStatuses).NotTo(BeEmpty(),
				"status.externalPeerStatuses should be populated for discovery-mode peers")
			for _, ps := range sc.Status.ExternalPeerStatuses {
				Expect(ps.Name).NotTo(BeEmpty(), "peer status should have a name")
				Expect(ps.DiscoveredAddresses).NotTo(BeEmpty(),
					"discovery peer %q should have discovered addresses", ps.Name)
			}
		} else if hasPodAddrPeers {
			// Static podAddresses peers are omitted from ExternalPeerStatuses —
			// the operator can't reach the replication network.
			Expect(sc.Status.ExternalPeerStatuses).To(BeEmpty(),
				"podAddresses peers should not appear in externalPeerStatuses")
		}
	}, NodeTimeout(30*time.Second))
})

// containsRID checks if an olcSyncRepl value contains the given RID.
// Handles the {N} ordering prefix that OpenLDAP adds internally.
func containsRID(stanza, rid string) bool {
	s := stanza
	// Strip {N} prefix if present.
	if len(s) > 0 && s[0] == '{' {
		if idx := strings.Index(s, "}"); idx >= 0 {
			s = s[idx+1:]
		}
	}
	s = strings.TrimSpace(s)
	prefix := "rid=" + rid
	return strings.HasPrefix(s, prefix) && (len(s) == len(prefix) || s[len(prefix)] == ' ')
}

// dumpReplDiagnostics connects to a site's LDAP and returns a diagnostic string
// with contextCSN and syncrepl stanzas. Called in assertion messages on failure
// so the output appears in the test log.
//
// configPassword is that SITE's cn=config admin password, passed explicitly
// because it is per-cluster: this used to read the suite-wide rootPW, which is
// siteA's, so every remote invocation printed
// `config bind error: LDAP Result Code 49 "Invalid Credentials"` and the syncrepl
// stanzas — the single most useful thing to know when cross-site replication
// stalls — were never dumped. Pass "" to skip the cn=config section explicitly.
func dumpReplDiagnostics(addr, adminPassword, configPassword string) string {
	var b strings.Builder
	b.WriteString("--- replication diagnostics for " + addr + " ---\n")

	// Deadlines on every diagnostic dial: this runs inside a failure message,
	// often right after pods were replaced, and a diagnostic that blocks turns a
	// clear assertion failure into an opaque node timeout.
	conn, err := ldap.DialURL("ldap://"+addr, ldap.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}))
	if err != nil {
		fmt.Fprintf(&b, "dial error: %v\n", err)
		return b.String()
	}
	defer conn.Close()
	conn.SetTimeout(10 * time.Second)

	// contextCSN from the data DB (anonymous read of rootDSE-like operational attrs).
	if err := conn.Bind(fmt.Sprintf("cn=admin,%s", baseDN), adminPassword); err != nil {
		fmt.Fprintf(&b, "bind error: %v\n", err)
		return b.String()
	}

	sr, err := conn.Search(ldap.NewSearchRequest(
		baseDN, ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		0, 0, false, "(objectClass=*)",
		[]string{"contextCSN"}, nil))
	if err != nil {
		fmt.Fprintf(&b, "contextCSN search error: %v\n", err)
	} else if len(sr.Entries) > 0 {
		csns := sr.Entries[0].GetEqualFoldAttributeValues("contextCSN")
		fmt.Fprintf(&b, "contextCSN: %v\n", csns)
	}

	// syncrepl stanzas from cn=config (need config admin).
	if configPassword == "" {
		b.WriteString("cn=config not dumped: no config admin password for this site " +
			"(multi-site runs export E2E_REMOTE_ROOT_PW; single-site uses rootPW)\n")
		return b.String()
	}
	cfgConn, err := ldap.DialURL("ldap://"+addr, ldap.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}))
	if err != nil {
		fmt.Fprintf(&b, "config dial error: %v\n", err)
		return b.String()
	}
	defer cfgConn.Close()
	cfgConn.SetTimeout(10 * time.Second)

	if err := cfgConn.Bind("cn=admin,cn=config", configPassword); err != nil {
		fmt.Fprintf(&b, "config bind error: %v\n", err)
		return b.String()
	}

	sr, err = cfgConn.Search(ldap.NewSearchRequest(
		"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
		0, 0, false, fmt.Sprintf("(olcSuffix=%s)", baseDN),
		[]string{"olcSyncRepl"}, nil))
	if err != nil {
		fmt.Fprintf(&b, "syncrepl search error: %v\n", err)
	} else if len(sr.Entries) > 0 {
		for _, v := range sr.Entries[0].GetEqualFoldAttributeValues("olcSyncRepl") {
			fmt.Fprintf(&b, "olcSyncRepl: %s\n", v)
		}
	} else {
		b.WriteString("no data DB entry found in cn=config\n")
	}

	return b.String()
}

// ── Every replicated database converges cross-site ────────────────────────────
//
// The primary-database specs above cannot see a second database's breakage,
// and nothing else can either: per ADR-008 an idle broken link reads Synced
// (lag is only observable while writes flow), and the peer CSN check computes
// its verdict from whichever database's query succeeds — a failing database is
// silently skipped. That combination let example-db2's cross-site replication
// stay broken from the day the fixture gained a second database (2026-08-25)
// until 2026-09-13: the cluster-level ExternalPeer.bindDN bound every
// database's external stanzas as the FIRST database's replication identity,
// which ADR-020's accesslog ACL rightly denies on the second database's log
// (err=32 on the logbase search → the consumer halts and retries forever),
// and the second database's credentials Secret was never pre-created shared,
// so each site minted its own password for an entry that lives INSIDE the
// replicated DIT. See docs/reconcile-loop-fixes.md 2026-09-13.
//
// Red observed 2026-09-13 against the pre-fix lab (three-site mesh): a db1
// control marker replicated siteA→siteB within 90 s while the db2 marker never
// arrived (rid=251/252 looping `SYNC RESULT err=32 … rc -101 retrying`), and a
// remote bind as db2's replication identity failed with err=49.
//
// The primary database stays in the iteration deliberately: it is the built-in
// positive control. If it goes red too, the failure is not database-specific.

var _ = Describe("external replication of every database", Label("external-replication"), Ordered, func() {

	type crossSiteDB struct {
		name    string
		suffix  string
		adminPW string
		replPW  string
	}

	var (
		remoteAddr string
		dbs        []crossSiteDB
	)

	BeforeAll(func(ctx SpecContext) {
		if os.Getenv("E2E_EXTERNAL_REPL") != "1" {
			Skip("E2E_EXTERNAL_REPL not set; skipping external replication tests")
		}
		remoteAddr = os.Getenv("E2E_REMOTE_LDAP_ADDR")
		Expect(remoteAddr).NotTo(BeEmpty(), "E2E_REMOTE_LDAP_ADDR must be set")

		db2Name := os.Getenv("DB2_CR_NAME")
		if db2Name == "" {
			Skip("DB2_CR_NAME not set — the resource set declares a single SlapdDatabase; " +
				"the primary-database specs above already cover cross-site convergence")
		}

		// Primary database: suffix and admin password are suite-wide; the
		// replication password comes from its (shared, pre-created) Secret.
		sec1Name := envOrDefault("DB_CREDENTIALS_SECRET", dbCRName+"-credentials")
		sec1, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx, sec1Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(sec1.Data["replication-password"]).NotTo(BeEmpty())
		dbs = append(dbs, crossSiteDB{
			name:    dbCRName,
			suffix:  baseDN,
			adminPW: adminPW,
			replPW:  string(sec1.Data["replication-password"]),
		})

		// Second database: suffix from its CR, credentials from its Secret.
		db2 := &ldapv1alpha1.SlapdDatabase{}
		Expect(crdClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: db2Name}, db2)).To(Succeed())
		Expect(db2.Spec.Suffix).NotTo(BeEmpty())
		sec2Name := envOrDefault("DB2_CREDENTIALS_SECRET", db2Name+"-credentials")
		sec2, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx, sec2Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(sec2.Data["root-password"]).NotTo(BeEmpty())
		Expect(sec2.Data["replication-password"]).NotTo(BeEmpty())
		dbs = append(dbs, crossSiteDB{
			name:    db2Name,
			suffix:  db2.Spec.Suffix,
			adminPW: string(sec2.Data["root-password"]),
			replPW:  string(sec2.Data["replication-password"]),
		})
	}, NodeTimeout(60*time.Second))

	// (a) Marker propagation, both directions. A fresh mesh replicates each
	// database's seed via the initial refresh even over a broken delta link, so
	// only a write made AFTER convergence proves the delta path — a
	// convergence-of-seed assertion would stay green against a halted journal.
	It("a write to each database on siteA appears on siteB", func(ctx SpecContext) {
		for _, db := range dbs {
			localConn := connectLDAP(localLDAPAddr, db.suffix, db.adminPW)
			defer localConn.Close()
			remoteConn := connectLDAP(remoteAddr, db.suffix, db.adminPW)
			defer remoteConn.Close()

			uid := fmt.Sprintf("xsite-a2b-%s-%d", db.name, GinkgoRandomSeed())
			dn := addPersonEntry(localConn, db.suffix, uid)
			defer localConn.Del(ldap.NewDelRequest(dn, nil)) //nolint:errcheck

			GinkgoLogr.Info("wrote cross-site marker on siteA", "db", db.name, "dn", dn)
			Eventually(ctx, func() bool {
				return ldapExists(remoteConn, dn)
			}).WithTimeout(90*time.Second).WithPolling(3*time.Second).Should(BeTrue(),
				"database %s: entry %s should replicate from siteA to siteB within 90 s — "+
					"a red here with the primary database green means THIS database's "+
					"cross-site syncrepl is down (check its consumers for err=32/err=49 loops)",
				db.name, dn)
		}
	}, NodeTimeout(5*time.Minute))

	It("a write to each database on siteB appears on siteA", func(ctx SpecContext) {
		for _, db := range dbs {
			localConn := connectLDAP(localLDAPAddr, db.suffix, db.adminPW)
			defer localConn.Close()
			remoteConn := connectLDAP(remoteAddr, db.suffix, db.adminPW)
			defer remoteConn.Close()

			uid := fmt.Sprintf("xsite-b2a-%s-%d", db.name, GinkgoRandomSeed())
			dn := addPersonEntry(remoteConn, db.suffix, uid)
			defer remoteConn.Del(ldap.NewDelRequest(dn, nil)) //nolint:errcheck

			GinkgoLogr.Info("wrote cross-site marker on siteB", "db", db.name, "dn", dn)
			Eventually(ctx, func() bool {
				return ldapExists(localConn, dn)
			}).WithTimeout(90*time.Second).WithPolling(3*time.Second).Should(BeTrue(),
				"database %s: entry %s should replicate from siteB to siteA within 90 s",
				db.name, dn)
		}
	}, NodeTimeout(5*time.Minute))

	// (b) The whole credential chain, per database: the remote site's
	// cn=replication,<suffix> entry exists, carries a userPassword, and that
	// password matches this site's Secret. Catches what (a) cannot: markers
	// carry no userPassword, so a mesh whose replication entry lost its
	// password to an ACL strip (the 2026-04-17 class, re-reached through a
	// wrong bind identity) still propagates markers while every future
	// consumer bind is doomed.
	It("each database's replication identity can bind on the remote site", func(ctx SpecContext) {
		for _, db := range dbs {
			bindDN := fmt.Sprintf("cn=replication,%s", db.suffix)
			Eventually(ctx, func() error {
				c, err := ldap.DialURL("ldap://"+remoteAddr, ldap.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}))
				if err != nil {
					return fmt.Errorf("dial %s: %w", remoteAddr, err)
				}
				defer c.Close()
				c.SetTimeout(10 * time.Second)
				if err := c.Bind(bindDN, db.replPW); err != nil {
					return fmt.Errorf("bind %s on %s: %w", bindDN, remoteAddr, err)
				}
				return nil
			}).WithTimeout(90*time.Second).WithPolling(5*time.Second).Should(Succeed(),
				"database %s: the shared replication password must bind as %s on the remote "+
					"site — err=49 here means the sites do not share this database's "+
					"credentials Secret, or the replicated entry diverged from it",
				db.name, bindDN)
		}
	}, NodeTimeout(5*time.Minute))
})
