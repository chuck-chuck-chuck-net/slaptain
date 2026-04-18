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
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
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
	logf "sigs.k8s.io/controller-runtime/pkg/log"

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

	// 2. Reconcile credential secrets (<name>-passwords plaintext + <name>-config-password).
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

	// 5. Reconcile StatefulSet.
	if err := r.reconcileStatefulSet(ctx, sc); err != nil {
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
	if err := r.reconcileReadOnlyStatefulSet(ctx, sc); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcileReadOnlyStatefulSet: %w", err)
	}

	// 6. Bootstrap initial directory entries via LDAP once pod-0 is ready.
	//    Sets sc.Status.BootstrapComplete = true on success; status persisted below.
	if err := r.reconcileBootstrap(ctx, sc); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcileBootstrap: %w", err)
	}

	// 7. Apply spec.ldap.acls to each pod's cn=config (idempotent; unreachable pods
	//    are logged and skipped — the next reconcile will retry them).
	//    Track whether any pod was skipped so we keep requeueing.
	podsSkipped := false

	if skipped, err := r.reconcileACLs(ctx, sc); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcileACLs: %w", err)
	} else if skipped {
		podsSkipped = true
	}

	// 7a. Apply spec.ldap.schemas to each pod's cn=schema,cn=config (idempotent;
	//     DN existence check — existing schemas are skipped, unreachable pods retried).
	if skipped, err := r.reconcileSchemas(ctx, sc); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcileSchemas: %w", err)
	} else if skipped {
		podsSkipped = true
	}

	// 7b. Apply syncrepl + mirrormode to each pod's cn=config data DB entry.
	//     Operator owns all syncrepl configuration (in-cluster + external). See ADR-003.
	if skipped, err := r.reconcileReplication(ctx, sc); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcileReplication: %w", err)
	} else if skipped {
		podsSkipped = true
	}

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

	switch {
	case ready == 0:
		sc.Status.Phase = ldapv1alpha1.PhaseBootstrapping
	case ready < desired:
		sc.Status.Phase = ldapv1alpha1.PhaseDegraded
	case !sc.Status.BootstrapComplete:
		sc.Status.Phase = ldapv1alpha1.PhaseBootstrapping
	case podsSkipped:
		// All replicas are ready and bootstrap is done, but some pods haven't
		// been fully configured yet (ACLs, schemas, or syncrepl stanzas not
		// applied). Stay in Bootstrapping until the next reconcile succeeds
		// on all pods.
		sc.Status.Phase = ldapv1alpha1.PhaseBootstrapping
	default:
		sc.Status.Phase = ldapv1alpha1.PhaseRunning
	}

	readyStatus := metav1.ConditionFalse
	readyReason := "NotReady"
	readyMsg := fmt.Sprintf("%d/%d replicas ready", ready, desired)
	if ready >= desired && sc.Status.BootstrapComplete && !podsSkipped {
		readyStatus = metav1.ConditionTrue
		readyReason = "AllReplicasReady"
	} else if podsSkipped {
		readyReason = "PodsNotConfigured"
		readyMsg = fmt.Sprintf("%d/%d replicas ready, some pods pending configuration", ready, desired)
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

	// 8. Requeue until fully Running and all pods configured.
	if sc.Status.Phase != ldapv1alpha1.PhaseRunning || podsSkipped {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// reconcileSecret creates two plaintext credential secrets (both create-only):
//   - <name>-passwords       admin-password + replication-password
//   - <name>-config-password root-password
//
// If spec.ldap.credentialsSecretName is set the passwords are read from the
// user-provided secret (must contain admin-password and root-password; replication-password
// is optional). Otherwise all passwords are auto-generated.
func (r *SlapdClusterReconciler) reconcileSecret(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) error {
	pwName := sc.Name + "-passwords"
	cfgName := sc.Name + "-config-password"

	// If <name>-passwords already exists, both secrets are already in order.
	existing := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Name: pwName, Namespace: sc.Namespace}, existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}

	// Obtain plaintext passwords.
	var adminPW, rootPW, replPW string

	if sc.Spec.LDAP.CredentialsSecretName != "" {
		creds := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{
			Name:      sc.Spec.LDAP.CredentialsSecretName,
			Namespace: sc.Namespace,
		}, creds); err != nil {
			return fmt.Errorf("read credentialsSecret %s: %w", sc.Spec.LDAP.CredentialsSecretName, err)
		}
		adminPW = string(creds.Data["admin-password"])
		rootPW = string(creds.Data["root-password"])
		if adminPW == "" || rootPW == "" {
			return fmt.Errorf("secret %s must contain admin-password and root-password keys",
				sc.Spec.LDAP.CredentialsSecretName)
		}
		// replication-password is optional in user-provided secret.
		replPW = string(creds.Data["replication-password"])
	} else {
		var err error
		adminPW, err = generatePassword(24)
		if err != nil {
			return fmt.Errorf("generate admin password: %w", err)
		}
		rootPW, err = generatePassword(24)
		if err != nil {
			return fmt.Errorf("generate root password: %w", err)
		}
	}

	if replPW == "" {
		var err error
		replPW, err = generatePassword(32)
		if err != nil {
			return fmt.Errorf("generate replication password: %w", err)
		}
	}

	// Create <name>-passwords (admin-password + replication-password).
	pwSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: pwName, Namespace: sc.Namespace},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"admin-password":       adminPW,
			"replication-password": replPW,
		},
	}
	if err := controllerutil.SetControllerReference(sc, pwSecret, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, pwSecret); err != nil {
		return err
	}

	// Create <name>-config-password (root-password).
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
func (r *SlapdClusterReconciler) reconcileStatefulSet(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) error {
	sts := &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      sc.Name,
			Namespace: sc.Namespace,
		},
		Spec: r.buildStatefulSetSpec(sc),
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
func (r *SlapdClusterReconciler) reconcileReadOnlyStatefulSet(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) error {
	if sc.Spec.ReadReplicas == 0 {
		return nil
	}
	sts := &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      sc.Name + "-readonly",
			Namespace: sc.Namespace,
		},
		Spec: r.buildReadOnlyStatefulSetSpec(sc),
	}
	if err := controllerutil.SetControllerReference(sc, sts, r.Scheme); err != nil {
		return err
	}
	return r.Patch(ctx, sts, client.Apply, client.ForceOwnership, client.FieldOwner(fieldManager))
}

