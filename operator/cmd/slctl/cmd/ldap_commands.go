package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// splitLDAPArgs walks raw cobra args and pulls out slctl-specific flags,
// leaving everything else for the underlying ldap-utils tool. This lets users
// write `slctl ldapsearch -b foo '(uid=alice)'` without a `--` separator while
// still keeping `--as config`, `--pod 0`, etc. on the slctl side.
//
// Recognized slctl flags (long form only; short forms are intentionally
// omitted to avoid colliding with ldap-utils short flags like -d/-w):
//
//	--cluster <name>   --database <name>   --as <ident>
//	--password <pw>    --pod <ord>         --anonymous   --ldaps
//	--port-forward     --direct            --node-ip <ip>
//	--verbose          --redact-password
//
// `--help`/`-h` is delegated to cobra by NOT consuming it here.
// `--` terminates slctl-flag scanning; remaining tokens always pass through.
func splitLDAPArgs(raw []string, f *ldapTargetFlags) []string {
	stringFlags := map[string]*string{
		"--cluster":    &f.cluster,
		"--database":   &f.database,
		"--as":         &f.as,
		"--password":   &f.password,
		"--pod":        &f.pod,
		"--node-ip":    &f.nodeIP,
		"--kubeconfig": &kubeconfig,
		"--context":    &kubeContext,
		"--namespace":  &namespace,
		"-n":           &namespace, // short form of --namespace
	}
	boolFlags := map[string]*bool{
		"--anonymous":       &f.anonymous,
		"--ldaps":           &f.useTLS,
		"--port-forward":    &f.forcePortForward,
		"--direct":          &f.forceDirect,
		"--verbose":         &f.verbose,
		"--redact-password": &f.redactPassword,
	}

	var passthrough []string
	for i := 0; i < len(raw); i++ {
		tok := raw[i]

		if tok == "--" {
			passthrough = append(passthrough, raw[i+1:]...)
			break
		}

		// --name=value form
		if eq := strings.IndexByte(tok, '='); eq > 2 && strings.HasPrefix(tok, "--") {
			name := tok[:eq]
			if dst, ok := stringFlags[name]; ok {
				*dst = tok[eq+1:]
				continue
			}
			if dst, ok := boolFlags[name]; ok {
				*dst = tok[eq+1:] == "true"
				continue
			}
			passthrough = append(passthrough, tok)
			continue
		}

		// --name value form
		if dst, ok := stringFlags[tok]; ok {
			if i+1 >= len(raw) {
				// missing value — let ldap-utils complain, just pass through
				passthrough = append(passthrough, tok)
				continue
			}
			*dst = raw[i+1]
			i++
			continue
		}
		if dst, ok := boolFlags[tok]; ok {
			*dst = true
			continue
		}

		passthrough = append(passthrough, tok)
	}
	return passthrough
}

