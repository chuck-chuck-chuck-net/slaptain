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

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The backup Job of a SINGLE-REPLICA mesh member must carry /accesslog
// (MESH-PLAN Phase 6a; validated here, MESH-PLAN Phase 6).
//
// Why this is a real failure and not a cosmetic one: the Job's init container
// runs `slapcat -F /config/slapd.d`, which loads the WHOLE cn=config and
// validates every olcDbDirectory in it — the accesslog DB's included, even
// though only the data DB is dumped. A missing /accesslog therefore aborts the
// backup at config load, not at write time.
//
// Why the mesh is what exposes it. The mount is gated on
// NeedsAccesslogVolume(), which reads `Replicas > 1 || len(ExternalPeers) > 0`.
// A mesh-driven cluster hand-writes NEITHER: the peers are derived from the
// SlapdMesh plus the operator's own SITE_NAME, in memory, and are deliberately
// never written back to the spec (ADR-028 §4) — so every consumer has to
// resolve meshRef for itself, and one that forgets sees an UNWIRED cluster.
// At replicas > 1 the first clause hides the mistake; at replicas: 1 — an
// ordinary site that relies on its peers rather than on local HA — nothing
// does. Hence the size in this test's name is load-bearing.
//
// The subject is the pair the SlapdBackup reconciler actually runs:
// resolveMeshWiring (which the reconciler calls straight after fetching the
// cluster) and then buildBackupJob. Only the mesh Get is served from a fake
// client; the decision under test is production's.
//
// Green from birth — the defect it guards was fixed in Phase 6a, so there was
// no honest red to observe. Its teeth were shown by mutation instead; see the
// note at the bottom of this file.
func TestBackupJobMountsAccesslogOnSingleReplicaMeshMember(t *testing.T) {
	idx := func(i int32) *int32 { return &i }

	// A three-site mesh, indices 0/1/2 → serverID decades 0/100/200, which is
	// the lab's shape and what tests/e2e.sh generates.
	mesh := &ldapv1alpha1.SlapdMesh{
		ObjectMeta: metav1.ObjectMeta{Name: "slapd-mesh", Namespace: "ns"},
		Spec: ldapv1alpha1.SlapdMeshSpec{
			Sites: []ldapv1alpha1.MeshSite{
				{Name: "site-1", ServerIDIndex: idx(0)},
				{Name: "site-2", ServerIDIndex: idx(1)},
				{Name: "site-3", ServerIDIndex: idx(2)},
			},
			Network: &ldapv1alpha1.ReplicationNetworkConfig{Mode: "pod-routed"},
		},
	}

	tests := []struct {
		name     string
		cluster  *ldapv1alpha1.SlapdCluster
		identity string
		want     bool
		why      string
	}{
		{
			// The case the whole file exists for.
			name: "single replica, peers derived from a mesh",
			cluster: &ldapv1alpha1.SlapdCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "slapd", Namespace: "ns"},
				Spec: ldapv1alpha1.SlapdClusterSpec{
					Replicas:    1,
					MeshRef:     "slapd-mesh",
					Replication: ldapv1alpha1.SlapdReplicationConfig{Enabled: true},
				},
			},
			identity: "site-2",
			want:     true,
			why: "site-2 has two derived peers, so its cn=config carries an " +
				"accesslog DB and slapcat validates that DB's olcDbDirectory",
		},
		{
			// The control, and the reason the assertion above is not vacuous:
			// the same single-replica cluster WITHOUT a mesh genuinely has no
			// accesslog DB, so a Job that mounted /accesslog unconditionally
			// would pass the case above for the wrong reason.
			name: "single replica, no mesh and no peers",
			cluster: &ldapv1alpha1.SlapdCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "slapd", Namespace: "ns"},
				Spec: ldapv1alpha1.SlapdClusterSpec{
					Replicas:    1,
					Replication: ldapv1alpha1.SlapdReplicationConfig{Enabled: true},
				},
			},
			identity: "site-2",
			want:     false,
			why:      "nothing replicates: no local peer, no mesh, no accesslog DB",
		},
		{
			// A mesh whose sites the cluster spans only in part still derives
			// peers, and one peer is enough.
			name: "single replica, mesh narrowed by a site selector",
			cluster: &ldapv1alpha1.SlapdCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "slapd", Namespace: "ns"},
				Spec: ldapv1alpha1.SlapdClusterSpec{
					Replicas:    1,
					MeshRef:     "slapd-mesh",
					Sites:       []string{"site-1", "site-2"},
					Replication: ldapv1alpha1.SlapdReplicationConfig{Enabled: true},
				},
			},
			identity: "site-1",
			want:     true,
			why:      "one derived peer (site-2) is still a replicated database",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fake.NewClientBuilder().
				WithScheme(meshTestScheme(t)).
				WithObjects(mesh).
				Build()

			sc := tt.cluster.DeepCopy()
			// Exactly what SlapdBackupReconciler.Reconcile does after its Get.
			if err := resolveMeshWiring(context.Background(), c, sc, tt.identity); err != nil {
				t.Fatalf("resolveMeshWiring: %v", err)
			}

			job := buildBackupJob(
				&ldapv1alpha1.SlapdBackup{
					ObjectMeta: metav1.ObjectMeta{Name: "bk", Namespace: "ns"},
					Spec: ldapv1alpha1.SlapdBackupSpec{
						DatabaseRef: "example-db",
						Storage: ldapv1alpha1.S3StorageSpec{
							Bucket:                "b",
							CredentialsSecretName: "s3",
						},
					},
				},
				&ldapv1alpha1.SlapdDatabase{
					ObjectMeta: metav1.ObjectMeta{Name: "example-db", Namespace: "ns"},
					Spec:       ldapv1alpha1.SlapdDatabaseSpec{Suffix: "dc=example,dc=org"},
				},
				sc, "init:img", "operator:img", "key",
			)

			spec := job.Spec.Template.Spec
			gotMount := hasMountPath(spec.InitContainers[0].VolumeMounts, ldapv1alpha1.AccesslogRoot)
			if gotMount != tt.want {
				t.Errorf("slapcat container %s mount = %v, want %v — %s",
					ldapv1alpha1.AccesslogRoot, gotMount, tt.want, tt.why)
			}

			// A mount without its volume is an unschedulable pod, so the two
			// are asserted together rather than trusting them to stay in step.
			gotVolume := hasVolumeNamed(spec.Volumes, "accesslog")
			if gotVolume != tt.want {
				t.Errorf("pod volume \"accesslog\" = %v, want %v — %s", gotVolume, tt.want, tt.why)
			}
			if gotMount && gotVolume {
				if claim := pvcClaimOf(spec.Volumes, "accesslog"); claim != "accesslog-slapd-0" {
					t.Errorf("accesslog volume claims %q, want %q (the Job co-locates with pod-0)",
						claim, "accesslog-slapd-0")
				}
			}
		})
	}
}

