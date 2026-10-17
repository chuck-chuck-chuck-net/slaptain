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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
	"github.com/chuck-chuck-chuck-net/slaptain/operator/internal/ridcoord"
)

// RID-PLAN hazard 3: an unprovable uniqueness is not a proven one. judgeRIDSegment
// answers "is this segment unique among THESE siblings"; resolveRIDSegment is
// what decides which siblings those are, and therefore what happens when the
// question cannot be answered at all.
//
// The failure is silent by nature. A List that returns an error is easy to
// imagine; a List that returns an EMPTY list because it errored and the caller
// ignored it looks exactly like a cluster with one database, converges happily,
// and writes stanzas that share RIDs with a database the operator could not
// see. slapd accepts a duplicate RID without a word (ADR-031, "What slapd
// actually requires").

func ridScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatalf("build scheme: %v", err)
	}
	if err := ldapv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add ldap scheme: %v", err)
	}
	return s
}

func ridReconciler(c client.Client) *SlapdDatabaseReconciler {
	return &SlapdDatabaseReconciler{Client: c, ClusterDomain: "cluster.local"}
}

// TestResolveRIDSegmentListFails pins hazard 3 on both axes at once: the
// withhold, and the reason that explains it.
func TestResolveRIDSegmentListFails(t *testing.T) {
	boom := errors.New("etcdserver: request timed out")

	c := fake.NewClientBuilder().
		WithScheme(ridScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
				if _, ok := list.(*ldapv1alpha1.SlapdDatabaseList); ok {
					return boom
				}
				return nil
			},
		}).
		Build()

	sd := ridDB("alpha", testRIDCluster, new(int32(3)))
	v := ridReconciler(c).resolveRIDSegment(context.Background(), &sd, ridcoord.DefaultLayout())

	// 1. The withhold. Reading the failure as "no siblings, therefore no
	//    collision" is how two databases end up sharing RIDs.
	if v.OK {
		t.Fatalf("a failed List produced an OK verdict (segment %d): an unread sibling set is not an empty one",
			v.Segment)
	}

	// 2. The reason. The withhold is the same as a collision's; the diagnosis
	//    is not. RIDSegmentCollision sends a human to edit a CR — here nothing
	//    may be wrong with any CR at all, and a real collision occurring
	//    during an apiserver blip would be indistinguishable from the blip.
	if v.Reason != reasonRIDSegmentUnverified {
		t.Errorf("reason = %q, want %q — a reason is a diagnosis, and alerting and slctl key on it",
			v.Reason, reasonRIDSegmentUnverified)
	}

	// 3. The message must carry the underlying error, or the one person who
	//    can fix an RBAC regression never learns there was one.
	if !strings.Contains(v.Message, boom.Error()) {
		t.Errorf("message %q does not carry the List error %q", v.Message, boom)
	}
	if !strings.Contains(v.Message, sd.Namespace) {
		t.Errorf("message %q does not name the namespace that could not be read", v.Message)
	}
}

// TestResolveRIDSegmentReadsSiblings pins the other half: when the List DOES
// succeed, the verdict is judged against what it returned. Without this, an
// implementation that never passed the siblings through would satisfy the
// failure case above and silently approve every collision.
func TestResolveRIDSegmentReadsSiblings(t *testing.T) {
	alpha := ridDB("alpha", testRIDCluster, new(int32(3)))
	bravo := ridDB("bravo", testRIDCluster, new(int32(3)))

	c := fake.NewClientBuilder().
		WithScheme(ridScheme(t)).
		WithObjects(&alpha, &bravo).
		Build()

	v := ridReconciler(c).resolveRIDSegment(context.Background(), &alpha, ridcoord.DefaultLayout())

	if v.OK {
		t.Fatalf("alpha converged while bravo declares its segment: the sibling List was not consulted")
	}
	if v.Reason != reasonRIDSegmentCollision {
		t.Errorf("reason = %q, want %q", v.Reason, reasonRIDSegmentCollision)
	}
	if !strings.Contains(v.Message, "bravo") {
		t.Errorf("message %q does not name the colliding database", v.Message)
	}
}

// TestResolveRIDSegmentScopesToNamespace pins that the read is namespaced. A
// cluster-wide List would collide two tenants' databases against each other —
// databases that share no pod and cannot collide (ADR-031: a RID is unique per
// SERVER), stalling both for a reason neither can act on.
func TestResolveRIDSegmentScopesToNamespace(t *testing.T) {
	alpha := ridDB("alpha", testRIDCluster, new(int32(3)))
	foreign := ridDB("alpha", testRIDCluster, new(int32(3)))
	foreign.Namespace = "another-tenant"

	c := fake.NewClientBuilder().
		WithScheme(ridScheme(t)).
		WithObjects(&alpha, &foreign).
		Build()

	v := ridReconciler(c).resolveRIDSegment(context.Background(), &alpha, ridcoord.DefaultLayout())

	if !v.OK {
		t.Fatalf("verdict %q (%s): a database in another namespace was treated as a sibling",
			v.Reason, v.Message)
	}
}
