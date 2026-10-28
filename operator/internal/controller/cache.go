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
	"reflect"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// labelFilteredKinds are the kinds the cache holds only when labelled
// LabelCluster (ADR-044). Unfiltered, they are every object of that kind in the
// cluster, Helm release Secrets included. The slaptain CR kinds stay unfiltered:
// a user's CR carries no label and must still reconcile.
var labelFilteredKinds = []client.Object{
	&corev1.Secret{},
	&corev1.ConfigMap{},
	&corev1.Service{},
	&appsv1.StatefulSet{},
	&batchv1.Job{},
	&corev1.Pod{},
}

// CacheOptions restricts the manager's cache to labelled objects of the
// filtered kinds (ADR-044).
func CacheOptions() cache.Options {
	hasCluster, err := labels.NewRequirement(LabelCluster, selection.Exists, nil)
	if err != nil {
		panic(err) // a constant key; only a typo in LabelCluster gets here
	}
	labelled := labels.NewSelector().Add(*hasCluster)
	byObject := make(map[client.Object]cache.ByObject, len(labelFilteredKinds))
	for _, obj := range labelFilteredKinds {
		byObject[obj] = cache.ByObject{Label: labelled}
	}
	return cache.Options{ByObject: byObject}
}

// NewCacheFallbackClient reads through the cache and, for a filtered kind the
// cache does not hold, once more through apiReader (ADR-044). That is how the
// operator reads a Secret the user brought, which it must never label, and an
// object an older operator created before the label existed.
func NewCacheFallbackClient(cached client.Client, apiReader client.Reader) client.Client {
	filtered := make(map[reflect.Type]bool, len(labelFilteredKinds))
	for _, obj := range labelFilteredKinds {
		filtered[reflect.TypeOf(obj)] = true
	}
	return &cacheFallbackClient{Client: cached, apiReader: apiReader, filtered: filtered}
}

type cacheFallbackClient struct {
	client.Client
	apiReader client.Reader
	filtered  map[reflect.Type]bool
}

func (c *cacheFallbackClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := c.Client.Get(ctx, key, obj, opts...)
	if apierrors.IsNotFound(err) && c.filtered[reflect.TypeOf(obj)] {
		return c.apiReader.Get(ctx, key, obj, opts...)
	}
	return err
}

// labelOwnedObject adds the operator labels to obj, which an older operator
// created without them, when owner controls it. Without a controller reference
// the object may be the user's, and the operator does not write to it.
func labelOwnedObject(ctx context.Context, c client.Client, obj client.Object, owner metav1.Object) error {
	if !metav1.IsControlledBy(obj, owner) || obj.GetLabels()[LabelCluster] == clusterLabelValue(owner.GetName()) {
		return nil
	}
	patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
	obj.SetLabels(withOperatorLabels(owner.GetName(), obj.GetLabels()))
	return c.Patch(ctx, obj, patch)
}
