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

package slapdexporter

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-ldap/ldap/v3"
	"github.com/prometheus/client_golang/prometheus"
)

// Fixture values more than one test spells. socketURI is spelled out rather
// than taken from DefaultURI so a test pinning the default does not compare the
// constant with itself; wantDirectionLabel likewise for the label-set test.
const (
	remoteURI          = "ldap://ldap.example.org:1024"
	socketURI          = "ldapi:///run/openldap/slapd.ldapi"
	monitorBindDN      = "cn=monitor,dc=example,dc=org"
	flagBindSimple     = "--bind=simple"
	flagBindDN         = "--bind-dn=" + monitorBindDN
	testPassword       = "s3cret"
	caseDialFails      = "dial fails"
	caseBindFails      = "bind fails"
	caseSearchFails    = "search fails"
	typeGauge          = "GAUGE"
	typeCounter        = "COUNTER"
	wantDirectionLabel = "direction"
)

// ─── Fixtures ───────────────────────────────────────────────────────────────
//
// testdata/ holds REAL cn=Monitor output, recorded by testdata/record.sh from a
// throwaway slapd in the slapd-init image — the vendored 2.7.1 build
// (2026-10-08) and the -ol26 image's Debian 2.6.10 (2026-10-11) — bound by
// SASL EXTERNAL over ldapi under the ADR-033 constant monitor ACL, exactly the
// sidecar's view. Each file carries its provenance as an LDIF comment header.

const (
	fixtureMonitor27 = "monitor-2.7.1.ldif"
	fixtureMonitor26 = "monitor-2.6.10.ldif"
	// fixtureMonitorFiltered27 is slapd's answer to the collector's own filter.
	fixtureMonitorFiltered27 = "monitor-filtered-2.7.1.ldif"

	// The two mdb databases of both full fixtures. statsSuffix was ADDed after
	// the monitor database and carries olmMDB*; noStatsSuffix was opened before
	// it and carries none.
	statsSuffix   = "dc=example,dc=net"
	noStatsSuffix = "dc=example,dc=org"
	statsDBDN     = "cn=Database 3,cn=Databases,cn=Monitor"
	noStatsDBDN   = "cn=Database 1,cn=Databases,cn=Monitor"
)

// loadLDIF parses one of the recorded fixtures. The recordings are unwrapped
// (ldif-wrap=no) and contain no base64 values; both are asserted rather than
// assumed, so a re-recording that breaks either fails here and not as a
// mysterious wrong value later.
func loadLDIF(t *testing.T, name string) []*ldap.Entry {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var (
		entries []*ldap.Entry
		cur     *ldap.Entry
	)
	flush := func() {
		if cur != nil {
			entries = append(entries, cur)
			cur = nil
		}
	}
	for i, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "#"):
			continue
		case line == "":
			flush()
			continue
		case strings.HasPrefix(line, " "):
			t.Fatalf("%s:%d: wrapped LDIF line; record with -o ldif-wrap=no", name, i+1)
		}
		attr, val, ok := strings.Cut(line, ":")
		if !ok {
			t.Fatalf("%s:%d: not an LDIF line: %q", name, i+1, line)
		}
		if strings.HasPrefix(val, ":") || strings.HasPrefix(val, "<") {
			t.Fatalf("%s:%d: base64/URL value not supported by this parser", name, i+1)
		}
		val = strings.TrimPrefix(val, " ")
		if attr == "dn" {
			flush()
			cur = &ldap.Entry{DN: val}
			continue
		}
		if cur == nil {
			t.Fatalf("%s:%d: attribute before dn", name, i+1)
		}
		if a := attrOf(cur, attr); a != nil {
			a.Values = append(a.Values, val)
		} else {
			cur.Attributes = append(cur.Attributes, &ldap.EntryAttribute{Name: attr, Values: []string{val}})
		}
	}
	flush()
	if len(entries) == 0 {
		t.Fatalf("fixture %s has no entries", name)
	}
	return entries
}

func attrOf(e *ldap.Entry, name string) *ldap.EntryAttribute {
	for _, a := range e.Attributes {
		if strings.EqualFold(a.Name, name) {
			return a
		}
	}
	return nil
}

