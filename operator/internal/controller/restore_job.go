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
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

const restoreLDIFFile = "/staging/restore.ldif.gz"

// databaseDataDir returns the /data subdirectory name for a database, matching
// the SlapdDatabase controller: spec.dataDirectory, or the CR name when unset.
func databaseDataDir(sd *ldapv1alpha1.SlapdDatabase) string {
	if sd.Spec.DataDirectory != "" {
		return sd.Spec.DataDirectory
	}
	return sd.Name
}

// restorePodTarget identifies one pod to restore and the PVCs to mount for it.
type restorePodTarget struct {
	jobName      string
	dataPVC      string
	configPVC    string // only needed when loadData (slapadd reads cn=config)
	accesslogPVC string // "" when the pod has no accesslog DB
	// loadData: RW pods download + wipe + slapadd directly (no syncrepl refresh,
	// so no ITS#9580). RO pods are wipe-only — they re-refresh from the restored
	// RW pods on scale-up, which is their normal mode and (having no accesslog)
	// is not exposed to ITS#9580. See ADR-014 amendment.
	loadData bool
}

// buildRestoreJob constructs the per-pod restore Job (ADR-014 amendment,
// "slapadd into every pod"). It runs while the cluster is scaled to 0, so pod-0
// is gone and there is no PodAffinity — mounting the bound RWO PVCs drives the
// Job onto the volumes' node.
func buildRestoreJob(sc *ldapv1alpha1.SlapdCluster, sd *ldapv1alpha1.SlapdDatabase, st ldapv1alpha1.S3StorageSpec, objectKey, initImage, operatorImage string, t restorePodTarget) *batchv1.Job {
	dataDir := databaseDataDir(sd)
	noEsc := false
	secCtx := &corev1.SecurityContext{
		AllowPrivilegeEscalation: &noEsc,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
	wipe := `rm -f "/data/$DATADIR/data.mdb" "/data/$DATADIR/lock.mdb"`
	// Under replication the pod has an accesslog DB (olcDbDirectory /accesslog,
	// its own PVC). Wipe its LMDB too: it is a transient change journal, and any
	// pre-restore delta left behind — most damagingly a delete at a CSN newer
	// than the restored contextCSN — is replayed by delta-syncrepl on scale-up,
	// silently undoing the restore. slapd recreates an empty accesslog on start,
	// and after a full identical reload there are no pending changes to ship.
	// Mirrors the RO-pod rationale below and the promotion-path hazard noted in
	// slapddatabase_controller.go ("wipe /accesslog manually"). See ADR-014.
	if t.accesslogPVC != "" {
		wipe += "\n" + `rm -f /accesslog/data.mdb /accesslog/lock.mdb`
	}

	volumes := []corev1.Volume{pvcVolume("data", t.dataPVC)}
	var initContainers, containers []corev1.Container

	if t.loadData {
		// RW pod: download the artifact, wipe the target DB files, slapadd offline.
		volumes = append(volumes,
			pvcVolume("config", t.configPVC),
			corev1.Volume{Name: "staging", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		)
		downloadArgs := []string{"restore-download", "--bucket", st.Bucket, "--key", objectKey, "--out", restoreLDIFFile}
		if st.Endpoint != "" {
			downloadArgs = append(downloadArgs, "--endpoint", st.Endpoint)
		}
		if st.Region != "" {
			downloadArgs = append(downloadArgs, "--region", st.Region)
		}
		if st.InsecureTLS {
			downloadArgs = append(downloadArgs, "--insecure-tls")
		}
		secretRef := func(key string) *corev1.EnvVarSource {
			return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: st.CredentialsSecretName},
				Key:                  key,
			}}
		}
		initContainers = []corev1.Container{{
			Name:    "download",
			Image:   operatorImage,
			Command: []string{"/manager"},
			Args:    downloadArgs,
			Env: []corev1.EnvVar{
				{Name: "AWS_ACCESS_KEY_ID", ValueFrom: secretRef("access-key-id")},
				{Name: "AWS_SECRET_ACCESS_KEY", ValueFrom: secretRef("secret-access-key")},
			},
			VolumeMounts:    []corev1.VolumeMount{{Name: "staging", MountPath: backupStagingPath}},
			SecurityContext: secCtx,
		}}

		// slapadd (-F) loads the whole cn=config; the accesslog DB's
		// olcDbDirectory must exist or it aborts at config-load. Mount it when
		// this (RW) pod has one.
		mounts := []corev1.VolumeMount{
			{Name: "config", MountPath: "/config", ReadOnly: true},
			{Name: "data", MountPath: "/data"},
			{Name: "staging", MountPath: backupStagingPath, ReadOnly: true},
		}
		if t.accesslogPVC != "" {
			volumes = append(volumes, pvcVolume("accesslog", t.accesslogPVC))
			mounts = append(mounts, corev1.VolumeMount{Name: "accesslog", MountPath: "/accesslog"})
		}
		containers = []corev1.Container{{
			Name:            "restore",
			Image:           initImage,
			ImagePullPolicy: sc.Spec.Images.Init.PullPolicy,
			Command:         []string{"bash", "-c", "set -eo pipefail\n" + wipe + "\ngunzip -c " + restoreLDIFFile + ` | slapadd -F /config/slapd.d -b "$SUFFIX"`},
			Env: []corev1.EnvVar{
				{Name: "SUFFIX", Value: sd.Spec.Suffix},
				{Name: "DATADIR", Value: dataDir},
			},
			VolumeMounts:    mounts,
			SecurityContext: secCtx,
		}}
	} else {
		// RO pod: wipe-only. Dropping the data (and its contextCSN) forces a
		// clean full refresh from the restored RW pods on scale-up, even when
		// the rolled-back data has lower CSNs than the RO pod last saw.
		containers = []corev1.Container{{
			Name:            "wipe",
			Image:           initImage,
			ImagePullPolicy: sc.Spec.Images.Init.PullPolicy,
			Command:         []string{"bash", "-c", "set -eo pipefail\n" + wipe},
			Env:             []corev1.EnvVar{{Name: "DATADIR", Value: dataDir}},
			VolumeMounts:    []corev1.VolumeMount{{Name: "data", MountPath: "/data"}},
			SecurityContext: secCtx,
		}}
	}

	backoff := int32(2)
	ttl := int32(3600) // finished restore Jobs auto-clean after 1h (native GC)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      t.jobName,
			Namespace: sc.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":                "slapd",
				"app.kubernetes.io/instance":            sc.Name,
				"app.kubernetes.io/component":           "restore",
				"ldap.chuck-chuck-chuck.net/database":   sd.Name,
				"ldap.chuck-chuck-chuck.net/restore-id": sc.Status.Restore.ID,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:    corev1.RestartPolicyNever,
					SecurityContext:  backupPodSecurityContext(sc),
					ImagePullSecrets: sc.Spec.ImagePullSecrets,
					InitContainers:   initContainers,
					Containers:       containers,
					Volumes:          volumes,
				},
			},
		},
	}
}
