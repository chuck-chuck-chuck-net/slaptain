package e2e_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// Restore e2e: exercises bootstrapFrom — restoring a backup into a FRESH cluster
// (ADR-014, Phases 4–5), including the cluster-coordinated scale-to-0 → offline
// slapadd → scale-up state machine. Gated by E2E_BACKUP=1 (shares the versitygw
// S3 target the backup specs deploy).
//
// Restoring into an *existing/populated* cluster (in-place rollback) is a
// separate, deferred feature (ADR-014) and is NOT covered here.
//
// The restore cluster (slapd-restore) is a single-replica, replication-off
// target — the minimal restore scenario. Data is verified over a NodePort the
// spec creates for it (E2E_NODE_IP is set by tests/e2e.sh).
var _ = Describe("restore", Label("restore"), Label("restore-bootstrap"), Ordered, func() {
	const (
		srcBackup      = "restore-e2e-src"
		restoreCluster = "slapd-restore"
		restoreDB      = "restore-db"
		restoreNPSvc   = "slapd-restore-np"
		bucket         = "slaptain-backups"
		credSecret     = "versitygw-creds"
	)
	// Fully-qualified so it resolves from the OPERATOR's namespace too (inline
	// preflight runs in the operator pod, not the workload namespace).
	endpoint := fmt.Sprintf("http://versitygw.%s.svc:7480", namespace)

	var (
		srcImages      ldapv1alpha1.SlapdImages
		srcPullSecrets []corev1.LocalObjectReference
		restoreSuffix  string
		sourceCount    int
		nodeIP         string
		anySpecFailed  bool
	)

	BeforeAll(func(ctx SpecContext) {
		if os.Getenv("E2E_BACKUP") != "1" {
			Skip("E2E_BACKUP not set; skipping restore tests")
		}
		nodeIP = os.Getenv("E2E_NODE_IP")
		Expect(nodeIP).NotTo(BeEmpty(), "E2E_NODE_IP must be set to reach the restore cluster's NodePort")

		By("waiting for the versitygw S3 server to be ready")
		Eventually(ctx, func() bool {
			return deploymentReady(k8sClient, namespace, "versitygw")
		}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(BeTrue())

		By("cloning images + pull secrets + suffix from the source cluster")
		src := &ldapv1alpha1.SlapdCluster{}
		Expect(crdClient.Get(ctx, client.ObjectKey{Name: "slapd", Namespace: namespace}, src)).To(Succeed())
		srcImages = src.Spec.Images
		srcPullSecrets = src.Spec.ImagePullSecrets

		sd := &ldapv1alpha1.SlapdDatabase{}
		Expect(crdClient.Get(ctx, client.ObjectKey{Name: dbCRName, Namespace: namespace}, sd)).To(Succeed())
		restoreSuffix = sd.Spec.Suffix
		Expect(restoreSuffix).NotTo(BeEmpty())

		By("counting source DIT entries for later comparison")
		sourceCount = len(ldapSearch(ldapConn, restoreSuffix, "(objectClass=*)", "dn"))
		Expect(sourceCount).To(BeNumerically(">", 0))
	})

	// Autopsy at failure time: the cluster is still standing here, and the spec
	// that just failed is the only context in which its pod logs still exist.
	// This flake (docs/BACKLOG.md, "bootstrapFrom restore intermittently never
	// sets restoreApplied") resisted diagnosis for exactly this reason — cleanup
	// destroyed the evidence before anyone could look.
	AfterEach(func(ctx SpecContext) {
		if os.Getenv("E2E_BACKUP") != "1" || !CurrentSpecReport().Failed() {
			return
		}
		anySpecFailed = true
		dumpRestoreAutopsy(ctx, restoreCluster, restoreDB)
	})

	AfterAll(func(ctx SpecContext) {
		if os.Getenv("E2E_BACKUP") != "1" {
			return
		}
		failed := anySpecFailed || CurrentSpecReport().Failed()
		if failed && keepOnFailure() {
			reportKeptRestoreState(restoreCluster, restoreDB, []string{restoreNPSvc},
				[]string{srcBackup})
			return
		}
		// Best-effort teardown of everything this spec created.
		_ = k8sClient.CoreV1().Services(namespace).Delete(ctx, restoreNPSvc, metav1.DeleteOptions{})
		_ = crdClient.Delete(ctx, &ldapv1alpha1.SlapdDatabase{ObjectMeta: metav1.ObjectMeta{Name: restoreDB, Namespace: namespace}})
		_ = crdClient.Delete(ctx, &ldapv1alpha1.SlapdCluster{ObjectMeta: metav1.ObjectMeta{Name: restoreCluster, Namespace: namespace}})
		_ = crdClient.Delete(ctx, &ldapv1alpha1.SlapdBackup{ObjectMeta: metav1.ObjectMeta{Name: srcBackup, Namespace: namespace}})
		deleteRestorePVCs(ctx, restoreCluster)
	})

	It("restores a backup into a fresh cluster via bootstrapFrom", func(ctx SpecContext) {
		By("creating a source backup and waiting for Completed")
		Expect(crdClient.Create(ctx, &ldapv1alpha1.SlapdBackup{
			ObjectMeta: metav1.ObjectMeta{Name: srcBackup, Namespace: namespace},
			Spec: ldapv1alpha1.SlapdBackupSpec{
				DatabaseRef: dbCRName,
				Storage: ldapv1alpha1.S3StorageSpec{
					Bucket: bucket, Endpoint: endpoint, Region: "us-east-1",
					CredentialsSecretName: credSecret,
				},
			},
		})).To(Succeed())
		Eventually(ctx, func() ldapv1alpha1.SlapdBackupPhase {
			b := &ldapv1alpha1.SlapdBackup{}
			if err := crdClient.Get(ctx, client.ObjectKey{Name: srcBackup, Namespace: namespace}, b); err != nil {
				return ""
			}
			return b.Status.Phase
		}).WithTimeout(5*time.Minute).WithPolling(5*time.Second).Should(Equal(ldapv1alpha1.BackupPhaseCompleted))

		By("creating a fresh restore cluster + database with bootstrapFrom")
		Expect(crdClient.Create(ctx, &ldapv1alpha1.SlapdCluster{
			ObjectMeta: metav1.ObjectMeta{Name: restoreCluster, Namespace: namespace},
			Spec: ldapv1alpha1.SlapdClusterSpec{
				Images:           srcImages,
				ImagePullSecrets: srcPullSecrets,
				Replicas:         1,
				Replication:      ldapv1alpha1.SlapdReplicationConfig{Enabled: false},
				LDAP:             ldapv1alpha1.SlapdLDAPConfig{TLS: ldapv1alpha1.SlapdTLSConfig{Enabled: false}},
				Persistence: ldapv1alpha1.SlapdPersistenceConfig{
					Config: ldapv1alpha1.SlapdPVCConfig{Size: "1Gi"},
					Data:   ldapv1alpha1.SlapdPVCConfig{Size: "1Gi"},
				},
			},
		})).To(Succeed())

		disabled := false
		Expect(crdClient.Create(ctx, &ldapv1alpha1.SlapdDatabase{
			ObjectMeta: metav1.ObjectMeta{Name: restoreDB, Namespace: namespace},
			Spec: ldapv1alpha1.SlapdDatabaseSpec{
				ClusterRef:  restoreCluster,
				Suffix:      restoreSuffix,
				Replication: ldapv1alpha1.DatabaseReplicationConfig{Enabled: &disabled},
				BootstrapFrom: &ldapv1alpha1.BootstrapSource{
					BackupRef: srcBackup,
				},
			},
		})).To(Succeed())

		By("waiting for the restore state machine to complete (scale-to-0 → slapadd → scale-up)")
		Eventually(ctx, func() bool {
			sd := &ldapv1alpha1.SlapdDatabase{}
			if err := crdClient.Get(ctx, client.ObjectKey{Name: restoreDB, Namespace: namespace}, sd); err != nil {
				return false
			}
			return sd.Status.RestoreApplied
		}).WithTimeout(8*time.Minute).WithPolling(5*time.Second).Should(BeTrue(),
			"restore-db should reach restoreApplied=true")

		By("waiting for the cluster to return to Running with restore cleared")
		Eventually(ctx, func() bool {
			sc := &ldapv1alpha1.SlapdCluster{}
			if err := crdClient.Get(ctx, client.ObjectKey{Name: restoreCluster, Namespace: namespace}, sc); err != nil {
				return false
			}
			return sc.Status.Phase == ldapv1alpha1.PhaseRunning && sc.Status.Restore == nil
		}).WithTimeout(3*time.Minute).WithPolling(5*time.Second).Should(BeTrue())

		By("verifying the restored DIT matches the source")
		addr := exposeRestoreNodePort(ctx, restoreCluster, restoreNPSvc, nodeIP)
		credPW := readSecretKey(ctx, restoreDB+"-credentials", "root-password")
		conn := retryConnectLDAP(ctx, addr, restoreSuffix, credPW)
		defer conn.Close()

		entries := ldapSearch(conn, restoreSuffix, "(objectClass=*)", "dn")
		Expect(entries).To(HaveLen(sourceCount), "restored entry count should equal the source")
		Expect(ldapExists(conn, "ou=People,"+restoreSuffix)).To(BeTrue(), "ou=People should be restored")
	})

	// ADR-025: an artifact whose suffix entry is a GLUE entry (the multi-site
	// seed-race residue — objectClass top+glue, no RDN attribute) has a correct
	// DN line, so a DN-line-only preflight passes it and slapadd then fails with
	// "(65) attribute 'dc' not allowed" AFTER the cluster has been scaled to 0
	// (observed live 2026-09-13: three restore Jobs at BackoffLimitExceeded, the
	// cluster held at 0 replicas). Destroy-last demands the rejection happens in
	// preflight, while the cluster is still serving.
	//
	// Against pre-ADR-025 code this spec is red by construction: preflight
	// passes the glue artifact, the machine scales to 0, and both the
	// stays-in-Preflight and the never-scales-down assertions fail.
	It("rejects a glue-suffix artifact in preflight without scaling down", func(ctx SpecContext) {
		const glueDB = "restore-glue-db"
		const glueKey = "glue-e2e/artifact.ldif.gz"

		By("planting a hand-crafted glue-suffix artifact in the S3 backend")
		glueLDIF := "dn: " + restoreSuffix + "\n" +
			"objectClass: top\n" +
			"objectClass: glue\n" +
			"structuralObjectClass: glue\n" +
			"entryUUID: 1a914f3a-43f0-1041-9ed3-896165d17446\n" +
			"entryCSN: 20260913185308.559566Z#000000#065#000000\n" +
			"createTimestamp: 20260913185307Z\n" +
			"\n" +
			"dn: ou=People," + restoreSuffix + "\n" +
			"objectClass: organizationalUnit\n" +
			"ou: People\n"
		putVersitygwObjectGzip(ctx, bucket, glueKey, []byte(glueLDIF))

		By("creating a bootstrapFrom database pointing at the glue artifact")
		disabled := false
		Expect(crdClient.Create(ctx, &ldapv1alpha1.SlapdDatabase{
			ObjectMeta: metav1.ObjectMeta{Name: glueDB, Namespace: namespace},
			Spec: ldapv1alpha1.SlapdDatabaseSpec{
				ClusterRef:  restoreCluster,
				Suffix:      restoreSuffix,
				Replication: ldapv1alpha1.DatabaseReplicationConfig{Enabled: &disabled},
				BootstrapFrom: &ldapv1alpha1.BootstrapSource{
					S3: &ldapv1alpha1.BootstrapS3Source{
						Storage: ldapv1alpha1.S3StorageSpec{
							Bucket: bucket, Endpoint: endpoint, Region: "us-east-1",
							CredentialsSecretName: credSecret,
						},
						Key: glueKey,
					},
				},
			},
		})).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			if CurrentSpecReport().Failed() && keepOnFailure() {
				return // keep the evidence, like the rest of the restore suite
			}
			_ = crdClient.Delete(ctx, &ldapv1alpha1.SlapdDatabase{
				ObjectMeta: metav1.ObjectMeta{Name: glueDB, Namespace: namespace}})
		})

		By("waiting for preflight to reject the artifact as a glue suffix")
		Eventually(ctx, func() string {
			sc := &ldapv1alpha1.SlapdCluster{}
			if err := crdClient.Get(ctx, client.ObjectKey{Name: restoreCluster, Namespace: namespace}, sc); err != nil {
				return ""
			}
			if sc.Status.Restore == nil || sc.Status.Restore.Phase != ldapv1alpha1.RestorePreflight {
				return ""
			}
			return sc.Status.Restore.Message
		}).WithTimeout(5*time.Minute).WithPolling(5*time.Second).Should(ContainSubstring("glue"),
			"preflight should reject the glue-suffix artifact by name")

		By("verifying the machine never leaves Preflight and never scales down")
		Consistently(ctx, func() bool {
			sc := &ldapv1alpha1.SlapdCluster{}
			if err := crdClient.Get(ctx, client.ObjectKey{Name: restoreCluster, Namespace: namespace}, sc); err != nil {
				return false
			}
			if sc.Status.Restore == nil || sc.Status.Restore.Phase != ldapv1alpha1.RestorePreflight {
				return false
			}
			sts, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, restoreCluster, metav1.GetOptions{})
			if err != nil || sts.Spec.Replicas == nil {
				return false
			}
			return *sts.Spec.Replicas == 1
		}).WithTimeout(45*time.Second).WithPolling(5*time.Second).Should(BeTrue(),
			"the cluster must keep serving (replicas=1, phase Preflight) while the artifact is invalid — destroy-last")

		gdb := &ldapv1alpha1.SlapdDatabase{}
		Expect(crdClient.Get(ctx, client.ObjectKey{Name: glueDB, Namespace: namespace}, gdb)).To(Succeed())
		Expect(gdb.Status.RestoreApplied).To(BeFalse(), "a rejected artifact must never mark restoreApplied")
	})
})

