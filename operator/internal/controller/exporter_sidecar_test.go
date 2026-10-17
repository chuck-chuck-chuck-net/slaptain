/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// spec.monitoring.enabled (ADR-033 Surface 3) is the first field added under
// ADR-013's 2026-10-08 amendment: a field whose purpose is to change the pod
// rolls when the user flips it, and only then. Its tests must pin two things:
//
//   - unset or off is BYTE-IDENTICAL to the template without the feature, so
//     upgrading the operator onto an existing cluster rolls nothing;
//   - it defaults to the state that adds nothing.
//
// The first is asserted against golden files recorded from the tree BEFORE the
// feature existed (testdata/podtemplate-*.golden.json). A golden is the only
// shape of this check that catches a change nobody thought to name — an
// always-present volume, an annotation, a reordered env var — which is exactly
// how every roll trigger ADR-013 retired got in.
//
// Regenerating the goldens is NOT how you make a red here go green. Run with
// -update-pod-template-golden only when the template is MEANT to change for
// every existing cluster, and say in the commit that the change rolls the fleet.
var updatePodTemplateGolden = flag.Bool("update-pod-template-golden", false,
	"rewrite testdata/podtemplate-*.golden.json from the current builder (rolls every existing cluster's pods — say so in the commit)")

const (
	// testOperatorVersion is the fixture operator's release, the tag its
	// image and the defaulted data-plane images share.
	testOperatorVersion = "1.2.3"

	goldenOperatorImage = "registry.example/slaptain/operator:" + testOperatorVersion
	// goldenExporterImage is what goldenReconciler defaults the sidecar to:
	// the operator's repository with its last segment swapped, at the
	// compiled-in exporter release — not the operator's tag.
	goldenExporterImage = "registry.example/slaptain/slapd-exporter:" + exporterDefaultVersion
	goldenClusterName   = "slapd"
	goldenNamespace     = "slaptain"
	goldenTLSSecret     = "slapd-tls"
)

// goldenReconciler is the reconciler every golden is rendered with. The image
// fields are set so image references are concrete in the golden.
func goldenReconciler() *SlapdClusterReconciler {
	return &SlapdClusterReconciler{
		ClusterDomain:   testClusterDomain,
		DefaultImageTag: testOperatorVersion,
		OperatorImage:   goldenOperatorImage,
	}
}

// goldenCluster is a representative replicated cluster that exercises every
// optional branch of the pod template: TLS, an external peer CA, a Multus
// annotation, slapd resources, a pull secret, an explicit log level. It is
// rebuilt on every call because buildStatefulSetSpec writes into the spec's
// security context.
func goldenCluster() *ldapv1alpha1.SlapdCluster {
	sc := &ldapv1alpha1.SlapdCluster{}
	sc.Name = goldenClusterName
	sc.Namespace = goldenNamespace
	sc.Spec.Replicas = 3
	sc.Spec.ReadReplicas = 2
	sc.Spec.LogLevel = new(int32(256))
	sc.Spec.Replication.Enabled = true
	sc.Spec.Replication.Network = &ldapv1alpha1.ReplicationNetworkConfig{
		Mode:          "multus",
		MultusNetwork: "infra/replication-net",
	}
	sc.Spec.Replication.ExternalPeers = []ldapv1alpha1.ExternalPeer{{
		Name:          "site-b",
		URI:           "ldaps://ldap.site-b.example:636",
		TLSSecretName: "site-b-ca",
	}}
	sc.Spec.LDAP.TLS.Enabled = true
	sc.Spec.LDAP.TLS.SecretName = goldenTLSSecret
	sc.Spec.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("250m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
	}
	sc.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "regcred"}}
	return sc
}

func goldenPath(readOnly bool) string {
	if readOnly {
		return filepath.Join("testdata", "podtemplate-readonly.golden.json")
	}
	return filepath.Join("testdata", "podtemplate-readwrite.golden.json")
}

func marshalTemplate(t *testing.T, tmpl corev1.PodTemplateSpec) []byte {
	t.Helper()
	b, err := json.MarshalIndent(tmpl, "", "  ")
	if err != nil {
		t.Fatalf("marshal pod template: %v", err)
	}
	return append(b, '\n')
}

