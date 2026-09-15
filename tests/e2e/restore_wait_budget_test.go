package e2e_test

import (
	"strings"
	"testing"
	"time"
)

// Unit coverage for the restore wait policy (see restore_wait_test.go). This is
// a plain Go test, not a Ginkgo spec: it needs no cluster and runs offline with
//
//	go test -run TestRestore ./...
//
// The policy exists because a fixed deadline is not a property of the restore
// machine. Measured on a three-site mesh (2026-09-15, docs/BACKLOG.md entry
// "restore specs time out when the shared fixture has been inflated"), the
// bootstrapFrom machine spent its first ~347 s before a single restore Job
// started, in coarse operator ticks — all of it progress, none of it
// attributable to the artifact:
//
//	t=0     SlapdCluster + SlapdDatabase created
//	t=107   StatefulSet creates pod-0 (first event)
//	t=167   Ready=True, 1/1 replicas ready
//	t=227   status.restore appears, phase=Preflight
//	t=347   phase=Restoring, "running restore jobs"; pod-0 killed (scaled to 0)
//	t=480   spec's fixed 8-minute budget expires — the restore was still running
//	        and completed afterwards
//
// A spec must therefore fail on absence of progress, not on elapsed time.

// incidentTrace is the measured 2026-09-15 timeline above, as (elapsed seconds,
// fingerprint) pairs. Only the *changes* are listed; the sampler polls every 5 s
// and sees the same fingerprint in between.
var incidentTrace = []struct {
	at    time.Duration
	state string
	// working: a restore Job pod is Running, so the step in flight is opaque
	// (slapadd emits nothing until it is done).
	working bool
}{
	{at: 0, state: "cluster=Bootstrapping restore=<none> sts=0/0 applied=false"},
	{at: 107 * time.Second, state: "cluster=Bootstrapping restore=<none> sts=1/0 applied=false"},
	{at: 167 * time.Second, state: "cluster=Running restore=<none> sts=1/1 applied=false"},
	{at: 227 * time.Second, state: "cluster=Restoring restore=Preflight/ sts=1/1 applied=false"},
	{at: 347 * time.Second, state: "cluster=Restoring restore=Restoring/running restore jobs sts=0/0 applied=false", working: true},
}

// replay drives a budget over a trace, polling every 5 s, and returns the
// verdict and the elapsed time at which it was reached. done fires at doneAt.
func replay(b restoreBudget, trace []struct {
	at      time.Duration
	state   string
	working bool
}, doneAt time.Duration, until time.Duration) (restoreWaitVerdict, time.Duration) {
	var lastChange time.Duration
	last := ""
	for t := time.Duration(0); t <= until; t += 5 * time.Second {
		p := restoreProgress{}
		for _, e := range trace {
			if t >= e.at {
				p.state, p.working = e.state, e.working
			}
		}
		if doneAt > 0 && t >= doneAt {
			p.done = true
		}
		if p.state != last {
			last, lastChange = p.state, t
		}
		v, _ := b.verdict(t, t-lastChange, p)
		if v != restoreWaitContinue {
			return v, t
		}
	}
	return restoreWaitContinue, until
}

// The incident: a restore that was progressing the whole time must not be
// failed. The old fixed 8-minute deadline is the thing being replaced, so the
// same trace is also replayed against a deadline policy to pin what changed.
func TestRestoreBudgetDoesNotFailTheMeasuredIncident(t *testing.T) {
	b := defaultRestoreBudget()

	// Still running at the old 480 s deadline: keep waiting.
	if v, at := replay(b, incidentTrace, 0, 480*time.Second); v != restoreWaitContinue {
		t.Errorf("at the old 480s deadline the progressing restore was %v (at %s); want continue", v, at)
	}

	// It completed afterwards — say at 12 minutes, well past any fixed budget
	// the specs carried, but with a Job pod running throughout.
	if v, at := replay(b, incidentTrace, 12*time.Minute, 20*time.Minute); v != restoreWaitDone {
		t.Errorf("a restore completing at 12m was %v at %s; want done", v, at)
	}

	// The status quo, for contrast: a fixed deadline fails it.
	if v, at := replay(restoreBudget{stall: time.Hour, workingStall: time.Hour, ceiling: 8 * time.Minute},
		incidentTrace, 12*time.Minute, 20*time.Minute); v != restoreWaitFail || at > 8*time.Minute+5*time.Second {
		t.Errorf("fixed 8m deadline gave %v at %s; want fail at the 8m deadline (this is the defect being fixed)", v, at)
	}
}

