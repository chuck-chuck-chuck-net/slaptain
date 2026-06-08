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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// reconcileRestore drives the bootstrapFrom restore state machine (ADR-014). It
// returns handled=true when a restore is in progress and the caller should
// persist status and requeue with the returned Result, skipping the normal
// ready-based phase computation. handled=false means no restore is active (or
// one is merely pending the empty DBs being defined — in which case the cluster
// stays Running so the SlapdDatabase controller can define them).
//
// The StatefulSet replica count is forced to 0 by buildStatefulSetSpec whenever
// sc.RestoreHoldsDown() is true, so this machine only sets status and observes;
// the actual scaling happens via the normal StatefulSet reconcile one pass later.
func (r *SlapdClusterReconciler) reconcileRestore(ctx context.Context, sc *ldapv1alpha1.SlapdCluster, sts *appsv1.StatefulSet) (bool, ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Not yet started: decide whether to begin.
	if sc.Status.Restore == nil {
		dbs, allDefined, err := r.databasesNeedingRestore(ctx, sc)
		if err != nil {
			return false, ctrl.Result{}, err
		}
		if len(dbs) == 0 {
			return false, ctrl.Result{}, nil
		}
		if r.OperatorImage == "" {
			// Can't run the restore Job's download container without our image.
			log.Info("restore needed but OPERATOR_IMAGE unset; staying Running")
			return false, ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		if !allDefined {
			// Wait — keep the cluster Running so the SlapdDatabase controller can
			// create the empty databases. The SlapdDatabase watch re-triggers us.
			return false, ctrl.Result{}, nil
		}

		names := make([]string, len(dbs))
		for i := range dbs {
			names[i] = dbs[i].Name
		}
		now := metav1.Now()
		sc.Status.Restore = &ldapv1alpha1.SlapdClusterRestoreStatus{
			Phase:                ldapv1alpha1.RestoreScalingDown,
			OriginalReplicas:     sc.Spec.Replicas,
			OriginalReadReplicas: sc.Spec.ReadReplicas,
			Databases:            names,
			StartedAt:            &now,
			Message:              "scaling down for offline restore",
		}
		sc.Status.Phase = ldapv1alpha1.PhaseRestoring
		log.Info("entering restore", "databases", names)
		return true, ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Active restore: drive the machine. Phase stays Restoring throughout.
	rst := sc.Status.Restore
	sc.Status.Phase = ldapv1alpha1.PhaseRestoring

	switch rst.Phase {
	case ldapv1alpha1.RestoreScalingDown:
		// buildStatefulSetSpec has forced replicas to 0; wait for pod-0 (and all
		// RW pods) to terminate and release the data PVC.
		if sts.Status.Replicas > 0 {
			rst.Message = fmt.Sprintf("waiting for %d pod(s) to terminate", sts.Status.Replicas)
			return true, ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		rst.Phase = ldapv1alpha1.RestoreInProgress
		rst.Message = "running restore jobs"
		return true, ctrl.Result{RequeueAfter: 2 * time.Second}, nil

	case ldapv1alpha1.RestoreInProgress:
		allDone, anyFailed, err := r.runRestoreJobs(ctx, sc, rst.Databases)
		if err != nil {
			return true, ctrl.Result{}, err
		}
		if anyFailed {
			rst.Phase = ldapv1alpha1.RestoreFailed
			rst.Message = "a restore Job failed; cluster held at 0 replicas for inspection"
			log.Info("restore failed; holding cluster down")
			return true, ctrl.Result{}, nil
		}
		if !allDone {
			return true, ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		if err := r.markRestored(ctx, sc, rst.Databases); err != nil {
			return true, ctrl.Result{}, err
		}
		rst.Phase = ldapv1alpha1.RestoreScalingUp
		rst.Message = "scaling back up"
		return true, ctrl.Result{RequeueAfter: 5 * time.Second}, nil

	case ldapv1alpha1.RestoreScalingUp:
		// RestoreHoldsDown() is now false, so the StatefulSet scales back to
		// spec.replicas. Wait for readiness, then clear the restore status and
		// hand back to the normal flow.
		desired := rst.OriginalReplicas
		if desired == 0 {
			desired = 1
		}
		if sts.Status.ReadyReplicas < desired {
			rst.Message = fmt.Sprintf("scaling up: %d/%d ready", sts.Status.ReadyReplicas, desired)
			return true, ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		log.Info("restore complete", "databases", rst.Databases)
		sc.Status.Restore = nil
		return false, ctrl.Result{}, nil

	case ldapv1alpha1.RestoreFailed:
		// Terminal until a human intervenes (inspect/delete the failed Job, fix
		// the source). Cluster stays held at 0 replicas.
		return true, ctrl.Result{}, nil
	}

	return true, ctrl.Result{}, nil
}

// databasesNeedingRestore returns the SlapdDatabases in this cluster that have
// spec.bootstrapFrom set and have not yet been restored, plus whether every one
// of them has its empty database defined (status.phase == Running) — the
// precondition for scaling down and running slapadd.
func (r *SlapdClusterReconciler) databasesNeedingRestore(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) ([]ldapv1alpha1.SlapdDatabase, bool, error) {
	var list ldapv1alpha1.SlapdDatabaseList
	if err := r.List(ctx, &list, client.InNamespace(sc.Namespace)); err != nil {
		return nil, false, fmt.Errorf("list SlapdDatabases: %w", err)
	}
	needing := make([]ldapv1alpha1.SlapdDatabase, 0, len(list.Items))
	allDefined := true
	for i := range list.Items {
		sd := list.Items[i]
		if sd.Spec.ClusterRef != sc.Name || sd.Spec.BootstrapFrom == nil || sd.Status.RestoreApplied {
			continue
		}
		needing = append(needing, sd)
		if sd.Status.Phase != ldapv1alpha1.DatabasePhaseRunning {
			allDefined = false
		}
	}
	return needing, allDefined, nil
}

// runRestoreJobs ensures a restore Job exists for each database and reports
// whether all have completed and whether any failed.
func (r *SlapdClusterReconciler) runRestoreJobs(ctx context.Context, sc *ldapv1alpha1.SlapdCluster, dbNames []string) (allDone bool, anyFailed bool, err error) {
	allDone = true
	for _, name := range dbNames {
		sd := &ldapv1alpha1.SlapdDatabase{}
		if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: sc.Namespace}, sd); err != nil {
			return false, false, fmt.Errorf("get SlapdDatabase %q: %w", name, err)
		}
		st, key, err := r.resolveBootstrapSource(ctx, sc, sd)
		if err != nil {
			return false, false, fmt.Errorf("resolve bootstrapFrom for %q: %w", name, err)
		}

		jobName := name + "-restore"
		job := &batchv1.Job{}
		getErr := r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: sc.Namespace}, job)
		switch {
		case apierrors.IsNotFound(getErr):
			job = buildRestoreJob(sc, sd, st, key, r.imageRef(sc.Spec.Images.Init), r.OperatorImage)
			if err := controllerutil.SetControllerReference(sc, job, r.Scheme); err != nil {
				return false, false, err
			}
			if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
				return false, false, fmt.Errorf("create restore Job for %q: %w", name, err)
			}
			allDone = false
			continue
		case getErr != nil:
			return false, false, getErr
		}

		complete, failed := jobTerminalState(job)
		if failed {
			return false, true, nil
		}
		if !complete {
			allDone = false
		}
	}
	return allDone, false, nil
}

