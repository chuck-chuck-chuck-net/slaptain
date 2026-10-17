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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// ADR-028's amendment of 2026-10-05: ADR-031's layout governs the site index,
// and ADR-028 yields. A site's serverIDIndex must be STRICTLY BELOW the mesh's
// spec.ridLayout.maxSites, because a peer's RID slot is podsPerSite +
// serverIDIndex and the remote block is maxSites wide — at the default
// maxSites: 12, index 12 has no slot to live in and no RID can address that
// site.
//
// It is checked at admission rather than at the first reconcile because the
// alternative is a mesh that applies cleanly everywhere and then withholds
// every cross-site stanza at one site, which is the expensive way to find out.
// And it cannot be a CRD maximum, the way the old ceiling of 40 is: maxSites is
// configurable, so the bound is a property of the object being validated.
//
// These specs run against the REAL CRD the envtest API server installed, so
// they also answer the question a CEL rule has to answer before anything else:
// does it install at all? A rule the API server rejects on cost takes the whole
// CRD down with it — every SlapdMesh in the cluster, not just the bad field —
// and that failure shows up here as the create of a VALID mesh failing, not as
// a lint error.

var meshAdmissionSeq int

func meshAdmissionNamespace(ctx context.Context) string {
	meshAdmissionSeq++
	ns := fmt.Sprintf("mesh-siteindex-%d", meshAdmissionSeq)
	Expect(k8sClient.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	})).To(Succeed())
	return ns
}

// meshWithSites builds a SlapdMesh carrying `layout` (nil for "omitted", which
// means the defaults) and one site per index.
func meshWithSites(ns string, layout *ldapv1alpha1.RIDLayout, indices ...int32) *ldapv1alpha1.SlapdMesh {
	sites := make([]ldapv1alpha1.MeshSite, 0, len(indices))
	for i, idx := range indices {
		v := idx
		sites = append(sites, ldapv1alpha1.MeshSite{
			Name:          fmt.Sprintf("site-%d", i+1),
			ServerIDIndex: &v,
		})
	}
	return &ldapv1alpha1.SlapdMesh{
		ObjectMeta: metav1.ObjectMeta{Name: testMeshName, Namespace: ns},
		Spec: ldapv1alpha1.SlapdMeshSpec{
			Sites:     sites,
			RIDLayout: layout,
		},
	}
}

var _ = Describe("serverIDIndex is bounded by the mesh's own ridLayout (ADR-028 amendment, ADR-031 decision 4)", func() {
	ctx := context.Background()

	It("admits the three-site lab shape under the default layout", func() {
		// The golden topology, and the proof that the rule INSTALLED: if the
		// API server had rejected the CEL expression the CRD would carry no
		// SlapdMeshSpec schema at all and this create would fail.
		ns := meshAdmissionNamespace(ctx)
		Expect(k8sClient.Create(ctx, meshWithSites(ns, nil, 0, 1, 2))).To(Succeed())
	})

	It("admits an index one below the default maxSites", func() {
		ns := meshAdmissionNamespace(ctx)
		Expect(k8sClient.Create(ctx, meshWithSites(ns, nil, 0, 11))).To(Succeed(),
			"index 11 is the last slot of a 12-wide remote block; the bound is STRICTLY below maxSites, "+
				"so refusing 11 would cost the mesh a site it has every right to")
	})

	It("refuses an index at the default maxSites", func() {
		ns := meshAdmissionNamespace(ctx)
		err := k8sClient.Create(ctx, meshWithSites(ns, nil, 0, 12))
		Expect(err).To(HaveOccurred(),
			"serverIDIndex 12 with the default maxSites 12 has no slot in the remote block: "+
				"every peer stanza naming that site would need a RID the layout cannot produce")
		Expect(err.Error()).To(ContainSubstring("maxSites"),
			"the refusal must name the field a human can widen, not merely say the object is invalid")
	})

	It("refuses an index at a NARROWED maxSites that the default would admit", func() {
		// The case that makes this a per-object rule rather than a constant:
		// index 5 is unremarkable and legal under the defaults, and illegal
		// here only because this mesh declared a 4-wide remote block.
		ns := meshAdmissionNamespace(ctx)
		err := k8sClient.Create(ctx, meshWithSites(ns,
			&ldapv1alpha1.RIDLayout{PodsPerSite: 8, MaxSites: 4}, 0, 5))
		Expect(err).To(HaveOccurred(),
			"a mesh that narrows maxSites to 4 admits site indices 0..3 only")
	})

	It("admits a WIDENED maxSites that the default would refuse", func() {
		// The other direction, and the reason the old constant had to go:
		// widening the layout is a legitimate (if renumbering) operation, and a
		// hardcoded 12 would have made a 16-site mesh unexpressible.
		ns := meshAdmissionNamespace(ctx)
		Expect(k8sClient.Create(ctx, meshWithSites(ns,
			&ldapv1alpha1.RIDLayout{PodsPerSite: 8, MaxSites: 16}, 0, 14))).To(Succeed())
	})

	It("keeps slapd's own ceiling of 255 above the layout's", func() {
		// ADR-028's original bound survives as a hard ceiling, moved by the
		// hex stride: olcServerID caps at 4095 and the stride is 16, so index
		// 255 is the last whole stride slot whatever the RID layout says. A
		// layout wide enough to admit 256 must still not get it.
		ns := meshAdmissionNamespace(ctx)
		err := k8sClient.Create(ctx, meshWithSites(ns,
			&ldapv1alpha1.RIDLayout{PodsPerSite: 8, MaxSites: 300}, 0, 256))
		Expect(err).To(HaveOccurred(),
			"index 256 is past slapd's serverID stride ceiling even though this layout has a slot for it")
	})

	It("refuses a podsPerSite that the serverID stride cannot hold", func() {
		// The other half of the 2026-10-05 amendment: the RID matrix would
		// take podsPerSite 16 happily, but a site's pod ordinal 15 would then
		// take the serverID that is the next site's base, and nothing on the
		// wire detects two pods writing under one sid.
		ns := meshAdmissionNamespace(ctx)
		err := k8sClient.Create(ctx, meshWithSites(ns,
			&ldapv1alpha1.RIDLayout{PodsPerSite: 16, MaxSites: 12}, 0, 1))
		Expect(err).To(HaveOccurred(),
			"podsPerSite 16 overruns the serverID stride of 16 at pod ordinal 15")
	})
})
