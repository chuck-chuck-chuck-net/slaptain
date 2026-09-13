package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// newRestoreJobFixture builds a restore Job for the given pod target and returns
// the bash script its main container (restore/wipe) runs.
func newRestoreJobFixture(target restorePodTarget) (string, *ldapv1alpha1.SlapdDatabase) {
	sc := &ldapv1alpha1.SlapdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "slapd", Namespace: "ns"},
	}
	sc.Status.Restore = &ldapv1alpha1.SlapdClusterRestoreStatus{ID: "r1"}
	sd := &ldapv1alpha1.SlapdDatabase{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-db"},
		Spec:       ldapv1alpha1.SlapdDatabaseSpec{Suffix: "dc=demo,dc=example"},
	}
	st := ldapv1alpha1.S3StorageSpec{Bucket: "b", CredentialsSecretName: "s3"}
	job := buildRestoreJob(sc, sd, st, "demo/key.ldif.gz", "init:img", "op:img", target)
	// The main container is "restore" (loadData) or "wipe" (RO).
	for _, c := range job.Spec.Template.Spec.Containers {
		if c.Name == "restore" || c.Name == "wipe" {
			return c.Command[len(c.Command)-1], sd
		}
	}
	return "", sd
}

// Regression: an in-place / bootstrap restore of a REPLICATED database must wipe
// the accesslog LMDB too, not just the main DB. If the pre-restore accesslog
// survives, delta-syncrepl replays its deltas (most damagingly a delete) on
// scale-up at a CSN newer than the restored contextCSN, silently undoing the
// restore. Bug: restore_job.go only wiped /data/<dir>. See ADR-014.
func TestBuildRestoreJobWipesAccesslogWhenReplicated(t *testing.T) {
	script, sd := newRestoreJobFixture(restorePodTarget{
		jobName:      "j",
		dataPVC:      "data-slapd-0",
		configPVC:    "config-slapd-0",
		accesslogPVC: "accesslog-slapd-0",
		loadData:     true,
	})

	// Main DB wipe is always present.
	if !strings.Contains(script, `rm -f "/data/$DATADIR/data.mdb" "/data/$DATADIR/lock.mdb"`) {
		t.Errorf("restore script missing the main-DB wipe:\n%s", script)
	}
	// The accesslog journal must be wiped too, or syncrepl replays stale deltas.
	// ADR-019: the journal is per-database (cn=accesslog-<db> at
	// /accesslog/<db>), so the wipe is scoped to the restored database's own
	// directory. Restoring one database must not truncate another database's
	// journal — that would kick every consumer of the untouched database into a
	// full refresh for no reason.
	dir := ldapv1alpha1.AccesslogDir(sd.Name)
	if !strings.Contains(script, dir+"/data.mdb") || !strings.Contains(script, dir+"/lock.mdb") {
		t.Errorf("restore script does not wipe %s (stale deltas will be replayed):\n%s", dir, script)
	}
	// ...and it must NOT be the old cluster-wide wipe at the accesslog root.
	if strings.Contains(script, ldapv1alpha1.AccesslogRoot+"/data.mdb") ||
		strings.Contains(script, ldapv1alpha1.AccesslogRoot+"/lock.mdb") {
		t.Errorf("restore script wipes EVERY database's journal, not just %s's:\n%s", sd.Name, script)
	}
}

// A non-replicated RW restore has no accesslog PVC mounted, so the script must
// not touch /accesslog (the path does not exist in that pod).
func TestBuildRestoreJobNoAccesslogWipeWhenStandalone(t *testing.T) {
	script, _ := newRestoreJobFixture(restorePodTarget{
		jobName:   "j",
		dataPVC:   "data-slapd-0",
		configPVC: "config-slapd-0",
		loadData:  true,
	})
	if strings.Contains(script, "/accesslog") {
		t.Errorf("standalone restore script must not reference /accesslog:\n%s", script)
	}
}

// A restore runs with the whole cluster scaled to zero, so its duration is
// downtime; on a multi-GB LDIF the difference between slapadd's normal mode and
// its bulk mode is tens of minutes versus hours (production-config review,
// finding 12).
//
// The assertion is paired deliberately: -q is only sound against an empty
// database, so the test pins BOTH that the flag is there AND that the wipe still
// precedes the load. A future edit that reorders them, or drops the wipe,
// turns a safe optimisation into a corrupting one.
func TestBuildRestoreJobUsesBulkLoadAfterWiping(t *testing.T) {
	script, sd := newRestoreJobFixture(restorePodTarget{
		jobName: "j", dataPVC: "data-slapd-0", configPVC: "config-slapd-0",
		accesslogPVC: "accesslog-slapd-0", loadData: true,
	})

	if !strings.Contains(script, "slapadd -q ") {
		t.Errorf("restore must use slapadd bulk mode, got:\n%s", script)
	}

	wipeAt := strings.Index(script, "rm -f")
	loadAt := strings.Index(script, "slapadd")
	if wipeAt < 0 || loadAt < 0 || wipeAt > loadAt {
		t.Errorf("the wipe must precede the bulk load — -q is only sound against "+
			"an empty database (wipe at %d, load at %d):\n%s", wipeAt, loadAt, script)
	}
	if !strings.Contains(script, `rm -f "/data/$DATADIR/data.mdb"`) {
		t.Errorf("the wipe must target this database's directory (%s):\n%s", sd.Name, script)
	}
}