// buildReadOnlyStatefulSetSpec constructs the StatefulSet spec for read-only replicas.
// Compared to the RW spec: no accesslog volume, LDAP_READONLY_REPLICA=true,
// LDAP_REPLICAS = number of RW masters, no self-skip in syncrepl.
func (r *SlapdClusterReconciler) buildReadOnlyStatefulSetSpec(sc *ldapv1alpha1.SlapdCluster) appsv1.StatefulSetSpec {
	labels := readOnlySelectorLabels(sc.Name)
	replicas := sc.Spec.ReadReplicas
	logLevel := strconv.Itoa(int(sc.Spec.LogLevel))

	rwReplicas := sc.Spec.Replicas
	if rwReplicas == 0 {
		rwReplicas = 1
	}

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

	// ── Volumes (non-PVC) — no accesslog for RO replicas ─────────────────────
	volumes := []corev1.Volume{
		{
			Name:         "ldap-run",
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
			pvcTemplate("ldap-config", cfgSize, sc.Spec.Persistence.Config.StorageClass, cfgAM),
			pvcTemplate("ldap-data", dataSize, sc.Spec.Persistence.Data.StorageClass, dataAM),
		}
		// No accesslog PVC for read-only replicas.
	} else {
		volumes = append(volumes,
			corev1.Volume{
				Name:         "ldap-config",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			},
			corev1.Volume{
				Name:         "ldap-data",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			},
		)
	}

	if sc.Spec.LDAP.TLS.Enabled {
		volumes = append(volumes, corev1.Volume{
			Name: "ldap-tls",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: sc.Spec.LDAP.TLS.SecretName,
				},
			},
		})
	}

	// ── Init container ────────────────────────────────────────────────────────
	initEnv := []corev1.EnvVar{
		{Name: "LDAP_DOMAIN_DC", Value: sc.Spec.LDAP.Domain},
		{
			Name: "LDAP_ADMIN_PW",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: sc.Name + "-passwords"},
					Key:                  "admin-password",
				},
			},
		},
		{
			Name: "LDAP_ROOT_PW",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: sc.Name + "-config-password"},
					Key:                  "root-password",
				},
			},
		},
		{Name: "LDAP_TLS_ENABLED", Value: strconv.FormatBool(sc.Spec.LDAP.TLS.Enabled)},
		{Name: "FORCE_REBOOTSTRAP", Value: strconv.FormatBool(sc.Spec.LDAP.ForceRebootstrap)},
		{Name: "CONFIG_DIR", Value: "/ldap-config"},
		{Name: "DATA_DIR", Value: "/ldap-data"},
		{Name: "ACCESSLOG_DIR", Value: "/ldap-accesslog"},
		// Read-only replica flags.
		{Name: "LDAP_READONLY_REPLICA", Value: "true"},
		{Name: "LDAP_REPLICATION_ENABLED", Value: "true"},
		{Name: "LDAP_REPLICAS", Value: strconv.Itoa(int(rwReplicas))},
		{Name: "LDAP_CLUSTER_NAME", Value: sc.Name},
		{Name: "LDAP_CLUSTER_HEADLESS_SVC", Value: sc.Name + "-headless"},
		{Name: "LDAP_NAMESPACE", Value: sc.Namespace},
		{
			Name: "LDAP_REPLICATION_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: sc.Name + "-passwords"},
					Key:                  "replication-password",
				},
			},
		},
	}

	if sc.Spec.LDAP.TLS.Enabled {
		initEnv = append(initEnv,
			corev1.EnvVar{Name: "LDAP_TLS_CACERT_PATH", Value: "/etc/openldap/tls/ca.crt"},
			corev1.EnvVar{Name: "LDAP_TLS_CERT_PATH", Value: "/etc/openldap/tls/tls.crt"},
			corev1.EnvVar{Name: "LDAP_TLS_KEY_PATH", Value: "/etc/openldap/tls/tls.key"},
		)
	}

	initMounts := []corev1.VolumeMount{
		{Name: "ldap-config", MountPath: "/ldap-config"},
		{Name: "ldap-data", MountPath: "/ldap-data"},
	}
	// No accesslog mount for read-only replicas.
	if sc.Spec.LDAP.TLS.Enabled {
		initMounts = append(initMounts, corev1.VolumeMount{
			Name:      "ldap-tls",
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
		{Name: "ldap-config", MountPath: "/ldap-config"},
		{Name: "ldap-data", MountPath: "/ldap-data"},
		{Name: "ldap-run", MountPath: "/run/openldap"},
	}
	// No accesslog mount for read-only replicas.
	if sc.Spec.LDAP.TLS.Enabled {
		mainMounts = append(mainMounts, corev1.VolumeMount{
			Name:      "ldap-tls",
			MountPath: "/etc/openldap/tls",
			ReadOnly:  true,
		})
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
			"-F", "/ldap-config",
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
		ServiceName: sc.Name + "-readonly-headless",
		Replicas:    &replicas,
		Selector: &metav1.LabelSelector{
			MatchLabels: labels,
		},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: labels,
			},
			Spec: corev1.PodSpec{
				SecurityContext: podSecCtx,
				InitContainers:  []corev1.Container{initContainer},
				Containers:      []corev1.Container{mainContainer},
				Volumes:         volumes,
			},
		},
		VolumeClaimTemplates: volumeClaimTemplates,
	}

	return spec
}

