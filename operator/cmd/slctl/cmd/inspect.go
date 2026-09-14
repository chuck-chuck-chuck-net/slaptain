package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
	k8scli "github.com/chuck-chuck-chuck-net/slaptain/operator/internal/cli/k8s"
	"github.com/chuck-chuck-chuck-net/slaptain/operator/internal/suffixprobe"
)

// ── Flags ─────────────────────────────────────────────────────────────────────

var shortOutput bool

// ── JSON types ────────────────────────────────────────────────────────────────

type inspectJSON struct {
	Name          string             `json:"name"`
	Namespace     string             `json:"namespace"`
	Pods          []podJSON          `json:"pods"`
	ExternalPeers []externalPeerInfo `json:"externalPeers,omitempty"`
	Checks        []checkResult      `json:"checks"`
	Summary       string             `json:"summary"`
}

type externalPeerInfo struct {
	Name                string   `json:"name"`
	URI                 string   `json:"uri"`
	PodAddresses        []string `json:"podAddresses,omitempty"`
	DiscoveredAddresses []string `json:"discoveredAddresses,omitempty"`
	ReplicasPerPeer     int32    `json:"replicasPerPeer,omitempty"`
	Connected           *bool    `json:"connected,omitempty"` // nil = not tested (Multus/discovery)
	LastError           string   `json:"lastError,omitempty"`
	Multus              bool     `json:"multus,omitempty"` // true = podAddresses or discovery peer
	ReplicationState    string   `json:"replicationState,omitempty"`
	LagSeconds          string   `json:"lagSeconds,omitempty"`
}

type podJSON struct {
	Name             string   `json:"name"`
	Ready            bool     `json:"ready"`
	Phase            string   `json:"phase"`
	NamingContexts   []string `json:"namingContexts,omitempty"`
	ContextCSN       []string `json:"contextCSN,omitempty"`
	SyncRepl         []string `json:"syncRepl,omitempty"`
	MultiProvider    string   `json:"multiProvider,omitempty"`
	ConfigQueryError string   `json:"configQueryError,omitempty"`
	Error            string   `json:"error,omitempty"`
}

type checkResult struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "pass", "warn", "fail"
	Detail string `json:"detail,omitempty"`
}

// ── Internal pod state (shared by display + checks) ──────────────────────────

type podState struct {
	name           string
	ready          bool
	phase          string
	namingContexts []string
	contextCSN     []string
	syncRepl       []string
	multiProvider  string
	// dbs is every olcMdbConfig entry this pod serves — data and accesslog
	// alike — with each data DB's accesslog overlay resolved onto it. The
	// per-database accesslog checks need the whole set (ADR-019); syncRepl and
	// multiProvider above stay as they are, the first data DB's values, because
	// the per-pod display is written against them.
	dbs []observedDB
	// suffixEntries is each data suffix's base entry as this pod shows it —
	// ordinary visibility, glue verdict (ManageDSAIT), entryUUID (ADR-025).
	suffixEntries []suffixObservation
	configError   string
	err           string
}

func (ps podState) toJSON() podJSON {
	return podJSON{
		Name:             ps.name,
		Ready:            ps.ready,
		Phase:            ps.phase,
		NamingContexts:   ps.namingContexts,
		ContextCSN:       ps.contextCSN,
		SyncRepl:         ps.syncRepl,
		MultiProvider:    ps.multiProvider,
		ConfigQueryError: ps.configError,
		Error:            ps.err,
	}
}

// ── Command ───────────────────────────────────────────────────────────────────

var inspectCmd = &cobra.Command{
	Use:          "inspect [name]",
	Short:        "Inspect and verify a SlapdCluster",
	SilenceUsage: true,
	Long: `Query each pod's LDAP instance and run consistency checks.

Shows per-pod detail (namingContexts, contextCSN, syncRepl, multiProvider)
followed by automated verification checks (replica counts, CSN convergence,
topology correctness, etc.).

Use --short to show only the checks summary (for CI/pipelines).
Exits non-zero if any check fails.`,
	Args:              cobra.MaximumNArgs(1),
	RunE:              runInspect,
	ValidArgsFunction: completeSlapdClusterNames,
}

func init() {
	rootCmd.AddCommand(inspectCmd)
	inspectCmd.Flags().BoolVar(&shortOutput, "short", false, "show only checks summary, skip per-pod detail")
}

func runInspect(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	k8sClient, coreClient, config, ns, err := initClient()
	if err != nil {
		return err
	}

	targets, err := resolveTargets(ctx, k8sClient, args, ns)
	if err != nil {
		return err
	}

	var jsonResults []inspectJSON
	hasFail := false

	for i, key := range targets {
		sc := &ldapv1alpha1.SlapdCluster{}
		if err := k8sClient.Get(ctx, key, sc); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Error fetching %s/%s: %v\n", key.Namespace, key.Name, err)
			continue
		}

		configPW, _ := readSecretKey(ctx, coreClient, key.Namespace, sc.Name+"-config-password", "root-password")

		// SlapdDatabase CRs are the intent side of the per-database accesslog
		// checks (ADR-019): which databases exist, and which use delta-syncrepl.
		dbs, err := clusterDatabases(ctx, k8sClient, sc)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %v\n", err)
		}

		result := inspectAndVerify(ctx, coreClient, config, sc, dbs, configPW)

		if jsonOutput {
			jsonResults = append(jsonResults, result)
		} else {
			if i > 0 {
				fmt.Println()
			}
			printInspectResult(result)
		}

		for _, c := range result.Checks {
			if c.Status == "fail" {
				hasFail = true
			}
		}
	}

	if jsonOutput {
		out, _ := json.MarshalIndent(jsonResults, "", "  ")
		fmt.Println(string(out))
	}

	if hasFail {
		return fmt.Errorf("verification failed")
	}
	return nil
}

