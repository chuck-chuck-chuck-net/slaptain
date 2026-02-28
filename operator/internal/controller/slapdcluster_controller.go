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
	"encoding/base64"
	"fmt"
	"net"
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

	// 2. Reconcile credential secrets (<name>-credentials plaintext + <name>-passwords hashes).
	if err := r.reconcileSecret(ctx, sc); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcileSecret: %w", err)
	}

	// 3. Reconcile replication Secret (create-only; no-op when replication disabled).
	if err := r.reconcileReplicationSecret(ctx, sc); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcileReplicationSecret: %w", err)
	}

	// 4. Reconcile headless Service.
	if err := r.reconcileHeadlessService(ctx, sc); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcileHeadlessService: %w", err)
	}

	// 5. Reconcile ClusterIP Service.
	if err := r.reconcileClusterIPService(ctx, sc); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcileClusterIPService: %w", err)
	}

	// 6. Reconcile StatefulSet.
	if err := r.reconcileStatefulSet(ctx, sc); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcileStatefulSet: %w", err)
	}

	// 7. Bootstrap initial directory entries via LDAP once pod-0 is ready.
	//    Sets sc.Status.BootstrapComplete = true on success; status persisted below.
	if err := r.reconcileBootstrap(ctx, sc); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcileBootstrap: %w", err)
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

	switch {
	case ready == 0:
		sc.Status.Phase = ldapv1alpha1.PhaseBootstrapping
	case ready < desired:
		sc.Status.Phase = ldapv1alpha1.PhaseDegraded
	case !sc.Status.BootstrapComplete:
		sc.Status.Phase = ldapv1alpha1.PhaseBootstrapping
	default:
		sc.Status.Phase = ldapv1alpha1.PhaseRunning
	}

	readyStatus := metav1.ConditionFalse
	readyReason := "NotReady"
	readyMsg := fmt.Sprintf("%d/%d replicas ready", ready, desired)
	if ready >= desired && sc.Status.BootstrapComplete {
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

	if err := r.Status().Update(ctx, sc); err != nil {
		return ctrl.Result{}, err
	}

	// 9. Requeue until fully Running.
	if sc.Status.Phase != ldapv1alpha1.PhaseRunning {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// reconcileSecret creates the credential secrets:
//   - <name>-credentials  (plaintext admin-password + root-password; operator use)
//   - <name>-passwords    (SSHA hashes; init container use)
//
// Both are create-only. If spec.ldap.credentialsSecretName is set, plaintexts are
// read from the user-provided secret instead of auto-generated.
func (r *SlapdClusterReconciler) reconcileSecret(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) error {
	hashName := sc.Name + "-passwords"

	// If <name>-passwords already exists, secrets are already in order.
	hashSecret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Name: hashName, Namespace: sc.Namespace}, hashSecret); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}

	// Obtain plaintext passwords.
	var adminPW, rootPW string

	if sc.Spec.LDAP.CredentialsSecretName != "" {
		// Read from the user-provided secret.
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
	} else {
		// Auto-generate and store in <name>-credentials (create-only).
		credName := sc.Name + "-credentials"
		credSecret := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{Name: credName, Namespace: sc.Namespace}, credSecret); err == nil {
			// Already exists — read back the stored passwords.
			adminPW = string(credSecret.Data["admin-password"])
			rootPW = string(credSecret.Data["root-password"])
		} else if errors.IsNotFound(err) {
			var genErr error
			adminPW, genErr = generatePassword(24)
			if genErr != nil {
				return fmt.Errorf("generate admin password: %w", genErr)
			}
			rootPW, genErr = generatePassword(24)
			if genErr != nil {
				return fmt.Errorf("generate root password: %w", genErr)
			}
			cred := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: credName, Namespace: sc.Namespace},
				Type:       corev1.SecretTypeOpaque,
				StringData: map[string]string{
					"admin-password": adminPW,
					"root-password":  rootPW,
				},
			}
			if err := controllerutil.SetControllerReference(sc, cred, r.Scheme); err != nil {
				return err
			}
			if err := r.Create(ctx, cred); err != nil {
				return err
			}
		} else {
			return err
		}
	}

	// Hash the passwords and create <name>-passwords.
	adminHash, err := generateSSHAHash(adminPW)
	if err != nil {
		return fmt.Errorf("hash admin password: %w", err)
	}
	rootHash, err := generateSSHAHash(rootPW)
	if err != nil {
		return fmt.Errorf("hash root password: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: hashName, Namespace: sc.Namespace},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"admin-password-hash": adminHash,
			"root-password-hash":  rootHash,
		},
	}
	if err := controllerutil.SetControllerReference(sc, secret, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, secret)
}