// reconcileBootstrap seeds the initial LDAP directory entries via a live LDAP connection
// to pod-0 once it is ready.  It is idempotent: if the root entry already exists the
// function returns immediately after marking BootstrapComplete.
//
// Using a live connection (instead of slapadd) ensures that the accesslog overlay
// records all initial writes, which is required for delta-syncrepl to work correctly.
func (r *SlapdClusterReconciler) reconcileBootstrap(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) error {
	if sc.Status.BootstrapComplete {
		return nil
	}

	log := logf.FromContext(ctx)

	// Wait until pod-0 is ready before attempting to connect.
	pod0 := &corev1.Pod{}
	if err := r.Get(ctx, client.ObjectKey{Name: sc.Name + "-0", Namespace: sc.Namespace}, pod0); err != nil {
		if errors.IsNotFound(err) {
			log.Info("pod-0 not yet created; deferring bootstrap")
			return nil
		}
		return err
	}
	if !isPodReady(pod0) {
		log.Info("pod-0 not yet ready; deferring bootstrap")
		return nil
	}
	if pod0.Status.PodIP == "" {
		log.Info("pod-0 has no IP yet; deferring bootstrap")
		return nil
	}

	// Connect directly to pod-0's IP on the container LDAP port.
	// Using the pod IP avoids the cluster-domain-discovery problem and is
	// more direct than routing through the ClusterIP service.
	addr := pod0.Status.PodIP + ":" + strconv.Itoa(int(ldapContainerPort))
	conn, err := ldap.DialURL("ldap://"+addr,
		ldap.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}),
	)
	if err != nil {
		log.Info("LDAP dial to pod-0 failed; deferring bootstrap", "addr", addr, "err", err)
		return nil
	}
	defer conn.Close()
	conn.SetTimeout(ldapRequestTimeout)

	// Bind as the rootdn.  In OpenLDAP the rootdn+rootpw defined in slapd.conf
	// allow authentication even before the LDAP entry for that DN exists.
	adminDN := "cn=admin," + sc.Spec.LDAP.Domain
	adminPW, err := r.getAdminPassword(ctx, sc)
	if err != nil {
		return err
	}
	if err := conn.Bind(adminDN, adminPW); err != nil {
		log.Info("LDAP bind failed; deferring bootstrap", "dn", adminDN, "err", err)
		return nil
	}

	// Idempotency: if the root entry already exists we're done.
	exists, err := ldapEntryExists(conn, sc.Spec.LDAP.Domain)
	if err != nil {
		return fmt.Errorf("check root entry: %w", err)
	}
	if exists {
		log.Info("root entry already exists; marking bootstrap complete")
		sc.Status.BootstrapComplete = true
		return nil
	}

	// Extract the first DC label from the domain (e.g. "dc=example,dc=org" → "example").
	parts := strings.SplitN(sc.Spec.LDAP.Domain, ",", 2)
	dc := strings.TrimPrefix(parts[0], "dc=")

	// Add root entry.
	addRoot := ldap.NewAddRequest(sc.Spec.LDAP.Domain, nil)
	addRoot.Attribute("objectClass", []string{"top", "dcObject", "organization"})
	addRoot.Attribute("o", []string{dc})
	addRoot.Attribute("dc", []string{dc})
	if err := conn.Add(addRoot); err != nil {
		return fmt.Errorf("add root entry %s: %w", sc.Spec.LDAP.Domain, err)
	}

	// Add admin entry.  Hash the plaintext password we already have.
	adminHash, err := generateSSHAHash(adminPW)
	if err != nil {
		return fmt.Errorf("hash admin password: %w", err)
	}
	addAdmin := ldap.NewAddRequest(adminDN, nil)
	addAdmin.Attribute("objectClass", []string{"simpleSecurityObject", "organizationalRole"})
	addAdmin.Attribute("cn", []string{"admin"})
	addAdmin.Attribute("description", []string{"LDAP administrator"})
	addAdmin.Attribute("userPassword", []string{adminHash})
	if err := conn.Add(addAdmin); err != nil {
		return fmt.Errorf("add admin entry: %w", err)
	}

	// Add replication user when replication is enabled.
	if sc.Spec.Replication.Enabled {
		pwSecret := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{
			Name:      sc.Name + "-passwords",
			Namespace: sc.Namespace,
		}, pwSecret); err != nil {
			return fmt.Errorf("read passwords secret: %w", err)
		}
		replPW := string(pwSecret.Data["replication-password"])
		replHash, err := generateSSHAHash(replPW)
		if err != nil {
			return fmt.Errorf("hash replication password: %w", err)
		}
		replDN := "cn=replication," + sc.Spec.LDAP.Domain
		addRepl := ldap.NewAddRequest(replDN, nil)
		addRepl.Attribute("objectClass", []string{"simpleSecurityObject", "organizationalRole"})
		addRepl.Attribute("cn", []string{"replication"})
		addRepl.Attribute("description", []string{"Syncrepl bind account"})
		addRepl.Attribute("userPassword", []string{replHash})
		if err := conn.Add(addRepl); err != nil {
			return fmt.Errorf("add replication entry: %w", err)
		}
	}

	log.Info("bootstrap complete", "baseDN", sc.Spec.LDAP.Domain)
	sc.Status.BootstrapComplete = true
	return nil
}

