package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var (
	debugPod          string
	debugImage        string
	debugPullPolicy   string
	debugTarget       string
	debugNoPrivileged bool
)

// debugFullCaps is the capability set added when the namespace permits it —
// enough for strace/gdb (SYS_PTRACE), mount/namespace inspection (SYS_ADMIN),
// and packet capture (NET_ADMIN/NET_RAW).
var debugFullCaps = []string{"SYS_PTRACE", "SYS_ADMIN", "NET_ADMIN", "NET_RAW"}

// slapdDataVolumes are the slapd PVC/emptyDir volume names worth mounting into a
// reduced-privilege debug container, in mount order. Only those present on the
// pod are mounted (RO replicas have no accesslog; a non-replicated cluster none).
var slapdDataVolumes = []string{"config", "data", "accesslog"}

type psaLevel int

const (
	psaPrivileged psaLevel = iota
	psaBaseline
	psaRestricted
)

var debugCmd = &cobra.Command{
	Use:   "debug [cluster] [-- command...]",
	Short: "Attach an ephemeral debug container (slapd-toolkit) to a slapd pod",
	Long: `debug launches an interactive ephemeral debug container alongside a running
slapd pod, using the slapd-toolkit image (ldap-utils, python3/ldap3, strace,
tcpdump, lsof, procps, iproute2, …).

The slapd container itself is distroless, rootless (UID 1024), and has a
read-only root filesystem — you cannot exec a shell into it. This command
sidesteps that without weakening it; the slapd container's own security context
is never touched. slapd shares the debug container's PID namespace (--target),
so it is PID 1 there, and its LDAP endpoint is on localhost:1024.

How much power you get is auto-detected from the namespace's PodSecurity level
(probed with a server-side dry-run):

  privileged namespace   Full power: the debug container runs as root with
                         SYS_PTRACE/SYS_ADMIN/NET_ADMIN/NET_RAW. slapd's *live*
                         filesystem is at /proc/1/root/ and 'strace -p 1' works.
  baseline namespace     Reduced: root, no added capabilities (strace/proc
                         traversal are blocked by the kernel). Instead, slapd's
                         PVCs are mounted read-only at /config, /data, /accesslog.
  restricted namespace   Further reduced: UID 1024, same read-only PVC mounts.

To get full power in a locked-down namespace, raise its PodSecurity level:
  kubectl label ns <ns> pod-security.kubernetes.io/enforce=privileged --overwrite

The toolkit image is derived from the target pod's running slapd image
(…/slapd:<tag> → …/slapd-toolkit:<tag>) — so it works even when spec.images is
omitted and the operator defaults the image; override with --image.

Examples:
  slctl debug -n slaptain-demo                          # pod-0, auto-detected power
  slctl debug -n slaptain-demo --pod readonly-0         # a read-only replica
  slctl debug -n slaptain-demo --no-privileged          # force the read-only-mount mode
  slctl debug mycluster -- ldapwhoami -H ldap://localhost:1024 -x`,
	SilenceUsage: true,
	Args:         cobra.ArbitraryArgs,
	RunE:         runDebug,
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeSlapdClusterNames(cmd, args, toComplete)
	},
}

