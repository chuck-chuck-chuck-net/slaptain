package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
	k8scli "github.com/chuck-chuck-chuck-net/slaptain/operator/internal/cli/k8s"
)

var (
	kubeconfig    string
	namespace     string
	allNamespaces bool
	jsonOutput    bool
)

var rootCmd = &cobra.Command{
	Use:   "slctl",
	Short: "CLI for slaptain SlapdCluster resources",
	Long:  "slctl is a diagnostic and management utility for SlapdCluster custom resources managed by the slaptain operator.",
}

func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.PersistentFlags().StringVar(&kubeconfig, "kubeconfig", "", "path to kubeconfig file")
	rootCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", "", "target namespace (defaults to kubeconfig context)")
	rootCmd.PersistentFlags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "list across all namespaces")
	rootCmd.PersistentFlags().BoolVar(&jsonOutput, "json", false, "output in JSON format")

	rootCmd.RegisterFlagCompletionFunc("namespace", completeNamespaces)
}

// initClient creates k8s clients and resolves the target namespace.
func initClient() (client.Client, kubernetes.Interface, *rest.Config, string, error) {
	k8sClient, coreClient, config, defaultNS, err := k8scli.NewClient(kubeconfig)
	if err != nil {
		return nil, nil, nil, "", err
	}
	ns := namespace
	if ns == "" {
		ns = defaultNS
	}
	return k8sClient, coreClient, config, ns, nil
}

// resolveTargets returns the list of SlapdCluster object keys to operate on.
func resolveTargets(ctx context.Context, k8sClient client.Client, args []string, ns string) ([]client.ObjectKey, error) {
	if len(args) > 0 {
		return []client.ObjectKey{{Name: args[0], Namespace: ns}}, nil
	}

	list := &ldapv1alpha1.SlapdClusterList{}
	var opts []client.ListOption
	if !allNamespaces {
		opts = append(opts, client.InNamespace(ns))
	}
	if err := k8sClient.List(ctx, list, opts...); err != nil {
		return nil, fmt.Errorf("list SlapdClusters: %w", err)
	}
	if len(list.Items) == 0 {
		return nil, fmt.Errorf("no SlapdCluster resources found")
	}

	keys := make([]client.ObjectKey, len(list.Items))
	for i, item := range list.Items {
		keys[i] = client.ObjectKey{Name: item.Name, Namespace: item.Namespace}
	}
	return keys, nil
}

// completeSlapdClusterNames provides tab completion for SlapdCluster names.
func completeSlapdClusterNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	k8sClient, _, _, ns, err := initClient()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	list := &ldapv1alpha1.SlapdClusterList{}
	if err := k8sClient.List(context.Background(), list, client.InNamespace(ns)); err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	var names []string
	for _, item := range list.Items {
		names = append(names, item.Name)
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

func completeNamespaces(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	_, coreClient, _, _, err := initClient()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	nsList, err := coreClient.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	var names []string
	for _, ns := range nsList.Items {
		names = append(names, ns.Name)
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

// printSeparator prints a visual separator line.
func printSeparator() {
	fmt.Println("────────────────────────────────────────")
}

// podNames returns the RW pod names for a SlapdCluster.
func podNames(name string, replicas int32) []string {
	names := make([]string, replicas)
	for i := int32(0); i < replicas; i++ {
		names[i] = fmt.Sprintf("%s-%d", name, i)
	}
	return names
}

// roPodNames returns the read-only pod names for a SlapdCluster.
func roPodNames(name string, roReplicas int32) []string {
	names := make([]string, roReplicas)
	for i := int32(0); i < roReplicas; i++ {
		names[i] = fmt.Sprintf("%s-readonly-%d", name, i)
	}
	return names
}

func errExit(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "Error: "+format+"\n", a...)
	os.Exit(1)
}