// buildStatefulSetSpec constructs the StatefulSet spec.
func (r *SlapdClusterReconciler) buildStatefulSetSpec(sc *ldapv1alpha1.SlapdCluster) appsv1.StatefulSetSpec {
	labels := selectorLabels(sc.Name)
	replicas := sc.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}
	logLevel := strconv.Itoa(int(sc.Spec.LogLevel))
	replicationEnabled := sc.Spec.Replication.Enabled && replicas > 1

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
			Name:         "ldap-run",
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
		accesslogSize := sc.Spec.Persistence.Accesslog.Size
		if accesslogSize == "" {
			accesslogSize = "1Gi"
		}
		cfgAM := sc.Spec.Persistence.Config.AccessMode
		if cfgAM == "" {
			cfgAM = corev1.ReadWriteOnce
		}
		dataAM := sc.Spec.Persistence.Data.AccessMode
		if dataAM == "" {
			dataAM = corev1.ReadWriteOnce
		}
		accesslogAM := sc.Spec.Persistence.Accesslog.AccessMode
		if accesslogAM == "" {
			accesslogAM = corev1.ReadWriteOnce
		}

		volumeClaimTemplates = []corev1.PersistentVolumeClaim{
			pvcTemplate("ldap-config", cfgSize, sc.Spec.Persistence.Config.StorageClass, cfgAM),
			pvcTemplate("ldap-data", dataSize, sc.Spec.Persistence.Data.StorageClass, dataAM),
		}
		if replicationEnabled {
			volumeClaimTemplates = append(volumeClaimTemplates,
				pvcTemplate("ldap-accesslog", accesslogSize, sc.Spec.Persistence.Accesslog.StorageClass, accesslogAM),
			)
		}
	} else {
		volumes = append(volumes,
			corev1.Volume{
				Name:         "ldap-config",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			},
			corev1.Volume{
				Name:         "ldap-data",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			},
		)
		if replicationEnabled {
			volumes = append(volumes, corev1.Volume{
				Name:         "ldap-accesslog",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			})
		}
	}

	if sc.Spec.LDAP.TLS.Enabled {
		volumes = append(volumes, corev1.Volume{
			Name: "ldap-tls",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: sc.Spec.LDAP.TLS.SecretName,
				},
			},
		})
	}

	// External peer TLS CA cert volumes (Phase 3).
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

	// ── Init container ────────────────────────────────────────────────────────
	initEnv := []corev1.EnvVar{
		{Name: "LDAP_DOMAIN_DC", Value: sc.Spec.LDAP.Domain},
		{
			Name: "LDAP_ADMIN_PW",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: sc.Name + "-passwords"},
					Key:                  "admin-password",
				},
			},
		},
		{
			Name: "LDAP_ROOT_PW",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: sc.Name + "-config-password"},
					Key:                  "root-password",
				},
			},
		},
		{Name: "LDAP_TLS_ENABLED", Value: strconv.FormatBool(sc.Spec.LDAP.TLS.Enabled)},
		{Name: "FORCE_REBOOTSTRAP", Value: strconv.FormatBool(sc.Spec.LDAP.ForceRebootstrap)},
		{Name: "CONFIG_DIR", Value: "/ldap-config"},
		{Name: "DATA_DIR", Value: "/ldap-data"},
		{Name: "ACCESSLOG_DIR", Value: "/ldap-accesslog"},
	}

	if sc.Spec.LDAP.TLS.Enabled {
		initEnv = append(initEnv,
			corev1.EnvVar{Name: "LDAP_TLS_CACERT_PATH", Value: "/etc/openldap/tls/ca.crt"},
			corev1.EnvVar{Name: "LDAP_TLS_CERT_PATH", Value: "/etc/openldap/tls/tls.crt"},
			corev1.EnvVar{Name: "LDAP_TLS_KEY_PATH", Value: "/etc/openldap/tls/tls.key"},
		)
	}

	if replicationEnabled {
		initEnv = append(initEnv,
			corev1.EnvVar{Name: "LDAP_REPLICATION_ENABLED", Value: "true"},
			corev1.EnvVar{Name: "LDAP_REPLICAS", Value: strconv.Itoa(int(replicas))},
			corev1.EnvVar{Name: "LDAP_CLUSTER_NAME", Value: sc.Name},
			corev1.EnvVar{Name: "LDAP_CLUSTER_HEADLESS_SVC", Value: sc.Name + "-headless"},
			corev1.EnvVar{Name: "LDAP_NAMESPACE", Value: sc.Namespace},
			corev1.EnvVar{
				Name: "LDAP_REPLICATION_PASSWORD",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: sc.Name + "-passwords"},
						Key:                  "replication-password",
					},
				},
			},
		)
	}

	initMounts := []corev1.VolumeMount{
		{Name: "ldap-config", MountPath: "/ldap-config"},
		{Name: "ldap-data", MountPath: "/ldap-data"},
	}
	if replicationEnabled {
		initMounts = append(initMounts, corev1.VolumeMount{
			Name:      "ldap-accesslog",
			MountPath: "/ldap-accesslog",
		})
	}
	if sc.Spec.LDAP.TLS.Enabled {
		initMounts = append(initMounts, corev1.VolumeMount{
			Name:      "ldap-tls",
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
		{Name: "ldap-config", MountPath: "/ldap-config"},
		{Name: "ldap-data", MountPath: "/ldap-data"},
		{Name: "ldap-run", MountPath: "/run/openldap"},
	}
	if replicationEnabled {
		mainMounts = append(mainMounts, corev1.VolumeMount{
			Name:      "ldap-accesslog",
			MountPath: "/ldap-accesslog",
		})
	}
	if sc.Spec.LDAP.TLS.Enabled {
		mainMounts = append(mainMounts, corev1.VolumeMount{
			Name:      "ldap-tls",
			MountPath: "/etc/openldap/tls",
			ReadOnly:  true,
		})
	}

	// External peer TLS CA cert volume mounts (Phase 3).
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
			"-F", "/ldap-config",
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
		ServiceName: sc.Name + "-headless",
		Replicas:    &replicas,
		Selector: &metav1.LabelSelector{
			MatchLabels: labels,
		},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: labels,
			},
			Spec: corev1.PodSpec{
				SecurityContext: podSecCtx,
				InitContainers:  []corev1.Container{initContainer},
				Containers:      []corev1.Container{mainContainer},
				Volumes:         volumes,
			},
		},
		VolumeClaimTemplates: volumeClaimTemplates,
	}

	return spec
}

// reconcileACLs applies spec.ldap.acls to every pod's data database cn=config entry.
// Because cn=config is node-local (never replicated), each pod must be updated
// individually via its headless-service DNS address.
// Pods that cannot be reached (not yet ready) are logged and skipped; the next
// reconcile loop will retry.  A non-nil error is returned only for hard failures
// such as a missing credential secret.
func (r *SlapdClusterReconciler) reconcileACLs(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) (bool, error) {
	if len(sc.Spec.LDAP.ACLs) == 0 {
		return false, nil
	}

	rootPW, err := r.getConfigPassword(ctx, sc)
	if err != nil {
		return false, err
	}

	// Build effective ACL list. When replication is enabled, prepend a rule
	// granting the replication bind DN read access to all attributes. Without
	// this, user-specified ACLs that deny attribute reads (e.g. userPassword)
	// would prevent the syncrepl consumer from receiving those attributes,
	// causing silent data loss on replicas.
	acls := sc.Spec.LDAP.ACLs
	replicationEnabled := sc.Spec.Replication.Enabled && sc.Spec.Replicas > 1
	if replicationEnabled || len(sc.Spec.Replication.ExternalPeers) > 0 {
		replACL := fmt.Sprintf(
			`to * by dn.exact="cn=replication,%s" read by * break`,
			sc.Spec.LDAP.Domain)
		acls = append([]string{replACL}, acls...)
	}

	log := logf.FromContext(ctx)
	replicas := sc.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}
	headlessSvc := sc.Name + "-headless"
	skipped := false

	for i := int32(0); i < replicas; i++ {
		host := fmt.Sprintf("%s-%d.%s.%s.svc.cluster.local",
			sc.Name, i, headlessSvc, sc.Namespace)
		if err := r.applyACLsToPod(ctx, host, rootPW, sc.Spec.LDAP.Domain, acls); err != nil {
			log.Info("ACL reconcile skipped for pod (will retry on next reconcile)",
				"ordinal", i, "host", host, "err", err)
			skipped = true
		}
	}

	// Apply ACLs to read-only pods too (cn=config is node-local).
	if sc.Spec.ReadReplicas > 0 {
		roHeadless := sc.Name + "-readonly-headless"
		for i := int32(0); i < sc.Spec.ReadReplicas; i++ {
			host := fmt.Sprintf("%s-readonly-%d.%s.%s.svc.cluster.local",
				sc.Name, i, roHeadless, sc.Namespace)
			if err := r.applyACLsToPod(ctx, host, rootPW, sc.Spec.LDAP.Domain, acls); err != nil {
				log.Info("ACL reconcile skipped for read-only pod (will retry on next reconcile)",
					"ordinal", i, "host", host, "err", err)
				skipped = true
			}
		}
	}

	return skipped, nil
}

