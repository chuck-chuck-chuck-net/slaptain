package cmd

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// A cluster can exist with no SlapdDatabase at all — a fresh SlapdCluster, or
// one whose databases have not been applied yet — and cn=config is exactly
// what you want to read then. Only the bind identities that are DEFINED by a
// database (its rootDN, its credentials Secret, its suffix) need one.
func TestBindNeedsDatabase(t *testing.T) {
	cases := []struct {
		name string
		f    ldapTargetFlags
		want bool
	}{
		{"default (admin)", ldapTargetFlags{}, true},
		{identAdmin, ldapTargetFlags{as: identAdmin}, true},
		{identReplication, ldapTargetFlags{as: identReplication}, true},
		{identConfig, ldapTargetFlags{as: identConfig}, false},
		{"anonymous", ldapTargetFlags{anonymous: true}, false},
		{"anonymous wins over admin", ldapTargetFlags{as: identAdmin, anonymous: true}, false},
		{"literal full DN", ldapTargetFlags{as: configAdminDN}, false},
		{"literal DN with trailing comma", ldapTargetFlags{as: "cn=someone,ou=People,"}, true},
		{"literal short form", ldapTargetFlags{as: "someone"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bindNeedsDatabase(&tc.f); got != tc.want {
				t.Fatalf("bindNeedsDatabase(%+v) = %v, want %v", tc.f, got, tc.want)
			}
		})
	}
}

// resolveBind must survive a nil database for the identities that do not need
// one — that is the whole point of making the lookup conditional.
func TestResolveBindWithoutDatabase(t *testing.T) {
	coreClient := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "slapd-config-password", Namespace: testNamespace},
		Data:       map[string][]byte{"root-password": []byte("s3cret")},
	})
	sc := &ldapv1alpha1.SlapdCluster{ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Namespace: testNamespace}}

	t.Run(identConfig, func(t *testing.T) {
		dn, pw, err := resolveBind(context.Background(), coreClient, testNamespace, sc, nil,
			&ldapTargetFlags{as: identConfig})
		if err != nil {
			t.Fatalf("resolveBind: %v", err)
		}
		if dn != configAdminDN || pw != "s3cret" {
			t.Fatalf("got (%q, %q), want (cn=admin,cn=config, s3cret)", dn, pw)
		}
	})

	t.Run("anonymous", func(t *testing.T) {
		dn, pw, err := resolveBind(context.Background(), coreClient, testNamespace, sc, nil,
			&ldapTargetFlags{anonymous: true})
		if err != nil {
			t.Fatalf("resolveBind: %v", err)
		}
		if dn != "" || pw != "" {
			t.Fatalf("got (%q, %q), want empty bind", dn, pw)
		}
	})

	t.Run("literal full DN", func(t *testing.T) {
		dn, _, err := resolveBind(context.Background(), coreClient, testNamespace, sc, nil,
			&ldapTargetFlags{as: "cn=monitor,cn=config", password: "pw"})
		if err != nil {
			t.Fatalf("resolveBind: %v", err)
		}
		if dn != "cn=monitor,cn=config" {
			t.Fatalf("got %q, want cn=monitor,cn=config", dn)
		}
	})
}
