package cmd

import (
	"context"
	"encoding/json"
	"fmt"
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

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
	k8scli "github.com/chuck-chuck-chuck-net/slaptain/operator/internal/cli/k8s"
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
	Name      string `json:"name"`
	URI       string `json:"uri"`
	Connected bool   `json:"connected"`
	LastError string `json:"lastError,omitempty"`
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
	configError    string
	err            string
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
	Use:           "inspect [name]",
	Short:         "Inspect and verify a SlapdCluster",
	SilenceUsage:  true,
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
		result := inspectAndVerify(ctx, coreClient, config, sc, configPW)

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

func inspectAndVerify(ctx context.Context, coreClient kubernetes.Interface, config *rest.Config, sc *ldapv1alpha1.SlapdCluster, configPW string) inspectJSON {
	result := inspectJSON{
		Name:      sc.Name,
		Namespace: sc.Namespace,
	}

	// Gather state from all pods (one port-forward + LDAP session per pod)
	var rwPods, roPods []podState

	for _, pn := range podNames(sc.Name, sc.Spec.Replicas) {
		ps := gatherPodState(ctx, coreClient, config, sc, pn, configPW)
		rwPods = append(rwPods, ps)
		result.Pods = append(result.Pods, ps.toJSON())
	}
	for _, pn := range roPodNames(sc.Name, sc.Spec.ReadReplicas) {
		ps := gatherPodState(ctx, coreClient, config, sc, pn, configPW)
		roPods = append(roPods, ps)
		result.Pods = append(result.Pods, ps.toJSON())
	}

	// Populate external peer info
	for _, ep := range sc.Spec.Replication.ExternalPeers {
		epi := externalPeerInfo{Name: ep.Name, URI: ep.URI}
		for _, eps := range sc.Status.ExternalPeerStatuses {
			if eps.Name == ep.Name {
				epi.Connected = eps.Connected
				epi.LastError = eps.LastError
				break
			}
		}
		result.ExternalPeers = append(result.ExternalPeers, epi)
	}

	// Run checks against gathered state
	result.Checks = runChecks(sc, rwPods, roPods)

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

	localPort, cancel, err := k8scli.PortForward(ctx, coreClient, config, sc.Namespace, podName, 1024)
	if err != nil {
		ps.err = fmt.Sprintf("port-forward: %v", err)
		return ps
	}
	defer cancel()

	addr := fmt.Sprintf("localhost:%d", localPort)

	conn, err := ldap.Dial("tcp", addr)
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

	// contextCSN (anonymous)
	csnResult, err := conn.Search(ldap.NewSearchRequest(
		sc.Spec.LDAP.Domain, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 5, false,
		"(objectClass=*)", []string{"contextCSN"}, nil,
	))
	if err == nil && len(csnResult.Entries) > 0 {
		ps.contextCSN = csnResult.Entries[0].GetEqualFoldAttributeValues("contextCSN")
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
				dbFilter := fmt.Sprintf("(&(objectClass=olcMdbConfig)(olcSuffix=%s))", sc.Spec.LDAP.Domain)
				dbResult, err := configConn.Search(ldap.NewSearchRequest(
					"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases, 0, 5, false,
					dbFilter, []string{"olcSyncRepl", "olcMultiProvider"}, nil,
				))
				if err != nil {
					ps.configError = fmt.Sprintf("cn=config search: %v", err)
				} else if len(dbResult.Entries) == 0 {
					ps.configError = fmt.Sprintf("no olcMdbConfig with olcSuffix=%s found", sc.Spec.LDAP.Domain)
				} else {
					entry := dbResult.Entries[0]
					ps.syncRepl = entry.GetEqualFoldAttributeValues("olcSyncRepl")
					if vals := entry.GetEqualFoldAttributeValues("olcMultiProvider"); len(vals) > 0 {
						ps.multiProvider = vals[0]
					}
				}
			}
		}
	}

	return ps
}

// ── Consistency checks ────────────────────────────────────────────────────────

