/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
	"github.com/chuck-chuck-chuck-net/slaptain/operator/internal/ridcoord"
)

// SlapdMesh.spec.ridLayout ships in M3 as live API that NOTHING reads — the
// judgement hardcodes ridcoord.DefaultLayout(). A declared layout is therefore
// silently ignored, which ADR-024 forbids outright: a spec field that cannot be
// honoured is refused or converged, never ignored. ridLayoutFor is the gate
// that closes it, and these tests are the gate's spec.
//
// The third case is the one with teeth. A mesh that cannot be read means the
// layout is UNKNOWN, and unknown withholds (RID-PLAN hazard 3) — because
// defaulting a layout the mesh may have narrowed renumbers every stanza on
// every pod of THIS site and no other, which is the one failure a mesh-scoped
// constant exists to prevent.

const testMeshName = "ldap"

// ridCluster builds a SlapdCluster in the fixture namespace, with meshRef set
// to `meshRef` ("" for a cluster that is not part of a mesh).
func ridCluster(meshRef string) *ldapv1alpha1.SlapdCluster {
	return &ldapv1alpha1.SlapdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: testRIDCluster, Namespace: testRIDNS},
		Spec:       ldapv1alpha1.SlapdClusterSpec{MeshRef: meshRef},
	}
}

// ridMesh builds a SlapdMesh carrying `layout` (nil for "ridLayout omitted").
func ridMesh(layout *ldapv1alpha1.RIDLayout) *ldapv1alpha1.SlapdMesh {
	return &ldapv1alpha1.SlapdMesh{
		ObjectMeta: metav1.ObjectMeta{Name: testMeshName, Namespace: testRIDNS},
		Spec:       ldapv1alpha1.SlapdMeshSpec{RIDLayout: layout},
	}
}

func TestRIDLayoutForReadsTheMesh(t *testing.T) {
	def := ridcoord.DefaultLayout()

	for _, tc := range []struct {
		name   string
		mesh   *ldapv1alpha1.RIDLayout
		want   ridcoord.Layout
		reason string
	}{
		{
			name:   "a declared layout is the one in force",
			mesh:   &ldapv1alpha1.RIDLayout{PodsPerSite: 4, MaxSites: 6},
			want:   ridcoord.Layout{PodsPerSite: 4, MaxSites: 6},
			reason: "the field exists to be obeyed; hardcoding the defaults is M3's open gate",
		},
		{
			name:   "an omitted ridLayout means the defaults",
			mesh:   nil,
			want:   def,
			reason: "ADR-031 decision 5: omitted means the defaults, which is not an error",
		},
		{
			name: "a zero podsPerSite is unset, not unusable",
			mesh: &ldapv1alpha1.RIDLayout{MaxSites: 6},
			want: ridcoord.Layout{PodsPerSite: def.PodsPerSite, MaxSites: 6},
			reason: "API-server defaulting runs only on the API server's path, so a mesh unmarshalled " +
				"from a manifest or restored from a backup arrives with a zero in the axis it never set",
		},
		{
			name: "a zero maxSites is unset, not unusable",
			mesh: &ldapv1alpha1.RIDLayout{PodsPerSite: 4},
			want: ridcoord.Layout{PodsPerSite: 4, MaxSites: def.MaxSites},
			reason: "defaulting is PER AXIS: one declared axis must not drag the other to zero, " +
				"which would make the whole layout unusable and withhold every stanza in the mesh",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := ridCluster(testMeshName)
			c := fake.NewClientBuilder().
				WithScheme(ridScheme(t)).
				WithObjects(sc, ridMesh(tc.mesh)).
				Build()

			got, err := ridLayoutFor(context.Background(), c, sc)
			if err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
			if got != tc.want {
				t.Errorf("layout = %+v, want %+v — %s", got, tc.want, tc.reason)
			}
		})
	}
}