func readGolden(t *testing.T, readOnly bool) []byte {
	t.Helper()
	b, err := os.ReadFile(goldenPath(readOnly))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	return b
}

// Upgrading the operator onto an existing cluster must roll nothing: with
// monitoring unset, or explicitly off, the pod template is byte-identical to
// the template recorded before the feature existed — in every container, every
// volume, every annotation. Resources set while the switch is off are still
// nothing: they configure a sidecar that does not exist.
func TestPodTemplateIsUnchangedWhileMonitoringIsOff(t *testing.T) {
	variants := []struct {
		name string
		set  func(*ldapv1alpha1.SlapdCluster)
	}{
		{"monitoring unset", func(*ldapv1alpha1.SlapdCluster) {}},
		{"monitoring.enabled=false", func(sc *ldapv1alpha1.SlapdCluster) {
			sc.Spec.Monitoring.Enabled = new(false)
		}},
		{"monitoring.enabled=false with sidecar resources set", func(sc *ldapv1alpha1.SlapdCluster) {
			sc.Spec.Monitoring.Enabled = new(false)
			sc.Spec.Monitoring.Resources = &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m")},
			}
		}},
	}

	for _, readOnly := range []bool{false, true} {
		if *updatePodTemplateGolden {
			got := marshalTemplate(t, goldenReconciler().buildStatefulSetSpec(goldenCluster(), readOnly).Template)
			if err := os.MkdirAll("testdata", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(goldenPath(readOnly), got, 0o644); err != nil {
				t.Fatal(err)
			}
			t.Logf("rewrote %s", goldenPath(readOnly))
		}
		want := readGolden(t, readOnly)

		for _, v := range variants {
			sc := goldenCluster()
			v.set(sc)
			got := marshalTemplate(t, goldenReconciler().buildStatefulSetSpec(sc, readOnly).Template)
			if !bytes.Equal(got, want) {
				t.Errorf("readOnly=%v, %s: the pod template differs from %s — every existing cluster "+
					"would roll on operator upgrade (ADR-013 amendment 2026-10-08).\n--- got ---\n%s",
					readOnly, v.name, goldenPath(readOnly), got)
			}
		}
	}
}

// Opting in adds exactly one container and changes nothing else: init
// container, slapd container, volumes, pod-level fields and metadata all equal
// the monitoring-off template (which the test above holds byte-identical to the
// golden). Anything else the feature touches would turn "the roll happens when
// the user opts in" into "the roll also changes slapd". The baseline is built in
// memory rather than decoded from the golden so that the comparison is not
// disturbed by JSON round-trip artefacts (resource.Quantity caching, nil vs
// empty maps).
func TestMonitoringEnabledAddsOnlyTheExporterContainer(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "read-write", true: "read-only"}[readOnly], func(t *testing.T) {
			monitoringEnabledAddsOnlyTheExporter(t, readOnly)
		})
	}
}

