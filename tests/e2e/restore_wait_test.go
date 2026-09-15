package e2e_test

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// ── Waiting on the restore machine: progress, not a deadline ─────────────────
//
// The ADR-014 restore machine (preflight → scale-to-0 → slapadd on every pod →
// scale-up) has no duration a spec can compute in advance. Two of its terms are
// outside the spec's knowledge:
//
//  1. The operator's own cadence. Its steps land on ~60 s ticks, and on a loaded
//     operator the gaps stretch: measured 107 s from CR creation to the pod
//     being created, then 60 s to Ready, 60 s to preflight, 120 s in preflight
//     — ~347 s of pure overhead before the first restore Job started
//     (2026-09-15, three-site mesh).
//  2. The artifact size. The offline `slapadd -q` is proportional to the DIT,
//     and the DIT belongs to the shared fixture: `E2E_SCALE=1` inflates it to
//     1200+ entries, and the big-DIT lane in docs/BACKLOG.md will inflate it
//     further on purpose.
//
// Sizing a fixed budget off either term is guesswork; sizing it off their sum is
// guesswork twice. So these waits assert LIVENESS instead: any change in the
// machine's observable state (cluster phase, restore sub-phase and message,
// StatefulSet replicas, restore Job and Job-pod state, restoreApplied) counts as
// progress and resets the clock. A restore that stops advancing fails — sooner
// than the 8-minute deadline it replaces — while a slow one is allowed to
// finish, up to an absolute ceiling that keeps a genuinely wedged run from
// hanging the suite.
//
// This is the "liveness assertion class" docs/BACKLOG.md asks for, arriving from
// the other direction: that entry wants liveness bounded during convergence so a
// self-resolving freeze cannot pass green; this bounds liveness during a restore
// so a slow-but-advancing machine cannot fail red. Same signal, opposite failure
// mode — see that entry before inventing a third mechanism.

// restoreProgress is one sample of the restore machine's observable state.
type restoreProgress struct {
	// state is the progress fingerprint: any change means the machine advanced.
	// A read failure is part of the fingerprint (it can change too) and never
	// counts as done.
	state string
	// done is the spec's terminal predicate — positive evidence only.
	done bool
	// fatal, when non-empty, is terminal negative evidence (a restore Job at
	// BackoffLimitExceeded, phase Failed): fail now rather than wait out a
	// budget for a restore that is already dead.
	fatal string
	// working reports that a restore Job pod is Running. slapadd is silent until
	// it finishes, so that step is opaque: the fingerprint cannot change while
	// it runs, and it gets the longer stall budget.
	working bool
}

type restoreWaitVerdict int

const (
	restoreWaitContinue restoreWaitVerdict = iota
	restoreWaitDone
	restoreWaitFail
)

func (v restoreWaitVerdict) String() string {
	switch v {
	case restoreWaitDone:
		return "done"
	case restoreWaitFail:
		return "fail"
	default:
		return "continue"
	}
}

// restoreBudget is the wait policy: two stall budgets and an absolute ceiling.
type restoreBudget struct {
	// stall: nothing observable changed and no restore Job pod is running.
	stall time.Duration
	// workingStall: nothing observable changed, but a restore Job pod is
	// running — i.e. an opaque step (slapadd) is in flight.
	workingStall time.Duration
	// ceiling: the loud backstop. Not a budget for the work; a bound on how long
	// a wedged run may hold the suite.
	ceiling time.Duration
}

