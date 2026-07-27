package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// newRestoreJobFixture builds a restore Job for the given pod target and returns
// the bash script its main container (restore/wipe) runs.
func newRestoreJobFixture(target restorePodTarget) string {
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
			return c.Command[len(c.Command)-1]
		}
	}
	return ""
}

// Regression: an in-place / bootstrap restore of a REPLICATED database must wipe
// the accesslog LMDB too, not just the main DB. If the pre-restore accesslog
// survives, delta-syncrepl replays its deltas (most damagingly a delete) on
// scale-up at a CSN newer than the restored contextCSN, silently undoing the
// restore. Bug: restore_job.go only wiped /data/<dir>. See ADR-014.
func TestBuildRestoreJobWipesAccesslogWhenReplicated(t *testing.T) {
	script := newRestoreJobFixture(restorePodTarget{
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
	if !strings.Contains(script, "/accesslog/data.mdb") || !strings.Contains(script, "/accesslog/lock.mdb") {
		t.Errorf("restore script does not wipe the accesslog LMDB (stale deltas will be replayed):\n%s", script)
	}
}

// A non-replicated RW restore has no accesslog PVC mounted, so the script must
// not touch /accesslog (the path does not exist in that pod).
func TestBuildRestoreJobNoAccesslogWipeWhenStandalone(t *testing.T) {
	script := newRestoreJobFixture(restorePodTarget{
		jobName:   "j",
		dataPVC:   "data-slapd-0",
		configPVC: "config-slapd-0",
		loadData:  true,
	})
	if strings.Contains(script, "/accesslog") {
		t.Errorf("standalone restore script must not reference /accesslog:\n%s", script)
	}
}