// runLDAPUtil dispatches to the named ldap-utils binary (ldapsearch, ldapadd,
// ldapmodify, ldapdelete) with auto-discovered -H / -D / -w flags prepended,
// followed by any user-supplied extra args. stdin/stdout/stderr are wired
// straight through so LDIF input on stdin and the tool's normal output work.
func runLDAPUtil(tool string, raw []string) error {
	if _, err := exec.LookPath(tool); err != nil {
		return fmt.Errorf("%s not found in PATH (install ldap-utils): %w", tool, err)
	}

	var f ldapTargetFlags
	f.as = "admin" // default
	extraArgs := splitLDAPArgs(raw, &f)

	ctx := context.Background()
	k8sClient, coreClient, restCfg, ns, err := initClient()
	if err != nil {
		return err
	}

	target, err := resolveLDAPTarget(ctx, k8sClient, coreClient, restCfg, ns, &f)
	if err != nil {
		return err
	}
	defer target.Close()

	args := buildLDAPArgs(target, extraArgs)

	identity := target.BindDN
	if identity == "" {
		identity = "(anonymous)"
	}
	if f.verbose {
		// Transparency mode: print the exact reproducible commands so a user
		// can run them by hand (labs; "disenchant the magic"). The password is
		// shown in full by default — deliberately, so the printed ldap* line is
		// copy-pasteable — and masked only under --redact-password. Whether to
		// keep it out of shell history is the operator's call, not ours.
		if target.DirectNote != "" {
			fmt.Fprintf(os.Stderr, "→ direct pod IP: %s\n", target.DirectNote)
		}
		if target.PortForwardPod != "" {
			fmt.Fprintf(os.Stderr, "→ %s\n",
				formatPortForwardCmd(kubectlGlobalArgs(), ns, target.PortForwardPod, target.LocalPort, target.RemotePort))
		}
		fmt.Fprintf(os.Stderr, "→ %s\n", formatInvocation(tool, args, f.redactPassword))
	} else {
		fmt.Fprintf(os.Stderr, "→ %s %s as %s\n", tool, target.URI, identity)
	}

	c := exec.CommandContext(ctx, tool, args...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if target.ReqCert != "" {
		c.Env = append(os.Environ(), "LDAPTLS_REQCERT="+target.ReqCert)
	}

	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	return nil
}

// buildLDAPArgs constructs the ldap-utils argument vector for a resolved target
// plus the user's passthrough args: -H <uri>, then -x (always) and -D/-w when
// binding, then the extra args verbatim.
func buildLDAPArgs(target *ldapTarget, extraArgs []string) []string {
	args := []string{"-H", target.URI}
	if target.BindDN == "" {
		args = append(args, "-x")
	} else {
		args = append(args, "-x", "-D", target.BindDN, "-w", target.Password)
	}
	return append(args, extraArgs...)
}

// kubectlGlobalArgs renders the global kubeconfig/context flags that slctl was
// invoked with, so the printed `kubectl port-forward` line targets the same
// cluster. Namespace is passed separately by the caller (via -n).
func kubectlGlobalArgs() []string {
	var out []string
	if kubeContext != "" {
		out = append(out, "--context="+kubeContext)
	}
	if kubeconfig != "" {
		out = append(out, "--kubeconfig="+kubeconfig)
	}
	return out
}

// formatPortForwardCmd renders the `kubectl port-forward` command equivalent to
// the forward slctl set up, for --verbose output.
func formatPortForwardCmd(global []string, ns, pod string, localPort, remotePort int) string {
	parts := append([]string{"kubectl"}, global...)
	parts = append(parts, "-n", ns, "port-forward", "pod/"+pod,
		fmt.Sprintf("%d:%d", localPort, remotePort))
	return strings.Join(parts, " ")
}

// formatInvocation renders a copy-pasteable command line for `tool args...`,
// shell-quoting values that need it. The value following a `-w` token is always
// single-quoted (it may contain shell metacharacters), and is masked with a
// placeholder when redact is true.
func formatInvocation(tool string, args []string, redact bool) string {
	parts := []string{tool}
	for i := 0; i < len(args); i++ {
		parts = append(parts, shellQuote(args[i]))
		if args[i] == "-w" && i+1 < len(args) {
			val := args[i+1]
			if redact {
				val = "<password>"
			}
			parts = append(parts, singleQuote(val))
			i++
		}
	}
	return strings.Join(parts, " ")
}

// shellSafe matches tokens that need no quoting in a POSIX shell.
var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

func shellQuote(s string) string {
	if s != "" && shellSafe.MatchString(s) {
		return s
	}
	return singleQuote(s)
}

// singleQuote wraps s in single quotes, escaping any embedded single quote as
// the standard '\” idiom.
func singleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ── Commands ──────────────────────────────────────────────────────────────────

const ldapLongCommon = `slctl flags (long-form only; short flags pass through to the tool):
  --cluster <name>     SlapdCluster (default: the only one in the namespace)
  --database <name>    SlapdDatabase (default: the only one for the cluster)
  --as <ident>         'admin' (default), 'config', 'replication', or a literal DN
  --anonymous          bind anonymously
  --password <pw>      bind password (only with a literal --as DN)
  --pod <ord|name>     bypass the Service, talk to one pod (direct pod IP when
                       this host has a route to the pod network, else a
                       port-forward)
  --ldaps              connect via ldaps:// (port 1025)
  --port-forward       force a port-forward to pod-0 even when a NodePort/LB
                       Service exists (dual-homed clusters where the auto-picked
                       node IP isn't reachable from here); also disables the
                       direct pod-IP path
  --direct             force a direct pod-IP connection (default pod-0) without
                       the route check; falls back to a port-forward if the TCP
                       probe fails. Direct pod IPs are otherwise auto-detected
                       from this host's routing table; SLCTL_DIRECT_POD_IPS=
                       always|never|auto overrides the detection
  --node-ip <ip>       address to reach a NodePort endpoint (overrides the
                       auto-discovered node InternalIP)
  --verbose            print the reproducible kubectl port-forward + ldap*
                       command (with the real password) so you can run it by hand
  --redact-password    mask the password in --verbose output (for demos/screenshares)

All other arguments pass through to the wrapped ldap-utils tool. Use '--' to
mark the end of slctl flags explicitly if needed.`

var ldapsearchCmd = &cobra.Command{
	Use:                "ldapsearch [slctl-flags] [ldapsearch-args...]",
	Short:              "Run ldapsearch against a SlapdCluster with auto-discovered credentials",
	DisableFlagParsing: true,
	SilenceUsage:       true,
	Long: `Wraps the system ldapsearch with -H/-D/-w filled in from a SlapdCluster
and its SlapdDatabase.

Examples:
  slctl ldapsearch -b 'ou=People,dc=example,dc=org' '(uid=alice)'
  slctl ldapsearch --as config --pod 0 -b 'cn=config' -s base '(objectClass=*)'
  slctl ldapsearch --cluster slapd --anonymous -s base -b '' '+'
  slctl ldapsearch --verbose -b 'dc=example,dc=org' -s one dn   # print the equivalent commands
  slctl ldapsearch --node-ip 192.0.2.10 -b 'dc=example,dc=org'  # override NodePort node IP

` + ldapLongCommon,
	RunE: func(cmd *cobra.Command, args []string) error {
		if isHelp(args) {
			return cmd.Help()
		}
		return runLDAPUtil("ldapsearch", args)
	},
}

var ldapaddCmd = &cobra.Command{
	Use:                "ldapadd [slctl-flags] [ldapadd-args...]",
	Short:              "Run ldapadd against a SlapdCluster with auto-discovered credentials",
	DisableFlagParsing: true,
	SilenceUsage:       true,
	Long: `Wraps the system ldapadd. LDIF is read from stdin or via -f <file>.

Examples:
  cat user.ldif | slctl ldapadd
  slctl ldapadd -f user.ldif

` + ldapLongCommon,
	RunE: func(cmd *cobra.Command, args []string) error {
		if isHelp(args) {
			return cmd.Help()
		}
		return runLDAPUtil("ldapadd", args)
	},
}

var ldapmodifyCmd = &cobra.Command{
	Use:                "ldapmodify [slctl-flags] [ldapmodify-args...]",
	Short:              "Run ldapmodify against a SlapdCluster with auto-discovered credentials",
	DisableFlagParsing: true,
	SilenceUsage:       true,
	Long: `Wraps the system ldapmodify. LDIF is read from stdin or via -f <file>.

Examples:
  cat change.ldif | slctl ldapmodify
  slctl ldapmodify --as config -f olc-change.ldif

` + ldapLongCommon,
	RunE: func(cmd *cobra.Command, args []string) error {
		if isHelp(args) {
			return cmd.Help()
		}
		return runLDAPUtil("ldapmodify", args)
	},
}

var ldapdeleteCmd = &cobra.Command{
	Use:                "ldapdelete [slctl-flags] DN [DN...]",
	Short:              "Run ldapdelete against a SlapdCluster with auto-discovered credentials",
	DisableFlagParsing: true,
	SilenceUsage:       true,
	Long: `Wraps the system ldapdelete.

Examples:
  slctl ldapdelete 'uid=alice,ou=People,dc=example,dc=org'

` + ldapLongCommon,
	RunE: func(cmd *cobra.Command, args []string) error {
		if isHelp(args) {
			return cmd.Help()
		}
		return runLDAPUtil("ldapdelete", args)
	},
}

func isHelp(args []string) bool {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			return true
		}
		if a == "--" {
			return false
		}
	}
	return false
}

func init() {
	rootCmd.AddCommand(ldapsearchCmd)
	rootCmd.AddCommand(ldapaddCmd)
	rootCmd.AddCommand(ldapmodifyCmd)
	rootCmd.AddCommand(ldapdeleteCmd)
}
