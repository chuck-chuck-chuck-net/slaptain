/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

func scForServerIDs(name, ns string, replicas int32, replEnabled, tls bool, sidBase int32) *ldapv1alpha1.SlapdCluster {
	return &ldapv1alpha1.SlapdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: ldapv1alpha1.SlapdClusterSpec{
			Replicas: replicas,
			Replication: ldapv1alpha1.SlapdReplicationConfig{
				Enabled:      replEnabled,
				ServerIDBase: sidBase,
			},
			LDAP: ldapv1alpha1.SlapdLDAPConfig{
				TLS: ldapv1alpha1.SlapdTLSConfig{Enabled: tls},
			},
		},
	}
}

func TestDesiredServerID(t *testing.T) {
	// ADR-017: a pod's olcServerID is the bare integer serverIDBase+ordinal+1,
	// independent of TLS, cluster name, namespace, and DNS domain — none of
	// which appear in the value any more.
	t.Run("standalone replication-off pod carries sid 1 (sid-1-per-default)", func(t *testing.T) {
		got := desiredServerID(scForServerIDs("slapd", "ns", 1, false, true, 0), 0)
		if got != "1" {
			t.Errorf("got %q, want %q", got, "1")
		}
	})

	t.Run("ordinal maps to base+ordinal+1", func(t *testing.T) {
		sc := scForServerIDs("slapd", "ns", 3, true, true, 0)
		for ordinal, want := range map[int32]string{0: "1", 1: "2", 2: "3"} {
			if got := desiredServerID(sc, ordinal); got != want {
				t.Errorf("ordinal %d: got %q, want %q", ordinal, got, want)
			}
		}
	})

	t.Run("value is independent of TLS/name/namespace/domain", func(t *testing.T) {
		a := desiredServerID(scForServerIDs("slapd", "ns", 2, true, true, 0), 1)
		b := desiredServerID(scForServerIDs("sl", "ns2", 2, true, false, 0), 1)
		if a != "2" || b != "2" {
			t.Errorf("expected both %q, got a=%q b=%q", "2", a, b)
		}
	})

	t.Run("serverIDBase shifts the range (ADR-011)", func(t *testing.T) {
		sc := scForServerIDs("slapd", "ns", 2, true, true, 100)
		if got := desiredServerID(sc, 0); got != "101" {
			t.Errorf("ordinal 0: got %q, want %q", got, "101")
		}
		if got := desiredServerID(sc, 1); got != "102" {
			t.Errorf("ordinal 1: got %q, want %q", got, "102")
		}
	})
}

func TestServerIDSetsEqual(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want bool
	}{
		{"both empty", nil, nil, true},
		{"identical", []string{"1 ldap://a:1024"}, []string{"1 ldap://a:1024"}, true},
		{"order-insensitive",
			[]string{"1 ldap://a:1024", "2 ldap://b:1024"},
			[]string{"2 ldap://b:1024", "1 ldap://a:1024"}, true},
		{"whitespace runs normalised",
			[]string{"1  ldap://a:1024"}, []string{"1 ldap://a:1024"}, true},
		{"different sid", []string{"1 ldap://a:1024"}, []string{"2 ldap://a:1024"}, false},
		{"missing on one side (the pod-0 transition case)",
			nil, []string{"1 ldap://a:1024"}, false},
		{"stale shorter list (scale-out drift)",
			[]string{"1 ldap://a:1024", "2 ldap://b:1024"},
			[]string{"1 ldap://a:1024", "2 ldap://b:1024", "3 ldap://c:1024"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := serverIDSetsEqual(c.a, c.b); got != c.want {
				t.Errorf("serverIDSetsEqual(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}