// The numbers, and what they are and are not derived from:
//
//   - stall (5m) is a margin over the operator's coarsest measured gap between
//     restore-machine steps: 120 s (preflight) and 107 s (CR → pod) on a busy
//     three-site operator, 60 s being the normal tick. 5m is ~2.5× the largest
//     measured gap. It is the budget that does the real work, and it is
//     DIT-independent: the fingerprint changes at every step regardless of size.
//   - workingStall (15m) bounds ONE opaque step, i.e. the offline `slapadd -q`.
//     Measured against the slapd-init image's own 2.7.1 slapadd (2026-09-15,
//     `-q`, 7-attribute index set, verified by slapcat count): 1202 entries
//     25 ms, 12 002 entries 182 ms, 60 002 entries 1.0 s, 250 002 entries 4.8 s
//     — linear at ~19 µs/entry, so even 10^6 entries is ~20 s of slapadd. That
//     was a workstation with the LMDB environment on an overlay FS, not lab
//     node-local storage, so treat the slope as the finding and the absolute
//     numbers as a floor; on a real restore Job the whole Restoring phase
//     (download + wipe + slapadd) measured 5 s on a fixture-sized DIT (t3e).
//     15m is therefore two orders of magnitude of headroom over anything an
//     e2e fixture can produce — deliberately, because being too generous here
//     costs a slow failure while being too tight is the defect this fixes.
//   - ceiling (30m) is the backstop, chosen the same way.
//
// Note what these measurements say about the reported cause: the artifact size
// is NOT what blew the 8-minute budget. slapadd of the 1200-entry E2E_SCALE
// fixture is sub-second; the 480 s was ~347 s of operator cadence plus a Job
// phase. E2E_SCALE does lengthen restores, but through the operator's per-tick
// workload, not through slapadd — which is exactly why a per-entry coefficient
// would have been fitted to the wrong variable. If a future big-DIT lane
// (docs/BACKLOG.md) ever makes a real slapadd approach workingStall, re-measure
// and raise it; do not re-derive it per entry count.
func defaultRestoreBudget() restoreBudget {
	return restoreBudget{
		stall:        5 * time.Minute,
		workingStall: 15 * time.Minute,
		ceiling:      30 * time.Minute,
	}
}

// verdict is the whole policy, pure and unit-tested (restore_wait_budget_test.go).
func (b restoreBudget) verdict(elapsed, sinceChange time.Duration, p restoreProgress) (restoreWaitVerdict, string) {
	if p.done {
		return restoreWaitDone, ""
	}
	if p.fatal != "" {
		return restoreWaitFail, p.fatal
	}
	if elapsed > b.ceiling {
		return restoreWaitFail, fmt.Sprintf("exceeded the %s absolute ceiling", b.ceiling)
	}
	limit, what := b.stall, "no restore Job pod running"
	if p.working {
		limit, what = b.workingStall, "a restore Job pod is Running (opaque step)"
	}
	if sinceChange > limit {
		return restoreWaitFail, fmt.Sprintf("no observable progress for %s (budget %s; %s)",
			sinceChange.Round(time.Second), limit, what)
	}
	return restoreWaitContinue, ""
}

// restoreWaitTarget describes one wait: what to sample and what "done" means.
type restoreWaitTarget struct {
	what     string // human description, used in the failure message
	cluster  string
	database string // SlapdDatabase to sample; "" to skip
	request  string // SlapdRestore to sample; "" to skip
	// srcEntries is the source DIT entry count the spec already measured, if
	// known (0 = unknown). Diagnostic only — it is reported, never budgeted
	// against.
	srcEntries int
	// done reads the terminal predicate off the sampled CRs. Either may be nil
	// when not sampled.
	done func(sd *ldapv1alpha1.SlapdDatabase, sr *ldapv1alpha1.SlapdRestore) bool
}

