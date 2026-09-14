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
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

const (
	schemaFieldManager = "slapdschema-controller"
)

// SlapdSchemaReconciler reconciles a SlapdSchema object.
// It ensures that all declared attributeTypes and objectClasses exist in
// cn=schema,cn=config on every pod of the referenced SlapdCluster.
// See ADR-004 and ADR-006 for design rationale.
type SlapdSchemaReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// ClusterDomain is the Kubernetes DNS domain used to build pod FQDNs for
	// per-pod LDAP connections. See ADR-015.
	ClusterDomain string
}

// +kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapdschemas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapdschemas/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapdschemas/finalizers,verbs=update

func (r *SlapdSchemaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// 1. Fetch the SlapdSchema resource.
	ss := &ldapv1alpha1.SlapdSchema{}
	if err := r.Get(ctx, req.NamespacedName, ss); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 2. Fetch the referenced SlapdCluster.
	sc := &ldapv1alpha1.SlapdCluster{}
	if err := r.Get(ctx, client.ObjectKey{
		Name:      ss.Spec.ClusterRef,
		Namespace: ss.Namespace,
	}, sc); err != nil {
		if errors.IsNotFound(err) {
			log.Info("referenced SlapdCluster not found", "clusterRef", ss.Spec.ClusterRef)
			r.setStatus(ctx, ss, false, nil, nil, "ClusterNotFound",
				fmt.Sprintf("SlapdCluster %q not found", ss.Spec.ClusterRef))
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	// 2a. Honor spec.suspend on either CR.
	if ss.Spec.Suspend || sc.Spec.Suspend {
		log.Info("reconciliation suspended", "ssSuspend", ss.Spec.Suspend, "scSuspend", sc.Spec.Suspend)
		return ctrl.Result{}, nil
	}

	// 3. Wait for cluster to be Running.
	if sc.Status.Phase != ldapv1alpha1.PhaseRunning {
		log.Info("waiting for cluster to be Running", "phase", sc.Status.Phase)
		r.setStatus(ctx, ss, false, nil, nil, "ClusterNotReady",
			fmt.Sprintf("SlapdCluster %q is %s, waiting for Running", ss.Spec.ClusterRef, sc.Status.Phase))
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// 4. Nothing to apply if spec is empty.
	if len(ss.Spec.AttributeTypes) == 0 && len(ss.Spec.ObjectClasses) == 0 {
		r.setStatus(ctx, ss, true, nil, nil, "EmptySpec", "No schema elements declared")
		return ctrl.Result{}, nil
	}

	// 5. Read cn=config admin password.
	configPW, err := r.getConfigPassword(ctx, sc)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getConfigPassword: %w", err)
	}

	// 6. Apply schema to every pod (RW + RO). cn=config is node-local (ADR-002).
	replicas := sc.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}
	headlessSvc := sc.Name + "-headless"

	var appliedPods, failedPods []string

	// RW pods.
	for i := int32(0); i < replicas; i++ {
		podName := fmt.Sprintf("%s-%d", sc.Name, i)
		host := fmt.Sprintf("%s.%s.%s.svc.%s",
			podName, headlessSvc, sc.Namespace, r.ClusterDomain)
		if err := r.applySchemaToPod(ctx, host, configPW, ss); err != nil {
			log.Info("schema apply skipped for pod (will retry)",
				"pod", podName, "err", err)
			failedPods = append(failedPods, podName)
		} else {
			appliedPods = append(appliedPods, podName)
		}
	}

	// RO pods.
	if sc.Spec.ReadReplicas > 0 {
		roHeadless := sc.Name + "-readonly-headless"
		for i := int32(0); i < sc.Spec.ReadReplicas; i++ {
			podName := fmt.Sprintf("%s-readonly-%d", sc.Name, i)
			host := fmt.Sprintf("%s.%s.%s.svc.%s",
				podName, roHeadless, sc.Namespace, r.ClusterDomain)
			if err := r.applySchemaToPod(ctx, host, configPW, ss); err != nil {
				log.Info("schema apply skipped for read-only pod (will retry)",
					"pod", podName, "err", err)
				failedPods = append(failedPods, podName)
			} else {
				appliedPods = append(appliedPods, podName)
			}
		}
	}

	// 7. Update status.
	allApplied := len(failedPods) == 0
	reason := "Applied"
	msg := fmt.Sprintf("Schema applied to %d pods", len(appliedPods))
	if !allApplied {
		reason = "PartiallyApplied"
		msg = fmt.Sprintf("Schema applied to %d pods, failed on %d pods",
			len(appliedPods), len(failedPods))
	}
	r.setStatus(ctx, ss, allApplied, appliedPods, failedPods, reason, msg)

	if !allApplied {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// applySchemaToPod connects to one pod and ensures all declared attributeTypes
// and objectClasses exist in cn=schema,cn=config. Missing elements are added;
// existing ones are skipped (desired-minimum model, ADR-006).
func (r *SlapdSchemaReconciler) applySchemaToPod(
	ctx context.Context,
	host, configPW string,
	ss *ldapv1alpha1.SlapdSchema,
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

	if err := conn.Bind("cn=admin,cn=config", configPW); err != nil {
		return fmt.Errorf("bind cn=admin,cn=config at %s: %w", host, err)
	}

	// Check if a schema sub-entry for this SlapdSchema already exists.
	// OpenLDAP auto-numbers sub-entries: cn=myschema becomes cn={4}myschema.
	// We list all sub-entries, strip the {N} prefix, and check by name.
	existingSchemaCNs, err := listSchemaCNs(conn)
	if err != nil {
		return fmt.Errorf("list schema CNs at %s: %w", host, err)
	}

	schemaCN := ss.Name
	if existingSchemaCNs[schemaCN] {
		log.V(1).Info("schema sub-entry already exists", "host", host, "cn", schemaCN)
		return nil
	}

	// Create the schema sub-entry via ldap.Add. The existence check above is
	// critical: a duplicate ADD while syncrepl threads are active deadlocks
	// slapd permanently (see docs/reconcile-loop-fixes.md). The check prevents
	// the duplicate from being issued.
	log.Info("adding schema sub-entry", "host", host, "cn", schemaCN,
		"attributeTypes", len(ss.Spec.AttributeTypes), "objectClasses", len(ss.Spec.ObjectClasses))

	dn := fmt.Sprintf("cn=%s,cn=schema,cn=config", schemaCN)
	addReq := ldap.NewAddRequest(dn, nil)
	addReq.Attribute("objectClass", []string{"olcSchemaConfig"})
	addReq.Attribute("cn", []string{schemaCN})
	if len(ss.Spec.AttributeTypes) > 0 {
		addReq.Attribute("olcAttributeTypes", ss.Spec.AttributeTypes)
	}
	if len(ss.Spec.ObjectClasses) > 0 {
		addReq.Attribute("olcObjectClasses", ss.Spec.ObjectClasses)
	}
	if err := conn.Add(addReq); err != nil {
		// Defence in depth: handle "already exists" in case of a race.
		if ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists) {
			log.V(1).Info("schema sub-entry already exists (ADD returned 68)", "host", host, "cn", schemaCN)
			return nil
		}
		return fmt.Errorf("add schema cn=%s at %s: %w", schemaCN, host, err)
	}

	return nil
}

// listSchemaCNs returns a set of schema cn values (with {N} prefix stripped)
// that exist under cn=schema,cn=config. Used for existence checks before
// adding new schema sub-entries.
func listSchemaCNs(conn *ldap.Conn) (map[string]bool, error) {
	sr, err := conn.Search(ldap.NewSearchRequest(
		"cn=schema,cn=config",
		ldap.ScopeSingleLevel,
		ldap.NeverDerefAliases,
		0, 0, false,
		"(objectClass=*)",
		[]string{"cn"},
		nil,
	))
	if err != nil {
		return nil, err
	}
	result := make(map[string]bool, len(sr.Entries))
	for _, entry := range sr.Entries {
		for _, cn := range entry.GetEqualFoldAttributeValues("cn") {
			result[stripOrderingPrefix(cn)] = true
		}
	}
	return result, nil
}

// stripOrderingPrefix removes the {N} ordering prefix from an OpenLDAP cn value.
// e.g. "{4}ox" → "ox", "{0}core" → "core", "plain" → "plain".
func stripOrderingPrefix(cn string) string {
	if len(cn) > 0 && cn[0] == '{' {
		if idx := strings.IndexByte(cn, '}'); idx >= 0 {
			return cn[idx+1:]
		}
	}
	return cn
}

// getConfigPassword reads the cn=config admin password from the cluster's secret.
func (r *SlapdSchemaReconciler) getConfigPassword(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) (string, error) {
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

// setStatus updates the SlapdSchema status via SSA.
func (r *SlapdSchemaReconciler) setStatus(
	ctx context.Context,
	ss *ldapv1alpha1.SlapdSchema,
	applied bool,
	appliedPods, failedPods []string,
	reason, message string,
) {
	// Sort for deterministic output.
	sort.Strings(appliedPods)
	sort.Strings(failedPods)

	ss.Status.Applied = applied
	ss.Status.AppliedToPods = appliedPods
	ss.Status.FailedPods = failedPods
	ss.Status.ObservedGeneration = ss.Generation

	condStatus := metav1.ConditionFalse
	if applied {
		condStatus = metav1.ConditionTrue
	}
	setCondition(&ss.Status.Conditions, metav1.Condition{
		Type:               "Applied",
		Status:             condStatus,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: ss.Generation,
	})

	statusPatch := &ldapv1alpha1.SlapdSchema{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "ldap.chuck-chuck-chuck.net/v1alpha1",
			Kind:       "SlapdSchema",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ss.Name,
			Namespace: ss.Namespace,
		},
	}
	statusPatch.Status = ss.Status
	ac, err := applyConfiguration(statusPatch)
	if err != nil {
		logf.FromContext(ctx).Error(err, "failed to build SlapdSchema status apply configuration")
		return
	}
	if err := r.Status().Apply(ctx, ac, client.ForceOwnership, client.FieldOwner(schemaFieldManager)); err != nil {
		logf.FromContext(ctx).Error(err, "failed to patch SlapdSchema status")
	}
}

// SetupWithManager sets up the controller with the Manager.
//
// Besides its own CR, the controller watches SlapdCluster: schemas are
// per-pod state (cn=config is node-local, ADR-002), so any topology change —
// scale-up, readReplicas, a cluster reaching Running — must re-trigger schema
// application, or pods created after the schema reached Applied never receive
// it and syncrepl to them fails with rc 21 on entries using the custom
// schema. Same pattern (and same bug class) as the SlapdDatabase controller's
// externalPeers watch — see docs/reconcile-loop-fixes.md (2026-04-20 and
// 2026-07-15).
func (r *SlapdSchemaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ldapv1alpha1.SlapdSchema{}).
		Watches(&ldapv1alpha1.SlapdCluster{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, obj client.Object) []ctrl.Request {
				sc, ok := obj.(*ldapv1alpha1.SlapdCluster)
				if !ok {
					return nil
				}
				// Find all SlapdSchemas that reference this cluster.
				var ssList ldapv1alpha1.SlapdSchemaList
				if err := r.List(ctx, &ssList, client.InNamespace(sc.Namespace)); err != nil {
					return nil
				}
				var reqs []ctrl.Request
				for _, ss := range ssList.Items {
					if ss.Spec.ClusterRef == sc.Name {
						reqs = append(reqs, ctrl.Request{
							NamespacedName: client.ObjectKey{
								Name:      ss.Name,
								Namespace: ss.Namespace,
							},
						})
					}
				}
				return reqs
			},
		)).
		Named("slapdschema").
		Complete(r)
}
