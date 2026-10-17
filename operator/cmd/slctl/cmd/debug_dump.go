package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
	k8scli "github.com/chuck-chuck-chuck-net/slaptain/operator/internal/cli/k8s"
)

var debugDumpCmd = &cobra.Command{
	Use:               "debug-dump <name>",
	Short:             "Collect debug artifacts for a SlapdCluster",
	Long:              "Gather CR YAML, pod logs, LDAP state, and Kubernetes resources into a timestamped directory.",
	Args:              cobra.ExactArgs(1),
	RunE:              runDebugDump,
	ValidArgsFunction: completeSlapdClusterNames,
}

func init() {
	rootCmd.AddCommand(debugDumpCmd)
}

func runDebugDump(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	k8sClient, coreClient, config, ns, err := initClient()
	if err != nil {
		return err
	}

	name := args[0]
	sc := &ldapv1alpha1.SlapdCluster{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, sc); err != nil {
		return fmt.Errorf("get SlapdCluster %s/%s: %w", ns, name, err)
	}

	configPW, _ := readSecretKey(ctx, coreClient, ns, name+"-config-password", "root-password")

	// Create output directory
	ts := time.Now().Format("20060102-150405")
	dir := fmt.Sprintf("slctl-debug-%s-%s", name, ts)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	fmt.Printf("Collecting debug artifacts in %s/\n", dir)

	warnings := &dumpWarnings{w: cmd.ErrOrStderr()}
	writeFile := func(filename, data string) {
		path := filepath.Join(dir, filename)
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			warnings.warnf("write %s: %v", path, err)
		} else {
			fmt.Printf("  %s\n", filename)
		}
	}
	collect := func(filename string, fn func() (string, error)) {
		data, err := fn()
		if err != nil {
			warnings.warnf("%s: %v", filename, err)
		}
		// Whatever was read is kept even alongside an error: a log stream that
		// failed to close after a complete read is still the evidence wanted.
		if data == "" {
			return
		}
		writeFile(filename, data)
	}
	// collectOptional silently skips errors (e.g. previous logs when no crash),
	// keeping whatever was read, as collect does.
	collectOptional := func(filename string, fn func() (string, error)) {
		data, _ := fn()
		if data == "" {
			return
		}
		writeFile(filename, data)
	}

	// 1. CR YAML
	collect("slapdcluster.yaml", func() (string, error) {
		b, err := yaml.Marshal(sc)
		return string(b), err
	})

	// 2. CR status JSON
	collect("status.json", func() (string, error) {
		b, err := json.MarshalIndent(sc.Status, "", "  ")
		return string(b), err
	})

	// 3. Per-pod artifacts
	allPods := append(podNames(name, sc.Status.Replicas), roPodNames(name, sc.Status.ReadOnlyReplicas)...)
	for _, podName := range allPods {
		pn := podName

		collect(fmt.Sprintf("pod-%s.yaml", pn), func() (string, error) {
			pod, err := coreClient.CoreV1().Pods(ns).Get(ctx, pn, metav1.GetOptions{})
			if err != nil {
				return "", err
			}
			b, err := yaml.Marshal(pod)
			return string(b), err
		})

		collect(fmt.Sprintf("logs-%s.txt", pn), func() (string, error) {
			return getPodLogs(ctx, coreClient, ns, pn, "slapd", false)
		})

		collectOptional(fmt.Sprintf("logs-%s-previous.txt", pn), func() (string, error) {
			return getPodLogs(ctx, coreClient, ns, pn, "slapd", true)
		})

		collectOptional(fmt.Sprintf("logs-%s-init.txt", pn), func() (string, error) {
			return getPodLogs(ctx, coreClient, ns, pn, "init", false)
		})

		// LDAP queries via port-forward + go-ldap (with timeout to avoid hanging)
		done := make(chan struct{}, 1)
		go func() {
			collectLDAPArtifacts(ctx, coreClient, config, ns, pn, sc, configPW, dir, warnings)
			done <- struct{}{}
		}()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			warnings.warnf("LDAP queries for %s timed out after 15s", pn)
		}
	}

	// 4. Services
	collect("services.yaml", func() (string, error) {
		svcs, err := coreClient.CoreV1().Services(ns).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/managed-by=slapdcluster-controller",
		})
		if err != nil {
			return "", err
		}
		b, err := yaml.Marshal(svcs)
		return string(b), err
	})

	// 5. StatefulSets
	collect("statefulsets.yaml", func() (string, error) {
		stsList, err := coreClient.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/managed-by=slapdcluster-controller",
		})
		if err != nil {
			return "", err
		}
		b, err := yaml.Marshal(stsList)
		return string(b), err
	})

	// 6. PVCs
	collect("pvcs.yaml", func() (string, error) {
		pvcList, err := coreClient.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app.kubernetes.io/instance=%s", name),
		})
		if err != nil {
			return "", err
		}
		b, err := yaml.Marshal(pvcList)
		return string(b), err
	})

	// 7. Secrets (names and keys only)
	collect("secrets.txt", func() (string, error) {
		secrets, err := coreClient.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return "", err
		}
		var lines []string
		for _, s := range secrets.Items {
			if strings.Contains(s.Name, name) {
				var keys []string
				for k := range s.Data {
					keys = append(keys, k)
				}
				lines = append(lines, fmt.Sprintf("%s  keys=[%s]", s.Name, strings.Join(keys, ", ")))
			}
		}
		return strings.Join(lines, "\n"), nil
	})

	// 8. Events
	collect("events.txt", func() (string, error) {
		events, err := coreClient.CoreV1().Events(ns).List(ctx, metav1.ListOptions{
			FieldSelector: fmt.Sprintf("involvedObject.name=%s", name),
		})
		if err != nil {
			return "", err
		}
		var lines []string
		for _, e := range events.Items {
			lines = append(lines, fmt.Sprintf("%s  %s  %s  %s: %s",
				e.LastTimestamp.Format(time.RFC3339),
				e.Type, e.Reason,
				e.InvolvedObject.Kind, e.Message))
		}
		return strings.Join(lines, "\n"), nil
	})

	// 9. Operator logs
	collect("operator-logs.txt", func() (string, error) {
		pods, err := coreClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/name=slaptain",
		})
		if err != nil || len(pods.Items) == 0 {
			pods, err = coreClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{
				LabelSelector: "control-plane=controller-manager",
			})
			if err != nil || len(pods.Items) == 0 {
				return "", fmt.Errorf("operator pod not found")
			}
		}
		opPod := pods.Items[0]
		logs, err := getPodLogs(ctx, coreClient, opPod.Namespace, opPod.Name, "manager", false)
		if err != nil {
			return "", err
		}
		var filtered []string
		for line := range strings.SplitSeq(logs, "\n") {
			if strings.Contains(line, name) || strings.Contains(line, "Reconcil") {
				filtered = append(filtered, line)
			}
		}
		return strings.Join(filtered, "\n"), nil
	})

	fmt.Printf("\nDone. %d artifacts collected", countFiles(dir))
	if n := warnings.count(); n > 0 {
		fmt.Printf(" (%d warnings)", n)
	}
	fmt.Println()
	return warnings.writeErr()
}

