package cmd

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
	k8scli "github.com/chuck-chuck-chuck-net/slaptain/operator/internal/cli/k8s"
)

// ldapTargetFlags holds the flags shared by `slctl ldap*` wrappers.
type ldapTargetFlags struct {
	cluster   string
	database  string
	as        string // "admin" (default), "config", "replication", or a literal DN
	anonymous bool
	password  string // only honored when --as is a literal DN
	pod       string // ordinal "0" or full pod name; targets one pod (direct or port-forward)
	useTLS    bool   // ldaps:// instead of ldap://

	// Endpoint overrides (labs / dual-homed clusters).
	forcePortForward bool   // --port-forward: bypass Service discovery, forward to pod-0
	forceDirect      bool   // --direct: connect to the pod IP without a route check
	nodeIP           string // --node-ip: override the node address for a NodePort endpoint

	// Transparency.
	verbose        bool // --verbose: print the reproducible kubectl/ldap* commands
	redactPassword bool // --redact-password: mask the password in --verbose output
}

// ldapTarget is the resolved connection + bind information.
type ldapTarget struct {
	URI      string
	BindDN   string // empty when anonymous
	Password string // empty when anonymous
	cleanup  func() // port-forward teardown; may be nil
	// reqCert is the LDAPTLS_REQCERT value to set ("" = inherit). Set to
	// "never" when we cannot present a hostname that matches the cert (e.g.
	// port-forwarded to localhost while using ldaps).
	ReqCert string

	// Port-forward provenance, for --verbose. PortForwardPod is non-empty only
	// when the endpoint is reached through an active `kubectl port-forward`.
	PortForwardPod string
	LocalPort      int
	RemotePort     int

	// Direct pod-IP provenance, for --verbose. DirectPod is non-empty when the
	// endpoint is the pod's IP (no port-forward). DirectNote explains the
	// direct-connectivity decision either way (why direct was used, or why a
	// pod-targeted request fell back to a port-forward).
	DirectPod  string
	DirectNote string
}

func (t *ldapTarget) Close() {
	if t.cleanup != nil {
		t.cleanup()
	}
}

// resolveLDAPTarget discovers the cluster, database, connection endpoint, and
// bind credentials from k8s, returning everything needed to invoke an ldap-utils
// command. Callers must defer target.Close().
func resolveLDAPTarget(
	ctx context.Context,
	k8sClient client.Client,
	coreClient kubernetes.Interface,
	restCfg *rest.Config,
	ns string,
	f *ldapTargetFlags,
) (*ldapTarget, error) {
	sc, err := pickCluster(ctx, k8sClient, ns, f.cluster)
	if err != nil {
		return nil, err
	}

	db, err := pickDatabase(ctx, k8sClient, ns, sc.Name, f.database)
	if err != nil {
		return nil, err
	}

	bindDN, password, err := resolveBind(ctx, coreClient, ns, sc, db, f)
	if err != nil {
		return nil, err
	}

	ep, err := resolveEndpoint(ctx, coreClient, restCfg, ns, sc, f)
	if err != nil {
		return nil, err
	}

	return &ldapTarget{
		URI:            ep.uri,
		BindDN:         bindDN,
		Password:       password,
		cleanup:        ep.cleanup,
		ReqCert:        ep.reqCert,
		PortForwardPod: ep.pfPod,
		LocalPort:      ep.localPort,
		RemotePort:     ep.remotePort,
		DirectPod:      ep.directPod,
		DirectNote:     ep.directNote,
	}, nil
}

// resolvedEndpoint is the outcome of endpoint resolution: the URI to dial, the
// TLS reqcert mode, an optional port-forward teardown, and (when a port-forward
// is active) enough provenance to reproduce it in --verbose output.
type resolvedEndpoint struct {
	uri        string
	reqCert    string
	cleanup    func()
	pfPod      string // non-empty when reached via port-forward
	localPort  int
	remotePort int
	directPod  string // non-empty when connecting to the pod IP directly
	directNote string // direct-connectivity decision, for --verbose
}

// ── Cluster / Database selection ──────────────────────────────────────────────