func runDebug(cmd *cobra.Command, args []string) error {
	if _, err := exec.LookPath("kubectl"); err != nil {
		return fmt.Errorf("kubectl not found in PATH: %w", err)
	}

	// Split positional cluster name from the post-`--` command to run.
	clusterName := ""
	var containerCmd []string
	if dash := cmd.ArgsLenAtDash(); dash >= 0 {
		if dash > 0 {
			clusterName = args[0]
		}
		containerCmd = args[dash:]
	} else if len(args) > 0 {
		clusterName = args[0]
	}

	ctx := context.Background()
	k8sClient, coreClient, _, ns, err := initClient()
	if err != nil {
		return err
	}

	sc, err := pickCluster(ctx, k8sClient, ns, clusterName)
	if err != nil {
		return err
	}

	podName := resolvePodName(sc.Name, debugPod)
	pod, err := coreClient.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get pod %s/%s: %w", ns, podName, err)
	}

	image := debugImage
	if image == "" {
		image = toolkitImageFor(pod, debugTarget)
	}

	// Detect what the namespace's PodSecurity level actually admits, then build
	// the matching profile + securityContext. kubectl's default "general" debug
	// profile always adds SYS_PTRACE (rejected under baseline/restricted), so we
	// base on "baseline"/"restricted" and express everything via --custom.
	level, probeErr := detectPSALevel(ctx, coreClient, ns, debugNoPrivileged)
	if probeErr != nil {
		fmt.Fprintf(os.Stderr,
			"⚠ could not probe the namespace PodSecurity level (%v); assuming baseline.\n", probeErr)
	}

	var (
		profile string
		sctx    debugSecurityContext
		mounts  []debugVolumeMount
		who     string
		fsHint  string
	)
	switch level {
	case psaPrivileged:
		profile = "baseline" // adds nothing; our --custom adds the caps
		no := false
		sctx = debugSecurityContext{
			RunAsUser:    ptrI64(0),
			RunAsNonRoot: &no,
			Capabilities: &debugCapabilities{Add: debugFullCaps},
		}
		who = "root + [" + strings.Join(debugFullCaps, ",") + "] (privileged namespace — full power)"
		fsHint = "slapd's live filesystem: /proc/1/root/   |   trace slapd: strace -p 1 -f"
	case psaBaseline:
		profile = "baseline"
		no := false
		sctx = debugSecurityContext{RunAsUser: ptrI64(0), RunAsNonRoot: &no}
		mounts = slapdMountsFor(pod.Spec.Volumes)
		fmt.Fprintf(os.Stderr,
			"⚠ namespace enforces PodSecurity \"baseline\" — reducing debugging power "+
				"(no ptrace/strace, no /proc/1/root traversal).\n"+
				"  For full power, raise the level:  kubectl label ns %s pod-security.kubernetes.io/enforce=privileged --overwrite\n\n",
			ns)
		who = "root, no added capabilities (baseline-safe)"
		fsHint = "slapd PVCs mounted read-only at: " + mountPaths(mounts)
	case psaRestricted:
		profile = "restricted"
		sctx = debugSecurityContext{RunAsUser: ptrI64(1024), RunAsGroup: ptrI64(1024)}
		mounts = slapdMountsFor(pod.Spec.Volumes)
		fmt.Fprintf(os.Stderr,
			"⚠ namespace enforces PodSecurity \"restricted\" — running non-root (UID 1024), "+
				"further reduced.\n"+
				"  For full power, raise the level:  kubectl label ns %s pod-security.kubernetes.io/enforce=privileged --overwrite\n\n",
			ns)
		who = "UID 1024 (restricted-compatible: drop ALL, seccomp RuntimeDefault)"
		fsHint = "slapd PVCs mounted read-only at: " + mountPaths(mounts)
	}

	custom, err := writeCustom(sctx, mounts)
	if err != nil {
		return err
	}
	defer os.Remove(custom)

	// Default to an interactive bash. The slapd-toolkit image is based on the
	// python image, whose default entrypoint is a Python REPL — not a debug shell.
	if len(containerCmd) == 0 {
		containerCmd = []string{"bash"}
	}

	kubeArgs := []string{"debug", podName, "-it", "-n", ns,
		"--image=" + image,
		"--image-pull-policy=" + debugPullPolicy,
		"--target=" + debugTarget,
		"--profile=" + profile,
		"--custom=" + custom,
	}
	if kubeContext != "" {
		kubeArgs = append(kubeArgs, "--context="+kubeContext)
	}
	if kubeconfig != "" {
		kubeArgs = append(kubeArgs, "--kubeconfig="+kubeconfig)
	}
	kubeArgs = append(kubeArgs, "--")
	kubeArgs = append(kubeArgs, containerCmd...)

	fmt.Fprintf(os.Stderr,
		"→ debugging %s/%s (target container %q) as %s\n"+
			"  image:  %s\n"+
			"  %s\n"+
			"  ldap:   ldapsearch -x -H ldap://localhost:1024 -b '' -s base namingContexts\n\n",
		ns, podName, debugTarget, who, image, fsHint)

	c := exec.CommandContext(ctx, "kubectl", kubeArgs...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	return nil
}

