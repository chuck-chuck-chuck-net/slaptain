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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The mkdir Job — ADR-013 amendment of 2026-09-28, option H.
//
// back-mdb does not create olcDbDirectory, so a database cannot be added to a
// pod until the directory exists on that pod's PVC. The init container can only
// do that at pod start, which is why adding a database used to roll the fleet
// for an operation slapd performs live.
//
// ADR-018's co-located Job pattern removes the need: a Job pinned to the target
// pod's node mounts that pod's ReadWriteOnce data and accesslog PVCs while
// slapd holds them open (RWO is per-node, and creating a sibling directory
// touches no LMDB file), runs mkdir -p, and exits. No new RBAC verb, no
// permanent sidecar, no change to the slapd image — the three costs that made
// this ADR defer the fix in May.
//
// ADR-018 applies in full: the Job's pod is a lease on the PVC it names, so the
// operator reaps it from an observed terminal state, on success and on failure
// alike, with TTLSecondsAfterFinished as a crash backstop only.

const (
	// dirJobLabel marks every Job this file creates, for reaping and for a
	// human reading `kubectl get jobs`.
	dirJobLabel = "ldap.chuck-chuck-chuck.net/database-dirs"
	// dirJobTTL is the ADR-018 R4 crash backstop. Short: the Job's work is a
	// handful of mkdirs, so anything still around after this died.
	dirJobTTL = int32(300)
)

// errDatabaseDirsPending says the directories are being created but are not
// there yet. The per-pod caller already treats an error as "skip this pod and
// retry", which is exactly the required behaviour — so waiting costs a requeue
// rather than a blocked reconcile worker.
var errDatabaseDirsPending = errors.New("per-database directories are still being created")

// ensureDatabaseDirs guarantees that one pod's PVCs carry every per-database
// LMDB directory the cluster needs, creating them through a co-located Job.
//
// Returns nil only when a Job has been OBSERVED to succeed, so the caller may
// treat nil as "the directory is there". Called on the path that creates a
// database on a pod — that is, at most once per (pod, database set), and never
// in steady state.
func (r *SlapdDatabaseReconciler) ensureDatabaseDirs(
	ctx context.Context,
	sc *ldapv1alpha1.SlapdCluster,
	podName string,
	readOnly bool,
) error {
	log := logf.FromContext(ctx)

	var dbList ldapv1alpha1.SlapdDatabaseList
	if err := r.List(ctx, &dbList, client.InNamespace(sc.Namespace)); err != nil {
		return fmt.Errorf("list SlapdDatabases: %w", err)
	}
	paths := databaseDirPaths(databaseDirsFor(dbList.Items, sc.Name), sc, readOnly)
	if len(paths) == 0 {
		return nil
	}

	name := dirJobName(podName, paths)

	// Reap this pod's Jobs from any SUPERSEDED path set before looking at the
	// current one. The name is a hash of (pod, paths), so a path set that
	// changes between creating a Job and coming back to observe it — a second
	// database applied moments later, a scale across the 1<->2 accesslog
	// boundary, an operator restart mid-flight — leaves a Job nothing will ever
	// Get() again. Without this, only TTLSecondsAfterFinished removes it, and
	// ADR-018 R4 makes the TTL a CRASH backstop, not the routine path: until it
	// fires, that Job's pod holds a deletion lease on this pod's PVCs (R1).
	//
	// Observed live 2026-09-28: a `slapd-0-mkdir-7adce24e` from the demo's
	// one-database standalone stage outlived three database adds and was
	// removed by the TTL at 5m03s.
	//
	// Reaped regardless of state, not just when terminal: a Job for a
	// superseded path set has no reason to run, and a PENDING one (unschedulable
	// node) holds the lease exactly as firmly while never reaching a terminal
	// state to be swept from.
	if err := r.reapSupersededDirJobs(ctx, sc.Namespace, podName, name); err != nil {
		return err
	}

	job := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: sc.Namespace}, job)
	switch {
	case apierrors.IsNotFound(err):
		built, buildErr := r.buildDirJob(sc, podName, name, paths, readOnly)
		if buildErr != nil {
			return buildErr
		}
		if createErr := r.Create(ctx, built); createErr != nil && !apierrors.IsAlreadyExists(createErr) {
			return fmt.Errorf("create mkdir Job %s: %w", name, createErr)
		}
		log.Info("creating per-database directories", "pod", podName, "job", name, "paths", paths)
		return errDatabaseDirsPending
	case err != nil:
		return fmt.Errorf("get mkdir Job %s: %w", name, err)
	}

	switch {
	case job.Status.Succeeded > 0:
		// Reap before returning: the Job's pod holds a lease on this pod's
		// PVCs (ADR-018 R1/R2), and the directories it created outlive it.
		if reapErr := reapJob(ctx, r.Client, sc.Namespace, name); reapErr != nil {
			return reapErr
		}
		return nil
	case jobHasFailed(job):
		// Record the cause before the object that carries it is deleted
		// (ADR-018 R2), then reap so the next pass starts clean rather than
		// waiting forever on a Job that will never run again.
		summary := jobFailureSummary(job, jobPods(ctx, r.Client, sc.Namespace, name))
		if reapErr := reapJob(ctx, r.Client, sc.Namespace, name); reapErr != nil {
			return reapErr
		}
		return fmt.Errorf("creating per-database directories on %s failed: %s", podName, summary)
	default:
		return errDatabaseDirsPending
	}
}

