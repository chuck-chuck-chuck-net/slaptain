package e2e_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The certificate gate (ADR-029).
//
// Before the gate, a SlapdCluster naming a TLS Secret that did not exist got a
// StatefulSet whose pods wedged in ContainerCreating, while the CR itself said
// nothing: the only evidence was in `kubectl describe pod`. This spec pins both
// halves of the fix, and the second half is the one a unit test cannot show —
// that the cluster proceeds ON ITS OWN once the Secret appears, with no edit to
// the CR and no operator restart.
//
// Ungated: it needs no storage class, no images that must actually run, and no
// extra infrastructure. Nothing here waits for a pod to become ready — the
// StatefulSet's existence is what the gate controls, and that is what is
// asserted, which keeps the spec fast and independent of image availability.
var _ = Describe("TLS certificate gate", Label("tls-gate"), Ordered, func() {
	const (
		gateCluster = "slapd-tlsgate"
		gateSecret  = "slapd-tlsgate-tls"
	)

	var srcImages ldapv1alpha1.SlapdImages

	BeforeAll(func(ctx SpecContext) {
		By("cloning images from the source cluster so the StatefulSet is well-formed")
		src := &ldapv1alpha1.SlapdCluster{}
		Expect(crdClient.Get(ctx, client.ObjectKey{Name: "slapd", Namespace: namespace}, src)).To(Succeed())
		srcImages = src.Spec.Images
	})

	AfterAll(func(ctx SpecContext) {
		_ = crdClient.Delete(ctx, &ldapv1alpha1.SlapdCluster{
			ObjectMeta: metav1.ObjectMeta{Name: gateCluster, Namespace: namespace}})
		_ = k8sClient.CoreV1().Secrets(namespace).Delete(ctx, gateSecret, metav1.DeleteOptions{})
		// volumeClaimTemplates outlive the StatefulSet by design.
		for _, tpl := range []string{"config", "data", "accesslog"} {
			_ = k8sClient.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx,
				tpl+"-"+gateCluster+"-0", metav1.DeleteOptions{})
		}
	})

	It("withholds the StatefulSet and says why, then proceeds when the Secret appears", func(ctx SpecContext) {
		By("creating a cluster whose TLS Secret does not exist")
		Expect(crdClient.Create(ctx, &ldapv1alpha1.SlapdCluster{
			ObjectMeta: metav1.ObjectMeta{Name: gateCluster, Namespace: namespace},
			Spec: ldapv1alpha1.SlapdClusterSpec{
				Images:   srcImages,
				Replicas: 1,
				LDAP: ldapv1alpha1.SlapdLDAPConfig{
					TLS: ldapv1alpha1.SlapdTLSConfig{Enabled: true, SecretName: gateSecret},
				},
			},
		})).To(Succeed())

		By("reporting TLSReady=False/SecretMissing, naming the Secret to create")
		Eventually(func(g Gomega) {
			sc := &ldapv1alpha1.SlapdCluster{}
			g.Expect(crdClient.Get(ctx, client.ObjectKey{Name: gateCluster, Namespace: namespace}, sc)).To(Succeed())
			cond := findCondition(sc.Status.Conditions, "TLSReady")
			g.Expect(cond).NotTo(BeNil(), "the cluster must carry a TLSReady condition")
			g.Expect(string(cond.Status)).To(Equal("False"))
			g.Expect(cond.Reason).To(Equal("SecretMissing"))
			// The message is the whole point: ContainerCreating was already a
			// False signal, it just did not say what to do about it.
			g.Expect(cond.Message).To(ContainSubstring(gateSecret))
		}).WithTimeout(90 * time.Second).WithPolling(3 * time.Second).Should(Succeed())

		By("creating NO StatefulSet while the certificate is missing")
		// Held across a window rather than sampled once: the gate must keep
		// withholding on every pass, not merely lose a race on the first.
		Consistently(func(g Gomega) {
			_, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, gateCluster, metav1.GetOptions{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no StatefulSet may exist while TLSReady is False")
		}).WithTimeout(30 * time.Second).WithPolling(5 * time.Second).Should(Succeed())

		By("creating the TLS Secret, copied from the fixture cluster's own")
		src, err := k8sClient.CoreV1().Secrets(namespace).Get(ctx, "slapd-tls", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "the fixture's slapd-tls Secret is the source of usable cert material")
		_, err = k8sClient.CoreV1().Secrets(namespace).Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: gateSecret, Namespace: namespace},
			Type:       corev1.SecretTypeTLS,
			Data:       src.Data,
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("lifting the gate with no edit to the CR and no operator restart")
		Eventually(func(g Gomega) {
			sc := &ldapv1alpha1.SlapdCluster{}
			g.Expect(crdClient.Get(ctx, client.ObjectKey{Name: gateCluster, Namespace: namespace}, sc)).To(Succeed())
			cond := findCondition(sc.Status.Conditions, "TLSReady")
			g.Expect(cond).NotTo(BeNil())
			g.Expect(string(cond.Status)).To(Equal("True"), "reason=%s msg=%s", cond.Reason, cond.Message)
		}).WithTimeout(90 * time.Second).WithPolling(3 * time.Second).Should(Succeed())

		By("creating the StatefulSet it had been withholding")
		Eventually(func(g Gomega) {
			_, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, gateCluster, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
		}).WithTimeout(90 * time.Second).WithPolling(3 * time.Second).Should(Succeed())
	})

	It("does not tear down a running cluster when its Secret is deleted", func(ctx SpecContext) {
		By("deleting the TLS Secret out from under the existing StatefulSet")
		Expect(k8sClient.CoreV1().Secrets(namespace).Delete(ctx, gateSecret, metav1.DeleteOptions{})).To(Succeed())

		By("reporting the loss")
		Eventually(func(g Gomega) {
			sc := &ldapv1alpha1.SlapdCluster{}
			g.Expect(crdClient.Get(ctx, client.ObjectKey{Name: gateCluster, Namespace: namespace}, sc)).To(Succeed())
			cond := findCondition(sc.Status.Conditions, "TLSReady")
			g.Expect(cond).NotTo(BeNil())
			g.Expect(string(cond.Status)).To(Equal("False"))
			g.Expect(cond.Reason).To(Equal("SecretMissing"))
		}).WithTimeout(90 * time.Second).WithPolling(3 * time.Second).Should(Succeed())

		By("leaving the StatefulSet in place — a deleted prerequisite is not an outage")
		Consistently(func(g Gomega) {
			_, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, gateCluster, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "the gate withholds work; it must never undo it")
		}).WithTimeout(30 * time.Second).WithPolling(5 * time.Second).Should(Succeed())
	})
})