// detectPSALevel probes, via server-side dry-run, the strongest security
// context the namespace's PodSecurity enforcement will admit. skipPrivileged
// forces the reduced (mount-based) path even where privileged is allowed. A
// non-nil error means the probe itself failed (e.g. no create permission); the
// caller should treat the returned level (baseline) as a best-effort default.
func detectPSALevel(ctx context.Context, coreClient kubernetes.Interface, ns string, skipPrivileged bool) (psaLevel, error) {
	no := false
	if !skipPrivileged {
		privSC := &corev1.SecurityContext{
			RunAsUser:    ptrI64(0),
			RunAsNonRoot: &no,
			Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"SYS_PTRACE"}},
		}
		ok, err := psaAdmits(ctx, coreClient, ns, privSC)
		if err != nil {
			return psaBaseline, err
		}
		if ok {
			return psaPrivileged, nil
		}
	}
	rootSC := &corev1.SecurityContext{RunAsUser: ptrI64(0), RunAsNonRoot: &no}
	ok, err := psaAdmits(ctx, coreClient, ns, rootSC)
	if err != nil {
		return psaBaseline, err
	}
	if ok {
		return psaBaseline, nil
	}
	return psaRestricted, nil
}

// psaAdmits reports whether a pod carrying the given container securityContext
// would be admitted into the namespace (server-side dry-run — nothing is
// created). A PodSecurity rejection returns (false, nil); any other error
// (RBAC, connectivity) returns (false, err) so the caller can fall back.
func psaAdmits(ctx context.Context, coreClient kubernetes.Interface, ns string, sc *corev1.SecurityContext) (bool, error) {
	probe := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "slctl-psa-probe", Namespace: ns},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:            "probe",
				Image:           "slctl-psa-probe",
				SecurityContext: sc,
			}},
		},
	}
	_, err := coreClient.CoreV1().Pods(ns).Create(ctx, probe, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	if err == nil {
		return true, nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "podsecurity") || strings.Contains(msg, "violates") {
		return false, nil
	}
	return false, err
}

// slapdMountsFor returns read-only volume mounts for the slapd data volumes
// present on the pod, mounted at their native paths (/config, /data, /accesslog).
func slapdMountsFor(volumes []corev1.Volume) []debugVolumeMount {
	present := make(map[string]bool, len(volumes))
	for _, v := range volumes {
		present[v.Name] = true
	}
	var mounts []debugVolumeMount
	for _, name := range slapdDataVolumes {
		if present[name] {
			mounts = append(mounts, debugVolumeMount{Name: name, MountPath: "/" + name, ReadOnly: true})
		}
	}
	return mounts
}

func mountPaths(mounts []debugVolumeMount) string {
	if len(mounts) == 0 {
		return "(no slapd data volumes found on this pod)"
	}
	var paths []string
	for _, m := range mounts {
		paths = append(paths, m.MountPath)
	}
	return strings.Join(paths, " ")
}

