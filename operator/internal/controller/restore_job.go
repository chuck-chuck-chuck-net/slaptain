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

// buildRestoreJob constructs the per-database restore Job (ADR-014 Phase 5),
// run while the cluster is held at 0 replicas (slapd stopped). An init container
// (operator image) downloads the gzipped LDIF from S3; the main container
// (slapd-init image) wipes the target DB files and runs offline `slapadd` into
// pod-0's data PVC. The Job is node-pinned to pod-0 so it can mount that pod's
// config/data PVCs.
func buildRestoreJob(sc *ldapv1alpha1.SlapdCluster, sd *ldapv1alpha1.SlapdDatabase, st ldapv1alpha1.S3StorageSpec, objectKey, initImage, operatorImage string) *batchv1.Job {
	configPVC := "config-" + sc.Name + "-0"
	dataPVC := "data-" + sc.Name + "-0"
	dataDir := databaseDataDir(sd)

	noEsc := false
	dropAll := &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}

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

	download := corev1.Container{
		Name:    "download",
		Image:   operatorImage,
		Command: []string{"/manager"},
		Args:    downloadArgs,
		Env: []corev1.EnvVar{
			{Name: "AWS_ACCESS_KEY_ID", ValueFrom: secretRef("access-key-id")},
			{Name: "AWS_SECRET_ACCESS_KEY", ValueFrom: secretRef("secret-access-key")},
		},
		VolumeMounts:    []corev1.VolumeMount{{Name: "staging", MountPath: backupStagingPath}},
		SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &noEsc, Capabilities: dropAll},
	}

	// Wipe the (empty) target DB files so slapadd loads cleanly and a retried Job
	// is idempotent, then stream the gunzipped LDIF into slapadd offline.
	restoreScript := `set -eo pipefail
rm -f "/data/$DATADIR/data.mdb" "/data/$DATADIR/lock.mdb"
gunzip -c ` + restoreLDIFFile + ` | slapadd -F /config/slapd.d -b "$SUFFIX"`

	restore := corev1.Container{
		Name:            "restore",
		Image:           initImage,
		ImagePullPolicy: sc.Spec.Images.Init.PullPolicy,
		Command:         []string{"bash", "-c", restoreScript},
		Env: []corev1.EnvVar{
			{Name: "SUFFIX", Value: sd.Spec.Suffix},
			{Name: "DATADIR", Value: dataDir},
		},
		// slapadd (-F) loads the whole cn=config; the accesslog DB's
		// olcDbDirectory must exist or it aborts at config-load. Mount it when
		// the cluster provisions the accesslog PVC.
		VolumeMounts: mountsWithAccesslog(sc, []corev1.VolumeMount{
			{Name: "config", MountPath: "/config", ReadOnly: true},
			{Name: "data", MountPath: "/data"},
			{Name: "staging", MountPath: backupStagingPath, ReadOnly: true},
		}),
		SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &noEsc, Capabilities: dropAll},
	}

	backoff := int32(2)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sd.Name + "-restore",
			Namespace: sc.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":              "slapd",
				"app.kubernetes.io/instance":          sc.Name,
				"app.kubernetes.io/component":         "restore",
				"ldap.chuck-chuck-chuck.net/database": sd.Name,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:    corev1.RestartPolicyNever,
					SecurityContext:  backupPodSecurityContext(sc),
					ImagePullSecrets: sc.Spec.ImagePullSecrets,
					InitContainers:   []corev1.Container{download},
					Containers:       []corev1.Container{restore},
					Volumes: volumesWithAccesslog(sc, []corev1.Volume{
						pvcVolume("config", configPVC),
						pvcVolume("data", dataPVC),
						{Name: "staging", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					}),
				},
			},
		},
	}

	// No PodAffinity here (unlike the backup Job): during restore the cluster is
	// scaled to 0, so pod-0 does not exist to co-locate with. Mounting the bound
	// RWO config/data PVCs drives scheduling onto the volumes' node, and the
	// scale-down wait guarantees pod-0 has released them first.

	return job
}