// meshTestScheme builds a scheme carrying the slaptain API group, which the
// fake client needs to serve the SlapdMesh Get.
func meshTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := ldapv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add slaptain scheme: %v", err)
	}
	return s
}

func hasMountPath(mounts []corev1.VolumeMount, path string) bool {
	for _, m := range mounts {
		if m.MountPath == path {
			return true
		}
	}
	return false
}

func hasVolumeNamed(volumes []corev1.Volume, name string) bool {
	for _, v := range volumes {
		if v.Name == name {
			return true
		}
	}
	return false
}

func pvcClaimOf(volumes []corev1.Volume, name string) string {
	for _, v := range volumes {
		if v.Name == name && v.PersistentVolumeClaim != nil {
			return v.PersistentVolumeClaim.ClaimName
		}
	}
	return ""
}

// Mutation checks run 2026-09-17, each reverted immediately (Test Discipline:
// a test that was green from birth must be shown to have teeth):
//
//  1. NeedsAccesslogVolume's last line changed to `return sc.Spec.Replicas > 1`
//     — the pre-mesh gate. The mesh cases went red on both the mount and the
//     volume; the no-mesh control stayed green. This is the defect's exact
//     shape.
//  2. applyMeshWiring's `sc.Spec.Replication.ExternalPeers = w.ExternalPeers`
//     removed — the "a consumer forgot to resolve" shape. Same two cases red,
//     control green.
//
// Both mutations turn the two mesh cases red and leave the control green, which
// is what distinguishes this from a test that would pass on anything.
