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

// The directory list is not a list of CR names.
//
// A database's data directory is spec.dataDirectory when set, falling back to
// the CR name (slapddatabase_controller.go:863), while its accesslog directory
// is always AccesslogDir(CR name) (ADR-019 R3). The two diverge the moment
// dataDirectory is used, and the old listDatabaseNames collected db.Name for
// both — so a CR setting dataDirectory had the WRONG directory created for it
// and its ldap-add failed with LDAP 80 until someone noticed. Keyed on the
// effective names, in two separate lists, that whole class is gone.
func TestDatabaseDirsFor(t *testing.T) {
	db := func(name, clusterRef, dataDir string) ldapv1alpha1.SlapdDatabase {
		d := ldapv1alpha1.SlapdDatabase{ObjectMeta: metav1.ObjectMeta{Name: name}}
		d.Spec.ClusterRef = clusterRef
		d.Spec.DataDirectory = dataDir
		return d
	}

	for _, tc := range []struct {
		name string
		in   []ldapv1alpha1.SlapdDatabase
		want databaseDirs
	}{
		{
			name: "no databases",
			in:   nil,
			want: databaseDirs{},
		},
		{
			name: "the CR name is the directory when dataDirectory is unset",
			in:   []ldapv1alpha1.SlapdDatabase{db(testDBName, "slapd", "")},
			want: databaseDirs{Data: []string{testDBName}, Accesslog: []string{testDBName}},
		},
		{
			name: "dataDirectory overrides the DATA directory only",
			in:   []ldapv1alpha1.SlapdDatabase{db(testDBName, "slapd", "custom")},
			want: databaseDirs{Data: []string{"custom"}, Accesslog: []string{testDBName}},
		},
		{
			name: "another cluster's databases are not ours to provision",
			in: []ldapv1alpha1.SlapdDatabase{
				db("mine", "slapd", ""),
				db("theirs", "other-cluster", ""),
			},
			want: databaseDirs{Data: []string{"mine"}, Accesslog: []string{"mine"}},
		},
		{
			name: "sorted and deduplicated, so the list is a stable fingerprint",
			in: []ldapv1alpha1.SlapdDatabase{
				db("b-db", "slapd", "shared"),
				db("a-db", "slapd", "shared"),
			},
			want: databaseDirs{Data: []string{"shared"}, Accesslog: []string{"a-db", "b-db"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := databaseDirsFor(tc.in, "slapd")
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("databaseDirsFor() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