// resolveBootstrapSource derives the S3 storage config and object key for a
// database's bootstrapFrom source (a SlapdBackup reference or a direct S3 path).
func (r *SlapdClusterReconciler) resolveBootstrapSource(ctx context.Context, sc *ldapv1alpha1.SlapdCluster, sd *ldapv1alpha1.SlapdDatabase) (ldapv1alpha1.S3StorageSpec, string, error) {
	bf := sd.Spec.BootstrapFrom
	switch {
	case bf == nil:
		return ldapv1alpha1.S3StorageSpec{}, "", fmt.Errorf("bootstrapFrom is nil")
	case bf.S3 != nil:
		return bf.S3.Storage, bf.S3.Key, nil
	case bf.BackupRef != "":
		sb := &ldapv1alpha1.SlapdBackup{}
		if err := r.Get(ctx, client.ObjectKey{Name: bf.BackupRef, Namespace: sc.Namespace}, sb); err != nil {
			return ldapv1alpha1.S3StorageSpec{}, "", fmt.Errorf("get SlapdBackup %q: %w", bf.BackupRef, err)
		}
		if sb.Status.Phase != ldapv1alpha1.BackupPhaseCompleted || sb.Status.Path == "" {
			return ldapv1alpha1.S3StorageSpec{}, "", fmt.Errorf("SlapdBackup %q is not Completed", bf.BackupRef)
		}
		return sb.Spec.Storage, sb.Status.Path, nil
	default:
		return ldapv1alpha1.S3StorageSpec{}, "", fmt.Errorf("bootstrapFrom has neither backupRef nor s3")
	}
}

// markRestored sets status.restoreApplied=true on each database via a
// server-side apply owning only that field (the SlapdDatabase controller owns
// the rest of the status). One-shot: the trigger condition is
// bootstrapFrom && !restoreApplied, so this prevents re-restoring.
func (r *SlapdClusterReconciler) markRestored(ctx context.Context, sc *ldapv1alpha1.SlapdCluster, dbNames []string) error {
	for _, name := range dbNames {
		patch := &ldapv1alpha1.SlapdDatabase{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "ldap.chuck-chuck-chuck.net/v1alpha1",
				Kind:       "SlapdDatabase",
			},
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: sc.Namespace},
		}
		patch.Status.RestoreApplied = true
		if err := r.Status().Patch(ctx, patch, client.Apply, client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
			return fmt.Errorf("mark %q restoreApplied: %w", name, err)
		}
	}
	return nil
}

// applyStatus server-side-applies the SlapdCluster status subresource.
func (r *SlapdClusterReconciler) applyStatus(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) error {
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
	return r.Status().Patch(ctx, statusPatch, client.Apply, client.ForceOwnership, client.FieldOwner(fieldManager))
}

// jobTerminalState reports a Job's completion/failure from its conditions.
func jobTerminalState(job *batchv1.Job) (complete bool, failed bool) {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			complete = true
		case batchv1.JobFailed:
			failed = true
		}
	}
	return complete, failed
}
