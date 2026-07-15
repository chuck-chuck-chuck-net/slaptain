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
	"k8s.io/apimachinery/pkg/util/rand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
	"github.com/chuck-chuck-chuck-net/slaptain/operator/internal/backup"
)

// s3ConfigFromStorage reads the credentials Secret referenced by an
// S3StorageSpec and returns a static-credential S3Config for inline
// operator-side S3 operations (preflight validation, retention deletes).
func s3ConfigFromStorage(ctx context.Context, c client.Client, ns string, st ldapv1alpha1.S3StorageSpec) (backup.S3Config, error) {
	secret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Name: st.CredentialsSecretName, Namespace: ns}, secret); err != nil {
		return backup.S3Config{}, fmt.Errorf("read S3 credentials secret %q: %w", st.CredentialsSecretName, err)
	}
	return backup.S3Config{
		Bucket:          st.Bucket,
		Endpoint:        st.Endpoint,
		Region:          st.Region,
		InsecureTLS:     st.InsecureTLS,
		AccessKeyID:     string(secret.Data["access-key-id"]),
		SecretAccessKey: string(secret.Data["secret-access-key"]),
	}, nil
}

// databaseReplPassword reads a database's replication-password from its
// credentials Secret (the source-of-truth the restore must match against the
// backup). Returns "" when the Secret or key is absent.
func (r *SlapdClusterReconciler) databaseReplPassword(ctx context.Context, sd *ldapv1alpha1.SlapdDatabase) (string, error) {
	name := sd.Spec.Credentials.SecretName
	if name == "" {
		name = sd.Name + "-credentials"
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: sd.Namespace}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("read credentials secret %q: %w", name, err)
	}
	return string(secret.Data["replication-password"]), nil
}

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
			// No bootstrapFrom restore pending — check for an imperative
			// SlapdRestore request (in-place rollback) instead.
			return r.maybeStartRequestedRestore(ctx, sc)
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
			Phase:                ldapv1alpha1.RestorePreflight,
			ID:                   rand.String(6),
			OriginalReplicas:     sc.Spec.Replicas,
			OriginalReadReplicas: sc.Spec.ReadReplicas,
			Databases:            names,
			StartedAt:            &now,
			Message:              "validating backup source(s) before scaling down",
		}
		// Preflight runs while the cluster is still serving — phase stays Running.
		sc.Status.Phase = ldapv1alpha1.PhaseRunning
		log.Info("entering restore (preflight)", "databases", names)
		return true, ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// Active restore. Preflight runs with the cluster still up; everything from
	// ScalingDown onward holds the cluster down (RestoreHoldsDown) and reports
	// phase=Restoring.
	rst := sc.Status.Restore
	if rst.Phase == ldapv1alpha1.RestorePreflight {
		sc.Status.Phase = ldapv1alpha1.PhaseRunning
	} else {
		sc.Status.Phase = ldapv1alpha1.PhaseRestoring
	}

	switch rst.Phase {
	case ldapv1alpha1.RestorePreflight:
		return r.reconcileRestorePreflight(ctx, sc, rst)

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
			if err := r.updateRestoreRequest(ctx, sc.Namespace, rst.RequestRef,
				ldapv1alpha1.RestoreRequestFailed, rst.Message); err != nil {
				return true, ctrl.Result{}, err
			}
			return true, ctrl.Result{}, nil
		}
		if !allDone {
			return true, ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		// restoreApplied is the bootstrapFrom one-shot guard; an in-place
		// SlapdRestore must not set it (the DB has no bootstrapFrom, and the
		// restore may be repeated by creating another SlapdRestore).
		if rst.RequestRef == "" {
			if err := r.markRestored(ctx, sc, rst.Databases); err != nil {
				return true, ctrl.Result{}, err
			}
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
		if err := r.updateRestoreRequest(ctx, sc.Namespace, rst.RequestRef,
			ldapv1alpha1.RestoreRequestCompleted, "restore complete; cluster scaled back up"); err != nil {
			return true, ctrl.Result{}, err
		}
		sc.Status.Restore = nil
		return false, ctrl.Result{}, nil

	case ldapv1alpha1.RestoreFailed:
		// Terminal until a human intervenes (inspect/delete the failed Job, fix
		// the source). Cluster stays held at 0 replicas.
		return true, ctrl.Result{}, nil
	}

	return true, ctrl.Result{}, nil
}

