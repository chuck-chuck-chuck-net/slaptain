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
