package e2e_test

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// ReplicationConverged is a shipped status condition, and a backup consumes it
// verbatim as SourceConverged (ADR-014 amendment 2026-09-12). A condition that
// reports a falsehood is worse than no condition: it trains every reader to
// ignore it, and the deferred requireConverged gate would wait on it forever.
//
// The fixture has TWO SlapdDatabases, which is the whole point: their contextCSN
// vectors differ by construction (independent write histories, disjoint serverID
// activity, last writes at unrelated times). Before the per-database fix this
// spec was red on every deployment — single-site included — with messages like
// "local CSN divergence: 0.0s lag across 3 pods", a "lag" that was really the
// age gap between two different databases' last writes.
//
// Ungated: one CR read, and it guards the honesty of a published field.
var _ = Describe("replication convergence condition", Label("replication-converged"), func() {

	It("reports the cluster converged once every database has settled", func(ctx SpecContext) {
		sc := &ldapv1alpha1.SlapdCluster{}
		Expect(crdClient.Get(ctx, client.ObjectKey{Name: "slapd", Namespace: namespace}, sc)).To(Succeed())

		if !sc.Spec.Replication.Enabled || sc.Spec.Replicas < 2 {
			Skip("cluster is not a multi-replica replication participant; " +
				"the local convergence condition is not produced")
		}

		By("waiting for ReplicationConverged=True on a healthy, settled cluster")
		var last *metav1.Condition
		Eventually(func(g Gomega) metav1.ConditionStatus {
			cur := &ldapv1alpha1.SlapdCluster{}
			g.Expect(crdClient.Get(ctx, client.ObjectKey{Name: "slapd", Namespace: namespace}, cur)).To(Succeed())
			last = findCondition(cur.Status.Conditions, "ReplicationConverged")
			g.Expect(last).NotTo(BeNil(), "a replicating cluster must publish ReplicationConverged")
			return last.Status
		}).WithTimeout(3 * time.Minute).WithPolling(10 * time.Second).
			Should(Equal(metav1.ConditionTrue), func() string {
				if last == nil {
					return "ReplicationConverged was never published"
				}
				return fmt.Sprintf("ReplicationConverged=%s (%s): %s — every database's pods must agree "+
					"on that database's OWN contextCSN; a verdict that compares one database's vector "+
					"against another's is diverged by construction",
					last.Status, last.Reason, last.Message)
			})

		fmt.Fprintf(GinkgoWriter, "ReplicationConverged=%s (%s): %s\n",
			last.Status, last.Reason, last.Message)
	})

	It("never reports an external peer Synced off a subset of the databases", func(ctx SpecContext) {
		sc := &ldapv1alpha1.SlapdCluster{}
		Expect(crdClient.Get(ctx, client.ObjectKey{Name: "slapd", Namespace: namespace}, sc)).To(Succeed())
		if len(sc.Spec.Replication.ExternalPeers) == 0 {
			Skip("no external peers configured")
		}

		// The db2 breakage of 2026-09-13 read Synced off db1 while db2's remote
		// binds failed err=49 on the same hosts. A peer whose evidence is
		// incomplete must say so — PartiallyVerified — rather than borrow the
		// verdict of whichever database answered.
		for _, ps := range sc.Status.ExternalPeerStatuses {
			fmt.Fprintf(GinkgoWriter, "peer %s: state=%s lag=%s lastError=%q\n",
				ps.Name, ps.ReplicationState, ps.LagSeconds, ps.LastError)
			if ps.ReplicationState == ldapv1alpha1.ReplicationSynced {
				Expect(ps.LastError).To(BeEmpty(),
					"peer %s reports Synced while carrying an error (%q): a Synced verdict "+
						"must rest on every database having been read", ps.Name, ps.LastError)
			}
		}
	})
})
