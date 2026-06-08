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

const (
	backupStagingPath = "/staging"
	backupDumpFile    = "/staging/dump.ldif.gz"
)

// backupPodSecurityContext mirrors the StatefulSet's pod security context so the
// backup Job can read the slapd-owned PVC files (fsGroup) and stays admissible
// to PSA "restricted" namespaces.
func backupPodSecurityContext(sc *ldapv1alpha1.SlapdCluster) *corev1.PodSecurityContext {
	if sc.Spec.SecurityContext != nil {
		return sc.Spec.SecurityContext
	}
	uid := int64(1024)
	gid := int64(1024)
	nonRoot := true
	return &corev1.PodSecurityContext{
		RunAsUser:      &uid,
		RunAsGroup:     &gid,
		FSGroup:        &gid,
		RunAsNonRoot:   &nonRoot,
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// buildBackupJob constructs the two-step backup Job (ADR-014 Phase 3): an init
// container (slapd-init image) runs `slapcat | gzip` into an emptyDir staging
// volume, then the main container (operator image) uploads it to S3 via the
// `manager backup-upload` subcommand. The Job is co-located with pod-0
// (required PodAffinity) so it can mount that pod's ReadWriteOnce config/data
// PVCs while slapd holds them (RWO permits same-node co-mount; slapcat is safe
// online on back-mdb — see ADR-014).
func buildBackupJob(sb *ldapv1alpha1.SlapdBackup, sd *ldapv1alpha1.SlapdDatabase, sc *ldapv1alpha1.SlapdCluster, initImage, operatorImage, objectKey string) *batchv1.Job {
	st := sb.Spec.Storage
	configPVC := "config-" + sc.Name + "-0"
	dataPVC := "data-" + sc.Name + "-0"

	noEsc := false
	dropAll := &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}

	// slapcat → gzip into staging. bash + pipefail so a slapcat failure isn't
	// masked by gzip's exit 0. Config is read-only; data is RW because LMDB
	// registers a reader slot in the lock file even for a read transaction.
	slapcat := corev1.Container{
		Name:            "slapcat",
		Image:           initImage,
		ImagePullPolicy: sc.Spec.Images.Init.PullPolicy,
		Command:         []string{"bash", "-c", `set -eo pipefail; slapcat -F /config/slapd.d -b "$SUFFIX" | gzip -c > ` + backupDumpFile},
		Env:             []corev1.EnvVar{{Name: "SUFFIX", Value: sd.Spec.Suffix}},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "config", MountPath: "/config", ReadOnly: true},
			{Name: "data", MountPath: "/data"},
			{Name: "staging", MountPath: backupStagingPath},
		},
		SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &noEsc, Capabilities: dropAll},
	}

	uploadArgs := []string{"backup-upload", "--bucket", st.Bucket, "--key", objectKey, "--file", backupDumpFile}
	if st.Endpoint != "" {
		uploadArgs = append(uploadArgs, "--endpoint", st.Endpoint)
	}
	if st.Region != "" {
		uploadArgs = append(uploadArgs, "--region", st.Region)
	}
	if st.InsecureTLS {
		uploadArgs = append(uploadArgs, "--insecure-tls")
	}

	secretRef := func(key string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: st.CredentialsSecretName},
			Key:                  key,
		}}
	}
	upload := corev1.Container{
		Name:    "upload",
		Image:   operatorImage,
		Command: []string{"/manager"},
		Args:    uploadArgs,
		Env: []corev1.EnvVar{
			{Name: "AWS_ACCESS_KEY_ID", ValueFrom: secretRef("access-key-id")},
			{Name: "AWS_SECRET_ACCESS_KEY", ValueFrom: secretRef("secret-access-key")},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "staging", MountPath: backupStagingPath, ReadOnly: true},
		},
		SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &noEsc, Capabilities: dropAll},
	}

	backoff := int32(2)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sb.Name + "-backup",
			Namespace: sb.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":            "slapd",
				"app.kubernetes.io/instance":        sc.Name,
				"app.kubernetes.io/component":       "backup",
				"ldap.chuck-chuck-chuck.net/backup": sb.Name,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:    corev1.RestartPolicyNever,
					SecurityContext:  backupPodSecurityContext(sc),
					ImagePullSecrets: sc.Spec.ImagePullSecrets,
					InitContainers:   []corev1.Container{slapcat},
					Containers:       []corev1.Container{upload},
					Volumes: []corev1.Volume{
						{Name: "config", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: configPVC}}},
						{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: dataPVC}}},
						{Name: "staging", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}

	// Co-locate with pod-0's node so the RWO PVCs are mountable (mariadb-operator
	// PhysicalBackup pattern). Tunable off via spec.podAffinity for clusters that
	// forbid co-scheduling onto data nodes.
	if sb.PodAffinityEnabled() {
		job.Spec.Template.Spec.Affinity = &corev1.Affinity{
			PodAffinity: &corev1.PodAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
					LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						"app.kubernetes.io/instance":         sc.Name,
						"statefulset.kubernetes.io/pod-name": sc.Name + "-0",
					}},
					TopologyKey: "kubernetes.io/hostname",
				}},
			},
		}
	}

	return job
}
