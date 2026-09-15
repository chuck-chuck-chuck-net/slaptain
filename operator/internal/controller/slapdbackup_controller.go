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
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"
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
	"github.com/chuck-chuck-chuck-net/slaptain/operator/internal/suffixprobe"
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
	// ClusterDomain is the cluster's DNS domain, used to build the per-pod FQDN
	// for the source contextCSN read (ADR-015). Optional: when unset it is
	// resolved on demand, so an unwired manager still records honest sources.
	ClusterDomain string
}

// clusterDomain returns the configured DNS domain, resolving it on demand when
// the manager did not wire one (ADR-015 precedence: CLUSTER_DOMAIN env →
// resolv.conf → cluster.local). Called only when a backup Job is created, so
// the lazy resolution costs nothing on the hot path.
func (r *SlapdBackupReconciler) clusterDomain() string {
	if r.ClusterDomain != "" {
		return r.ClusterDomain
	}
	return ResolveClusterDomain()
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

	// Backups are immutable once finished — nothing more to do except release the
	// Job's hold on the slapd PVCs.
	//
	// ADR-018: the finished Job's pod keeps `kubernetes.io/pvc-protection` on
	// pod-0's config/data/accesslog PVCs for as long as the pod object exists, so
	// a retained backup record would block ADR-012 case-2 recovery on pod-0
	// indefinitely. Reaping from this early return is deliberate (R3): the
	// terminal phase is already persisted and this branch precedes Job creation,
	// so the reap can never re-trigger a backup. It is idempotent, so repeating
	// it on every reconcile of a finished backup is harmless (ADR-001).
	if sb.Status.Phase == ldapv1alpha1.BackupPhaseCompleted || sb.Status.Phase == ldapv1alpha1.BackupPhaseFailed {
		if sb.Status.JobName != "" {
			if err := reapJob(ctx, r.Client, sb.Namespace, sb.Status.JobName); err != nil {
				return ctrl.Result{}, err
			}
		}
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

	// Where the artifact comes from is a static fact about how backups work
	// (the Job co-locates with pod-0), so it is recorded on every pass, not only
	// the one that creates the Job — an operator restart between Create and the
	// status patch must not leave the record blank.
	sb.Status.SourcePod = backupSourcePod(sc)

	jobName := sb.Name + "-backup"
	job := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: sb.Namespace}, job)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	jobExists := err == nil

	// Record the circumstances the artifact is taken under: which pod it comes
	// from, where that pod sat in the replication timeline, and what the cluster
	// said about convergence and about its suffix entry. This is honesty, not
	// policy — nothing here can refuse or delay the backup (ADR-014 amendment
	// 2026-09-12).
	//
	// The trigger is the record's own absence, never the Job's (ADR-026 R3) —
	// see shouldRecordSourceCircumstances. jobExists is passed on only to caveat
	// what the recorded CSN vector means: a lower bound when recorded before the
	// dump, the source's later position when recorded after it.
	if shouldRecordSourceCircumstances(sb.Status) {
		r.recordSource(ctx, sb, sd, sc, jobExists)
	}

	// Create the Job if it doesn't exist yet.
	if !jobExists {
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
			// ADR-018 R2: the Job is reaped on the next reconcile, so capture the
			// cause into status now, while its pods still exist.
			msg := jobFailureSummary(job, jobPods(ctx, r.Client, sb.Namespace, jobName))
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

// recordSource stamps the backup's source circumstances into status: the pod
// the artifact is read from, that pod's contextCSN vector, and a SourceConverged
// condition consuming the cluster's own ReplicationConverged verdict.
//
// Why this exists: a SlapdBackup always slapcats pod-0, but under multi-master
// a write ACKed through the cluster Service seconds earlier may have landed on
// another pod and not yet reached pod-0 — so the artifact can legitimately miss
// it. That is inherent to backing up one replica of a replicating set and is not
// a defect. The defect was that it was SILENT.
//
// Best-effort throughout: a failed CSN read logs and leaves the field unset. A
// backup never fails because its bookkeeping did.
//
// late says the backup Job already exists, i.e. the dump may already have been
// taken. It changes nothing about what is read — only the SourceConverged
// message, which then retracts the lower-bound reading of sourceContextCSN
// (lateRecordingCaveat).
//
// status.SourcePod is NOT written here: the caller stamps it unconditionally on
// every pass, so it has exactly one writer (it is a pure function of the
// cluster, needs no I/O, and must be right even on a pass that skips this).
func (r *SlapdBackupReconciler) recordSource(ctx context.Context, sb *ldapv1alpha1.SlapdBackup, sd *ldapv1alpha1.SlapdDatabase, sc *ldapv1alpha1.SlapdCluster, late bool) {
	log := logf.FromContext(ctx)

	sourcePod := backupSourcePod(sc)
	if late {
		log.Info("recording backup source circumstances late: the record was missing while the Job already existed",
			"backup", sb.Name, "pod", sourcePod)
	}
	setCondition(&sb.Status.Conditions, recordedSourceConvergedCondition(sc, sb.Generation, metav1.Now(), late))

	// SourceSuffixHealthy (ADR-025 C2): is the source pod's suffix entry a real
	// entry, or the hidden glue a multi-site seed race leaves behind — in which
	// case the artifact will fail restore preflight, and this record is how a
	// reader finds out without downloading it. Record-only, best-effort.
	outcome, detail := r.probeSourceSuffix(ctx, sb.Namespace, sd, sc, sourcePod)
	setCondition(&sb.Status.Conditions, sourceSuffixHealthyCondition(outcome, detail, sb.Generation, metav1.Now()))
	if outcome != suffixProbeVisible {
		log.Info("backup source suffix probe not healthy (recorded, backup proceeds)",
			"pod", sourcePod, "suffix", sd.Spec.Suffix, "outcome", outcome, "detail", detail)
	}

	csns, err := r.readSourceContextCSN(ctx, sb.Namespace, sd, sc, sourcePod)
	if err != nil {
		log.Info("backup source contextCSN read failed; status.sourceContextCSN left unset",
			"pod", sourcePod, "suffix", sd.Spec.Suffix, "err", err)
		return
	}
	sb.Status.SourceContextCSN = normalizedCSNVector(csns)
}

// probeSourceSuffix classifies the backup source pod's suffix entry, binding as
// the replication identity (ADR-008, ADR-027). The classification itself is the
// shared suffixprobe.Probe — ordinary base search, then ManageDsaIT to tell a
// hidden glue from a genuinely absent entry (ADR-025). What lives here is only
// how to reach that pod. Best-effort: any transport failure reports
// suffixProbeError.
func (r *SlapdBackupReconciler) probeSourceSuffix(ctx context.Context, ns string, sd *ldapv1alpha1.SlapdDatabase, sc *ldapv1alpha1.SlapdCluster, sourcePod string) (suffixProbeOutcome, string) {
	if sd.Spec.Suffix == "" {
		return suffixProbeError, fmt.Sprintf("database %q has no suffix", sd.Name)
	}
	host := fmt.Sprintf("%s.%s-headless.%s.svc.%s", sourcePod, sc.Name, ns, r.clusterDomain())
	tlsEnabled := sc.Spec.LDAP.TLS.Enabled
	port := ldapContainerPort
	if tlsEnabled {
		port = ldapsContainerPort
	}
	bindDNs := csnBindDNs(sd.Name, sd.Spec.Suffix)
	bindPW, err := r.replicationPassword(ctx, ns, sd)
	if err != nil {
		return suffixProbeError, err.Error()
	}

	scheme := "ldap"
	dialOpts := []ldap.DialOpt{ldap.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second})}
	if tlsEnabled {
		scheme = "ldaps"
		dialOpts = append(dialOpts, ldap.DialWithTLSConfig(&tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // per-pod headless DNS, CA trust only (ADR-007 §7)
		}))
	}
	conn, err := ldap.DialURL(fmt.Sprintf("%s://%s:%d", scheme, host, port), dialOpts...)
	if err != nil {
		return suffixProbeError, err.Error()
	}
	defer conn.Close()
	conn.SetTimeout(ldapRequestTimeout)
	// Same candidate order as the CSN queries (csnBindDNs): the node-local
	// identity first, the legacy one as the fallback that keeps this working
	// against a pod the cutover has not reached yet.
	var bindErr error
	for _, dn := range bindDNs {
		if bindErr = conn.Bind(dn, bindPW); bindErr == nil {
			break
		}
	}
	if bindErr != nil {
		return suffixProbeError, bindErr.Error()
	}

	obs := suffixprobe.Probe(conn, sd.Spec.Suffix)
	return obs.Outcome, obs.Detail
}