// ── Core logic ────────────────────────────────────────────────────────────────

func inspectAndVerify(ctx context.Context, coreClient kubernetes.Interface, config *rest.Config, sc *ldapv1alpha1.SlapdCluster, dbs []dbIdentity, configPW string) inspectJSON {
	result := inspectJSON{
		Name:      sc.Name,
		Namespace: sc.Namespace,
	}

	// Gather state from all pods (one port-forward + LDAP session per pod)
	var rwPods, roPods []podState

	for _, pn := range podNames(sc.Name, sc.Spec.Replicas) {
		if !jsonOutput && !shortOutput {
			fmt.Fprintf(os.Stderr, "  Inspecting %s...\n", pn)
		}
		ps := gatherPodStateWithTimeout(ctx, coreClient, config, sc, pn, configPW)
		rwPods = append(rwPods, ps)
		result.Pods = append(result.Pods, ps.toJSON())
	}
	for _, pn := range roPodNames(sc.Name, sc.Spec.ReadReplicas) {
		if !jsonOutput && !shortOutput {
			fmt.Fprintf(os.Stderr, "  Inspecting %s...\n", pn)
		}
		ps := gatherPodStateWithTimeout(ctx, coreClient, config, sc, pn, configPW)
		roPods = append(roPods, ps)
		result.Pods = append(result.Pods, ps.toJSON())
	}

	// Populate external peer info
	for _, ep := range sc.Spec.Replication.ExternalPeers {
		epi := externalPeerInfo{Name: ep.Name}
		if ep.Discovery != nil {
			epi.Multus = true
		} else if len(ep.PodAddresses) > 0 {
			epi.Multus = true
			epi.PodAddresses = ep.PodAddresses
		} else {
			epi.URI = ep.URI
		}
		// Look up status entry for this peer.
		for _, eps := range sc.Status.ExternalPeerStatuses {
			if eps.Name != ep.Name {
				continue
			}
			c := eps.Connected
			epi.Connected = &c
			epi.LastError = eps.LastError
			epi.DiscoveredAddresses = eps.DiscoveredAddresses
			epi.ReplicationState = string(eps.ReplicationState)
			epi.LagSeconds = eps.LagSeconds
			break
		}
		// Set URI display for Multus/discovery modes.
		rpp := int32(1)
		if ep.ReplicasPerPeer != nil && *ep.ReplicasPerPeer > 1 {
			rpp = *ep.ReplicasPerPeer
		}
		epi.ReplicasPerPeer = rpp
		fanout := ""
		if ep.Discovery != nil {
			n := int32(len(epi.DiscoveredAddresses))
			if n > 0 && rpp > 1 {
				selected := rpp
				if selected > n {
					selected = n
				}
				fanout = fmt.Sprintf(", rpp=%d/%d", selected, n)
			}
			if len(epi.DiscoveredAddresses) > 0 {
				epi.URI = fmt.Sprintf("%d pod(s) via discovery%s", len(epi.DiscoveredAddresses), fanout)
			} else {
				epi.URI = "discovery (no addresses yet)"
			}
		} else if len(ep.PodAddresses) > 0 {
			n := int32(len(ep.PodAddresses))
			if rpp > 1 {
				selected := rpp
				if selected > n {
					selected = n
				}
				fanout = fmt.Sprintf(", rpp=%d/%d", selected, n)
			}
			epi.URI = fmt.Sprintf("%d pod(s) via Multus%s", len(ep.PodAddresses), fanout)
		}
		result.ExternalPeers = append(result.ExternalPeers, epi)
	}

	// Run checks against gathered state
	result.Checks = runChecks(sc, dbs, rwPods, roPods)

	pass, warn, fail := 0, 0, 0
	for _, c := range result.Checks {
		switch c.Status {
		case "pass":
			pass++
		case "warn":
			warn++
		case "fail":
			fail++
		}
	}
	result.Summary = fmt.Sprintf("%d passed, %d warnings, %d failed", pass, warn, fail)

	return result
}

// ── Pod state gathering ───────────────────────────────────────────────────────

const perPodTimeout = 15 * time.Second

// gatherPodStateWithTimeout wraps gatherPodState with a hard timeout.
// The SPDY port-forward dialer doesn't respect context cancellation,
// so we run the gather in a goroutine and abandon it if it takes too long.
func gatherPodStateWithTimeout(ctx context.Context, coreClient kubernetes.Interface, config *rest.Config, sc *ldapv1alpha1.SlapdCluster, podName, configPW string) podState {
	// Quick check: if the pod isn't ready, skip without starting a goroutine
	pod, err := coreClient.CoreV1().Pods(sc.Namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return podState{name: podName, err: fmt.Sprintf("pod not found: %v", err)}
	}
	if !isPodReady(pod) {
		return podState{name: podName, phase: string(pod.Status.Phase), err: "pod not ready"}
	}

	ch := make(chan podState, 1)
	go func() {
		ch <- gatherPodState(ctx, coreClient, config, sc, podName, configPW)
	}()

	select {
	case ps := <-ch:
		return ps
	case <-time.After(perPodTimeout):
		return podState{
			name:  podName,
			phase: string(pod.Status.Phase),
			ready: true,
			err:   fmt.Sprintf("timed out after %s (pod ready but LDAP unresponsive)", perPodTimeout),
		}
	}
}

