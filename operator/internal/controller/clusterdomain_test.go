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
	"strings"
	"testing"
)

func TestParseClusterDomainFromResolvConf(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "non-default domain",
			in:   "search slaptain-testing.svc.k8s.example svc.k8s.example k8s.example\nnameserver 10.234.0.10\noptions ndots:5\n",
			want: "k8s.example",
		},
		{
			name: "default cluster.local",
			in:   "search default.svc.cluster.local svc.cluster.local cluster.local\nnameserver 10.96.0.10\noptions ndots:5\n",
			want: "cluster.local",
		},
		{
			name: "only ns.svc.<domain> present (fallback path)",
			in:   "search myns.svc.example.corp\nnameserver 10.0.0.10\n",
			want: "example.corp",
		},
		{
			name: "no svc entry — laptop resolv.conf",
			in:   "search example.com\nnameserver 1.1.1.1\n",
			want: "",
		},
		{
			name: "no search line",
			in:   "nameserver 1.1.1.1\n",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseClusterDomainFromResolvConf(strings.NewReader(tc.in))
			if got != tc.want {
				t.Fatalf("parseClusterDomainFromResolvConf() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveClusterDomain_EnvOverride(t *testing.T) {
	t.Setenv("CLUSTER_DOMAIN", "override.internal")
	if got := ResolveClusterDomain(); got != "override.internal" {
		t.Fatalf("ResolveClusterDomain() with env override = %q, want %q", got, "override.internal")
	}
}

func TestResolveClusterDomain_FallbackDefault(t *testing.T) {
	t.Setenv("CLUSTER_DOMAIN", "")
	orig := resolvConfPath
	resolvConfPath = "/nonexistent/resolv.conf"
	defer func() { resolvConfPath = orig }()
	if got := ResolveClusterDomain(); got != DefaultClusterDomain {
		t.Fatalf("ResolveClusterDomain() fallback = %q, want %q", got, DefaultClusterDomain)
	}
}
