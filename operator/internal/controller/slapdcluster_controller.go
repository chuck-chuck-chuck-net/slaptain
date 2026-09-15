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
	"encoding/json"
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
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
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

	// csnCheckInterval is the periodic reconcile interval for CSN convergence
	// monitoring when the cluster is Running with replication enabled. Without
	// this, CSN checks only run on resource changes and go stale.
	csnCheckInterval = 60 * time.Second
)

// SlapdClusterReconciler reconciles a SlapdCluster object.
// It manages infrastructure only: StatefulSet, Services, cn=config credentials.
// Database, schema, and ACL management are handled by SlapdDatabase and
// SlapdSchema controllers. See ADR-004.
type SlapdClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// DefaultImageTag is the tag substituted into SlapdCluster.spec.images.{slapd,init}
	// when the user leaves them blank. Wired from the OPERATOR_IMAGE_TAG env in
	// the operator Deployment (Helm chart injects it from Chart.AppVersion), so
	// "unpinned" data-plane images track the operator's own version. Empty falls
	// back to "latest" so `make run` outside a cluster still works.
	DefaultImageTag string
	// OperatorImage is the operator's own image reference, used for the download
	// container of restore Jobs (ADR-014). Wired from the OPERATOR_IMAGE env.
	OperatorImage string
	// ClusterDomain is the Kubernetes DNS domain (e.g. "cluster.local" or
	// "k8s.example") used to build every pod FQDN. Resolved once at startup
	// via ResolveClusterDomain and injected from main. See ADR-015.
	ClusterDomain string
}

// imageRef builds the "repository:tag" image reference for a data-plane image,
// falling back to the operator's running tag (then "latest") when the tag is
// unpinned and to the operator-derived default repository when the repository
// is unset. defaultRepo is the repository to use when img.Repository is empty
// (see defaultDataPlaneRepo).
func (r *SlapdClusterReconciler) imageRef(img ldapv1alpha1.SlapdImageConfig, defaultRepo string) string {
	return resolveImageRef(img, defaultRepo, r.DefaultImageTag)
}

// canonicalRegistryPath is the upstream registry/path under which the slaptain
// images are published. Used only as a last-resort fallback when the operator's
// own image reference (OPERATOR_IMAGE) is unset (e.g. `make run` locally).
const canonicalRegistryPath = "ghcr.io/chuck-chuck-chuck-net/slaptain"

// resolveImageRef builds "repository:tag" for a data-plane image, applying the
// repository and tag defaults. Empty repository → defaultRepo; empty tag →
// defaultTag, then "latest".
func resolveImageRef(img ldapv1alpha1.SlapdImageConfig, defaultRepo, defaultTag string) string {
	repo := img.Repository
	if repo == "" {
		repo = defaultRepo
	}
	tag := img.Tag
	if tag == "" {
		tag = defaultTag
	}
	if tag == "" {
		tag = "latest"
	}
	return repo + ":" + tag
}

// defaultDataPlaneRepo derives the default repository for a data-plane image
// (component "slapd" or "slapd-init") from the operator's own image reference,
// so unpinned operand images live in the same registry/path as the operator.
// It swaps the trailing path segment of the operator repository (e.g.
// ".../slaptain/operator") for the component name (".../slaptain/slapd"). Falls
// back to the canonical upstream path when operatorImage is unset (`make run`).
func defaultDataPlaneRepo(operatorImage, component string) string {
	base := stripImageTag(operatorImage)
	if base == "" {
		return canonicalRegistryPath + "/" + component
	}
	if i := strings.LastIndex(base, "/"); i >= 0 {
		return base[:i+1] + component
	}
	return component
}

// stripImageTag returns the repository portion of an "repository:tag" or
// "repository@sha256:..." image reference. It only treats a colon as a tag
// separator when it appears after the last slash, so registry ports
// (host:5000/path) are preserved.
func stripImageTag(imageRef string) string {
	if imageRef == "" {
		return ""
	}
	if i := strings.Index(imageRef, "@"); i >= 0 {
		imageRef = imageRef[:i]
	}
	slash := strings.LastIndex(imageRef, "/")
	if colon := strings.LastIndex(imageRef, ":"); colon > slash {
		imageRef = imageRef[:colon]
	}
	return imageRef
}

// +kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapdclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapdclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapdclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapddatabases,verbs=get;list;watch
// +kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapddatabases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapdbackups,verbs=get;list;watch
// +kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapdrestores,verbs=get;list;watch
// +kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapdrestores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete

