package v1alpha1

import "testing"

// ── ADR-019: per-database accesslog derivation ───────────────────────────────

func TestAccesslogSuffixAndDir(t *testing.T) {
	// ADR-019 R1: naming is keyed on the SlapdDatabase CR name; suffix
	// cn=accesslog-<dbname>, backing directory /accesslog/<dbname>.
	cases := []struct {
		dbName  string
		wantSfx string
		wantDir string
	}{
		{"default", "cn=accesslog-default", "/accesslog/default"},
		{"secondary", "cn=accesslog-secondary", "/accesslog/secondary"},
	}
	for _, c := range cases {
		if got := AccesslogSuffix(c.dbName); got != c.wantSfx {
			t.Errorf("AccesslogSuffix(%q) = %q, want %q", c.dbName, got, c.wantSfx)
		}
		if got := AccesslogDir(c.dbName); got != c.wantDir {
			t.Errorf("AccesslogDir(%q) = %q, want %q", c.dbName, got, c.wantDir)
		}
	}
	// Every per-database directory sits beneath the one accesslog mount
	// (ADR-019 R3) — one PVC, one LMDB directory per database.
	if got, want := AccesslogDir("default"), AccesslogRoot+"/default"; got != want {
		t.Errorf("AccesslogDir must be rooted at AccesslogRoot: %q, want %q", got, want)
	}
}

func TestExternalLogBase(t *testing.T) {
	// ADR-019 R9/R10: default is derived from this database's own accesslog
	// suffix; a non-empty spec override wins, and nothing is written back.
	sd := &SlapdDatabase{}
	sd.Name = "default"
	if got, want := ExternalLogBase(sd), "cn=accesslog-default"; got != want {
		t.Errorf("ExternalLogBase (derived) = %q, want %q", got, want)
	}

	sd.Spec.Replication.ExternalAccesslogSuffix = "cn=log"
	if got, want := ExternalLogBase(sd), "cn=log"; got != want {
		t.Errorf("ExternalLogBase (override) = %q, want %q", got, want)
	}
	if sd.Spec.Replication.ExternalAccesslogSuffix != "cn=log" {
		t.Errorf("ExternalLogBase must not mutate .spec (ADR-019 R10)")
	}
}
