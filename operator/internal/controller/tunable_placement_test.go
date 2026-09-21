package controller

import "testing"

// WHICH ENTRY a global tunable lives in is a decision, not an incidental
// argument to a Modify call — and for one of them slapd has an opinion:
//
//	olcPasswordHash: value #0: setting password scheme in the global entry is
//	deprecated. The server may refuse to start if it is provided by a loadable
//	module, please move it to the frontend database instead
//
// Observed on every pod start (OpenLDAP 2.7.1). Harmless while the scheme is
// built in, fatal the day slaptain ships pw-argon2 and someone asks for
// {ARGON2} — which is exactly the backlog item this blocks. So the placement
// is pinned here, per attribute, rather than left as a literal in the caller.
func TestGlobalTunableEntry(t *testing.T) {
	tests := []struct {
		attr string
		want string
	}{
		// slapd says: the frontend, not the global entry.
		{"olcPasswordHash", "olcDatabase={-1}frontend,cn=config"},

		// The rest are genuinely global and slapd accepts them there.
		{"olcToolThreads", "cn=config"},
		{"olcTLSProtocolMin", "cn=config"},
		{"olcTLSCipherSuite", "cn=config"},

		// An attribute nobody has placed yet must default to the global entry
		// rather than silently landing in the frontend.
		{"olcSomethingElse", "cn=config"},
	}
	for _, tc := range tests {
		t.Run(tc.attr, func(t *testing.T) {
			if got := tunableEntryDN(tc.attr); got != tc.want {
				t.Errorf("tunableEntryDN(%q) = %q, want %q", tc.attr, got, tc.want)
			}
		})
	}
}