func gatherPodState(ctx context.Context, coreClient kubernetes.Interface, config *rest.Config, sc *ldapv1alpha1.SlapdCluster, podName, configPW string) podState {
	ps := podState{name: podName}

	pod, err := coreClient.CoreV1().Pods(sc.Namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		ps.err = fmt.Sprintf("pod not found: %v", err)
		return ps
	}
	ps.phase = string(pod.Status.Phase)
	ps.ready = isPodReady(pod)

	if !ps.ready {
		ps.err = "pod not ready"
		return ps
	}

	// Per-pod timeout to avoid hanging on unresponsive pods
	podCtx, podCancel := context.WithTimeout(ctx, perPodTimeout)
	defer podCancel()

	localPort, cancel, err := k8scli.PortForward(podCtx, coreClient, config, sc.Namespace, podName, 1024)
	if err != nil {
		ps.err = fmt.Sprintf("port-forward: %v", err)
		return ps
	}
	defer cancel()

	addr := fmt.Sprintf("localhost:%d", localPort)

	dialer := net.Dialer{Timeout: 5 * time.Second}
	netConn, err := dialer.DialContext(podCtx, "tcp", addr)
	if err != nil {
		ps.err = fmt.Sprintf("LDAP dial: %v", err)
		return ps
	}
	conn := ldap.NewConn(netConn, false)
	conn.Start()
	if err != nil {
		ps.err = fmt.Sprintf("LDAP dial: %v", err)
		return ps
	}
	defer conn.Close()

	// rootDSE (anonymous)
	rootDSE, err := conn.Search(ldap.NewSearchRequest(
		"", ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 5, false,
		"(objectClass=*)", []string{"namingContexts"}, nil,
	))
	if err == nil && len(rootDSE.Entries) > 0 {
		ps.namingContexts = rootDSE.Entries[0].GetEqualFoldAttributeValues("namingContexts")
	}

	// Discover data suffix from namingContexts (skip cn= internal DBs)
	dataSuffix := dataSuffixFromNamingContexts(ps.namingContexts)

	// contextCSN (anonymous) — only if we found a data suffix
	if dataSuffix != "" {
		csnResult, err := conn.Search(ldap.NewSearchRequest(
			dataSuffix, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 5, false,
			"(objectClass=*)", []string{"contextCSN"}, nil,
		))
		if err == nil && len(csnResult.Entries) > 0 {
			ps.contextCSN = csnResult.Entries[0].GetEqualFoldAttributeValues("contextCSN")
		}
	}

	// Suffix-entry health (ADR-025): for every data suffix, probe the base
	// entry — ordinary search first (a real entry is visible, a glue is not),
	// then ManageDSAIT (RFC 3296) when hidden, which reveals glue entries
	// (slapd hides them at the frontend, not via ACLs). Anonymous, like the
	// contextCSN probe above; per-pod DISAGREEMENT is the check's signal, so
	// an ACL that hides the entry uniformly only downgrades the check to warn.
	for _, nc := range ps.namingContexts {
		if strings.HasPrefix(strings.ToLower(nc), "cn=") {
			continue // accesslog / internal DBs journal, they have no seed identity
		}
		ps.suffixEntries = append(ps.suffixEntries, suffixprobe.Probe(conn, nc))
	}

	// cn=config (config admin bind)
	if configPW == "" {
		if sc.Spec.Replication.Enabled {
			ps.configError = "config password not available"
		}
	} else {
		configConn, err := ldap.Dial("tcp", addr)
		if err != nil {
			ps.configError = fmt.Sprintf("dial for cn=config: %v", err)
		} else {
			defer configConn.Close()
			if err := configConn.Bind("cn=admin,cn=config", configPW); err != nil {
				ps.configError = fmt.Sprintf("cn=config bind failed: %v", err)
			} else {
				// Search for all olcMdbConfig entries to find syncRepl/multiProvider
				dbResult, err := configConn.Search(ldap.NewSearchRequest(
					"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases, 0, 0, false,
					"(objectClass=olcMdbConfig)",
					[]string{"olcSuffix", "olcSyncRepl", "olcMultiProvider", "olcDbDirectory"}, nil,
				))
				if err != nil {
					ps.configError = fmt.Sprintf("cn=config search: %v", err)
				} else {
					multiProvider := make(map[string]string, len(dbResult.Entries))
					for _, entry := range dbResult.Entries {
						suffix := ""
						if vals := entry.GetEqualFoldAttributeValues("olcSuffix"); len(vals) > 0 {
							suffix = vals[0]
						}
						ps.dbs = append(ps.dbs, observedDB{
							DN:       entry.DN,
							Suffix:   suffix,
							Dir:      entry.GetEqualFoldAttributeValue("olcDbDirectory"),
							SyncRepl: entry.GetEqualFoldAttributeValues("olcSyncRepl"),
						})
						multiProvider[entry.DN] = entry.GetEqualFoldAttributeValue("olcMultiProvider")
					}
					// The per-pod display shows one data DB's stanzas and
					// multiProvider; the accesslog DBs (and any other cn=…
					// internal database) are not it.
					for _, od := range ps.dbs {
						if strings.HasPrefix(od.Suffix, "cn=") {
							continue
						}
						ps.syncRepl = od.SyncRepl
						ps.multiProvider = multiProvider[od.DN]
						break
					}
					// Resolve each data DB's accesslog overlay onto it. The
					// overlay hangs under the database it logs, and slapd's {N}
					// ordering prefix makes its own RDN unpredictable, so it is
					// attributed by parent DN (ADR-019 R5: olcAccessLogDB must
					// agree with logbase and the log's olcSuffix).
					ovResult, err := configConn.Search(ldap.NewSearchRequest(
						"cn=config", ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
						"(objectClass=olcAccessLogConfig)", []string{"olcAccessLogDB"}, nil,
					))
					if err != nil {
						ps.configError = fmt.Sprintf("cn=config accesslog overlay search: %v", err)
					} else {
						for _, ov := range ovResult.Entries {
							logDB := ov.GetEqualFoldAttributeValue("olcAccessLogDB")
							for i := range ps.dbs {
								if dnHasParent(ov.DN, ps.dbs[i].DN) {
									ps.dbs[i].AccessLogDB = logDB
									break
								}
							}
						}
					}
				}
			}
		}
	}

	return ps
}

