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
	"bufio"
	"io"
	"os"
	"strings"
)

// DefaultClusterDomain is the Kubernetes DNS domain assumed when neither the
// CLUSTER_DOMAIN override nor /etc/resolv.conf yields one. Nearly every cluster
// uses this.
const DefaultClusterDomain = "cluster.local"

// resolvConfPath is a package var so tests can point discovery at a fixture.
var resolvConfPath = "/etc/resolv.conf"

// ResolveClusterDomain determines the cluster's DNS domain (e.g. "cluster.local"
// or "k8s.example"). Every pod FQDN slaptain builds — serverID URLs,
// syncrepl provider URIs, and the operator's own per-pod LDAP connections —
// must use the real domain, or they resolve to NXDOMAIN and fail TLS hostname
// verification on clusters that don't use the default. See ADR-015.
//
// Precedence:
//  1. CLUSTER_DOMAIN env (explicit override; e.g. `make run` off-cluster, or
//     non-standard resolver setups).
//  2. The `svc.<domain>` entry in the operator pod's /etc/resolv.conf search
//     list — kubelet injects this into every container regardless of base image.
//  3. DefaultClusterDomain ("cluster.local").
func ResolveClusterDomain() string {
	if v := strings.TrimSpace(os.Getenv("CLUSTER_DOMAIN")); v != "" {
		return v
	}
	f, err := os.Open(resolvConfPath)
	if err == nil {
		defer f.Close()
		if d := parseClusterDomainFromResolvConf(f); d != "" {
			return d
		}
	}
	return DefaultClusterDomain
}

// parseClusterDomainFromResolvConf extracts the cluster domain from a
// resolv.conf search list by finding the `svc.<domain>` token and returning
// <domain>. For the search line
//
//	search myns.svc.k8s.example svc.k8s.example k8s.example
//
// it returns "k8s.example". Returns "" if no such token is present.
// Mirrors the discovery in tests/gencert.sh so cert SANs and operator FQDNs
// agree on the domain.
func parseClusterDomainFromResolvConf(r io.Reader) string {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || fields[0] != "search" {
			continue
		}
		for _, tok := range fields[1:] {
			// Prefer the bare `svc.<domain>` entry over `<ns>.svc.<domain>`.
			if d, ok := strings.CutPrefix(tok, "svc."); ok {
				return d
			}
		}
		// Fallback: derive from a `<ns>.svc.<domain>` entry if present.
		for _, tok := range fields[1:] {
			if _, d, ok := strings.Cut(tok, ".svc."); ok {
				return d
			}
		}
	}
	return ""
}
