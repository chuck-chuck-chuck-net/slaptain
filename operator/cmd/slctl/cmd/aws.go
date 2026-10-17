package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
	k8scli "github.com/chuck-chuck-chuck-net/slaptain/operator/internal/cli/k8s"
)

// ── Storage source ────────────────────────────────────────────────────────────

// storageSource names the custom resource a `slctl aws` invocation reads its S3
// storage spec from. It is always explicit: slctl does not scan the namespace
// for something that looks right.
//
// This is deliberately LESS clever than `slctl ldapsearch`, and the asymmetry is
// the point. A namespace has one SlapdCluster to talk to, so discovering it is
// discovering a fact. Four kinds carry an S3StorageSpec and they routinely
// disagree — a scheduled backup writing one bucket, a restore reading another —
// so "the obvious one" is not a well-defined notion, and a command that picks
// one would have a blast radius that depends on what else happens to be in the
// namespace. Naming the object costs one flag and removes the whole question.
type storageSource struct {
	Kind string // canonical kind, e.g. "SlapdBackup"
	Name string
}

// storageKindAliases maps what a user may type to the canonical kind. The short
// forms are the CRD short names, so they match `kubectl get sb` muscle memory.
var storageKindAliases = map[string]string{
	"slapdbackup":          "SlapdBackup",
	"sb":                   "SlapdBackup",
	"slapdscheduledbackup": "SlapdScheduledBackup",
	"ssb":                  "SlapdScheduledBackup",
	"slapddatabase":        "SlapdDatabase",
	"sd":                   "SlapdDatabase",
	"slapdrestore":         "SlapdRestore",
	"sr":                   "SlapdRestore",
}