// putVersitygwObjectGzip gzips content and writes it as an object into the
// versitygw POSIX backend (/data/<bucket>/<key>) via an ephemeral container —
// the write-side sibling of artifactGrepExitCode, and versitygw-specific for
// the same reason: it keeps the e2e module free of an S3 client and request
// signing. versitygw's posix backend serves pre-existing files as objects, so
// a file planted this way is GETtable through its S3 API.
func putVersitygwObjectGzip(ctx context.Context, bucket, objectKey string, content []byte) {
	GinkgoHelper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, err := gz.Write(content)
	Expect(err).NotTo(HaveOccurred())
	Expect(gz.Close()).To(Succeed())
	b64 := base64.StdEncoding.EncodeToString(buf.Bytes())

	pods, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=versitygw",
	})
	Expect(err).NotTo(HaveOccurred(), "list versitygw pods")
	var podName string
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning {
			podName = p.Name
			break
		}
	}
	Expect(podName).NotTo(BeEmpty(), "no Running versitygw pod to plant the artifact on")

	path := "/data/" + bucket + "/" + objectKey
	ecName := fmt.Sprintf("artifact-put-%d", time.Now().UnixNano()%1000000)

	pod, err := k8sClient.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred(), "get versitygw pod")
	pod.Spec.EphemeralContainers = append(pod.Spec.EphemeralContainers, corev1.EphemeralContainer{
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name:  ecName,
			Image: "busybox:stable",
			Command: []string{"sh", "-c",
				`mkdir -p "$(dirname "$F")" && printf %s "$B64" | base64 -d > "$F"`},
			Env: []corev1.EnvVar{
				{Name: "F", Value: path},
				{Name: "B64", Value: b64},
			},
			VolumeMounts:             []corev1.VolumeMount{{Name: "data", MountPath: "/data"}},
			TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		},
	})
	_, err = k8sClient.CoreV1().Pods(namespace).UpdateEphemeralContainers(ctx, podName, pod, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred(), "attach artifact-put container %s to %s", ecName, podName)

	Eventually(ctx, func() int32 {
		p, err := k8sClient.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return -1
		}
		for _, st := range p.Status.EphemeralContainerStatuses {
			if st.Name == ecName && st.State.Terminated != nil {
				return st.State.Terminated.ExitCode
			}
		}
		return -1
	}).WithTimeout(2*time.Minute).WithPolling(3*time.Second).Should(BeZero(),
		"artifact-put container %s must terminate cleanly", ecName)
}

