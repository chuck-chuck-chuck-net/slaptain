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
//	E2E_MIGRATION=1                 — enables this suite
//	E2E_MIGRATION_LDAP_ADDR         — slaptain's LDAP endpoint (NodePort)
//	E2E_MIGRATION_ADMIN_PW          — slaptain's cn=admin,<suffix> password
//	E2E_MIGRATION_NS_SLAPTAIN       — slaptain namespace
//	E2E_MIGRATION_NS_FAKEPROD       — fake-prod namespace
//	E2E_MIGRATION_SUFFIX            — shared base DN (e.g. dc=example,dc=org)

var _ = Describe("migration: consumer-only → peer", Label("migration"), Ordered, func() {

	var (
		ldapAddr     string
		adminPW      string
		nsSlap       string
		suffix       string
		adminDN      string
		aliceDN      string
		migConn      *ldap.Conn
		preEntryUUID string
	)

	BeforeAll(func() {
		if os.Getenv("E2E_MIGRATION") != "1" {
			Skip("E2E_MIGRATION not set")
		}
		ldapAddr = os.Getenv("E2E_MIGRATION_LDAP_ADDR")
		adminPW = os.Getenv("E2E_MIGRATION_ADMIN_PW")
		nsSlap = os.Getenv("E2E_MIGRATION_NS_SLAPTAIN")
		suffix = os.Getenv("E2E_MIGRATION_SUFFIX")
		Expect(ldapAddr).NotTo(BeEmpty(), "E2E_MIGRATION_LDAP_ADDR required")
		Expect(adminPW).NotTo(BeEmpty(), "E2E_MIGRATION_ADMIN_PW required")
		Expect(nsSlap).NotTo(BeEmpty(), "E2E_MIGRATION_NS_SLAPTAIN required")
		Expect(suffix).NotTo(BeEmpty(), "E2E_MIGRATION_SUFFIX required")
		adminDN = "cn=admin," + suffix
		aliceDN = "uid=alice,ou=People," + suffix

		var err error
		migConn, err = ldap.DialURL("ldap://" + ldapAddr)
		Expect(err).NotTo(HaveOccurred(), "dial %s", ldapAddr)
		Expect(migConn.Bind(adminDN, adminPW)).To(Succeed(), "admin bind")
	})

	AfterAll(func() {
		if migConn != nil {
			migConn.Close()
		}
	})

	// ── Stage 1: consumer-only — data has synced, writes are rejected ────────

	It("reads the seeded entry replicated from fake-prod", func() {
		Eventually(func() bool {
			return ldapExists(migConn, aliceDN)
		}, 60*time.Second, 2*time.Second).Should(BeTrue(),
			"alice never reached slaptain via syncrepl from fake-prod")
	})

	It("preserves operational attributes from the source (entryUUID)", func() {
		req := ldap.NewSearchRequest(aliceDN,
			ldap.ScopeBaseObject, ldap.NeverDerefAliases,
			1, 0, false, "(objectClass=*)", []string{"entryUUID", "creatorsName"}, nil)
		res, err := migConn.Search(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Entries).To(HaveLen(1))
		preEntryUUID = res.Entries[0].GetAttributeValue("entryUUID")
		Expect(preEntryUUID).NotTo(BeEmpty(),
			"entryUUID should be preserved from fake-prod's copy (syncrepl protocol guarantee)")
		GinkgoWriter.Printf("pre-promotion entryUUID(alice) = %s\n", preEntryUUID)
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
