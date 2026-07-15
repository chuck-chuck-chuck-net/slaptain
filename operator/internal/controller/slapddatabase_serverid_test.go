/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"reflect"
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

func TestDesiredServerIDs(t *testing.T) {
	const domain = "cluster.local"

	t.Run("standalone replication-off cluster carries sid 1 (sid-1-per-default)", func(t *testing.T) {
		want := []string{"1 ldaps://slapd-0.slapd-headless.ns.svc.cluster.local:1025"}
		got := desiredServerIDs(scForServerIDs("slapd", "ns", 1, false, true, 0), domain)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("replicas unset defaults to 1", func(t *testing.T) {
		want := []string{"1 ldaps://slapd-0.slapd-headless.ns.svc.cluster.local:1025"}
		got := desiredServerIDs(scForServerIDs("slapd", "ns", 0, false, true, 0), domain)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("scale-down shrinks the list (replicas 3→2 desired state)", func(t *testing.T) {
		got := desiredServerIDs(scForServerIDs("slapd", "ns", 2, true, true, 0), domain)
		if len(got) != 2 {
			t.Errorf("want 2 entries after scale-down, got %v", got)
		}
	})

	t.Run("3 replicas TLS", func(t *testing.T) {
		want := []string{
			"1 ldaps://slapd-0.slapd-headless.ns.svc.cluster.local:1025",
			"2 ldaps://slapd-1.slapd-headless.ns.svc.cluster.local:1025",
			"3 ldaps://slapd-2.slapd-headless.ns.svc.cluster.local:1025",
		}
		got := desiredServerIDs(scForServerIDs("slapd", "ns", 3, true, true, 0), domain)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("2 replicas no TLS, non-default domain", func(t *testing.T) {
		want := []string{
			"1 ldap://sl-0.sl-headless.ns2.svc.k8s.example:1024",
			"2 ldap://sl-1.sl-headless.ns2.svc.k8s.example:1024",
		}
		got := desiredServerIDs(scForServerIDs("sl", "ns2", 2, true, false, 0), "k8s.example")
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("serverIDBase shifts the range (ADR-011)", func(t *testing.T) {
		got := desiredServerIDs(scForServerIDs("slapd", "ns", 2, true, true, 100), domain)
		want := []string{
			"101 ldaps://slapd-0.slapd-headless.ns.svc.cluster.local:1025",
			"102 ldaps://slapd-1.slapd-headless.ns.svc.cluster.local:1025",
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
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