// awaitRestoreProgress polls the restore machine until the target's terminal
// predicate holds, failing the spec when the machine stops advancing (see the
// header comment). On failure it prints the whole observed progression, which is
// the evidence needed to tell "slow" from "stuck" — the distinction the fixed
// deadline destroyed.
func awaitRestoreProgress(ctx SpecContext, t restoreWaitTarget) {
	GinkgoHelper()

	b := defaultRestoreBudget()
	start := time.Now()
	lastChange := start
	last := ""
	var trail []restoreWaitStep

	for {
		p := sampleRestoreProgress(ctx, t)
		now := time.Now()
		if p.state != last {
			last, lastChange = p.state, now
			trail = append(trail, restoreWaitStep{at: now.Sub(start).Round(time.Second), state: p.state})
		}
		v, why := b.verdict(now.Sub(start), now.Sub(lastChange), p)
		switch v {
		case restoreWaitDone:
			// The trail is reported on success too: it is the only record of how
			// long each step of the machine actually took, and the numbers in
			// defaultRestoreBudget have to come from somewhere. Visible under
			// ginkgo -v.
			AddReportEntry(fmt.Sprintf("restore wait: %s reached after %s (source DIT entries: %s)\n%s",
				t.what, now.Sub(start).Round(time.Second), entryCountOrUnknown(t.srcEntries),
				formatTrail(trail)))
			return
		case restoreWaitFail:
			var sb strings.Builder
			fmt.Fprintf(&sb, "waiting for %s: %s\n", t.what, why)
			fmt.Fprintf(&sb, "elapsed %s; source DIT entries: %s\n",
				now.Sub(start).Round(time.Second), entryCountOrUnknown(t.srcEntries))
			sb.WriteString(formatTrail(trail))
			sb.WriteString("a state that kept changing means the machine was advancing (slow, not stuck) — " +
				"see tests/e2e/restore_wait_test.go")
			Fail(sb.String())
			return
		}
		select {
		case <-ctx.Done():
			Fail(fmt.Sprintf("context cancelled while waiting for %s", t.what))
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// restoreWaitStep is one observed change of the machine's state.
type restoreWaitStep struct {
	at    time.Duration
	state string
}

func formatTrail(trail []restoreWaitStep) string {
	var sb strings.Builder
	sb.WriteString("observed progression (elapsed → state):\n")
	for _, s := range trail {
		fmt.Fprintf(&sb, "  %8s  %s\n", s.at, s.state)
	}
	return sb.String()
}

func entryCountOrUnknown(n int) string {
	if n <= 0 {
		return "unknown"
	}
	return fmt.Sprintf("%d", n)
}

// sampleRestoreProgress builds one restoreProgress from live cluster state.
func sampleRestoreProgress(ctx SpecContext, t restoreWaitTarget) restoreProgress {
	var p restoreProgress
	var parts []string

	sc := &ldapv1alpha1.SlapdCluster{}
	if err := crdClient.Get(ctx, client.ObjectKey{Name: t.cluster, Namespace: namespace}, sc); err != nil {
		parts = append(parts, "cluster=<unreadable: "+err.Error()+">")
	} else {
		parts = append(parts, "cluster="+string(sc.Status.Phase))
		if r := sc.Status.Restore; r == nil {
			parts = append(parts, "restore=<none>")
		} else {
			parts = append(parts, fmt.Sprintf("restore=%s/%s", r.Phase, r.Message))
			if r.Phase == ldapv1alpha1.RestoreFailed {
				p.fatal = fmt.Sprintf("cluster restore reached phase Failed: %s", r.Message)
			}
		}
	}

	if sts, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, t.cluster, metav1.GetOptions{}); err == nil {
		spec := int32(0)
		if sts.Spec.Replicas != nil {
			spec = *sts.Spec.Replicas
		}
		parts = append(parts, fmt.Sprintf("sts=%d/%d", spec, sts.Status.ReadyReplicas))
	} else {
		parts = append(parts, "sts=<absent>")
	}

	var sd *ldapv1alpha1.SlapdDatabase
	if t.database != "" {
		sd = &ldapv1alpha1.SlapdDatabase{}
		if err := crdClient.Get(ctx, client.ObjectKey{Name: t.database, Namespace: namespace}, sd); err != nil {
			parts = append(parts, "db=<unreadable>")
			sd = nil
		} else {
			parts = append(parts, fmt.Sprintf("db=%s applied=%t", sd.Status.Phase, sd.Status.RestoreApplied))
		}
	}

	var sr *ldapv1alpha1.SlapdRestore
	if t.request != "" {
		sr = &ldapv1alpha1.SlapdRestore{}
		if err := crdClient.Get(ctx, client.ObjectKey{Name: t.request, Namespace: namespace}, sr); err != nil {
			parts = append(parts, "request=<unreadable>")
			sr = nil
		} else {
			parts = append(parts, fmt.Sprintf("request=%s/%s", sr.Status.Phase, sr.Status.Message))
			if sr.Status.Phase == ldapv1alpha1.RestoreRequestFailed && p.fatal == "" {
				p.fatal = fmt.Sprintf("SlapdRestore %s reached phase Failed: %s", t.request, sr.Status.Message)
			}
		}
	}

	jobs, working, jobFatal := sampleRestoreJobs(ctx, t.cluster)
	parts = append(parts, jobs)
	p.working = working
	if jobFatal != "" && p.fatal == "" {
		p.fatal = jobFatal
	}

	p.state = strings.Join(parts, " ")
	if t.done != nil {
		p.done = t.done(sd, sr)
	}
	return p
}

// sampleRestoreJobs fingerprints this cluster's restore Jobs and their pods. The
// Job counts alone cannot show progress *inside* slapadd — nothing can, it is
// silent — but a Running pod distinguishes "working on an opaque step" from
// "nothing is happening", and a Failed Job is terminal evidence worth failing on
// immediately (ADR-014's failure mode: Jobs at BackoffLimitExceeded while the
// cluster sits at 0 replicas).
func sampleRestoreJobs(ctx SpecContext, cluster string) (fingerprint string, working bool, fatal string) {
	sel := fmt.Sprintf("app.kubernetes.io/instance=%s,app.kubernetes.io/component=restore", cluster)
	list, err := k8sClient.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return "jobs=<unreadable>", false, ""
	}
	if len(list.Items) == 0 {
		return "jobs=none", false, ""
	}
	var parts []string
	for i := range list.Items {
		j := &list.Items[i]
		desc := fmt.Sprintf("%s(a%d/s%d/f%d", j.Name, j.Status.Active, j.Status.Succeeded, j.Status.Failed)
		for _, c := range j.Status.Conditions {
			if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
				desc += ",FAILED:" + c.Reason
				if fatal == "" {
					fatal = fmt.Sprintf("restore Job %s failed: %s (%s)", j.Name, c.Reason, c.Message)
				}
			}
		}
		phases, running := restoreJobPodPhases(ctx, j.Name)
		working = working || running
		desc += "," + phases + ")"
		parts = append(parts, desc)
	}
	// Job order from the API is stable enough for a fingerprint (name-sorted by
	// the apiserver), so no sort is needed to keep it from flapping.
	return "jobs=" + strings.Join(parts, " "), working, fatal
}