// ── Consistency checks ────────────────────────────────────────────────────────

func runChecks(sc *ldapv1alpha1.SlapdCluster, dbs []dbIdentity, rwPods, roPods []podState) []checkResult {
	var checks []checkResult
	check := func(name, status, detail string) {
		checks = append(checks, checkResult{Name: name, Status: status, Detail: detail})
	}

	// ── RW replica count ──
	if sc.Status.ReadyReplicas == sc.Spec.Replicas {
		check("rw-replica-count", "pass",
			fmt.Sprintf("%d/%d ready", sc.Status.ReadyReplicas, sc.Spec.Replicas))
	} else {
		check("rw-replica-count", "fail",
			fmt.Sprintf("%d/%d ready (want %d)", sc.Status.ReadyReplicas, sc.Status.Replicas, sc.Spec.Replicas))
	}

	// ── RO replica count ──
	if sc.Spec.ReadReplicas > 0 {
		if sc.Status.ReadOnlyReadyReplicas == sc.Spec.ReadReplicas {
			check("ro-replica-count", "pass",
				fmt.Sprintf("%d/%d ready", sc.Status.ReadOnlyReadyReplicas, sc.Spec.ReadReplicas))
		} else {
			check("ro-replica-count", "fail",
				fmt.Sprintf("%d/%d ready (want %d)", sc.Status.ReadOnlyReadyReplicas, sc.Status.ReadOnlyReplicas, sc.Spec.ReadReplicas))
		}
	}

	// ── Pod readiness ──
	allReady := true
	var notReady []string
	for _, ps := range append(rwPods, roPods...) {
		if !ps.ready {
			allReady = false
			notReady = append(notReady, ps.name)
		}
	}
	if allReady {
		check("pod-readiness", "pass", "all pods running and ready")
	} else {
		check("pod-readiness", "fail", fmt.Sprintf("not ready: %s", strings.Join(notReady, ", ")))
	}

	// ── Bootstrap (inferred from phase) ──
	if sc.Status.Phase == ldapv1alpha1.PhaseRunning {
		check("bootstrap", "pass", "cluster phase is Running")
	} else {
		check("bootstrap", "warn", fmt.Sprintf("cluster phase is %s", sc.Status.Phase))
	}

	// Remaining checks require replication
	if !sc.Spec.Replication.Enabled {
		return checks
	}

	// ── namingContexts + per-database accesslog consistency (ADR-019) ──
	rwStates := accesslogStates(rwPods, false)
	roStates := accesslogStates(roPods, true)
	allStates := append(rwStates, roStates...)
	checks = append(checks,
		checkNamingContexts(dbs, allStates),
		checkAccesslogConsistency(dbs, allStates, externalPeerNeedles(sc)),
	)

	// ── Suffix-entry health: glue detection + entryUUID agreement (ADR-025) ──
	// RW and RO pods alike: a consumer that initial-synced from a glued
	// provider replicates the glue (measured live 2026-09-13).
	suffixPods := append(append([]podState{}, rwPods...), roPods...)
	checks = append(checks,
		checkSuffixVisibility(dbs, suffixPods),
		checkSuffixUUIDAgreement(dbs, suffixPods),
	)

	// ── contextCSN convergence ──
	allPods := append(rwPods, roPods...)
	var csnSets []string
	csnPodMap := make(map[string][]string)
	// Pods that are reachable but report NO contextCSN at all. When other pods
	// DO report one, this is not "no data yet" — it is a pod that never
	// completed an initial sync (e.g. the provider it consumes from has no
	// working syncprov). Treating these as skippable once green-lit a cluster
	// where 2 of 3 RW pods had never synced ("all 1 pods report identical
	// CSN") — see reconcile-loop-fixes.md 2026-07-15.
	var csnMissing []string
	// Track newest CSN timestamp per pod for lag calculation
	podNewest := make(map[string]time.Time)
	for _, ps := range allPods {
		if ps.err != "" {
			continue
		}
		if len(ps.contextCSN) == 0 {
			csnMissing = append(csnMissing, ps.name)
			continue
		}
		normalized := normalizeCSN(ps.contextCSN)
		csnSets = append(csnSets, normalized)
		csnPodMap[normalized] = append(csnPodMap[normalized], ps.name)
		// Find newest CSN timestamp for this pod
		var newest time.Time
		for _, csn := range ps.contextCSN {
			if t, err := parseCSNTime(csn); err == nil && t.After(newest) {
				newest = t
			}
		}
		if !newest.IsZero() {
			podNewest[ps.name] = newest
		}
	}
	if len(csnSets) == 0 {
		// Nobody reports a contextCSN: legitimate for a fresh/empty database.
		check("csn-convergence", "warn", "no contextCSN data available")
	} else if len(csnMissing) > 0 {
		// Some pods report, some don't: the data exists but never reached the
		// silent pods. Broken replication, not an empty cluster.
		sort.Strings(csnMissing)
		check("csn-convergence", "fail",
			fmt.Sprintf("%d of %d pods report no contextCSN at all (never synced?): %s",
				len(csnMissing), len(csnMissing)+len(csnSets), strings.Join(csnMissing, ", ")))
	} else {
		unique := uniqueStrings(csnSets)
		if len(unique) == 1 {
			check("csn-convergence", "pass",
				fmt.Sprintf("all %d pods report identical CSN", len(csnSets)))
		} else {
			// Find newest and oldest across all pods to compute lag
			var newest, oldest time.Time
			var newestPod, oldestPod string
			for pod, t := range podNewest {
				if newest.IsZero() || t.After(newest) {
					newest = t
					newestPod = pod
				}
				if oldest.IsZero() || t.Before(oldest) {
					oldest = t
					oldestPod = pod
				}
			}
			lag := newest.Sub(oldest)

			var groups []string
			for _, csn := range unique {
				pods := csnPodMap[csn]
				groups = append(groups, fmt.Sprintf("[%s]", strings.Join(pods, ",")))
			}
			sort.Strings(groups)

			detail := fmt.Sprintf("%d distinct CSN vectors (%s behind %s by %s): %s",
				len(unique), oldestPod, newestPod, formatDuration(lag),
				strings.Join(groups, " vs "))
			check("csn-convergence", "warn", detail)
		}
	}

	// ── syncRepl stanza count ──
	// Each ExternalPeer contributes 1 stanza (URI mode) or min(replicasPerPeer,
	// len(addresses)) stanzas in podAddresses / discovery modes.
	externalStanzaCount := 0
	for _, ep := range sc.Spec.Replication.ExternalPeers {
		var addrCount int
		if ep.Discovery != nil {
			for _, eps := range sc.Status.ExternalPeerStatuses {
				if eps.Name == ep.Name {
					addrCount = len(eps.DiscoveredAddresses)
					break
				}
			}
		} else if len(ep.PodAddresses) > 0 {
			addrCount = len(ep.PodAddresses)
		} else {
			externalStanzaCount++
			continue
		}
		if addrCount == 0 {
			continue
		}
		rpp := 1
		if ep.ReplicasPerPeer != nil && int(*ep.ReplicasPerPeer) > 1 {
			rpp = int(*ep.ReplicasPerPeer)
		}
		if rpp > addrCount {
			rpp = addrCount
		}
		externalStanzaCount += rpp
	}
	expectedRW := int(sc.Spec.Replicas-1) + externalStanzaCount
	expectedRO := int(sc.Spec.Replicas)

	stanzaOK := true
	var stanzaIssues []string
	for _, ps := range rwPods {
		if ps.err != "" {
			continue
		}
		if len(ps.syncRepl) != expectedRW {
			stanzaOK = false
			stanzaIssues = append(stanzaIssues,
				fmt.Sprintf("%s: %d stanzas (want %d)", ps.name, len(ps.syncRepl), expectedRW))
		}
	}
	for _, ps := range roPods {
		if ps.err != "" {
			continue
		}
		if len(ps.syncRepl) != expectedRO {
			stanzaOK = false
			stanzaIssues = append(stanzaIssues,
				fmt.Sprintf("%s: %d stanzas (want %d)", ps.name, len(ps.syncRepl), expectedRO))
		}
	}
	if stanzaOK {
		check("syncrepl-stanza-count", "pass",
			fmt.Sprintf("RW: %d each, RO: %d each", expectedRW, expectedRO))
	} else {
		check("syncrepl-stanza-count", "fail", strings.Join(stanzaIssues, "; "))
	}

	// ── syncRepl skip-self ──
	selfOK := true
	var selfIssues []string
	headlessSvc := sc.Name + "-headless"
	for _, ps := range rwPods {
		if ps.err != "" {
			continue
		}
		selfURI := fmt.Sprintf("%s.%s.", ps.name, headlessSvc)
		for _, sr := range ps.syncRepl {
			if strings.Contains(sr, selfURI) {
				selfOK = false
				selfIssues = append(selfIssues, fmt.Sprintf("%s replicates from itself", ps.name))
			}
		}
	}
	if selfOK {
		check("syncrepl-skip-self", "pass", "no RW pod replicates from itself")
	} else {
		check("syncrepl-skip-self", "fail", strings.Join(selfIssues, "; "))
	}

	// ── multiProvider on RW, absent on RO ──
	if sc.Spec.Replicas > 1 {
		mpOK := true
		var mpIssues []string
		for _, ps := range rwPods {
			if ps.err != "" {
				continue
			}
			if !strings.EqualFold(ps.multiProvider, "TRUE") {
				mpOK = false
				mpIssues = append(mpIssues,
					fmt.Sprintf("%s: multiProvider=%q (want TRUE)", ps.name, ps.multiProvider))
			}
		}
		for _, ps := range roPods {
			if ps.err != "" {
				continue
			}
			if ps.multiProvider != "" && !strings.EqualFold(ps.multiProvider, "FALSE") {
				mpOK = false
				mpIssues = append(mpIssues,
					fmt.Sprintf("%s: RO pod has multiProvider=%q", ps.name, ps.multiProvider))
			}
		}
		if mpOK {
			check("multi-provider", "pass", "TRUE on RW, absent on RO")
		} else {
			check("multi-provider", "fail", strings.Join(mpIssues, "; "))
		}
	}

	// ── RID uniqueness ──
	ridOK := true
	var ridIssues []string
	ridRe := regexp.MustCompile(`rid=(\d+)`)
	for _, ps := range append(rwPods, roPods...) {
		if ps.err != "" {
			continue
		}
		seen := make(map[string]bool)
		for _, sr := range ps.syncRepl {
			m := ridRe.FindStringSubmatch(sr)
			if len(m) < 2 {
				continue
			}
			rid := m[1]
			if seen[rid] {
				ridOK = false
				ridIssues = append(ridIssues, fmt.Sprintf("%s: duplicate RID %s", ps.name, rid))
			}
			seen[rid] = true
		}
	}
	if ridOK {
		check("rid-uniqueness", "pass", "all RIDs unique within each pod")
	} else {
		check("rid-uniqueness", "fail", strings.Join(ridIssues, "; "))
	}

	// ── External peer connectivity ──
	if len(sc.Spec.Replication.ExternalPeers) > 0 {
		// Peers using podAddresses or discovery are on a Multus replication
		// network that the operator (and slctl) cannot reach directly.
		// Only check URI-based peers for connectivity.
		var uriPeerCount, multusPeerCount, discoveryPeerCount int
		for _, ep := range sc.Spec.Replication.ExternalPeers {
			if ep.Discovery != nil {
				discoveryPeerCount++
			} else if len(ep.PodAddresses) > 0 {
				multusPeerCount++
			} else if ep.URI != "" {
				uriPeerCount++
			}
		}

		if uriPeerCount > 0 {
			epOK := true
			var epIssues []string
			for _, ep := range sc.Status.ExternalPeerStatuses {
				if len(ep.DiscoveredAddresses) > 0 {
					continue // discovery peer — skip connectivity check
				}
				if !ep.Connected {
					epOK = false
					detail := ep.Name + ": disconnected"
					if ep.LastError != "" {
						detail += " (" + ep.LastError + ")"
					}
					epIssues = append(epIssues, detail)
				}
			}
			if epOK {
				check("external-peers", "pass",
					fmt.Sprintf("all %d URI peers connected", uriPeerCount))
			} else {
				check("external-peers", "fail", strings.Join(epIssues, "; "))
			}
		}
		if discoveryPeerCount > 0 {
			// Report discovery peer status: count discovered addresses.
			var details []string
			for _, ep := range sc.Spec.Replication.ExternalPeers {
				if ep.Discovery == nil {
					continue
				}
				for _, eps := range sc.Status.ExternalPeerStatuses {
					if eps.Name == ep.Name {
						if len(eps.DiscoveredAddresses) > 0 {
							details = append(details, fmt.Sprintf("%s: %d pod(s) discovered",
								ep.Name, len(eps.DiscoveredAddresses)))
						} else {
							details = append(details, fmt.Sprintf("%s: no addresses discovered",
								ep.Name))
						}
						break
					}
				}
			}
			check("external-peers", "pass",
				fmt.Sprintf("all %d discovery peers use Multus (%s)",
					discoveryPeerCount, strings.Join(details, ", ")))
		}
		if multusPeerCount > 0 && uriPeerCount == 0 && discoveryPeerCount == 0 {
			check("external-peers", "pass",
				fmt.Sprintf("all %d external peers use Multus podAddresses (connectivity not testable from operator)",
					multusPeerCount))
		}

		// ── External peer syncrepl stanzas present on each RW pod ──
		// For each ExternalPeer, check that at least one stanza references its
		// URI (single-endpoint), podAddresses (static Multus), or
		// DiscoveredAddresses (discovery mode).
		epStanzaOK := true
		var epStanzaIssues []string
		for _, ep := range sc.Spec.Replication.ExternalPeers {
			// Build the set of strings to search for in syncrepl stanzas.
			var needles []string
			if ep.Discovery != nil {
				// Discovery mode: use resolved addresses from status.
				for _, eps := range sc.Status.ExternalPeerStatuses {
					if eps.Name == ep.Name {
						needles = eps.DiscoveredAddresses
						break
					}
				}
			} else if len(ep.PodAddresses) > 0 {
				needles = ep.PodAddresses
			} else if ep.URI != "" {
				needles = []string{ep.URI}
			}

			for _, ps := range rwPods {
				if ps.err != "" {
					continue
				}
				found := false
				for _, sr := range ps.syncRepl {
					for _, needle := range needles {
						if strings.Contains(sr, needle) {
							found = true
							break
						}
					}
					if found {
						break
					}
				}
				if !found {
					epStanzaOK = false
					epStanzaIssues = append(epStanzaIssues,
						fmt.Sprintf("%s: missing stanza for %s", ps.name, ep.Name))
				}
			}
		}
		if epStanzaOK {
			check("external-syncrepl", "pass",
				fmt.Sprintf("all RW pods have stanzas for all %d external peers", len(sc.Spec.Replication.ExternalPeers)))
		} else {
			check("external-syncrepl", "fail", strings.Join(epStanzaIssues, "; "))
		}

		// ── Cross-site CSN convergence (from operator status) ──
		csnOK := true
		var csnIssues []string
		anyCSNData := false
		for _, eps := range sc.Status.ExternalPeerStatuses {
			switch eps.ReplicationState {
			case "Synced":
				anyCSNData = true
			case "Lagging":
				anyCSNData = true
				csnOK = false
				detail := eps.Name + ": Lagging"
				if eps.LagSeconds != "" {
					detail += fmt.Sprintf(" (%ss)", eps.LagSeconds)
				}
				csnIssues = append(csnIssues, detail)
			case "PartiallyVerified":
				// The peer answered, and everything that could be read is
				// current — but at least one database yielded no evidence.
				// Surfaced rather than swallowed: a state nobody renders is a
				// state that silently reads as healthy.
				anyCSNData = true
				csnOK = false
				detail := eps.Name + ": PartiallyVerified"
				if eps.LastError != "" {
					detail += " (" + eps.LastError + ")"
				}
				csnIssues = append(csnIssues, detail)
			case "Unreachable":
				anyCSNData = true
				csnOK = false
				csnIssues = append(csnIssues, eps.Name+": Unreachable")
			}
		}
		if anyCSNData {
			if csnOK {
				check("cross-site-csn", "pass", "all external peers report Synced")
			} else {
				// Use "warn" for Lagging (data is flowing, just delayed) and
				// "fail" for Unreachable (can't reach remote pods).
				hasUnreachable := false
				for _, issue := range csnIssues {
					if strings.Contains(issue, "Unreachable") {
						hasUnreachable = true
						break
					}
				}
				if hasUnreachable {
					check("cross-site-csn", "fail", strings.Join(csnIssues, "; "))
				} else {
					check("cross-site-csn", "warn", strings.Join(csnIssues, "; "))
				}
			}
		}
	}

	return checks
}

