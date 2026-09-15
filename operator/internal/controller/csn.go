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
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"
)

// queryContextCSN connects to an LDAP server and reads the contextCSN attribute
// from the data suffix root entry. When bindDNs and bindPW are non-empty, binds
// first (required when ACLs deny anonymous access to the suffix). Otherwise
// uses an anonymous connection.
// Returns the raw CSN strings (one per serverID that has written to this replica).
func queryContextCSN(host string, port int32, tlsEnabled bool, suffix string, bindDNs []string, bindPW string) ([]string, error) {
	scheme := "ldap"
	if tlsEnabled {
		scheme = "ldaps"
	}
	uri := fmt.Sprintf("%s://%s:%d", scheme, host, port)
	return doCSNQuery(uri, tlsEnabled, suffix, bindDNs, bindPW)
}

// queryContextCSNFromURI connects to an LDAP URI (ldap:// or ldaps://) and reads contextCSN.
// Used for URI-mode external peers where the full URI is already available.
func queryContextCSNFromURI(uri, suffix string, bindDNs []string, bindPW string) ([]string, error) {
	useTLS := strings.HasPrefix(uri, "ldaps://")
	return doCSNQuery(uri, useTLS, suffix, bindDNs, bindPW)
}

// doCSNQuery is the shared implementation for contextCSN queries.
//
// bindDNs are candidate identities tried in order (csnBindDNs): the node-local
// replication identity first, the legacy cn=replication,<suffix> as a fallback.
// The fallback is not belt-and-braces — the same query runs against CROSS-SITE
// peers, and a peer still running a pre-ADR-027 operator has no node-local
// entry to bind as, so without it every such peer reads Unreachable for the
// whole migration window. A rejected bind is the only thing that advances to
// the next candidate; a dial or search failure is returned as-is, because
// retrying a different DN against a server that never answered proves nothing.
func doCSNQuery(uri string, useTLS bool, suffix string, bindDNs []string, bindPW string) ([]string, error) {
	dialOpts := []ldap.DialOpt{
		ldap.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}),
	}
	if useTLS {
		dialOpts = append(dialOpts, ldap.DialWithTLSConfig(&tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // IP-based Multus addresses, CA trust only (ADR-007 §7)
		}))
	}

	conn, err := ldap.DialURL(uri, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", uri, err)
	}
	defer conn.Close()
	conn.SetTimeout(ldapRequestTimeout)

	if bindPW != "" {
		// Report EVERY candidate that was tried, not just the last one to fail.
		//
		// A message naming only the final candidate points a reader at the wrong
		// credential during exactly the window when someone is most likely to be
		// reading it: the migration window, where "bind cn=replication,… err=49"
		// looks like a legacy-credential problem when the real cause is that the
		// remote pod's node-local identity has not been written yet. That cost
		// real debugging time during the mesh validation, and it is the same
		// misleading-evidence class the csn-convergence verdicts were fixed for.
		var bindErrs []error
		bound := false
		for _, dn := range bindDNs {
			if dn == "" {
				continue
			}
			err := conn.Bind(dn, bindPW)
			if err == nil {
				bound = true
				break
			}
			bindErrs = append(bindErrs, fmt.Errorf("as %s: %w", dn, err))
		}
		if !bound && len(bindErrs) > 0 {
			return nil, fmt.Errorf("bind on %s failed for all %d candidate identities "+
				"(node-local first, legacy second — ADR-027): %w",
				uri, len(bindErrs), errors.Join(bindErrs...))
		}
	}

	result, err := conn.Search(ldap.NewSearchRequest(
		suffix, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 5, false,
		"(objectClass=*)", []string{"contextCSN"}, nil,
	))
	if err != nil {
		return nil, fmt.Errorf("search contextCSN on %s: %w", uri, err)
	}
	if len(result.Entries) == 0 {
		return nil, nil
	}
	return result.Entries[0].GetEqualFoldAttributeValues("contextCSN"), nil
}

// parseCSNTime extracts the timestamp from a contextCSN value.
// Format: YYYYMMDDHHMMSS.µsZ#count#serverID#modcount
func parseCSNTime(csn string) (time.Time, error) {
	parts := strings.SplitN(csn, "#", 2)
	if len(parts) == 0 {
		return time.Time{}, fmt.Errorf("invalid CSN: %s", csn)
	}
	ts := strings.TrimSuffix(parts[0], "Z")
	return time.Parse("20060102150405.000000", ts)
}

// newestCSN finds the CSN with the latest timestamp from a list of CSN strings.
// Returns the parsed time and the raw CSN string. Returns zero time if the list is empty.
func newestCSN(csns []string) (time.Time, string, error) {
	var newest time.Time
	var newestRaw string
	for _, csn := range csns {
		t, err := parseCSNTime(csn)
		if err != nil {
			continue
		}
		if t.After(newest) {
			newest = t
			newestRaw = csn
		}
	}
	if newestRaw == "" && len(csns) > 0 {
		return time.Time{}, "", fmt.Errorf("no parseable CSN in %v", csns)
	}
	return newest, newestRaw, nil
}

// csnConverged checks if all CSN sets are identical (same CSN vectors).
// Returns true when all sets match or when fewer than 2 sets are provided.
func csnConverged(csnSets [][]string) bool {
	if len(csnSets) < 2 {
		return true
	}
	ref := normalizeCSNSet(csnSets[0])
	for _, set := range csnSets[1:] {
		if normalizeCSNSet(set) != ref {
			return false
		}
	}
	return true
}

// normalizeCSNSet sorts and joins CSN strings for comparison.
func normalizeCSNSet(csns []string) string {
	sorted := make([]string, len(csns))
	copy(sorted, csns)
	sort.Strings(sorted)
	return strings.Join(sorted, "|")
}
