package e2e_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Custom resource readiness", func() {

	Describe("SlapdDatabase", func() {
		It("reaches Running phase", func(ctx SpecContext) {
			dbCRName := envOrDefault("DB_CR_NAME", "slapd-db")
			Eventually(ctx, func() bool {
				return slapdDatabaseRunning(crdClient, namespace, dbCRName)
			}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(BeTrue(),
				"SlapdDatabase %s did not reach Running phase", dbCRName)
		}, NodeTimeout(6*time.Minute))
	})

	Describe("SlapdSchema", func() {
		It("is applied to all pods", func(ctx SpecContext) {
			schemaCRName := envOrDefault("SCHEMA_CR_NAME", "app-schema")
			Eventually(ctx, func() bool {
				return slapdSchemaApplied(crdClient, namespace, schemaCRName)
			}).WithTimeout(3 * time.Minute).WithPolling(5 * time.Second).Should(BeTrue(),
				"SlapdSchema %s was not applied", schemaCRName)
		}, NodeTimeout(4*time.Minute))
	})
})
