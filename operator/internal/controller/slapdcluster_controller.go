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
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

const (
	ldapContainerPort  = int32(1024)
	ldapsContainerPort = int32(1025)
	fieldManager       = "slapdcluster-controller"

	// ldapRequestTimeout caps every LDAP operation (Bind, Search, Add, Modify)
	// so that a hung or deadlocked slapd cannot block the reconcile loop forever.
	ldapRequestTimeout = 10 * time.Second
)

// SlapdClusterReconciler reconciles a SlapdCluster object.
// It manages infrastructure only: StatefulSet, Services, cn=config credentials.
// Database, schema, and ACL management are handled by SlapdDatabase and
// SlapdSchema controllers. See ADR-004.
type SlapdClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapdclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapdclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapdclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *SlapdClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// 1. Fetch the SlapdCluster resource.
	sc := &ldapv1alpha1.SlapdCluster{}
	if err := r.Get(ctx, req.NamespacedName, sc); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 2. Reconcile cn=config credential secret (<name>-config-password).
	if err := r.reconcileSecret(ctx, sc); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcileSecret: %w", err)
	}

	// 3. Reconcile headless Service.
	if err := r.reconcileHeadlessService(ctx, sc); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcileHeadlessService: %w", err)
	}

	// 4. Reconcile ClusterIP Service.
	if err := r.reconcileClusterIPService(ctx, sc); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcileClusterIPService: %w", err)
	}

	// Query SlapdDatabase CRs referencing this cluster. Their names are passed
	// to the init container so it can create per-database data directories.
	databaseNames, err := r.listDatabaseNames(ctx, sc)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listDatabaseNames: %w", err)
	}

	// 5. Reconcile StatefulSet.
	if err := r.reconcileStatefulSet(ctx, sc, databaseNames); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcileStatefulSet: %w", err)
	}

	// 5a. Reconcile read-only headless Service.
	if err := r.reconcileReadOnlyHeadlessService(ctx, sc); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcileReadOnlyHeadlessService: %w", err)
	}

	// 5b. Reconcile read-only ClusterIP Service.
	if err := r.reconcileReadOnlyService(ctx, sc); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcileReadOnlyService: %w", err)
	}

	// 5c. Reconcile read-only StatefulSet.
	if err := r.reconcileReadOnlyStatefulSet(ctx, sc, databaseNames); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcileReadOnlyStatefulSet: %w", err)
	}

	// NOTE: Steps 6-7b (bootstrap, ACLs, schemas, replication) have been removed
	// from SlapdCluster. They are now managed by SlapdDatabase and SlapdSchema
	// controllers. See ADR-004.

	// 8. Observe StatefulSet status → update SlapdCluster status.
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, req.NamespacedName, sts); err != nil {
		return ctrl.Result{}, fmt.Errorf("get StatefulSet: %w", err)
	}

	desired := sc.Spec.Replicas
	if desired == 0 {
		desired = 1
	}
	ready := sts.Status.ReadyReplicas

	sc.Status.Replicas = sts.Status.Replicas
	sc.Status.ReadyReplicas = ready
	sc.Status.ObservedGeneration = sc.Generation

	// Read-only StatefulSet status (informational only — does not affect phase).
	if sc.Spec.ReadReplicas > 0 {
		roSts := &appsv1.StatefulSet{}
		roKey := client.ObjectKey{Name: sc.Name + "-readonly", Namespace: sc.Namespace}
		if err := r.Get(ctx, roKey, roSts); err != nil {
			if !errors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("get read-only StatefulSet: %w", err)
			}
		} else {
			sc.Status.ReadOnlyReplicas = roSts.Status.Replicas
			sc.Status.ReadOnlyReadyReplicas = roSts.Status.ReadyReplicas
		}
	} else {
		sc.Status.ReadOnlyReplicas = 0
		sc.Status.ReadOnlyReadyReplicas = 0
	}

	// External peer connectivity status.
	sc.Status.ExternalPeerStatuses = nil
	for _, ep := range sc.Spec.Replication.ExternalPeers {
		status := ldapv1alpha1.ExternalPeerStatus{
			Name: ep.Name,
		}
		if err := testExternalPeerConnectivity(ep.URI); err != nil {
			status.Connected = false
			status.LastError = err.Error()
		} else {
			status.Connected = true
		}
		sc.Status.ExternalPeerStatuses = append(sc.Status.ExternalPeerStatuses, status)
	}

	switch {
	case ready == 0:
		sc.Status.Phase = ldapv1alpha1.PhaseBootstrapping
	case ready < desired:
		sc.Status.Phase = ldapv1alpha1.PhaseDegraded
	default:
		sc.Status.Phase = ldapv1alpha1.PhaseRunning
	}

	readyStatus := metav1.ConditionFalse
	readyReason := "NotReady"
	readyMsg := fmt.Sprintf("%d/%d replicas ready", ready, desired)
	if ready >= desired {
		readyStatus = metav1.ConditionTrue
		readyReason = "AllReplicasReady"
	}
	setCondition(&sc.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             readyStatus,
		Reason:             readyReason,
		Message:            readyMsg,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: sc.Generation,
	})

	// SSA patch on the status subresource: no resourceVersion check, no conflict possible.
	statusPatch := &ldapv1alpha1.SlapdCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "ldap.chuck-chuck-chuck.net/v1alpha1",
			Kind:       "SlapdCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      sc.Name,
			Namespace: sc.Namespace,
		},
	}
	statusPatch.Status = sc.Status
	if err := r.Status().Patch(ctx, statusPatch, client.Apply, client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, err
	}

	// Requeue until fully Running.
	if sc.Status.Phase != ldapv1alpha1.PhaseRunning {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// reconcileSecret creates the cn=config admin credential secret (create-only):
//   - <name>-config-password  root-password
//
// If spec.ldap.cnConfigCredentials.secretName is set, the operator uses that
// secret directly. Otherwise, a password is auto-generated.
func (r *SlapdClusterReconciler) reconcileSecret(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) error {
	cfgName := sc.Name + "-config-password"

	// If the user provided a cnConfigCredentials secret, nothing to create.
	if sc.Spec.LDAP.CnConfigCredentials.SecretName != "" {
		return nil
	}

	// If auto-generated secret already exists, nothing to do.
	existing := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Name: cfgName, Namespace: sc.Namespace}, existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}

	rootPW, err := generatePassword(24)
	if err != nil {
		return fmt.Errorf("generate root password: %w", err)
	}

	cfgSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: cfgName, Namespace: sc.Namespace},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"root-password": rootPW,
		},
	}
	if err := controllerutil.SetControllerReference(sc, cfgSecret, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, cfgSecret)
}