// ── Output ────────────────────────────────────────────────────────────────────

func printInspectResult(result inspectJSON) {
	fmt.Printf("SlapdCluster: %s/%s\n", result.Namespace, result.Name)
	printSeparator()

	if !shortOutput {
		for _, pod := range result.Pods {
			fmt.Printf("\n  Pod: %s  [%s", pod.Name, pod.Phase)
			if pod.Ready {
				fmt.Print(", ready")
			}
			fmt.Println("]")

			if pod.Error != "" {
				fmt.Printf("    Error: %s\n", pod.Error)
				continue
			}

			if len(pod.NamingContexts) > 0 {
				fmt.Printf("    namingContexts: %s\n", strings.Join(pod.NamingContexts, ", "))
			}

			if len(pod.ContextCSN) > 0 {
				fmt.Println("    contextCSN:")
				for _, csn := range pod.ContextCSN {
					fmt.Printf("      %s\n", csn)
				}
			}

			if len(pod.SyncRepl) > 0 {
				// Classify stanzas into in-cluster and cross-site.
				// Match against URI (NodePort), podAddresses (static Multus),
				// or DiscoveredAddresses (discovery mode).
				var inCluster, crossSite []string
				for _, sr := range pod.SyncRepl {
					matched := false
					for _, ep := range result.ExternalPeers {
						var needles []string
						if len(ep.DiscoveredAddresses) > 0 {
							needles = ep.DiscoveredAddresses
						} else if ep.Multus {
							needles = ep.PodAddresses
						} else {
							needles = []string{ep.URI}
						}
						for _, needle := range needles {
							if needle != "" && strings.Contains(sr, needle) {
								label := fmt.Sprintf("[%s] ", ep.Name)
								if len(sr) > 100 {
									sr = sr[:100] + "..."
								}
								crossSite = append(crossSite, label+sr)
								matched = true
								break
							}
						}
						if matched {
							break
						}
					}
					if !matched {
						if len(sr) > 120 {
							sr = sr[:120] + "..."
						}
						inCluster = append(inCluster, sr)
					}
				}
				if len(inCluster) > 0 {
					fmt.Println("    syncRepl (in-cluster):")
					for _, sr := range inCluster {
						fmt.Printf("      %s\n", sr)
					}
				}
				if len(crossSite) > 0 {
					fmt.Println("    syncRepl (cross-site):")
					for _, sr := range crossSite {
						fmt.Printf("      %s\n", sr)
					}
				}
			}
			if pod.MultiProvider != "" {
				fmt.Printf("    multiProvider: %s\n", pod.MultiProvider)
			}
			if pod.ConfigQueryError != "" {
				fmt.Printf("    cn=config:    %s\n", pod.ConfigQueryError)
			}
		}

		// External peers summary
		if len(result.ExternalPeers) > 0 {
			fmt.Println("\n  External Peers:")
			for _, ep := range result.ExternalPeers {
				var replStr string
				switch ep.ReplicationState {
				case "Synced":
					replStr = "Synced"
				case "Lagging":
					if ep.LagSeconds != "" {
						replStr = fmt.Sprintf("Lagging (%ss)", ep.LagSeconds)
					} else {
						replStr = "Lagging"
					}
				case "PartiallyVerified":
					replStr = "PartiallyVerified"
					if ep.LastError != "" {
						replStr += " (" + ep.LastError + ")"
					}
				case "Unreachable":
					replStr = "Unreachable"
					if ep.LastError != "" {
						replStr += " (" + ep.LastError + ")"
					}
				default:
					if ep.Connected != nil && !*ep.Connected {
						replStr = "disconnected"
						if ep.LastError != "" {
							replStr += " (" + ep.LastError + ")"
						}
					}
				}
				if replStr != "" {
					fmt.Printf("    %-20s %s  %s\n", ep.Name, ep.URI, replStr)
				} else {
					fmt.Printf("    %-20s %s\n", ep.Name, ep.URI)
				}
			}
		}

		fmt.Println()
	}

	// Always print checks
	fmt.Println("Checks:")
	for _, c := range result.Checks {
		var symbol string
		switch c.Status {
		case "pass":
			symbol = "OK"
		case "warn":
			symbol = "WARN"
		case "fail":
			symbol = "FAIL"
		}
		fmt.Printf("  [%-4s] %-24s %s\n", symbol, c.Name, c.Detail)
	}

	fmt.Printf("\n%s\n", result.Summary)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func readSecretKey(ctx context.Context, coreClient kubernetes.Interface, ns, secretName, key string) (string, error) {
	secret, err := coreClient.CoreV1().Secrets(ns).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	val, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("key %q not found in secret %s", key, secretName)
	}
	return strings.TrimSpace(string(val)), nil
}

func isPodReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func normalizeCSN(csns []string) string {
	sorted := make([]string, len(csns))
	copy(sorted, csns)
	sort.Strings(sorted)
	return strings.Join(sorted, "|")
}

func uniqueStrings(ss []string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}

// parseCSNTime extracts the timestamp from a contextCSN value.
// Format: YYYYMMDDHHMMSS.µsZ#count#serverID#modcount
func parseCSNTime(csn string) (time.Time, error) {
	// Take everything before the first '#'
	parts := strings.SplitN(csn, "#", 2)
	if len(parts) == 0 {
		return time.Time{}, fmt.Errorf("invalid CSN: %s", csn)
	}
	ts := strings.TrimSuffix(parts[0], "Z")
	return time.Parse("20060102150405.000000", ts)
}

// dataSuffixFromNamingContexts returns the first non-internal (non cn=) naming
// context, which is the data suffix. Returns "" if none found.
func dataSuffixFromNamingContexts(contexts []string) string {
	for _, nc := range contexts {
		if !strings.HasPrefix(nc, "cn=") {
			return nc
		}
	}
	return ""
}

// formatDuration produces a human-friendly duration string.
func formatDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%.1fm", d.Minutes())
	default:
		return fmt.Sprintf("%.1fh", d.Hours())
	}
}