func runChecks(sc *ldapv1alpha1.SlapdCluster, rwPods, roPods []podState) []checkResult {
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

	// ── Bootstrap ──
	if sc.Status.BootstrapComplete {
		check("bootstrap", "pass", "complete")
	} else {
		check("bootstrap", "fail", "not complete")
	}

	// Remaining checks require replication
	if !sc.Spec.Replication.Enabled {
		return checks
	}

	// ── namingContexts ──
	ncOK := true
	var ncIssues []string
	for _, ps := range rwPods {
		if ps.err != "" {
			continue
		}
		hasAccesslog, hasData := false, false
		for _, nc := range ps.namingContexts {
			if nc == "cn=accesslog" {
				hasAccesslog = true
			}
			if nc == sc.Spec.LDAP.Domain {
				hasData = true
			}
		}
		if !hasAccesslog {
			ncOK = false
			ncIssues = append(ncIssues, fmt.Sprintf("%s: missing cn=accesslog", ps.name))
		}
		if !hasData {
			ncOK = false
			ncIssues = append(ncIssues, fmt.Sprintf("%s: missing %s", ps.name, sc.Spec.LDAP.Domain))
		}
	}
	for _, ps := range roPods {
		if ps.err != "" {
			continue
		}
		for _, nc := range ps.namingContexts {
			if nc == "cn=accesslog" {
				ncOK = false
				ncIssues = append(ncIssues, fmt.Sprintf("%s: RO pod should not have cn=accesslog", ps.name))
			}
		}
	}
	if ncOK {
		check("naming-contexts", "pass", "RW: accesslog+data, RO: data only")
	} else {
		check("naming-contexts", "fail", strings.Join(ncIssues, "; "))
	}

	// ── contextCSN convergence ──
	allPods := append(rwPods, roPods...)
	var csnSets []string
	csnPodMap := make(map[string][]string)
	// Track newest CSN timestamp per pod for lag calculation
	podNewest := make(map[string]time.Time)
	for _, ps := range allPods {
		if ps.err != "" || len(ps.contextCSN) == 0 {
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
		check("csn-convergence", "warn", "no contextCSN data available")
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
	externalPeerCount := len(sc.Spec.Replication.ExternalPeers)
	expectedRW := int(sc.Spec.Replicas-1) + externalPeerCount
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
		epOK := true
		var epIssues []string
		for _, ep := range sc.Status.ExternalPeerStatuses {
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
				fmt.Sprintf("all %d external peers connected", len(sc.Spec.Replication.ExternalPeers)))
		} else {
			check("external-peers", "fail", strings.Join(epIssues, "; "))
		}

		// ── External peer syncrepl stanzas present on each RW pod ──
		epStanzaOK := true
		var epStanzaIssues []string
		for _, ep := range sc.Spec.Replication.ExternalPeers {
			for _, ps := range rwPods {
				if ps.err != "" {
					continue
				}
				found := false
				for _, sr := range ps.syncRepl {
					if strings.Contains(sr, ep.URI) {
						found = true
						break
					}
				}
				if !found {
					epStanzaOK = false
					epStanzaIssues = append(epStanzaIssues,
						fmt.Sprintf("%s: missing stanza for %s (%s)", ps.name, ep.Name, ep.URI))
				}
			}
		}
		if epStanzaOK {
			check("external-syncrepl", "pass",
				fmt.Sprintf("all RW pods have stanzas for all %d external peers", len(sc.Spec.Replication.ExternalPeers)))
		} else {
			check("external-syncrepl", "fail", strings.Join(epStanzaIssues, "; "))
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
				// Classify stanzas into in-cluster and external
				var inCluster, external []string
				for _, sr := range pod.SyncRepl {
					isExternal := false
					for _, ep := range result.ExternalPeers {
						if strings.Contains(sr, ep.URI) {
							label := fmt.Sprintf("[%s] ", ep.Name)
							if len(sr) > 100 {
								sr = sr[:100] + "..."
							}
							external = append(external, label+sr)
							isExternal = true
							break
						}
					}
					if !isExternal {
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
				if len(external) > 0 {
					fmt.Println("    syncRepl (external):")
					for _, sr := range external {
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
				status := "connected"
				if !ep.Connected {
					status = "disconnected"
					if ep.LastError != "" {
						status += " (" + ep.LastError + ")"
					}
				}
				fmt.Printf("    %-20s %s  %s\n", ep.Name, ep.URI, status)
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
