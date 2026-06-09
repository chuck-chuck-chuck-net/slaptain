/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package backup

import (
	"os"
	"path/filepath"
	"testing"
)

// sshaSecret is an independent {SSHA} vector: SHA-1("s3cr3t" + "abcd1234")
// base64-encoded with the salt appended. Generated outside this code so the test
// verifies the implementation rather than round-tripping it against itself.
const sshaSecret = "{SSHA}SfVY60kkPbmJmcsqh9E8B+/CbZlhYmNkMTIzNA=="

func TestSSHAMatches(t *testing.T) {
	tests := []struct {
		name     string
		stored   string
		password string
		want     bool
	}{
		{"correct password", sshaSecret, "s3cr3t", true},
		{"wrong password", sshaSecret, "wrong", false},
		{"empty password", sshaSecret, "", false},
		// Default-deny: anything that is not a verifiable {SSHA} must return false
		// so the caller fails the restore unless explicitly overridden.
		{"plaintext scheme", "{CLEARTEXT}s3cr3t", "s3cr3t", false},
		{"crypt scheme", "{CRYPT}abcdef", "s3cr3t", false},
		{"argon2 scheme", "{ARGON2}$argon2id$v=19$x", "s3cr3t", false},
		{"bare hash no scheme", "deadbeef", "s3cr3t", false},
		{"empty stored", "", "s3cr3t", false},
		{"invalid base64", "{SSHA}not-base64!!", "s3cr3t", false},
		{"too short to hold salt", "{SSHA}YWJj", "s3cr3t", false}, // decodes to "abc", < sha1.Size
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sshaMatches(tc.stored, tc.password); got != tc.want {
				t.Errorf("sshaMatches(%q, %q) = %v, want %v", tc.stored, tc.password, got, tc.want)
			}
		})
	}
}

func TestLDIFValue(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		attr      string
		wantVal   string
		wantFound bool
	}{
		{"plain value", "userPassword: {SSHA}abc", "userPassword", "{SSHA}abc", true},
		{"base64 value", "userPassword:: e1NTSEF9YWJj", "userPassword", "{SSHA}abc", true}, // base64("{SSHA}abc")
		{"different attr", "cn: replication", "userPassword", "", false},
		{"attr is prefix of another", "userPasswordPolicy: x", "userPassword", "", false},
		{"invalid base64", "userPassword:: not-base64!!!", "userPassword", "", false},
		{"no space after colon", "userPassword:x", "userPassword", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			val, found := ldifValue(tc.line, tc.attr)
			if found != tc.wantFound || val != tc.wantVal {
				t.Errorf("ldifValue(%q, %q) = (%q, %v), want (%q, %v)",
					tc.line, tc.attr, val, found, tc.wantVal, tc.wantFound)
			}
		})
	}
}

func TestMetaValue(t *testing.T) {
	meta := map[string]string{"Replication-Pw-Hash": "  {SSHA}abc  ", "other": "x"}
	if got := metaValue(meta, "replication-pw-hash"); got != "{SSHA}abc" {
		t.Errorf("metaValue case-insensitive+trim = %q, want %q", got, "{SSHA}abc")
	}
	if got := metaValue(meta, "absent"); got != "" {
		t.Errorf("metaValue(absent) = %q, want empty", got)
	}
	if got := metaValue(nil, "x"); got != "" {
		t.Errorf("metaValue(nil) = %q, want empty", got)
	}
}

func TestReplicationHashFromLDIF(t *testing.T) {
	dir := t.TempDir()

	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}

	t.Run("absent file returns empty without error", func(t *testing.T) {
		got, err := ReplicationHashFromLDIF(filepath.Join(dir, "does-not-exist.ldif"))
		if err != nil || got != "" {
			t.Errorf("got (%q, %v), want (\"\", nil)", got, err)
		}
	})

	t.Run("entry with userPassword", func(t *testing.T) {
		p := write("repl.ldif", "dn: cn=replication,dc=example,dc=org\n"+
			"objectClass: simpleSecurityObject\n"+
			"cn: replication\n"+
			"userPassword: "+sshaSecret+"\n")
		got, err := ReplicationHashFromLDIF(p)
		if err != nil || got != sshaSecret {
			t.Errorf("got (%q, %v), want (%q, nil)", got, err, sshaSecret)
		}
	})

	t.Run("folded continuation line is unfolded", func(t *testing.T) {
		// slapcat wraps long values at 78 cols with a leading-space continuation.
		half1 := sshaSecret[:20]
		half2 := sshaSecret[20:]
		p := write("folded.ldif", "dn: cn=replication,dc=example,dc=org\n"+
			"userPassword: "+half1+"\n"+
			" "+half2+"\n")
		got, err := ReplicationHashFromLDIF(p)
		if err != nil || got != sshaSecret {
			t.Errorf("got (%q, %v), want (%q, nil)", got, err, sshaSecret)
		}
	})

	t.Run("empty slapcat output (no entry) returns empty", func(t *testing.T) {
		p := write("empty.ldif", "")
		got, err := ReplicationHashFromLDIF(p)
		if err != nil || got != "" {
			t.Errorf("got (%q, %v), want (\"\", nil)", got, err)
		}
	})
}
