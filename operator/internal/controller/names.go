/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import corev1 "k8s.io/api/core/v1"

// The vocabulary the controllers build objects from: names that must be
// spelled identically wherever they appear, because a pod template, a Job
// that mounts the same PVCs, and a selector all refer to one another by them.

// Recommended Kubernetes labels and the values slaptain stamps into them.
const (
	labelName      = "app.kubernetes.io/name"
	labelInstance  = "app.kubernetes.io/instance"
	labelComponent = "app.kubernetes.io/component"

	// appName is the app.kubernetes.io/name of every slaptain-managed pod.
	appName = "slapd"
)

// Containers, ports and volumes of the slapd pod — and of the Jobs that mount
// its PVCs (ADR-018), which must use the same volume names and mount paths.
const (
	slapdContainerName = "slapd"

	portNameLDAP  = "ldap"
	portNameLDAPS = "ldaps"

	volumeConfig    = "config"
	volumeData      = "data"
	volumeAccesslog = "accesslog"
	volumeStaging   = "staging"
	volumeTLS       = "tls"

	configMountPath = "/config"
	dataMountPath   = "/data"

	// jobShell runs the inline scripts of the operator's Jobs.
	jobShell = "bash"

	// envTrue is how a boolean reads in a container environment variable.
	envTrue = "true"
)

// capabilityAll is the capability set every container drops.
const capabilityAll corev1.Capability = "ALL"

// Kinds and API versions for the TypeMeta of server-side-applied objects.
const (
	apiVersionAppsV1  = "apps/v1"
	kindService       = "Service"
	kindStatefulSet   = "StatefulSet"
	kindSlapdDatabase = "SlapdDatabase"
)

// Keys of the operator-managed credential Secrets.
const (
	secretKeyRootPassword        = "root-password"
	secretKeyReplicationPassword = "replication-password"
)

// URL schemes the operator dials slapd on.
const (
	schemeLDAP  = "ldap"
	schemeLDAPS = "ldaps"
)

// cn=config vocabulary: object classes, attributes and values the operator
// reads and writes. Spelled as slapd spells them.
const (
	configDN = "cn=config"

	backendMDB = "mdb"

	overlayAccesslog = "accesslog"
	overlaySyncprov  = "syncprov"

	ocDatabaseConfig       = "olcDatabaseConfig"
	ocMdbConfig            = "olcMdbConfig"
	ocOverlayConfig        = "olcOverlayConfig"
	ocSyncProvConfig       = "olcSyncProvConfig"
	ocAccessLogConfig      = "olcAccessLogConfig"
	ocOrganizationalRole   = "organizationalRole"
	ocSimpleSecurityObject = "simpleSecurityObject"

	attrObjectClass     = "objectClass"
	attrEntryCSN        = "entryCSN"
	attrUserPassword    = "userPassword"
	attrOlcAccess       = "olcAccess"
	attrOlcPasswordHash = "olcPasswordHash"

	// olcTrue and olcFalse are cn=config's boolean syntax.
	olcTrue  = "TRUE"
	olcFalse = "FALSE"

	// aclDenyAll is the closing rule that denies everything not granted above.
	aclDenyAll = "to * by * none"

	// tlsReqcertAllow relaxes a syncrepl stanza's certificate check for
	// IP-addressed peers, whose certificates cannot name them (ADR-007).
	tlsReqcertAllow = " tls_reqcert=allow"
)

// Condition types shared across the operator's resources.
const (
	conditionReady   = "Ready"
	conditionApplied = "Applied"
)

// replicationModeConsumerOnly is spec.replication.mode for a cluster that only
// pulls from its external peers (ADR-010).
const replicationModeConsumerOnly = "consumer-only"