// applyACLsToPod applies the desired ACL rules to one slapd pod's cn=config data DB entry.
// It is a no-op when the pod's current olcAccess already matches desired.
func (r *SlapdClusterReconciler) applyACLsToPod(
	ctx context.Context,
	host, rootPW, domain string,
	desired []string,
) error {
	log := logf.FromContext(ctx)

	addr := host + ":" + strconv.Itoa(int(ldapContainerPort))
	conn, err := ldap.DialURL("ldap://"+addr,
		ldap.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}),
	)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()
	conn.SetTimeout(ldapRequestTimeout)

	if err := conn.Bind("cn=admin,cn=config", rootPW); err != nil {
		return fmt.Errorf("bind cn=admin,cn=config at %s: %w", host, err)
	}

	// Locate the data database entry in cn=config.
	dataDN, err := findDataDBDN(conn, domain)
	if err != nil {
		return err
	}

	// Read current olcAccess values.
	sr, err := conn.Search(ldap.NewSearchRequest(
		dataDN, ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		1, 0, false, "(objectClass=*)", []string{"olcAccess"}, nil,
	))
	if err != nil {
		return fmt.Errorf("read olcAccess on %s at %s: %w", dataDN, host, err)
	}
	if len(sr.Entries) == 0 {
		return fmt.Errorf("no entry at %s on %s", dataDN, host)
	}

	current := sr.Entries[0].GetEqualFoldAttributeValues("olcAccess")
	if aclsMatch(current, desired) {
		log.V(1).Info("ACLs already up-to-date", "host", host)
		return nil
	}

	log.Info("replacing ACL rules", "host", host, "rules", len(desired))
	modReq := ldap.NewModifyRequest(dataDN, nil)
	modReq.Replace("olcAccess", desired)
	if err := conn.Modify(modReq); err != nil {
		return fmt.Errorf("replace olcAccess on %s at %s: %w", dataDN, host, err)
	}
	return nil
}

// findDataDBDN searches cn=config one level deep for the MDB database entry whose
// olcSuffix matches the given domain and returns its DN.
func findDataDBDN(conn *ldap.Conn, domain string) (string, error) {
	sr, err := conn.Search(ldap.NewSearchRequest(
		"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
		0, 0, false,
		fmt.Sprintf("(olcSuffix=%s)", domain),
		[]string{"dn"}, nil,
	))
	if err != nil {
		return "", fmt.Errorf("search cn=config for olcSuffix=%s: %w", domain, err)
	}
	if len(sr.Entries) == 0 {
		return "", fmt.Errorf("no database entry with olcSuffix=%s in cn=config", domain)
	}
	return sr.Entries[0].DN, nil
}

// aclsMatch returns true when the stored olcAccess values (which carry {N} index
// prefixes, e.g. "{0}to * by * read") match the desired rules (without prefixes).
func aclsMatch(current, desired []string) bool {
	if len(current) != len(desired) {
		return false
	}
	for i, c := range current {
		bare := c
		if len(c) > 0 && c[0] == '{' {
			if idx := strings.Index(c, "}"); idx >= 0 {
				bare = c[idx+1:]
			}
		}
		if bare != desired[i] {
			return false
		}
	}
	return true
}

// ── Schema reconciliation ─────────────────────────────────────────────────────

// schemaEntry is a parsed representation of a JSON-encoded schema to be added
// to cn=schema,cn=config.
type schemaEntry struct {
	DN          string
	ObjectClass []string
	Attributes  map[string][]string
}

// parseSchemaJSON parses a JSON string into a schemaEntry.
// The JSON format matches slapd-test's customSchemaJson: objectClass may be a
// string or []string, and attribute values may be a string or []string.
func parseSchemaJSON(raw string) (schemaEntry, error) {
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return schemaEntry{}, fmt.Errorf("unmarshal schema JSON: %w", err)
	}

	dn, _ := m["dn"].(string)
	if dn == "" {
		return schemaEntry{}, fmt.Errorf("schema JSON missing required 'dn' field")
	}

	var objectClass []string
	switch v := m["objectClass"].(type) {
	case string:
		objectClass = []string{v}
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok {
				objectClass = append(objectClass, s)
			}
		}
	}

	attrs := make(map[string][]string)
	if attrMap, ok := m["attributes"].(map[string]interface{}); ok {
		for k, v := range attrMap {
			switch val := v.(type) {
			case string:
				attrs[k] = []string{val}
			case []interface{}:
				for _, item := range val {
					if s, ok := item.(string); ok {
						attrs[k] = append(attrs[k], s)
					}
				}
			}
		}
	}

	return schemaEntry{
		DN:          dn,
		ObjectClass: objectClass,
		Attributes:  attrs,
	}, nil
}

// reconcileSchemas applies spec.ldap.schemas to every pod's cn=schema,cn=config.
// Like reconcileACLs, each pod is contacted individually via headless DNS because
// cn=config is node-local. Missing schemas are added; existing ones are skipped.
// Unreachable pods are logged and retried on the next reconcile.
func (r *SlapdClusterReconciler) reconcileSchemas(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) (bool, error) {
	if len(sc.Spec.LDAP.Schemas) == 0 {
		return false, nil
	}

	// Parse all schema entries up front so we fail fast on bad JSON.
	schemas := make([]schemaEntry, 0, len(sc.Spec.LDAP.Schemas))
	for i, raw := range sc.Spec.LDAP.Schemas {
		entry, err := parseSchemaJSON(raw)
		if err != nil {
			return false, fmt.Errorf("spec.ldap.schemas[%d]: %w", i, err)
		}
		schemas = append(schemas, entry)
	}

	rootPW, err := r.getConfigPassword(ctx, sc)
	if err != nil {
		return false, err
	}

	log := logf.FromContext(ctx)
	replicas := sc.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}
	headlessSvc := sc.Name + "-headless"
	skipped := false

	for i := int32(0); i < replicas; i++ {
		host := fmt.Sprintf("%s-%d.%s.%s.svc.cluster.local",
			sc.Name, i, headlessSvc, sc.Namespace)
		if err := r.applySchemaToPod(ctx, host, rootPW, schemas); err != nil {
			log.Info("Schema reconcile skipped for pod (will retry on next reconcile)",
				"ordinal", i, "host", host, "err", err)
			skipped = true
		}
	}

	// Apply schemas to read-only pods too (cn=config is node-local).
	if sc.Spec.ReadReplicas > 0 {
		roHeadless := sc.Name + "-readonly-headless"
		for i := int32(0); i < sc.Spec.ReadReplicas; i++ {
			host := fmt.Sprintf("%s-readonly-%d.%s.%s.svc.cluster.local",
				sc.Name, i, roHeadless, sc.Namespace)
			if err := r.applySchemaToPod(ctx, host, rootPW, schemas); err != nil {
				log.Info("Schema reconcile skipped for read-only pod (will retry on next reconcile)",
					"ordinal", i, "host", host, "err", err)
				skipped = true
			}
		}
	}

	return skipped, nil
}

