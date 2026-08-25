package controller

import (
	"testing"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// ADR-019 R8: converging an existing cluster off its legacy cluster-shared
// cn=accesslog is a teardown, not a rename (olcSuffix and olcDbDirectory are
// not runtime-mutable). planAccesslogMigration is the pure decision seam: given
// one pod's observed cn=config mdb databases and every accesslog overlay on it,
// decide what this SlapdDatabase's reconcile must tear down.
//
// The two properties that matter most:
//
//   - it must take ZERO action on a cluster that only has per-database logs, so
//     the teardown can never fire on a healthy cluster; and
//   - detection must be exact — cn=accesslog-default is a per-database log, not
//     the legacy shared one.
func TestPlanAccesslogMigration(t *testing.T) {
	const (
		dataDN   = "olcDatabase={1}mdb,cn=config"
		otherDN  = "olcDatabase={3}mdb,cn=config"
		legacyDN = "olcDatabase={2}mdb,cn=config"
		myLogDN  = "olcDatabase={4}mdb,cn=config"
	)
	var (
		myOverlay    = "olcOverlay={0}accesslog," + dataDN
		otherOverlay = "olcOverlay={0}accesslog," + otherDN
		// The legacy shared log: suffix cn=accesslog at the accesslog mount root.
		legacyLog = observedLogDB{DN: legacyDN, Suffix: "cn=accesslog", Dir: ldapv1alpha1.AccesslogRoot}
		// This database's own per-database log (ADR-019 R1).
		myLog = observedLogDB{DN: myLogDN, Suffix: ldapv1alpha1.AccesslogSuffix("default"), Dir: ldapv1alpha1.AccesslogDir("default")}
	)

	cases := []struct {
		name     string
		mdbs     []observedLogDB
		overlays []observedAccesslogOverlay
		want     accesslogMigrationPlan
	}{
		{
			// A cluster that never had a shared log, or has already migrated.
			// Nothing may be torn down here — this is the guard that keeps R8
			// from touching a healthy cluster.
			name:     "clean per-database cluster takes no action",
			mdbs:     []observedLogDB{myLog},
			overlays: []observedAccesslogOverlay{{DN: myOverlay, LogDB: ldapv1alpha1.AccesslogSuffix("default")}},
			want:     accesslogMigrationPlan{},
		},
		{
			// The legacy shape: one shared log, our overlay pointing at it, and
			// nobody else referencing it. Drop the overlay, then reap the DB.
			name:     "legacy shared log with our overlay migrates fully",
			mdbs:     []observedLogDB{legacyLog},
			overlays: []observedAccesslogOverlay{{DN: myOverlay, LogDB: "cn=accesslog"}},
			want:     accesslogMigrationPlan{DropOverlay: true, DeleteLegacyDN: legacyDN},
		},
		{
			// Exactness. A per-database log is a prefix-extension of the legacy
			// suffix, so a substring test would tear down every healthy cluster.
			name:     "cn=accesslog-default is not the legacy shared log",
			mdbs:     []observedLogDB{myLog},
			overlays: []observedAccesslogOverlay{{DN: myOverlay, LogDB: ldapv1alpha1.AccesslogSuffix("default")}},
			want:     accesslogMigrationPlan{},
		},
		{
			// Second reconcile straight after a migration (ADR-001): the log DB
			// is per-database, the overlay names it, the legacy DB is gone.
			// Indistinguishable from the first — no action.
			name:     "already-migrated pod is idempotent on the next reconcile",
			mdbs:     []observedLogDB{myLog, {DN: otherDN, Suffix: "dc=ex,dc=com", Dir: "/data/default"}},
			overlays: []observedAccesslogOverlay{{DN: myOverlay, LogDB: ldapv1alpha1.AccesslogSuffix("default")}},
			want:     accesslogMigrationPlan{},
		},
		{
			// Multi-database interleaving. Another SlapdDatabase's overlay still
			// journals into the shared log, so the DB must NOT be deleted yet:
			// yanking it out from under a live overlay leaves that database's
			// writes pointing at a database slapd no longer has. Our own overlay
			// still goes, and the other CR's reconcile reaps the DB when it is
			// the last one out.
			name: "shared log still referenced by another database is kept",
			mdbs: []observedLogDB{legacyLog},
			overlays: []observedAccesslogOverlay{
				{DN: myOverlay, LogDB: "cn=accesslog"},
				{DN: otherOverlay, LogDB: "cn=accesslog"},
			},
			want: accesslogMigrationPlan{DropOverlay: true},
		},
		{
			// The tail of the interleaved case, and of a database that was
			// demoted out of delta-sync: the shared log is orphaned. Reap it
			// without touching our (already correct) overlay.
			name:     "orphaned shared log is reaped without dropping our overlay",
			mdbs:     []observedLogDB{legacyLog, myLog},
			overlays: []observedAccesslogOverlay{{DN: myOverlay, LogDB: ldapv1alpha1.AccesslogSuffix("default")}},
			want:     accesslogMigrationPlan{DeleteLegacyDN: legacyDN},
		},
		{
			// A cn=accesslog that is NOT at the accesslog mount root is not the
			// log this operator ever created. Both the suffix and the directory
			// must match before anything is deleted.
			name:     "cn=accesslog at a foreign directory is not ours to delete",
			mdbs:     []observedLogDB{myLog, {DN: legacyDN, Suffix: "cn=accesslog", Dir: "/srv/handmade"}},
			overlays: []observedAccesslogOverlay{{DN: myOverlay, LogDB: ldapv1alpha1.AccesslogSuffix("default")}},
			want:     accesslogMigrationPlan{},
		},
		{
			// cn=config attribute values are case-insensitive DNs and slapd
			// normalises them on its own schedule; the observed spelling must
			// not decide whether a cluster migrates.
			name:     "detection is case- and whitespace-insensitive",
			mdbs:     []observedLogDB{{DN: legacyDN, Suffix: "CN=AccessLog", Dir: ldapv1alpha1.AccesslogRoot + "/"}},
			overlays: []observedAccesslogOverlay{{DN: myOverlay, LogDB: " cn=AccessLog "}},
			want:     accesslogMigrationPlan{DropOverlay: true, DeleteLegacyDN: legacyDN},
		},
		{
			// The overlay belonging to another database must never be read as
			// ours, even when ours is missing entirely (hand-deleted, or a
			// failed earlier migration attempt that got as far as the drop).
			name:     "another database's overlay is not mistaken for ours",
			mdbs:     []observedLogDB{legacyLog},
			overlays: []observedAccesslogOverlay{{DN: otherOverlay, LogDB: "cn=accesslog"}},
			want:     accesslogMigrationPlan{},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := planAccesslogMigration(dataDN, c.mdbs, c.overlays)
			if got != c.want {
				t.Errorf("planAccesslogMigration = %+v, want %+v", got, c.want)
			}
		})
	}
}