// reconcileHeadlessService applies the headless Service (clusterIP: None) via SSA.
// Named <name>-headless so the bare <name> can be used for the ClusterIP service.
func (r *SlapdClusterReconciler) reconcileHeadlessService(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) error {
	svc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      sc.Name + "-headless",
			Namespace: sc.Namespace,
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector:  selectorLabels(sc.Name),
			Ports: []corev1.ServicePort{
				{
					Name:       "ldap",
					Port:       ldapContainerPort,
					TargetPort: intstr.FromString("ldap"),
					Protocol:   corev1.ProtocolTCP,
				},
				{
					Name:       "ldaps",
					Port:       ldapsContainerPort,
					TargetPort: intstr.FromString("ldaps"),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(sc, svc, r.Scheme); err != nil {
		return err
	}
	return r.Patch(ctx, svc, client.Apply, client.ForceOwnership, client.FieldOwner(fieldManager))
}

// reconcileClusterIPService applies the ClusterIP Service via SSA.
func (r *SlapdClusterReconciler) reconcileClusterIPService(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) error {
	svcType := sc.Spec.Service.Type
	if svcType == "" {
		svcType = corev1.ServiceTypeClusterIP
	}
	ldapPort := sc.Spec.Service.LDAPPort
	if ldapPort == 0 {
		ldapPort = 389
	}
	ldapsPort := sc.Spec.Service.LDAPSPort
	if ldapsPort == 0 {
		ldapsPort = 636
	}

	svc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      sc.Name,
			Namespace: sc.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Type:     svcType,
			Selector: selectorLabels(sc.Name),
			Ports: []corev1.ServicePort{
				{
					Name:       "ldap",
					Port:       ldapPort,
					TargetPort: intstr.FromString("ldap"),
					Protocol:   corev1.ProtocolTCP,
				},
				{
					Name:       "ldaps",
					Port:       ldapsPort,
					TargetPort: intstr.FromString("ldaps"),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(sc, svc, r.Scheme); err != nil {
		return err
	}
	return r.Patch(ctx, svc, client.Apply, client.ForceOwnership, client.FieldOwner(fieldManager))
}

// reconcileStatefulSet applies the StatefulSet via SSA.
func (r *SlapdClusterReconciler) reconcileStatefulSet(ctx context.Context, sc *ldapv1alpha1.SlapdCluster, databaseNames []string) error {
	sts := &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      sc.Name,
			Namespace: sc.Namespace,
		},
		Spec: r.buildStatefulSetSpec(sc, false, databaseNames),
	}
	if err := controllerutil.SetControllerReference(sc, sts, r.Scheme); err != nil {
		return err
	}
	return r.Patch(ctx, sts, client.Apply, client.ForceOwnership, client.FieldOwner(fieldManager))
}

// reconcileReadOnlyHeadlessService applies the read-only headless Service via SSA.
// Skipped when spec.readReplicas == 0.
func (r *SlapdClusterReconciler) reconcileReadOnlyHeadlessService(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) error {
	if sc.Spec.ReadReplicas == 0 {
		return nil
	}
	svc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      sc.Name + "-readonly-headless",
			Namespace: sc.Namespace,
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector:  readOnlySelectorLabels(sc.Name),
			Ports: []corev1.ServicePort{
				{
					Name:       "ldap",
					Port:       ldapContainerPort,
					TargetPort: intstr.FromString("ldap"),
					Protocol:   corev1.ProtocolTCP,
				},
				{
					Name:       "ldaps",
					Port:       ldapsContainerPort,
					TargetPort: intstr.FromString("ldaps"),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(sc, svc, r.Scheme); err != nil {
		return err
	}
	return r.Patch(ctx, svc, client.Apply, client.ForceOwnership, client.FieldOwner(fieldManager))
}

// reconcileReadOnlyService applies the read-only ClusterIP Service via SSA.
// Skipped when spec.readReplicas == 0.
func (r *SlapdClusterReconciler) reconcileReadOnlyService(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) error {
	if sc.Spec.ReadReplicas == 0 {
		return nil
	}
	svcType := sc.Spec.Service.Type
	if svcType == "" {
		svcType = corev1.ServiceTypeClusterIP
	}
	ldapPort := sc.Spec.Service.LDAPPort
	if ldapPort == 0 {
		ldapPort = 389
	}
	ldapsPort := sc.Spec.Service.LDAPSPort
	if ldapsPort == 0 {
		ldapsPort = 636
	}

	svc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      sc.Name + "-readonly",
			Namespace: sc.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Type:     svcType,
			Selector: readOnlySelectorLabels(sc.Name),
			Ports: []corev1.ServicePort{
				{
					Name:       "ldap",
					Port:       ldapPort,
					TargetPort: intstr.FromString("ldap"),
					Protocol:   corev1.ProtocolTCP,
				},
				{
					Name:       "ldaps",
					Port:       ldapsPort,
					TargetPort: intstr.FromString("ldaps"),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(sc, svc, r.Scheme); err != nil {
		return err
	}
	return r.Patch(ctx, svc, client.Apply, client.ForceOwnership, client.FieldOwner(fieldManager))
}

// reconcileReadOnlyStatefulSet applies the read-only StatefulSet via SSA.
// Skipped when spec.readReplicas == 0.
func (r *SlapdClusterReconciler) reconcileReadOnlyStatefulSet(ctx context.Context, sc *ldapv1alpha1.SlapdCluster, databaseNames []string) error {
	if sc.Spec.ReadReplicas == 0 {
		return nil
	}
	sts := &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      sc.Name + "-readonly",
			Namespace: sc.Namespace,
		},
		Spec: r.buildStatefulSetSpec(sc, true, databaseNames),
	}
	if err := controllerutil.SetControllerReference(sc, sts, r.Scheme); err != nil {
		return err
	}
	return r.Patch(ctx, sts, client.Apply, client.ForceOwnership, client.FieldOwner(fieldManager))
}

// buildStatefulSetSpec constructs the StatefulSet spec for RW or RO replicas.
// With the multi-resource architecture (ADR-004), the init container only sets up
// cn=config infrastructure (modules, TLS). Data databases, schemas, ACLs, and
// replication are managed by SlapdDatabase and SlapdSchema controllers.
func (r *SlapdClusterReconciler) buildStatefulSetSpec(sc *ldapv1alpha1.SlapdCluster, readOnly bool, databaseNames []string) appsv1.StatefulSetSpec {
	var labels map[string]string
	var replicas int32
	var serviceName string

	if readOnly {
		labels = readOnlySelectorLabels(sc.Name)
		replicas = sc.Spec.ReadReplicas
		serviceName = sc.Name + "-readonly-headless"
	} else {
		labels = selectorLabels(sc.Name)
		replicas = sc.Spec.Replicas
		if replicas == 0 {
			replicas = 1
		}
		serviceName = sc.Name + "-headless"
	}

	logLevel := strconv.Itoa(int(sc.Spec.LogLevel))
	replicationEnabled := sc.Spec.Replication.Enabled && sc.Spec.Replicas > 1

	// Pod security context.
	podSecCtx := sc.Spec.SecurityContext
	if podSecCtx == nil {
		uid := int64(1024)
		gid := int64(1024)
		podSecCtx = &corev1.PodSecurityContext{
			RunAsUser:  &uid,
			RunAsGroup: &gid,
			FSGroup:    &gid,
		}
	}

	// ── Volumes (non-PVC) ─────────────────────────────────────────────────────
	volumes := []corev1.Volume{
		{
			Name:         "run",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
	}

	var volumeClaimTemplates []corev1.PersistentVolumeClaim

	if sc.Spec.Persistence.Enabled {
		cfgSize := sc.Spec.Persistence.Config.Size
		if cfgSize == "" {
			cfgSize = "1Gi"
		}
		dataSize := sc.Spec.Persistence.Data.Size
		if dataSize == "" {
			dataSize = "5Gi"
		}
		cfgAM := sc.Spec.Persistence.Config.AccessMode
		if cfgAM == "" {
			cfgAM = corev1.ReadWriteOnce
		}
		dataAM := sc.Spec.Persistence.Data.AccessMode
		if dataAM == "" {
			dataAM = corev1.ReadWriteOnce
		}
		volumeClaimTemplates = []corev1.PersistentVolumeClaim{
			pvcTemplate("config", cfgSize, sc.Spec.Persistence.Config.StorageClass, cfgAM),
			pvcTemplate("data", dataSize, sc.Spec.Persistence.Data.StorageClass, dataAM),
		}
		// Accesslog PVC only for RW replicas with replication enabled.
		if replicationEnabled && !readOnly {
			accesslogSize := sc.Spec.Persistence.Accesslog.Size
			if accesslogSize == "" {
				accesslogSize = "1Gi"
			}
			accesslogAM := sc.Spec.Persistence.Accesslog.AccessMode
			if accesslogAM == "" {
				accesslogAM = corev1.ReadWriteOnce
			}
			volumeClaimTemplates = append(volumeClaimTemplates,
				pvcTemplate("accesslog", accesslogSize, sc.Spec.Persistence.Accesslog.StorageClass, accesslogAM),
			)
		}
	} else {
		volumes = append(volumes,
			corev1.Volume{
				Name:         "config",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			},
			corev1.Volume{
				Name:         "data",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			},
		)
		if replicationEnabled && !readOnly {
			volumes = append(volumes, corev1.Volume{
				Name:         "accesslog",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			})
		}
	}

	if sc.Spec.LDAP.TLS.Enabled {
		volumes = append(volumes, corev1.Volume{
			Name: "tls",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: sc.Spec.LDAP.TLS.SecretName,
				},
			},
		})
	}

	// External peer TLS CA cert volumes.
	if !readOnly {
		for _, peer := range sc.Spec.Replication.ExternalPeers {
			if peer.TLSSecretName == "" {
				continue
			}
			volumes = append(volumes, corev1.Volume{
				Name: "peer-tls-" + peer.Name,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: peer.TLSSecretName,
					},
				},
			})
		}
	}

	// ── Resolve config password secret name ──────────────────────────────────
	configSecretName := sc.Name + "-config-password"
	if sc.Spec.LDAP.CnConfigCredentials.SecretName != "" {
		configSecretName = sc.Spec.LDAP.CnConfigCredentials.SecretName
	}

	// ── Init container ────────────────────────────────────────────────────────
	// The init container sets up cn=config infrastructure only (modules, TLS).
	// Data databases are created by the SlapdDatabase controller at runtime.
	initEnv := []corev1.EnvVar{
		{
			Name: "LDAP_ROOT_PW",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: configSecretName},
					Key:                  "root-password",
				},
			},
		},
		{Name: "LDAP_TLS_ENABLED", Value: strconv.FormatBool(sc.Spec.LDAP.TLS.Enabled)},
		{Name: "FORCE_REBOOTSTRAP", Value: strconv.FormatBool(sc.Spec.LDAP.ForceRebootstrap)},
		{Name: "CONFIG_DIR", Value: "/config"},
		{Name: "DATA_DIR", Value: "/data"},
		{Name: "ACCESSLOG_DIR", Value: "/accesslog"},
		{Name: "DATABASE_DIRS", Value: strings.Join(databaseNames, ",")},
	}

	if sc.Spec.LDAP.TLS.Enabled {
		initEnv = append(initEnv,
			corev1.EnvVar{Name: "LDAP_TLS_CACERT_PATH", Value: "/etc/openldap/tls/ca.crt"},
			corev1.EnvVar{Name: "LDAP_TLS_CERT_PATH", Value: "/etc/openldap/tls/tls.crt"},
			corev1.EnvVar{Name: "LDAP_TLS_KEY_PATH", Value: "/etc/openldap/tls/tls.key"},
		)
	}

	if readOnly {
		initEnv = append(initEnv,
			corev1.EnvVar{Name: "LDAP_READONLY_REPLICA", Value: "true"},
			corev1.EnvVar{Name: "LDAP_REPLICATION_ENABLED", Value: "true"},
		)
	} else if replicationEnabled {
		initEnv = append(initEnv,
			corev1.EnvVar{Name: "LDAP_REPLICATION_ENABLED", Value: "true"},
		)
	}

	initMounts := []corev1.VolumeMount{
		{Name: "config", MountPath: "/config"},
		{Name: "data", MountPath: "/data"},
	}
	if replicationEnabled && !readOnly {
		initMounts = append(initMounts, corev1.VolumeMount{
			Name:      "accesslog",
			MountPath: "/accesslog",
		})
	}
	if sc.Spec.LDAP.TLS.Enabled {
		initMounts = append(initMounts, corev1.VolumeMount{
			Name:      "tls",
			MountPath: "/etc/openldap/tls",
			ReadOnly:  true,
		})
	}

	initImage := sc.Spec.Images.Init.Repository
	if sc.Spec.Images.Init.Tag != "" {
		initImage += ":" + sc.Spec.Images.Init.Tag
	}

	initContainer := corev1.Container{
		Name:            "init",
		Image:           initImage,
		ImagePullPolicy: sc.Spec.Images.Init.PullPolicy,
		Env:             initEnv,
		VolumeMounts:    initMounts,
	}

	// ── Main container ────────────────────────────────────────────────────────
	mainMounts := []corev1.VolumeMount{
		{Name: "config", MountPath: "/config"},
		{Name: "data", MountPath: "/data"},
		{Name: "run", MountPath: "/run/openldap"},
	}
	if replicationEnabled && !readOnly {
		mainMounts = append(mainMounts, corev1.VolumeMount{
			Name:      "accesslog",
			MountPath: "/accesslog",
		})
	}
	if sc.Spec.LDAP.TLS.Enabled {
		mainMounts = append(mainMounts, corev1.VolumeMount{
			Name:      "tls",
			MountPath: "/etc/openldap/tls",
			ReadOnly:  true,
		})
	}

	// External peer TLS CA cert volume mounts.
	if !readOnly {
		for _, peer := range sc.Spec.Replication.ExternalPeers {
			if peer.TLSSecretName == "" {
				continue
			}
			mainMounts = append(mainMounts, corev1.VolumeMount{
				Name:      "peer-tls-" + peer.Name,
				MountPath: "/etc/openldap/tls/peers/" + peer.Name,
				ReadOnly:  true,
			})
		}
	}

	falseVal := false
	trueVal := true
	mainSecCtx := &corev1.SecurityContext{
		AllowPrivilegeEscalation: &falseVal,
		ReadOnlyRootFilesystem:   &trueVal,
	}

	mainImage := sc.Spec.Images.Slapd.Repository
	if sc.Spec.Images.Slapd.Tag != "" {
		mainImage += ":" + sc.Spec.Images.Slapd.Tag
	}

	mainContainer := corev1.Container{
		Name:            "slapd",
		Image:           mainImage,
		ImagePullPolicy: sc.Spec.Images.Slapd.PullPolicy,
		Args: []string{
			"-h", "ldap://:1024/ ldaps://:1025/ ldapi://%2frun%2fopenldap%2fslapd.ldapi",
			"-d", logLevel,
			"-F", "/config",
		},
		Ports: []corev1.ContainerPort{
			{Name: "ldap", ContainerPort: ldapContainerPort, Protocol: corev1.ProtocolTCP},
			{Name: "ldaps", ContainerPort: ldapsContainerPort, Protocol: corev1.ProtocolTCP},
		},
		VolumeMounts:    mainMounts,
		SecurityContext: mainSecCtx,
		Resources:       sc.Spec.Resources,
	}

	spec := appsv1.StatefulSetSpec{
		ServiceName: serviceName,
		Replicas:    &replicas,
		Selector: &metav1.LabelSelector{
			MatchLabels: labels,
		},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: labels,
			},
			Spec: corev1.PodSpec{
				SecurityContext:  podSecCtx,
				ImagePullSecrets: sc.Spec.ImagePullSecrets,
				InitContainers:   []corev1.Container{initContainer},
				Containers:       []corev1.Container{mainContainer},
				Volumes:          volumes,
			},
		},
		VolumeClaimTemplates: volumeClaimTemplates,
	}

	return spec
}