// cloneEntries deep-copies entries so a test can edit one without touching the
// fixture another test reads.
func cloneEntries(in []*ldap.Entry) []*ldap.Entry {
	out := make([]*ldap.Entry, 0, len(in))
	for _, e := range in {
		c := &ldap.Entry{DN: e.DN}
		for _, a := range e.Attributes {
			c.Attributes = append(c.Attributes, &ldap.EntryAttribute{Name: a.Name, Values: slices.Clone(a.Values)})
		}
		out = append(out, c)
	}
	return out
}

func findEntry(t *testing.T, entries []*ldap.Entry, dn string) *ldap.Entry {
	t.Helper()
	for _, e := range entries {
		if e.DN == dn {
			return e
		}
	}
	t.Fatalf("fixture has no entry %q", dn)
	return nil
}

// setAttr replaces (or adds) an attribute; nil values removes it.
func setAttr(e *ldap.Entry, name string, values []string) {
	for i, a := range e.Attributes {
		if strings.EqualFold(a.Name, name) {
			if values == nil {
				e.Attributes = slices.Delete(e.Attributes, i, i+1)
				return
			}
			a.Values = values
			return
		}
	}
	if values != nil {
		e.Attributes = append(e.Attributes, &ldap.EntryAttribute{Name: name, Values: values})
	}
}

// removeMDBStats strips every olmMDB* attribute — the shape of a database
// opened before the monitor database.
func removeMDBStats(e *ldap.Entry) {
	e.Attributes = slices.DeleteFunc(e.Attributes, func(a *ldap.EntryAttribute) bool {
		return strings.HasPrefix(strings.ToLower(a.Name), "olmmdb")
	})
}

// ─── Samples as comparable values ───────────────────────────────────────────

