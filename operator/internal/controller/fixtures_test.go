/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

// Fixture names shared by this package's tests. The values are arbitrary; what
// matters is that one cluster, pod, site or address has one spelling across
// the fixtures that refer to it.
const (
	testClusterName = "slapd"
	testNamespace   = "slaptain"
	// testE2ENamespace is the namespace the e2e suite deploys into, for
	// fixtures that mirror what it produces.
	testE2ENamespace  = "slaptain-testing"
	testTLSSecretName = "slapd-tls"
	testHeadlessSvc   = "slapd-headless"
	testClusterDomain = "cluster.local"

	testDBName    = "example-db"
	testDefaultDB = "default"
	// testAccesslogRDN is the accesslog database naming the "default" data
	// database (ADR-019); testAccesslogDN appends the throwaway suffix the
	// startup-only comparison tests use, where the suffix carries no meaning.
	testAccesslogRDN = "cn=accesslog-" + testDefaultDB
	testAccesslogDN  = testAccesslogRDN + ",dc=x"
	testDB2          = "db2"
	testSuffix       = "dc=example,dc=org"

	testPod0   = "slapd-0"
	testPod1   = "slapd-1"
	testPod2   = "slapd-2"
	testROPod0 = "slapd-readonly-0"

	testConfigPVC0    = "config-slapd-0"
	testDataPVC0      = "data-slapd-0"
	testAccesslogPVC0 = "accesslog-slapd-0"

	testPodIP1 = "198.51.100.1"
	testPodIP2 = "198.51.100.2"
	testPodIP3 = "198.51.100.3"

	testSite1 = "site-1"
	testSite2 = "site-2"
	testSite3 = "site-3"
	testSiteA = "site-a"
	testSiteB = "site-b"
	testSiteC = "site-c"
	// testSite9 is a site name no fixture mesh declares.
	testSite9 = "site-9"

	// testPeerBindSecret is the bind-password Secret of the external peer
	// testSite2 in the RID peer-resolution fixtures.
	testPeerBindSecret = "site-2-repl"
)

// Index specifications as slapd writes them in olcDbIndex.
const (
	testIndexObjectClassEq = "objectClass eq"
	testIndexEntryCSNEq    = "entryCSN eq"
)

// testConvergedMessage is the message of a fixture ReplicationConverged=True
// condition.
const testConvergedMessage = "all local pods report identical contextCSN"

// Table-case inputs and names several tables share.
const (
	// testBlank is whitespace-only input, which must read as unset.
	testBlank = "   "

	testCaseEmpty     = "empty"
	testCaseBothEmpty = "both empty"
)