// getConfigPassword reads the plaintext root (cn=config admin) password.
func (r *SlapdClusterReconciler) getConfigPassword(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) (string, error) {
	secretName := sc.Name + "-config-password"
	if sc.Spec.LDAP.CnConfigCredentials.SecretName != "" {
		secretName = sc.Spec.LDAP.CnConfigCredentials.SecretName
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{
		Name: secretName, Namespace: sc.Namespace,
	}, secret); err != nil {
		return "", fmt.Errorf("read config password secret %s: %w", secretName, err)
	}
	pw := string(secret.Data["root-password"])
	if pw == "" {
		return "", fmt.Errorf("secret %s is missing root-password key", secretName)
	}
	return pw, nil
}

// listDatabaseNames returns the names of all SlapdDatabase CRs that reference
// this cluster. These names become data subdirectories under /data/.
func (r *SlapdClusterReconciler) listDatabaseNames(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) ([]string, error) {
	var dbList ldapv1alpha1.SlapdDatabaseList
	if err := r.List(ctx, &dbList, client.InNamespace(sc.Namespace)); err != nil {
		return nil, err
	}
	var names []string
	for _, db := range dbList.Items {
		if db.Spec.ClusterRef == sc.Name {
			names = append(names, db.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// SetupWithManager sets up the controller with the Manager.
// Watches SlapdDatabase CRs to trigger reconciliation when databases are
// added or removed — the StatefulSet's init container env (DATABASE_DIRS)
// must be updated so the init container creates per-database data directories.
func (r *SlapdClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ldapv1alpha1.SlapdCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Watches(&ldapv1alpha1.SlapdDatabase{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, obj client.Object) []ctrl.Request {
				db, ok := obj.(*ldapv1alpha1.SlapdDatabase)
				if !ok {
					return nil
				}
				return []ctrl.Request{{
					NamespacedName: client.ObjectKey{
						Name:      db.Spec.ClusterRef,
						Namespace: db.Namespace,
					},
				}}
			},
		)).
		Named("slapdcluster").
		Complete(r)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// selectorLabels returns the standard pod selector labels.
func selectorLabels(name string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "slapd",
		"app.kubernetes.io/instance": name,
	}
}

// readOnlySelectorLabels returns the pod selector labels for read-only replicas.
func readOnlySelectorLabels(name string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "slapd",
		"app.kubernetes.io/instance": name + "-readonly",
	}
}

// setCondition upserts a condition in the conditions slice.
func setCondition(conditions *[]metav1.Condition, newCond metav1.Condition) {
	for i, c := range *conditions {
		if c.Type == newCond.Type {
			(*conditions)[i] = newCond
			return
		}
	}
	*conditions = append(*conditions, newCond)
}

// pvcTemplate returns a PVC for use in StatefulSet volumeClaimTemplates.
func pvcTemplate(name, size, storageClass string, accessMode corev1.PersistentVolumeAccessMode) corev1.PersistentVolumeClaim {
	qty := resource.MustParse(size)
	pvc := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{accessMode},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: qty,
				},
			},
		},
	}
	if storageClass != "" {
		pvc.Spec.StorageClassName = &storageClass
	}
	return pvc
}

// generatePassword returns a URL-safe base64-encoded random password.
func generatePassword(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

// isPodReady returns true when all containers in the pod report Ready.
func isPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// testExternalPeerConnectivity attempts a TLS/TCP connection to the external peer URI
// to check basic network reachability. Returns nil on success.
func testExternalPeerConnectivity(uri string) error {
	addr := uri
	useTLS := false
	if strings.HasPrefix(uri, "ldaps://") {
		addr = strings.TrimPrefix(uri, "ldaps://")
		useTLS = true
	} else if strings.HasPrefix(uri, "ldap://") {
		addr = strings.TrimPrefix(uri, "ldap://")
	}
	addr = strings.TrimRight(addr, "/")

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	if useTLS {
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // connectivity check only
		})
		if err != nil {
			return err
		}
		conn.Close()
		return nil
	}
	conn, err := dialer.Dial("tcp", addr)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}