// exposeRestoreNodePort creates a NodePort Service for the restore cluster's RW
// pods and returns a host:port reachable via E2E_NODE_IP.
func exposeRestoreNodePort(ctx SpecContext, cluster, svcName, nodeIP string) string {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: svcName, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeNodePort,
			Selector: map[string]string{
				"app.kubernetes.io/name":     "slapd",
				"app.kubernetes.io/instance": cluster,
			},
			Ports: []corev1.ServicePort{{
				Name:       "ldap",
				Port:       389,
				TargetPort: intstr.FromInt(1024),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
	created, err := k8sClient.CoreV1().Services(namespace).Create(ctx, svc, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "create restore NodePort service")
	np := created.Spec.Ports[0].NodePort
	Expect(np).NotTo(BeZero(), "NodePort should be assigned")
	return fmt.Sprintf("%s:%d", nodeIP, np)
}

// ── Failure inspectability (docs/BACKLOG.md: the bootstrapFrom restore flake) ──
//
// The restore specs create a whole throwaway cluster and delete it again in
// their AfterAll. When one of them fails, that teardown is also the destruction
// of the only evidence: the pod that would not start, its init-container log,
// and the CR statuses. Three pieces close that hole:
//
//	keepOnFailure()           — skip teardown when the spec failed (default on)
//	dumpRestoreAutopsy()      — dump the evidence at failure time, into the
//	                            spec's own output, before anything is deleted
//	deleteRestorePVCs()       — on a green run, also remove the
//	                            volumeClaimTemplates PVCs, which are never
//	                            garbage-collected with the StatefulSet
//
// keepOnFailure and the autopsy are deliberately independent: the autopsy runs
// even with E2E_KEEP_ON_FAILURE=0, so a CI run that must leave nothing behind
// still prints its own post-mortem.

// keepOnFailure reports whether a failed restore spec should leave its cluster,
// database, services and PVCs standing for inspection. Default: keep. Set
// E2E_KEEP_ON_FAILURE=0 to restore unconditional teardown (CI, or a loop that
// must not have its next iteration poisoned by the previous one's corpse).
func keepOnFailure() bool { return os.Getenv("E2E_KEEP_ON_FAILURE") != "0" }

// reportKeptRestoreState prints exactly what was left behind and how to look at
// it. Printed to both GinkgoWriter (spec output) and stdout, because a kept
// cluster is a fact about the *operator's* state that outlives this test run.
func reportKeptRestoreState(cluster, database string, services, backups []string) {
	var b strings.Builder
	fmt.Fprintf(&b, "\n=== KEEPING FAILED RESTORE STATE FOR INSPECTION (E2E_KEEP_ON_FAILURE) ===\n")
	fmt.Fprintf(&b, "namespace:      %s\n", namespace)
	fmt.Fprintf(&b, "SlapdCluster:   %s\n", cluster)
	fmt.Fprintf(&b, "SlapdDatabase:  %s\n", database)
	if len(services) > 0 {
		fmt.Fprintf(&b, "Services:       %s\n", strings.Join(services, ", "))
	}
	if len(backups) > 0 {
		fmt.Fprintf(&b, "SlapdBackups:   %s\n", strings.Join(backups, ", "))
	}
	fmt.Fprintf(&b, "PVCs:           app.kubernetes.io/instance=%s (config-%s-N, data-%s-N, …)\n",
		cluster, cluster, cluster)
	fmt.Fprintf(&b, "\ninspect it:\n")
	fmt.Fprintf(&b, "  slctl debug-dump -n %s %s\n", namespace, cluster)
	fmt.Fprintf(&b, "  kubectl -n %s describe pod %s-0\n", namespace, cluster)
	// The init container is named "init" (not "slapd-init" — that is the image).
	fmt.Fprintf(&b, "  kubectl -n %s logs %s-0 -c init\n", namespace, cluster)
	fmt.Fprintf(&b, "  kubectl -n %s logs %s-0 -c slapd\n", namespace, cluster)
	fmt.Fprintf(&b, "\nclean it up when done:\n")
	fmt.Fprintf(&b, "  kubectl -n %s delete slapddatabase %s; kubectl -n %s delete slapdcluster %s\n",
		namespace, database, namespace, cluster)
	fmt.Fprintf(&b, "  kubectl -n %s delete pvc -l app.kubernetes.io/instance=%s\n", namespace, cluster)
	fmt.Fprintf(&b, "=========================================================================\n")
	fmt.Fprint(GinkgoWriter, b.String())
	fmt.Print(b.String())
}

// dumpRestoreAutopsy writes the post-mortem of a restore cluster into the
// current spec's output: CR statuses, pod phase and per-container state, the
// init-container and slapd logs, and the namespace's recent events for the
// cluster. Everything is best-effort — a missing piece is reported inline, never
// fatal, because this runs while the spec has *already* failed and must not
// replace the real failure message with one of its own.
func dumpRestoreAutopsy(ctx context.Context, cluster, database string) {
	out := GinkgoWriter
	fmt.Fprintf(out, "\n=== RESTORE AUTOPSY: cluster=%s database=%s ns=%s ===\n", cluster, database, namespace)

	sc := &ldapv1alpha1.SlapdCluster{}
	if err := crdClient.Get(ctx, client.ObjectKey{Name: cluster, Namespace: namespace}, sc); err != nil {
		fmt.Fprintf(out, "--- SlapdCluster %s: %v\n", cluster, err)
	} else {
		fmt.Fprintf(out, "--- SlapdCluster %s status ---\n%s", cluster, toYAML(sc.Status))
	}
	sd := &ldapv1alpha1.SlapdDatabase{}
	if err := crdClient.Get(ctx, client.ObjectKey{Name: database, Namespace: namespace}, sd); err != nil {
		fmt.Fprintf(out, "--- SlapdDatabase %s: %v\n", database, err)
	} else {
		fmt.Fprintf(out, "--- SlapdDatabase %s status ---\n%s", database, toYAML(sd.Status))
	}

	pods, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/instance=" + cluster,
	})
	if err != nil {
		fmt.Fprintf(out, "--- pods: list failed: %v\n", err)
		return
	}
	if len(pods.Items) == 0 {
		fmt.Fprintf(out, "--- pods: NONE with app.kubernetes.io/instance=%s "+
			"(scheduling/StatefulSet problem, not a slapd problem)\n", cluster)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		fmt.Fprintf(out, "--- pod %s: phase=%s node=%s podIP=%s\n", p.Name, p.Status.Phase, p.Spec.NodeName, p.Status.PodIP)
		for _, c := range p.Status.Conditions {
			fmt.Fprintf(out, "      condition %s=%s %s %s\n", c.Type, c.Status, c.Reason, c.Message)
		}
		for _, cs := range p.Status.InitContainerStatuses {
			fmt.Fprintf(out, "      init  %-14s ready=%t restarts=%d state=%s\n",
				cs.Name, cs.Ready, cs.RestartCount, describeContainerState(cs.State))
		}
		for _, cs := range p.Status.ContainerStatuses {
			fmt.Fprintf(out, "      main  %-14s ready=%t restarts=%d state=%s last=%s\n",
				cs.Name, cs.Ready, cs.RestartCount, describeContainerState(cs.State),
				describeContainerState(cs.LastTerminationState))
		}
		for _, c := range p.Spec.InitContainers {
			dumpContainerLog(ctx, p.Name, c.Name, false)
		}
		for _, c := range p.Spec.Containers {
			dumpContainerLog(ctx, p.Name, c.Name, false)
			// A crash-looping slapd has its evidence in the *previous* container.
			for _, cs := range p.Status.ContainerStatuses {
				if cs.Name == c.Name && cs.RestartCount > 0 {
					dumpContainerLog(ctx, p.Name, c.Name, true)
				}
			}
		}
	}

	dumpClusterEvents(ctx, cluster)
	fmt.Fprintf(out, "=== END RESTORE AUTOPSY (%s) ===\n\n", cluster)
}

