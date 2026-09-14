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
	"sort"
	"time"

	"github.com/robfig/cron/v3"
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

const (
	scheduledBackupFieldManager = "slapdscheduledbackup-controller"
	// scheduledBackupLabel links emitted SlapdBackups back to their schedule.
	scheduledBackupLabel = "ldap.chuck-chuck-chuck.net/scheduled-backup"
	// maxMissedSchedules caps backfill scanning so a long-paused schedule doesn't
	// spin. We only ever create the single most-recent due backup, never backfill.
	maxMissedSchedules = 100
)

// SlapdScheduledBackupReconciler reconciles a SlapdScheduledBackup object: it
// emits owned SlapdBackup objects on a cron schedule and prunes old ones (and
// their S3 objects) per the retention policy. See ADR-014.
type SlapdScheduledBackupReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapdscheduledbackups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapdscheduledbackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapdbackups,verbs=get;list;watch;create;delete

func (r *SlapdScheduledBackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	ssb := &ldapv1alpha1.SlapdScheduledBackup{}
	if err := r.Get(ctx, req.NamespacedName, ssb); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if ssb.Spec.Suspend {
		ssb.Status.NextScheduleTime = nil
		r.patchStatus(ctx, ssb, metav1.ConditionFalse, "Suspended", "scheduling is suspended via spec.suspend")
		return ctrl.Result{}, nil
	}

	sched, err := cron.ParseStandard(ssb.Spec.Schedule)
	if err != nil {
		// Invalid cron — surface and wait for a spec fix (no tight requeue).
		r.patchStatus(ctx, ssb, metav1.ConditionFalse, "InvalidSchedule", fmt.Sprintf("invalid schedule %q: %v", ssb.Spec.Schedule, err))
		return ctrl.Result{}, nil
	}

	now := time.Now()

	// Create a backup if one is due (only the most recent due time; no backfill).
	if scheduledTime := r.dueTime(ssb, sched, now); !scheduledTime.IsZero() {
		name := scheduledBackupName(ssb, scheduledTime)
		sb := &ldapv1alpha1.SlapdBackup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ssb.Namespace,
				Labels:    map[string]string{scheduledBackupLabel: ssb.Name},
			},
			Spec: ldapv1alpha1.SlapdBackupSpec{
				DatabaseRef: ssb.Spec.DatabaseRef,
				Storage:     ssb.Spec.Storage,
				Compression: ssb.Spec.Compression,
				PodAffinity: ssb.Spec.PodAffinity,
			},
		}
		if err := controllerutil.SetControllerReference(ssb, sb, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, sb); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, fmt.Errorf("create scheduled SlapdBackup: %w", err)
		}
		st := metav1.NewTime(scheduledTime)
		ssb.Status.LastScheduleTime = &st
		ssb.Status.LastBackupRef = name
		log.Info("created scheduled backup", "backup", name, "scheduledTime", scheduledTime)
	}

	// Prune old backups (best-effort; a failure here must not block scheduling).
	if err := r.enforceRetention(ctx, ssb, now); err != nil {
		log.Error(err, "retention pruning failed")
	}

	next := sched.Next(now)
	nt := metav1.NewTime(next)
	ssb.Status.NextScheduleTime = &nt
	r.patchStatus(ctx, ssb, metav1.ConditionTrue, "Scheduled", fmt.Sprintf("next backup at %s", next.Format(time.RFC3339)))

	return ctrl.Result{RequeueAfter: time.Until(next)}, nil
}

// dueTime returns the most recent scheduled time in (lastRun, now], or the zero
// time if no backup is due. Immediate fires once on first reconcile.
func (r *SlapdScheduledBackupReconciler) dueTime(ssb *ldapv1alpha1.SlapdScheduledBackup, sched cron.Schedule, now time.Time) time.Time {
	if ssb.Spec.Immediate && ssb.Status.LastScheduleTime == nil {
		return now
	}

	earliest := ssb.CreationTimestamp.Time
	if ssb.Status.LastScheduleTime != nil {
		earliest = ssb.Status.LastScheduleTime.Time
	}

	var last time.Time
	count := 0
	for t := sched.Next(earliest); !t.After(now); t = sched.Next(t) {
		last = t
		if count++; count > maxMissedSchedules {
			break
		}
	}
	return last
}