func monitoringEnabledAddsOnlyTheExporter(t *testing.T, readOnly bool) {
	t.Helper()
	golden := goldenReconciler().buildStatefulSetSpec(goldenCluster(), readOnly).Template

	sc := goldenCluster()
	sc.Spec.Monitoring.Enabled = new(true)
	got := goldenReconciler().buildStatefulSetSpec(sc, readOnly).Template

	if n, want := len(got.Spec.Containers), len(golden.Spec.Containers)+1; n != want {
		t.Fatalf("readOnly=%v: %d containers, want %d (slapd + exporter)", readOnly, n, want)
	}
	// slapd stays first: kubectl logs/exec/debug default to the first
	// container, and every runbook in this repo means slapd by that.
	if !reflect.DeepEqual(got.Spec.Containers[0], golden.Spec.Containers[0]) {
		t.Errorf("readOnly=%v: the slapd container changed (or is no longer first) when monitoring "+
			"was enabled\n got: %+v\nwant: %+v", readOnly, got.Spec.Containers[0], golden.Spec.Containers[0])
	}
	if c := got.Spec.Containers[len(got.Spec.Containers)-1]; c.Name != exporterContainerName {
		t.Errorf("readOnly=%v: added container is %q, want %q", readOnly, c.Name, exporterContainerName)
	}
	if !reflect.DeepEqual(got.Spec.InitContainers, golden.Spec.InitContainers) {
		t.Errorf("readOnly=%v: init containers changed when monitoring was enabled", readOnly)
	}
	if !reflect.DeepEqual(got.Spec.Volumes, golden.Spec.Volumes) {
		t.Errorf("readOnly=%v: volumes changed when monitoring was enabled — the sidecar needs only "+
			"the existing \"run\" emptyDir\n got: %+v\nwant: %+v", readOnly, got.Spec.Volumes, golden.Spec.Volumes)
	}
	if !reflect.DeepEqual(got.ObjectMeta, golden.ObjectMeta) {
		t.Errorf("readOnly=%v: pod template metadata changed when monitoring was enabled\n got: %+v\nwant: %+v",
			readOnly, got.ObjectMeta, golden.ObjectMeta)
	}
	gotPod, wantPod := got.Spec, golden.Spec
	gotPod.Containers, wantPod.Containers = nil, nil
	if !reflect.DeepEqual(gotPod, wantPod) {
		t.Errorf("readOnly=%v: pod-level fields changed when monitoring was enabled\n got: %+v\nwant: %+v",
			readOnly, gotPod, wantPod)
	}
}

