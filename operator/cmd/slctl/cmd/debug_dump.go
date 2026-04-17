package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	Use:   "debug-dump <name>",
	Short: "Collect debug artifacts for a SlapdCluster",
	Long:  "Gather CR YAML, pod logs, LDAP state, and Kubernetes resources into a timestamped directory.",
	Args:  cobra.ExactArgs(1),
	RunE:  runDebugDump,
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

	var warnings int
	writeFile := func(filename, data string) {
		path := filepath.Join(dir, filename)
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "  WARN: write %s: %v\n", path, err)
			warnings++
		} else {
			fmt.Printf("  %s\n", filename)
		}
	}
	collect := func(filename string, fn func() (string, error)) {
		data, err := fn()
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "  WARN: %s: %v\n", filename, err)
			warnings++
			return
		}
		if data == "" {
			return
		}
		writeFile(filename, data)
	}
	// collectOptional silently skips errors (e.g. previous logs when no crash).
	collectOptional := func(filename string, fn func() (string, error)) {
		data, err := fn()
		if err != nil || data == "" {
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

		// LDAP queries via port-forward + go-ldap
		collectLDAPArtifacts(ctx, coreClient, config, ns, pn, sc, configPW, dir, cmd, &warnings)
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
			LabelSelector: "app.kubernetes.io/name=slaptain-operator",
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
		for _, line := range strings.Split(logs, "\n") {
			if strings.Contains(line, name) || strings.Contains(line, "Reconcil") {
				filtered = append(filtered, line)
			}
		}
		return strings.Join(filtered, "\n"), nil
	})

	fmt.Printf("\nDone. %d artifacts collected", countFiles(dir))
	if warnings > 0 {
		fmt.Printf(" (%d warnings)", warnings)
	}
	fmt.Println()
	return nil
}

func collectLDAPArtifacts(ctx context.Context, coreClient kubernetes.Interface, config *rest.Config, ns, podName string, sc *ldapv1alpha1.SlapdCluster, configPW, dir string, cmd *cobra.Command, warnings *int) {
	localPort, cancel, err := k8scli.PortForward(ctx, coreClient, config, ns, podName, 1024)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "  WARN: port-forward %s: %v\n", podName, err)
		*warnings++
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
			fmt.Fprintf(cmd.ErrOrStderr(), "  WARN: write %s: %v\n", path, err)
			*warnings++
		} else {
			fmt.Printf("  %s\n", filename)
		}
	}

	// rootDSE (anonymous)
	conn, err := ldap.Dial("tcp", addr)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "  WARN: LDAP dial %s: %v\n", podName, err)
		*warnings++
		return
	}

	rootDSE, err := conn.Search(ldap.NewSearchRequest(
		"", ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 5, false,
		"(objectClass=*)", []string{"*", "+"}, nil,
	))
	if err == nil && len(rootDSE.Entries) > 0 {
		write(fmt.Sprintf("rootdse-%s.txt", podName), formatLDAPEntry(rootDSE.Entries[0]))
	}

	// contextCSN
	csnResult, err := conn.Search(ldap.NewSearchRequest(
		sc.Spec.LDAP.Domain, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 5, false,
		"(objectClass=*)", []string{"contextCSN"}, nil,
	))
	if err == nil && len(csnResult.Entries) > 0 {
		write(fmt.Sprintf("contextcsn-%s.txt", podName), formatLDAPEntry(csnResult.Entries[0]))
	}
	conn.Close()

	// cn=config queries (config admin)
	// The data DB index varies: {1}mdb without accesslog, {2}mdb with accesslog.
	// Search by olcSuffix to find the right entry.
	if configPW != "" {
		configConn, err := ldap.Dial("tcp", addr)
		if err == nil {
			if err := configConn.Bind("cn=admin,cn=config", configPW); err == nil {
				dbFilter := fmt.Sprintf("(&(objectClass=olcMdbConfig)(olcSuffix=%s))", sc.Spec.LDAP.Domain)

				// syncrepl + multiProvider
				syncResult, err := configConn.Search(ldap.NewSearchRequest(
					"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases, 0, 5, false,
					dbFilter, []string{"olcSyncRepl", "olcMultiProvider"}, nil,
				))
				if err == nil && len(syncResult.Entries) > 0 {
					write(fmt.Sprintf("syncrepl-%s.txt", podName), formatLDAPEntry(syncResult.Entries[0]))
				}

				// ACLs
				aclResult, err := configConn.Search(ldap.NewSearchRequest(
					"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases, 0, 5, false,
					dbFilter, []string{"olcAccess"}, nil,
				))
				if err == nil && len(aclResult.Entries) > 0 {
					write(fmt.Sprintf("acls-%s.txt", podName), formatLDAPEntry(aclResult.Entries[0]))
				}
			}
			configConn.Close()
		}
	}
}

func formatLDAPEntry(entry *ldap.Entry) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "dn: %s\n", entry.DN)
	for _, attr := range entry.Attributes {
		for _, val := range attr.Values {
			fmt.Fprintf(&sb, "%s: %s\n", attr.Name, val)
		}
	}
	return sb.String()
}

func getPodLogs(ctx context.Context, coreClient kubernetes.Interface, ns, podName, container string, previous bool) (string, error) {
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
	defer stream.Close()
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
