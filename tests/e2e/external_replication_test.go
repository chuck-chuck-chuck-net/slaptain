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
)

// External replication tests verify cross-cluster delta-syncrepl between two
// independent SlapdClusters. Gated by E2E_EXTERNAL_REPL=1.
//
// Prerequisites — both clusters must be fully deployed BEFORE running tests:
//   - Operator installed on both siteA and siteB
//   - SlapdCluster deployed on both with externalPeers pointing at each other
//   - slapd-test deployed on siteA (data replicates to siteB automatically)
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

var _ = Describe("external replication", Label("external-replication"), func() {

	var (
		remoteAddr    string
		remoteAdminPW string
		replicas      int32
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

			syncreplVals := sr.Entries[0].GetAttributeValues("olcSyncRepl")
			hasExternal := false
			for _, v := range syncreplVals {
				if containsRID(v, "101") {
					hasExternal = true
					break
				}
			}
			Expect(hasExternal).To(BeTrue(),
				"pod %s should have external peer syncrepl stanza (rid=101), got: %v", podName, syncreplVals)
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
			"entry %s should replicate from siteA to siteB within 60 s", dn)
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
			"entry %s should replicate from siteB to siteA within 60 s", dn)
	}, NodeTimeout(3*time.Minute))

	// ── 4. Removing external peer removes stanza ─────────────────────────────

	It("removing an external peer from the spec removes its syncrepl stanza", func(ctx SpecContext) {
		Skip("requires live SlapdCluster CR modification; implement when test infrastructure supports typed client")
	})

	// ── 5. External peer status reported in CR ───────────────────────────────

	It("external peer status is reported in the SlapdCluster status", func(ctx SpecContext) {
		Skip("requires CRD client; implement when test infrastructure supports typed client")
	})
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
