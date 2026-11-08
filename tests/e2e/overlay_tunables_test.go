package e2e_test

import (
	"fmt"
	"strings"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The two overlay tunables the operator converges rather than writes once:
// olcAccessLogPurge on the accesslog overlay and olcSpCheckpoint on the data
// DB's syncprov overlay (ADR-024 R4, with an R5 default for the purge window).
//
// Both used to be written ONLY into the overlay-creation addReq, so a spec edit
// — or a newly introduced operator default — never reached a pod whose overlay
// already existed. That is invisible on a healthy fixture: replication works
// fine with no purge policy right up to the day the journal hits its map
// ceiling and, because the accesslog overlay sits in the DATA write path, takes
// data writes down with it. An assertion on cn=config is the only standing
// guard.
//
// cn=config is node-local (ADR-002), so every RW pod is checked independently:
// a purge window converged on two pods out of three leaves the third growing.
//
// Both attributes were verified live-modifiable on a running slapd before being
// converged at all (replace AND delete, no crash, no hang) — the precondition
// ADR-024's 2026-09-12 and 2026-09-13 amendments made mandatory for this class.
//
// Ungated: a bind and two searches per pod per database.
var _ = Describe("overlay tunables: purge window and checkpoint",
	Label("tunables"), Label("accesslog"), Ordered, ContinueOnFailure, func() {

		var (
			rwPods []string
			dbs    []ldapv1alpha1.SlapdDatabase
			// rotating, slotLength and replicas describe the ADR-037 grid the
			// cluster declares. replicas is spec.replicas — DECLARED, never
			// readiness — because it is what the operator derives the purge
			// interval T = n·t from.
			rotating   bool
			slotLength time.Duration
			replicas   int32
			// clusterInterval is T: how often every pod sweeps. Always
			// positive — that is the invariant this milestone exists for.
			clusterInterval time.Duration
		)

		BeforeAll(func(ctx SpecContext) {
			By("reading the SlapdCluster")
			sc := &ldapv1alpha1.SlapdCluster{}
			Expect(crdClient.Get(ctx, nsName(namespace, "slapd"), sc)).To(Succeed())
			replicas = sc.Spec.Replicas
			if replicas == 0 {
				replicas = 1
			}
			if r := sc.Spec.Rotation; r.Enabled != nil && *r.Enabled && r.SlotLength != nil {
				slotLength = r.SlotLength.Duration
				rotating = slotLength > 0 && slotLength%time.Second == 0
			}
			// The INTERVAL is the cluster's whichever form declared it
			// (ADR-024 R6): derived as replicas × slotLength under rotation,
			// declared in spec.accesslogPurge otherwise, and the operator's
			// 24h default when the cluster says nothing. No database declares
			// one, which is what makes a withheld — and therefore unpurged —
			// database unreachable.
			switch {
			case rotating:
				clusterInterval = time.Duration(replicas) * slotLength
			case sc.Spec.AccesslogPurge != nil && sc.Spec.AccesslogPurge.Interval != nil &&
				sc.Spec.AccesslogPurge.Interval.Duration > 0:
				clusterInterval = sc.Spec.AccesslogPurge.Interval.Duration
			default:
				clusterInterval = 24 * time.Hour
			}
			if !sc.Spec.Replication.Enabled || sc.IsConsumerOnly() {
				Skip("cluster is not a syncrepl provider, so it carries neither " +
					"an accesslog overlay nor a data-DB syncprov overlay")
			}

			By("listing the SlapdDatabase CRs in the namespace")
			list := &ldapv1alpha1.SlapdDatabaseList{}
			Expect(crdClient.List(ctx, list)).To(Succeed())
			for _, sd := range list.Items {
				if sd.Namespace == namespace && sd.Spec.ClusterRef == "slapd" {
					dbs = append(dbs, sd)
				}
			}
			Expect(dbs).NotTo(BeEmpty(), "no SlapdDatabase CRs found for cluster slapd")

			for _, sd := range dbs {
				name := sd.Name
				Eventually(ctx, func() bool {
					return slapdDatabaseRunning(crdClient, namespace, name)
				}).WithTimeout(5*time.Minute).WithPolling(5*time.Second).Should(BeTrue(),
					"SlapdDatabase %s never reached Running", name)
			}

			By("reading the RW StatefulSet")
			sts, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, "slapd", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			n := int32(1)
			if sts.Spec.Replicas != nil {
				n = *sts.Spec.Replicas
			}
			for i := int32(0); i < n; i++ {
				rwPods = append(rwPods, fmt.Sprintf("slapd-%d", i))
			}
		}, NodeTimeout(8*time.Minute))

		It("converges the accesslog purge window on every RW pod", func(ctx SpecContext) {
			if rotating {
				Skip("spec.rotation is on, so olcAccessLogPurge carries the cluster's " +
					"interval and this pod's phase — see the rotation spec below")
			}
			for _, pod := range rwPods {
				By("inspecting cn=config on " + pod)
				conn := dialPodConfigEventually(ctx, pod)

				for _, sd := range dbs {
					if !(&sd).DeltaSyncEnabled() {
						continue
					}
					want := wantPurgeValue(sd, clusterInterval, 0, false)

					dataDB, ok := findMdb(observedMdbDatabases(conn), sd.Spec.Suffix)
					Expect(ok).To(BeTrue(), "pod %s has no data database %q", pod, sd.Spec.Suffix)

					dn, values := overlayAttrValues(conn, dataDB.dn,
						"(objectClass=olcAccessLogConfig)", "olcAccessLogPurge")
					Expect(dn).NotTo(BeEmpty(),
						"pod %s: data database %q has no accesslog overlay", pod, sd.Spec.Suffix)

					if want == "" {
						Expect(values).To(BeEmpty(),
							"pod %s: %s (db %s) has accesslogPurge.enabled false, so it must "+
								"carry no olcAccessLogPurge", pod, dn, sd.Name)
						continue
					}
					Expect(values).To(ConsistOf(want),
						"pod %s: %s (db %s) must carry olcAccessLogPurge %q — an unpurged "+
							"journal grows to its map ceiling and then stops data writes (ADR-024 R4/R5)",
						pod, dn, sd.Name, want)
				}
				conn.Close()
			}
		}, NodeTimeout(6*time.Minute))

		// ADR-037: with spec.rotation on, olcAccessLogPurge gains a third value
		// and the operator composes it per (pod, database) — the age from the
		// SlapdDatabase, the interval T = spec.replicas × slotLength and the
		// phase k × slotLength from the SlapdCluster.
		//
		// This spec exists because of ADR-037's third amendment, which measured
		// a stagger that had never happened: a driver assuming the SERVER
		// multiplies the value by the pod's ordinal would write ONE phase for
		// every pod, the providers would stay synchronised, and the arm would
		// report no difference — reading as "the patch does nothing" rather
		// than "the stagger never happened". Nothing outside cn=config can tell
		// those two apart, so the distinctness is asserted here, directly, per
		// pod. Staggering alone changes no client-visible number by design; the
		// write endpoint moving is what moves the number, and that is a
		// different milestone.
		It("gives every RW pod its own purge phase on the cluster's grid",
			Label("purge-rotation"), func(ctx SpecContext) {
				if !rotating {
					Skip("spec.rotation.enabled is not set (or its slotLength is not a usable " +
						"whole number of seconds), so no pod carries a purge phase")
				}

				wantInterval := logAge(time.Duration(replicas) * slotLength)
				// phase → the pod that carries it, so a collision names both.
				seen := map[string]string{}

				for i, pod := range rwPods {
					By("inspecting cn=config on " + pod)
					conn := dialPodConfigEventually(ctx, pod)

					wantPhase := logAge(time.Duration(i) * slotLength)
					podPhase := ""

					for _, sd := range dbs {
						if !(&sd).DeltaSyncEnabled() {
							continue
						}
						if purgeDisabled(sd) {
							continue
						}

						dataDB, ok := findMdb(observedMdbDatabases(conn), sd.Spec.Suffix)
						Expect(ok).To(BeTrue(), "pod %s has no data database %q", pod, sd.Spec.Suffix)

						dn, values := overlayAttrValues(conn, dataDB.dn,
							"(objectClass=olcAccessLogConfig)", "olcAccessLogPurge")
						Expect(dn).NotTo(BeEmpty(),
							"pod %s: data database %q has no accesslog overlay", pod, sd.Spec.Suffix)
						Expect(values).To(HaveLen(1),
							"pod %s: %s carries %d olcAccessLogPurge values, want exactly one",
							pod, dn, len(values))

						fields := strings.Fields(values[0])
						Expect(fields).To(HaveLen(3),
							"pod %s: %s (db %s) carries olcAccessLogPurge %q — with spec.rotation "+
								"on it must carry THREE fields <age> <interval> <phase>. Two fields "+
								"means this pod purges on its own uncoordinated schedule, which is "+
								"the synchronised purge ADR-037 exists to remove; check the "+
								"AccesslogPurgeConverged condition on the SlapdDatabase and whether "+
								"this slapd image carries the accesslog-purge-phase patch",
							pod, dn, sd.Name, values[0])

						Expect(fields[1]).To(Equal(wantInterval),
							"pod %s: %s (db %s) has purge interval %q, want %q "+
								"(spec.replicas=%d × slotLength=%s)",
							pod, dn, sd.Name, fields[1], wantInterval, replicas, slotLength)

						Expect(fields[2]).To(Equal(wantPhase),
							"pod %s: %s (db %s) has purge phase %q, want %q (ordinal %d × "+
								"slotLength %s). A phase that does not track the ordinal means "+
								"the fleet is not staggered",
							pod, dn, sd.Name, fields[2], wantPhase, i, slotLength)

						// Databases ALIGN, they do not spread: one busy window
						// per pod, in the slot where it is not the writer.
						if podPhase == "" {
							podPhase = fields[2]
						}
						Expect(fields[2]).To(Equal(podPhase),
							"pod %s: database %s purges at %q while another of this pod's "+
								"databases purges at %q. Databases must align on their pod's "+
								"phase, not spread across the period (ADR-037)",
							pod, sd.Name, fields[2], podPhase)
					}

					if podPhase != "" {
						Expect(seen).NotTo(HaveKey(podPhase),
							"pods %s and %s both purge at phase %q — the stagger never happened, "+
								"and every provider will stall its own writes at the same instant",
							seen[podPhase], pod, podPhase)
						seen[podPhase] = pod
					}
					conn.Close()
				}

				Expect(seen).To(HaveLen(len(rwPods)),
					"expected one distinct purge phase per RW pod, got %d for %d pods: %v",
					len(seen), len(rwPods), seen)
			}, NodeTimeout(6*time.Minute))

		// THE DEFECT, AS A STANDING REGRESSION GUARD. Measured on a fresh
		// three-site mesh: a database declaring an interval, on a cluster
		// created with rotation already on, ended up with NO olcAccessLogPurge
		// at all on every pod at every site — the withhold assumed there was an
		// existing value to keep. An unpurged journal grows to its map ceiling
		// and then stops DATA writes, because the accesslog overlay sits in the
		// data database's write path.
		//
		// Deliberately the weakest possible assertion and deliberately
		// unconditional: not which value, not how many fields, not whether the
		// fleet is staggered — just that EVERY replicated database on EVERY RW
		// pod carries SOMETHING. The specs above check the composition; this one
		// would have caught the outage, and it costs one search per pod per
		// database either way.
		It("leaves no replicated database unpurged on any RW pod",
			Label("purge-rotation"), func(ctx SpecContext) {
				for _, pod := range rwPods {
					By("inspecting cn=config on " + pod)
					conn := dialPodConfigEventually(ctx, pod)

					for _, sd := range dbs {
						if !(&sd).DeltaSyncEnabled() || purgeDisabled(sd) {
							continue
						}
						dataDB, ok := findMdb(observedMdbDatabases(conn), sd.Spec.Suffix)
						Expect(ok).To(BeTrue(), "pod %s has no data database %q", pod, sd.Spec.Suffix)

						dn, values := overlayAttrValues(conn, dataDB.dn,
							"(objectClass=olcAccessLogConfig)", "olcAccessLogPurge")
						Expect(dn).NotTo(BeEmpty(),
							"pod %s: data database %q has no accesslog overlay", pod, sd.Spec.Suffix)
						Expect(values).NotTo(BeEmpty(),
							"pod %s: %s (db %s) carries NO olcAccessLogPurge at all. Its journal "+
								"grows to its map ceiling and then stops DATA writes — the "+
								"outage ADR-024 R4 exists to prevent. Only "+
								"accesslogPurge.enabled: false may produce this, and this "+
								"database does not set it",
							pod, dn, sd.Name)
						Expect(len(strings.Fields(values[0]))).To(BeNumerically(">=", 2),
							"pod %s: %s (db %s) carries olcAccessLogPurge %q, which slapd cannot "+
								"parse as <age> <interval>", pod, dn, sd.Name, values[0])
					}
					conn.Close()
				}
			}, NodeTimeout(6*time.Minute))

		It("converges the syncprov checkpoint on every RW pod's data database", func(ctx SpecContext) {
			for _, pod := range rwPods {
				By("inspecting cn=config on " + pod)
				conn := dialPodConfigEventually(ctx, pod)

				for _, sd := range dbs {
					want := wantDataCheckpoint(sd)

					dataDB, ok := findMdb(observedMdbDatabases(conn), sd.Spec.Suffix)
					Expect(ok).To(BeTrue(), "pod %s has no data database %q", pod, sd.Spec.Suffix)

					dn, values := overlayAttrValues(conn, dataDB.dn,
						"(objectClass=olcSyncProvConfig)", "olcSpCheckpoint")
					Expect(dn).NotTo(BeEmpty(),
						"pod %s: data database %q has no syncprov overlay", pod, sd.Spec.Suffix)

					if want == "" {
						Expect(values).To(BeEmpty(),
							"pod %s: %s (db %s) opts out with syncprovCheckpoint: none, so it "+
								"must carry no olcSpCheckpoint at all", pod, dn, sd.Name)
						continue
					}
					Expect(values).To(ConsistOf(want),
						"pod %s: %s (db %s) must carry olcSpCheckpoint %q, converged on every "+
							"reconcile and not only at overlay creation (ADR-024 R4). A database "+
							"that declares NOTHING takes the operator default: unset is not "+
							"slapd's own behaviour, it is slapd's behaviour minus the startup "+
							"repair back-mdb never runs, and after an unclean stop such a pod "+
							"regenerates contextCSN ahead of its own content (ADR-045)",
						pod, dn, sd.Name, want)
				}
				conn.Close()
			}
		}, NodeTimeout(6*time.Minute))
	})

// wantDataCheckpoint mirrors the operator's resolution of the DATA database's
// syncprov checkpoint (ADR-045 R1/R2). Spelled out here rather than imported,
// like wantPurgeValue below, so the e2e states the contract instead of
// restating the code: a default both sides read from one constant could be
// changed on both sides at once and this spec would never notice.
//
// An empty answer means the attribute must be ABSENT, and the ONLY way to get
// one is the explicit "none" sentinel. A field left unset is a field the user
// did not fill in and takes the default — which is the shape ADR-045 R5 made
// the fixtures cover, because it was previously the one shape the suite never
// ran.
func wantDataCheckpoint(sd ldapv1alpha1.SlapdDatabase) string {
	v := strings.TrimSpace(sd.Spec.Replication.SyncprovCheckpoint)
	if strings.EqualFold(v, "none") {
		return ""
	}
	if v == "" {
		return "100 10"
	}
	return v
}

// wantPurgeValue mirrors the operator's composition of olcAccessLogPurge from
// its THREE owners (ADR-024 R6): the database's retention, the cluster's
// interval, and — with rotation on — this pod's phase. Spelled out here rather
// than imported so the e2e states the contract instead of restating the code.
//
// An empty answer means the attribute must be ABSENT, and the only way to get
// one is the explicit enabled: false. That is the defect's negation: before the
// decomposition a database declaring an interval on a rotating cluster was
// WITHHELD, and ended up unpurged on every pod at every site.
func wantPurgeValue(sd ldapv1alpha1.SlapdDatabase, interval, phase time.Duration, phased bool) string {
	retention := wantRetention(sd)
	if retention <= 0 {
		return ""
	}
	v := logAge(retention) + " " + logAge(interval)
	if phased {
		v += " " + logAge(phase)
	}
	return v
}

// wantRetention is the database's half: what it KEEPS. Zero means purging is
// switched off entirely. The operator's default is 7 days.
func wantRetention(sd ldapv1alpha1.SlapdDatabase) time.Duration {
	p := sd.Spec.Replication.AccesslogPurge
	if p == nil {
		return 7 * 24 * time.Hour
	}
	if p.Enabled != nil && !*p.Enabled {
		return 0
	}
	if p.Retention == nil || p.Retention.Duration <= 0 ||
		p.Retention.Duration%time.Second != 0 {
		// An unusable declared retention is REPORTED and the operator's
		// default written anyway — the journal must stay bounded whatever the
		// user got wrong.
		return 7 * 24 * time.Hour
	}
	return p.Retention.Duration
}

// purgeDisabled is the explicit "never purge, keep forever" opt-out, which
// survives rotation — a schedule must not resurrect a purge the user turned
// off. It replaced the "none" sentinel when the directive was decomposed.
func purgeDisabled(sd ldapv1alpha1.SlapdDatabase) bool {
	p := sd.Spec.Replication.AccesslogPurge
	return p != nil && p.Enabled != nil && !*p.Enabled
}

// logAge renders a duration the way slapd's log_age_parse accepts it:
// "hh:mm:ss", or "ddd+hh:mm:ss" from a day up. Spelled out here rather than
// imported, so the e2e states the wire format instead of restating the code —
// if the operator's formatter drifts, this is what catches it.
func logAge(d time.Duration) string {
	secs := int64(d / time.Second)
	days, rest := secs/86400, secs%86400
	hh, mm, ss := rest/3600, rest%3600/60, rest%60
	if days > 0 {
		return fmt.Sprintf("%d+%02d:%02d:%02d", days, hh, mm, ss)
	}
	return fmt.Sprintf("%02d:%02d:%02d", hh, mm, ss)
}

// overlayAttrValues returns the DN of the overlay under dbDN matching filter and
// that overlay's values for attr (trimmed). An empty DN means no such overlay.
func overlayAttrValues(conn *ldap.Conn, dbDN, filter, attr string) (string, []string) {
	res, err := conn.Search(ldap.NewSearchRequest(
		dbDN, ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
		0, 0, false, filter, []string{attr}, nil))
	Expect(err).NotTo(HaveOccurred(), "search %s under %s", filter, dbDN)
	if len(res.Entries) == 0 {
		return "", nil
	}
	var out []string
	for _, v := range res.Entries[0].GetEqualFoldAttributeValues(attr) {
		out = append(out, strings.TrimSpace(v))
	}
	return res.Entries[0].DN, out
}