// reconcileRestorePreflight runs the destroy-last validation for every database
// in the restore window BEFORE any scale-down: resolve the source, verify the
// replication-password (default-deny) when the DB replicates, and confirm the
// artifact is fetchable + valid. A failure costs neither downtime nor data, so
// it stays in Preflight and retries. On success it advances to ScalingDown.
func (r *SlapdClusterReconciler) reconcileRestorePreflight(ctx context.Context, sc *ldapv1alpha1.SlapdCluster, rst *ldapv1alpha1.SlapdClusterRestoreStatus) (bool, ctrl.Result, error) {
	log := logf.FromContext(ctx)
	for _, name := range rst.Databases {
		sd := &ldapv1alpha1.SlapdDatabase{}
		if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: sc.Namespace}, sd); err != nil {
			return true, ctrl.Result{}, err
		}
		st, key, skipReplCheck, err := r.resolveRestoreSource(ctx, sc, sd)
		if err != nil {
			rst.Message = fmt.Sprintf("preflight: %s: %v", name, err)
			return true, ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		cfg, err := s3ConfigFromStorage(ctx, r.Client, sc.Namespace, st)
		if err != nil {
			rst.Message = fmt.Sprintf("preflight: %s: %v", name, err)
			return true, ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		// Verify the provided replication-password matches the backup only when
		// this DB actually participates in replication — and unless the operator
		// was told to skip the check (foreign/legacy dump, or a deliberate
		// mismatch the user owns; see BootstrapSource /
		// SlapdRestore.spec.skipReplicationPasswordCheck).
		replPW := ""
		switch {
		case skipReplCheck:
			log.Info("restore preflight: skipping replication-password verification per skipReplicationPasswordCheck",
				"database", name)
		case sc.Spec.Replication.Enabled && sd.ReplicationEnabled():
			replPW, err = r.databaseReplPassword(ctx, sd)
			if err != nil {
				rst.Message = fmt.Sprintf("preflight: %s: %v", name, err)
				return true, ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
		}
		if err := backup.Preflight(ctx, cfg, key, sd.Spec.Suffix, replPW); err != nil {
			rst.Message = fmt.Sprintf("preflight failed for %s: %v", name, err)
			log.Info("restore preflight failed; cluster stays up, will retry", "database", name, "err", err)
			return true, ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
	}
	rst.Phase = ldapv1alpha1.RestoreScalingDown
	rst.Message = "preflight passed; scaling down for offline restore"
	log.Info("restore preflight passed; scaling down", "databases", rst.Databases)
	if err := r.updateRestoreRequest(ctx, sc.Namespace, rst.RequestRef,
		ldapv1alpha1.RestoreRequestRestoring, "preflight passed; scaling down for offline restore"); err != nil {
		return true, ctrl.Result{}, err
	}
	return true, ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// maybeStartRequestedRestore looks for a non-terminal SlapdRestore targeting a
// Running database in this cluster and, if found, begins an in-place restore.
// Only one restore runs at a time (this is reached only when sc.Status.Restore
// is nil), so concurrent SlapdRestores serialise: extra requests stay Pending
// until the current one clears and a later reconcile picks the next-oldest.
// Returns handled=true when a restore was started.
func (r *SlapdClusterReconciler) maybeStartRequestedRestore(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) (bool, ctrl.Result, error) {
	log := logf.FromContext(ctx)
	sr, sd, err := r.pendingRestoreRequest(ctx, sc)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	if sr == nil {
		return false, ctrl.Result{}, nil
	}
	if r.OperatorImage == "" {
		log.Info("SlapdRestore pending but OPERATOR_IMAGE unset; staying Running", "request", sr.Name)
		_ = r.setRestoreRequestPhase(ctx, sr, ldapv1alpha1.RestoreRequestPending, "waiting: operator image unset")
		return false, ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	now := metav1.Now()
	sc.Status.Restore = &ldapv1alpha1.SlapdClusterRestoreStatus{
		Phase:                ldapv1alpha1.RestorePreflight,
		ID:                   rand.String(6),
		OriginalReplicas:     sc.Spec.Replicas,
		OriginalReadReplicas: sc.Spec.ReadReplicas,
		Databases:            []string{sd.Name},
		RequestRef:           sr.Name,
		StartedAt:            &now,
		Message:              fmt.Sprintf("validating backup source for SlapdRestore %q before scaling down", sr.Name),
	}
	// Preflight runs while the cluster is still serving — phase stays Running.
	sc.Status.Phase = ldapv1alpha1.PhaseRunning
	_ = r.setRestoreRequestPhase(ctx, sr, ldapv1alpha1.RestoreRequestPreflight, "validating backup source")
	log.Info("entering in-place restore (preflight)", "database", sd.Name, "request", sr.Name)
	return true, ctrl.Result{RequeueAfter: 2 * time.Second}, nil
}

// pendingRestoreRequest returns the oldest non-terminal SlapdRestore whose target
// database belongs to this cluster and is Running, plus that database. Returns
// (nil, nil, nil) when there is none. SlapdRestores whose database is missing,
// not yet Running, or in another cluster are skipped — a later reconcile (driven
// by the SlapdRestore / SlapdDatabase watches) re-evaluates them.
func (r *SlapdClusterReconciler) pendingRestoreRequest(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) (*ldapv1alpha1.SlapdRestore, *ldapv1alpha1.SlapdDatabase, error) {
	var list ldapv1alpha1.SlapdRestoreList
	if err := r.List(ctx, &list, client.InNamespace(sc.Namespace)); err != nil {
		return nil, nil, fmt.Errorf("list SlapdRestores: %w", err)
	}
	var chosen *ldapv1alpha1.SlapdRestore
	var chosenDB *ldapv1alpha1.SlapdDatabase
	for i := range list.Items {
		sr := &list.Items[i]
		if sr.IsTerminal() {
			continue
		}
		sd := &ldapv1alpha1.SlapdDatabase{}
		if err := r.Get(ctx, client.ObjectKey{Name: sr.Spec.DatabaseRef, Namespace: sc.Namespace}, sd); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, nil, err
		}
		if sd.Spec.ClusterRef != sc.Name || sd.Status.Phase != ldapv1alpha1.DatabasePhaseRunning {
			continue
		}
		if chosen == nil || sr.CreationTimestamp.Before(&chosen.CreationTimestamp) {
			chosen = sr
			chosenDB = sd
		}
	}
	return chosen, chosenDB, nil
}

// updateRestoreRequest fetches the named SlapdRestore (if any) and advances its
// status phase. A no-op when name is empty (a bootstrapFrom-driven restore) or
// the SlapdRestore was deleted mid-restore (the machine continues regardless).
func (r *SlapdClusterReconciler) updateRestoreRequest(ctx context.Context, ns, name string, phase ldapv1alpha1.SlapdRestorePhase, msg string) error {
	if name == "" {
		return nil
	}
	sr := &ldapv1alpha1.SlapdRestore{}
	if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, sr); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get SlapdRestore %q: %w", name, err)
	}
	return r.setRestoreRequestPhase(ctx, sr, phase, msg)
}

// setRestoreRequestPhase writes a SlapdRestore's status phase/message, stamping
// startedAt/completedAt on the relevant transitions. The SlapdCluster controller
// is the sole writer of SlapdRestore status (ADR-014 Architecture A).
func (r *SlapdClusterReconciler) setRestoreRequestPhase(ctx context.Context, sr *ldapv1alpha1.SlapdRestore, phase ldapv1alpha1.SlapdRestorePhase, msg string) error {
	if sr.Status.Phase == phase && sr.Status.Message == msg {
		return nil
	}
	now := metav1.Now()
	sr.Status.Phase = phase
	sr.Status.Message = msg
	sr.Status.ObservedGeneration = sr.Generation
	if phase == ldapv1alpha1.RestoreRequestPreflight && sr.Status.StartedAt == nil {
		sr.Status.StartedAt = &now
	}
	if (phase == ldapv1alpha1.RestoreRequestCompleted || phase == ldapv1alpha1.RestoreRequestFailed) && sr.Status.CompletedAt == nil {
		sr.Status.CompletedAt = &now
	}
	if err := r.Status().Update(ctx, sr); err != nil {
		return fmt.Errorf("update SlapdRestore %q status: %w", sr.Name, err)
	}
	return nil
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

// runRestoreJobs fans a restore Job out across every pod of every restoring
// database — RW pods load the artifact directly (no syncrepl refresh), RO pods
// are wiped and re-refresh on scale-up (ADR-014 amendment). Reports whether all
// Jobs have completed and whether any failed.
func (r *SlapdClusterReconciler) runRestoreJobs(ctx context.Context, sc *ldapv1alpha1.SlapdCluster, dbNames []string) (allDone bool, anyFailed bool, err error) {
	rwN := sc.Status.Restore.OriginalReplicas
	if rwN == 0 {
		rwN = 1
	}
	roN := sc.Status.Restore.OriginalReadReplicas

	allDone = true
	for _, name := range dbNames {
		sd := &ldapv1alpha1.SlapdDatabase{}
		if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: sc.Namespace}, sd); err != nil {
			return false, false, fmt.Errorf("get SlapdDatabase %q: %w", name, err)
		}
		st, key, _, err := r.resolveRestoreSource(ctx, sc, sd)
		if err != nil {
			return false, false, fmt.Errorf("resolve restore source for %q: %w", name, err)
		}

		for _, t := range restorePodTargets(sc, sd, rwN, roN) {
			job := &batchv1.Job{}
			getErr := r.Get(ctx, client.ObjectKey{Name: t.jobName, Namespace: sc.Namespace}, job)
			switch {
			case apierrors.IsNotFound(getErr):
				job = buildRestoreJob(sc, sd, st, key, r.imageRef(sc.Spec.Images.Init, defaultDataPlaneRepo(r.OperatorImage, "slapd-init")), r.OperatorImage, t)
				if err := controllerutil.SetControllerReference(sc, job, r.Scheme); err != nil {
					return false, false, err
				}
				if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
					return false, false, fmt.Errorf("create restore Job %q: %w", t.jobName, err)
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
	}
	return allDone, false, nil
}

// restorePodTargets enumerates the per-pod restore Jobs for one database: every
// RW pod (load the artifact directly; mount the accesslog PVC when present) and
// every RO pod (wipe-only).
func restorePodTargets(sc *ldapv1alpha1.SlapdCluster, sd *ldapv1alpha1.SlapdDatabase, rwN, roN int32) []restorePodTarget {
	// Job names embed the per-restore id so a new restore never collides with a
	// prior restore's Jobs: <db>-restore-<id>-rw-<i>. Cap the db base to keep the
	// name within the 63-char limit.
	base := sd.Name
	if len(base) > 24 {
		base = base[:24]
	}
	id := sc.Status.Restore.ID
	mountAccesslog := sc.NeedsAccesslogVolume()

	targets := make([]restorePodTarget, 0, int(rwN+roN))
	for i := range rwN {
		t := restorePodTarget{
			jobName:   fmt.Sprintf("%s-restore-%s-rw-%d", base, id, i),
			dataPVC:   fmt.Sprintf("data-%s-%d", sc.Name, i),
			configPVC: fmt.Sprintf("config-%s-%d", sc.Name, i),
			loadData:  true,
		}
		if mountAccesslog {
			t.accesslogPVC = fmt.Sprintf("accesslog-%s-%d", sc.Name, i)
		}
		targets = append(targets, t)
	}
	for j := range roN {
		targets = append(targets, restorePodTarget{
			jobName:  fmt.Sprintf("%s-restore-%s-ro-%d", base, id, j),
			dataPVC:  fmt.Sprintf("data-%s-readonly-%d", sc.Name, j),
			loadData: false,
		})
	}
	return targets
}

// resolveRestoreSource derives the S3 storage, object key, and
// skip-replication-password-check flag for a database in the current restore
// window — from the triggering SlapdRestore (in-place) when RequestRef is set,
// otherwise from the database's bootstrapFrom.
func (r *SlapdClusterReconciler) resolveRestoreSource(ctx context.Context, sc *ldapv1alpha1.SlapdCluster, sd *ldapv1alpha1.SlapdDatabase) (ldapv1alpha1.S3StorageSpec, string, bool, error) {
	if req := sc.Status.Restore.RequestRef; req != "" {
		sr := &ldapv1alpha1.SlapdRestore{}
		if err := r.Get(ctx, client.ObjectKey{Name: req, Namespace: sc.Namespace}, sr); err != nil {
			return ldapv1alpha1.S3StorageSpec{}, "", false, fmt.Errorf("get SlapdRestore %q: %w", req, err)
		}
		st, key, err := r.resolveSourceObject(ctx, sc.Namespace, sr.Spec.Source.BackupRef, sr.Spec.Source.S3)
		return st, key, sr.Spec.SkipReplicationPasswordCheck, err
	}
	st, key, err := r.resolveBootstrapSource(ctx, sc, sd)
	skip := sd.Spec.BootstrapFrom != nil && sd.Spec.BootstrapFrom.SkipReplicationPasswordCheck
	return st, key, skip, err
}

// resolveBootstrapSource derives the S3 storage config and object key for a
// database's bootstrapFrom source (a SlapdBackup reference or a direct S3 path).
func (r *SlapdClusterReconciler) resolveBootstrapSource(ctx context.Context, sc *ldapv1alpha1.SlapdCluster, sd *ldapv1alpha1.SlapdDatabase) (ldapv1alpha1.S3StorageSpec, string, error) {
	if sd.Spec.BootstrapFrom == nil {
		return ldapv1alpha1.S3StorageSpec{}, "", fmt.Errorf("bootstrapFrom is nil")
	}
	return r.resolveSourceObject(ctx, sc.Namespace, sd.Spec.BootstrapFrom.BackupRef, sd.Spec.BootstrapFrom.S3)
}

// resolveSourceObject derives the S3 storage config and object key from a backup
// reference (a SlapdBackup name) or a direct S3 source. Shared by bootstrapFrom
// and SlapdRestore (they carry the same backupRef/s3 shape).
func (r *SlapdClusterReconciler) resolveSourceObject(ctx context.Context, ns, backupRef string, s3src *ldapv1alpha1.BootstrapS3Source) (ldapv1alpha1.S3StorageSpec, string, error) {
	switch {
	case s3src != nil:
		return s3src.Storage, s3src.Key, nil
	case backupRef != "":
		sb := &ldapv1alpha1.SlapdBackup{}
		if err := r.Get(ctx, client.ObjectKey{Name: backupRef, Namespace: ns}, sb); err != nil {
			return ldapv1alpha1.S3StorageSpec{}, "", fmt.Errorf("get SlapdBackup %q: %w", backupRef, err)
		}
		if sb.Status.Phase != ldapv1alpha1.BackupPhaseCompleted || sb.Status.Path == "" {
			return ldapv1alpha1.S3StorageSpec{}, "", fmt.Errorf("SlapdBackup %q is not Completed", backupRef)
		}
		return sb.Spec.Storage, sb.Status.Path, nil
	default:
		return ldapv1alpha1.S3StorageSpec{}, "", fmt.Errorf("restore source has neither backupRef nor s3")
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

// applyStatus server-side-applies the SlapdCluster status subresource. A
// NotFound is ignored: the cluster can be deleted while a reconcile is in
// flight (e.g. teardown), and patching the status of a gone object is a no-op,
// not an error worth logging with a stack trace.
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
	if err := r.Status().Patch(ctx, statusPatch, client.Apply, client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	return nil
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
