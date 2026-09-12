package controller

import (
	"strings"
	"testing"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The pure seams behind the five breaks-at-scale fixes (ADR-024). Each of these
// decides something the LDAP half only executes: which olcLimits values a
// database must carry, whether an olcDbMaxSize write is a grow or a rejected
// shrink, which baseline indices are missing, and what the unset search limits
// resolve to.
//
// Red-first: written against a stub file that returns zero values for all of
// them, observed failing, then implemented.

func ptrStr(s string) *string { return &s }

func dbWith(f func(*ldapv1alpha1.SlapdDatabaseSpec)) *ldapv1alpha1.SlapdDatabase {
	sd := &ldapv1alpha1.SlapdDatabase{}
	sd.Spec.Suffix = "dc=example,dc=org"
	f(&sd.Spec)
	return sd
}

// ── 1. Replication identity limits (ADR-024 R7, ADR-020 amendment) ──────────

func TestReplicationLimits(t *testing.T) {
	got := replicationLimits("dc=example,dc=org")
	want := `dn.exact="cn=replication,dc=example,dc=org" ` +
		`time.soft=unlimited time.hard=unlimited size.soft=unlimited size.hard=unlimited`
	if got != want {
		t.Fatalf("replicationLimits =\n  %q\nwant\n  %q", got, want)
	}
}

// The DN the limits name must be the same DN the ACL names. Drift between the
// two is the failure mode this pins: an ACL that grants read to an identity
// whose searches are capped at 500 is a replication cap nobody can see in the
// ACL.
func TestReplicationLimitsMatchesACLIdentity(t *testing.T) {
	const suffix = "o=drift,dc=example,dc=net"
	acl := accesslogACL(suffix)
	lim := replicationLimits(suffix)
	const dn = `dn.exact="cn=replication,` + suffix + `"`
	if !strings.Contains(acl, dn) {
		t.Fatalf("accesslogACL(%q) = %q does not name %s", suffix, acl, dn)
	}
	if !strings.HasPrefix(lim, dn+" ") {
		t.Fatalf("replicationLimits(%q) = %q does not select %s", suffix, lim, dn)
	}
}

func TestDesiredLimits(t *testing.T) {
	repl := replicationLimits("dc=example,dc=org")
	user := `dn.exact="cn=bulk,dc=example,dc=org" size=unlimited`

	cases := []struct {
		name        string
		limits      []string
		replicating bool
		want        []string
	}{
		{"no replication, no user limits", nil, false, nil},
		{"replicating, no user limits", nil, true, []string{repl}},
		{"replicating, user limits follow ours", []string{user}, true, []string{repl, user}},
		{"not replicating, user limits stand alone", []string{user}, false, []string{user}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sd := dbWith(func(s *ldapv1alpha1.SlapdDatabaseSpec) { s.Limits = tc.limits })
			got := desiredLimits(sd, tc.replicating)
			if len(got) != len(tc.want) {
				t.Fatalf("desiredLimits = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("desiredLimits[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// desiredLimits must never mutate the caller's spec slice — applyACLs' sibling
// bug class: prepending onto sd.Spec.X aliases the CR's own backing array.
func TestDesiredLimitsDoesNotMutateSpec(t *testing.T) {
	user := []string{`dn.exact="cn=bulk,dc=example,dc=org" size=unlimited`}
	sd := dbWith(func(s *ldapv1alpha1.SlapdDatabaseSpec) { s.Limits = user })
	_ = desiredLimits(sd, true)
	if len(sd.Spec.Limits) != 1 || sd.Spec.Limits[0] != user[0] {
		t.Fatalf("spec.limits mutated: %v", sd.Spec.Limits)
	}
}

func TestLimitsMatch(t *testing.T) {
	repl := replicationLimits("dc=example,dc=org")
	cases := []struct {
		name    string
		current []string
		desired []string
		want    bool
	}{
		{"both empty", nil, nil, true},
		{"stored with {N} prefix matches", []string{"{0}" + repl}, []string{repl}, true},
		{"missing entirely", nil, []string{repl}, false},
		{"stale value", []string{`{0}dn.exact="cn=replication,dc=example,dc=org" size=500`}, []string{repl}, false},
		{"extra stored value", []string{"{0}" + repl, "{1}other"}, []string{repl}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := limitsMatch(tc.current, tc.desired); got != tc.want {
				t.Fatalf("limitsMatch(%v, %v) = %v, want %v", tc.current, tc.desired, got, tc.want)
			}
		})
	}
}

// ── 2. olcDbMaxSize (ADR-024 R1 + R4) ───────────────────────────────────────

func TestParseMaxSize(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"34359738368", 34359738368, false}, // bare bytes, the form slapd takes
		{"32Gi", 34359738368, false},        // the form the doc comment promised
		{"1Gi", 1073741824, false},
		{"1G", 1000000000, false},
		{" 32Gi ", 34359738368, false},
		{"", 0, true},
		{"32GB", 0, true}, // not a k8s quantity suffix
		{"-1", 0, true},
		{"0", 0, true}, // a zero map size is not a thing
		{"lots", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseMaxSize(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseMaxSize(%q) = %d, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMaxSize(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("parseMaxSize(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestDefaultMaxSizes(t *testing.T) {
	// The operator's opinion, not back-mdb's ~10 MB. Pinned so a change is a
	// deliberate edit with a reason, and so both defaults stay comfortably
	// above the point where a real directory or a real journal wedges.
	const gib = int64(1) << 30
	if defaultDataMaxSizeBytes != 32*gib {
		t.Fatalf("defaultDataMaxSizeBytes = %d, want %d (32Gi)", defaultDataMaxSizeBytes, 32*gib)
	}
	if defaultAccesslogMaxSizeBytes != 8*gib {
		t.Fatalf("defaultAccesslogMaxSizeBytes = %d, want %d (8Gi)", defaultAccesslogMaxSizeBytes, 8*gib)
	}
}

func TestDesiredDataMaxSize(t *testing.T) {
	t.Run("unset takes the operator default", func(t *testing.T) {
		sd := dbWith(func(s *ldapv1alpha1.SlapdDatabaseSpec) { s.MaxSize = "" })
		got, err := desiredDataMaxSize(sd)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != defaultDataMaxSizeBytes {
			t.Fatalf("desiredDataMaxSize = %d, want %d", got, defaultDataMaxSizeBytes)
		}
	})
	t.Run("quantity is honoured", func(t *testing.T) {
		sd := dbWith(func(s *ldapv1alpha1.SlapdDatabaseSpec) { s.MaxSize = "64Gi" })
		got, err := desiredDataMaxSize(sd)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != 64*(int64(1)<<30) {
			t.Fatalf("desiredDataMaxSize = %d, want %d", got, 64*(int64(1)<<30))
		}
	})
	t.Run("garbage is an error, not a silent default", func(t *testing.T) {
		sd := dbWith(func(s *ldapv1alpha1.SlapdDatabaseSpec) { s.MaxSize = "big" })
		if _, err := desiredDataMaxSize(sd); err == nil {
			t.Fatal("desiredDataMaxSize(\"big\") returned no error")
		}
	})
}

func TestPlanMaxSize(t *testing.T) {
	const want32Gi = int64(34359738368)
	cases := []struct {
		name       string
		current    string
		desired    int64
		wantAction maxSizeAction
		wantValue  string
	}{
		{"absent → write", "", want32Gi, maxSizeWrite, "34359738368"},
		{"equal → noop", "34359738368", want32Gi, maxSizeNoop, ""},
		{"smaller → grow", "1073741824", want32Gi, maxSizeWrite, "34359738368"},
		{"larger → rejected, never shrunk", "68719476736", want32Gi, maxSizeShrinkRejected, "68719476736"},
		{"unparseable current → write ours", "wat", want32Gi, maxSizeWrite, "34359738368"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			action, value := planMaxSize(tc.current, tc.desired)
			if action != tc.wantAction {
				t.Fatalf("planMaxSize(%q, %d) action = %v, want %v",
					tc.current, tc.desired, action, tc.wantAction)
			}
			if value != tc.wantValue {
				t.Fatalf("planMaxSize(%q, %d) value = %q, want %q",
					tc.current, tc.desired, value, tc.wantValue)
			}
		})
	}
}

// ── 3. Search limits (ADR-024 R5) ───────────────────────────────────────────

func TestDesiredSearchLimits(t *testing.T) {
	cases := []struct {
		name      string
		size      *string
		time      *string
		wantSize  string
		wantTime  string
		wantWrite bool
	}{
		{"unset → slaptain's unlimited, not slapd's 500/3600", nil, nil, "unlimited", "unlimited", true},
		{"explicit numbers are honoured", ptrStr("500"), ptrStr("3600"), "500", "3600", true},
		{"explicit unlimited", ptrStr("unlimited"), ptrStr("unlimited"), "unlimited", "unlimited", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sd := dbWith(func(s *ldapv1alpha1.SlapdDatabaseSpec) {
				s.SizeLimit, s.TimeLimit = tc.size, tc.time
			})
			gotS, writeS := desiredSizeLimit(sd)
			gotT, writeT := desiredTimeLimit(sd)
			if writeS != tc.wantWrite || gotS != tc.wantSize {
				t.Fatalf("desiredSizeLimit = (%q, %v), want (%q, %v)", gotS, writeS, tc.wantSize, tc.wantWrite)
			}
			if writeT != tc.wantWrite || gotT != tc.wantTime {
				t.Fatalf("desiredTimeLimit = (%q, %v), want (%q, %v)", gotT, writeT, tc.wantTime, tc.wantWrite)
			}
		})
	}
}

// ── 4. Data-DB syncrepl baseline indices (ADR-024 R7) ───────────────────────

func TestDataBaselineIndexAttrs(t *testing.T) {
	want := map[string]bool{"objectClass": true, "entryCSN": true, "entryUUID": true}
	if len(dataBaselineIndexAttrs) != len(want) {
		t.Fatalf("dataBaselineIndexAttrs = %v, want exactly %v", dataBaselineIndexAttrs, want)
	}
	for _, a := range dataBaselineIndexAttrs {
		if !want[a] {
			t.Fatalf("dataBaselineIndexAttrs contains unexpected %q (%v)", a, dataBaselineIndexAttrs)
		}
	}
}

func TestPlanDataBaselineIndices(t *testing.T) {
	cases := []struct {
		name    string
		current []string
		want    []string
	}{
		{"fresh DB plans the whole set", nil, []string{"objectClass,entryCSN,entryUUID eq"}},
		{"only objectClass today", []string{"objectClass eq"}, []string{"entryCSN,entryUUID eq"}},
		{"user declared them already", []string{"objectClass eq", "entryCSN eq", "entryUUID eq"}, nil},
		{"combined value counts", []string{"objectClass,entryCSN,entryUUID eq"}, nil},
		{"richer index types are left alone", []string{"objectClass eq", "entryCSN,entryUUID eq,sub"}, nil},
		{"unrelated indices don't help", []string{"uid eq,sub"}, []string{"objectClass,entryCSN,entryUUID eq"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planDataBaselineIndices(tc.current)
			if len(got) != len(tc.want) {
				t.Fatalf("planDataBaselineIndices(%v) = %v, want %v", tc.current, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("planDataBaselineIndices(%v)[%d] = %q, want %q", tc.current, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// Converging twice must be a no-op — the property that makes this safe to run
// on every reconcile (ADR-001).
func TestPlanDataBaselineIndicesIdempotent(t *testing.T) {
	current := []string{"uid eq,sub"}
	after := append(append([]string{}, current...), planDataBaselineIndices(current)...)
	if got := planDataBaselineIndices(after); got != nil {
		t.Fatalf("planDataBaselineIndices is not idempotent: second pass wants %v", got)
	}
}

// planIndices is the shared seam; the accesslog planner must be exactly it,
// applied to accesslogIndexAttrs. Drift-guard against a future edit to one.
func TestPlanIndicesIsSharedWithAccesslog(t *testing.T) {
	current := []string{"objectClass eq"}
	a := planAccesslogIndices(current)
	b := planIndices(current, accesslogIndexAttrs)
	if len(a) != len(b) {
		t.Fatalf("planAccesslogIndices = %v, planIndices = %v", a, b)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("planAccesslogIndices[%d] = %q, planIndices[%d] = %q", i, a[i], i, b[i])
		}
	}
}

// ── 5. back-mdb IDL exponent (ADR-024 R2) ───────────────────────────────────

func TestMdbIdlExponentEnv(t *testing.T) {
	sc := &ldapv1alpha1.SlapdCluster{}
	if got, ok := mdbIdlExponent(sc); ok {
		t.Fatalf("unset backend must not emit an idlexp directive; got %d", got)
	}
	exp := int32(20)
	sc.Spec.Backend = &ldapv1alpha1.SlapdMdbBackendConfig{IDLExponent: &exp}
	got, ok := mdbIdlExponent(sc)
	if !ok || got != 20 {
		t.Fatalf("mdbIdlExponent = (%d, %v), want (20, true)", got, ok)
	}
}
