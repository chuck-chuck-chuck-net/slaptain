package controller

import (
	"testing"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// ADR-034's source becomes the DEFAULT: unset now means Accesslog, where it
// used to mean the in-memory log. The in-memory log is lost at every restart,
// so a consumer that reconnects after one falls out of the window and takes a
// full refresh it did not need; the journal survives.
//
// Two things this must NOT do, and they are the reason for a test rather than
// a one-character edit:
//
//   - override an explicit `syncprovSessionlog: 0`, which is ADR-022's "no
//     session log at all" opt-out. Defaulting a source over somebody's
//     explicit off switch would hand them the thing they switched off.
//   - override an explicit `InMemory`, which is now the way to ask for the old
//     behaviour.
func TestSessionlogSourceDefaultsToAccesslog(t *testing.T) {
	db := func(src string, ops *int32) *ldapv1alpha1.SlapdDatabase {
		sd := &ldapv1alpha1.SlapdDatabase{}
		sd.Name = "example-db"
		sd.Spec.Replication.SyncprovSessionlogSource = src
		sd.Spec.Replication.SyncprovSessionlog = ops
		return sd
	}
	n := func(v int32) *int32 { return &v }

	cases := []struct {
		name     string
		sd       *ldapv1alpha1.SlapdDatabase
		wantKind sessionlogKind
		wantDN   string
	}{
		{
			name:     "unset now means the accesslog, not the in-memory log",
			sd:       db("", nil),
			wantKind: sessionlogKindAccesslog,
			wantDN:   testAccesslogDB,
		},
		{
			name:     "explicit Accesslog is unchanged",
			sd:       db(ldapv1alpha1.SyncprovSessionlogSourceAccesslog, nil),
			wantKind: sessionlogKindAccesslog,
			wantDN:   testAccesslogDB,
		},
		{
			name:     "explicit InMemory is how you ask for the old behaviour",
			sd:       db(ldapv1alpha1.SyncprovSessionlogSourceInMemory, nil),
			wantKind: sessionlogKindInMemory,
		},
		{
			name:     "an explicit size still gets the in-memory log it sized",
			sd:       db(ldapv1alpha1.SyncprovSessionlogSourceInMemory, n(9000)),
			wantKind: sessionlogKindInMemory,
		},
		{
			name:     "syncprovSessionlog 0 means NO log, and the default must not undo it",
			sd:       db("", n(0)),
			wantKind: sessionlogKindNone,
		},
		{
			name:     "0 with an explicit InMemory is still no log",
			sd:       db(ldapv1alpha1.SyncprovSessionlogSourceInMemory, n(0)),
			wantKind: sessionlogKindNone,
		},
		{
			name:     "but an EXPLICIT Accesslog source outranks 0 — the user named a source",
			sd:       db(ldapv1alpha1.SyncprovSessionlogSourceAccesslog, n(0)),
			wantKind: sessionlogKindAccesslog,
			wantDN:   testAccesslogDB,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := desiredSessionlog(tc.sd)
			if got.Kind != tc.wantKind {
				t.Fatalf("desiredSessionlog kind = %v, want %v", got.Kind, tc.wantKind)
			}
			if tc.wantDN != "" && got.SourceDN != tc.wantDN {
				t.Errorf("desiredSessionlog SourceDN = %q, want %q", got.SourceDN, tc.wantDN)
			}
		})
	}
}

// The startup-config table is what actually reaches a pod, and it only carries
// accesslog-sourced databases. With the default flipped, a database that says
// nothing must now appear in it — otherwise the default would be a value the
// init container never converges.
func TestTheDefaultReachesTheStartupConfigTable(t *testing.T) {
	sd := ldapv1alpha1.SlapdDatabase{
		Name: "example-db"}
	sd.Spec.ClusterRef = testClusterName
	sd.Spec.Suffix = "dc=example,dc=org"

	got := startupConfigFor([]ldapv1alpha1.SlapdDatabase{sd}, "slapd")
	if len(got.Records) != 1 {
		t.Fatalf("a database that says nothing must now be in the startup table, got %d records", len(got.Records))
	}
	if got.Records[0].Value != testAccesslogDB {
		t.Errorf("record value = %q, want the journal DN", got.Records[0].Value)
	}
}