// dumpContainerLog tails one container's log into the spec output.
func dumpContainerLog(ctx context.Context, pod, container string, previous bool) {
	tail := int64(300)
	label := container
	if previous {
		label += " (previous)"
	}
	req := k8sClient.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: container, TailLines: &tail, Previous: previous,
	})
	body, err := req.DoRaw(ctx)
	if err != nil {
		fmt.Fprintf(GinkgoWriter, "--- logs %s/%s: %v\n", pod, label, err)
		return
	}
	if len(body) == 0 {
		fmt.Fprintf(GinkgoWriter, "--- logs %s/%s: EMPTY (container produced no output)\n", pod, label)
		return
	}
	fmt.Fprintf(GinkgoWriter, "--- logs %s/%s (last %d lines) ---\n%s\n", pod, label, tail, body)
}

// dumpClusterEvents prints the namespace's events whose involved object belongs
// to the cluster, oldest first. Events are how "pod never started" explains
// itself (FailedScheduling, FailedMount, ImagePullBackOff, …).
func dumpClusterEvents(ctx context.Context, cluster string) {
	evs, err := k8sClient.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(GinkgoWriter, "--- events: list failed: %v\n", err)
		return
	}
	var rows []corev1.Event
	for _, e := range evs.Items {
		n := e.InvolvedObject.Name
		if n == cluster || strings.HasPrefix(n, cluster+"-") {
			rows = append(rows, e)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return eventTime(rows[i]).Before(eventTime(rows[j])) })
	fmt.Fprintf(GinkgoWriter, "--- events for %s* (%d) ---\n", cluster, len(rows))
	for _, e := range rows {
		fmt.Fprintf(GinkgoWriter, "  %s %-7s %-24s %-22s %s\n",
			eventTime(e).Format(time.RFC3339), e.Type,
			e.InvolvedObject.Kind+"/"+e.InvolvedObject.Name, e.Reason, e.Message)
	}
}