// applySchemaToPod adds missing schema entries to one slapd pod's cn=schema,cn=config.
// Existing schemas (by DN) are skipped.
func (r *SlapdClusterReconciler) applySchemaToPod(
	ctx context.Context,
	host, rootPW string,
	schemas []schemaEntry,
) error {
	log := logf.FromContext(ctx)

	addr := host + ":" + strconv.Itoa(int(ldapContainerPort))
	conn, err := ldap.DialURL("ldap://"+addr,
		ldap.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}),
	)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()
	conn.SetTimeout(ldapRequestTimeout)

	if err := conn.Bind("cn=admin,cn=config", rootPW); err != nil {
		return fmt.Errorf("bind cn=admin,cn=config at %s: %w", host, err)
	}

	for _, s := range schemas {
		exists, err := schemaExistsByCN(conn, schemaCNFromDN(s.DN))
		if err != nil {
			return fmt.Errorf("check schema DN %s at %s: %w", s.DN, host, err)
		}
		if exists {
			log.V(1).Info("Schema already exists, skipping", "host", host, "dn", s.DN)
			continue
		}

		log.Info("Adding schema", "host", host, "dn", s.DN)
		addReq := ldap.NewAddRequest(s.DN, nil)
		if len(s.ObjectClass) > 0 {
			addReq.Attribute("objectClass", s.ObjectClass)
		}
		for attr, vals := range s.Attributes {
			addReq.Attribute(attr, vals)
		}
		if err := conn.Add(addReq); err != nil {
			// OpenLDAP returns err=80 (Other) with "Duplicate attributeType" when
			// the schema's attribute types are already registered globally (e.g.
			// added by the bootstrap job before the operator ran). Treat this as
			// "already exists" rather than a hard failure.
			if ldap.IsErrorWithCode(err, ldap.LDAPResultOther) && strings.Contains(err.Error(), "Duplicate") {
				log.V(1).Info("Schema attributes already registered, skipping", "host", host, "dn", s.DN)
				continue
			}
			return fmt.Errorf("add schema %s at %s: %w", s.DN, host, err)
		}
	}

	return nil
}

// schemaCNFromDN extracts the cn value from a schema DN.
// e.g. "cn=ox,cn=schema,cn=config" → "ox".
func schemaCNFromDN(dn string) string {
	parts := strings.SplitN(dn, ",", 2)
	if len(parts) == 0 {
		return ""
	}
	kv := strings.SplitN(parts[0], "=", 2)
	if len(kv) != 2 {
		return ""
	}
	return kv[1]
}

// schemaExistsByCN searches cn=schema,cn=config one level deep for an entry
// whose cn matches the given name. OpenLDAP auto-numbers schema entries
// (e.g. cn=ox becomes cn={4}ox), so a direct base-scope search on the
// un-numbered DN always returns "no such object". A one-level search with
// a cn equality filter works because OpenLDAP's config backend strips the
// {N} ordering prefix for filter evaluation.
func schemaExistsByCN(conn *ldap.Conn, cn string) (bool, error) {
	if cn == "" {
		return false, fmt.Errorf("empty cn for schema existence check")
	}
	sr, err := conn.Search(ldap.NewSearchRequest(
		"cn=schema,cn=config",
		ldap.ScopeSingleLevel,
		ldap.NeverDerefAliases,
		1, 0, false,
		fmt.Sprintf("(cn=%s)", ldap.EscapeFilter(cn)),
		[]string{"dn"},
		nil,
	))
	if err != nil {
		return false, err
	}
	return len(sr.Entries) > 0, nil
}

// ── Replication reconciliation (Phase 3, ADR-003) ─────────────────────────────

// parsedExternalPeer holds a resolved external peer with its bind password.
type parsedExternalPeer struct {
	Name     string
	URI      string
	BindDN   string
	Password string
	// TLS CA cert path inside the container (empty if no TLS secret).
	TLSCACertPath string
}

// reconcileReplication applies syncrepl + mirrormode to each RW pod's cn=config
// data DB entry. The operator is the sole owner of olcSyncRepl and olcMirrorMode
// (see ADR-003). RO replicas also get syncrepl stanzas pointing to RW masters.
func (r *SlapdClusterReconciler) reconcileReplication(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) (bool, error) {
	log := logf.FromContext(ctx)

	replicas := sc.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}

	hasExternalPeers := len(sc.Spec.Replication.ExternalPeers) > 0
	replicationEnabled := sc.Spec.Replication.Enabled && replicas > 1

	// Nothing to do if replication is disabled and there are no external peers.
	if !replicationEnabled && !hasExternalPeers {
		// Clear external peer statuses if previously set.
		sc.Status.ExternalPeerStatuses = nil
		return false, nil
	}

	rootPW, err := r.getConfigPassword(ctx, sc)
	if err != nil {
		return false, err
	}

	// Read in-cluster replication password.
	var replPassword string
	if replicationEnabled {
		pwSecret := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{
			Name:      sc.Name + "-passwords",
			Namespace: sc.Namespace,
		}, pwSecret); err != nil {
			return false, fmt.Errorf("read passwords secret for replication: %w", err)
		}
		replPassword = string(pwSecret.Data["replication-password"])
	}

	// Resolve external peers: read bind passwords from their secrets.
	var externalPeers []parsedExternalPeer
	for _, ep := range sc.Spec.Replication.ExternalPeers {
		parsed := parsedExternalPeer{
			Name:   ep.Name,
			URI:    ep.URI,
			BindDN: ep.BindDN,
		}
		if ep.TLSSecretName != "" {
			parsed.TLSCACertPath = "/etc/openldap/tls/peers/" + ep.Name + "/ca.crt"
		}
		if ep.BindPasswordSecretName != "" {
			secret := &corev1.Secret{}
			if err := r.Get(ctx, client.ObjectKey{
				Name:      ep.BindPasswordSecretName,
				Namespace: sc.Namespace,
			}, secret); err != nil {
				log.Info("skipping external peer: cannot read bind password secret",
					"peer", ep.Name, "secret", ep.BindPasswordSecretName, "err", err)
				continue
			}
			parsed.Password = string(secret.Data["password"])
			if parsed.Password == "" {
				// Try replication-password key as fallback.
				parsed.Password = string(secret.Data["replication-password"])
			}
		}
		externalPeers = append(externalPeers, parsed)
	}

	headlessSvc := sc.Name + "-headless"
	tlsEnabled := sc.Spec.LDAP.TLS.Enabled
	tlsCACertPath := "/etc/openldap/tls/ca.crt"
	skipped := false

	// Apply syncrepl to RW pods.
	for i := int32(0); i < replicas; i++ {
		host := fmt.Sprintf("%s-%d.%s.%s.svc.cluster.local",
			sc.Name, i, headlessSvc, sc.Namespace)

		desired := buildDesiredSyncRepl(
			sc.Name, headlessSvc, sc.Namespace, sc.Spec.LDAP.Domain,
			replicas, i, replPassword,
			tlsEnabled, tlsCACertPath,
			externalPeers,
		)
		desiredMirrorMode := "TRUE"

		if err := r.applySyncreplToPod(ctx, host, rootPW, sc.Spec.LDAP.Domain, desired, desiredMirrorMode); err != nil {
			log.Info("Replication reconcile skipped for pod (will retry on next reconcile)",
				"ordinal", i, "host", host, "err", err)
			skipped = true
		}
	}

	// Apply syncrepl to RO pods (in-cluster RW masters only, no external peers, no mirrormode).
	if sc.Spec.ReadReplicas > 0 {
		roHeadless := sc.Name + "-readonly-headless"
		for i := int32(0); i < sc.Spec.ReadReplicas; i++ {
			host := fmt.Sprintf("%s-readonly-%d.%s.%s.svc.cluster.local",
				sc.Name, i, roHeadless, sc.Namespace)

			// RO replicas get stanzas for ALL RW masters (no self-skip).
			desired := buildDesiredSyncReplRO(
				sc.Name, headlessSvc, sc.Namespace, sc.Spec.LDAP.Domain,
				replicas, replPassword,
				tlsEnabled, tlsCACertPath,
			)

			if err := r.applySyncreplToPod(ctx, host, rootPW, sc.Spec.LDAP.Domain, desired, ""); err != nil {
				log.Info("Replication reconcile skipped for read-only pod (will retry on next reconcile)",
					"ordinal", i, "host", host, "err", err)
				skipped = true
			}
		}
	}

	// External peer status reporting.
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

	return skipped, nil
}

