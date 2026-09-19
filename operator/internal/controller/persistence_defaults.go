package controller

import (
	corev1 "k8s.io/api/core/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// PVC defaults. THIS IS THE ONLY PLACE THEY LIVE.
//
// Deliberately not +kubebuilder:default on SlapdPVCConfig: a CRD default is
// materialised into the stored object at admission, and only when its PARENT
// object is present — so `persistence: {}` would be filled by the API server
// while an omitted `persistence:` would fall through to the controller, and the
// two sources would have to be kept in step forever to agree. Worse, a value
// materialised into a CR is frozen there: changing the default in a later
// release would never reach a cluster created under the old one, while the CR
// gives no hint that nobody chose it. Resolving here, at reconcile time, means
// unset always means "whatever this operator version thinks", which is the same
// rule ADR-024 applies to slapd's own tunables.
//
// The numbers: config holds cn=config and accesslog holds a purged change
// journal, so both stay small. Data is the volume that actually stops a
// database — olcDbMaxSize is an address-space reservation, the PVC is the real
// limit — so it starts higher, while staying far below the 32Gi default map
// size so a cluster runs out of PVC (visible, alertable, expandable) long
// before it runs out of map. See docs/TUNING.md.
const (
	defaultConfigPVCSize    = "1Gi"
	defaultDataPVCSize      = "5Gi"
	defaultAccesslogPVCSize = "1Gi"
)

// resolvePersistence fills each volume's unset fields with slaptain's default.
// Anything the spec states is returned untouched.
func resolvePersistence(p ldapv1alpha1.SlapdPersistenceConfig) (config, data, accesslog ldapv1alpha1.SlapdPVCConfig) {
	return resolvePVC(p.Config, defaultConfigPVCSize),
		resolvePVC(p.Data, defaultDataPVCSize),
		resolvePVC(p.Accesslog, defaultAccesslogPVCSize)
}

func resolvePVC(c ldapv1alpha1.SlapdPVCConfig, defaultSize string) ldapv1alpha1.SlapdPVCConfig {
	if c.Size == "" {
		c.Size = defaultSize
	}
	if c.AccessMode == "" {
		c.AccessMode = corev1.ReadWriteOnce
	}
	// StorageClass stays empty on purpose: empty means the cluster's default
	// StorageClass, which is a property of the cluster and not ours to invent.
	return c
}
