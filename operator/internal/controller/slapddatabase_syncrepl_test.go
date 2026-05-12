/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// extractRIDs returns the set of rid=NNN values from a list of syncrepl stanzas.
func extractRIDs(stanzas []string) []string {
	re := regexp.MustCompile(`rid=(\d+)`)
	out := make([]string, 0, len(stanzas))
	for _, s := range stanzas {
		if m := re.FindStringSubmatch(s); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

// extractProviderURIs returns the provider= URIs in stanza order.
func extractProviderURIs(stanzas []string) []string {
	re := regexp.MustCompile(`provider=(\S+)`)
	out := make([]string, 0, len(stanzas))
	for _, s := range stanzas {
		if m := re.FindStringSubmatch(s); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

// filterExternal returns stanzas whose RID >= ridBase+50+1 (external peer range).
// In-cluster stanzas use peerHost = "<cluster>-N..."; external stanzas use the
// provider URIs we synthesised, so we filter on those.
func filterExternal(stanzas []string, externalURIPrefix string) []string {
	out := make([]string, 0, len(stanzas))
	for _, s := range stanzas {
		if strings.Contains(s, externalURIPrefix) {
			out = append(out, s)
		}
	}
	return out
}

func TestBuildDatabaseSyncRepl_DiagonalFanout(t *testing.T) {
	const (
		clusterName = "slapd"
		headlessSvc = "slapd-headless"
		namespace   = "ns"
		suffix      = "dc=ex,dc=com"
		replicas    = int32(3)
		replPW      = "pw"
		ridBase     = int32(0)
		retry       = "10 +"
		keepalive   = ""
		deltaSync   = true
	)

	// 3 remote pods, identifiable by the IP fragment.
	remoteIPs := []string{"10.0.0.10", "10.0.0.11", "10.0.0.12"}
	mkPeer := func(rpp int32) []resolvedExternalPeer {
		uris := make([]string, len(remoteIPs))
		for i, ip := range remoteIPs {
			uris[i] = fmt.Sprintf("ldaps://%s:1025", ip)
		}
		return []resolvedExternalPeer{{
			Name:            "site-b",
			URIs:            uris,
			ReplicasPerPeer: rpp,
			BindDN:          "cn=replication," + suffix,
			Password:        "epw",
		}}
	}

	build := func(ordinal int32, peers []resolvedExternalPeer) []string {
		return buildDatabaseSyncRepl(
			clusterName, headlessSvc, namespace, suffix,
			replicas, ordinal, replPW,
			true, ridBase, retry, keepalive, deltaSync,
			peers, nil, false,
		)
	}

	t.Run("rpp=1 yields 1:1 diagonal", func(t *testing.T) {
		peers := mkPeer(1)
		// Each local pod selects exactly one remote URI: URIs[ordinal % 3].
		want := map[int32]string{
			0: "10.0.0.10",
			1: "10.0.0.11",
			2: "10.0.0.12",
		}
		for ord, ip := range want {
			ext := filterExternal(build(ord, peers), "10.0.0.")
			if len(ext) != 1 {
				t.Fatalf("ordinal %d: got %d external stanzas, want 1: %v", ord, len(ext), ext)
			}
			if !strings.Contains(ext[0], ip) {
				t.Fatalf("ordinal %d: expected URI containing %s, got %q", ord, ip, ext[0])
			}
		}
	})

	t.Run("rpp=N degrades to full mesh", func(t *testing.T) {
		peers := mkPeer(int32(len(remoteIPs)))
		for ord := int32(0); ord < replicas; ord++ {
			ext := filterExternal(build(ord, peers), "10.0.0.")
			if len(ext) != len(remoteIPs) {
				t.Fatalf("ordinal %d: got %d external stanzas, want %d", ord, len(ext), len(remoteIPs))
			}
			seen := map[string]bool{}
			for _, ip := range remoteIPs {
				for _, st := range ext {
					if strings.Contains(st, ip) {
						seen[ip] = true
					}
				}
			}
			if len(seen) != len(remoteIPs) {
				t.Fatalf("ordinal %d: full mesh missing IPs, seen=%v stanzas=%v", ord, seen, ext)
			}
		}
	})

	t.Run("rpp>N silently caps at N", func(t *testing.T) {
		peers := mkPeer(int32(len(remoteIPs)) + 5)
		ext := filterExternal(build(0, peers), "10.0.0.")
		if len(ext) != len(remoteIPs) {
			t.Fatalf("got %d external stanzas, want %d (cap)", len(ext), len(remoteIPs))
		}
	})

	t.Run("rpp=2 picks (i+0, i+1) mod N per local pod", func(t *testing.T) {
		peers := mkPeer(2)
		want := map[int32][]string{
			0: {"10.0.0.10", "10.0.0.11"},
			1: {"10.0.0.11", "10.0.0.12"},
			2: {"10.0.0.12", "10.0.0.10"},
		}
		for ord, ips := range want {
			uris := extractProviderURIs(filterExternal(build(ord, peers), "10.0.0."))
			if len(uris) != len(ips) {
				t.Fatalf("ordinal %d: got %d uris, want %d: %v", ord, len(uris), len(ips), uris)
			}
			for k, ip := range ips {
				if !strings.Contains(uris[k], ip) {
					t.Fatalf("ordinal %d connection %d: want %s, got %s", ord, k, ip, uris[k])
				}
			}
		}
	})

	t.Run("RIDs unique within each pod's stanzas", func(t *testing.T) {
		peers := mkPeer(2)
		for ord := int32(0); ord < replicas; ord++ {
			rids := extractRIDs(build(ord, peers))
			seen := map[string]bool{}
			for _, r := range rids {
				if seen[r] {
					t.Fatalf("ordinal %d: duplicate RID %s in %v", ord, r, rids)
				}
				seen[r] = true
			}
		}
	})

	t.Run("URI-mode peer unaffected by ReplicasPerPeer", func(t *testing.T) {
		peers := []resolvedExternalPeer{{
			Name:            "site-uri",
			URIs:            []string{"ldaps://remote.example.com:636"},
			ReplicasPerPeer: 1,
			BindDN:          "cn=repl," + suffix,
			Password:        "pw",
		}}
		ext := filterExternal(build(0, peers), "remote.example.com")
		if len(ext) != 1 {
			t.Fatalf("URI-mode: got %d external stanzas, want 1", len(ext))
		}
		// IP-detection path must NOT mark the DNS URI as IP-based.
		if strings.Contains(ext[0], "tls_reqcert=allow") {
			t.Fatalf("URI-mode DNS peer should not get tls_reqcert=allow: %s", ext[0])
		}
	})

	t.Run("consumer-only suppresses in-cluster stanzas, keeps external", func(t *testing.T) {
		// In consumer-only mode (ADR-010), each pod independently consumes from
		// externalPeers — there is no in-cluster mesh.
		peers := []resolvedExternalPeer{{
			Name:            "legacy-prod",
			URIs:            []string{"ldaps://10.1.1.20:636"},
			ReplicasPerPeer: 1,
			BindDN:          "cn=syncuser,ou=config,o=example",
			Password:        "pw",
			PlainSyncRepl:   true,
		}}
		stanzas := buildDatabaseSyncRepl(
			clusterName, headlessSvc, namespace, suffix,
			replicas, 0, replPW,
			true, ridBase, retry, keepalive, deltaSync,
			peers, nil, true, // consumerOnly=true
		)
		// No in-cluster peers means no slapd-1 or slapd-2 references.
		for _, s := range stanzas {
			if strings.Contains(s, headlessSvc) {
				t.Fatalf("consumer-only emitted in-cluster stanza: %s", s)
			}
		}
		// External peer stanza is present.
		if len(filterExternal(stanzas, "10.1.1.20")) != 1 {
			t.Fatalf("consumer-only: external stanza missing in %v", stanzas)
		}
		// Plain-syncrepl peer must NOT carry delta opts.
		for _, s := range stanzas {
			if strings.Contains(s, "syncdata=accesslog") || strings.Contains(s, "logbase") {
				t.Fatalf("plain-syncrepl peer carries delta opts: %s", s)
			}
		}
	})
}