// findExporter returns the one container named exporterContainerName, failing
// the test when there is none or more than one.
func findExporter(t *testing.T, tmpl corev1.PodTemplateSpec, label string) corev1.Container {
	t.Helper()
	var found []corev1.Container
	for _, c := range tmpl.Spec.Containers {
		if c.Name == exporterContainerName {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s: %d containers named %q, want exactly 1", label, len(found), exporterContainerName)
	}
	return found[0]
}

// The sidecar's shape is the interface pinned between the exporter (S3-c1) and
// the pod template (S3-c2), plus the properties ADR-033 decision 4 and its
// 2026-10-08 amendments require of a container living next to slapd.
func TestExporterSidecarShape(t *testing.T) {
	sidecarResources := &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("10m"),
			corev1.ResourceMemory: resource.MustParse("32Mi"),
		},
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
	}
	// A user-supplied pod securityContext with a non-default UID: the sidecar
	// must inherit whatever the pod runs as, never pin 1024 itself.
	customUID := func(sc *ldapv1alpha1.SlapdCluster) {
		sc.Spec.SecurityContext = &corev1.PodSecurityContext{
			RunAsUser:  new(int64(1000650000)),
			RunAsGroup: new(int64(0)),
			FSGroup:    new(int64(1000650000)),
		}
	}

	cases := []struct {
		name          string
		set           func(*ldapv1alpha1.SlapdCluster)
		wantResources corev1.ResourceRequirements
	}{
		{
			name:          "no sidecar resources: empty, not slapd's spec.resources",
			set:           func(*ldapv1alpha1.SlapdCluster) {},
			wantResources: corev1.ResourceRequirements{},
		},
		{
			name:          "sidecar resources set: used verbatim",
			set:           func(sc *ldapv1alpha1.SlapdCluster) { sc.Spec.Monitoring.Resources = sidecarResources.DeepCopy() },
			wantResources: *sidecarResources,
		},
		{
			name:          "custom pod UID: inherited, not overridden",
			set:           customUID,
			wantResources: corev1.ResourceRequirements{},
		},
	}

	for _, tc := range cases {
		for _, readOnly := range []bool{false, true} {
			label := tc.name
			if readOnly {
				label += " (read-only)"
			} else {
				label += " (read-write)"
			}
			t.Run(label, func(t *testing.T) {
				sc := goldenCluster()
				sc.Spec.Monitoring.Enabled = new(true)
				tc.set(sc)
				tmpl := goldenReconciler().buildStatefulSetSpec(sc, readOnly).Template
				c := findExporter(t, tmpl, label)

				// slapd is the first container (pinned by
				// TestMonitoringEnabledAddsOnlyTheExporterContainer).
				slapd := &tmpl.Spec.Containers[0]
				if slapd.Name == exporterContainerName {
					t.Fatal("the exporter displaced slapd as the first container")
				}

				// spec.images.exporter unset: the slapd-exporter image beside
				// the operator's, at the compiled-in exporter release (ADR-033
				// decision 4, "Packaging").
				if want := goldenExporterImage; c.Image != want {
					t.Errorf("image = %q, want %q", c.Image, want)
				}
				// No command, no arguments: the image's entrypoint and the
				// exporter's defaults (ldapi socket, SASL EXTERNAL, :9330) are
				// the whole contract, so the two version lines can drift.
				if c.Command != nil || c.Args != nil {
					t.Errorf("command = %q, args = %q; the operator must pass neither", c.Command, c.Args)
				}

				// The ldapi socket's directory, READ-ONLY — connect() needs
				// write permission on the socket inode, not a writable mount —
				// and nothing else: the exporter reads cn=Monitor, never a PVC,
				// a certificate or a Secret.
				wantMounts := []corev1.VolumeMount{{Name: "run", MountPath: "/run/openldap", ReadOnly: true}}
				if !reflect.DeepEqual(c.VolumeMounts, wantMounts) {
					t.Errorf("volumeMounts = %+v, want %+v", c.VolumeMounts, wantMounts)
				}

				wantPorts := []corev1.ContainerPort{{Name: "metrics", ContainerPort: 9330, Protocol: corev1.ProtocolTCP}}
				if !reflect.DeepEqual(c.Ports, wantPorts) {
					t.Errorf("ports = %+v, want %+v", c.Ports, wantPorts)
				}

				// SASL EXTERNAL over ldapi: no credential of any kind.
				for _, e := range c.Env {
					if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
						t.Errorf("env %q reads a Secret; the exporter binds EXTERNAL and needs no credential", e.Name)
					}
				}
				for _, ef := range c.EnvFrom {
					if ef.SecretRef != nil {
						t.Errorf("envFrom reads Secret %q; the exporter needs no credential", ef.SecretRef.Name)
					}
				}

				assertExporterSecurityContext(t, c.SecurityContext, slapd.SecurityContext)

				// Ready is the AND of all containers: a probe here would let the
				// exporter take a serving slapd out of its Service.
				if c.ReadinessProbe != nil || c.LivenessProbe != nil || c.StartupProbe != nil {
					t.Errorf("the sidecar carries a probe (readiness=%v liveness=%v startup=%v); it must never "+
						"be able to make the pod NotReady or restart it", c.ReadinessProbe != nil,
						c.LivenessProbe != nil, c.StartupProbe != nil)
				}

				if !reflect.DeepEqual(c.Resources, tc.wantResources) {
					t.Errorf("resources = %+v, want %+v", c.Resources, tc.wantResources)
				}
			})
		}
	}
}