// key renders a sample's identity (name + sorted labels), without its value.
func key(name string, labels map[string]string) string {
	names := make([]string, 0, len(labels))
	for k := range labels {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, k := range names {
		parts = append(parts, fmt.Sprintf("%s=%q", k, labels[k]))
	}
	return name + "{" + strings.Join(parts, ",") + "}"
}

// byKey indexes samples by identity, failing on a duplicate: two samples with
// one identity is a broken exposition, not a detail.
func byKey(t *testing.T, samples []Sample) map[string]float64 {
	t.Helper()
	out := make(map[string]float64, len(samples))
	for _, s := range samples {
		k := key(s.Name, s.Labels)
		if _, dup := out[k]; dup {
			t.Fatalf("duplicate sample %s", k)
		}
		out[k] = s.Value
	}
	return out
}

func filterSamples(samples []Sample, keep func(Sample) bool) []Sample {
	var out []Sample
	for _, s := range samples {
		if keep(s) {
			out = append(out, s)
		}
	}
	return out
}

func suffix(name, suffixValue string) string {
	return key(name, map[string]string{"suffix": suffixValue})
}
func result(name, r string) string { return key(name, map[string]string{"result": r}) }
func op(name, o string) string     { return key(name, map[string]string{"op": o}) }
func bare(name string) string      { return key(name, nil) }

// The families of ADR-033 decision 4, "Curated families". Spelled out here, not
// imported from the package, so a rename in the implementation is a red test
// and not a silent follow.
const (
	famOpsInitiated   = "openldap_operations_initiated_total"
	famOpsCompleted   = "openldap_operations_completed_total"
	famConnsTotal     = "openldap_connections_total"
	famConnsCurrent   = "openldap_connections_current"
	famMaxFDs         = "openldap_max_file_descriptors"
	famSentBytes      = "openldap_sent_bytes_total"
	famSentPDUs       = "openldap_sent_pdus_total"
	famSentEntries    = "openldap_sent_entries_total"
	famSentReferrals  = "openldap_sent_referrals_total"
	famThreadsOpen    = "openldap_threads_open"
	famThreadsActive  = "openldap_threads_active"
	famThreadsPending = "openldap_threads_pending"
	famThreadsStart   = "openldap_threads_starting"
	famThreadsBack    = "openldap_threads_backload"
	famThreadsMax     = "openldap_threads_max"
	famThreadsMaxPend = "openldap_threads_max_pending"
	famWaiters        = "openldap_waiters"
	famStartTime      = "openldap_start_time_seconds"

	// The exporter's own families: its observation of its own attempt.
	famUp    = "openldap_up"
	famDials = "openldap_exporter_dials_total"
	famBinds = "openldap_exporter_binds_total"
)

// mdbFamilies maps each olmMDB* attribute to the family it feeds.
var mdbFamilies = map[string]string{
	"olmMDBPagesMax":    "openldap_mdb_pages_max",
	"olmMDBPagesUsed":   "openldap_mdb_pages_used",
	"olmMDBPagesFree":   "openldap_mdb_pages_free",
	"olmMDBReadersMax":  "openldap_mdb_readers_max",
	"olmMDBReadersUsed": "openldap_mdb_readers_used",
	"olmMDBEntries":     "openldap_mdb_entries",
	"olmMDBPageSize":    "openldap_mdb_page_size_bytes",
}

func isMDBFamily(name string) bool { return strings.HasPrefix(name, "openldap_mdb_") }

// isExporterFamily: the exporter's own series, present whatever the read did.
func isExporterFamily(name string) bool {
	return name == famUp || strings.HasPrefix(name, "openldap_exporter_")
}

// isMonitorFamily: a series derived from cn=Monitor — everything else.
func isMonitorFamily(name string) bool { return !isExporterFamily(name) }

// ─── A fake slapd behind the Conn seam ──────────────────────────────────────

// fakeSlapd stands in for slapd. Tests change its fields between scrapes to
// script a sequence (succeeds, then fails, then recovers...).
type fakeSlapd struct {
	mu sync.Mutex

	entries   []*ldap.Entry // what a search returns
	dialErr   error
	bindErr   error
	searchErr error
	// partial makes a failing search ALSO return entries, as go-ldap does for
	// e.g. a size-limit error.
	partial bool

	// hang names the call that blocks until release is closed (hangDial,
	// hangBind, hangSearch); "" for none.
	hang    string
	release chan struct{}

	dials, closes int
	conns         []*fakeConn
	deadlines     []time.Time // the deadline each dial was given
	uris          []string
	binds         []string // "external" or "simple:<dn>:<password>"
	searches      []*ldap.SearchRequest
}

var errFake = errors.New("fake slapd: injected failure")

func (f *fakeSlapd) set(fn func(*fakeSlapd)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeSlapd) counts() (dials, closes, binds, searches int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dials, f.closes, len(f.binds), len(f.searches)
}

func (f *fakeSlapd) dial(uri string, deadline time.Time) (Conn, error) {
	f.mu.Lock()
	f.dials++
	f.uris = append(f.uris, uri)
	f.deadlines = append(f.deadlines, deadline)
	hang, release := f.hang, f.release
	err := f.dialErr
	f.mu.Unlock()
	if hang == hangDial {
		// A dial that hangs. It honours the deadline it was given, as a
		// net.Dialer does; without one it hangs until released and then —
		// too late — succeeds, and that connection must not stay open.
		if waitOrDeadline(release, nil, deadline) {
			return nil, ldap.NewError(ldap.ErrorNetwork, errDeadline)
		}
	}
	if err != nil {
		return nil, err
	}
	c := &fakeConn{s: f, closed: make(chan struct{}), deadline: deadline}
	f.mu.Lock()
	f.conns = append(f.conns, c)
	f.mu.Unlock()
	return c, nil
}

// Which call hangs until release is closed (or, for bind and search, until the
// connection is closed — as go-ldap's Close aborts an in-flight operation).
const (
	hangDial   = "dial"
	hangBind   = "bind"
	hangSearch = "search"
)

// openConns is the number of connections handed out and never closed.
func (f *fakeSlapd) openConns() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.conns {
		if !c.isClosed {
			n++
		}
	}
	return n
}

type fakeConn struct {
	s        *fakeSlapd
	closed   chan struct{}
	deadline time.Time // the socket deadline the DialFunc was given
	isClosed bool      // guarded by s.mu
}

var errDeadline = errors.New("fake slapd: i/o timeout (deadline exceeded)")

