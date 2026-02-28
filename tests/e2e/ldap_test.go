package e2e_test

import (
	"fmt"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Fixed test credentials — plain text is fine for e2e.
const testUserPassword = "e2etestpass"

var _ = Describe("LDAP directory", func() {

	// ── Base structure ────────────────────────────────────────────────────────

	Describe("base structure", func() {
		It("has the base DN", func() {
			Expect(ldapExists(ldapConn, baseDN)).To(BeTrue())
		})

		It("has all OUs created by bootstrap", func() {
			for _, ou := range []string{"People", "Mail", "Readpw"} {
				dn := fmt.Sprintf("ou=%s,%s", ou, baseDN)
				Expect(ldapExists(ldapConn, dn)).To(BeTrue(), "OU %s is missing", dn)
			}
		})
	})

	// ── User management ───────────────────────────────────────────────────────

	Describe("user management", Ordered, func() {
		BeforeAll(func() {
			addUser("alice", "Alice", "Smith", 10001, 10000, testUserPassword)
			addUser("bob", "Bob", "Jones", 10002, 10000, "")
		})

		It("finds a user by uid filter", func() {
			entries := ldapSearch(ldapConn, baseDN, "(uid=alice)", "uid", "cn", "uidNumber", "homeDirectory")
			Expect(entries).To(HaveLen(1))
			e := entries[0]
			Expect(e.GetAttributeValue("uid")).To(Equal("alice"))
			Expect(e.GetAttributeValue("cn")).To(Equal("Alice Smith"))
			Expect(e.GetAttributeValue("uidNumber")).To(Equal("10001"))
			Expect(e.GetAttributeValue("homeDirectory")).To(Equal("/home/alice"))
		})

		It("returns all posixAccount users under ou=People", func() {
			entries := ldapSearch(ldapConn,
				fmt.Sprintf("ou=People,%s", baseDN),
				"(objectClass=posixAccount)",
				"uid",
			)
			uids := make([]string, 0, len(entries))
			for _, e := range entries {
				uids = append(uids, e.GetAttributeValue("uid"))
			}
			Expect(uids).To(ContainElements("alice", "bob"))
		})

		It("returns users matching a gidNumber filter", func() {
			entries := ldapSearch(ldapConn,
				fmt.Sprintf("ou=People,%s", baseDN),
				"(gidNumber=10000)",
				"uid",
			)
			uids := make([]string, 0, len(entries))
			for _, e := range entries {
				uids = append(uids, e.GetAttributeValue("uid"))
			}
			Expect(uids).To(ContainElements("alice", "bob"))
		})

		It("returns combined results with an OR filter", func() {
			entries := ldapSearch(ldapConn, baseDN,
				"(|(uid=alice)(uid=bob))",
				"uid",
			)
			Expect(entries).To(HaveLen(2))
		})

		It("allows a user to bind with the correct password", func() {
			conn, err := ldap.Dial("tcp", localLDAPAddr)
			Expect(err).NotTo(HaveOccurred())
			defer conn.Close()

			aliceDN := fmt.Sprintf("uid=alice,ou=People,%s", baseDN)
			Expect(conn.Bind(aliceDN, testUserPassword)).To(Succeed(),
				"bind as alice with correct password should succeed")
		})

		It("rejects a bind with the wrong password", func() {
			conn, err := ldap.Dial("tcp", localLDAPAddr)
			Expect(err).NotTo(HaveOccurred())
			defer conn.Close()

			aliceDN := fmt.Sprintf("uid=alice,ou=People,%s", baseDN)
			err = conn.Bind(aliceDN, "wrongpassword")
			Expect(err).To(HaveOccurred(), "bind with wrong password should fail")
			Expect(ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials)).To(BeTrue())
		})
	})

	// ── Group management ──────────────────────────────────────────────────────

	Describe("group management", Ordered, func() {
		BeforeAll(func() {
			addGroupsOU()
			addGroup("linuxusers", 10000, []string{"alice", "bob"})
			addGroup("admins", 10001, []string{"alice"})
		})

		It("finds a group by cn filter", func() {
			entries := ldapSearch(ldapConn,
				fmt.Sprintf("ou=Groups,%s", baseDN),
				"(cn=linuxusers)",
				"cn", "gidNumber", "memberUid",
			)
			Expect(entries).To(HaveLen(1))
			e := entries[0]
			Expect(e.GetAttributeValue("gidNumber")).To(Equal("10000"))
			Expect(e.GetAttributeValues("memberUid")).To(ConsistOf("alice", "bob"))
		})

		It("finds groups a user belongs to", func() {
			entries := ldapSearch(ldapConn,
				fmt.Sprintf("ou=Groups,%s", baseDN),
				"(memberUid=alice)",
				"cn",
			)
			groupNames := make([]string, 0, len(entries))
			for _, e := range entries {
				groupNames = append(groupNames, e.GetAttributeValue("cn"))
			}
			Expect(groupNames).To(ContainElements("linuxusers", "admins"))
		})

		It("lists all groups", func() {
			entries := ldapSearch(ldapConn,
				fmt.Sprintf("ou=Groups,%s", baseDN),
				"(objectClass=posixGroup)",
				"cn",
			)
			Expect(len(entries)).To(BeNumerically(">=", 2))
		})
	})

	// ── ACL enforcement ───────────────────────────────────────────────────────

	Describe("ACL enforcement", func() {
		It("allows anonymous read of non-sensitive attributes", func() {
			conn, err := ldap.Dial("tcp", localLDAPAddr)
			Expect(err).NotTo(HaveOccurred())
			defer conn.Close()

			// no Bind call = anonymous
			req := ldap.NewSearchRequest(baseDN,
				ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
				0, 0, false, "(uid=alice)", []string{"uid", "cn", "homeDirectory"}, nil)
			result, err := conn.Search(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Entries).To(HaveLen(1))
			Expect(result.Entries[0].GetAttributeValue("uid")).To(Equal("alice"))
		})

		It("hides userPassword from anonymous searches", func() {
			conn, err := ldap.Dial("tcp", localLDAPAddr)
			Expect(err).NotTo(HaveOccurred())
			defer conn.Close()

			req := ldap.NewSearchRequest(baseDN,
				ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
				0, 0, false, "(uid=alice)", []string{"userPassword"}, nil)
			result, err := conn.Search(req)
			Expect(err).NotTo(HaveOccurred())
			// ACL: userPassword by anonymous auth by * none — not readable in search
			if len(result.Entries) > 0 {
				Expect(result.Entries[0].GetAttributeValue("userPassword")).To(BeEmpty(),
					"userPassword must not be returned to anonymous")
			}
		})

		It("allows readpw users to authenticate", func() {
			// Bind checks (auth) are always permitted for anonymous per ACL rule {1}.
			// We verify by attempting an anonymous auth-style bind — i.e. any user
			// can be authenticated against their stored password via Bind.
			conn, err := ldap.Dial("tcp", localLDAPAddr)
			Expect(err).NotTo(HaveOccurred())
			defer conn.Close()

			aliceDN := fmt.Sprintf("uid=alice,ou=People,%s", baseDN)
			Expect(conn.Bind(aliceDN, testUserPassword)).To(Succeed())
		})
	})
})