// toolkitImageFor derives the slapd-toolkit image reference from the pod's
// actual running slapd container image: the trailing `slapd` path segment
// becomes `slapd-toolkit`, keeping the registry and tag. Reading the resolved
// pod image (rather than the CR spec) is deliberate — the operator defaults
// spec.images at runtime, so an omitted spec.images block leaves the CR empty
// while the pod still carries the real, tagged image. Falls back to the
// canonical public image when the pod image can't be resolved or doesn't follow
// the naming convention.
func toolkitImageFor(pod *corev1.Pod, containerName string) string {
	repo, tag := splitImageRef(slapdContainerImage(pod, containerName))
	if tag == "" {
		tag = "latest"
	}
	switch {
	case strings.HasSuffix(repo, "/slapd"):
		repo = strings.TrimSuffix(repo, "/slapd") + "/slapd-toolkit"
	case repo == "slapd":
		repo = "slapd-toolkit"
	default:
		repo = "ghcr.io/chuck-chuck-chuck-net/slaptain/slapd-toolkit"
	}
	return repo + ":" + tag
}

// slapdContainerImage returns the image of the named container (the slapd
// container by default), falling back to the first container's image.
func slapdContainerImage(pod *corev1.Pod, containerName string) string {
	for _, c := range pod.Spec.Containers {
		if c.Name == containerName {
			return c.Image
		}
	}
	if len(pod.Spec.Containers) > 0 {
		return pod.Spec.Containers[0].Image
	}
	return ""
}

// splitImageRef splits an image reference into (repository, tag). A colon only
// separates a tag when it appears after the last slash, so registry ports
// (host:5000/path) are preserved. A digest-pinned reference (repo@sha256:...)
// yields an empty tag — the toolkit can't share the slapd digest.
func splitImageRef(imageRef string) (string, string) {
	if imageRef == "" {
		return "", ""
	}
	if i := strings.Index(imageRef, "@"); i >= 0 {
		return imageRef[:i], ""
	}
	slash := strings.LastIndex(imageRef, "/")
	if colon := strings.LastIndex(imageRef, ":"); colon > slash {
		return imageRef[:colon], imageRef[colon+1:]
	}
	return imageRef, ""
}

func ptrI64(v int64) *int64 { return &v }

type debugCapabilities struct {
	Add  []string `json:"add,omitempty"`
	Drop []string `json:"drop,omitempty"`
}

type debugSecurityContext struct {
	RunAsUser    *int64             `json:"runAsUser,omitempty"`
	RunAsGroup   *int64             `json:"runAsGroup,omitempty"`
	RunAsNonRoot *bool              `json:"runAsNonRoot,omitempty"`
	Capabilities *debugCapabilities `json:"capabilities,omitempty"`
}

type debugVolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly"`
}

// writeCustom writes a partial container spec (securityContext + optional volume
// mounts) to a temp file for `kubectl debug --custom`, and returns its path.
func writeCustom(sctx debugSecurityContext, mounts []debugVolumeMount) (string, error) {
	payload, err := json.Marshal(struct {
		SecurityContext debugSecurityContext `json:"securityContext"`
		VolumeMounts    []debugVolumeMount   `json:"volumeMounts,omitempty"`
	}{SecurityContext: sctx, VolumeMounts: mounts})
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "slctl-debug-profile-*.json")
	if err != nil {
		return "", fmt.Errorf("create debug profile: %w", err)
	}
	if _, err := f.Write(payload); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("write debug profile: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func init() {
	debugCmd.Flags().StringVar(&debugPod, "pod", "0", "pod to debug: ordinal (0), 'readonly-N', or full pod name")
	debugCmd.Flags().StringVar(&debugImage, "image", "", "debug container image (default: derived slapd-toolkit image)")
	debugCmd.Flags().StringVar(&debugPullPolicy, "image-pull-policy", "IfNotPresent", "image pull policy for the debug container")
	debugCmd.Flags().StringVar(&debugTarget, "target", "slapd", "container whose process namespace to share")
	debugCmd.Flags().BoolVar(&debugNoPrivileged, "no-privileged", false, "never escalate to privileged; use the read-only-mount mode even where privileged is allowed")
	rootCmd.AddCommand(debugCmd)
}