// applySyncreplToPod applies the desired olcSyncRepl and olcMultiProvider values to one pod.
// mirrorMode should be "TRUE" for RW pods; empty string means do not set olcMultiProvider.
// Note: olcMultiProvider is the OpenLDAP 2.6+ name for the former olcMirrorMode attribute.
func (r *SlapdClusterReconciler) applySyncreplToPod(
	ctx context.Context,
	host, rootPW, domain string,
	desired []string,
	mirrorMode string,
) error {
	log := logf.FromContext(ctx)

	addr := host + ":" + strconv.Itoa(int(ldapContainerPort))
	conn, err := ldap.DialURL("ldap://"+addr,
		ldap.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}),
	)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()
	conn.SetTimeout(ldapRequestTimeout)

	if err := conn.Bind("cn=admin,cn=config", rootPW); err != nil {
		return fmt.Errorf("bind cn=admin,cn=config at %s: %w", host, err)
	}

	dataDN, err := findDataDBDN(conn, domain)
	if err != nil {
		return err
	}

	// Read current olcSyncRepl and olcMultiProvider (formerly olcMirrorMode in OpenLDAP <2.6).
	sr, err := conn.Search(ldap.NewSearchRequest(
		dataDN, ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		1, 0, false, "(objectClass=*)", []string{"olcSyncRepl", "olcMultiProvider"}, nil,
	))
	if err != nil {
		return fmt.Errorf("read olcSyncRepl on %s at %s: %w", dataDN, host, err)
	}
	if len(sr.Entries) == 0 {
		return fmt.Errorf("no entry at %s on %s", dataDN, host)
	}

	currentSyncRepl := sr.Entries[0].GetEqualFoldAttributeValues("olcSyncRepl")
	currentMirrorMode := ""
	if vals := sr.Entries[0].GetEqualFoldAttributeValues("olcMultiProvider"); len(vals) > 0 {
		currentMirrorMode = vals[0]
	}

	syncreplChanged := !syncreplMatch(currentSyncRepl, desired)
	mirrorModeChanged := mirrorMode != "" && !strings.EqualFold(currentMirrorMode, mirrorMode)

	if !syncreplChanged && !mirrorModeChanged {
		log.V(1).Info("syncrepl already up-to-date", "host", host)
		return nil
	}

	log.Info("replacing syncrepl stanzas", "host", host, "stanzas", len(desired),
		"syncreplChanged", syncreplChanged, "mirrorModeChanged", mirrorModeChanged)

	modReq := ldap.NewModifyRequest(dataDN, nil)
	if syncreplChanged {
		modReq.Replace("olcSyncRepl", desired)
	}
	if mirrorModeChanged {
		modReq.Replace("olcMultiProvider", []string{mirrorMode})
	}
	if err := conn.Modify(modReq); err != nil {
		return fmt.Errorf("replace olcSyncRepl/olcMultiProvider on %s at %s: %w", dataDN, host, err)
	}
	return nil
}

// buildDesiredSyncRepl computes the desired olcSyncRepl stanzas for one RW pod.
// In-cluster peers get RIDs 1..N (skip self), external peers get RIDs 101..100+M.
func buildDesiredSyncRepl(
	clusterName, headlessSvc, namespace, domain string,
	replicas, ordinal int32,
	replPassword string,
	tlsEnabled bool, tlsCACertPath string,
	externalPeers []parsedExternalPeer,
) []string {
	var stanzas []string

	// Peer URL scheme and port depend on TLS.
	peerScheme := "ldap"
	peerPort := int32(1024)
	syncreplTLSOpt := ""
	if tlsEnabled {
		peerScheme = "ldaps"
		peerPort = 1025
		syncreplTLSOpt = fmt.Sprintf(" tls_cacert=%s", tlsCACertPath)
	}

	// In-cluster stanzas: one per RW peer, skip self.
	for i := int32(0); i < replicas; i++ {
		if i == ordinal {
			continue
		}
		rid := fmt.Sprintf("%03d", i+1)
		peerHost := fmt.Sprintf("%s-%d.%s.%s.svc.cluster.local", clusterName, i, headlessSvc, namespace)
		providerURI := fmt.Sprintf("%s://%s:%d", peerScheme, peerHost, peerPort)

		stanza := fmt.Sprintf("rid=%s provider=%s"+
			" type=refreshAndPersist"+
			" searchbase=\"%s\""+
			" scope=sub"+
			" schemachecking=off"+
			" bindmethod=simple"+
			" binddn=\"cn=replication,%s\""+
			" credentials=%s"+
			" logbase=\"cn=accesslog\""+
			" logfilter=\"(&(objectClass=auditWriteObject)(reqResult=0))\""+
			" syncdata=accesslog"+
			"%s"+
			" retry=\"5 +\"",
			rid, providerURI, domain, domain, replPassword, syncreplTLSOpt)
		stanzas = append(stanzas, stanza)
	}

	// External peer stanzas: RID 101..100+M.
	for idx, ep := range externalPeers {
		rid := fmt.Sprintf("%03d", 101+idx)
		tlsOpts := ""
		if ep.TLSCACertPath != "" {
			tlsOpts = fmt.Sprintf(" tls_cacert=%s", ep.TLSCACertPath)
			if tlsEnabled {
				tlsOpts += " tls_cert=/etc/openldap/tls/tls.crt tls_key=/etc/openldap/tls/tls.key"
			}
		}

		stanza := fmt.Sprintf("rid=%s provider=%s"+
			" type=refreshAndPersist"+
			" searchbase=\"%s\""+
			" scope=sub"+
			" schemachecking=off"+
			" bindmethod=simple"+
			" binddn=\"%s\""+
			" credentials=%s"+
			" logbase=\"cn=accesslog\""+
			" logfilter=\"(&(objectClass=auditWriteObject)(reqResult=0))\""+
			" syncdata=accesslog"+
			"%s"+
			" retry=\"5 +\"",
			rid, ep.URI, domain, ep.BindDN, ep.Password, tlsOpts)
		stanzas = append(stanzas, stanza)
	}

	return stanzas
}