// jobHasFailed reports whether a Job has exhausted its retries.
func jobHasFailed(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// databaseDirPaths renders the absolute paths one pod must carry.
//
// The accesslog half is dropped for read-only pods and for clusters with no
// accesslog PVC: an RO replica produces no changes and never gets the volume,
// so naming its PVC would make the Job unschedulable rather than thorough.
func databaseDirPaths(dirs databaseDirs, sc *ldapv1alpha1.SlapdCluster, readOnly bool) []string {
	var paths []string
	for _, d := range dirs.Data {
		paths = append(paths, "/data/"+d)
	}
	if !readOnly && sc.NeedsAccesslogVolume() {
		for _, d := range dirs.Accesslog {
			paths = append(paths, ldapv1alpha1.AccesslogDir(d))
		}
	}
	return paths
}

// dirJobName is deterministic in (pod, path set), which is what makes the whole
// thing idempotent: two databases added at once compute the same name and
// collapse onto one Job, a re-entrant reconcile finds the Job it created rather
// than making a second one, and a later database produces a different name
// because the path set changed.
func dirJobName(podName string, paths []string) string {
	sum := sha256.Sum256([]byte(strings.Join(paths, "\n")))
	return podName + "-mkdir-" + hex.EncodeToString(sum[:])[:8]
}

// buildDirJob constructs the Job. Deliberately minimal: the paths are baked
// into the command rather than read from the ConfigMap, because a Job is built
// fresh every time and has no pod template to keep stable.
func (r *SlapdDatabaseReconciler) buildDirJob(
	sc *ldapv1alpha1.SlapdCluster,
	podName, jobName string,
	paths []string,
	readOnly bool,
) (*batchv1.Job, error) {
	quoted := make([]string, 0, len(paths))
	for _, p := range paths {
		quoted = append(quoted, "'"+p+"'")
	}

	volumes := []corev1.Volume{pvcVolume("data", "data-"+podName)}
	mounts := []corev1.VolumeMount{{Name: volumeData, MountPath: dataMountPath}}
	if !readOnly && sc.NeedsAccesslogVolume() {
		volumes = append(volumes, pvcVolume("accesslog", "accesslog-"+podName))
		mounts = append(mounts, corev1.VolumeMount{Name: volumeAccesslog, MountPath: ldapv1alpha1.AccesslogRoot})
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: sc.Namespace,
			Labels: map[string]string{
				labelName:      appName,
				labelInstance:  sc.Name,
				labelComponent: "database-dirs",
				dirJobLabel:    podName,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: new(dirJobTTL),
			BackoffLimit:            new(int32(2)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:    corev1.RestartPolicyNever,
					SecurityContext:  backupPodSecurityContext(sc),
					ImagePullSecrets: sc.Spec.ImagePullSecrets,
					Containers: []corev1.Container{{
						Name:            "mkdir",
						Image:           r.imageRef(sc.Spec.Images.Init, defaultDataPlaneRepo(r.OperatorImage, "slapd-init")),
						ImagePullPolicy: sc.Spec.Images.Init.PullPolicy,
						Command:         []string{jobShell, "-c", "set -eux; mkdir -p " + strings.Join(quoted, " ")},
						VolumeMounts:    mounts,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: new(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{capabilityAll}},
							ReadOnlyRootFilesystem:   new(true),
						},
					}},
					Volumes: volumes,
					// Co-located with the target pod, because its PVCs are
					// ReadWriteOnce: RWO is a per-NODE constraint, so the Job
					// can mount them only where slapd already has them.
					Affinity: &corev1.Affinity{
						PodAffinity: &corev1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
								LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
									"statefulset.kubernetes.io/pod-name": podName,
								}},
								TopologyKey: "kubernetes.io/hostname",
							}},
						},
					},
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(sc, job, r.Scheme); err != nil {
		return nil, err
	}
	return job, nil
}

// reapSupersededDirJobs deletes this pod's mkdir Jobs other than `keep`.
//
// Selected by the pod label rather than by listing every Job in the namespace,
// so it can never touch a backup or restore Job — those carry their own labels
// and their own reaping rules.
func (r *SlapdDatabaseReconciler) reapSupersededDirJobs(
	ctx context.Context, namespace, podName, keep string,
) error {
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(namespace),
		client.MatchingLabels{dirJobLabel: podName}); err != nil {
		return fmt.Errorf("list mkdir Jobs for %s: %w", podName, err)
	}
	var errs []error
	for i := range jobs.Items {
		j := &jobs.Items[i]
		if j.Name == keep || j.DeletionTimestamp != nil {
			continue
		}
		if err := reapJob(ctx, r.Client, namespace, j.Name); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
