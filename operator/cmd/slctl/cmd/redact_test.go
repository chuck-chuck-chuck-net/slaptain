package cmd

import "testing"

func TestRedactCredentials(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{
			"bare value in a full stanza",
			`{0}rid=102 provider=ldaps://p:1025 binddn="cn=repl,dc=x" credentials=s3cr3t+/= retry="10 +"`,
			`{0}rid=102 provider=ldaps://p:1025 binddn="cn=repl,dc=x" credentials=<redacted> retry="10 +"`,
		},
		{
			"quoted value",
			`credentials="with space" searchbase="dc=x"`,
			`credentials=<redacted> searchbase="dc=x"`,
		},
		{
			"every occurrence, not just the first",
			`credentials=a credentials=b`,
			`credentials=<redacted> credentials=<redacted>`,
		},
		{
			"a stanza without one is untouched",
			`{0}rid=102 provider=ldaps://p:1025 bindmethod=sasl`,
			`{0}rid=102 provider=ldaps://p:1025 bindmethod=sasl`,
		},
		{
			"network-timeout is not mistaken for it",
			`network-timeout=10 keepalive=240:3:30`,
			`network-timeout=10 keepalive=240:3:30`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactCredentials(tc.in); got != tc.want {
				t.Errorf("redactCredentials()\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}
