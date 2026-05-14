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

			// Re-bind as cn=admin,cn=config to read olcSyncRepl.
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
			dumpReplDiagnostics(remoteAddr, remoteAdminPW))
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
			dumpReplDiagnostics(localLDAPAddr, adminPW))
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
		Eventually(ctx, func() bool {
			for i := int32(0); i < replicas; i++ {
				podName := fmt.Sprintf("slapd-%d", i)
				localPort := fmt.Sprintf("%d", 14000+i)
				conn, cancel := dialPodLDAP(namespace, podName, localPort)
				defer cancel()
				defer conn.Close()

				Expect(conn.Bind("cn=admin,cn=config", rootPW)).To(Succeed())
				sr, err := conn.Search(ldap.NewSearchRequest(
					"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
					0, 0, false, fmt.Sprintf("(olcSuffix=%s)", baseDN),
					[]string{"olcSyncRepl"}, nil))
				Expect(err).NotTo(HaveOccurred())
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
		Eventually(ctx, func() bool {
			for i := int32(0); i < replicas; i++ {
				podName := fmt.Sprintf("slapd-%d", i)
				localPort := fmt.Sprintf("%d", 14000+i)
				conn, cancel := dialPodLDAP(namespace, podName, localPort)
				defer cancel()
				defer conn.Close()

				Expect(conn.Bind("cn=admin,cn=config", rootPW)).To(Succeed())
				sr, err := conn.Search(ldap.NewSearchRequest(
					"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
					0, 0, false, fmt.Sprintf("(olcSuffix=%s)", baseDN),
					[]string{"olcSyncRepl"}, nil))
				Expect(err).NotTo(HaveOccurred())
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
		}).WithTimeout(60 * time.Second).WithPolling(5 * time.Second).Should(BeTrue(),
			"external peer syncrepl stanza (rid=%s) should be restored on all RW pods after re-adding peer", extPeerRID)
	}, NodeTimeout(3*time.Minute))

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
func dumpReplDiagnostics(addr, adminPassword string) string {
	var b strings.Builder
	b.WriteString("--- replication diagnostics for " + addr + " ---\n")

	conn, err := ldap.DialURL("ldap://" + addr)
	if err != nil {
		fmt.Fprintf(&b, "dial error: %v\n", err)
		return b.String()
	}
	defer conn.Close()

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
	cfgConn, err := ldap.DialURL("ldap://" + addr)
	if err != nil {
		fmt.Fprintf(&b, "config dial error: %v\n", err)
		return b.String()
	}
	defer cfgConn.Close()

	if err := cfgConn.Bind("cn=admin,cn=config", rootPW); err != nil {
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
