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
	sd.Name = "exdb"
	sd.Spec.Suffix = "dc=example,dc=org"
	f(&sd.Spec)
	return sd
}

// ── 1. Replication identity limits (ADR-024 R7, ADR-020 amendment) ──────────

func TestReplicationLimits(t *testing.T) {
	got := replicationLimits("exdb", "dc=example,dc=org")
	want := []string{
		`dn.exact="cn=repl-exdb,cn=slaptain-auth" ` +
			`time.soft=unlimited time.hard=unlimited size.soft=unlimited size.hard=unlimited`,
		`dn.exact="cn=replication,dc=example,dc=org" ` +
			`time.soft=unlimited time.hard=unlimited size.soft=unlimited size.hard=unlimited`,
	}
	if len(got) != len(want) {
		t.Fatalf("replicationLimits =\n  %q\nwant\n  %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("replicationLimits[%d] =\n  %q\nwant\n  %q", i, got[i], want[i])
		}
	}
}

// The DN the limits name must be the same DN the ACL names. Drift between the
// two is the failure mode this pins: an ACL that grants read to an identity
// whose searches are capped at 500 is a replication cap nobody can see in the
// ACL.
func TestReplicationLimitsMatchesACLIdentity(t *testing.T) {
	const (
		dbName = "driftdb"
		suffix = "o=drift,dc=example,dc=net"
	)
	acl := accesslogACL(dbName, suffix)
	lims := replicationLimits(dbName, suffix)
	// Every identity the ACL grants read must also carry a limits exemption,
	// and vice versa. Since ADR-027 that is both DNs.
	for _, dn := range []string{
		`dn.exact="cn=repl-` + dbName + `,cn=slaptain-auth"`,
		`dn.exact="cn=replication,` + suffix + `"`,
	} {
		if !strings.Contains(acl, dn) {
			t.Errorf("accesslogACL(%q,%q) = %q does not name %s", dbName, suffix, acl, dn)
		}
		found := false
		for _, l := range lims {
			if strings.HasPrefix(l, dn+" ") {
				found = true
			}
		}
		if !found {
			t.Errorf("replicationLimits(%q,%q) = %q does not select %s", dbName, suffix, lims, dn)
		}
	}
}

func TestDesiredLimits(t *testing.T) {
	// Both replication identities, in the order desiredLimits emits them
	// (ADR-027: node-local first, legacy second — see replicationLimits).
	repl := replicationLimits("exdb", "dc=example,dc=org")
	user := `dn.exact="cn=bulk,dc=example,dc=org" size=unlimited`

	cases := []struct {
		name        string
		limits      []string
		replicating bool
		want        []string
	}{
		{"no replication, no user limits", nil, false, nil},
		{"replicating, no user limits", nil, true, repl},
		{"replicating, user limits follow ours", []string{user}, true, append(append([]string{}, repl...), user)},
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
	repl := replicationLimits("exdb", "dc=example,dc=org")[0]
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

// The live cluster overruled the design here: an ldapmodify of olcDbMaxSize
// against a running back-mdb database segfaults slapd (observed on OpenLDAP
// 2.7.1, exit 139). So the seam COMPARES; nothing writes to a live database.
func TestCompareMaxSize(t *testing.T) {
	const want32Gi = int64(34359738368)
	cases := []struct {
		name        string
		current     string
		desired     int64
		wantVerdict maxSizeVerdict
		wantCurrent string
	}{
		{"absent → needs grow (back-mdb's ~10 MB)", "", want32Gi, maxSizeNeedsGrow, ""},
		{"equal → matches", "34359738368", want32Gi, maxSizeMatches, "34359738368"},
		{"smaller → needs grow", "1073741824", want32Gi, maxSizeNeedsGrow, "1073741824"},
		{"larger → needs shrink", "68719476736", want32Gi, maxSizeNeedsShrink, "68719476736"},
		{"unparseable current → needs grow, reported verbatim", "wat", want32Gi, maxSizeNeedsGrow, "wat"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict, cur := compareMaxSize(tc.current, tc.desired)
			if verdict != tc.wantVerdict {
				t.Fatalf("compareMaxSize(%q, %d) verdict = %v, want %v",
					tc.current, tc.desired, verdict, tc.wantVerdict)
			}
			if cur != tc.wantCurrent {
				t.Fatalf("compareMaxSize(%q, %d) current = %q, want %q",
					tc.current, tc.desired, cur, tc.wantCurrent)
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

// ── 6. User indices vs the operator baseline ────────────────────────────────

// Live regression, found on t3e the first time a database was created by the
// new code: back-mdb rejects a SECOND olcDbIndex definition for an attribute
// that already has one ("duplicate index definition for attr \"entryCSN\"",
// LDAP result 80), and applyIndices compared whole VALUES, not attributes. So
// a fresh DB carrying the baseline as one combined value —
// "objectClass,entryCSN,entryUUID eq" — plus a user spec.indices listing
// "entryCSN eq" wedged every pod of the database in Error.
//
// The seam must subtract what is already indexed, per attribute, and keep the
// user's index types for whatever is left.
func TestPlanUserIndices(t *testing.T) {
	cases := []struct {
		name    string
		current []string
		desired []string
		want    []string
	}{
		{
			name:    "the t3e wedge: baseline as one combined value",
			current: []string{"objectClass,entryCSN,entryUUID eq"},
			desired: []string{"objectClass eq", "uid eq,sub", "entryCSN eq", "entryUUID eq"},
			want:    []string{"uid eq,sub"},
		},
		{
			name:    "nothing indexed yet",
			current: nil,
			desired: []string{"uid eq,sub", "cn eq"},
			want:    []string{"uid eq,sub", "cn eq"},
		},
		{
			name:    "partially covered multi-attribute value keeps the rest",
			current: []string{"objectClass eq"},
			desired: []string{"objectClass,sn eq"},
			want:    []string{"sn eq"},
		},
		{
			name:    "exact duplicates are dropped",
			current: []string{"uid eq,sub"},
			desired: []string{"uid eq,sub"},
			want:    nil,
		},
		{
			name:    "a value with no type field still parses",
			current: []string{"objectClass eq"},
			desired: []string{"member"},
			want:    []string{"member"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planUserIndices(tc.current, tc.desired)
			if len(got) != len(tc.want) {
				t.Fatalf("planUserIndices(%v, %v) = %v, want %v", tc.current, tc.desired, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("planUserIndices[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}
