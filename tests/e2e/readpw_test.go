package e2e_test

import (
	"fmt"
	"math/rand"
	"strings"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// emailDomainFromDN converts a DC-notation base DN to a dotted domain name.
// "dc=chuck-chuck-chuck,dc=net" → "chuck-chuck-chuck.net"
func emailDomainFromDN(dn string) string {
	var parts []string
	for _, rdn := range strings.Split(dn, ",") {
		if after, ok := strings.CutPrefix(strings.TrimSpace(rdn), "dc="); ok {
			parts = append(parts, after)
		}
	}
	return strings.Join(parts, ".")
}

// ── Config access ─────────────────────────────────────────────────────────────

var _ = Describe("config access", func() {

	It("root DN binds to cn=config and reports correct suffix and rootDN", func() {
		conn, err := ldap.Dial("tcp", localLDAPAddr)
		Expect(err).NotTo(HaveOccurred())
		defer conn.Close()

		Expect(conn.Bind("cn=admin,cn=config", rootPW)).To(Succeed(),
			"bind as cn=admin,cn=config should succeed")

		// olcSuffix and olcRootDN live on the database entry, not on cn=config itself.
		req := ldap.NewSearchRequest("cn=config",
			ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
			0, 0, false, "(olcSuffix=*)", []string{"olcSuffix", "olcRootDN"}, nil)
		result, err := conn.Search(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Entries).NotTo(BeEmpty(),
			"cn=config subtree should contain at least one database entry with olcSuffix")

		var found bool
		for _, e := range result.Entries {
			if e.GetAttributeValue("olcSuffix") == baseDN {
				found = true
				Expect(e.GetAttributeValue("olcRootDN")).To(
					Equal(fmt.Sprintf("cn=admin,%s", baseDN)),
					"olcRootDN should be cn=admin,<baseDN>")
			}
		}
		Expect(found).To(BeTrue(),
			"expected a database entry with olcSuffix=%s in cn=config", baseDN)
	})

	It("admin can list readpw users with their stored password hashes", func() {
		if len(readpwPWs) == 0 {
			Skip("no readpw passwords configured — set bootstrap.readpwPasswords in values")
		}

		// ldapConn is bound as cn=admin,<baseDN> which is the data rootDN and
		// therefore bypasses ACLs — it can read userPassword from any entry.
		readpwBase := fmt.Sprintf("ou=%s,%s", readpwOU, baseDN)
		entries := ldapSearch(ldapConn, readpwBase, "(objectClass=posixAccount)", "uid", "userPassword")

		byUID := make(map[string]*ldap.Entry, len(entries))
		for _, e := range entries {
			byUID[e.GetAttributeValue("uid")] = e
		}
		for user := range readpwPWs {
			e, ok := byUID[user]
			Expect(ok).To(BeTrue(), "readpw user %q should be present in %s", user, readpwBase)
			Expect(e.GetAttributeValue("userPassword")).NotTo(BeEmpty(),
				"readpw user %q should have userPassword set", user)
		}
	})
})

// ── Readpw ACL enforcement ────────────────────────────────────────────────────

var _ = Describe("readpw ACL enforcement", Ordered, func() {

	// Variables shared across It specs; set in BeforeAll, used by subsequent specs.
	var (
		mailUserDN string
		mailUserPW string
	)

	BeforeAll(func() {
		if len(readpwPWs) == 0 {
			Skip("no readpw passwords configured — set bootstrap.readpwPasswords in values")
		}

		// Ensure a People user exists for the "cannot read" ACL check later.
		// ldapAdd is idempotent so this is safe even if ldap_test.go already added alice.
		addUser("alice", "Alice", "Smith", 10001, 10000, testUserPassword)

		// Build the identity for the mail user that subsequent It specs will create and use.
		// The actual ldapAdd happens in the first It so it appears as a visible test step.
		uid := fmt.Sprintf("testmail-%06d", rand.Intn(1000000))
		mailUserPW = fmt.Sprintf("mailpass-%06d", rand.Intn(1000000))
		mailUserDN = fmt.Sprintf("uid=%s,ou=Mail,%s", uid, baseDN)
	})

	AfterAll(func() {
		if mailUserDN == "" {
			return
		}
		err := ldapConn.Del(ldap.NewDelRequest(mailUserDN, nil))
		if err != nil && !ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
			Expect(err).NotTo(HaveOccurred(), "cleanup: failed to delete %s", mailUserDN)
		}
	})

	// ── User provisioning ─────────────────────────────────────────────────────

	It("creates a mail user in ou=Mail", func() {
		uid := strings.SplitN(mailUserDN, ",", 2)[0] // "uid=testmail-XXXXXX"
		uid = strings.TrimPrefix(uid, "uid=")

		req := ldap.NewAddRequest(mailUserDN, nil)
		req.Attribute("objectClass", []string{"posixAccount", "shadowAccount", "inetOrgPerson"})
		req.Attribute("cn", []string{"Test Mail"})
		req.Attribute("sn", []string{"Mail"})
		req.Attribute("uid", []string{uid})
		req.Attribute("uidNumber", []string{"65534"})
		req.Attribute("gidNumber", []string{"65534"})
		req.Attribute("homeDirectory", []string{"/dev/null"})
		req.Attribute("mail", []string{uid + "@" + emailDomainFromDN(baseDN)})
		req.Attribute("userPassword", []string{mailUserPW})
		ldapAdd(ldapConn, req)
	})

	It("the mail user can bind with their own password", func(ctx SpecContext) {
		// ACL rule {0}: to dn.subtree="ou=Mail,..." attrs=userPassword
		//   … by anonymous auth …
		// The "by anonymous auth" permission is what allows any client to verify
		// a user's password via ldap bind, even without being able to read the
		// userPassword attribute value directly.
		//
		// Use Eventually: the write went through ldapConn (pinned to one pod);
		// a fresh connection may route to a different pod and the entry may not
		// have replicated yet.  err=49 from a non-existent DN is indistinguishable
		// from a wrong password, so we retry until the entry is visible everywhere.
		Eventually(ctx, func(g Gomega) {
			conn, err := ldap.Dial("tcp", localLDAPAddr)
			g.Expect(err).NotTo(HaveOccurred())
			defer conn.Close()

			g.Expect(conn.Bind(mailUserDN, mailUserPW)).To(Succeed(),
				"mail user should be able to bind with their own password")
		}).WithTimeout(30 * time.Second).WithPolling(2 * time.Second).Should(Succeed())
	}, SpecTimeout(35*time.Second))

	// ── Readpw access ─────────────────────────────────────────────────────────

	It("each readpw user can bind", func(ctx SpecContext) {
		for user, pw := range readpwPWs {
			user, pw := user, pw // capture for closure
			dn := fmt.Sprintf("uid=%s,ou=%s,%s", user, readpwOU, baseDN)
			Eventually(ctx, func() error {
				conn, err := ldap.Dial("tcp", localLDAPAddr)
				if err != nil {
					return err
				}
				defer conn.Close()
				return conn.Bind(dn, pw)
			}).WithTimeout(30 * time.Second).WithPolling(2 * time.Second).Should(Succeed(),
				"readpw user %q should be able to bind", user)
		}
	}, SpecTimeout(60*time.Second))

	It("a readpw user can read userPassword from ou=Mail", func(ctx SpecContext) {
		// ACL rule {0}: to dn.subtree="ou=Mail,..." attrs=userPassword
		//   … by dn.children="ou=<readpwOU>,..." read …
		//
		// Use Eventually: the operator applies spec.ldap.acls to each pod
		// individually via headless service DNS on every reconcile loop.  By the
		// time the test runs, some pods may still carry the default ACLs (which
		// deny userPassword reads).  Each retry opens a fresh TCP connection to the
		// ClusterIP, which may route to a different pod; within 30 s the operator
		// will have reconciled all pods and every connection attempt will succeed.
		var user, pw string
		for u, p := range readpwPWs {
			user, pw = u, p
			break
		}

		Eventually(ctx, func(g Gomega) {
			conn, err := ldap.Dial("tcp", localLDAPAddr)
			g.Expect(err).NotTo(HaveOccurred())
			defer conn.Close()

			g.Expect(conn.Bind(fmt.Sprintf("uid=%s,ou=%s,%s", user, readpwOU, baseDN), pw)).To(Succeed())

			req := ldap.NewSearchRequest(mailUserDN,
				ldap.ScopeBaseObject, ldap.NeverDerefAliases,
				0, 0, false, "(objectClass=*)", []string{"userPassword"}, nil)
			result, err := conn.Search(req)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.Entries).To(HaveLen(1))
			g.Expect(result.Entries[0].GetAttributeValue("userPassword")).NotTo(BeEmpty(),
				"readpw user %q should be able to read userPassword from ou=Mail", user)
		}).WithTimeout(30 * time.Second).WithPolling(2 * time.Second).Should(Succeed())
	}, SpecTimeout(35*time.Second))

	It("a readpw user cannot read userPassword from ou=People", func() {
		// ACL rule {1}: to attrs=userPassword … by * none
		// For entries outside ou=Mail, the wildcard "by * none" denies read access
		// to everyone except self and the rootDN.
		var user, pw string
		for u, p := range readpwPWs {
			user, pw = u, p
			break
		}

		conn, err := ldap.Dial("tcp", localLDAPAddr)
		Expect(err).NotTo(HaveOccurred())
		defer conn.Close()

		Expect(conn.Bind(fmt.Sprintf("uid=%s,ou=%s,%s", user, readpwOU, baseDN), pw)).To(Succeed())

		// Eventually: this fresh dial landed via the LB on whatever pod kube-proxy
		// picked, which may not be the same one ldapConn (the admin connection
		// that added alice in BeforeAll) wrote to. Allow up to 30s for syncrepl
		// to propagate alice into the pod we're bound to. Same pattern as the
		// sibling "CAN read userPassword from ou=Mail" test above. The race is
		// more frequently triggered on the ephemeral fixture, where the
		// dataloss-recovery test deliberately wipes a pod mid-suite.
		Eventually(func(g Gomega) {
			req := ldap.NewSearchRequest(fmt.Sprintf("uid=alice,ou=People,%s", baseDN),
				ldap.ScopeBaseObject, ldap.NeverDerefAliases,
				0, 0, false, "(objectClass=*)", []string{"userPassword"}, nil)
			result, err := conn.Search(req)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.Entries).To(HaveLen(1),
				"alice must be present on the readpw-bound pod before ACL denial can be asserted")
			g.Expect(result.Entries[0].GetAttributeValue("userPassword")).To(BeEmpty(),
				"readpw user %q must not be able to read userPassword from ou=People", user)
		}).WithTimeout(30 * time.Second).WithPolling(2 * time.Second).Should(Succeed())
	})
})