// waitOrDeadline blocks until release or closed is closed, or the deadline
// (when set) passes; it reports whether the wait ended by deadline or close —
// i.e. the operation failed rather than completed.
func waitOrDeadline(release, closed <-chan struct{}, deadline time.Time) (failed bool) {
	var expired <-chan time.Time
	if !deadline.IsZero() {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		expired = timer.C
	}
	select {
	case <-release:
		return false
	case <-closed:
		return true
	case <-expired:
		return true
	}
}

// maybeHang blocks while this call is the scripted hang — a slapd that
// accepted the connection and stopped answering. Like a real socket it fails
// at the deadline, or when the connection is closed under it; it reports
// whether the call failed.
func (c *fakeConn) maybeHang(op string) (aborted bool) {
	c.s.mu.Lock()
	hang, release := c.s.hang, c.s.release
	c.s.mu.Unlock()
	if hang != op {
		return false
	}
	return waitOrDeadline(release, c.closed, c.deadline)
}

func (c *fakeConn) bind(record string) error {
	c.s.mu.Lock()
	c.s.binds = append(c.s.binds, record)
	err := c.s.bindErr
	c.s.mu.Unlock()
	if c.maybeHang(hangBind) {
		return ldap.NewError(ldap.ErrorNetwork, errFake)
	}
	return err
}

func (c *fakeConn) Bind(username, password string) error {
	return c.bind("simple:" + username + ":" + password)
}

func (c *fakeConn) ExternalBind() error { return c.bind("external") }

func (c *fakeConn) Search(req *ldap.SearchRequest) (*ldap.SearchResult, error) {
	c.s.mu.Lock()
	c.s.searches = append(c.s.searches, req)
	res := &ldap.SearchResult{Entries: cloneEntries(c.s.entries)}
	searchErr, partial := c.s.searchErr, c.s.partial
	c.s.mu.Unlock()
	if c.maybeHang(hangSearch) {
		return nil, ldap.NewError(ldap.ErrorNetwork, errFake)
	}
	if searchErr != nil {
		if partial {
			return res, searchErr
		}
		return nil, searchErr
	}
	return res, nil
}

func (c *fakeConn) Close() error {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	c.s.closes++
	if !c.isClosed {
		c.isClosed = true
		close(c.closed)
	}
	return nil
}

// ─── Scraping a Collector ───────────────────────────────────────────────────

// scraped is one gathered sample with its family type.
type scraped struct {
	Sample
	Type string // "GAUGE", "COUNTER", ...
}

// scrape runs one Gather against reg (the registry outlives the call, as
// Prometheus's scrapes outlive each other) and flattens the result.
func scrape(t *testing.T, reg *prometheus.Registry) []scraped {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v — a failed cn=Monitor read must not fail the scrape", err)
	}
	var out []scraped
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			var v float64
			typ := mf.GetType().String()
			switch typ {
			case typeGauge:
				v = m.GetGauge().GetValue()
			case typeCounter:
				v = m.GetCounter().GetValue()
			case "UNTYPED":
				v = m.GetUntyped().GetValue()
			default:
				t.Fatalf("family %s has unexpected type %s", mf.GetName(), typ)
			}
			out = append(out, scraped{Name: mf.GetName(), Labels: labels, Value: v, Type: typ})
		}
	}
	return out
}

func samplesOf(in []scraped) []Sample {
	out := make([]Sample, 0, len(in))
	for _, s := range in {
		out = append(out, s.Sample)
	}
	return out
}

// newScrapeTarget wires a Collector over a fake slapd into its own registry.
func newScrapeTarget(t *testing.T, opts Options, f *fakeSlapd) *prometheus.Registry {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(NewCollector(opts, f.dial)); err != nil {
		t.Fatalf("register collector: %v", err)
	}
	return reg
}

func externalOpts() Options {
	return Options{URI: DefaultURI, Bind: BindExternal, Listen: DefaultListen, Timeout: DefaultTimeout}
}

// monitorKeys is the set of cn=Monitor-derived identities in a scrape.
func monitorKeys(t *testing.T, in []scraped) map[string]float64 {
	t.Helper()
	return byKey(t, filterSamples(samplesOf(in), func(s Sample) bool { return isMonitorFamily(s.Name) }))
}