func eventTime(e corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if e.EventTime.Time.IsZero() {
		return e.CreationTimestamp.Time
	}
	return e.EventTime.Time
}

func describeContainerState(s corev1.ContainerState) string {
	switch {
	case s.Running != nil:
		return fmt.Sprintf("running(since %s)", s.Running.StartedAt.Format(time.RFC3339))
	case s.Waiting != nil:
		return fmt.Sprintf("waiting(%s: %s)", s.Waiting.Reason, s.Waiting.Message)
	case s.Terminated != nil:
		return fmt.Sprintf("terminated(exit=%d %s: %s)",
			s.Terminated.ExitCode, s.Terminated.Reason, s.Terminated.Message)
	default:
		return "none"
	}
}

func toYAML(v any) string {
	b, err := yaml.Marshal(v)
	if err != nil {
		return fmt.Sprintf("<marshal failed: %v>\n", err)
	}
	return string(b)
}

// deleteRestorePVCs removes a restore cluster's volumeClaimTemplates PVCs.
// StatefulSet PVCs are never garbage-collected (no
// persistentVolumeClaimRetentionPolicy is set, and ADR-005's Retain default is
// deliberate), so without this every restore run leaks its volumes and the next
// run rebinds populated ones.
func deleteRestorePVCs(ctx context.Context, cluster string) {
	err := k8sClient.CoreV1().PersistentVolumeClaims(namespace).DeleteCollection(ctx,
		metav1.DeleteOptions{},
		metav1.ListOptions{LabelSelector: "app.kubernetes.io/instance=" + cluster})
	if err != nil {
		fmt.Fprintf(GinkgoWriter, "warning: deleting PVCs for %s failed: %v\n", cluster, err)
	}
}

// readSecretKey reads a single key from a Secret in the test namespace.
func readSecretKey(ctx SpecContext, name, key string) string {
	s, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred(), "get secret %s", name)
	v := string(s.Data[key])
	Expect(v).NotTo(BeEmpty(), "secret %s key %s must not be empty", name, key)
	return v
}
