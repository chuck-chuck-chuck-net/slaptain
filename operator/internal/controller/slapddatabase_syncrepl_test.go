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

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
	"github.com/chuck-chuck-chuck-net/slaptain/operator/internal/ridcoord"
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
		clusterName   = "slapd"
		headlessSvc   = "slapd-headless"
		namespace     = "ns"
		clusterDomain = "cluster.local"
		suffix        = "dc=ex,dc=com"
		dbName        = "default"
		replicas      = int32(3)
		replPW        = "pw"
		ridSegment    = int32(0) // was ridBase 0; ridSegment = ridBase / 100
		retry         = "10 +"
		keepalive     = ""
		deltaSync     = true
	)

	// 3 remote pods, identifiable by the IP fragment.
	remoteIPs := []string{"10.0.0.10", "10.0.0.11", "10.0.0.12"}
	mkPeer := func(rpp int32) []resolvedExternalPeer {
		uris := make([]string, len(remoteIPs))
		for i, ip := range remoteIPs {
			uris[i] = fmt.Sprintf("ldaps://%s:1025", ip)
		}
		extRID, _ := externalStanzaRID(ridcoord.DefaultLayout(), ridSegment, 0)
		return []resolvedExternalPeer{{
			Name:            "site-b",
			RID:             extRID,
			URIs:            uris,
			ReplicasPerPeer: rpp,
			BindDN:          "cn=replication," + suffix,
			Password:        "epw",
		}}
	}

	build := func(ordinal int32, peers []resolvedExternalPeer) []string {
		rids, _ := inClusterStanzaRIDs(ridcoord.DefaultLayout(), ridSegment, replicas, &ordinal)
		stanzas, err := buildDatabaseSyncRepl(
			clusterName, headlessSvc, namespace, clusterDomain, suffix, dbName,
			replicas, ordinal, replPW,
			true, rids, retry, keepalive, deltaSync,
			ldapv1alpha1.AccesslogSuffix(dbName),
			peers, nil, false,
		)
		if err != nil {
			t.Fatalf("builder refused: %v", err)
		}
		return stanzas
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

	// Three subtests stood here and were retired by ADR-031 M4a:
	//
	//   "rpp=N degrades to full mesh"
	//   "rpp>N silently caps at N"
	//   "rpp=2 picks (i+0, i+1) mod N per local pod"
	//
	// All three pinned MORE THAN ONE syncrepl link from one local pod to one
	// peer site. ADR-031 decision 9 abolishes that shape: the remote block of a
	// database's segment is S slots wide, one per peer site, so a second link
	// from a pod to a peer has no slot of its own and would have to share the
	// first one's RID — which slapd loads in silence and then runs on a shared
	// connid. The operator now refuses replicasPerPeer >= 2 outright
	// (judgeExternalPeers, reasonReplicasPerPeerUnsupported).
	//
	// They are retired rather than repaired because no implementation can
	// satisfy them AND their own sibling below, "RIDs unique within each pod's
	// stanzas" — which is as clean a proof as one gets that they describe a
	// scheme that no longer exists.
	//
	// The redundancy they were written for is not cancelled, only moved: ADR-032
	// brings the quantity back as a site-pair R, spread across DISTINCT local
	// pods, which needs no second slot. Whoever implements that will want
	// fixtures of this shape again — mkPeer, filterExternal and
	// extractProviderURIs are all still here — but written per site pair, not
	// per pod.

	// Pointed at a single-link peer since M4a: one link per peer site is the
	// only topology ADR-031 decision 9 admits, and uniqueness is exactly what a
	// second link to the same site would break.
	t.Run("RIDs unique within each pod's stanzas", func(t *testing.T) {
		peers := mkPeer(1)
		for ord := range replicas {
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
		uriRID, _ := externalStanzaRID(ridcoord.DefaultLayout(), ridSegment, 0)
		peers := []resolvedExternalPeer{{
			Name:            "site-uri",
			RID:             uriRID,
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
		legacyRID, _ := externalStanzaRID(ridcoord.DefaultLayout(), ridSegment, 0)
		peers := []resolvedExternalPeer{{
			Name:            "legacy-prod",
			RID:             legacyRID,
			URIs:            []string{"ldaps://10.1.1.20:636"},
			ReplicasPerPeer: 1,
			BindDN:          "cn=syncuser,ou=config,o=example",
			Password:        "pw",
			PlainSyncRepl:   true,
		}}
		stanzas, err := buildDatabaseSyncRepl(
			clusterName, headlessSvc, namespace, clusterDomain, suffix, dbName,
			replicas, 0, replPW,
			true, nil, retry, keepalive, deltaSync,
			ldapv1alpha1.AccesslogSuffix(dbName),
			peers, nil, true, // consumerOnly=true
		)
		if err != nil {
			t.Fatalf("builder refused: %v", err)
		}
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

// ── ADR-019: per-database accesslog derivation ───────────────────────────────
//
// The naming helpers themselves (AccesslogSuffix / AccesslogDir /
// ExternalLogBase) are unit-tested in api/v1alpha1, which owns them; what is
// asserted here is that the stanza builders actually consume them.

func TestBuildDatabaseSyncRepl_PerDatabaseLogbase(t *testing.T) {
	const (
		clusterName   = "slapd"
		headlessSvc   = "slapd-headless"
		namespace     = "ns"
		clusterDomain = "cluster.local"
		suffix        = "dc=ex,dc=com"
		dbName        = "secondary"
		replicas      = int32(3)
		replPW        = "pw"
		ridSegment    = int32(2) // was ridBase 200
		retry         = "10 +"
		keepalive     = ""
	)
	layout := ridcoord.DefaultLayout()
	rwRIDs, _ := inClusterStanzaRIDs(layout, ridSegment, replicas, i32(0))
	roRIDs, _ := inClusterStanzaRIDs(layout, ridSegment, replicas, nil)
	extRID, _ := externalStanzaRID(layout, ridSegment, 0)

	wantLogbase := `logbase="cn=accesslog-secondary"`

	t.Run("in-cluster stanzas carry the per-database logbase", func(t *testing.T) {
		sd := &ldapv1alpha1.SlapdDatabase{}
		sd.Name = dbName
		stanzas, err := buildDatabaseSyncRepl(
			clusterName, headlessSvc, namespace, clusterDomain, suffix, dbName,
			replicas, 0, replPW,
			true, rwRIDs, retry, keepalive, true,
			ldapv1alpha1.ExternalLogBase(sd),
			nil, nil, false,
		)
		if err != nil {
			t.Fatalf("builder refused: %v", err)
		}
		if len(stanzas) == 0 {
			t.Fatalf("no stanzas emitted")
		}
		for _, s := range stanzas {
			if !strings.Contains(s, wantLogbase) {
				t.Errorf("in-cluster stanza missing %s: %s", wantLogbase, s)
			}
		}
	})

	t.Run("RO stanzas carry the same per-database logbase", func(t *testing.T) {
		stanzas, err := buildDatabaseSyncReplRO(
			clusterName, headlessSvc, namespace, clusterDomain, suffix, dbName,
			replicas, replPW,
			true, roRIDs, retry, keepalive, true,
			nil,
		)
		if err != nil {
			t.Fatalf("builder refused: %v", err)
		}
		if len(stanzas) == 0 {
			t.Fatalf("no stanzas emitted")
		}
		for _, s := range stanzas {
			if !strings.Contains(s, wantLogbase) {
				t.Errorf("RO stanza missing %s: %s", wantLogbase, s)
			}
		}
	})

	mkExternal := func(plain bool) []resolvedExternalPeer {
		return []resolvedExternalPeer{{
			Name:            "site-b",
			RID:             extRID,
			URIs:            []string{"ldaps://10.9.9.9:1025"},
			ReplicasPerPeer: 1,
			BindDN:          "cn=replication," + suffix,
			Password:        "epw",
			PlainSyncRepl:   plain,
		}}
	}
	buildExt := func(sd *ldapv1alpha1.SlapdDatabase, plain bool) []string {
		stanzas, err := buildDatabaseSyncRepl(
			clusterName, headlessSvc, namespace, clusterDomain, suffix, dbName,
			replicas, 0, replPW,
			true, rwRIDs, retry, keepalive, true,
			ldapv1alpha1.ExternalLogBase(sd),
			mkExternal(plain), nil, false,
		)
		if err != nil {
			t.Fatalf("builder refused: %v", err)
		}
		return filterExternal(stanzas, "10.9.9.9")
	}

	t.Run("delta external peer without override uses the derived suffix", func(t *testing.T) {
		sd := &ldapv1alpha1.SlapdDatabase{}
		sd.Name = dbName
		ext := buildExt(sd, false)
		if len(ext) != 1 {
			t.Fatalf("got %d external stanzas, want 1", len(ext))
		}
		if !strings.Contains(ext[0], wantLogbase) {
			t.Errorf("external stanza missing %s: %s", wantLogbase, ext[0])
		}
	})

	t.Run("delta external peer honours externalAccesslogSuffix (ADR-019 R9)", func(t *testing.T) {
		sd := &ldapv1alpha1.SlapdDatabase{}
		sd.Name = dbName
		sd.Spec.Replication.ExternalAccesslogSuffix = "cn=log"
		ext := buildExt(sd, false)
		if len(ext) != 1 {
			t.Fatalf("got %d external stanzas, want 1", len(ext))
		}
		if !strings.Contains(ext[0], `logbase="cn=log"`) {
			t.Errorf(`external stanza missing logbase="cn=log": %s`, ext[0])
		}
		if strings.Contains(ext[0], "accesslog-secondary") {
			t.Errorf("override ignored, local suffix leaked: %s", ext[0])
		}
	})

	t.Run("plain external peer emits no logbase at all (ADR-011)", func(t *testing.T) {
		sd := &ldapv1alpha1.SlapdDatabase{}
		sd.Name = dbName
		ext := buildExt(sd, true)
		if len(ext) != 1 {
			t.Fatalf("got %d external stanzas, want 1", len(ext))
		}
		if strings.Contains(ext[0], "logbase") || strings.Contains(ext[0], "syncdata=accesslog") {
			t.Errorf("plain-syncrepl peer carries delta opts: %s", ext[0])
		}
	})
}

func TestAccesslogACL(t *testing.T) {
	// ADR-020 R1: exactly one rule, granting read to the journalled database's
	// replication bind identities and nothing to anyone else, in the same
	// dn.exact form applyACLs uses. Since ADR-027's cutover that is TWO
	// identities: the node-local cn=repl-<db>,cn=slaptain-auth that stanzas now
	// bind as, and the legacy cn=replication,<data suffix> kept so a
	// not-yet-upgraded peer keeps replicating through the migration window.
	got := accesslogACL("exdb", "dc=ex,dc=com")
	want := `to * by dn.exact="cn=repl-exdb,cn=slaptain-auth" read ` +
		`by dn.exact="cn=replication,dc=ex,dc=com" read by * none`
	if got != want {
		t.Fatalf("accesslogACL = %q, want %q", got, want)
	}
	// ADR-020 R3: no explicit rootDN grant — rootDN bypasses ACLs.
	if strings.Contains(got, "cn=admin") {
		t.Errorf("ACL restates a rootDN bypass (ADR-020 R3): %s", got)
	}
	// The ACL must name the data suffix, never the log's own suffix.
	if strings.Contains(got, "accesslog") {
		t.Errorf("ACL names the log suffix instead of the data suffix: %s", got)
	}
}

// A syncrepl stanza that carries no network-timeout and no keepalive notices a
// half-open provider only when TCP gives up, which on Linux defaults to a
// quarter of an hour of a consumer silently not consuming (production-config
// review, finding 11).
//
// The keepalive triple is asserted at its source — desiredKeepalive, in
// scale_tunables_test.go — because the builder takes it as a parameter; what is
// asserted here is that every stanza the builder emits, of every kind, carries
// the network-timeout and passes a keepalive through — and that neither
// `timeout=` nor `timelimit=` ever appears (see the negative assertions below
// for why each is forbidden).
func TestBuildDatabaseSyncRepl_StanzaTimeouts(t *testing.T) {
	const (
		clusterName   = "slapd"
		headlessSvc   = "slapd-headless"
		namespace     = "ns"
		clusterDomain = "cluster.local"
		suffix        = "dc=ex,dc=com"
		dbName        = "primary"
		replicas      = int32(3)
		replPW        = "pw"
		ridSegment    = int32(1) // was ridBase 100
		retry         = "10 +"
	)
	keepalive := defaultKeepalive
	layout := ridcoord.DefaultLayout()
	rwRIDs, _ := inClusterStanzaRIDs(layout, ridSegment, replicas, i32(0))
	roRIDs, _ := inClusterStanzaRIDs(layout, ridSegment, replicas, nil)
	extRID, _ := externalStanzaRID(layout, ridSegment, 0)

	sd := &ldapv1alpha1.SlapdDatabase{}
	sd.Name = dbName
	external := []resolvedExternalPeer{{
		Name:            "site-b",
		RID:             extRID,
		URIs:            []string{"ldaps://10.9.9.9:1025"},
		ReplicasPerPeer: 1,
		BindDN:          "cn=replication," + suffix,
		Password:        "epw",
	}}

	rwStanzas, rwErr := buildDatabaseSyncRepl(
		clusterName, headlessSvc, namespace, clusterDomain, suffix, dbName,
		replicas, 0, replPW,
		true, rwRIDs, retry, keepalive, true,
		ldapv1alpha1.ExternalLogBase(sd),
		external, nil, false,
	)
	roStanzas, roErr := buildDatabaseSyncReplRO(
		clusterName, headlessSvc, namespace, clusterDomain, suffix, dbName,
		replicas, replPW,
		true, roRIDs, retry, keepalive, true,
		nil,
	)
	if rwErr != nil || roErr != nil {
		t.Fatalf("builder refused: rw=%v ro=%v", rwErr, roErr)
	}
	groups := map[string][]string{
		"RW (in-cluster + external)": rwStanzas,
		"read-only replica":          roStanzas,
	}

	for what, stanzas := range groups {
		if len(stanzas) == 0 {
			t.Fatalf("%s: no stanzas emitted", what)
		}
		for _, s := range stanzas {
			for _, want := range []string{
				"network-timeout=10",
				"keepalive=" + defaultKeepalive,
			} {
				if !strings.Contains(s, want) {
					t.Errorf("%s stanza missing %q:\n  %s", what, want, s)
				}
			}
			// timeout= (LDAP_OPT_TIMEOUT) must NEVER appear. In the refresh
			// phase do_syncrep2 uses it as a BLOCKING ldap_result wait on a
			// threadpool thread (syncrepl.c:1362, OpenLDAP 2.7.1), and slapd's
			// task can only honour a cn=config pause between messages
			// (syncrepl.c:2128) — so every external config MOD, which pauses
			// the whole pool (bconfig.c:6512), froze the entire server for up
			// to the timeout while any consumer was refreshing. Measured live:
			// three ~300s total-silence freezes per convergence pass. Expiry
			// detects nothing (rc 0 → SYNC_TIMEOUT → "listen for more"), so
			// the wait bought no protection either. Unset means sb_timeout_api
			// = 0 → non-blocking poll, the pre-regression behaviour.
			// The leading space keeps this from matching network-timeout=.
			if strings.Contains(s, " timeout=") {
				t.Errorf("%s stanza carries timeout=, which turns every "+
					"cn=config write into a server-wide freeze while a "+
					"consumer is in refresh phase:\n  %s", what, s)
			}
			// timelimit= maps to LDAP_OPT_TIMELIMIT, a server-side search time
			// limit that would abort a refreshAndPersist connection. It must
			// never appear, however tempting the name looks next to timeout=.
			if strings.Contains(s, "timelimit=") {
				t.Errorf("%s stanza carries timelimit=, which would kill the "+
					"persistent search:\n  %s", what, s)
			}
		}
	}
}