func knownStorageKinds() string {
	seen := map[string][]string{}
	for alias, canonical := range storageKindAliases {
		seen[canonical] = append(seen[canonical], alias)
	}
	out := make([]string, 0, len(seen))
	for canonical, aliases := range seen {
		sort.Slice(aliases, func(i, j int) bool { return len(aliases[i]) > len(aliases[j]) })
		out = append(out, fmt.Sprintf("%s (%s)", canonical, strings.Join(aliases, "|")))
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func parseStorageSource(raw string) (storageSource, error) {
	if strings.TrimSpace(raw) == "" {
		return storageSource{}, fmt.Errorf(
			"--from is required: name the resource whose S3 storage spec to use, "+
				"e.g. --from slapdbackup/demo-backup-1. Kinds: %s", knownStorageKinds())
	}
	parts := strings.Split(raw, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return storageSource{}, fmt.Errorf("--from %q must be <kind>/<name>, e.g. slapdbackup/demo-backup-1", raw)
	}
	kind, ok := storageKindAliases[strings.ToLower(parts[0])]
	if !ok {
		return storageSource{}, fmt.Errorf("--from %q: unknown kind %q. Kinds: %s", raw, parts[0], knownStorageKinds())
	}
	return storageSource{Kind: kind, Name: parts[1]}, nil
}

// resolveStorage fetches the named resource and returns the S3 storage spec it
// carries. The two bootstrap-shaped kinds hold it behind an optional source, so
// "exists but names no S3 source" is its own, reportable state.
func resolveStorage(
	ctx context.Context,
	k8sClient client.Client,
	ns string,
	src storageSource,
) (*ldapv1alpha1.S3StorageSpec, error) {
	key := types.NamespacedName{Namespace: ns, Name: src.Name}
	notFound := func(err error) error {
		return fmt.Errorf("%s %q in namespace %q: %w", src.Kind, src.Name, ns, err)
	}

	switch src.Kind {
	case "SlapdBackup":
		var obj ldapv1alpha1.SlapdBackup
		if err := k8sClient.Get(ctx, key, &obj); err != nil {
			return nil, notFound(err)
		}
		return &obj.Spec.Storage, nil

	case "SlapdScheduledBackup":
		var obj ldapv1alpha1.SlapdScheduledBackup
		if err := k8sClient.Get(ctx, key, &obj); err != nil {
			return nil, notFound(err)
		}
		return &obj.Spec.Storage, nil

	case "SlapdDatabase":
		var obj ldapv1alpha1.SlapdDatabase
		if err := k8sClient.Get(ctx, key, &obj); err != nil {
			return nil, notFound(err)
		}
		if obj.Spec.BootstrapFrom == nil || obj.Spec.BootstrapFrom.S3 == nil {
			return nil, fmt.Errorf("SlapdDatabase %q has no spec.bootstrapFrom.s3 — it carries no S3 storage spec", src.Name)
		}
		return &obj.Spec.BootstrapFrom.S3.Storage, nil

	case "SlapdRestore":
		var obj ldapv1alpha1.SlapdRestore
		if err := k8sClient.Get(ctx, key, &obj); err != nil {
			return nil, notFound(err)
		}
		if obj.Spec.Source.S3 == nil {
			return nil, fmt.Errorf("SlapdRestore %q has no spec.source.s3 — it restores from a SlapdBackup; "+
				"point --from at that backup instead", src.Name)
		}
		return &obj.Spec.Source.S3.Storage, nil
	}
	return nil, fmt.Errorf("unhandled kind %q", src.Kind)
}

// ── Endpoint ──────────────────────────────────────────────────────────────────

// endpointPlan says how to reach the S3 endpoint from the machine slctl runs on.
//
// A storage spec's endpoint is written for the pods that use it, so it is
// routinely a cluster-internal Service DNS name — which does not resolve here.
// Classifying it is the one piece of real work this command does.
type endpointPlan struct {
	URL       string // endpoint for aws; "" means the AWS default endpoint
	Forward   bool   // the endpoint is in-cluster: port-forward and rewrite the URL
	Service   string
	Namespace string
	Port      int
	Scheme    string
}

func planEndpoint(raw, defaultNS string) (endpointPlan, error) {
	if strings.TrimSpace(raw) == "" {
		return endpointPlan{}, nil // real AWS S3
	}

	u, err := url.Parse(raw)
	if err != nil {
		return endpointPlan{}, fmt.Errorf("endpoint %q: %w", raw, err)
	}
	// A bare host with no scheme is not something to guess at: picking http vs
	// https wrong against a real store either fails or sends a credential in
	// clear. The CRD documents the field as a URL.
	if u.Scheme != "http" && u.Scheme != "https" {
		return endpointPlan{}, fmt.Errorf(
			"endpoint %q must be a URL with an http:// or https:// scheme", raw)
	}

	host := u.Hostname()
	port := 0
	if p := u.Port(); p != "" {
		if port, err = strconv.Atoi(p); err != nil {
			return endpointPlan{}, fmt.Errorf("endpoint %q: bad port %q", raw, p)
		}
	} else if u.Scheme == "https" {
		port = 443
	} else {
		port = 80
	}

	svc, ns, inCluster := clusterServiceHost(host, defaultNS)
	if !inCluster {
		return endpointPlan{URL: raw}, nil
	}
	return endpointPlan{
		Forward:   true,
		Service:   svc,
		Namespace: ns,
		Port:      port,
		Scheme:    u.Scheme,
	}, nil
}

// clusterServiceHost decides whether a hostname names a Service in the cluster.
//
// Two shapes count, and only two: a single label (no dots — not resolvable as a
// public name, so it can only mean a Service in the same namespace), and
// `<svc>.<ns>.svc[.<cluster domain>]`, which is the form the operator writes.
// `<svc>.<ns>` is deliberately NOT treated as in-cluster: it is indistinguishable
// from an ordinary two-label public host like `s3.example`.
func clusterServiceHost(host, defaultNS string) (svc, ns string, ok bool) {
	labels := strings.Split(host, ".")
	if len(labels) == 1 {
		return labels[0], defaultNS, true
	}
	if len(labels) >= 3 && labels[2] == "svc" {
		return labels[0], labels[1], true
	}
	return "", "", false
}

// resolveServicePod finds a pod behind svcName and the port on it that serves
// the Service's port, so the forward lands where the Service would have sent it.
func resolveServicePod(
	ctx context.Context,
	coreClient kubernetes.Interface,
	ns, svcName string,
	svcPort int,
) (string, int, error) {
	svc, err := coreClient.CoreV1().Services(ns).Get(ctx, svcName, metav1.GetOptions{})
	if err != nil {
		return "", 0, fmt.Errorf("get Service %s/%s: %w", ns, svcName, err)
	}
	if len(svc.Spec.Selector) == 0 {
		return "", 0, fmt.Errorf("cannot port-forward Service %s/%s: it has no selector "+
			"(an ExternalName or manually managed Service)", ns, svcName)
	}

	var target *corev1.ServicePort
	for i := range svc.Spec.Ports {
		if int(svc.Spec.Ports[i].Port) == svcPort {
			target = &svc.Spec.Ports[i]
			break
		}
	}
	if target == nil {
		var have []string
		for _, p := range svc.Spec.Ports {
			have = append(have, strconv.Itoa(int(p.Port)))
		}
		return "", 0, fmt.Errorf("no port %d on Service %s/%s (it serves %s) — "+
			"the storage spec's endpoint port does not match the Service",
			svcPort, ns, svcName, strings.Join(have, ", "))
	}

	var sel []string
	for k, v := range svc.Spec.Selector {
		sel = append(sel, k+"="+v)
	}
	sort.Strings(sel)
	pods, err := coreClient.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: strings.Join(sel, ",")})
	if err != nil {
		return "", 0, fmt.Errorf("list pods for Service %s/%s: %w", ns, svcName, err)
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		ready := false
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if !ready {
			continue
		}
		podPort, err := containerPort(pod, target)
		if err != nil {
			return "", 0, err
		}
		return pod.Name, podPort, nil
	}
	return "", 0, fmt.Errorf("no Ready pod behind Service %s/%s", ns, svcName)
}

