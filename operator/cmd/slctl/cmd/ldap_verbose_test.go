package cmd

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestFormatInvocation(t *testing.T) {
	cases := []struct {
		name   string
		tool   string
		args   []string
		redact bool
		want   string
	}{
		{
			name: "bind invocation, password shown and single-quoted",
			tool: "ldapsearch",
			args: []string{"-H", "ldap://localhost:34287", "-x", "-D", "cn=admin,dc=demo,dc=example", "-w", "s3cr3t", "-b", "dc=demo,dc=example", "-s", "one", "dn"},
			want: "ldapsearch -H ldap://localhost:34287 -x -D cn=admin,dc=demo,dc=example -w 's3cr3t' -b dc=demo,dc=example -s one dn",
		},
		{
			name:   "redacted password",
			tool:   "ldapsearch",
			args:   []string{"-H", "ldap://localhost:34287", "-x", "-D", "cn=admin,dc=demo,dc=example", "-w", "s3cr3t"},
			redact: true,
			want:   "ldapsearch -H ldap://localhost:34287 -x -D cn=admin,dc=demo,dc=example -w '<password>'",
		},
		{
			name: "anonymous bind has no -w to touch",
			tool: "ldapsearch",
			args: []string{"-H", "ldap://localhost:1024", "-x", "-b", "", "-s", "base", "+"},
			want: "ldapsearch -H ldap://localhost:1024 -x -b '' -s base +",
		},
		{
			name: "value needing quotes is shell-quoted",
			tool: "ldapsearch",
			args: []string{"-H", "ldap://localhost:1024", "-x", "-b", "ou=People,dc=demo,dc=example", "(uid=alice)"},
			want: "ldapsearch -H ldap://localhost:1024 -x -b ou=People,dc=demo,dc=example '(uid=alice)'",
		},
		{
			name: "password with a single quote stays safely quoted",
			tool: "ldapadd",
			args: []string{"-H", "ldap://localhost:1024", "-x", "-D", "cn=admin,dc=demo,dc=example", "-w", "a'b"},
			want: `ldapadd -H ldap://localhost:1024 -x -D cn=admin,dc=demo,dc=example -w 'a'\''b'`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatInvocation(tc.tool, tc.args, tc.redact); got != tc.want {
				t.Errorf("formatInvocation() =\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}

func TestFormatPortForwardCmd(t *testing.T) {
	cases := []struct {
		name       string
		global     []string
		ns         string
		pod        string
		localPort  int
		remotePort int
		want       string
	}{
		{
			name:       "no global flags",
			ns:         "slaptain-demo",
			pod:        "slapd-0",
			localPort:  34287,
			remotePort: 1024,
			want:       "kubectl -n slaptain-demo port-forward pod/slapd-0 34287:1024",
		},
		{
			name:       "context flag threaded through",
			global:     []string{"--context=s1"},
			ns:         "slaptain-demo",
			pod:        "slapd-0",
			localPort:  40001,
			remotePort: 1025,
			want:       "kubectl --context=s1 -n slaptain-demo port-forward pod/slapd-0 40001:1025",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatPortForwardCmd(tc.global, tc.ns, tc.pod, tc.localPort, tc.remotePort)
			if got != tc.want {
				t.Errorf("formatPortForwardCmd() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestChooseNodeIP(t *testing.T) {
	nodeWith := func(addrs ...corev1.NodeAddress) corev1.Node {
		return corev1.Node{Status: corev1.NodeStatus{Addresses: addrs}}
	}
	internal := corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.0.0.5"}
	external := corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: "203.0.113.9"}

	t.Run("override wins over auto-discovery", func(t *testing.T) {
		got, err := chooseNodeIP("192.0.2.50", []corev1.Node{nodeWith(internal)})
		if err != nil || got != "192.0.2.50" {
			t.Fatalf("chooseNodeIP(override) = (%q, %v), want (192.0.2.50, nil)", got, err)
		}
	})
	t.Run("falls back to InternalIP", func(t *testing.T) {
		got, err := chooseNodeIP("", []corev1.Node{nodeWith(external, internal)})
		if err != nil || got != "10.0.0.5" {
			t.Fatalf("chooseNodeIP(auto) = (%q, %v), want (10.0.0.5, nil)", got, err)
		}
	})
	t.Run("errors when no InternalIP and no override", func(t *testing.T) {
		if _, err := chooseNodeIP("", []corev1.Node{nodeWith(external)}); err == nil {
			t.Fatalf("chooseNodeIP with no InternalIP: want error, got nil")
		}
	})
}
