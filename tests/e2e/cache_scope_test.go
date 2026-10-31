package e2e_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// ADR-042: the operator's cache holds what the operator manages and nothing
// else. Issue #28 measured the alternative: on a shared cluster with 84
// namespaces the operator was OOM-killed at its 128Mi default while managing
// one two-pod cluster, because every informer behind an Owns() or a cached Get
// held EVERY object of its kind — Helm release Secrets included.
//
// Three specs, each a different half of the rule:
//
//  1. The measurement the issue reports, as an assertion: Secrets created in a
//     namespace the operator has no business in must not grow its heap. The
//     reading is the operator's own go_memstats, scraped through the API
//     server's pod proxy — which is why tests/e2e.sh installs the operator with
//     metrics.enabled=true. Before the fix the informer decoded and held every
//     byte of them (and, past the limit, the pod restarted); after it, the
//     watch is filtered at the API server and the operator never sees them.
//     A second, independent reading — the kubelet's working-set figure for
//     the container — is recorded beside it when the node proxy answers, so a
//     surprising heap number can be checked against an instrument that does
//     not share Go's accounting.
//  2. Every object the operator creates carries the selector label, so the
//     filtered cache can hold it. An object it could not see again is one it
//     would create twice, never adopt, or — for the Secret it watches — never
//     react to.
//  3. The Secrets a user supplies carry NO label of ours and the cluster works
//     anyway, because Secret reads bypass the cache. The fixture's TLS Secret
//     is the instrument: tests/gencert.sh writes it unlabelled, and TLSReady
//     is True only if the operator found it. This spec is green from birth by
//     design — it guards the instrument. Label the fixture's TLS Secret and it
//     goes red, which is the mutation that proves it: were the Secret labelled,
//     a cached read would pass this suite while failing every real user.
var _ = Describe("operator cache scope", Label("cache-scope"), Ordered, ContinueOnFailure, func() {
	const (
		// cacheSelector is ADR-042's selector, spelled out so that a change to
		// the operator's constant cannot move both sides of the check.
		cacheSelector = "app.kubernetes.io/name"
		cacheValue    = "slapd"
		// metricsPort is charts/operator values.metrics.port.
		metricsPort = "8080"
		// Foreign load: enough that an unfiltered Secret informer cannot hide
		// it behind GC noise, little enough to apply in seconds. The data is
		// random so that neither etcd nor the decoder can compress it away.
		foreignSecrets    = 96
		foreignSecretSize = 768 * 1024
		// heapBudget is how much the operator's live heap may grow while the
		// foreign Secrets exist. The load is 72 MiB raw; an informer holding
		// it grows by at least that. A filtered informer grows by nothing.
		heapBudget = 24 * 1024 * 1024
	)
	var (
		operatorNamespace = envOrDefault("NAMESPACE", "slaptain-system")
		operatorPod       string
		operatorNode      string
		restartsBefore    int32
	)

	heapInUse := func(ctx context.Context) (float64, error) {
		res := k8sClient.CoreV1().RESTClient().Get().
			Namespace(operatorNamespace).Resource("pods").Name(operatorPod + ":" + metricsPort).
			SubResource("proxy").Suffix("metrics").Do(ctx)
		body, err := res.Raw()
		if err != nil {
			return 0, fmt.Errorf("GET /metrics on %s/%s via the pod proxy: %w", operatorNamespace, operatorPod, err)
		}
		vals := sampleValues(parseExposition(body), "go_memstats_heap_inuse_bytes")
		if len(vals) != 1 {
			return 0, fmt.Errorf("go_memstats_heap_inuse_bytes: %d samples, want 1", len(vals))
		}
		return strconv.ParseFloat(vals[0], 64)
	}

	// workingSet is the kubelet's container_memory_working_set for the
	// operator container, read from the node's stats summary through the API
	// server's node proxy. A second instrument for the same question, which
	// fails differently from the operator's own heap accounting. Best-effort:
	// a cluster whose node proxy is closed reports -1 and the spec says so.
	workingSet := func(ctx context.Context) int64 {
		raw, err := k8sClient.CoreV1().RESTClient().Get().
			Resource("nodes").Name(operatorNode).
			SubResource("proxy").Suffix("stats/summary").DoRaw(ctx)
		if err != nil {
			return -1
		}
		var summary struct {
			Pods []struct {
				PodRef     struct{ Name, Namespace string } `json:"podRef"`
				Containers []struct {
					Name   string `json:"name"`
					Memory struct {
						WorkingSetBytes *int64 `json:"workingSetBytes"`
					} `json:"memory"`
				} `json:"containers"`
			} `json:"pods"`
		}
		if err := json.Unmarshal(raw, &summary); err != nil {
			return -1
		}
		for _, p := range summary.Pods {
			if p.PodRef.Namespace != operatorNamespace || p.PodRef.Name != operatorPod {
				continue
			}
			for _, c := range p.Containers {
				if c.Name == "manager" && c.Memory.WorkingSetBytes != nil {
					return *c.Memory.WorkingSetBytes
				}
			}
		}
		return -1
	}

	restarts := func(ctx context.Context) int32 {
		pod, err := k8sClient.CoreV1().Pods(operatorNamespace).Get(ctx, operatorPod, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		var n int32
		for _, cs := range pod.Status.ContainerStatuses {
			n += cs.RestartCount
		}
		return n
	}

	BeforeAll(func(ctx SpecContext) {
		pods, err := k8sClient.CoreV1().Pods(operatorNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "control-plane=controller-manager",
		})
		Expect(err).NotTo(HaveOccurred())
		for _, p := range pods.Items {
			if p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil {
				operatorPod, operatorNode = p.Name, p.Spec.NodeName
			}
		}
		Expect(operatorPod).NotTo(BeEmpty(), "no running operator pod in %s", operatorNamespace)
		restartsBefore = restarts(ctx)

		_, err = heapInUse(ctx)
		Expect(err).NotTo(HaveOccurred(),
			"the operator's /metrics must be reachable: tests/e2e.sh installs the chart with "+
				"metrics.enabled=true; this spec cannot measure the cache without it")
		GinkgoWriter.Printf("operator pod %s on node %s, %d restarts so far, working set %d bytes\n",
			operatorPod, operatorNode, restartsBefore, workingSet(ctx))
	})

	It("holds no foreign Secret: a namespace full of them leaves the operator's heap alone", func(ctx SpecContext) {
		By("taking the baseline")
		before, err := heapInUse(ctx)
		Expect(err).NotTo(HaveOccurred())
		wsBefore := workingSet(ctx)

		By(fmt.Sprintf("creating %d Secrets of %d KiB in a namespace of their own", foreignSecrets, foreignSecretSize/1024))
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "slaptain-e2e-foreign-"}}
		ns, err = k8sClient.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx SpecContext) {
			_ = k8sClient.CoreV1().Namespaces().Delete(ctx, ns.Name, metav1.DeleteOptions{})
		})
		payload := make([]byte, foreignSecretSize)
		for i := 0; i < foreignSecrets; i++ {
			_, err := rand.Read(payload)
			Expect(err).NotTo(HaveOccurred())
			_, err = k8sClient.CoreV1().Secrets(ns.Name).Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("foreign-%03d", i)},
				// Helm's shape: the kind a shared cluster is full of.
				Type: "helm.sh/release.v1",
				Data: map[string][]byte{"release": payload},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By("watching the operator's heap settle")
		// An unfiltered informer receives the objects within a second and
		// keeps them; a filtered one never receives them. The minimum over
		// the window is the live heap after whatever GC ran, so a baseline
		// taken at a GC high-water mark cannot read as growth and a real
		// 72 MiB cannot hide behind one.
		after := math.Inf(1)
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			v, err := heapInUse(ctx)
			Expect(err).NotTo(HaveOccurred(), "the operator stopped answering while holding the foreign Secrets")
			after = math.Min(after, v)
			time.Sleep(5 * time.Second)
		}
		wsAfter := workingSet(ctx)
		growth := after - before
		GinkgoWriter.Printf("heap in use: %.1f MiB → min %.1f MiB (growth %.1f MiB); kubelet working set: %d → %d bytes\n",
			before/1024/1024, after/1024/1024, growth/1024/1024, wsBefore, wsAfter)

		Expect(restarts(ctx)).To(Equal(restartsBefore),
			"the operator restarted while %d foreign Secrets existed — issue #28's OOM", foreignSecrets)
		Expect(growth).To(BeNumerically("<", float64(heapBudget)),
			"the operator's heap grew by %.1f MiB on %d MiB of Secrets it does not manage: "+
				"its Secret informer is not filtered (ADR-042); kubelet working set %d → %d",
			growth/1024/1024, foreignSecrets*foreignSecretSize/1024/1024, wsBefore, wsAfter)
	})

	It("labels every object it creates, so its filtered cache can hold it", func(ctx SpecContext) {
		var problems []string
		check := func(what string, labels map[string]string) {
			if labels[cacheSelector] != cacheValue {
				problems = append(problems, fmt.Sprintf("%s: %s=%q", what, cacheSelector, labels[cacheSelector]))
			}
		}
		for _, name := range []string{"slapd", "slapd-headless"} {
			svc, err := k8sClient.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			check("Service "+name, svc.Labels)
		}
		sts, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, "slapd", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		check("StatefulSet slapd", sts.Labels)
		for _, name := range []string{"slapd-database-dirs", "slapd-startup-config"} {
			cm, err := k8sClient.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			check("ConfigMap "+name, cm.Labels)
		}
		// The Secrets the operator OWNS — the one kind it watches. A
		// credentials Secret the mesh scripts pre-created carries no owner
		// and is read uncached, so it is not held to this.
		secrets, err := k8sClient.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		var owned int
		for _, s := range secrets.Items {
			if owner := metav1.GetControllerOf(&s); owner != nil && owner.Kind == "SlapdCluster" {
				owned++
				check("Secret "+s.Name, s.Labels)
			}
		}
		Expect(owned).NotTo(BeZero(), "no Secret owned by a SlapdCluster in %s; the check ran against nothing", namespace)
		Expect(problems).To(BeEmpty(), "objects the operator created but its cache cannot select (ADR-042)")
	})

	It("reads the user's unlabelled TLS Secret anyway, because Secret reads bypass the cache", func(ctx SpecContext) {
		sc := &ldapv1alpha1.SlapdCluster{}
		Expect(crdClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "slapd"}, sc)).To(Succeed())
		if !sc.Spec.LDAP.TLS.Enabled || sc.Spec.LDAP.TLS.SecretName == "" {
			Skip("the fixture runs without TLS; no user-supplied Secret to read")
		}
		secret, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx, sc.Spec.LDAP.TLS.SecretName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(secret.Labels).NotTo(HaveKeyWithValue(cacheSelector, cacheValue),
			"the fixture's TLS Secret %s carries the operator's cache label — this spec then proves nothing: "+
				"a cached Secret read would pass here and fail every real user", secret.Name)

		cond := findCondition(sc.Status.Conditions, "TLSReady")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue),
			"TLSReady=%s/%s: the operator did not find the unlabelled Secret %s — %s",
			cond.Status, cond.Reason, secret.Name, cond.Message)
	})
})
