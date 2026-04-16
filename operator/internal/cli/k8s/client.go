package k8s

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/transport/spdy"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// NewClient returns a controller-runtime client (for CR access), a standard
// kubernetes clientset (for exec/logs), the REST config, and the default
// namespace from kubeconfig.
func NewClient(kubeconfig string) (client.Client, kubernetes.Interface, *rest.Config, string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	cfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})

	restCfg, err := cfg.ClientConfig()
	if err != nil {
		return nil, nil, nil, "", fmt.Errorf("build REST config: %w", err)
	}

	ns, _, err := cfg.Namespace()
	if err != nil {
		ns = "default"
	}

	s := scheme.Scheme
	if err := ldapv1alpha1.AddToScheme(s); err != nil {
		return nil, nil, nil, "", fmt.Errorf("register SlapdCluster scheme: %w", err)
	}

	k8sClient, err := client.New(restCfg, client.Options{Scheme: s})
	if err != nil {
		return nil, nil, nil, "", fmt.Errorf("create controller-runtime client: %w", err)
	}

	coreClient, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, nil, nil, "", fmt.Errorf("create kubernetes clientset: %w", err)
	}

	return k8sClient, coreClient, restCfg, ns, nil
}

// Exec runs a command in a pod container and returns stdout and stderr.
func Exec(ctx context.Context, coreClient kubernetes.Interface, config *rest.Config, namespace, pod, container string, command []string) (string, string, error) {
	req := coreClient.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		return "", "", fmt.Errorf("create SPDY executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return stdout.String(), stderr.String(), err
	}
	return stdout.String(), stderr.String(), nil
}

// PortForward opens a port-forward tunnel to a pod and returns the local port
// and a cancel function. The local port is dynamically allocated.
func PortForward(ctx context.Context, coreClient kubernetes.Interface, config *rest.Config, namespace, podName string, remotePort int) (localPort int, cancel func(), err error) {
	// Find a free local port
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, nil, fmt.Errorf("find free port: %w", err)
	}
	localPort = ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	transport, upgrader, err := spdy.RoundTripperFor(config)
	if err != nil {
		return 0, nil, fmt.Errorf("create SPDY round tripper: %w", err)
	}

	url := coreClient.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(namespace).
		Name(podName).
		SubResource("portforward").
		URL()

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", url)

	ports := []string{fmt.Sprintf("%d:%d", localPort, remotePort)}
	readyCh := make(chan struct{})
	stopCh := make(chan struct{})

	fw, err := portforward.New(dialer, ports, stopCh, readyCh, io.Discard, io.Discard)
	if err != nil {
		return 0, nil, fmt.Errorf("create port forwarder: %w", err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- fw.ForwardPorts()
	}()

	select {
	case <-readyCh:
		// Port forward is ready
	case err := <-errCh:
		return 0, nil, fmt.Errorf("port forward failed: %w", err)
	case <-ctx.Done():
		close(stopCh)
		return 0, nil, ctx.Err()
	}

	return localPort, func() { close(stopCh) }, nil
}
