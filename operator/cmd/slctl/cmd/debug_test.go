package cmd

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestSplitImageRef(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantRepo string
		wantTag  string
	}{
		{"repo:tag", "ghcr.io/chuck-chuck-chuck-net/slaptain/slapd:v0.0.1", "ghcr.io/chuck-chuck-chuck-net/slaptain/slapd", "v0.0.1"},
		{"no tag", "ghcr.io/chuck-chuck-chuck-net/slaptain/slapd", "ghcr.io/chuck-chuck-chuck-net/slaptain/slapd", ""},
		{"registry port preserved", "registry.example:5000/slaptain/slapd:v1", "registry.example:5000/slaptain/slapd", "v1"},
		{"registry port, no tag", "registry.example:5000/slaptain/slapd", "registry.example:5000/slaptain/slapd", ""},
		{"digest yields no tag", "ghcr.io/chuck-chuck-chuck-net/slaptain/slapd@sha256:abc", "ghcr.io/chuck-chuck-chuck-net/slaptain/slapd", ""},
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
			pod:       podWith(corev1.Container{Name: "slapd", Image: "ghcr.io/chuck-chuck-chuck-net/slaptain/slapd:v0.0.1"}),
			container: "slapd",
			want:      "ghcr.io/chuck-chuck-chuck-net/slaptain/slapd-toolkit:v0.0.1",
		},
		{
			name:      "operator-defaulted image (real tag, not :latest)",
			pod:       podWith(corev1.Container{Name: "init", Image: "x"}, corev1.Container{Name: "slapd", Image: "ghcr.io/chuck-chuck-chuck-net/slaptain/slapd:2026-07-14"}),
			container: "slapd",
			want:      "ghcr.io/chuck-chuck-chuck-net/slaptain/slapd-toolkit:2026-07-14",
		},
		{
			name:      "private mirror follows the pod image",
			pod:       podWith(corev1.Container{Name: "slapd", Image: "registry.example:5000/mirror/slaptain/slapd:v2"}),
			container: "slapd",
			want:      "registry.example:5000/mirror/slaptain/slapd-toolkit:v2",
		},
		{
			name:      "untagged pod image falls back to latest tag",
			pod:       podWith(corev1.Container{Name: "slapd", Image: "ghcr.io/chuck-chuck-chuck-net/slaptain/slapd"}),
			container: "slapd",
			want:      "ghcr.io/chuck-chuck-chuck-net/slaptain/slapd-toolkit:latest",
		},
		{
			name:      "non-conventional repo falls back to canonical toolkit",
			pod:       podWith(corev1.Container{Name: "slapd", Image: "example.com/weird/openldap:v1"}),
			container: "slapd",
			want:      "ghcr.io/chuck-chuck-chuck-net/slaptain/slapd-toolkit:v1",
		},
		{
			name:      "container not found falls back to first container",
			pod:       podWith(corev1.Container{Name: "slapd", Image: "ghcr.io/chuck-chuck-chuck-net/slaptain/slapd:v3"}),
			container: "does-not-exist",
			want:      "ghcr.io/chuck-chuck-chuck-net/slaptain/slapd-toolkit:v3",
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
