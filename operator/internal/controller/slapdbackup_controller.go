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
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
	"github.com/chuck-chuck-chuck-net/slaptain/operator/internal/backup"
)

const backupFieldManager = "slapdbackup-controller"

// SlapdBackupReconciler reconciles a SlapdBackup object. It runs an on-demand
// backup of one SlapdDatabase's data tree to S3 by creating a co-located Job
// (slapcat → gzip → upload). See ADR-014.
type SlapdBackupReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// DefaultImageTag resolves the slapd-init image tag when unpinned, mirroring
	// the SlapdCluster controller.
	DefaultImageTag string
	// OperatorImage is the operator's own image reference (repository:tag), used
	// for the Job's uploader container. Wired from the OPERATOR_IMAGE env.
	OperatorImage string
}

// imageRef builds "repository:tag", falling back to the operator's running tag
// (then "latest") when the tag is unpinned and to the operator-derived default
// repository (defaultRepo) when the repository is unset. Mirrors the SlapdCluster
// controller's resolution.
func (r *SlapdBackupReconciler) imageRef(img ldapv1alpha1.SlapdImageConfig, defaultRepo string) string {
	return resolveImageRef(img, defaultRepo, r.DefaultImageTag)
}

// +kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapdbackups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapdbackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapddatabases;slapdclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete

func (r *SlapdBackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	sb := &ldapv1alpha1.SlapdBackup{}
	if err := r.Get(ctx, req.NamespacedName, sb); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Backups are immutable once finished — nothing more to do.
	if sb.Status.Phase == ldapv1alpha1.BackupPhaseCompleted || sb.Status.Phase == ldapv1alpha1.BackupPhaseFailed {
		return ctrl.Result{}, nil
	}

	// Resolve the database (same namespace) and its cluster.
	sd := &ldapv1alpha1.SlapdDatabase{}
	if err := r.Get(ctx, client.ObjectKey{Name: sb.Spec.DatabaseRef, Namespace: sb.Namespace}, sd); err != nil {
		if apierrors.IsNotFound(err) {
			r.setPending(ctx, sb, "DatabaseNotFound", fmt.Sprintf("SlapdDatabase %q not found", sb.Spec.DatabaseRef))
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	sc := &ldapv1alpha1.SlapdCluster{}
	if err := r.Get(ctx, client.ObjectKey{Name: sd.Spec.ClusterRef, Namespace: sb.Namespace}, sc); err != nil {
		if apierrors.IsNotFound(err) {
			r.setPending(ctx, sb, "ClusterNotFound", fmt.Sprintf("SlapdCluster %q not found", sd.Spec.ClusterRef))
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	if sc.Status.Phase != ldapv1alpha1.PhaseRunning {
		r.setPending(ctx, sb, "ClusterNotReady", fmt.Sprintf("SlapdCluster %q is %s, waiting for Running", sc.Name, sc.Status.Phase))
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if r.OperatorImage == "" {
		// Deploy misconfiguration — won't self-heal, but surface it loudly and
		// keep checking in case the Deployment is fixed.
		r.setPending(ctx, sb, "OperatorImageUnset", "operator image not configured (OPERATOR_IMAGE env); cannot run backup Job")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	objectKey := backupObjectKey(sb, sc, sd)

	// Create the Job if it doesn't exist yet.
	jobName := sb.Name + "-backup"
	job := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: sb.Namespace}, job)
	switch {
	case apierrors.IsNotFound(err):
		job = buildBackupJob(sb, sd, sc, r.imageRef(sc.Spec.Images.Init, defaultDataPlaneRepo(r.OperatorImage, "slapd-init")), r.OperatorImage, objectKey)
		if err := controllerutil.SetControllerReference(sb, job, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, fmt.Errorf("create backup Job: %w", err)
		}
		log.Info("created backup Job", "job", jobName, "key", objectKey)
		now := metav1.Now()
		sb.Status.StartedAt = &now
		r.setRunning(ctx, sb, jobName, objectKey, "BackupStarted", "backup Job created")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	case err != nil:
		return ctrl.Result{}, err
	}

	// Observe the existing Job.
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			now := metav1.Now()
			sb.Status.CompletedAt = &now
			// Record the artifact size (HEAD the object). Best-effort: a HEAD
			// failure must not flip a successful backup to Failed, so we log and
			// leave sizeBytes unset rather than erroring.
			if cfg, err := s3ConfigFromStorage(ctx, r.Client, sb.Namespace, sb.Spec.Storage); err != nil {
				log.Info("backup completed but reading S3 credentials for size failed; sizeBytes unset", "err", err)
			} else if size, err := backup.ObjectSize(ctx, cfg, objectKey); err != nil {
				log.Info("backup completed but object HEAD failed; sizeBytes unset", "err", err)
			} else {
				sb.Status.SizeBytes = size
			}
			r.setTerminal(ctx, sb, ldapv1alpha1.BackupPhaseCompleted, jobName, objectKey, "BackupCompleted", "backup uploaded to S3")
			return ctrl.Result{}, nil
		case batchv1.JobFailed:
			now := metav1.Now()
			sb.Status.CompletedAt = &now
			msg := c.Message
			if msg == "" {
				msg = "backup Job failed"
			}
			r.setTerminal(ctx, sb, ldapv1alpha1.BackupPhaseFailed, jobName, objectKey, "BackupFailed", msg)
			return ctrl.Result{}, nil
		}
	}

	// Still running.
	r.setRunning(ctx, sb, jobName, objectKey, "BackupRunning", "backup Job in progress")
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// backupObjectKey computes a stable S3 object key from the backup's immutable
// creation timestamp (so reconciles never produce a drifting key):
// "<prefix>/<cluster>/<database>/<creationTimestamp>.ldif.gz".
func backupObjectKey(sb *ldapv1alpha1.SlapdBackup, sc *ldapv1alpha1.SlapdCluster, sd *ldapv1alpha1.SlapdDatabase) string {
	ts := sb.CreationTimestamp.UTC().Format("20060102T150405Z")
	key := sc.Name + "/" + sd.Name + "/" + ts + ".ldif.gz"
	if p := strings.Trim(sb.Spec.Storage.Prefix, "/"); p != "" {
		key = p + "/" + key
	}
	return key
}

func (r *SlapdBackupReconciler) setPending(ctx context.Context, sb *ldapv1alpha1.SlapdBackup, reason, msg string) {
	sb.Status.Phase = ldapv1alpha1.BackupPhasePending
	r.patchStatus(ctx, sb, metav1.ConditionFalse, reason, msg)
}

func (r *SlapdBackupReconciler) setRunning(ctx context.Context, sb *ldapv1alpha1.SlapdBackup, jobName, key, reason, msg string) {
	sb.Status.Phase = ldapv1alpha1.BackupPhaseRunning
	sb.Status.JobName = jobName
	sb.Status.Path = key
	r.patchStatus(ctx, sb, metav1.ConditionFalse, reason, msg)
}

func (r *SlapdBackupReconciler) setTerminal(ctx context.Context, sb *ldapv1alpha1.SlapdBackup, phase ldapv1alpha1.SlapdBackupPhase, jobName, key, reason, msg string) {
	sb.Status.Phase = phase
	sb.Status.JobName = jobName
	sb.Status.Path = key
	condStatus := metav1.ConditionFalse
	if phase == ldapv1alpha1.BackupPhaseCompleted {
		condStatus = metav1.ConditionTrue
	}
	r.patchStatus(ctx, sb, condStatus, reason, msg)
}

// patchStatus server-side-applies the backup's status subresource. The full
// sb.Status is sent so SSA preserves fields this manager previously set
// (e.g. StartedAt) across reconciles.
func (r *SlapdBackupReconciler) patchStatus(ctx context.Context, sb *ldapv1alpha1.SlapdBackup, condStatus metav1.ConditionStatus, reason, msg string) {
	sb.Status.ObservedGeneration = sb.Generation
	setCondition(&sb.Status.Conditions, metav1.Condition{
		Type:               "Complete",
		Status:             condStatus,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: sb.Generation,
	})

	statusPatch := &ldapv1alpha1.SlapdBackup{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "ldap.chuck-chuck-chuck.net/v1alpha1",
			Kind:       "SlapdBackup",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      sb.Name,
			Namespace: sb.Namespace,
		},
	}
	statusPatch.Status = sb.Status
	if err := r.Status().Patch(ctx, statusPatch, client.Apply, client.ForceOwnership, client.FieldOwner(backupFieldManager)); err != nil {
		logf.FromContext(ctx).Error(err, "failed to patch SlapdBackup status")
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *SlapdBackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ldapv1alpha1.SlapdBackup{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}