// containerPort resolves a ServicePort's targetPort against one pod — a number
// is used as-is, a name is looked up in the pod's containers, and an unset
// targetPort means the Service port itself (the Kubernetes default).
func containerPort(pod *corev1.Pod, sp *corev1.ServicePort) (int, error) {
	switch {
	case sp.TargetPort.StrVal != "":
		for _, c := range pod.Spec.Containers {
			for _, p := range c.Ports {
				if p.Name == sp.TargetPort.StrVal {
					return int(p.ContainerPort), nil
				}
			}
		}
		return 0, fmt.Errorf("pod %s has no port named %q (the Service's targetPort)", pod.Name, sp.TargetPort.StrVal)
	case sp.TargetPort.IntVal != 0:
		return int(sp.TargetPort.IntVal), nil
	default:
		return int(sp.Port), nil
	}
}

// ── Child environment ─────────────────────────────────────────────────────────

// awsEnv builds the environment for the aws child process.
//
// Credentials go here and never into argv: an argument vector is readable by
// anyone who can see /proc, and it is what lands in shell history when a user
// copies the line --verbose printed. Values slctl resolved win over inherited
// ones — an AWS_ENDPOINT_URL or AWS_SESSION_TOKEN already in the environment
// would otherwise silently send the request somewhere else, or outrank the
// static credentials we just read from the Secret.
func awsEnv(base []string, st ldapv1alpha1.S3StorageSpec, accessKey, secretKey, endpointURL string) []string {
	drop := map[string]bool{
		"AWS_ACCESS_KEY_ID":     true,
		"AWS_SECRET_ACCESS_KEY": true,
		"AWS_SESSION_TOKEN":     true,
	}
	if endpointURL != "" {
		drop["AWS_ENDPOINT_URL"] = true
	}
	if st.Region != "" {
		drop["AWS_REGION"] = true
		drop["AWS_DEFAULT_REGION"] = true
	}

	var out []string
	for _, kv := range base {
		if i := strings.IndexByte(kv, '='); i > 0 && drop[kv[:i]] {
			continue
		}
		out = append(out, kv)
	}

	out = append(out,
		"AWS_ACCESS_KEY_ID="+accessKey,
		"AWS_SECRET_ACCESS_KEY="+secretKey,
	)
	if endpointURL != "" {
		out = append(out, "AWS_ENDPOINT_URL="+endpointURL)
	}
	if st.Region != "" {
		// Both spellings: AWS_REGION outranks AWS_DEFAULT_REGION in the v2 CLI,
		// so setting only the latter would let an inherited AWS_REGION win.
		out = append(out, "AWS_DEFAULT_REGION="+st.Region, "AWS_REGION="+st.Region)
	}
	return out
}

