package cmd

import (
	"strings"
	"testing"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// `slctl aws` requires the source to be named. Four kinds carry an
// S3StorageSpec and nothing makes one of them the obvious choice, so the
// command refuses rather than picking — the KISS counterpart to the discovery
// `slctl ldapsearch` does, where there is exactly one thing to discover.
// The command's own namespace, and a backup it may name.
const (
	cmdNamespace = "demo"
	demoBackup   = "demo-backup-1"
	// s3Service is the demo's S3 server (versitygw), as a Service name.
	s3Service = "versitygw"
)

func TestParseStorageSource(t *testing.T) {
	ok := []struct {
		in   string
		want storageSource
	}{
		{"slapdbackup/demo-backup-1", storageSource{kindSlapdBackup, demoBackup}},
		{"sb/demo-backup-1", storageSource{kindSlapdBackup, demoBackup}},
		{"SlapdBackup/demo-backup-1", storageSource{kindSlapdBackup, demoBackup}},
		{"slapdscheduledbackup/nightly", storageSource{kindSlapdScheduledBackup, "nightly"}},
		{"ssb/nightly", storageSource{kindSlapdScheduledBackup, "nightly"}},
		{"slapddatabase/demo-db", storageSource{kindSlapdDatabase, "demo-db"}},
		{"sd/demo-db", storageSource{kindSlapdDatabase, "demo-db"}},
		{"slapdrestore/rollback", storageSource{kindSlapdRestore, "rollback"}},
		{"sr/rollback", storageSource{kindSlapdRestore, "rollback"}},
	}
	for _, tc := range ok {
		got, err := parseStorageSource(tc.in)
		if err != nil {
			t.Errorf("parseStorageSource(%q): unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseStorageSource(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}

	const wantFromForm = "<kind>/<name>"
	bad := []struct{ in, wantSubstr string }{
		{"", "--from is required"},
		{demoBackup, wantFromForm},
		{"backup/demo-backup-1", "unknown kind"},
		{"sb/", wantFromForm},
		{"/demo-backup-1", wantFromForm},
		{"sb/a/b", wantFromForm},
	}
	for _, tc := range bad {
		_, err := parseStorageSource(tc.in)
		if err == nil {
			t.Errorf("parseStorageSource(%q): want error, got nil", tc.in)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantSubstr) {
			t.Errorf("parseStorageSource(%q) error = %q, want it to mention %q", tc.in, err, tc.wantSubstr)
		}
	}
}

// The endpoint in a storage spec is written for the pods that use it, so it is
// routinely a cluster-internal Service DNS name that does not resolve on the
// machine running slctl. That is the whole reason this command exists: classify
// the endpoint, and port-forward the ones that need it.
func TestPlanEndpoint(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		ns   string
		want endpointPlan
	}{
		{
			name: "empty means real AWS",
			raw:  "", ns: cmdNamespace,
			want: endpointPlan{},
		},
		{
			name: "external host passes through untouched",
			raw:  "https://s3.example:9000", ns: cmdNamespace,
			want: endpointPlan{URL: "https://s3.example:9000"},
		},
		{
			name: "service DNS needs a forward",
			raw:  "http://versitygw.slaptain-demo.svc:7480", ns: cmdNamespace,
			want: endpointPlan{Forward: true, Service: s3Service, Namespace: demoNamespace, Port: 7480, Scheme: schemeHTTP},
		},
		{
			name: "fully qualified service DNS needs a forward",
			raw:  "http://versitygw.slaptain-demo.svc.cluster.local:7480", ns: cmdNamespace,
			want: endpointPlan{Forward: true, Service: s3Service, Namespace: demoNamespace, Port: 7480, Scheme: schemeHTTP},
		},
		{
			name: "bare single label is in the target namespace",
			raw:  "http://versitygw:7480", ns: cmdNamespace,
			want: endpointPlan{Forward: true, Service: s3Service, Namespace: cmdNamespace, Port: 7480, Scheme: schemeHTTP},
		},
		{
			name: "no port defaults to the scheme's port",
			raw:  "https://rgw.store.svc", ns: cmdNamespace,
			want: endpointPlan{Forward: true, Service: "rgw", Namespace: "store", Port: 443, Scheme: schemeHTTPS},
		},
		{
			name: "two labels without .svc is external",
			raw:  "https://s3.example.com", ns: cmdNamespace,
			want: endpointPlan{URL: "https://s3.example.com"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := planEndpoint(tc.raw, tc.ns)
			if err != nil {
				t.Fatalf("planEndpoint(%q): unexpected error: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("planEndpoint(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}

	// A bare host with no scheme is not a URL we can act on: we would have to
	// guess http vs https, and guessing wrong against a real store is a
	// credential sent in clear.
	if _, err := planEndpoint("versitygw.slaptain-demo.svc:7480", cmdNamespace); err == nil {
		t.Error("planEndpoint without a scheme: want error, got nil")
	}
}

// Credentials reach the child process through its environment, never through
// argv: an argument vector is world-readable in /proc and lands in shell
// history when the user copies the --verbose line.
func TestAWSEnv(t *testing.T) {
	st := ldapv1alpha1.S3StorageSpec{Bucket: "slaptain-backups", Region: "us-east-1"}
	inherited := []string{"PATH=/usr/bin", "AWS_ENDPOINT_URL=http://stale"}
	env := awsEnv(inherited, st, "AKIA", "s3cret", "http://localhost:34567")

	want := map[string]string{
		"AWS_ACCESS_KEY_ID":     "AKIA",
		"AWS_SECRET_ACCESS_KEY": "s3cret",
		"AWS_DEFAULT_REGION":    "us-east-1",
		"AWS_ENDPOINT_URL":      "http://localhost:34567",
	}
	got := map[string]string{}
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			got[kv[:i]] = kv[i+1:]
		}
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("env[%s] = %q, want %q", k, got[k], v)
		}
	}
	if got["PATH"] != "/usr/bin" {
		t.Errorf("base environment not preserved: PATH = %q", got["PATH"])
	}
	// An inherited AWS_ENDPOINT_URL must not win over the one we resolved: it
	// would silently send the request somewhere else.
	for _, kv := range env {
		if kv == "AWS_ENDPOINT_URL=http://stale" {
			t.Error("inherited AWS_ENDPOINT_URL survived; the resolved endpoint must win")
		}
	}

	// No region in the spec means no opinion from slctl — the user's own aws
	// config decides, rather than slctl inventing a region.
	st.Region = ""
	for _, kv := range awsEnv(nil, st, "AKIA", "s3cret", "") {
		if strings.HasPrefix(kv, "AWS_DEFAULT_REGION=") {
			t.Errorf("unset region produced %q", kv)
		}
		if strings.HasPrefix(kv, "AWS_ENDPOINT_URL=") {
			t.Errorf("empty endpoint produced %q", kv)
		}
	}
}
