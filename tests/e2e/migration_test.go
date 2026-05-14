package e2e_test

import (
	"context"
	"fmt"
	"os"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// Migration e2e tests verify the consumer-only → peer in-place promotion path
// against a "fake prod" SlapdCluster acting as a plain-syncrepl source.
// Driven by tests/e2e-migration.sh which sets up the two-cluster topology.
//
// Required env vars (set by e2e-migration.sh test):
//
//	E2E_MIGRATION=1                       — enables this suite
//	E2E_MIGRATION_LDAP_ADDR               — slaptain's LDAP endpoint (NodePort)
//	E2E_MIGRATION_ADMIN_PW                — slaptain's cn=admin,<suffix> password
//	E2E_MIGRATION_FAKEPROD_LDAP_ADDR      — fakeprod's LDAP endpoint (NodePort)
//	E2E_MIGRATION_FAKEPROD_ADMIN_PW       — fakeprod's cn=admin,<suffix> password
//	E2E_MIGRATION_NS_SLAPTAIN             — slaptain namespace
//	E2E_MIGRATION_NS_FAKEPROD             — fake-prod namespace
//	E2E_MIGRATION_SUFFIX                  — shared base DN (e.g. dc=example,dc=org)

// Migration specs are gated at *registration* time, not at exec time. Without
// the gate, every non-migration run would surface this Describe block as
// "SKIPPED E2E_MIGRATION not set" in the report, polluting the summary. With
// the gate, the block doesn't exist unless E2E_MIGRATION=1 — keeping
// non-migration runs free of unrelated spec noise.
var _ = func() bool {
	if os.Getenv("E2E_MIGRATION") != "1" {
		return false
	}
	Describe("migration: consumer-only → peer", Label("migration"), Ordered, func() {

		var (
			ldapAddr         string
			adminPW          string
			fakeprodLdapAddr string
			fakeprodAdminPW  string
			nsSlap           string
			suffix           string
			adminDN          string
			aliceDN          string
			migConn          *ldap.Conn // bound to slaptain (the target under test)
			srcConn          *ldap.Conn // bound to fakeprod (the source for comparison)
			preEntryUUID     string     // alice's entryUUID on slaptain before promotion
		)

		BeforeAll(func() {
			ldapAddr = os.Getenv("E2E_MIGRATION_LDAP_ADDR")
			adminPW = os.Getenv("E2E_MIGRATION_ADMIN_PW")
			fakeprodLdapAddr = os.Getenv("E2E_MIGRATION_FAKEPROD_LDAP_ADDR")
			fakeprodAdminPW = os.Getenv("E2E_MIGRATION_FAKEPROD_ADMIN_PW")
			nsSlap = os.Getenv("E2E_MIGRATION_NS_SLAPTAIN")
			suffix = os.Getenv("E2E_MIGRATION_SUFFIX")
			Expect(ldapAddr).NotTo(BeEmpty(), "E2E_MIGRATION_LDAP_ADDR required")
			Expect(adminPW).NotTo(BeEmpty(), "E2E_MIGRATION_ADMIN_PW required")
			Expect(fakeprodLdapAddr).NotTo(BeEmpty(), "E2E_MIGRATION_FAKEPROD_LDAP_ADDR required")
			Expect(fakeprodAdminPW).NotTo(BeEmpty(), "E2E_MIGRATION_FAKEPROD_ADMIN_PW required")
			Expect(nsSlap).NotTo(BeEmpty(), "E2E_MIGRATION_NS_SLAPTAIN required")
			Expect(suffix).NotTo(BeEmpty(), "E2E_MIGRATION_SUFFIX required")
			adminDN = "cn=admin," + suffix
			aliceDN = "uid=alice,ou=People," + suffix

			var err error
			migConn, err = ldap.DialURL("ldap://" + ldapAddr)
			Expect(err).NotTo(HaveOccurred(), "dial slaptain %s", ldapAddr)
			Expect(migConn.Bind(adminDN, adminPW)).To(Succeed(), "slaptain admin bind")

			srcConn, err = ldap.DialURL("ldap://" + fakeprodLdapAddr)
			Expect(err).NotTo(HaveOccurred(), "dial fakeprod %s", fakeprodLdapAddr)
			Expect(srcConn.Bind(adminDN, fakeprodAdminPW)).To(Succeed(), "fakeprod admin bind")
		})

		AfterAll(func() {
			if migConn != nil {
				migConn.Close()
			}
			if srcConn != nil {
				srcConn.Close()
			}
		})

		// ── Stage 1: consumer-only — data has synced, writes are rejected ────────

		It("reads the seeded entry replicated from fake-prod", func() {
			Eventually(func() bool {
				return ldapExists(migConn, aliceDN)
			}, 60*time.Second, 2*time.Second).Should(BeTrue(),
				"alice never reached slaptain via syncrepl from fake-prod")
		})

		It("preserves operational attributes from the source (entryUUID matches fakeprod)", func() {
			// Direct source-vs-target equality: read alice from BOTH clusters and
			// assert the entryUUIDs (and other operational attributes) are
			// identical byte-for-byte. This is the load-bearing migration
			// guarantee — that data lands on slaptain carrying the source's
			// identity, not a locally-fabricated one.
			attrs := []string{"entryUUID", "creatorsName", "createTimestamp"}
			readAlice := func(conn *ldap.Conn, label string) *ldap.Entry {
				req := ldap.NewSearchRequest(aliceDN,
					ldap.ScopeBaseObject, ldap.NeverDerefAliases,
					1, 0, false, "(objectClass=*)", attrs, nil)
				res, err := conn.Search(req)
				Expect(err).NotTo(HaveOccurred(), "search alice on %s", label)
				Expect(res.Entries).To(HaveLen(1), "alice not present on %s", label)
				return res.Entries[0]
			}

			srcAlice := readAlice(srcConn, "fakeprod")
			tgtAlice := readAlice(migConn, "slaptain")

			srcUUID := srcAlice.GetAttributeValue("entryUUID")
			tgtUUID := tgtAlice.GetAttributeValue("entryUUID")
			Expect(srcUUID).NotTo(BeEmpty(), "fakeprod alice missing entryUUID")
			Expect(tgtUUID).NotTo(BeEmpty(), "slaptain alice missing entryUUID")
			Expect(tgtUUID).To(Equal(srcUUID),
				"slaptain's entryUUID for alice (%s) differs from fakeprod's (%s) — syncrepl did NOT preserve the source's UUID",
				tgtUUID, srcUUID)

			// creatorsName and createTimestamp should also match. These are the
			// other "client cannot set" operational attributes that ldapadd-over-
			// wire would strip and a faithful syncrepl preserves.
			Expect(tgtAlice.GetAttributeValue("creatorsName")).To(Equal(
				srcAlice.GetAttributeValue("creatorsName")),
				"creatorsName differs")
			Expect(tgtAlice.GetAttributeValue("createTimestamp")).To(Equal(
				srcAlice.GetAttributeValue("createTimestamp")),
				"createTimestamp differs")

			preEntryUUID = tgtUUID
			GinkgoWriter.Printf("pre-promotion entryUUID(alice) = %s (matches fakeprod ✓)\n", preEntryUUID)
		})

		It("rejects writes while in consumer-only mode (olcReadOnly=TRUE)", func() {
			addReq := ldap.NewAddRequest("uid=will-fail,ou=People,"+suffix, nil)
			addReq.Attribute("objectClass", []string{"inetOrgPerson"})
			addReq.Attribute("cn", []string{"Should Fail"})
			addReq.Attribute("sn", []string{"Fail"})
			addReq.Attribute("uid", []string{"will-fail"})
			err := migConn.Add(addReq)
			Expect(err).To(HaveOccurred(), "write to consumer-only DB should be rejected")
			Expect(ldap.IsErrorWithCode(err, ldap.LDAPResultUnwillingToPerform)).To(BeTrue(),
				"want LDAP code 53 (unwillingToPerform), got: %v", err)
		})

		// ── Stage 2: in-place promotion to peer mode ────────────────────────────

		It("promotes to peer mode in place (no pod restart)", func() {
			ctx := context.Background()
			preUID, preStart, err := slapdPodIdentity(ctx, nsSlap, "slaptain-0")
			Expect(err).NotTo(HaveOccurred())

			// Patch the SlapdCluster's replication.mode field.
			var sc ldapv1alpha1.SlapdCluster
			Expect(crdClient.Get(ctx, types.NamespacedName{
				Name: "slaptain", Namespace: nsSlap,
			}, &sc)).To(Succeed())
			sc.Spec.Replication.Mode = "peer"
			Expect(crdClient.Update(ctx, &sc)).To(Succeed(), "patch mode=peer")

			// Wait for status.replicationMode to converge.
			Eventually(func() string {
				var fresh ldapv1alpha1.SlapdCluster
				if err := crdClient.Get(ctx, types.NamespacedName{
					Name: "slaptain", Namespace: nsSlap,
				}, &fresh); err != nil {
					return ""
				}
				return fresh.Status.ReplicationMode
			}, 90*time.Second, 2*time.Second).Should(Equal("peer"),
				"status.replicationMode never reached peer")

			// In-place check: pod UID and slapd container start time unchanged.
			postUID, postStart, err := slapdPodIdentity(ctx, nsSlap, "slaptain-0")
			Expect(err).NotTo(HaveOccurred())
			Expect(postUID).To(Equal(preUID),
				"pod UID changed — promotion was not in-place (pod was recreated)")
			Expect(postStart).To(Equal(preStart),
				"slapd container start time changed — promotion was not in-place (container restarted)")
		})

		It("accepts writes after promotion to peer", func() {
			Eventually(func() error {
				addReq := ldap.NewAddRequest("uid=post-promote,ou=People,"+suffix, nil)
				addReq.Attribute("objectClass", []string{"inetOrgPerson"})
				addReq.Attribute("cn", []string{"Post Promote"})
				addReq.Attribute("sn", []string{"Promote"})
				addReq.Attribute("uid", []string{"post-promote"})
				err := migConn.Add(addReq)
				if err == nil || ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists) {
					return nil
				}
				return err
			}, 30*time.Second, 2*time.Second).Should(Succeed(),
				"writes should be accepted post-promotion")
		})

		It("preserves pre-promotion entryUUIDs (no re-sync happened)", func() {
			// This is a different invariant from the earlier "matches fakeprod"
			// check — it asserts that the in-place promotion did NOT re-key the
			// data (e.g., by deleting+recreating the DB and re-pulling from
			// scratch). Comparing pre- and post-promotion entryUUIDs on the same
			// cluster is the direct way to prove "metadata-only transition."
			req := ldap.NewSearchRequest(aliceDN,
				ldap.ScopeBaseObject, ldap.NeverDerefAliases,
				1, 0, false, "(objectClass=*)", []string{"entryUUID"}, nil)
			res, err := migConn.Search(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Entries).To(HaveLen(1))
			postEntryUUID := res.Entries[0].GetAttributeValue("entryUUID")
			Expect(postEntryUUID).To(Equal(preEntryUUID),
				"entryUUID changed across promotion — data was re-synced rather than retained in place")
		})
	})
	return true
}()

// slapdPodIdentity returns the pod's UID and the slapd container's start
// timestamp — stable identifiers used to assert "no pod restart" across a
// promotion. A pod restart would produce a new UID; a container restart
// (without pod replacement) would produce a new StartedAt.
func slapdPodIdentity(ctx context.Context, namespace, name string) (string, metav1.Time, error) {
	pod, err := k8sClient.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", metav1.Time{}, fmt.Errorf("get pod %s/%s: %w", namespace, name, err)
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == "slapd" && cs.State.Running != nil {
			return string(pod.UID), cs.State.Running.StartedAt, nil
		}
	}
	return string(pod.UID), metav1.Time{}, fmt.Errorf("slapd container not running in %s/%s", namespace, name)
}