// restoreJobPodPhases reports the phases of a Job's pods and whether any is
// Running. A Pending pod deliberately does NOT count as working: pull-back-off
// and unschedulable-because-a-PVC-is-leased (ADR-018) are exactly the stalls the
// shorter budget should catch.
func restoreJobPodPhases(ctx SpecContext, job string) (string, bool) {
	pods, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "batch.kubernetes.io/job-name=" + job,
	})
	if err != nil || len(pods.Items) == 0 {
		return "pods=none", false
	}
	var phases []string
	running := false
	for i := range pods.Items {
		ph := pods.Items[i].Status.Phase
		phases = append(phases, string(ph))
		if ph == corev1.PodRunning {
			running = true
		}
	}
	return "pods=" + strings.Join(phases, "/"), running
}

// ── The two waits the restore specs share ────────────────────────────────────

// awaitBootstrapRestore waits for a bootstrapFrom restore to set
// restoreApplied on the database. Used by restore_test.go,
// restore_inplace_test.go and restore_replication_test.go.
func awaitBootstrapRestore(ctx SpecContext, cluster, database string, srcEntries int) {
	GinkgoHelper()
	awaitRestoreProgress(ctx, restoreWaitTarget{
		what:       fmt.Sprintf("the bootstrapFrom restore of %s to set restoreApplied=true", database),
		cluster:    cluster,
		database:   database,
		srcEntries: srcEntries,
		done: func(sd *ldapv1alpha1.SlapdDatabase, _ *ldapv1alpha1.SlapdRestore) bool {
			return sd != nil && sd.Status.RestoreApplied
		},
	})
}

// awaitRestoreRequest waits for an in-place SlapdRestore to reach Completed.
// Used by restore_inplace_test.go and restore_replay_test.go.
func awaitRestoreRequest(ctx SpecContext, cluster, database, request string, srcEntries int) {
	GinkgoHelper()
	awaitRestoreProgress(ctx, restoreWaitTarget{
		what:       fmt.Sprintf("SlapdRestore %s to reach Completed", request),
		cluster:    cluster,
		database:   database,
		request:    request,
		srcEntries: srcEntries,
		done: func(_ *ldapv1alpha1.SlapdDatabase, sr *ldapv1alpha1.SlapdRestore) bool {
			return sr != nil && sr.Status.Phase == ldapv1alpha1.RestoreRequestCompleted
		},
	})
}
