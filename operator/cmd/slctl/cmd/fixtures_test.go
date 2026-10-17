package cmd

// Names shared by the fixtures across this package's tests: one cluster in
// one namespace, its pods, its database, and the mesh it belongs to.
const (
	testNamespace   = "default"
	testClusterName = "slapd"
	testPod0        = "slapd-0"
	testPod1        = "slapd-1"
	testROPod0      = "slapd-readonly-0"
	testDBName      = "default"
	testMeshName    = "lab"
)

// demoNamespace is the namespace of the demo walkthrough (docs/demo).
const demoNamespace = "slaptain-demo"

// Peer sites, named by the letter of their index.
const (
	siteA = "site-a"
	siteB = "site-b"
	siteC = "site-c"
	siteI = "site-i"
)

// glueMarker is the word a check's detail must carry when it reports a glue
// suffix entry (ADR-025).
const glueMarker = "glue"