// Opt-in: unset is off. An opt-out default would make the operator upgrade
// itself the roll trigger (ADR-013 amendment 2026-10-08).
func TestDesiredMonitoringEnabled(t *testing.T) {
	cases := []struct {
		name    string
		enabled *bool
		want    bool
	}{
		{"unset is off — the upgrade must add nothing to the pod", nil, false},
		{"explicit false is off", new(false), false},
		{"explicit true is on", new(true), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := &ldapv1alpha1.SlapdCluster{}
			sc.Spec.Monitoring.Enabled = tc.enabled
			if got := desiredMonitoringEnabled(sc); got != tc.want {
				t.Errorf("desiredMonitoringEnabled(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
	if desiredMonitoringEnabled(nil) {
		t.Error("desiredMonitoringEnabled(nil) = true, want false")
	}
}

// assertExporterSecurityContext pins the sidecar's hardening: exactly slapd's
// container securityContext, and no identity of its own.
func assertExporterSecurityContext(t *testing.T, got, slapdSec *corev1.SecurityContext) {
	t.Helper()
	sec := got
	if sec == nil {
		t.Fatal("securityContext is nil; the sidecar must be as hardened as slapd")
	}
	if !reflect.DeepEqual(sec, slapdSec) {
		t.Errorf("securityContext = %+v, want slapd's %+v", sec, slapdSec)
	}
	if sec.AllowPrivilegeEscalation == nil || *sec.AllowPrivilegeEscalation {
		t.Error("allowPrivilegeEscalation must be false")
	}
	if sec.ReadOnlyRootFilesystem == nil || !*sec.ReadOnlyRootFilesystem {
		t.Error("readOnlyRootFilesystem must be true")
	}
	if sec.Capabilities == nil || !reflect.DeepEqual(sec.Capabilities.Drop, []corev1.Capability{"ALL"}) ||
		len(sec.Capabilities.Add) != 0 {
		t.Errorf("capabilities = %+v, want drop [ALL] and add nothing", sec.Capabilities)
	}
	if sec.RunAsUser != nil || sec.RunAsGroup != nil {
		t.Errorf("runAsUser/runAsGroup set on the sidecar (%v/%v); the pod securityContext decides "+
			"the UID and the monitor ACL is UID-agnostic", sec.RunAsUser, sec.RunAsGroup)
	}
}

// exporterContainer renders a monitoring-enabled read-write template and
// returns its exporter container.
func exporterContainer(t *testing.T, r *SlapdClusterReconciler, sc *ldapv1alpha1.SlapdCluster) corev1.Container {
	t.Helper()
	sc.Spec.Monitoring.Enabled = new(true)
	return findExporter(t, r.buildStatefulSetSpec(sc, false).Template, "exporter image")
}

// spec.images.exporter resolves like slapd and init — the repository is the
// operator's with its trailing path segment swapped — except for the default
// TAG, which is the compiled-in exporter release rather than the operator's
// tag: the exporter has its own version line (ADR-033 decision 4, "Packaging").
func TestExporterImageResolution(t *testing.T) {
	const canonicalExporter = canonicalRegistryPath + "/slapd-exporter"
	cases := []struct {
		name          string
		operatorImage string
		defaultTag    string
		exporter      ldapv1alpha1.SlapdImageConfig
		wantImage     string
		wantPolicy    corev1.PullPolicy
	}{
		{
			name:          "unset: the operator's repository, segment swapped, at the compiled-in version",
			operatorImage: "registry.example:5000/slaptain/operator:1.2.3",
			defaultTag:    testOperatorVersion,
			wantImage:     "registry.example:5000/slaptain/slapd-exporter:" + exporterDefaultVersion,
		},
		{
			name:          "unset: a digest-pinned operator image contributes its repository only",
			operatorImage: "registry.example/slaptain/operator@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			defaultTag:    testOperatorVersion,
			wantImage:     "registry.example/slaptain/slapd-exporter:" + exporterDefaultVersion,
		},
		{
			name:          "tag only: the derived repository with that tag",
			operatorImage: "registry.example:5000/slaptain/operator:1.2.3",
			defaultTag:    testOperatorVersion,
			exporter:      ldapv1alpha1.SlapdImageConfig{Tag: "9.9.9"},
			wantImage:     "registry.example:5000/slaptain/slapd-exporter:9.9.9",
		},
		{
			name:          "repository and tag: exactly those",
			operatorImage: goldenOperatorImage,
			defaultTag:    testOperatorVersion,
			exporter:      ldapv1alpha1.SlapdImageConfig{Repository: "mirror.example/team/slapd-exporter", Tag: "1.0.0"},
			wantImage:     "mirror.example/team/slapd-exporter:1.0.0",
		},
		{
			// Unlike slapd and init, an unpinned tag is NOT the operator's tag.
			name:          "repository only: that repository at the compiled-in version",
			operatorImage: goldenOperatorImage,
			defaultTag:    testOperatorVersion,
			exporter:      ldapv1alpha1.SlapdImageConfig{Repository: "mirror.example/team/slapd-exporter"},
			wantImage:     "mirror.example/team/slapd-exporter:" + exporterDefaultVersion,
		},
		{
			name:          "pullPolicy set: used",
			operatorImage: goldenOperatorImage,
			defaultTag:    testOperatorVersion,
			exporter:      ldapv1alpha1.SlapdImageConfig{PullPolicy: corev1.PullAlways},
			wantImage:     "registry.example/slaptain/slapd-exporter:" + exporterDefaultVersion,
			wantPolicy:    corev1.PullAlways,
		},
		{
			name:       "no operator image (make run): canonical repository, compiled-in version",
			defaultTag: testOperatorVersion,
			wantImage:  canonicalExporter + ":" + exporterDefaultVersion,
		},
		{
			name:      "no operator image and no operator tag: still the compiled-in version, never latest",
			wantImage: canonicalExporter + ":" + exporterDefaultVersion,
		},
		{
			name:       "no operator image, tag only: canonical repository with that tag",
			defaultTag: testOperatorVersion,
			exporter:   ldapv1alpha1.SlapdImageConfig{Tag: "9.9.9"},
			wantImage:  canonicalExporter + ":9.9.9",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := goldenReconciler()
			r.OperatorImage, r.DefaultImageTag = tc.operatorImage, tc.defaultTag
			sc := goldenCluster()
			sc.Spec.Images.Exporter = tc.exporter
			c := exporterContainer(t, r, sc)
			if c.Image != tc.wantImage {
				t.Errorf("image = %q, want %q", c.Image, tc.wantImage)
			}
			// Unset stays unset: the API server defaults it from the tag
			// (Always for :latest, IfNotPresent otherwise).
			if c.ImagePullPolicy != tc.wantPolicy {
				t.Errorf("imagePullPolicy = %q, want %q", c.ImagePullPolicy, tc.wantPolicy)
			}
		})
	}
}

// An operator upgrade must not roll a monitoring-enabled cluster through its
// sidecar (ADR-013 amendment 2026-10-08; ADR-033 decision 4, "Packaging"): the
// exporter has its own version line, so an operator whose image differs only
// in tag renders the identical sidecar — with spec.images.exporter unset, not
// only when pinned.
func TestExporterTemplateUnderOperatorUpgrade(t *testing.T) {
	render := func(operatorImage, defaultTag string, pinAll bool) corev1.PodTemplateSpec {
		r := goldenReconciler()
		r.OperatorImage, r.DefaultImageTag = operatorImage, defaultTag
		sc := goldenCluster()
		sc.Spec.Monitoring.Enabled = new(true)
		if pinAll {
			// slapd and init pinned, so no operator-derived image is left
			// except the exporter's.
			sc.Spec.Images.Slapd = ldapv1alpha1.SlapdImageConfig{Repository: "registry.example/slaptain/slapd", Tag: testOperatorVersion}
			sc.Spec.Images.Init = ldapv1alpha1.SlapdImageConfig{Repository: "registry.example/slaptain/slapd-init", Tag: testOperatorVersion}
		}
		return r.buildStatefulSetSpec(sc, false).Template
	}
	const before, after = goldenOperatorImage, "registry.example/slaptain/operator:1.3.0"

	t.Run("exporter unset: the sidecar container is identical across the upgrade", func(t *testing.T) {
		a := findExporter(t, render(before, testOperatorVersion, false), "before")
		b := findExporter(t, render(after, "1.3.0", false), "after")
		if !reflect.DeepEqual(a, b) {
			t.Errorf("an operator upgrade changed the exporter sidecar — every pod of a monitoring-enabled "+
				"cluster would roll\n before: %+v\n  after: %+v", a, b)
		}
	})

	t.Run("slapd and init pinned, exporter unset: byte-identical pod template across the upgrade", func(t *testing.T) {
		a := marshalTemplate(t, render(before, testOperatorVersion, true))
		b := marshalTemplate(t, render(after, "1.3.0", true))
		if !bytes.Equal(a, b) {
			t.Errorf("an operator upgrade changed the pod template although slapd and init are pinned — "+
				"every pod of a monitoring-enabled cluster would roll\n--- before ---\n%s\n--- after ---\n%s", a, b)
		}
	})
}