func pickCluster(ctx context.Context, k8sClient client.Client, ns, name string) (*ldapv1alpha1.SlapdCluster, error) {
	if name != "" {
		sc := &ldapv1alpha1.SlapdCluster{}
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, sc); err != nil {
			return nil, fmt.Errorf("get SlapdCluster %s/%s: %w", ns, name, err)
		}
		return sc, nil
	}
	list := &ldapv1alpha1.SlapdClusterList{}
	if err := k8sClient.List(ctx, list, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("list SlapdClusters in %s: %w", ns, err)
	}
	switch len(list.Items) {
	case 0:
		return nil, fmt.Errorf("no SlapdCluster in namespace %q (use --cluster)", ns)
	case 1:
		return &list.Items[0], nil
	default:
		var names []string
		for _, sc := range list.Items {
			names = append(names, sc.Name)
		}
		return nil, fmt.Errorf("multiple SlapdClusters in %q (%s); pick one with --cluster",
			ns, strings.Join(names, ", "))
	}
}

func pickDatabase(ctx context.Context, k8sClient client.Client, ns, clusterName, name string) (*ldapv1alpha1.SlapdDatabase, error) {
	list := &ldapv1alpha1.SlapdDatabaseList{}
	if err := k8sClient.List(ctx, list, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("list SlapdDatabases in %s: %w", ns, err)
	}
	var forCluster []ldapv1alpha1.SlapdDatabase
	for _, db := range list.Items {
		if db.Spec.ClusterRef == clusterName {
			forCluster = append(forCluster, db)
		}
	}
	if name != "" {
		for i := range forCluster {
			if forCluster[i].Name == name {
				return &forCluster[i], nil
			}
		}
		return nil, fmt.Errorf("SlapdDatabase %q not found for cluster %q in namespace %q", name, clusterName, ns)
	}
	switch len(forCluster) {
	case 0:
		return nil, fmt.Errorf("no SlapdDatabase for cluster %q in namespace %q", clusterName, ns)
	case 1:
		return &forCluster[0], nil
	default:
		var names []string
		for _, db := range forCluster {
			names = append(names, db.Name)
		}
		return nil, fmt.Errorf("multiple SlapdDatabases for cluster %q (%s); pick one with --database",
			clusterName, strings.Join(names, ", "))
	}
}

// ── Bind resolution ───────────────────────────────────────────────────────────

func resolveBind(
	ctx context.Context,
	coreClient kubernetes.Interface,
	ns string,
	sc *ldapv1alpha1.SlapdCluster,
	db *ldapv1alpha1.SlapdDatabase,
	f *ldapTargetFlags,
) (bindDN, password string, err error) {
	if f.anonymous {
		return "", "", nil
	}

	suffix := db.Spec.Suffix

	switch f.as {
	case "admin", "":
		dn := db.Spec.RootDN
		if dn == "" {
			dn = "cn=admin," + suffix
		}
		pw, err := readSecretKey(ctx, coreClient, ns, dbCredentialsSecret(db), "root-password")
		if err != nil {
			return "", "", fmt.Errorf("read admin password: %w", err)
		}
		return dn, pw, nil
	case "config":
		pw, err := readSecretKey(ctx, coreClient, ns, sc.Name+"-config-password", "root-password")
		if err != nil {
			return "", "", fmt.Errorf("read config password: %w", err)
		}
		return "cn=admin,cn=config", pw, nil
	case "replication":
		pw, err := readSecretKey(ctx, coreClient, ns, dbCredentialsSecret(db), "replication-password")
		if err != nil {
			return "", "", fmt.Errorf("read replication password: %w", err)
		}
		return "cn=replication," + suffix, pw, nil
	}

	// Literal DN. Append the suffix if it looks like a short form
	// (trailing comma, or no '=' present at all — the search-domain idea).
	dn := f.as
	if strings.HasSuffix(dn, ",") {
		dn = dn + suffix
	} else if !strings.Contains(dn, "=") {
		dn = dn + "," + suffix
	}
	if f.password == "" {
		return "", "", fmt.Errorf("--password is required when --as is a literal DN")
	}
	return dn, f.password, nil
}

func dbCredentialsSecret(db *ldapv1alpha1.SlapdDatabase) string {
	if db.Spec.Credentials.SecretName != "" {
		return db.Spec.Credentials.SecretName
	}
	return db.Name + "-credentials"
}

// ── Endpoint resolution ───────────────────────────────────────────────────────