// readSourceContextCSN reads contextCSN off the backup's source pod, using the
// database's replication bind credentials — the same identity and the same
// per-pod headless-DNS path the SlapdCluster controller's CSN monitoring uses
// (ADR-008), so it works wherever that already works.
func (r *SlapdBackupReconciler) readSourceContextCSN(ctx context.Context, ns string, sd *ldapv1alpha1.SlapdDatabase, sc *ldapv1alpha1.SlapdCluster, sourcePod string) ([]string, error) {
	if sd.Spec.Suffix == "" {
		return nil, fmt.Errorf("database %q has no suffix", sd.Name)
	}
	host := fmt.Sprintf("%s.%s-headless.%s.svc.%s", sourcePod, sc.Name, ns, r.clusterDomain())
	tlsEnabled := sc.Spec.LDAP.TLS.Enabled
	port := ldapContainerPort
	if tlsEnabled {
		port = ldapsContainerPort
	}
	bindPW, err := r.replicationPassword(ctx, ns, sd)
	if err != nil {
		return nil, err
	}
	return queryContextCSN(host, port, tlsEnabled, sd.Spec.Suffix,
		csnBindDNs(sd.Name, sd.Spec.Suffix), bindPW)
}

// replicationPassword reads a database's replication bind password from its
// credentials Secret (mirrors the SlapdCluster controller's listDatabaseInfo).
func (r *SlapdBackupReconciler) replicationPassword(ctx context.Context, ns string, sd *ldapv1alpha1.SlapdDatabase) (string, error) {
	secretName := sd.Name + "-credentials"
	if sd.Spec.Credentials.SecretName != "" {
		secretName = sd.Spec.Credentials.SecretName
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Name: secretName, Namespace: ns}, secret); err != nil {
		return "", fmt.Errorf("read %s: %w", secretName, err)
	}
	pw := string(secret.Data["replication-password"])
	if pw == "" {
		return "", fmt.Errorf("secret %s has no replication-password", secretName)
	}
	return pw, nil
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
	ac, err := applyConfiguration(statusPatch)
	if err != nil {
		logf.FromContext(ctx).Error(err, "failed to build SlapdBackup status apply configuration")
		return
	}
	if err := r.Status().Apply(ctx, ac, client.ForceOwnership, client.FieldOwner(backupFieldManager)); err != nil {
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
