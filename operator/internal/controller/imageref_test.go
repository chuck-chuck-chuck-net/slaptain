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
	"testing"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

const (
	operatorRepo     = "ghcr.io/chuck-chuck-chuck-net/slaptain/operator"
	operatorTag      = "v0.0.1"
	operatorImage    = operatorRepo + ":" + operatorTag
	registryPortRepo = "registry.example:5000/slaptain/operator"
	componentSlapd   = "slapd"
)

func TestStripImageTag(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"repo:tag", operatorImage, operatorRepo},
		{"no tag", operatorRepo, operatorRepo},
		{"registry port preserved", "registry.example:5000/slaptain/operator:v1", registryPortRepo},
		{"registry port, no tag", registryPortRepo, registryPortRepo},
		{"digest", "ghcr.io/chuck-chuck-chuck-net/slaptain/operator@sha256:abc123", operatorRepo},
		{testCaseEmpty, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripImageTag(tc.in); got != tc.want {
				t.Errorf("stripImageTag(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDefaultDataPlaneRepo(t *testing.T) {
	cases := []struct {
		name          string
		operatorImage string
		component     string
		want          string
	}{
		{
			name:          "derives from operator image, drops tag",
			operatorImage: operatorImage,
			component:     componentSlapd,
			want:          "ghcr.io/chuck-chuck-chuck-net/slaptain/slapd",
		},
		{
			name:          "init component",
			operatorImage: operatorImage,
			component:     "slapd-init",
			want:          "ghcr.io/chuck-chuck-chuck-net/slaptain/slapd-init",
		},
		{
			name:          "private mirror follows the operator",
			operatorImage: "registry.example:5000/mirror/slaptain/operator:v2",
			component:     componentSlapd,
			want:          "registry.example:5000/mirror/slaptain/slapd",
		},
		{
			name:          "unset falls back to canonical upstream",
			operatorImage: "",
			component:     componentSlapd,
			want:          canonicalRegistryPath + "/slapd",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := defaultDataPlaneRepo(tc.operatorImage, tc.component); got != tc.want {
				t.Errorf("defaultDataPlaneRepo(%q, %q) = %q, want %q", tc.operatorImage, tc.component, got, tc.want)
			}
		})
	}
}

func TestResolveImageRef(t *testing.T) {
	const defaultRepo = "ghcr.io/chuck-chuck-chuck-net/slaptain/slapd"
	cases := []struct {
		name       string
		img        ldapv1alpha1.SlapdImageConfig
		defaultTag string
		want       string
	}{
		{
			name:       "both unset uses defaults",
			img:        ldapv1alpha1.SlapdImageConfig{},
			defaultTag: operatorTag,
			want:       "ghcr.io/chuck-chuck-chuck-net/slaptain/slapd:v0.0.1",
		},
		{
			name:       "empty tag with empty default falls back to latest",
			img:        ldapv1alpha1.SlapdImageConfig{},
			defaultTag: "",
			want:       "ghcr.io/chuck-chuck-chuck-net/slaptain/slapd:latest",
		},
		{
			name:       "explicit repository overrides default",
			img:        ldapv1alpha1.SlapdImageConfig{Repository: "example.com/custom/slapd"},
			defaultTag: operatorTag,
			want:       "example.com/custom/slapd:v0.0.1",
		},
		{
			name:       "explicit tag overrides default",
			img:        ldapv1alpha1.SlapdImageConfig{Tag: "v9.9.9"},
			defaultTag: operatorTag,
			want:       "ghcr.io/chuck-chuck-chuck-net/slaptain/slapd:v9.9.9",
		},
		{
			name:       "fully pinned",
			img:        ldapv1alpha1.SlapdImageConfig{Repository: "example.com/custom/slapd", Tag: "dev"},
			defaultTag: operatorTag,
			want:       "example.com/custom/slapd:dev",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveImageRef(tc.img, defaultRepo, tc.defaultTag); got != tc.want {
				t.Errorf("resolveImageRef(%+v, %q, %q) = %q, want %q", tc.img, defaultRepo, tc.defaultTag, got, tc.want)
			}
		})
	}
}