// dumpWarnings counts the problems a dump runs into and reports each on stderr.
// It is shared with the per-pod LDAP goroutine, which can outlive its timeout,
// hence the lock. A warning that cannot even be written is the one problem it
// cannot report, so the first such failure becomes the command's error.
type dumpWarnings struct {
	w io.Writer

	mu  sync.Mutex
	n   int
	err error
}

func (d *dumpWarnings) warnf(format string, a ...any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.n++
	if _, err := fmt.Fprintf(d.w, "  WARN: "+format+"\n", a...); err != nil && d.err == nil {
		d.err = fmt.Errorf("write warning: %w", err)
	}
}

func (d *dumpWarnings) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.n
}

func (d *dumpWarnings) writeErr() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

func collectLDAPArtifacts(ctx context.Context, coreClient kubernetes.Interface, config *rest.Config, ns, podName string, sc *ldapv1alpha1.SlapdCluster, configPW, dir string, warnings *dumpWarnings) {
	// Per-pod timeout to avoid hanging on unresponsive pods
	podCtx, podCancel := context.WithTimeout(ctx, 15*time.Second)
	defer podCancel()

	localPort, cancel, err := k8scli.PortForward(podCtx, coreClient, config, ns, podName, 1024)
	if err != nil {
		warnings.warnf("port-forward %s: %v", podName, err)
		return
	}
	defer cancel()

	addr := fmt.Sprintf("localhost:%d", localPort)
	write := func(filename, content string) {
		if content == "" {
			return
		}
		path := filepath.Join(dir, filename)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			warnings.warnf("write %s: %v", path, err)
		} else {
			fmt.Printf("  %s\n", filename)
		}
	}

	// rootDSE (anonymous)
	dialer := net.Dialer{Timeout: 5 * time.Second}
	netConn, err := dialer.DialContext(podCtx, "tcp", addr)
	if err != nil {
		warnings.warnf("LDAP dial %s: %v", podName, err)
		return
	}
	conn := ldap.NewConn(netConn, false)
	conn.Start()

	rootDSE, err := conn.Search(ldap.NewSearchRequest(
		"", ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 5, false,
		"(objectClass=*)", []string{"*", "+"}, nil,
	))
	if err == nil && len(rootDSE.Entries) > 0 {
		write(fmt.Sprintf("rootdse-%s.txt", podName), formatLDAPEntry(rootDSE.Entries[0]))
	}

	// Discover data suffix from rootDSE namingContexts
	var namingContexts []string
	if rootDSE != nil && len(rootDSE.Entries) > 0 {
		namingContexts = rootDSE.Entries[0].GetEqualFoldAttributeValues("namingContexts")
	}
	// contextCSN, for EVERY data suffix. A vector belongs to one database
	// (ADR-008 amendment 2026-09-14), so dumping only the first naming
	// context leaves every other database absent from the post-mortem —
	// exactly when a dump is the only evidence left.
	var csnDump []string
	for _, nc := range namingContexts {
		if strings.HasPrefix(strings.ToLower(nc), "cn=") {
			continue // accesslog / internal DBs
		}
		csnResult, err := conn.Search(ldap.NewSearchRequest(
			nc, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 5, false,
			"(objectClass=*)", []string{"contextCSN"}, nil,
		))
		if err != nil {
			csnDump = append(csnDump, fmt.Sprintf("# %s: %v\n", nc, err))
			continue
		}
		if len(csnResult.Entries) == 0 {
			csnDump = append(csnDump, fmt.Sprintf("# %s: no entry returned\n", nc))
			continue
		}
		csnDump = append(csnDump, formatLDAPEntry(csnResult.Entries[0]))
	}
	if len(csnDump) > 0 {
		write(fmt.Sprintf("contextcsn-%s.txt", podName), strings.Join(csnDump, "\n"))
	}
	if err := conn.Close(); err != nil {
		warnings.warnf("close LDAP connection to %s: %v", podName, err)
	}

	// cn=config queries (config admin)
	// The data DB index varies: {1}mdb without accesslog, {2}mdb with accesslog.
	// Search for all olcMdbConfig entries to find data DBs.
	if configPW != "" {
		configConn, err := ldap.DialURL("ldap://" + addr)
		if err == nil {
			if err := configConn.Bind("cn=admin,cn=config", configPW); err == nil {
				// Find all data DB entries (non-internal suffixes)
				dbResult, err := configConn.Search(ldap.NewSearchRequest(
					"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases, 0, 0, false,
					"(objectClass=olcMdbConfig)", []string{"olcSuffix", "olcSyncRepl", "olcMultiProvider", "olcAccess"}, nil,
				))
				if err == nil {
					// EVERY data database, not the first one. A bundle is
					// handed to someone who cannot re-run the query, so a dump
					// that silently covers db1 and omits db2 sends them looking
					// in the wrong place — the same first-data-DB blind spot
					// `inspect` carried until 2026-09-28. os.WriteFile
					// truncates, so the entries are concatenated into one file
					// per pod rather than written per database.
					var dump strings.Builder
					for _, entry := range dbResult.Entries {
						suffix := ""
						if vals := entry.GetEqualFoldAttributeValues("olcSuffix"); len(vals) > 0 {
							suffix = vals[0]
						}
						if strings.HasPrefix(suffix, "cn=") {
							continue
						}
						if dump.Len() > 0 {
							dump.WriteString("\n")
						}
						dump.WriteString(formatLDAPEntry(entry))
					}
					// One entry carries both olcSyncRepl and olcAccess, so the
					// two files are the same dump under the two names a reader
					// would look for.
					write(fmt.Sprintf("syncrepl-%s.txt", podName), dump.String())
					write(fmt.Sprintf("acls-%s.txt", podName), dump.String())
				}
			}
			if err := configConn.Close(); err != nil {
				warnings.warnf("close cn=config connection to %s: %v", podName, err)
			}
		}
	}
}

// formatLDAPEntry renders an entry for the dump bundle. Values pass through
// redactCredentials: a debug-dump is written to be handed to someone else, and
// syncrepl stanzas carry the replication password in the clear.
func formatLDAPEntry(entry *ldap.Entry) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "dn: %s\n", entry.DN)
	for _, attr := range entry.Attributes {
		for _, val := range attr.Values {
			fmt.Fprintf(&sb, "%s: %s\n", attr.Name, redactCredentials(val))
		}
	}
	return sb.String()
}

func getPodLogs(ctx context.Context, coreClient kubernetes.Interface, ns, podName, container string, previous bool) (logs string, err error) {
	tailLines := int64(1000)
	opts := &corev1.PodLogOptions{
		Container: container,
		Previous:  previous,
		TailLines: &tailLines,
	}
	req := coreClient.CoreV1().Pods(ns).GetLogs(podName, opts)
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", err
	}
	defer func() {
		if cerr := stream.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("close log stream: %w", cerr))
		}
	}()
	b, err := io.ReadAll(stream)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func countFiles(dir string) int {
	entries, _ := os.ReadDir(dir)
	return len(entries)
}
