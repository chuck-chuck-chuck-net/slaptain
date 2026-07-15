package e2e_test

import (
	"fmt"
	"os"
	"strings"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// Scale-up e2e: exercises the standalone → HA transition (replicas 1→2 +
// replication.enabled flip on a LIVE cluster). Gated by E2E_SCALEUP=1.
//
// This transition is a distinct code path from a fresh replicated deploy:
// pod-0's cn=config was bootstrapped WITHOUT replication, and bootstrap.sh
// never touches an existing config again. Three operator-side runtime
// mechanisms must therefore kick in (docs/reconcile-loop-fixes.md 2026-07-15):
//
//  1. ensureModulesLoaded — pod-0 lacks the accesslog/syncprov modules;
//     without them every overlay add fails and pod-0 never becomes a
//     provider (writes on pod-0 silently don't replicate out).
//  2. SlapdSchema's SlapdCluster watch — the custom schema reached Applied
//     when the cluster had one pod; pod-1 must still receive it, or entries
//     using the custom objectClass fail syncrepl on the consumer (rc 21).
//  3. ensureServerIDs — pod-0 has no olcServerID (stamps CSNs as sid 0,
//     outside the ADR-011 scheme); both pods must converge on the full list.
//
// The assertions are behavioural where possible: an entry carrying the
// custom objectClass written on pod-0 BEFORE the scale-up must arrive on
// pod-1 (proves 1+2 together), and a write on pod-1 must flow back to pod-0
// (proves the mesh is symmetric). ServerIDs are asserted directly on
// cn=config.
var _ = Describe("scale-up", Label("scaleup"), Ordered, func() {
	const (
		scaleCluster = "slapd-scaleup"
		scaleDB      = "scaleup-db"
		scaleSchema  = "scaleup-schema"
		scaleSuffix  = "dc=scaleup,dc=example"
	)

	var (
		srcImages      ldapv1alpha1.SlapdImages
		srcPullSecrets []corev1.LocalObjectReference
		nodeIP         string
		credPW         string
	)

	BeforeAll(func(ctx SpecContext) {
		if os.Getenv("E2E_SCALEUP") != "1" {
			Skip("E2E_SCALEUP not set; skipping scale-up tests")
		}
		nodeIP = os.Getenv("E2E_NODE_IP")
		Expect(nodeIP).NotTo(BeEmpty(), "E2E_NODE_IP must be set to reach the scale-up cluster's NodePorts")

		By("cloning images + pull secrets from the source cluster")
		src := &ldapv1alpha1.SlapdCluster{}
		Expect(crdClient.Get(ctx, client.ObjectKey{Name: "slapd", Namespace: namespace}, src)).To(Succeed())
		srcImages = src.Spec.Images
		srcPullSecrets = src.Spec.ImagePullSecrets
	})

	AfterAll(func(ctx SpecContext) {
		if os.Getenv("E2E_SCALEUP") != "1" {
			return
		}
		// Best-effort teardown. PVCs are NOT owned by the cluster CR
		// (volumeClaimTemplates outlive the StatefulSet by design), so they
		// must be deleted explicitly or a re-run bootstraps against stale
		// config — the exact failure mode this spec exists to prevent.
		for _, ordinal := range []int{0, 1} {
			_ = k8sClient.CoreV1().Services(namespace).Delete(ctx,
				fmt.Sprintf("%s-np-%d", scaleCluster, ordinal), metav1.DeleteOptions{})
		}
		_ = crdClient.Delete(ctx, &ldapv1alpha1.SlapdSchema{ObjectMeta: metav1.ObjectMeta{Name: scaleSchema, Namespace: namespace}})
		_ = crdClient.Delete(ctx, &ldapv1alpha1.SlapdDatabase{ObjectMeta: metav1.ObjectMeta{Name: scaleDB, Namespace: namespace}})
		_ = crdClient.Delete(ctx, &ldapv1alpha1.SlapdCluster{ObjectMeta: metav1.ObjectMeta{Name: scaleCluster, Namespace: namespace}})
		for _, tpl := range []string{"config", "data", "accesslog"} {
			for _, ordinal := range []int{0, 1} {
				_ = k8sClient.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx,
					fmt.Sprintf("%s-%s-%d", tpl, scaleCluster, ordinal), metav1.DeleteOptions{})
			}
		}
	})

	It("runs standalone with a custom schema and seeded data", func(ctx SpecContext) {
		By("creating a single-replica cluster with replication OFF")
		Expect(crdClient.Create(ctx, &ldapv1alpha1.SlapdCluster{
			ObjectMeta: metav1.ObjectMeta{Name: scaleCluster, Namespace: namespace},
			Spec: ldapv1alpha1.SlapdClusterSpec{
				Images:           srcImages,
				ImagePullSecrets: srcPullSecrets,
				Replicas:         1,
				Replication:      ldapv1alpha1.SlapdReplicationConfig{Enabled: false},
				LDAP:             ldapv1alpha1.SlapdLDAPConfig{TLS: ldapv1alpha1.SlapdTLSConfig{Enabled: false}},
				Persistence: ldapv1alpha1.SlapdPersistenceConfig{
					Config:    ldapv1alpha1.SlapdPVCConfig{Size: "1Gi"},
					Data:      ldapv1alpha1.SlapdPVCConfig{Size: "1Gi"},
					Accesslog: ldapv1alpha1.SlapdPVCConfig{Size: "1Gi"},
				},
			},
		})).To(Succeed())

		By("declaring a custom schema (applied while the cluster has ONE pod)")
		Expect(crdClient.Create(ctx, &ldapv1alpha1.SlapdSchema{
			ObjectMeta: metav1.ObjectMeta{Name: scaleSchema, Namespace: namespace},
			Spec: ldapv1alpha1.SlapdSchemaSpec{
				ClusterRef: scaleCluster,
				AttributeTypes: []string{
					`( 1.3.6.1.4.1.99999.3.1.1 NAME 'scaleupQuota' DESC 'e2e scale-up marker attribute' EQUALITY integerMatch SYNTAX 1.3.6.1.4.1.1466.115.121.1.27 SINGLE-VALUE )`,
				},
				ObjectClasses: []string{
					`( 1.3.6.1.4.1.99999.3.2.1 NAME 'scaleupUser' DESC 'e2e scale-up marker class' SUP inetOrgPerson STRUCTURAL MAY ( scaleupQuota ) )`,
				},
			},
		})).To(Succeed())

		By("declaring the database with delta-syncrepl intent")
		enabled := true
		ridBase := int32(300)
		Expect(crdClient.Create(ctx, &ldapv1alpha1.SlapdDatabase{
			ObjectMeta: metav1.ObjectMeta{Name: scaleDB, Namespace: namespace},
			Spec: ldapv1alpha1.SlapdDatabaseSpec{
				ClusterRef: scaleCluster,
				Suffix:     scaleSuffix,
				Replication: ldapv1alpha1.DatabaseReplicationConfig{
					Enabled: &enabled,
					RIDBase: &ridBase,
				},
				Seed: &ldapv1alpha1.DatabaseSeedConfig{
					Entries: []string{
						"dn: " + scaleSuffix + "\nobjectClass: top\nobjectClass: dcObject\nobjectClass: organization\no: scaleup\ndc: scaleup\n",
						"dn: ou=People," + scaleSuffix + "\nobjectClass: organizationalUnit\nou: People\n",
					},
				},
			},
		})).To(Succeed())

		By("waiting for the database and schema to be ready on the single pod")
		Eventually(ctx, func() bool {
			return slapdDatabaseRunning(crdClient, namespace, scaleDB)
		}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(BeTrue())
		Eventually(ctx, func() bool {
			return slapdSchemaApplied(crdClient, namespace, scaleSchema)
		}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(BeTrue())

		By("writing an entry that USES the custom schema on pod-0")
		credPW = readSecretKey(ctx, scaleDB+"-credentials", "root-password")
		addr0 := exposePodNodePort(ctx, scaleCluster, scaleCluster+"-0", scaleCluster+"-np-0", nodeIP)
		conn0 := retryConnectLDAP(ctx, addr0, scaleSuffix, credPW)
		defer conn0.Close()

		By("verifying the standalone pod already carries sid 1 (sid-1-per-default)")
		configPW := readSecretKey(ctx, scaleCluster+"-config-password", "root-password")
		Eventually(ctx, func() []int {
			return readServerIDSids(ctx, nodeIP, scaleCluster, 0, configPW)
		}).WithTimeout(2*time.Minute).WithPolling(5*time.Second).Should(
			ConsistOf(1),
			"a standalone cluster must be born with serverID 1, not sid 0")

		req := ldap.NewAddRequest("uid=scaletest,ou=People,"+scaleSuffix, nil)
		req.Attribute("objectClass", []string{"scaleupUser"})
		req.Attribute("uid", []string{"scaletest"})
		req.Attribute("cn", []string{"Scale Test"})
		req.Attribute("sn", []string{"Test"})
		req.Attribute("scaleupQuota", []string{"12345"})
		ldapAdd(conn0, req)
	})

	It("scales to 2-way multi-master and converges", func(ctx SpecContext) {
		By("flipping replicas to 2 and replication.enabled to true (the demo Act-6 transition)")
		Eventually(ctx, func() error {
			sc := &ldapv1alpha1.SlapdCluster{}
			if err := crdClient.Get(ctx, client.ObjectKey{Name: scaleCluster, Namespace: namespace}, sc); err != nil {
				return err
			}
			sc.Spec.Replicas = 2
			sc.Spec.Replication.Enabled = true
			return crdClient.Update(ctx, sc)
		}).WithTimeout(30 * time.Second).WithPolling(2 * time.Second).Should(Succeed())

		By("waiting for both pods to be ready and the cluster Running")
		Eventually(ctx, func() bool {
			return statefulSetReady(k8sClient, namespace, scaleCluster) &&
				podReady(k8sClient, namespace, scaleCluster+"-1")
		}).WithTimeout(6 * time.Minute).WithPolling(5 * time.Second).Should(BeTrue())
		Eventually(ctx, func() ldapv1alpha1.SlapdClusterPhase {
			sc := &ldapv1alpha1.SlapdCluster{}
			if err := crdClient.Get(ctx, client.ObjectKey{Name: scaleCluster, Namespace: namespace}, sc); err != nil {
				return ""
			}
			return sc.Status.Phase
		}).WithTimeout(3 * time.Minute).WithPolling(5 * time.Second).Should(Equal(ldapv1alpha1.PhaseRunning))

		By("verifying the custom schema reached the NEW pod (SlapdCluster watch on the schema controller)")
		Eventually(ctx, func() []string {
			ss := &ldapv1alpha1.SlapdSchema{}
			if err := crdClient.Get(ctx, client.ObjectKey{Name: scaleSchema, Namespace: namespace}, ss); err != nil {
				return nil
			}
			return ss.Status.AppliedToPods
		}).WithTimeout(3*time.Minute).WithPolling(5*time.Second).Should(
			ContainElements(scaleCluster+"-0", scaleCluster+"-1"),
			"schema must be re-applied to pods created after it first reached Applied")

		By("verifying the pre-scale-up entry replicated pod-0 → pod-1 (pod-0 became a real provider)")
		addr1 := exposePodNodePort(ctx, scaleCluster, scaleCluster+"-1", scaleCluster+"-np-1", nodeIP)
		conn1 := retryConnectLDAP(ctx, addr1, scaleSuffix, credPW)
		defer conn1.Close()
		// Initial sync waits on the consumer's syncrepl retry interval
		// (default 60s) after pod-0's modules/overlays land, so be generous.
		Eventually(ctx, func() bool {
			return ldapExists(conn1, "uid=scaletest,ou=People,"+scaleSuffix)
		}).WithTimeout(5*time.Minute).WithPolling(5*time.Second).Should(BeTrue(),
			"entry with custom objectClass written before scale-up must arrive on pod-1")

		By("verifying the reverse direction: a write on pod-1 arrives on pod-0")
		req := ldap.NewAddRequest("uid=scaletest2,ou=People,"+scaleSuffix, nil)
		req.Attribute("objectClass", []string{"scaleupUser"})
		req.Attribute("uid", []string{"scaletest2"})
		req.Attribute("cn", []string{"Scale Test Two"})
		req.Attribute("sn", []string{"Test"})
		req.Attribute("scaleupQuota", []string{"67890"})
		ldapAdd(conn1, req)

		// Pod-0's NodePort service persists from the first It; re-read its port.
		svc, err := k8sClient.CoreV1().Services(namespace).Get(ctx, scaleCluster+"-np-0", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		conn0 := retryConnectLDAP(ctx, fmt.Sprintf("%s:%d", nodeIP, svc.Spec.Ports[0].NodePort), scaleSuffix, credPW)
		defer conn0.Close()
		Eventually(ctx, func() bool {
			return ldapExists(conn0, "uid=scaletest2,ou=People,"+scaleSuffix)
		}).WithTimeout(3 * time.Minute).WithPolling(5 * time.Second).Should(BeTrue())

		By("verifying olcServerID extended to [1 2] on BOTH pods (including the pre-existing one)")
		configPW := readSecretKey(ctx, scaleCluster+"-config-password", "root-password")
		for _, ordinal := range []int{0, 1} {
			Eventually(ctx, func() []int {
				return readServerIDSids(ctx, nodeIP, scaleCluster, ordinal, configPW)
			}).WithTimeout(2*time.Minute).WithPolling(5*time.Second).Should(
				ConsistOf(1, 2),
				"pod-%d must carry the full ADR-011 serverID list", ordinal)
		}
	})
})

// readServerIDSids binds as cn=admin,cn=config on one pod of the scale-up
// cluster (via its per-pod NodePort service) and returns the numeric sids
// from the global olcServerID list. Returns nil on any error so callers can
// poll with Eventually.
func readServerIDSids(ctx SpecContext, nodeIP, cluster string, ordinal int, configPW string) []int {
	svc, err := k8sClient.CoreV1().Services(namespace).Get(ctx,
		fmt.Sprintf("%s-np-%d", cluster, ordinal), metav1.GetOptions{})
	if err != nil {
		return nil
	}
	conn, err := ldap.DialURL(fmt.Sprintf("ldap://%s:%d", nodeIP, svc.Spec.Ports[0].NodePort))
	if err != nil {
		return nil
	}
	defer conn.Close()
	if err := conn.Bind("cn=admin,cn=config", configPW); err != nil {
		return nil
	}
	sr, err := conn.Search(ldap.NewSearchRequest(
		"cn=config", ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		0, 0, false, "(objectClass=*)", []string{"olcServerID"}, nil,
	))
	if err != nil || len(sr.Entries) == 0 {
		return nil
	}
	var sids []int
	for _, v := range sr.Entries[0].GetAttributeValues("olcServerID") {
		var sid int
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &sid); err == nil {
			sids = append(sids, sid)
		}
	}
	return sids
}
