package cmd

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

const (
	slapdRepo   = "ghcr.io/chuck-chuck-chuck-net/slaptain/slapd"
	toolkitRepo = "ghcr.io/chuck-chuck-chuck-net/slaptain/slapd-toolkit"
)

func TestSplitImageRef(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantRepo string
		wantTag  string
	}{
		{"repo:tag", slapdRepo + ":v0.0.1", slapdRepo, "v0.0.1"},
		{"no tag", slapdRepo, slapdRepo, ""},
		{"registry port preserved", "registry.example:5000/slaptain/slapd:v1", "registry.example:5000/slaptain/slapd", "v1"},
		{"registry port, no tag", "registry.example:5000/slaptain/slapd", "registry.example:5000/slaptain/slapd", ""},
		{"digest yields no tag", slapdRepo + "@sha256:abc", slapdRepo, ""},
		{"empty", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, tag := splitImageRef(tc.in)
			if repo != tc.wantRepo || tag != tc.wantTag {
				t.Errorf("splitImageRef(%q) = (%q, %q), want (%q, %q)", tc.in, repo, tag, tc.wantRepo, tc.wantTag)
			}
		})
	}
}

func TestToolkitImageFor(t *testing.T) {
	podWith := func(containers ...corev1.Container) *corev1.Pod {
		return &corev1.Pod{Spec: corev1.PodSpec{Containers: containers}}
	}
	cases := []struct {
		name      string
		pod       *corev1.Pod
		container string
		want      string
	}{
		{
			name:      "derives from running slapd image, keeps tag",
			pod:       podWith(corev1.Container{Name: "slapd", Image: slapdRepo + ":v0.0.1"}),
			container: "slapd",
			want:      toolkitRepo + ":v0.0.1",
		},
		{
			name: "operator-defaulted image (real tag, not :latest)",
			pod: podWith(corev1.Container{Name: "init", Image: "x"},
				corev1.Container{Name: "slapd", Image: slapdRepo + ":2026-07-14"}),
			container: "slapd",
			want:      toolkitRepo + ":2026-07-14",
		},
		{
			name:      "private mirror follows the pod image",
			pod:       podWith(corev1.Container{Name: "slapd", Image: "registry.example:5000/mirror/slaptain/slapd:v2"}),
			container: "slapd",
			want:      "registry.example:5000/mirror/slaptain/slapd-toolkit:v2",
		},
		{
			name:      "untagged pod image falls back to latest tag",
			pod:       podWith(corev1.Container{Name: "slapd", Image: slapdRepo}),
			container: "slapd",
			want:      toolkitRepo + ":latest",
		},
		{
			name:      "non-conventional repo falls back to canonical toolkit",
			pod:       podWith(corev1.Container{Name: "slapd", Image: "example.com/weird/openldap:v1"}),
			container: "slapd",
			want:      toolkitRepo + ":v1",
		},
		{
			name:      "container not found falls back to first container",
			pod:       podWith(corev1.Container{Name: "slapd", Image: slapdRepo + ":v3"}),
			container: "does-not-exist",
			want:      toolkitRepo + ":v3",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolkitImageFor(tc.pod, tc.container); got != tc.want {
				t.Errorf("toolkitImageFor(...) = %q, want %q", got, tc.want)
			}
		})
	}
}