// A restore that stops advancing must still fail, and fail sooner than the
// deadline it replaces — that is the whole point of trading the deadline for a
// liveness signal.
func TestRestoreBudgetFailsAStalledRestore(t *testing.T) {
	b := defaultRestoreBudget()
	stuck := []struct {
		at      time.Duration
		state   string
		working bool
	}{
		{at: 0, state: "cluster=Restoring restore=Preflight/could not fetch artifact sts=1/1 applied=false"},
	}
	v, at := replay(b, stuck, 0, 30*time.Minute)
	if v != restoreWaitFail {
		t.Fatalf("a restore stuck in Preflight was %v; want fail", v)
	}
	if at <= b.stall || at > b.stall+15*time.Second {
		t.Errorf("stalled restore failed at %s; want just after the %s stall budget", at, b.stall)
	}
	if at >= 8*time.Minute {
		t.Errorf("stalled restore failed at %s, no sooner than the 8m deadline it replaces", at)
	}
}

// While a restore Job pod is Running the step is opaque, so the stall budget is
// longer — but it is still a budget: an slapadd that hangs forever must fail,
// and not before the no-worker budget would have.
func TestRestoreBudgetToleratesAnOpaqueWorkingStep(t *testing.T) {
	b := defaultRestoreBudget()
	working := []struct {
		at      time.Duration
		state   string
		working bool
	}{
		{at: 0, state: "cluster=Restoring restore=Restoring/running restore jobs sts=0/0 applied=false", working: true},
	}
	if v, at := replay(b, working, 0, b.stall+time.Minute); v != restoreWaitContinue {
		t.Errorf("a running restore Job was %v at %s; want continue past the %s no-worker budget", v, at, b.stall)
	}
	v, at := replay(b, working, 0, b.ceiling+time.Minute)
	if v != restoreWaitFail {
		t.Fatalf("a hung restore Job was %v; want fail", v)
	}
	if at > b.ceiling {
		t.Errorf("hung restore Job failed at %s; want at or before the %s ceiling", at, b.ceiling)
	}
}

// Negative terminal evidence fails immediately: the Jobs are at
// BackoffLimitExceeded, or the machine reported Failed. Waiting out any budget
// for a restore that is already dead only delays the autopsy.
func TestRestoreBudgetFailsFastOnTerminalFailure(t *testing.T) {
	b := defaultRestoreBudget()
	v, msg := b.verdict(30*time.Second, 5*time.Second, restoreProgress{
		state: "cluster=Restoring restore=Failed/restore Job failed sts=0/0 applied=false",
		fatal: "restore Job ip-db-restore-0 failed (BackoffLimitExceeded)",
	})
	if v != restoreWaitFail {
		t.Fatalf("a failed restore Job was %v; want fail", v)
	}
	if msg == "" || !strings.Contains(msg, "BackoffLimitExceeded") {
		t.Errorf("failure message %q does not carry the fatal reason", msg)
	}
}

// Positive control: a restore that is simply done is done, regardless of how
// long its last state has been unchanged.
func TestRestoreBudgetPassesACompletedRestore(t *testing.T) {
	b := defaultRestoreBudget()
	if v, _ := b.verdict(b.ceiling+time.Hour, b.workingStall+time.Hour,
		restoreProgress{state: "done", done: true}); v != restoreWaitDone {
		t.Errorf("a completed restore was %v; want done", v)
	}
}
