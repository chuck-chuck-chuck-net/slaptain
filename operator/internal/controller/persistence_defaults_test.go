package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The PVC defaults live in exactly ONE place — resolvePersistence — because a
// second copy (a +kubebuilder:default on the CRD, a value written by a Helm
// chart) freezes whatever it says into every object created while it stood, and
// the two then disagree for the lifetime of the cluster.
//
// The numbers themselves: config and accesslog hold configuration and a change
// journal and stay small; data is the volume that actually stops a database, so
// it starts higher — deliberately still far below the 32Gi default LMDB map, so
// a cluster runs out of PVC (visible, alertable, expandable) long before it runs
// out of map. See docs/TUNING.md.
func TestResolvePersistenceDefaults(t *testing.T) {
	const rwo = corev1.ReadWriteOnce

	tests := []struct {
		name                          string
		in                            ldapv1alpha1.SlapdPersistenceConfig
		wantCfg, wantData, wantAccess ldapv1alpha1.SlapdPVCConfig
	}{
		{
			name:       "nothing set: every volume gets slaptain's default",
			in:         ldapv1alpha1.SlapdPersistenceConfig{},
			wantCfg:    ldapv1alpha1.SlapdPVCConfig{Size: "1Gi", AccessMode: rwo},
			wantData:   ldapv1alpha1.SlapdPVCConfig{Size: "5Gi", AccessMode: rwo},
			wantAccess: ldapv1alpha1.SlapdPVCConfig{Size: "1Gi", AccessMode: rwo},
		},
		{
			name: "explicit sizes win over every default",
			in: ldapv1alpha1.SlapdPersistenceConfig{
				Config:    ldapv1alpha1.SlapdPVCConfig{Size: "2Gi"},
				Data:      ldapv1alpha1.SlapdPVCConfig{Size: "500Gi"},
				Accesslog: ldapv1alpha1.SlapdPVCConfig{Size: "20Gi"},
			},
			wantCfg:    ldapv1alpha1.SlapdPVCConfig{Size: "2Gi", AccessMode: rwo},
			wantData:   ldapv1alpha1.SlapdPVCConfig{Size: "500Gi", AccessMode: rwo},
			wantAccess: ldapv1alpha1.SlapdPVCConfig{Size: "20Gi", AccessMode: rwo},
		},
		{
			name: "a partial override defaults only what it omits",
			in: ldapv1alpha1.SlapdPersistenceConfig{
				Data: ldapv1alpha1.SlapdPVCConfig{AccessMode: corev1.ReadWriteMany, StorageClass: "ceph"},
			},
			wantCfg:    ldapv1alpha1.SlapdPVCConfig{Size: "1Gi", AccessMode: rwo},
			wantData:   ldapv1alpha1.SlapdPVCConfig{Size: "5Gi", AccessMode: corev1.ReadWriteMany, StorageClass: "ceph"},
			wantAccess: ldapv1alpha1.SlapdPVCConfig{Size: "1Gi", AccessMode: rwo},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, data, access := resolvePersistence(tc.in)
			if cfg != tc.wantCfg {
				t.Errorf("config: got %+v, want %+v", cfg, tc.wantCfg)
			}
			if data != tc.wantData {
				t.Errorf("data: got %+v, want %+v", data, tc.wantData)
			}
			if access != tc.wantAccess {
				t.Errorf("accesslog: got %+v, want %+v", access, tc.wantAccess)
			}
		})
	}
}