// enforceRetention deletes Completed backups (and their S3 objects) beyond
// maxCount or older than maxAge. Running/Pending/Failed backups are left alone:
// in-progress ones must not be touched, and failed ones are kept for inspection.
func (r *SlapdScheduledBackupReconciler) enforceRetention(ctx context.Context, ssb *ldapv1alpha1.SlapdScheduledBackup, now time.Time) error {
	ret := ssb.Spec.Retention
	if ret.MaxCount == 0 && ret.MaxAge == "" {
		return nil
	}

	var list ldapv1alpha1.SlapdBackupList
	if err := r.List(ctx, &list,
		client.InNamespace(ssb.Namespace),
		client.MatchingLabels{scheduledBackupLabel: ssb.Name},
	); err != nil {
		return fmt.Errorf("list scheduled backups: %w", err)
	}

	completed := make([]ldapv1alpha1.SlapdBackup, 0, len(list.Items))
	for _, sb := range list.Items {
		if sb.Status.Phase == ldapv1alpha1.BackupPhaseCompleted {
			completed = append(completed, sb)
		}
	}
	// Newest first.
	sort.Slice(completed, func(i, j int) bool {
		return backupTime(&completed[i]).After(backupTime(&completed[j]))
	})

	prune := map[string]*ldapv1alpha1.SlapdBackup{}
	if ret.MaxCount > 0 && len(completed) > int(ret.MaxCount) {
		for i := int(ret.MaxCount); i < len(completed); i++ {
			prune[completed[i].Name] = &completed[i]
		}
	}
	if ret.MaxAge != "" {
		if d, err := time.ParseDuration(ret.MaxAge); err == nil {
			cutoff := now.Add(-d)
			for i := range completed {
				if backupTime(&completed[i]).Before(cutoff) {
					prune[completed[i].Name] = &completed[i]
				}
			}
		} else {
			return fmt.Errorf("invalid retention.maxAge %q: %w", ret.MaxAge, err)
		}
	}

	for _, sb := range prune {
		// Delete the S3 object first — it isn't garbage-collected with the CR.
		if sb.Status.Path != "" {
			cfg, err := r.s3Config(ctx, sb)
			if err != nil {
				logf.FromContext(ctx).Error(err, "skipping prune: cannot build S3 config", "backup", sb.Name)
				continue
			}
			if err := backup.Delete(ctx, cfg, sb.Status.Path); err != nil {
				logf.FromContext(ctx).Error(err, "skipping prune: S3 delete failed", "backup", sb.Name, "key", sb.Status.Path)
				continue
			}
		}
		if err := r.Delete(ctx, sb); err != nil && !apierrors.IsNotFound(err) {
			logf.FromContext(ctx).Error(err, "failed to delete pruned backup", "backup", sb.Name)
		}
	}
	return nil
}

// s3Config builds an S3 config for a backup, reading static credentials from its
// referenced Secret (keys access-key-id / secret-access-key).
func (r *SlapdScheduledBackupReconciler) s3Config(ctx context.Context, sb *ldapv1alpha1.SlapdBackup) (backup.S3Config, error) {
	return s3ConfigFromStorage(ctx, r.Client, sb.Namespace, sb.Spec.Storage)
}

// backupTime is the timestamp used to order/age a backup: its completion time
// when known, otherwise its creation time.
func backupTime(sb *ldapv1alpha1.SlapdBackup) time.Time {
	if sb.Status.CompletedAt != nil {
		return sb.Status.CompletedAt.Time
	}
	return sb.CreationTimestamp.Time
}

// scheduledBackupName is deterministic per scheduled time so a re-reconcile of
// the same tick does not create a duplicate. The schedule name is capped so the
// result stays within the 63-char object-name limit.
func scheduledBackupName(ssb *ldapv1alpha1.SlapdScheduledBackup, t time.Time) string {
	prefix := ssb.Name
	if len(prefix) > 40 {
		prefix = prefix[:40]
	}
	return fmt.Sprintf("%s-%d", prefix, t.Unix())
}

func (r *SlapdScheduledBackupReconciler) patchStatus(ctx context.Context, ssb *ldapv1alpha1.SlapdScheduledBackup, condStatus metav1.ConditionStatus, reason, msg string) {
	ssb.Status.ObservedGeneration = ssb.Generation
	setCondition(&ssb.Status.Conditions, metav1.Condition{
		Type:               "Active",
		Status:             condStatus,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: ssb.Generation,
	})

	statusPatch := &ldapv1alpha1.SlapdScheduledBackup{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "ldap.chuck-chuck-chuck.net/v1alpha1",
			Kind:       "SlapdScheduledBackup",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ssb.Name,
			Namespace: ssb.Namespace,
		},
	}
	statusPatch.Status = ssb.Status
	ac, err := applyConfiguration(statusPatch)
	if err != nil {
		logf.FromContext(ctx).Error(err, "failed to build SlapdScheduledBackup status apply configuration")
		return
	}
	if err := r.Status().Apply(ctx, ac, client.ForceOwnership, client.FieldOwner(scheduledBackupFieldManager)); err != nil {
		logf.FromContext(ctx).Error(err, "failed to patch SlapdScheduledBackup status")
	}
}

// SetupWithManager sets up the controller with the Manager. Owns the SlapdBackup
// objects it emits so completions re-trigger retention pruning.
func (r *SlapdScheduledBackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ldapv1alpha1.SlapdScheduledBackup{}).
		Owns(&ldapv1alpha1.SlapdBackup{}).
		Complete(r)
}