// A cluster with no meshRef uses the defaults and reads nothing. The no-I/O
// half is not fastidiousness: every SlapdDatabase reconcile of every
// non-mesh cluster would otherwise pay a Get for an object that cannot exist,
// and a Get against a kind the operator may have no RBAC for is an error that
// would then have to be excused — which is how an unreadable mesh ends up
// silently defaulting.
func TestRIDLayoutForWithoutAMeshDefaultsAndReadsNothing(t *testing.T) {
	sc := ridCluster("")

	c := fake.NewClientBuilder().
		WithScheme(ridScheme(t)).
		WithObjects(sc).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				t.Errorf("ridLayoutFor read %T %v for a cluster with no meshRef", obj, key)
				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	got, err := ridLayoutFor(context.Background(), c, sc)
	if err != nil {
		t.Fatalf("unexpected refusal for a cluster with no mesh: %v", err)
	}
	if want := ridcoord.DefaultLayout(); got != want {
		t.Errorf("layout = %+v, want the defaults %+v", got, want)
	}
}

// Hazard 3's shape, and the whole point of the gate: unknown withholds.
//
// Both halves are asserted, because an implementation can satisfy the first and
// still be the bug. Returning (DefaultLayout(), err) looks correct until one
// caller drops the error — and then a mesh that narrowed its layout is silently
// replaced by the defaults, which renumbers every stanza at this site alone and
// makes a RID read here decode to a different provider everywhere else.
func TestRIDLayoutForWithholdsWhenTheMeshCannotBeRead(t *testing.T) {
	boom := errors.New("etcdserver: request timed out")

	for _, tc := range []struct {
		name    string
		objects []client.Object
		funcs   interceptor.Funcs
	}{
		{
			name:    "the mesh read fails",
			objects: []client.Object{ridMesh(&ldapv1alpha1.RIDLayout{PodsPerSite: 4, MaxSites: 6})},
			funcs: interceptor.Funcs{
				Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
					if _, ok := obj.(*ldapv1alpha1.SlapdMesh); ok {
						return boom
					}
					return nil
				},
			},
		},
		{
			name: "the mesh named by meshRef does not exist",
			// Not found is not "no mesh, therefore defaults": the cluster says
			// it belongs to one, so the layout is a value this operator cannot
			// see rather than a value nobody declared.
			objects: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := ridCluster(testMeshName)
			b := fake.NewClientBuilder().WithScheme(ridScheme(t)).WithObjects(sc)
			for _, o := range tc.objects {
				b = b.WithObjects(o)
			}
			c := b.WithInterceptorFuncs(tc.funcs).Build()

			got, err := ridLayoutFor(context.Background(), c, sc)
			if err == nil {
				t.Fatalf("layout %+v returned with no error: an unreadable mesh is an unknown layout, "+
					"and unknown withholds rather than defaulting", got)
			}
			if got.Validate() == nil {
				t.Errorf("layout %+v is usable alongside the error — a caller that drops the error "+
					"converges on it, which is exactly the silent default this gate exists to prevent", got)
			}
		})
	}
}

// The belt to that brace, at the seam where it matters: the unusable layout a
// failed read returns is refused by the judgement itself, so even a caller that
// ignores the error writes no stanzas. Composition rather than duplication —
// judgeRIDSegment already refuses an invalid layout first (M3), and this pins
// that ridLayoutFor's failure value is the kind of layout it refuses.
func TestAnUnreadableMeshsLayoutIsRefusedByTheSegmentJudgement(t *testing.T) {
	sc := ridCluster(testMeshName)
	c := fake.NewClientBuilder().
		WithScheme(ridScheme(t)).
		WithObjects(sc).
		Build()

	layout, err := ridLayoutFor(context.Background(), c, sc)
	if err == nil {
		t.Fatalf("the mesh is absent and ridLayoutFor did not refuse")
	}

	db := ridDB("alpha", testRIDCluster, new(int32(0)))
	v := judgeRIDSegment(&db, nil, layout)
	if v.OK {
		t.Fatalf("segment %d accepted against a layout that could not be resolved", v.Segment)
	}
	if v.Reason != reasonRIDLayoutInvalid {
		t.Errorf("reason = %q, want %q", v.Reason, reasonRIDLayoutInvalid)
	}
}