// ── Per-database accesslog plumbing (ADR-019) ─────────────────────────────────

// clusterDatabases reduces the cluster's SlapdDatabase CRs to the intent the
// accesslog checks verify against. WantLog mirrors the operator's own gating:
// the cluster needs an accesslog at all (NeedsAccesslog — false for a
// single-pod cluster with no external peers, and for a consumer-only one) and
// this database uses delta-syncrepl.
func clusterDatabases(ctx context.Context, k8sClient client.Client, sc *ldapv1alpha1.SlapdCluster) ([]dbIdentity, error) {
	list := &ldapv1alpha1.SlapdDatabaseList{}
	if err := k8sClient.List(ctx, list, client.InNamespace(sc.Namespace)); err != nil {
		return nil, fmt.Errorf("list SlapdDatabases in %s: %w", sc.Namespace, err)
	}
	var out []dbIdentity
	for i := range list.Items {
		sd := &list.Items[i]
		if sd.Spec.ClusterRef != sc.Name {
			continue
		}
		out = append(out, dbIdentity{
			Name:            sd.Name,
			Suffix:          sd.Spec.Suffix,
			WantLog:         sc.NeedsAccesslog() && sd.DeltaSyncEnabled(),
			ExternalLogBase: ldapv1alpha1.ExternalLogBase(sd),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// accesslogStates projects gathered pod state onto the pure checks' input.
// A pod that could not be reached is marked Skip: pod-readiness reports it, and
// the accesslog checks must not turn an unreachable pod into a config verdict.
func accesslogStates(pods []podState, ro bool) []podAccesslogState {
	out := make([]podAccesslogState, 0, len(pods))
	for _, ps := range pods {
		out = append(out, podAccesslogState{
			Name:           ps.name,
			RO:             ro,
			Skip:           ps.err != "",
			NamingContexts: ps.namingContexts,
			DBs:            ps.dbs,
		})
	}
	return out
}

// externalPeerNeedles returns the strings that identify a cross-site syncrepl
// stanza — the peer's URI, its static Multus podAddresses, or its discovered
// addresses. Same match the per-pod display uses to label stanzas cross-site.
func externalPeerNeedles(sc *ldapv1alpha1.SlapdCluster) []string {
	var needles []string
	for _, ep := range sc.Spec.Replication.ExternalPeers {
		switch {
		case ep.Discovery != nil:
			for _, eps := range sc.Status.ExternalPeerStatuses {
				if eps.Name == ep.Name {
					needles = append(needles, eps.DiscoveredAddresses...)
					break
				}
			}
		case len(ep.PodAddresses) > 0:
			needles = append(needles, ep.PodAddresses...)
		case ep.URI != "":
			needles = append(needles, ep.URI)
		}
	}
	return needles
}

// dnHasParent reports whether child is a direct-or-deeper descendant of parent,
// case-insensitively — DN comparison in cn=config is case-insensitive.
func dnHasParent(child, parent string) bool {
	return len(child) > len(parent)+1 &&
		strings.EqualFold(child[len(child)-len(parent):], parent) &&
		child[len(child)-len(parent)-1] == ','
}
