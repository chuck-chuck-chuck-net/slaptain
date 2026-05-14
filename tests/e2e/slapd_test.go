package e2e_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("slapd chart", func() {

	Describe("StatefulSet", func() {
		It("has one ready replica", func(ctx SpecContext) {
			Eventually(ctx, func() bool {
				return statefulSetReady(k8sClient, namespace, "slapd")
			}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(BeTrue())
		}, NodeTimeout(3*time.Minute))

		It("has pod slapd-0 in Running phase", func(ctx SpecContext) {
			Eventually(ctx, func() corev1.PodPhase {
				pod, err := k8sClient.CoreV1().Pods(namespace).Get(ctx, "slapd-0", metav1.GetOptions{})
				if err != nil {
					return ""
				}
				return pod.Status.Phase
			}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(Equal(corev1.PodRunning))
		}, NodeTimeout(3*time.Minute))

		It("has pod slapd-0 with all containers ready", func() {
			pod, err := k8sClient.CoreV1().Pods(namespace).Get(context.Background(), "slapd-0", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			for _, cs := range pod.Status.ContainerStatuses {
				Expect(cs.Ready).To(BeTrue(), "container %q is not ready", cs.Name)
			}
		})
	})

	Describe("Service", func() {
		It("exists and has ldap + ldaps ports", func() {
			svc, err := k8sClient.CoreV1().Services(namespace).Get(context.Background(), "slapd", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			portNames := make([]string, 0, len(svc.Spec.Ports))
			for _, p := range svc.Spec.Ports {
				portNames = append(portNames, p.Name)
			}
			Expect(portNames).To(ContainElements("ldap", "ldaps"))
		})

		It("exposes plain LDAP on port 389", func() {
			svc, err := k8sClient.CoreV1().Services(namespace).Get(context.Background(), "slapd", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			found := false
			for _, p := range svc.Spec.Ports {
				if p.Name == "ldap" && p.Port == 389 {
					found = true
				}
			}
			Expect(found).To(BeTrue(), "expected ldap port 389 on slapd service")
		})

		It("exposes LDAPS on port 636", func() {
			svc, err := k8sClient.CoreV1().Services(namespace).Get(context.Background(), "slapd", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			found := false
			for _, p := range svc.Spec.Ports {
				if p.Name == "ldaps" && p.Port == 636 {
					found = true
				}
			}
			Expect(found).To(BeTrue(), "expected ldaps port 636 on slapd service")
		})
	})

	Describe("Secrets", func() {
		It("cluster config password secret exists with root-password", func() {
			secret, err := k8sClient.CoreV1().Secrets(namespace).Get(context.Background(), "slapd-config-password", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(secret.Data).To(HaveKey("root-password"))
			Expect(string(secret.Data["root-password"])).NotTo(BeEmpty())
		})
		It("database credentials secret exists with root-password", func() {
			dbCRName := envOrDefault("DB_CR_NAME", "slapd-db")
			secretName := dbCRName + "-credentials"
			secret, err := k8sClient.CoreV1().Secrets(namespace).Get(context.Background(), secretName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(secret.Data).To(HaveKey("root-password"))
			Expect(string(secret.Data["root-password"])).NotTo(BeEmpty())
		})
	})

	// PVCs only exist on the persistent fixture (volumeClaimTemplates).
	// The ephemeral fixture uses emptyDir for /config and /data — see
	// values.slapd-ephemeral.yaml.
	Describe("PersistentVolumeClaims", Label("persistent-only"), func() {
		// PVCs are created by StatefulSet volumeClaimTemplates: <type>-<name>-<ordinal>.
		for _, vol := range []string{"config", "data"} {
			vol := vol
			It("has a Bound PVC for "+vol+" on pod-0", func() {
				pvc, err := k8sClient.CoreV1().PersistentVolumeClaims(namespace).Get(
					context.Background(), vol+"-slapd-0", metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(pvc.Status.Phase).To(Equal(corev1.ClaimBound),
					"expected PVC %s-slapd-0 to be Bound", vol)
			})
		}
	})
})