// buildDesiredSyncReplRO computes syncrepl stanzas for a read-only replica.
// RO replicas get stanzas for ALL RW masters (no self-skip, no external peers, no mirrormode).
func buildDesiredSyncReplRO(
	clusterName, headlessSvc, namespace, domain string,
	rwReplicas int32,
	replPassword string,
	tlsEnabled bool, tlsCACertPath string,
) []string {
	var stanzas []string

	peerScheme := "ldap"
	peerPort := int32(1024)
	syncreplTLSOpt := ""
	if tlsEnabled {
		peerScheme = "ldaps"
		peerPort = 1025
		syncreplTLSOpt = fmt.Sprintf(" tls_cacert=%s", tlsCACertPath)
	}

	for i := int32(0); i < rwReplicas; i++ {
		rid := fmt.Sprintf("%03d", i+1)
		peerHost := fmt.Sprintf("%s-%d.%s.%s.svc.cluster.local", clusterName, i, headlessSvc, namespace)
		providerURI := fmt.Sprintf("%s://%s:%d", peerScheme, peerHost, peerPort)

		stanza := fmt.Sprintf("rid=%s provider=%s"+
			" type=refreshAndPersist"+
			" searchbase=\"%s\""+
			" scope=sub"+
			" schemachecking=off"+
			" bindmethod=simple"+
			" binddn=\"cn=replication,%s\""+
			" credentials=%s"+
			" logbase=\"cn=accesslog\""+
			" logfilter=\"(&(objectClass=auditWriteObject)(reqResult=0))\""+
			" syncdata=accesslog"+
			"%s"+
			" retry=\"5 +\"",
			rid, providerURI, domain, domain, replPassword, syncreplTLSOpt)
		stanzas = append(stanzas, stanza)
	}

	return stanzas
}

// syncreplMatch compares current olcSyncRepl values (with {N} prefixes) against desired.
// Comparison is RID-based and order-insensitive.
func syncreplMatch(current, desired []string) bool {
	if len(current) != len(desired) {
		return false
	}
	// Build maps keyed by RID.
	currentByRID := parseSyncreplByRID(current)
	desiredByRID := parseSyncreplByRID(desired)
	if len(currentByRID) != len(desiredByRID) {
		return false
	}
	for rid, dStanza := range desiredByRID {
		cStanza, ok := currentByRID[rid]
		if !ok {
			return false
		}
		if normalizeStanza(cStanza) != normalizeStanza(dStanza) {
			return false
		}
	}
	return true
}

// parseSyncreplByRID extracts RID → stanza body (without {N} prefix) from olcSyncRepl values.
func parseSyncreplByRID(stanzas []string) map[string]string {
	result := make(map[string]string, len(stanzas))
	for _, s := range stanzas {
		bare := s
		// Strip {N} prefix if present.
		if len(s) > 0 && s[0] == '{' {
			if idx := strings.Index(s, "}"); idx >= 0 {
				bare = s[idx+1:]
			}
		}
		// Extract RID from "rid=NNN ..."
		bare = strings.TrimSpace(bare)
		if strings.HasPrefix(bare, "rid=") {
			parts := strings.SplitN(bare, " ", 2)
			rid := strings.TrimPrefix(parts[0], "rid=")
			result[rid] = bare
		}
	}
	return result
}

// normalizeStanza normalizes whitespace in a syncrepl stanza for comparison.
func normalizeStanza(s string) string {
	fields := strings.Fields(s)
	sort.Strings(fields)
	return strings.Join(fields, " ")
}

// testExternalPeerConnectivity attempts a TLS/TCP connection to the external peer URI
// to check basic network reachability. Returns nil on success.
func testExternalPeerConnectivity(uri string) error {
	// Parse URI: ldaps://host:port or ldap://host:port
	addr := uri
	useTLS := false
	if strings.HasPrefix(uri, "ldaps://") {
		addr = strings.TrimPrefix(uri, "ldaps://")
		useTLS = true
	} else if strings.HasPrefix(uri, "ldap://") {
		addr = strings.TrimPrefix(uri, "ldap://")
	}
	// Strip trailing slash if any.
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

// getConfigPassword reads the plaintext root (cn=config admin) password from
// <name>-config-password.
func (r *SlapdClusterReconciler) getConfigPassword(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) (string, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{
		Name: sc.Name + "-config-password", Namespace: sc.Namespace,
	}, secret); err != nil {
		return "", fmt.Errorf("read %s-config-password: %w", sc.Name, err)
	}
	pw := string(secret.Data["root-password"])
	if pw == "" {
		return "", fmt.Errorf("secret %s-config-password is missing root-password key", sc.Name)
	}
	return pw, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SlapdClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ldapv1alpha1.SlapdCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
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

// generateSSHAHash returns an OpenLDAP-compatible {SSHA} password hash.
func generateSSHAHash(password string) (string, error) {
	salt := make([]byte, 8)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	h := sha1.New()
	h.Write([]byte(password))
	h.Write(salt)
	digest := h.Sum(nil)
	combined := append(digest, salt...)
	return "{SSHA}" + base64.StdEncoding.EncodeToString(combined), nil
}

// getAdminPassword reads the plaintext admin password from <name>-passwords.
func (r *SlapdClusterReconciler) getAdminPassword(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) (string, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Name: sc.Name + "-passwords", Namespace: sc.Namespace}, secret); err != nil {
		return "", fmt.Errorf("read passwords secret %s-passwords: %w", sc.Name, err)
	}
	pw := string(secret.Data["admin-password"])
	if pw == "" {
		return "", fmt.Errorf("secret %s-passwords is missing admin-password key", sc.Name)
	}
	return pw, nil
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

// ldapEntryExists returns true when the given DN exists in the directory.
func ldapEntryExists(conn *ldap.Conn, dn string) (bool, error) {
	req := ldap.NewSearchRequest(
		dn, ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		1, 0, false, "(objectClass=*)", []string{"dn"}, nil,
	)
	result, err := conn.Search(req)
	if err != nil {
		if ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
			return false, nil
		}
		return false, err
	}
	return len(result.Entries) > 0, nil
}