// resolveEndpoint returns the LDAP endpoint to use. Strategy:
//   - --pod set                 → direct pod IP if it works (see below), else
//     port-forward to that pod
//   - --port-forward            → port-forward to pod-0 (bypasses Services and
//     the direct pod-IP path)
//   - --direct                  → direct to the pod IP (default pod-0), no
//     route check; falls back to a port-forward if the TCP probe fails
//   - any LoadBalancer Service  → external IP + service port
//   - any NodePort Service      → node IP (--node-ip or InternalIP) + nodePort
//   - otherwise                 → direct pod IP if it works, else port-forward
//     to pod-0
//
// "Direct pod IP if it works" is decided by directProber (directroute.go): the
// kernel FIB gates a short confirming TCP dial, so machines without a specific
// route to the pod network pay no probe latency and go straight to the
// port-forward, while pod-routed labs and on-node shells skip the forward
// entirely. SLCTL_DIRECT_POD_IPS=never turns the whole path off.
//
// The Service search isn't restricted to the operator-owned `<name>` Service —
// we look at every Service in the namespace whose selector targets this
// cluster's RW pods. That picks up hand-applied NodePort/LB Services like
// `<name>-external` that users commonly add for off-cluster access.
//
// --port-forward and --node-ip are the escape hatches for dual-homed clusters
// where the auto-picked NodePort node IP (the k8s InternalIP) isn't reachable
// from the runner — e.g. the InternalIP sits on a routed replication network
// with no north-south access. Force a port-forward, or name the reachable IP.
func resolveEndpoint(
	ctx context.Context,
	coreClient kubernetes.Interface,
	restCfg *rest.Config,
	ns string,
	sc *ldapv1alpha1.SlapdCluster,
	f *ldapTargetFlags,
) (*resolvedEndpoint, error) {
	scheme := "ldap"
	remotePort := 1024
	portName := "ldap"
	if f.useTLS {
		scheme = "ldaps"
		remotePort = 1025
		portName = "ldaps"
	}
	// We can't validate the slapd cert against a node IP or an LB IP — set
	// REQCERT=never whenever we connect by IP rather than service DNS.
	reqCertIfTLS := func() string {
		if f.useTLS {
			return "never"
		}
		return ""
	}

	prober := newDirectProber(f.forceDirect)

	// Single-pod targeting: an explicit --pod, --port-forward, or --direct
	// (each defaults to pod-0). Direct pod IP is tried first unless
	// --port-forward forces the tunnel.
	if f.pod != "" || f.forcePortForward || prober.mode == directAlways {
		podSpec := f.pod
		if podSpec == "" {
			podSpec = "0"
		}
		podName := resolvePodName(sc.Name, podSpec)

		var directNote string
		if !f.forcePortForward {
			ep, note := tryDirectPodIP(ctx, coreClient, ns, podName, prober, scheme, remotePort, reqCertIfTLS())
			if ep != nil {
				return ep, nil
			}
			directNote = note
		}

		localPort, c, err := k8scli.PortForward(ctx, coreClient, restCfg, ns, podName, remotePort)
		if err != nil {
			return nil, fmt.Errorf("port-forward to pod %s: %w", podName, err)
		}
		return &resolvedEndpoint{
			uri:        fmt.Sprintf("%s://localhost:%d", scheme, localPort),
			reqCert:    reqCertIfTLS(),
			cleanup:    c,
			pfPod:      podName,
			localPort:  localPort,
			remotePort: remotePort,
			directNote: directNote,
		}, nil
	}

	// Find candidate Services targeting RW pods (instance label = cluster name).
	svcList, err := coreClient.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list Services in %s: %w", ns, err)
	}

	var lbSvc, npSvc *corev1.Service
	for i := range svcList.Items {
		svc := &svcList.Items[i]
		if svc.Spec.Selector["app.kubernetes.io/instance"] != sc.Name {
			continue
		}
		if svc.Spec.ClusterIP == corev1.ClusterIPNone {
			continue // headless
		}
		switch svc.Spec.Type {
		case corev1.ServiceTypeLoadBalancer:
			if lbSvc == nil && lbIngressAddr(svc) != "" {
				lbSvc = svc
			}
		case corev1.ServiceTypeNodePort:
			if npSvc == nil {
				npSvc = svc
			}
		}
	}

	if lbSvc != nil {
		addr := lbIngressAddr(lbSvc)
		port := servicePortByName(lbSvc, portName)
		if port == 0 {
			return nil, fmt.Errorf("LoadBalancer Service %s has no port named %q", lbSvc.Name, portName)
		}
		return &resolvedEndpoint{uri: fmt.Sprintf("%s://%s:%d", scheme, addr, port), reqCert: reqCertIfTLS()}, nil
	}
	if npSvc != nil {
		nodePort := nodePortByName(npSvc, portName)
		if nodePort == 0 {
			return nil, fmt.Errorf("NodePort Service %s has no NodePort named %q", npSvc.Name, portName)
		}
		nodeIP, err := pickNodeIP(ctx, coreClient, f.nodeIP)
		if err != nil {
			return nil, err
		}
		return &resolvedEndpoint{uri: fmt.Sprintf("%s://%s:%d", scheme, nodeIP, nodePort), reqCert: reqCertIfTLS()}, nil
	}

	// Fallback: no client-reachable Service — direct pod IP if it works,
	// else port-forward to pod-0.
	podName := sc.Name + "-0"
	ep, directNote := tryDirectPodIP(ctx, coreClient, ns, podName, prober, scheme, remotePort, reqCertIfTLS())
	if ep != nil {
		return ep, nil
	}
	localPort, c, err := k8scli.PortForward(ctx, coreClient, restCfg, ns, podName, remotePort)
	if err != nil {
		return nil, fmt.Errorf("port-forward fallback to %s: %w", podName, err)
	}
	return &resolvedEndpoint{
		uri:        fmt.Sprintf("%s://localhost:%d", scheme, localPort),
		reqCert:    reqCertIfTLS(),
		cleanup:    c,
		pfPod:      podName,
		localPort:  localPort,
		remotePort: remotePort,
		directNote: directNote,
	}, nil
}