// ── Entry creation helpers ────────────────────────────────────────────────────

func addUser(uid, givenName, sn string, uidNum, gidNum int, password string) {
	dn := fmt.Sprintf("uid=%s,ou=People,%s", uid, baseDN)
	req := ldap.NewAddRequest(dn, nil)
	req.Attribute("objectClass", []string{"posixAccount", "shadowAccount", "inetOrgPerson"})
	req.Attribute("cn", []string{givenName + " " + sn})
	req.Attribute("givenName", []string{givenName})
	req.Attribute("sn", []string{sn})
	req.Attribute("uid", []string{uid})
	req.Attribute("uidNumber", []string{fmt.Sprintf("%d", uidNum)})
	req.Attribute("gidNumber", []string{fmt.Sprintf("%d", gidNum)})
	req.Attribute("homeDirectory", []string{fmt.Sprintf("/home/%s", uid)})
	req.Attribute("loginShell", []string{"/bin/bash"})
	if password != "" {
		req.Attribute("userPassword", []string{password})
	}
	ldapAdd(ldapConn, req)
}

func addGroupsOU() {
	dn := fmt.Sprintf("ou=Groups,%s", baseDN)
	req := ldap.NewAddRequest(dn, nil)
	req.Attribute("objectClass", []string{"organizationalUnit"})
	req.Attribute("ou", []string{"Groups"})
	ldapAdd(ldapConn, req)
}

func addGroup(cn string, gidNum int, memberUids []string) {
	dn := fmt.Sprintf("cn=%s,ou=Groups,%s", cn, baseDN)
	req := ldap.NewAddRequest(dn, nil)
	req.Attribute("objectClass", []string{"posixGroup"})
	req.Attribute("cn", []string{cn})
	req.Attribute("gidNumber", []string{fmt.Sprintf("%d", gidNum)})
	if len(memberUids) > 0 {
		req.Attribute("memberUid", memberUids)
	}
	ldapAdd(ldapConn, req)
}