// ── Command ───────────────────────────────────────────────────────────────────

type awsFlags struct {
	from           string
	verbose        bool
	showPassword   bool
	redactPassword bool
}

// splitAWSArgs pulls slctl's own flags out of the raw args, leaving everything
// else for the aws CLI — the same contract as splitLDAPArgs, so `slctl aws s3 ls
// s3://bucket/ --recursive` needs no `--` separator.
func splitAWSArgs(raw []string, f *awsFlags) []string {
	stringFlags := map[string]*string{
		"--from":       &f.from,
		"--kubeconfig": &kubeconfig,
		"--context":    &kubeContext,
		"--namespace":  &namespace,
		"-n":           &namespace,
	}
	boolFlags := map[string]*bool{
		"--verbose":         &f.verbose,
		"--show-password":   &f.showPassword,
		"--redact-password": &f.redactPassword,
	}

	return splitOwnFlags(raw, stringFlags, boolFlags)
}

func runAWS(raw []string) error {
	if _, err := exec.LookPath("aws"); err != nil {
		return fmt.Errorf("aws not found in PATH (install the AWS CLI): %w", err)
	}

	var f awsFlags
	extraArgs := splitAWSArgs(raw, &f)

	src, err := parseStorageSource(f.from)
	if err != nil {
		return err
	}

	ctx := context.Background()
	k8sClient, coreClient, restCfg, ns, err := initClient()
	if err != nil {
		return err
	}

	storage, err := resolveStorage(ctx, k8sClient, ns, src)
	if err != nil {
		return err
	}

	accessKey, secretKey, err := readS3Credentials(ctx, coreClient, ns, storage.CredentialsSecretName)
	if err != nil {
		return err
	}

	plan, err := planEndpoint(storage.Endpoint, ns)
	if err != nil {
		return err
	}

	endpointURL := plan.URL
	var pfLine string
	if plan.Forward {
		podName, podPort, err := resolveServicePod(ctx, coreClient, plan.Namespace, plan.Service, plan.Port)
		if err != nil {
			return err
		}
		localPort, cancel, err := k8scli.PortForward(ctx, coreClient, restCfg, plan.Namespace, podName, podPort)
		if err != nil {
			return fmt.Errorf("port-forward to %s/%s: %w", plan.Namespace, podName, err)
		}
		defer cancel()
		endpointURL = fmt.Sprintf("%s://localhost:%d", plan.Scheme, localPort)
		pfLine = formatPortForwardCmd(kubectlGlobalArgs(), plan.Namespace, podName, localPort, podPort)

		// Through a forward the endpoint is localhost, which no certificate
		// issued for the Service name can match. Say so rather than quietly
		// turning verification off.
		if plan.Scheme == "https" && !storage.InsecureTLS {
			fmt.Fprintf(os.Stderr,
				"! %s is https, but a port-forward makes the endpoint localhost — TLS verification will fail.\n"+
					"  Set spec.storage.insecureTLS, or reach the endpoint directly.\n", storage.Endpoint)
		}
	}

	args := extraArgs
	if storage.InsecureTLS {
		// No env equivalent exists, so this one flag is prepended to argv. aws
		// takes global options before the service name.
		args = append([]string{"--no-verify-ssl"}, args...)
	}

	env := awsEnv(os.Environ(), *storage, accessKey, secretKey, endpointURL)

	if f.verbose {
		if pfLine != "" {
			fmt.Fprintf(os.Stderr, "→ %s\n", pfLine)
		}
		fmt.Fprintf(os.Stderr, "→ %s\n",
			formatAWSInvocation(storage, accessKey, secretKey, endpointURL, args,
				shouldRedactPassword(f.showPassword, f.redactPassword)))
	} else {
		where := endpointURL
		if where == "" {
			where = "AWS S3"
		}
		fmt.Fprintf(os.Stderr, "→ aws via %s (%s/%s, bucket %s)\n", where, src.Kind, src.Name, storage.Bucket)
	}

	c := exec.CommandContext(ctx, "aws", args...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Env = env

	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	return nil
}

// readS3Credentials reads the two keys the operator's backup Jobs use, so the
// command fails the same way they do when a Secret is wrong.
func readS3Credentials(
	ctx context.Context,
	coreClient kubernetes.Interface,
	ns, secretName string,
) (string, string, error) {
	if secretName == "" {
		return "", "", fmt.Errorf("storage spec names no credentialsSecretName")
	}
	sec, err := coreClient.CoreV1().Secrets(ns).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return "", "", fmt.Errorf("get Secret %s/%s: %w", ns, secretName, err)
	}
	id := string(sec.Data["access-key-id"])
	key := string(sec.Data["secret-access-key"])
	if id == "" || key == "" {
		return "", "", fmt.Errorf("the Secret %s/%s must carry non-empty %q and %q keys",
			ns, secretName, "access-key-id", "secret-access-key")
	}
	return id, key, nil
}