// tryDirectPodIP attempts to resolve podName to a directly reachable pod-IP
// endpoint. Returns (endpoint, "") on success and (nil, reason) when the
// caller should fall back to a port-forward; the reason feeds --verbose.
func tryDirectPodIP(
	ctx context.Context,
	coreClient kubernetes.Interface,
	ns, podName string,
	prober *directProber,
	scheme string,
	port int,
	reqCert string,
) (*resolvedEndpoint, string) {
	if prober.mode == directNever {
		return nil, "" // don't even fetch the pod; no note — nothing was attempted
	}
	podIP := ""
	if pod, err := coreClient.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{}); err == nil {
		podIP = pod.Status.PodIP
	}
	ok, reason := prober.probe(podIP, port)
	if !ok {
		return nil, fmt.Sprintf("%s: %s → port-forward", podName, reason)
	}
	return &resolvedEndpoint{
		uri:        fmt.Sprintf("%s://%s:%d", scheme, podIP, port),
		reqCert:    reqCert,
		directPod:  podName,
		directNote: fmt.Sprintf("%s is %s; %s", podName, podIP, reason),
	}, ""
}

func lbIngressAddr(svc *corev1.Service) string {
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		if ing.IP != "" {
			return ing.IP
		}
		if ing.Hostname != "" {
			return ing.Hostname
		}
	}
	return ""
}

func servicePortByName(svc *corev1.Service, name string) int32 {
	for _, p := range svc.Spec.Ports {
		if p.Name == name {
			return p.Port
		}
	}
	return 0
}

func nodePortByName(svc *corev1.Service, name string) int32 {
	for _, p := range svc.Spec.Ports {
		if p.Name == name {
			return p.NodePort
		}
	}
	return 0
}

// resolvePodName accepts an ordinal ("0"), a short suffix ("readonly-0"), or a
// full pod name ("slapd-1"). Anything else is returned verbatim.
func resolvePodName(cluster, spec string) string {
	if strings.HasPrefix(spec, cluster+"-") {
		return spec
	}
	if _, err := strconv.Atoi(spec); err == nil {
		return cluster + "-" + spec
	}
	return cluster + "-" + spec
}

func pickNodeIP(ctx context.Context, coreClient kubernetes.Interface, override string) (string, error) {
	if override != "" {
		return override, nil // don't even list nodes; the caller knows better
	}
	nodes, err := coreClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list nodes: %w", err)
	}
	return chooseNodeIP(override, nodes.Items)
}

// chooseNodeIP resolves the address used to reach a NodePort. An explicit
// override (--node-ip) always wins — the auto-picked InternalIP is wrong on
// dual-homed clusters where the InternalIP is on a network the runner can't
// reach (e.g. a routed replication network chosen as the primary node IP), and
// the reachable NIC isn't k8s-registered so it can't be discovered. Otherwise
// fall back to the first node's InternalIP.
func chooseNodeIP(override string, nodes []corev1.Node) (string, error) {
	if override != "" {
		return override, nil
	}
	for _, n := range nodes {
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP {
				return a.Address, nil
			}
		}
	}
	return "", fmt.Errorf("no node with InternalIP found; supply a reachable address with --node-ip")
}
