package cmd

import "regexp"

// credentialsRE matches the plaintext bind password inside an olcSyncRepl
// stanza, in both the bare and the quoted spelling OpenLDAP accepts.
var credentialsRE = regexp.MustCompile(`credentials=(?:"[^"]*"|\S+)`)

// redactCredentials masks the replication bind password in a syncrepl stanza.
//
// A stanza carries the credential in the clear — that is simple bind and is not
// negotiable (ADR-027 keeps it, and moves only the verifier copy). What IS
// negotiable is whether it lands in artifacts people hand to other people:
// `slctl debug-dump` writes a bundle for a bug report, `slctl inspect` output
// gets pasted into tickets, and the e2e prints stanzas in failure messages that
// end up in CI logs. A real replication password has been observed in a captured
// e2e log for exactly this reason.
//
// This is deliberately the OPPOSITE default from `slctl ldapsearch --verbose`,
// which prints the password in full so the command it echoes is copy-pasteable.
// That output is for the operator's own terminal, right now; this output is an
// artifact with an audience. Same project, different blast radius.
func redactCredentials(s string) string {
	return credentialsRE.ReplaceAllString(s, "credentials=<redacted>")
}

// redactCredentialsAll is redactCredentials over a slice, for the stanza lists
// inspect carries into both its human output and its --json.
func redactCredentialsAll(vals []string) []string {
	if len(vals) == 0 {
		return vals
	}
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = redactCredentials(v)
	}
	return out
}
