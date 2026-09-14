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

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The live shape this guards, read off t3e (3-replica replicated cluster,
// OpenLDAP 2.7.1) on 2026-09-14:
//
//	olcDatabase={1}mdb,cn=config                           dc=example,dc=org
//	olcOverlay={0}syncprov,olcDatabase={1}mdb,cn=config
//	olcOverlay={1}accesslog,olcDatabase={1}mdb,cn=config    olcAccessLogDB: cn=accesslog-example-db
//	olcDatabase={2}mdb,cn=config                           cn=accesslog-example-db
//	olcOverlay={0}syncprov,olcDatabase={2}mdb,cn=config
//
// A bare Del of olcDatabase={1}mdb is a delete of a non-leaf, which slapd
// refuses with notAllowedOnNonLeaf (66) — bconfig.c:6931 `else if
// ( ce->ce_kids )`, reached because slap.h:73 defines SLAP_CONFIG_DELETE
// unconditionally. So cleanupPolicy: Delete could never remove a replicated
// database.
func TestPlanSubtreeDeletion(t *testing.T) {
	const dataDN = "olcDatabase={1}mdb,cn=config"

	cases := []struct {
		name string
		dns  []string
		want []string
	}{
		{
			name: "nothing observed",
			dns:  nil,
			want: nil,
		},
		{
			name: "a leaf database deletes as itself",
			dns:  []string{dataDN},
			want: []string{dataDN},
		},
		{
			name: "replicated data DB: both overlays go before the database",
			dns: []string{
				dataDN,
				"olcOverlay={0}syncprov," + dataDN,
				"olcOverlay={1}accesslog," + dataDN,
			},
			want: []string{
				"olcOverlay={0}syncprov," + dataDN,
				"olcOverlay={1}accesslog," + dataDN,
				dataDN,
			},
		},
		{
			name: "subtree search order is irrelevant — depth decides",
			dns: []string{
				"olcOverlay={1}accesslog," + dataDN,
				dataDN,
				"olcOverlay={0}syncprov," + dataDN,
			},
			want: []string{
				"olcOverlay={1}accesslog," + dataDN,
				"olcOverlay={0}syncprov," + dataDN,
				dataDN,
			},
		},
		{
			name: "grandchildren go before children go before the parent",
			dns: []string{
				dataDN,
				"olcOverlay={0}syncprov," + dataDN,
				"cn=deeper,olcOverlay={0}syncprov," + dataDN,
			},
			want: []string{
				"cn=deeper,olcOverlay={0}syncprov," + dataDN,
				"olcOverlay={0}syncprov," + dataDN,
				dataDN,
			},
		},
		{
			name: "an escaped comma in an RDN is not a level",
			dns: []string{
				`olcDatabase={1}mdb,cn=config`,
				`cn=a\,b,olcDatabase={1}mdb,cn=config`,
			},
			want: []string{
				`cn=a\,b,olcDatabase={1}mdb,cn=config`,
				`olcDatabase={1}mdb,cn=config`,
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := planSubtreeDeletion(c.dns)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("planSubtreeDeletion() = %v, want %v", got, c.want)
			}
		})
	}
}

// ADR-005 says cleanupPolicy: Delete "removes the olcDatabase entry from each
// pod's cn=config". Each pod means every pod that carries the database —
// cn=config is node-local (ADR-002) and an RO replica carries its own copy of
// the data database, so a teardown that skips the RO StatefulSet leaves the
// database served there forever.
func TestDatabasePodHosts(t *testing.T) {
	sc := func(replicas, readReplicas int32) *ldapv1alpha1.SlapdCluster {
		return &ldapv1alpha1.SlapdCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "slapd", Namespace: "ns"},
			Spec: ldapv1alpha1.SlapdClusterSpec{
				Replicas:     replicas,
				ReadReplicas: readReplicas,
			},
		}
	}

	cases := []struct {
		name string
		sc   *ldapv1alpha1.SlapdCluster
		want []databasePodHost
	}{
		{
			name: "unset replicas means one pod, not zero",
			sc:   sc(0, 0),
			want: []databasePodHost{
				{Name: "slapd-0", Host: "slapd-0.slapd-headless.ns.svc.cluster.local", Ordinal: 0},
			},
		},
		{
			name: "three RW pods",
			sc:   sc(3, 0),
			want: []databasePodHost{
				{Name: "slapd-0", Host: "slapd-0.slapd-headless.ns.svc.cluster.local", Ordinal: 0},
				{Name: "slapd-1", Host: "slapd-1.slapd-headless.ns.svc.cluster.local", Ordinal: 1},
				{Name: "slapd-2", Host: "slapd-2.slapd-headless.ns.svc.cluster.local", Ordinal: 2},
			},
		},
		{
			name: "read replicas ride the RO headless service and are flagged",
			sc:   sc(2, 2),
			want: []databasePodHost{
				{Name: "slapd-0", Host: "slapd-0.slapd-headless.ns.svc.cluster.local", Ordinal: 0},
				{Name: "slapd-1", Host: "slapd-1.slapd-headless.ns.svc.cluster.local", Ordinal: 1},
				{
					Name: "slapd-readonly-0", ReadOnly: true, Ordinal: 0,
					Host: "slapd-readonly-0.slapd-readonly-headless.ns.svc.cluster.local",
				},
				{
					Name: "slapd-readonly-1", ReadOnly: true, Ordinal: 1,
					Host: "slapd-readonly-1.slapd-readonly-headless.ns.svc.cluster.local",
				},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := databasePodHosts(c.sc, "cluster.local")
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("databasePodHosts() =\n  %v\nwant\n  %v", got, c.want)
			}
		})
	}
}
