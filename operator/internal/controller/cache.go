/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"maps"

	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// managedLabels is the label set every object the operator creates carries.
// It is the selector's own key and value, nothing else, so that adding it to
// an object changes what the cache holds and nothing a user selects by.
func managedLabels() map[string]string {
	return map[string]string{labelName: appName}
}

// withManagedLabels returns labels extended with managedLabels; a nil input
// is fine.

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

// managedSelector is the label selector every core-kind informer is filtered
// by. It selects exactly managedLabels.
var managedSelector = labels.SelectorFromSet(managedLabels())

// cachedCoreKinds are the non-CRD kinds some controller Owns() or reads
// through the cached client, i.e. the kinds that get an informer at all.
// ConfigMaps and PersistentVolumeClaims are applied, never read, and have
// none.

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
