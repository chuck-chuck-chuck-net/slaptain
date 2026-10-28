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
	"maps"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// What the operator's cache holds (ADR-042).
//
// A controller-runtime manager starts one informer per kind a controller
// Owns() or reads through the cached client, and an informer holds EVERY
// object of its kind in the cluster. For the operator that meant every Secret
// (Helm release records included, megabytes each), every Pod and every
// Service of every tenant on a shared cluster: memory grew with the size of
// the cluster, not with the number of slapd deployments, and the default
// 128Mi limit OOM-killed the operator at 84 namespaces while it managed one
// two-pod cluster (issue #28).
//
// The rule that fixes it has two halves, and both are load-bearing:
//
//  1. Every informer over a core kind is filtered to managedSelector, and
//     every object the operator CREATES carries managedLabels — so the cache
//     holds what the operator manages and nothing else. The selector is the
//     app.kubernetes.io/name label the slapd pods have carried since Phase 1,
//     chosen so that no existing pod template changes (ADR-013: a label that
//     rolled every fleet on upgrade would be a side-effect roll).
//  2. A Secret the USER supplies — TLS, cnConfigCredentials, bring-your-own
//     credentials, S3 and kubeconfig Secrets, external peer passwords — is
//     never labelled by us and so is invisible to the filtered cache. Every
//     Secret read therefore goes to the API server through secretReader, never
//     through the cache. The Secret informer exists only for the Owns() watch
//     on <cluster>-config-password.
//
// Objects created by an operator older than this rule carry no label. The SSA
// applies land it on Services, StatefulSets and ConfigMaps in the first pass;
// managedGet covers the reads that happen before that apply, and
// reconcileSecret patches it onto the config-password Secret it owns.

// managedLabels is the label set every object the operator creates carries.
// It is the selector's own key and value, nothing else, so that adding it to
// an object changes what the cache holds and nothing a user selects by.
func managedLabels() map[string]string {
	return map[string]string{labelName: appName}
}

// withManagedLabels returns labels extended with managedLabels; a nil input
// is fine.
func withManagedLabels(l map[string]string) map[string]string {
	out := make(map[string]string, len(l)+1)
	maps.Copy(out, l)
	maps.Copy(out, managedLabels())
	return out
}

// managedSelector is the label selector every core-kind informer is filtered
// by. It selects exactly managedLabels.
var managedSelector = labels.SelectorFromSet(managedLabels())

// cachedCoreKinds are the non-CRD kinds some controller Owns() or reads
// through the cached client, i.e. the kinds that get an informer at all.
// ConfigMaps and PersistentVolumeClaims are applied, never read, and have
// none.
func cachedCoreKinds() []client.Object {
	return []client.Object{
		&corev1.Secret{},
		&corev1.Service{},
		&corev1.Pod{},
		&appsv1.StatefulSet{},
		&batchv1.Job{},
	}
}

// CacheOptions is the manager's cache configuration: every core-kind informer
// filtered to the objects the operator manages. CRDs are not filtered — a
// SlapdCluster carries no label of ours and is what the operator exists to
// watch.
func CacheOptions() cache.Options {
	byObject := make(map[client.Object]cache.ByObject, len(cachedCoreKinds()))
	for _, obj := range cachedCoreKinds() {
		byObject[obj] = cache.ByObject{Label: managedSelector}
	}
	return cache.Options{ByObject: byObject}
}

// secretReader is the reader every Secret is read through: the API server
// directly, so that a Secret the user supplied — which carries no label of
// ours and is therefore absent from the filtered cache — is found. nil means
// the client is already uncached (envtest, fake clients) and is used as is.
func secretReader(apiReader client.Reader, c client.Client) client.Reader {
	if apiReader != nil {
		return apiReader
	}
	return c
}

// managedGet reads an object the operator applies — a StatefulSet or a
// Service — from the cache, and on NotFound asks the API server once before
// believing it. The second read exists for one case: an object created by an
// operator that predates managedLabels, which the filtered informer does not
// hold until this pass's apply labels it. Without it, the pre-apply reads
// would take an existing object for a first create — the immutable-field
// adoption would re-assert volumeClaimTemplates against an existing
// StatefulSet (rejected, so the apply that would have labelled it never
// lands), and a <cluster>-write Service frozen on one pod would never be
// widened. In steady state the fallback costs one GET only while an object
// genuinely does not exist yet.
func managedGet(ctx context.Context, c client.Client, apiReader client.Reader, key client.ObjectKey, obj client.Object) error {
	err := c.Get(ctx, key, obj)
	if !apierrors.IsNotFound(err) || apiReader == nil {
		return err
	}
	return apiReader.Get(ctx, key, obj)
}

// secrets is the reader this controller reads Secrets through. See
// secretReader.
func (r *SlapdClusterReconciler) secrets() client.Reader { return secretReader(r.APIReader, r.Client) }

func (r *SlapdDatabaseReconciler) secrets() client.Reader { return secretReader(r.APIReader, r.Client) }

func (r *SlapdSchemaReconciler) secrets() client.Reader { return secretReader(r.APIReader, r.Client) }

func (r *SlapdBackupReconciler) secrets() client.Reader { return secretReader(r.APIReader, r.Client) }

func (r *SlapdScheduledBackupReconciler) secrets() client.Reader {
	return secretReader(r.APIReader, r.Client)
}

// getManaged is managedGet with this controller's clients.
func (r *SlapdClusterReconciler) getManaged(ctx context.Context, key client.ObjectKey, obj client.Object) error {
	return managedGet(ctx, r.Client, r.APIReader, key, obj)
}
