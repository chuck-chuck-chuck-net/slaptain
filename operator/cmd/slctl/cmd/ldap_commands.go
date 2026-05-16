package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
		"--kubeconfig": &kubeconfig,
		"--context":    &kubeContext,
		"--namespace":  &namespace,
		"-n":           &namespace, // short form of --namespace
	}
	boolFlags := map[string]*bool{
		"--anonymous": &f.anonymous,
		"--ldaps":     &f.useTLS,
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

	args := []string{"-H", target.URI}
	if target.BindDN == "" {
		args = append(args, "-x")
	} else {
		args = append(args, "-x", "-D", target.BindDN, "-w", target.Password)
	}
	args = append(args, extraArgs...)

	identity := target.BindDN
	if identity == "" {
		identity = "(anonymous)"
	}
	fmt.Fprintf(os.Stderr, "→ %s %s as %s\n", tool, target.URI, identity)

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

// ── Commands ──────────────────────────────────────────────────────────────────

const ldapLongCommon = `slctl flags (long-form only; short flags pass through to the tool):
  --cluster <name>     SlapdCluster (default: the only one in the namespace)
  --database <name>    SlapdDatabase (default: the only one for the cluster)
  --as <ident>         'admin' (default), 'config', 'replication', or a literal DN
  --anonymous          bind anonymously
  --password <pw>      bind password (only with a literal --as DN)
  --pod <ord|name>     bypass the Service, port-forward to one pod
  --ldaps              connect via ldaps:// (port 1025)

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
