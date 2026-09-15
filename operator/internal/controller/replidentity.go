package controller

import "fmt"

// STATUS-QUO STUB (pre-cutover behaviour) — replaced once the tests are red.
func replicationBindDN(dbName string) string { return "cn=replication" }
func legacyReplicationBindDN(suffix string) string {
	return "cn=replication," + suffix
}
func replicationACL(dbName, suffix string) string {
	return fmt.Sprintf(`to * by dn.exact="cn=replication,%s" read by * break`, suffix)
}
func csnBindDNs(dbName, suffix string) []string {
	return []string{legacyReplicationBindDN(suffix)}
}

type authIdentityState struct {
	AllRWPodsConverged bool
	AllROPodsConverged bool
}

// STATUS-QUO STUB: today there is no gate at all — the identity step is
// best-effort and stanzas are written regardless.
func authIdentityReadyForStanzas(authIdentityState) bool { return true }
