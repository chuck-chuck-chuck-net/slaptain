package e2e_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("slapd-test chart", func() {

	Describe("bootstrap Job", func() {
		It("completes successfully", func(ctx SpecContext) {
			Eventually(ctx, func() bool {
				return jobSucceeded(k8sClient, namespace, "slapd-test")
			}).WithTimeout(5 * time.Minute).WithPolling(10 * time.Second).Should(BeTrue())
		}, NodeTimeout(6*time.Minute))

		It("succeeded on the first or second attempt", func() {
			job, err := k8sClient.BatchV1().Jobs(namespace).Get(context.Background(), "slapd-test", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(job.Status.Failed).To(BeNumerically("<=", 2),
				"bootstrap Job failed too many times (expected at most 1 retry)")
		})
	})

	Describe("toolkit Deployment", func() {
		It("has at least one ready replica", func(ctx SpecContext) {
			Eventually(ctx, func() bool {
				return deploymentReady(k8sClient, namespace, "slapd-test-toolkit")
			}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(BeTrue())
		}, NodeTimeout(3*time.Minute))
	})
})