// formatAWSInvocation renders a copy-pasteable line for --verbose, with the
// environment prefixed the way a shell would take it. The access key ID is an
// identity and is printed; the secret access key is masked unless the user
// asked for it, the same rule --verbose applies to an LDAP bind password.
func formatAWSInvocation(
	st *ldapv1alpha1.S3StorageSpec,
	accessKey, secretKey, endpointURL string,
	args []string,
	redact bool,
) string {
	if redact {
		secretKey = "<secret-access-key>"
	}
	parts := []string{
		"AWS_ACCESS_KEY_ID=" + shellQuote(accessKey),
		"AWS_SECRET_ACCESS_KEY=" + singleQuote(secretKey),
	}
	if endpointURL != "" {
		parts = append(parts, "AWS_ENDPOINT_URL="+shellQuote(endpointURL))
	}
	if st.Region != "" {
		parts = append(parts, "AWS_DEFAULT_REGION="+shellQuote(st.Region))
	}
	parts = append(parts, "aws")
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

var awsCmd = &cobra.Command{
	Use:                "aws --from <kind>/<name> [aws-args...]",
	Short:              "Run the local aws CLI against a slaptain backup store",
	DisableFlagParsing: true,
	SilenceUsage:       true,
	Long: `Wraps the system aws CLI with the bucket's endpoint and credentials taken
from a slaptain resource, so no throwaway in-cluster client pod is needed.

The storage spec is ALWAYS named explicitly with --from. Four kinds carry one
and they routinely disagree, so slctl does not guess which you meant:

  --from slapdbackup/<name>           spec.storage
  --from slapdscheduledbackup/<name>  spec.storage
  --from slapddatabase/<name>         spec.bootstrapFrom.s3.storage
  --from slapdrestore/<name>          spec.source.s3.storage

Credentials come from that spec's credentialsSecretName (keys access-key-id and
secret-access-key) and are passed to aws in its environment, never on its
command line. When the endpoint is a cluster-internal Service DNS name — the
normal case, since it is written for the pods — slctl port-forwards to it and
rewrites the endpoint to localhost. An external endpoint is used as-is, and an
empty one means real AWS S3.

Everything slctl does not recognise is passed to aws verbatim.

Examples:
  slctl aws --from sb/demo-backup-1 s3 ls s3://slaptain-backups/ --recursive --human-readable
  slctl aws --from sb/demo-backup-1 s3 cp s3://slaptain-backups/<key> - | gzip -dc | head
  slctl aws --from ssb/nightly s3api head-object --bucket slaptain-backups --key <key>

Flags:
  --from <kind>/<name>  required; the resource carrying the S3 storage spec
  --verbose             print the port-forward and the aws invocation (secret masked)
  --show-password       print the secret access key in clear
  -n, --namespace       target namespace`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if isHelp(args) {
			return cmd.Help()
		}
		return runAWS(args)
	},
}

func init() {
	rootCmd.AddCommand(awsCmd)
}