// reconcileReplicationSecret creates the <name>-replication Secret with a randomly generated
// password when replication is enabled. It never updates an existing Secret.
func (r *SlapdClusterReconciler) reconcileReplicationSecret(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) error {
	if !sc.Spec.Replication.Enabled {
		return nil
	}

	name := sc.Name + "-replication"
	existing := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: sc.Namespace}, existing)
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return err
	}

	pw, err := generatePassword(32)
	if err != nil {
		return fmt.Errorf("generate replication password: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: sc.Namespace,
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"password": pw,
		},
	}
	if err := controllerutil.SetControllerReference(sc, secret, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, secret)
}

// reconcileHeadlessService creates or updates the headless Service (clusterIP: None).
// Named <name>-headless so the bare <name> can be used for the ClusterIP service.
func (r *SlapdClusterReconciler) reconcileHeadlessService(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sc.Name + "-headless",
			Namespace: sc.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Spec.ClusterIP = corev1.ClusterIPNone
		svc.Spec.Selector = selectorLabels(sc.Name)
		svc.Spec.Ports = []corev1.ServicePort{
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
		}
		return controllerutil.SetControllerReference(sc, svc, r.Scheme)
	})
	return err
}

// reconcileClusterIPService creates or updates the ClusterIP Service.
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
		ObjectMeta: metav1.ObjectMeta{
			Name:      sc.Name,
			Namespace: sc.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Spec.Type = svcType
		svc.Spec.Selector = selectorLabels(sc.Name)
		svc.Spec.Ports = []corev1.ServicePort{
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
		}
		return controllerutil.SetControllerReference(sc, svc, r.Scheme)
	})
	return err
}

// reconcileStatefulSet creates or updates the StatefulSet.
func (r *SlapdClusterReconciler) reconcileStatefulSet(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) error {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sc.Name,
			Namespace: sc.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		sts.Spec = r.buildStatefulSetSpec(sc)
		return controllerutil.SetControllerReference(sc, sts, r.Scheme)
	})
	return err
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

	// Add admin entry.  The password hash is already in <name>-passwords (created by
	// reconcileSecret) so we reuse it here rather than hashing again.
	adminHash, err := r.getAdminHash(ctx, sc)
	if err != nil {
		return err
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
		replSecret := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{
			Name:      sc.Name + "-replication",
			Namespace: sc.Namespace,
		}, replSecret); err != nil {
			return fmt.Errorf("read replication secret: %w", err)
		}
		replPW := string(replSecret.Data["password"])
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

	// The hashed-password secret is always <name>-passwords (created by reconcileSecret).
	secretName := sc.Name + "-passwords"

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

	// ── Init container ────────────────────────────────────────────────────────
	initEnv := []corev1.EnvVar{
		{Name: "LDAP_DOMAIN_DC", Value: sc.Spec.LDAP.Domain},
		{
			Name: "LDAP_ADMIN_PW_HASH",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
					Key:                  "admin-password-hash",
				},
			},
		},
		{
			Name: "LDAP_ROOT_PW_HASH",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
					Key:                  "root-password-hash",
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
						LocalObjectReference: corev1.LocalObjectReference{Name: sc.Name + "-replication"},
						Key:                  "password",
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

// getAdminPassword reads the plaintext admin password from <name>-credentials
// (or spec.ldap.credentialsSecretName when set).
func (r *SlapdClusterReconciler) getAdminPassword(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) (string, error) {
	secretName := sc.Spec.LDAP.CredentialsSecretName
	if secretName == "" {
		secretName = sc.Name + "-credentials"
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Name: secretName, Namespace: sc.Namespace}, secret); err != nil {
		return "", fmt.Errorf("read credentials secret %s: %w", secretName, err)
	}
	pw := string(secret.Data["admin-password"])
	if pw == "" {
		return "", fmt.Errorf("secret %s is missing admin-password key", secretName)
	}
	return pw, nil
}

// getAdminHash reads the SSHA admin password hash from <name>-passwords.
func (r *SlapdClusterReconciler) getAdminHash(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) (string, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Name: sc.Name + "-passwords", Namespace: sc.Namespace}, secret); err != nil {
		return "", fmt.Errorf("read passwords secret: %w", err)
	}
	hash := string(secret.Data["admin-password-hash"])
	if hash == "" {
		return "", fmt.Errorf("secret %s-passwords is missing admin-password-hash key", sc.Name)
	}
	return hash, nil
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