func (r *SlapdClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	_ = log // used in discovery status reporting

	// 1. Fetch the SlapdCluster resource.
	sc := &ldapv1alpha1.SlapdCluster{}
	if err := r.Get(ctx, req.NamespacedName, sc); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 1a. Honor spec.suspend — leave everything in place, stop observing.
	if sc.Spec.Suspend {
		log.Info("reconciliation suspended via spec.suspend; leaving owned resources untouched")
		return ctrl.Result{}, nil
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

	// Restore coordination (ADR-014): when a bootstrapFrom restore is in
	// progress, drive the scale-to-0 → slapadd → scale-up machine and skip the
	// normal ready-based phase logic. The StatefulSet replica count itself is
	// forced to 0 by buildStatefulSetSpec (sc.RestoreHoldsDown) one pass later.
	if handled, res, err := r.reconcileRestore(ctx, sc, sts); err != nil {
		return ctrl.Result{}, err
	} else if handled {
		if err := r.applyStatus(ctx, sc); err != nil {
			return ctrl.Result{}, err
		}
		return res, nil
	}

	// ReplicationMode reflects what the controller is reconciling toward. In
	// steady state, equals spec.replication.mode (or "peer" by default). 3d
	// (in-place promotion/demotion) will let this lag spec briefly during a
	// transition; for now it tracks spec verbatim.
	mode := sc.Spec.Replication.Mode
	if mode == "" {
		mode = "peer"
	}
	sc.Status.ReplicationMode = mode

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

	// Discover replication network IPs from Multus annotations (multus mode only;
	// pod-routed uses primary pod IPs discovered per-peer and headless DNS in-cluster).
	sc.Status.ReplicationNetworkIPs = nil
	if sc.NetworkMode() == ldapv1alpha1.NetworkModeMultus {
		podList := &corev1.PodList{}
		if err := r.List(ctx, podList,
			client.InNamespace(sc.Namespace),
			client.MatchingLabels(map[string]string{
				"app.kubernetes.io/name":     "slapd",
				"app.kubernetes.io/instance": sc.Name,
			}),
		); err == nil {
			sc.Status.ReplicationNetworkIPs = discoverReplicationNetworkIPs(
				podList.Items, sc.Spec.Replication.Network.MultusNetwork)
		}
	}

	// Local CSN convergence check + gather local newest CSN for cross-site comparison.
	// Uses the replication bind DN/password since ACLs may deny anonymous access.
	dbInfos := r.listDatabaseInfo(ctx, sc)

	// Server-global tunables, converged on every pod (ADR-002, ADR-024 R1/R6).
	// Only attempted once at least one pod is ready: during bootstrap there is
	// nothing to bind to, and the reconcile is requeued every 10s until Running
	// anyway.
	if ready > 0 {
		r.reconcileTunables(ctx, sc)
	}
	// Newest local CSN PER DATABASE: a peer's database can only be compared
	// against the same database here, never against whichever of ours wrote
	// most recently.
	var localNewest map[string]time.Time
	if sc.Spec.Replication.Enabled && ready >= 2 && len(dbInfos) > 0 {
		localNewest = r.checkLocalCSNConvergence(ctx, sc, dbInfos)
	}

	// External peer status: discovery, connectivity, and CSN convergence.
	sc.Status.ExternalPeerStatuses = nil
	for _, ep := range sc.Spec.Replication.ExternalPeers {
		status := ldapv1alpha1.ExternalPeerStatus{
			Name: ep.Name,
		}
		if ep.Discovery != nil {
			// Dynamic discovery mode: query remote k8s API for pod Multus IPs.
			addrs, err := r.discoverRemotePeerAddresses(ctx, sc, &ep)
			if err != nil {
				status.LastError = err.Error()
				log.Info("remote peer discovery failed", "peer", ep.Name, "err", err)
			} else {
				status.DiscoveredAddresses = addrs
			}
		} else if len(ep.PodAddresses) > 0 {
			// Static podAddresses: addresses are in the spec directly.
		} else if ep.URI != "" {
			if err := testExternalPeerConnectivity(ep.URI); err != nil {
				status.LastError = err.Error()
			}
		}

		// CSN convergence check against this peer.
		if len(dbInfos) > 0 && len(localNewest) > 0 {
			r.checkPeerCSNConvergence(ctx, sc, &ep, &status, dbInfos, localNewest)
		}

		// Derive Connected from ReplicationState (backward compat).
		status.Connected = status.ReplicationState != "" && status.ReplicationState != ldapv1alpha1.ReplicationUnreachable

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

	// Replication mode wiring conditions (ADR-010). These are informational
	// handoffs to the human operating the cross-cluster migration; slaptain
	// cannot autonomously verify that the external peer (typically a legacy
	// prod cluster) has performed the reciprocal configuration. The conditions
	// surface in `kubectl describe slapdcluster` to make the handoff explicit.
	emitWiringConditions(sc)

	// SSA patch on the status subresource: no resourceVersion check, no conflict possible.
	if err := r.applyStatus(ctx, sc); err != nil {
		return ctrl.Result{}, err
	}

	// Requeue until fully Running.
	if sc.Status.Phase != ldapv1alpha1.PhaseRunning {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Periodic requeue for CSN convergence monitoring when replication is active.
	// Without this, CSN checks only run on resource changes and go stale once
	// the cluster stabilises.
	if sc.Spec.Replication.Enabled {
		return ctrl.Result{RequeueAfter: csnCheckInterval}, nil
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
	ac, err := applyConfiguration(svc)
	if err != nil {
		return err
	}
	return r.Apply(ctx, ac, client.ForceOwnership, client.FieldOwner(fieldManager))
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
			Name:        sc.Name,
			Namespace:   sc.Namespace,
			Annotations: sc.Spec.Service.Annotations,
		},
		Spec: corev1.ServiceSpec{
			Type:                     svcType,
			Selector:                 selectorLabels(sc.Name),
			LoadBalancerIP:           sc.Spec.Service.LoadBalancerIP,
			LoadBalancerSourceRanges: sc.Spec.Service.LoadBalancerSourceRanges,
			ExternalTrafficPolicy:    sc.Spec.Service.ExternalTrafficPolicy,
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
	ac, err := applyConfiguration(svc)
	if err != nil {
		return err
	}
	return r.Apply(ctx, ac, client.ForceOwnership, client.FieldOwner(fieldManager))
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
	if err := r.adoptImmutableSTSFields(ctx, sts); err != nil {
		return err
	}
	ac, err := applyConfiguration(sts)
	if err != nil {
		return err
	}
	return r.Apply(ctx, ac, client.ForceOwnership, client.FieldOwner(fieldManager))
}

// adoptImmutableSTSFields preserves the StatefulSet's immutable fields from
// the cluster's stored copy when the STS already exists. K8s forbids updating
// volumeClaimTemplates, serviceName, selector, and podManagementPolicy on an
// existing StatefulSet (see appsv1 validation), so re-asserting our computed
// values on every reconcile is a footgun: if any input that contributes to
// those fields drifts between when the STS was first created and now —
// kubebuilder defaults landed, an operator-side fallback constant changed,
// new computed field — k8s rejects the apply with "Forbidden: updates to
// statefulset spec for fields other than ...".
//
// The fix is the standard SSA pattern for objects with immutable fields:
// adopt-from-storage rather than re-compute. Initial create stamps the spec
// from buildStatefulSetSpec; subsequent reconciles take the stored values and
// pass them through unchanged, so SSA sees "no diff" on those fields and the
// apply succeeds for mutable fields only (template, replicas, etc.).
//
// If the user genuinely wants to change one of these — e.g., a different PVC
// size for a fresh install — the operator can't help. They'd need to delete
// the STS (cascade=orphan keeps the pods + PVCs) and let the operator
// recreate it, OR resize the PVCs directly via VolumeExpansion (which is a
// separate, supported workflow).
func (r *SlapdClusterReconciler) adoptImmutableSTSFields(ctx context.Context, desired *appsv1.StatefulSet) error {
	existing := &appsv1.StatefulSet{}
	err := r.Get(ctx, client.ObjectKey{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if err != nil {
		// NotFound = first create, our spec is authoritative for these fields.
		// Any other error: let the caller propagate; the next reconcile retries.
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("read existing StatefulSet %s for immutable-field adoption: %w", desired.Name, err)
	}
	desired.Spec.VolumeClaimTemplates = existing.Spec.VolumeClaimTemplates
	desired.Spec.ServiceName = existing.Spec.ServiceName
	desired.Spec.Selector = existing.Spec.Selector
	desired.Spec.PodManagementPolicy = existing.Spec.PodManagementPolicy
	return nil
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
	ac, err := applyConfiguration(svc)
	if err != nil {
		return err
	}
	return r.Apply(ctx, ac, client.ForceOwnership, client.FieldOwner(fieldManager))
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
	ac, err := applyConfiguration(svc)
	if err != nil {
		return err
	}
	return r.Apply(ctx, ac, client.ForceOwnership, client.FieldOwner(fieldManager))
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
	if err := r.adoptImmutableSTSFields(ctx, sts); err != nil {
		return err
	}
	ac, err := applyConfiguration(sts)
	if err != nil {
		return err
	}
	return r.Apply(ctx, ac, client.ForceOwnership, client.FieldOwner(fieldManager))
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

	// Restore hold-down: an in-progress bootstrapFrom restore takes the
	// StatefulSet(s) to 0 so offline slapadd can run (ADR-014). Only the replica
	// count changes — the rest of the pod template is identical, so scaling back
	// up triggers no spurious rollout.
	if sc.RestoreHoldsDown() {
		replicas = 0
	}

	logLevel := strconv.Itoa(int(desiredLogLevel(sc)))
	// Three orthogonal gates:
	//
	//   accesslogMountNeeded — mount /accesslog into init + main containers.
	//     Only true when an accesslog DB might exist (peer-eligible with
	//     consumers AND DeltaSync). Empty in plain-syncrepl-only providers.
	//     The accesslog VOLUME (PVC or emptyDir) is provisioned unconditionally
	//     on every RW pod regardless of this gate — see the volumes block
	//     below for the immutability rationale.
	//
	//   replicationActive — the cluster participates in replication in some
	//     way (provider, consumer, or both). Triggers
	//     LDAP_REPLICATION_ENABLED=true on the init container, which loads
	//     syncprov + accesslog modules so slapd recognizes their objectClasses
	//     at runtime ldapadd time. Boot-time loading is only the fast path:
	//     modules CAN be loaded into a running slapd via ldapmodify on
	//     cn=module, and the SlapdDatabase controller does exactly that
	//     (ensureModulesLoaded) for pods bootstrapped before replication was
	//     enabled — see docs/reconcile-loop-fixes.md (2026-07-15). The boot
	//     gate stays broad (any cluster with replication.enabled=true,
	//     regardless of topology) so fresh pods don't depend on a reconcile
	//     pass for their modules.
	accesslogMountNeeded := sc.NeedsAccesslogVolume()
	replicationActive := sc.Spec.Replication.Enabled

	// Pod security context. PSA "restricted" profile is the floor: even when
	// the user supplies their own SecurityContext, we layer RunAsNonRoot and
	// SeccompProfile onto it so the pod stays admissible to restricted
	// namespaces. Users can still override the user/group/fsGroup.
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
	if podSecCtx.RunAsNonRoot == nil {
		trueVal := true
		podSecCtx.RunAsNonRoot = &trueVal
	}
	if podSecCtx.SeccompProfile == nil {
		podSecCtx.SeccompProfile = &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		}
	}

	// ── Volumes (non-PVC) ─────────────────────────────────────────────────────
	volumes := []corev1.Volume{
		{
			Name:         "run",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		{
			// Writable /tmp for the init container's bootstrap.sh ($TMP_CONF).
			// Lets us run init with readOnlyRootFilesystem=true.
			Name:         "tmp",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
	}

	// PVC size/access-mode fallbacks. The CRD's +kubebuilder:default="1Gi" on
	// SlapdPVCConfig.Size means in normal operation these branches never
	// fire — the API server stamps "1Gi" at admission. The fallbacks are
	// kept as belt-and-braces for `make run` outside a cluster (no
	// admission defaulting) and for forward-compat if the CRD default
	// ever changes. Critically: they must MATCH the CRD default. A skew
	// is silently load-bearing — the operator generates whatever shape
	// matched at first-create-time, and StatefulSet volumeClaimTemplates
	// are immutable, so a later mismatch makes the cluster controller
	// hard-fail with "Forbidden: updates to statefulset spec".
	cfgSize := sc.Spec.Persistence.Config.Size
	if cfgSize == "" {
		cfgSize = "1Gi"
	}
	dataSize := sc.Spec.Persistence.Data.Size
	if dataSize == "" {
		dataSize = "1Gi"
	}
	cfgAM := sc.Spec.Persistence.Config.AccessMode
	if cfgAM == "" {
		cfgAM = corev1.ReadWriteOnce
	}
	dataAM := sc.Spec.Persistence.Data.AccessMode
	if dataAM == "" {
		dataAM = corev1.ReadWriteOnce
	}
	volumeClaimTemplates := []corev1.PersistentVolumeClaim{
		pvcTemplate("config", cfgSize, sc.Spec.Persistence.Config.StorageClass, cfgAM),
		pvcTemplate("data", dataSize, sc.Spec.Persistence.Data.StorageClass, dataAM),
	}
	// Accesslog PVC: provisioned on every RW pod regardless of current
	// replication state. StatefulSet volumeClaimTemplates is immutable, so
	// adding accesslog later (when the user flips replication.enabled or
	// adds an externalPeer) would otherwise require a manual STS recreate
	// with --cascade=orphan. The cost is a ~1Gi PVC sitting unused in
	// standalone clusters. The MOUNT is still gated on accesslogMountNeeded
	// so slapd doesn't see an empty /accesslog when there's no DB for it.
	if !readOnly {
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
		{Name: "CONFIG_DIR", Value: "/config"},
		{Name: "DATA_DIR", Value: "/data"},
		{Name: "ACCESSLOG_DIR", Value: ldapv1alpha1.AccesslogRoot},
		{Name: "DATABASE_DIRS", Value: strings.Join(databaseNames, ",")},
	}

	// back-mdb BACKEND configuration (olcBackend={0}mdb) — bootstrap-time by
	// construction, since the backend is initialised before any database
	// exists, and the IDL exponent governs on-disk index layout so a late
	// write would apply to only part of the database (ADR-024 R2). Consumed by
	// bootstrap.sh's `backend mdb` stanza, which only runs on a FRESH /config
	// volume; changing this field later is a recreate, documented on the field.
	if exp, ok := mdbIdlExponent(sc); ok {
		initEnv = append(initEnv,
			corev1.EnvVar{Name: "LDAP_MDB_IDL_EXP", Value: strconv.Itoa(int(exp))},
		)
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
	} else if replicationActive {
		// Load syncprov + accesslog modules regardless of mode (peer or
		// consumer-only) so promotion / runtime overlay-add succeeds without
		// a pod restart. Modules can only be loaded at slapd startup.
		initEnv = append(initEnv,
			corev1.EnvVar{Name: "LDAP_REPLICATION_ENABLED", Value: "true"},
		)
	}

	// ServerID coordination (ADR-011 sid-1-per-default; ADR-017 bare form):
	// emit this pod's own serverID as a bare integer on EVERY RW pod, including
	// standalone clusters ("serverID 1"). A pod's sid is identity, not
	// capability — carrying it from birth keeps the CSN history uniform (no
	// sid-0 epoch). cn=config is node-local (ADR-002), so the pod needs only
	// its own ID; bootstrap.sh derives the ordinal from $HOSTNAME, needing just
	// the base (no FQDN/domain — that self-match was the ADR-015 crash surface).
	// LDAP_REPLICAS stays as the "operator-managed RW pod" gate. The
	// SlapdDatabase controller reconciles the value at runtime (ensureServerIDs),
	// so this is only the fresh-bootstrap fast path. External-peer ID
	// coordination during hot migration is handled separately via ExternalPeer
	// config.
	if !readOnly {
		initEnv = append(initEnv,
			corev1.EnvVar{Name: "LDAP_REPLICAS", Value: strconv.Itoa(int(sc.Spec.Replicas))},
			corev1.EnvVar{Name: "LDAP_SERVER_ID_BASE", Value: strconv.Itoa(int(sc.Spec.Replication.ServerIDBase))},
		)
	}

	initMounts := []corev1.VolumeMount{
		{Name: "config", MountPath: "/config"},
		{Name: "data", MountPath: "/data"},
		{Name: "tmp", MountPath: "/tmp"},
	}
	if accesslogMountNeeded && !readOnly {
		initMounts = append(initMounts, corev1.VolumeMount{
			Name:      "accesslog",
			MountPath: ldapv1alpha1.AccesslogRoot,
		})
	}
	if sc.Spec.LDAP.TLS.Enabled {
		initMounts = append(initMounts, corev1.VolumeMount{
			Name:      "tls",
			MountPath: "/etc/openldap/tls",
			ReadOnly:  true,
		})
	}

	initImage := r.imageRef(sc.Spec.Images.Init, defaultDataPlaneRepo(r.OperatorImage, "slapd-init"))

	// Init container hardening for PSA "restricted". readOnlyRootFilesystem is
	// enabled because bootstrap.sh's $TMP_CONF=/tmp/slapd.conf write target is
	// backed by the "tmp" emptyDir volume mounted at /tmp above.
	initFalse := false
	initTrue := true
	initSecCtx := &corev1.SecurityContext{
		AllowPrivilegeEscalation: &initFalse,
		ReadOnlyRootFilesystem:   &initTrue,
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}

	initContainer := corev1.Container{
		Name:            "init",
		Image:           initImage,
		ImagePullPolicy: sc.Spec.Images.Init.PullPolicy,
		Env:             initEnv,
		VolumeMounts:    initMounts,
		SecurityContext: initSecCtx,
	}

	// ── Main container ────────────────────────────────────────────────────────
	mainMounts := []corev1.VolumeMount{
		{Name: "config", MountPath: "/config"},
		{Name: "data", MountPath: "/data"},
		{Name: "run", MountPath: "/run/openldap"},
	}
	if accesslogMountNeeded && !readOnly {
		mainMounts = append(mainMounts, corev1.VolumeMount{
			Name:      "accesslog",
			MountPath: ldapv1alpha1.AccesslogRoot,
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
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}

	mainImage := r.imageRef(sc.Spec.Images.Slapd, defaultDataPlaneRepo(r.OperatorImage, "slapd"))

	mainContainer := corev1.Container{
		Name:            "slapd",
		Image:           mainImage,
		ImagePullPolicy: sc.Spec.Images.Slapd.PullPolicy,
		Args: []string{
			"-h", "ldap://:1024/ ldaps://:1025/ ldapi://%2frun%2fopenldap%2fslapd.ldapi",
			"-d", logLevel,
			"-F", "/config/slapd.d",
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
				Labels:      labels,
				Annotations: buildMultusAnnotations(sc),
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

// databaseCSNInfo holds the suffix and replication credentials for CSN queries.
type databaseCSNInfo struct {
	suffix string
	// bindDNs are the identities to try, in order (csnBindDNs): the node-local
	// replication identity first, the legacy cn=replication,<suffix> second.
	// Both are granted read by the replication ACL for the length of ADR-027's
	// migration window.
	bindDNs []string
	bindPW  string
}

// listDatabaseInfo returns suffix and replication bind credentials for all
// SlapdDatabase CRs referencing this cluster. The replication bind identities
// have read access granted by the replication ACL, so CSN queries work even
// when anonymous access is denied.
func (r *SlapdClusterReconciler) listDatabaseInfo(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) []databaseCSNInfo {
	log := logf.FromContext(ctx)
	var dbList ldapv1alpha1.SlapdDatabaseList
	if err := r.List(ctx, &dbList, client.InNamespace(sc.Namespace)); err != nil {
		return nil
	}
	var infos []databaseCSNInfo
	for _, db := range dbList.Items {
		if db.Spec.ClusterRef != sc.Name || db.Spec.Suffix == "" {
			continue
		}
		info := databaseCSNInfo{
			suffix:  db.Spec.Suffix,
			bindDNs: csnBindDNs(db.Name, db.Spec.Suffix),
		}
		// Read replication password from the database credentials secret.
		secretName := db.Name + "-credentials"
		if db.Spec.Credentials.SecretName != "" {
			secretName = db.Spec.Credentials.SecretName
		}
		secret := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{
			Name: secretName, Namespace: sc.Namespace,
		}, secret); err == nil {
			info.bindPW = string(secret.Data["replication-password"])
		}
		log.V(1).Info("CSN query credentials",
			"db", db.Name, "suffix", db.Spec.Suffix,
			"bindDNs", info.bindDNs, "bindPWLen", len(info.bindPW),
			"secret", secretName)
		infos = append(infos, info)
	}
	return infos
}

// checkLocalCSNConvergence queries contextCSN on all local RW pods, for every
// database, and sets the ReplicationConverged condition from the per-database
// verdict (see evaluateLocalConvergence). Returns the newest local CSN
// timestamp PER DATABASE SUFFIX, the baseline cross-site comparison uses.
func (r *SlapdClusterReconciler) checkLocalCSNConvergence(
	ctx context.Context,
	sc *ldapv1alpha1.SlapdCluster,
	dbInfos []databaseCSNInfo,
) map[string]time.Time {
	log := logf.FromContext(ctx)
	headlessSvc := sc.Name + "-headless"
	tlsEnabled := sc.Spec.LDAP.TLS.Enabled
	port := ldapContainerPort
	if tlsEnabled {
		port = ldapsContainerPort
	}

	var readings []csnReading
	for i := int32(0); i < sc.Spec.Replicas; i++ {
		host := fmt.Sprintf("%s-%d.%s.%s.svc.%s", sc.Name, i, headlessSvc, sc.Namespace, r.ClusterDomain)
		podName := fmt.Sprintf("%s-%d", sc.Name, i)
		for _, db := range dbInfos {
			csns, err := queryContextCSN(host, port, tlsEnabled, db.suffix, db.bindDNs, db.bindPW)
			if err != nil {
				log.V(1).Info("local CSN query failed", "pod", host, "suffix", db.suffix, "err", err)
				readings = append(readings, csnReading{Pod: podName, Suffix: db.suffix, Err: err.Error()})
				continue
			}
			readings = append(readings, csnReading{Pod: podName, Suffix: db.suffix, CSNs: csns})
		}
	}

	verdict := evaluateLocalConvergence(readings, sc.Spec.Replicas)
	if !verdict.Silent {
		setCondition(&sc.Status.Conditions, metav1.Condition{
			Type:               "ReplicationConverged",
			Status:             verdict.Status,
			Reason:             verdict.Reason,
			Message:            verdict.Message,
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: sc.Generation,
		})
	}

	return verdict.NewestBySuffix
}

// csnSyncThreshold is the maximum CSN lag considered "Synced".
// Cross-site delta-syncrepl has inherent propagation delay.
const csnSyncThreshold = 5 * time.Second

// checkPeerCSNConvergence queries contextCSN on a remote peer's pods and
// populates the ReplicationState, LagSeconds, and LastChecked fields.
func (r *SlapdClusterReconciler) checkPeerCSNConvergence(
	ctx context.Context,
	sc *ldapv1alpha1.SlapdCluster,
	ep *ldapv1alpha1.ExternalPeer,
	status *ldapv1alpha1.ExternalPeerStatus,
	dbInfos []databaseCSNInfo,
	localNewest map[string]time.Time,
) {
	log := logf.FromContext(ctx)
	now := metav1.Now()
	status.LastChecked = &now

	tlsEnabled := sc.Spec.LDAP.TLS.Enabled
	port := ep.Port
	if port == 0 {
		if tlsEnabled {
			port = ldapsContainerPort
		} else {
			port = ldapContainerPort
		}
	}

	// Determine remote addresses to query.
	var addrs []string
	if ep.Discovery != nil {
		addrs = status.DiscoveredAddresses
	} else if len(ep.PodAddresses) > 0 {
		addrs = ep.PodAddresses
	} else if ep.URI != "" {
		// URI mode: query contextCSN on the single URI directly — every
		// database, not just the first one that answers. The verdict comes from
		// the same seam as every other mode, so one database cannot decide a
		// peer's state on behalf of the others.
		var readings []csnReading
		for _, db := range dbInfos {
			csns, err := queryContextCSNFromURI(ep.URI, db.suffix, db.bindDNs, db.bindPW)
			if err != nil {
				log.V(1).Info("remote CSN query failed (URI)", "peer", ep.Name,
					"suffix", db.suffix, "err", err)
				readings = append(readings, csnReading{Pod: ep.URI, Suffix: db.suffix, Err: err.Error()})
				continue
			}
			readings = append(readings, csnReading{Pod: ep.URI, Suffix: db.suffix, CSNs: csns})
		}
		verdict := evaluatePeerConvergence(localNewest, readings, csnSyncThreshold)
		status.ReplicationState = verdict.State
		status.LagSeconds = verdict.LagSeconds
		status.LastError = verdict.LastError
		return
	}

	if len(addrs) == 0 {
		// No addresses available (discovery pending or empty podAddresses).
		status.ReplicationState = ldapv1alpha1.ReplicationUnreachable
		status.LastError = "no remote addresses available"
		return
	}

	// Query contextCSN on every remote pod, for every database.
	var readings []csnReading
	for _, addr := range addrs {
		for _, db := range dbInfos {
			csns, err := queryContextCSN(addr, port, tlsEnabled, db.suffix, db.bindDNs, db.bindPW)
			if err != nil {
				log.V(1).Info("remote CSN query failed", "peer", ep.Name, "addr", addr,
					"suffix", db.suffix, "err", err)
				readings = append(readings, csnReading{Pod: addr, Suffix: db.suffix, Err: err.Error()})
				continue
			}
			readings = append(readings, csnReading{Pod: addr, Suffix: db.suffix, CSNs: csns})
		}
	}

	verdict := evaluatePeerConvergence(localNewest, readings, csnSyncThreshold)
	status.ReplicationState = verdict.State
	status.LagSeconds = verdict.LagSeconds
	status.LastError = verdict.LastError
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
		// SlapdRestore drives an in-place restore through this controller
		// (ADR-014 Architecture A): enqueue the cluster that owns the target
		// database whenever a SlapdRestore changes.
		Watches(&ldapv1alpha1.SlapdRestore{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, obj client.Object) []ctrl.Request {
				sr, ok := obj.(*ldapv1alpha1.SlapdRestore)
				if !ok {
					return nil
				}
				db := &ldapv1alpha1.SlapdDatabase{}
				if err := r.Get(ctx, client.ObjectKey{Name: sr.Spec.DatabaseRef, Namespace: sr.Namespace}, db); err != nil {
					return nil
				}
				return []ctrl.Request{{
					NamespacedName: client.ObjectKey{
						Name:      db.Spec.ClusterRef,
						Namespace: sr.Namespace,
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

// emitWiringConditions surfaces the prod-side handoff conditions called out
// in ADR-010. They are informational — slaptain cannot verify that the
// external cluster has performed the matching configuration, so the conditions
// stand as documentation of "what the human must do next."
//
//   - ReplicationModePeerWiringRequired (Status=True) is set when the cluster
//     is in mode=peer with at least one externalPeer. The handoff: the
//     external cluster needs reciprocal syncrepl stanzas pointing at slaptain
//     and slaptain's ServerIDs in its olcServerID list.
//   - ReplicationModeDemoteWiringRequired (Status=True) is set when the
//     cluster is in mode=consumer-only with at least one externalPeer. The
//     handoff: the external cluster should drop syncrepl stanzas pointing at
//     slaptain before slaptain stops accepting writes, to avoid losing the
//     last batch of writes during the handover window.
//
// When externalPeers is empty, neither condition applies and both are removed
// from the status (mode-as-handoff is meaningless without a counterpart).
func emitWiringConditions(sc *ldapv1alpha1.SlapdCluster) {
	now := metav1.Now()
	hasPeers := len(sc.Spec.Replication.ExternalPeers) > 0

	peerCond := metav1.Condition{
		Type:               "ReplicationModePeerWiringRequired",
		Status:             metav1.ConditionFalse,
		Reason:             "NotApplicable",
		Message:            "Cluster is not in peer mode with external peers; no reciprocal wiring expected.",
		LastTransitionTime: now,
		ObservedGeneration: sc.Generation,
	}
	demoteCond := metav1.Condition{
		Type:               "ReplicationModeDemoteWiringRequired",
		Status:             metav1.ConditionFalse,
		Reason:             "NotApplicable",
		Message:            "Cluster is not in consumer-only mode with external peers; no handoff required.",
		LastTransitionTime: now,
		ObservedGeneration: sc.Generation,
	}

	if hasPeers {
		switch sc.Status.ReplicationMode {
		case "peer":
			peerCond.Status = metav1.ConditionTrue
			peerCond.Reason = "PeerModeActive"
			peerCond.Message = "External cluster(s) must add reciprocal syncrepl stanzas pointing at this slaptain cluster, and add slaptain's ServerIDs to their olcServerID list, for bidirectional replication to begin. See ADR-010 §promotion."
		case "consumer-only":
			demoteCond.Status = metav1.ConditionTrue
			demoteCond.Reason = "ConsumerOnlyModeActive"
			demoteCond.Message = "Before transitioning back to peer (or decommissioning this cluster), the external cluster(s) should drop syncrepl stanzas pointing at slaptain to avoid log spam from failed pulls. Informational only — not a transition gate."
		}
	}

	setCondition(&sc.Status.Conditions, peerCond)
	setCondition(&sc.Status.Conditions, demoteCond)
}

// setCondition upserts a condition in the conditions slice.
// LastTransitionTime is preserved when the Status field has not changed,
// per Kubernetes API conventions.
func setCondition(conditions *[]metav1.Condition, newCond metav1.Condition) {
	for i, c := range *conditions {
		if c.Type == newCond.Type {
			if c.Status == newCond.Status {
				newCond.LastTransitionTime = c.LastTransitionTime
			}
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

// buildMultusAnnotations returns pod template annotations for Multus network attachment.
// Returns nil when no Multus network is configured — including pod-routed mode
// (ADR-016), where there is no NAD and pods use only the primary network.
func buildMultusAnnotations(sc *ldapv1alpha1.SlapdCluster) map[string]string {
	n := sc.Spec.Replication.Network
	if n == nil || n.MultusNetwork == "" {
		return nil
	}
	return map[string]string{
		"k8s.v1.cni.cncf.io/networks": n.MultusNetwork,
	}
}

// multusNetworkStatus represents one entry in the k8s.v1.cni.cncf.io/network-status annotation.
type multusNetworkStatus struct {
	Name      string   `json:"name"`
	Interface string   `json:"interface"`
	IPs       []string `json:"ips"`
	Default   bool     `json:"default"`
}

// discoverReplicationNetworkIPs reads Multus network-status annotations from all pods
// in the StatefulSet and extracts IPs for the configured replication network.
func discoverReplicationNetworkIPs(pods []corev1.Pod, nadName string) map[string]string {
	result := make(map[string]string)
	for i := range pods {
		pod := &pods[i]
		ip := extractMultusIP(pod, nadName)
		if ip != "" {
			result[pod.Name] = ip
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// extractMultusIP reads a pod's network-status annotation and returns the IP for the named NAD.
// The nadName can be "namespace/name" or plain "name".
func extractMultusIP(pod *corev1.Pod, nadName string) string {
	raw, ok := pod.Annotations["k8s.v1.cni.cncf.io/network-status"]
	if !ok || raw == "" {
		return ""
	}
	var statuses []multusNetworkStatus
	if err := json.Unmarshal([]byte(raw), &statuses); err != nil {
		return ""
	}
	for _, s := range statuses {
		if s.Name == nadName && len(s.IPs) > 0 {
			return s.IPs[0]
		}
	}
	return ""
}

// discoverRemotePeerAddresses queries a remote Kubernetes cluster's API to discover
// Multus replication-network IPs of pods belonging to a remote SlapdCluster.
// This implements the dynamic peer discovery described in ADR-007 amendment.
func (r *SlapdClusterReconciler) discoverRemotePeerAddresses(
	ctx context.Context,
	sc *ldapv1alpha1.SlapdCluster,
	ep *ldapv1alpha1.ExternalPeer,
) ([]string, error) {
	if sc.Spec.Replication.Network == nil {
		return nil, fmt.Errorf("discovery requires spec.replication.network to be configured")
	}

	disc := ep.Discovery

	// Read the kubeconfig Secret.
	kubeconfigSecret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{
		Name:      disc.KubeconfigSecret.Name,
		Namespace: sc.Namespace,
	}, kubeconfigSecret); err != nil {
		return nil, fmt.Errorf("read kubeconfig secret %q: %w", disc.KubeconfigSecret.Name, err)
	}

	key := disc.KubeconfigSecret.Key
	if key == "" {
		key = "kubeconfig"
	}
	kubeconfigData, ok := kubeconfigSecret.Data[key]
	if !ok {
		return nil, fmt.Errorf("kubeconfig secret %q has no key %q", disc.KubeconfigSecret.Name, key)
	}

	// Build a client-go REST config from the kubeconfig.
	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigData)
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	restConfig.Timeout = 10 * time.Second

	// Build a typed clientset for the remote cluster.
	remoteClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("build remote client: %w", err)
	}

	// Determine remote namespace and cluster name (defaults to local values).
	remoteNS := disc.Namespace
	if remoteNS == "" {
		remoteNS = sc.Namespace
	}
	remoteCluster := disc.ClusterName
	if remoteCluster == "" {
		remoteCluster = sc.Name
	}

	// List pods on the remote cluster matching the remote SlapdCluster's labels.
	labelSelector := labels.SelectorFromSet(map[string]string{
		"app.kubernetes.io/name":     "slapd",
		"app.kubernetes.io/instance": remoteCluster,
	}).String()

	podList, err := remoteClient.CoreV1().Pods(remoteNS).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("list remote pods (ns=%s, labels=%s): %w", remoteNS, labelSelector, err)
	}

	// Extract each remote pod's replication address. In multus mode this is the
	// net1 IP from the pod's network-status annotation; in pod-routed mode (ADR-016)
	// it is the pod's primary IP, reachable via cross-site pod-CIDR routing.
	podRouted := sc.NetworkMode() == ldapv1alpha1.NetworkModePodRouted
	nadName := sc.Spec.Replication.Network.MultusNetwork
	var addresses []string
	for i := range podList.Items {
		pod := &podList.Items[i]
		// Skip pods that aren't running.
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		var ip string
		if podRouted {
			ip = pod.Status.PodIP
		} else {
			ip = extractMultusIP(pod, nadName)
		}
		if ip != "" {
			addresses = append(addresses, ip)
		}
	}

	sort.Strings(addresses)
	return addresses, nil
}
