/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The rotating write endpoint (ADR-037 decision 2, and the second amendment's
// "Writer selection"). This is the milestone the whole feature exists for.
//
// Everything before it is scaffolding. Per-pod purge phases landed without
// changing a single client-visible number, and that was not a disappointment —
// ADR-037's third amendment measured it: four arms at 100k ops/s showed the
// staggered fleet landing on the synchronised fleet and nowhere near the arm
// where the write target did not purge. "The only variable that matters is
// whether the write target purges." A Service that moves the writes off the
// purging pod is therefore the deliverable, and a grid that is perfect while
// the writes stay put measures exactly like no feature at all.
//
// The contract published with it (decision 2): follow the write Service and no
// purge ever runs where your writes land; do not, and you may be writing to a
// provider that is about to purge, and you will see the stall. It is a
// recommendation, not an enforcement — a multi-provider mesh accepts a write
// anywhere and we keep it that way (option D, rejected: olcReadOnly has one
// owner, ADR-036, and rotation does not write it).
//
// Three things this file must not become, each of them a silent failure:
//
//   - It must never select the purging pod while another is available. That is
//     the entire claim, and every health signal stays green when it breaks.
//   - It must never select NOTHING. A Service with an empty or unmatchable
//     selector is a total write outage, strictly worse than an endpoint that
//     stopped rotating (PURGE-ROTATION-PLAN, "Known risks": operator downtime
//     degrades to today, "but the write Service must not be left selecting
//     nothing").
//   - It must never requeue at zero. The rotation's cadence is a RequeueAfter
//     computed from an absolute grid; a non-positive one is a hot loop against
//     the API server, from a controller whose real duty cycle is a handful of
//     writes a day.
//
// Nothing here touches the pod template (PURGE-ROTATION-PLAN hazard 3): this is
// Services and a condition, and the golden templates in testdata/ pin that.
//
// The decisions are pure functions of the configuration and the clock, as
// ADR-037's "Test discipline" asks: the clock arrives as a parameter, the
// readiness arrives as a map, and only the reconciler calls time.Now().

// labelStatefulSetPodName is the label the StatefulSet controller stamps on
// every pod it creates, carrying that pod's own name. It is the only built-in
// way to select ONE pod of a StatefulSet, and nothing in this operator sets,
// strips or shadows it — the Jobs in backup_job.go and dirjob.go already select
// on it to co-locate with a specific pod's PVCs (ADR-018).
const labelStatefulSetPodName = "statefulset.kubernetes.io/pod-name"

// writeServiceSuffix is appended to the cluster name for the write endpoint,
// in the same shape as "-headless" and "-readonly".
const writeServiceSuffix = "-write"

// noGridRequeue is what nextSlotBoundary answers when there is no grid to find
// a boundary on. It matches the SlapdCluster controller's existing
// not-yet-Running cadence: slow enough never to hammer anything, fast enough
// that a cluster whose rotation becomes usable starts rotating promptly.
const noGridRequeue = 10 * time.Second

// writeEndpointCondition reports which pod the write Service names and when it
// moves next. Named positively, like every condition this operator publishes:
// True means a writer was chosen.
//
// It is OBSERVATION ONLY (PURGE-ROTATION-PLAN Phase 4 item 5). It gates
// nothing, withholds nothing and refuses nothing — the cluster-wide question of
// whether the rotation is configured at all is RotationConfigured's
// (rotation_gate.go), and the per-database question of whether a pod took its
// purge value is AccesslogPurgeConverged's (accesslog_purge_converged.go).
const writeEndpointCondition = "WriteEndpointRotating"

// The reasons WriteEndpointRotating publishes.
const (
	// reasonWriterSelected: a pod was chosen and the Service names it.
	reasonWriterSelected = "WriterSelected"
	// reasonNoEligibleWriter: writeTarget found no eligible pod — not even the
	// purging one, which ADR-037 would otherwise take. The Service keeps the
	// selector it already has, because a Service selecting nothing is a total
	// write outage. The message must say that, and must NOT read as though an
	// endpoint had been repointed.
	reasonNoEligibleWriter = "NoEligibleWriter"
)

// writeServiceName returns the name of the rotating write Service for a
// cluster: "<cluster>-write".
//
// It must not collide with any other Service the SlapdCluster controller owns —
// "<cluster>", "<cluster>-headless", "<cluster>-readonly" and
// "<cluster>-readonly-headless" — because an SSA patch under the same field
// manager would then quietly repoint the client-facing Service at a single pod.
//
// Defined behaviour for an input the controller cannot produce:
//
//   - an empty cluster name returns the empty string, not "-write". Both are
//     refused by the API server (a name must start alphanumeric), so neither
//     creates an object; the empty one is refused while looking like what it is.
//     A plausible-looking "-write" in a log line sends the reader hunting for a
//     cluster that was never named.
func writeServiceName(cluster string) string {
	if cluster == "" {
		return ""
	}
	return cluster + writeServiceSuffix
}

// writeServiceSelector returns the label selector that picks exactly one pod:
// the standard cluster labels plus statefulset.kubernetes.io/pod-name set to
// "<cluster>-<ordinal>".
//
// The pod-name label alone would be sufficient — pod names are unique within a
// namespace — and the cluster labels are carried anyway, deliberately. They are
// the failure floor: should the pod-name entry ever be dropped, the Service
// degrades to "every read-write pod of this cluster", which is precisely what
// the client-facing Service already does and is survivable. Without them the
// same slip degrades to "every pod in the namespace", which is not.
//
// Defined behaviour for inputs writeTarget cannot produce:
//
//   - a negative ordinal reads as pod 0. No pod is named "<cluster>--1", so the
//     honest rendering of a negative ordinal is a Service that selects nothing,
//     and a write Service selecting nothing is the one outcome this file's
//     header forbids outright. Pod 0 exists for as long as the StatefulSet does.
//   - the returned map is never empty and never nil, for any input. An empty
//     selector in a Service spec matches every pod in the namespace.
func writeServiceSelector(cluster string, ordinal int32) map[string]string {
	if ordinal < 0 {
		// No pod is named "<cluster>--1", so the honest rendering of a negative
		// ordinal would be a Service selecting nothing — the one outcome this
		// file's header forbids outright. Pod 0 exists as long as the
		// StatefulSet does.
		ordinal = 0
	}
	sel := selectorLabels(cluster)
	sel[labelStatefulSetPodName] = statefulSetPodName(cluster, ordinal)
	return sel
}

// writeService builds the client-facing Service that names the current writer.
//
// It mirrors readOnlyService, which is this operator's precedent for a
// client-facing Service that exists only in some configurations: same TypeMeta
// (required — applyConfiguration converts through unstructured, and an object
// without one has no GVK to convert to), the same port names and string
// targetPorts, and the same 389/636 defaulting from spec.service.
//
// Two differences from clusterIPService are deliberate:
//
//   - spec.service's LoadBalancerIP, LoadBalancerSourceRanges,
//     ExternalTrafficPolicy and annotations are NOT copied. A LoadBalancer IP is
//     one address and two Services cannot both claim it; readOnlyService leaves
//     them out for the same reason.
//   - PublishNotReadyAddresses stays unset. This is a client endpoint, not pod
//     DNS: stopping traffic at an unready pod is what the readiness probe is for
//     (ADR-030), and sending writes to a pod that is not answering LDAP is worse
//     than sending them to a pod that is merely about to purge.
//
// spec.service.type is honoured, as readOnlyService honours it: a cluster
// exposed as NodePort or LoadBalancer needs its write endpoint reachable by the
// same clients it just asked to follow that endpoint.
//
// writer and ok are writeTarget's pair and must be passed through together.
// They are not two arguments that happen to travel side by side: every ordinal,
// zero included, names a real pod, so a writer of 0 with ok false is "nobody was
// eligible" wearing pod 0's clothes. Taking the ordinal alone would make that
// state buildable, and the Service built from it would steer every write in the
// cluster onto pod 0 — which, on the slot where pod 0 is the one purging, is
// precisely the outcome this whole effort exists to prevent, arriving silently
// and with every health signal green.
//
// So the no-writer case is unrepresentable rather than merely untaken. ok false
// returns (nil, false), and the second return says "there is no Service to
// apply this pass" — the caller leaves the selector exactly where it was, which
// is what PURGE-ROTATION-PLAN's "Known risks" requires: operator downtime
// degrades to today, but the write Service must never be left selecting
// nothing. A guard in the reconciler would do the same job right up until
// somebody refactors the reconciler.
//
// A nil cluster returns (nil, false) for the same reason.
func (r *SlapdClusterReconciler) writeService(
	sc *ldapv1alpha1.SlapdCluster, writer int32, ok bool,
) (*corev1.Service, bool) {
	if sc == nil || !ok {
		return nil, false
	}

	svcType := sc.Spec.Service.Type
	if svcType == "" {
		svcType = corev1.ServiceTypeClusterIP
	}
	ldapPort := sc.Spec.Service.LDAPPort
	if ldapPort == 0 {
		ldapPort = 389
	}
	ldapsPort := sc.Spec.Service.LDAPSPort
	if ldapsPort == 0 {
		ldapsPort = 636
	}

	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: kindService},
		ObjectMeta: metav1.ObjectMeta{
			Name:      writeServiceName(sc.Name),
			Namespace: sc.Namespace,
			Labels:    managedLabels(),
		},
		Spec: corev1.ServiceSpec{
			Type:     svcType,
			Selector: writeServiceSelector(sc.Name, writer),
			Ports: []corev1.ServicePort{
				{
					Name:       portNameLDAP,
					Port:       ldapPort,
					TargetPort: intstr.FromString(portNameLDAP),
					Protocol:   corev1.ProtocolTCP,
				},
				{
					Name:       portNameLDAPS,
					Port:       ldapsPort,
					TargetPort: intstr.FromString(portNameLDAPS),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}, true
}

// nextSlotBoundary returns how long from now until the next slot boundary of
// the absolute grid, for the reconciler's RequeueAfter.
//
// The grid is the one rotationSlot reads and purgePhase writes: anchored to the
// Unix epoch, counted in whole seconds, because the accesslog-purge-phase patch
// stores the phase in tv_sec. nextSlotBoundary must therefore truncate t to
// whole seconds exactly as rotationSlot does — a requeue computed on a finer
// grid than the one being rotated would wake the controller just before or just
// after the slot it meant to catch, every time, and the drift would be silent.
//
// The return is STRICTLY POSITIVE for every input. That is the hardest rule in
// this file:
//
//   - exactly on a boundary it returns a full t, not zero. A slot that has just
//     begun has a whole slot left in it; zero would requeue instantly and the
//     next pass would find itself on the same boundary again.
//   - a sub-second t, and any non-positive t, mean there is no grid at all — the
//     same reading rotationSlot takes — and the answer is noGridRequeue. The
//     rotation gate has already refused such a slot length with a condition
//     (reasonSlotLengthNotPositive, reasonSlotLengthNotWholeSeconds), so this is
//     the defensive end of a path that should never be walked; what matters is
//     that walking it costs one requeue every ten seconds and not a hot loop.
func nextSlotBoundary(now time.Time, t time.Duration) time.Duration {
	// Truncated to whole seconds exactly as rotationSlot does. Two grids — one
	// from the requeue, one from the slot lookup — would disagree by the
	// remainder every single cycle, silently.
	step := int64(t / time.Second)
	if step <= 0 {
		return noGridRequeue
	}

	// Floor division, not Go's truncation, so the grid stays continuous across
	// the epoch.
	sec := now.Unix()
	slots := sec / step
	if sec < 0 && sec%step != 0 {
		slots--
	}

	// How far into the current slot we are, at nanosecond resolution. By
	// construction that is in [0, step) seconds, so the answer below is
	// strictly positive and at most a whole slot: exactly on a boundary it is a
	// full slot, never zero.
	elapsed := time.Duration(sec-slots*step)*time.Second + time.Duration(now.Nanosecond())
	return time.Duration(step)*time.Second - elapsed
}

// eligibleForWrites composes the predicate writeTarget consumes: the pods that
// may take writes, by everything the grid itself does not know.
//
// Two terms today.
//
// ready is per-pod readiness, and a pod ABSENT from the map is not ready.
// Unreadable evidence never counts in favour — ADR-025's rule, carried onto
// this surface. A pod the operator could not assess is not a pod it may point
// the write endpoint at.
//
// heldReadOnly is ADR-036's authorship gate: a rebuilt pod that has not
// recovered its own authorship is read-only, and pointing writes at it would
// produce exactly the unwillingToPerform the gate exists to produce. ADR-036 is
// still PROPOSED and nothing populates this map, so the term is inert by
// construction — an empty or nil map changes no outcome. It is written as a
// seam, never as a dependency that blocks: the rotation must work today, and
// must not need editing the day the gate lands.
//
// Decision 4's remaining promotion terms — a provider still working off its
// purge backlog, its replication lag, or a recent absence — join here when they
// are measured, and for the same reason: this predicate is the one place where
// anything beyond the grid may speak. What may never join it is anything
// derived from observed CLIENT behaviour. Option E was rejected on exactly that
// (a liveness requirement defeasible by the clients it protects), and decision 4
// turned the rejection into a rule: gate on operator-owned state, never on
// client-driven state.
//
// Both maps are read-only to the returned closure, and it is safe to call for
// any ordinal, including negative ones.
func eligibleForWrites(ready map[int32]bool, heldReadOnly map[int32]bool) func(int32) bool {
	return func(ordinal int32) bool {
		if ordinal < 0 {
			return false
		}
		// Absence is not readiness. A pod the operator could not assess is not
		// a pod it may point the write endpoint at (ADR-025's rule, carried
		// onto this surface).
		if !ready[ordinal] {
			return false
		}
		// ADR-036's authorship gate. Nothing populates this map today, so a nil
		// one and an empty one must change no outcome at all; the seam is inert
		// by construction rather than by a branch.
		return !heldReadOnly[ordinal]
	}
}

// writeRotationCondition renders the rotation's verdict as the published
// WriteEndpointRotating condition. Observation only: it names the pod the write
// Service points at and the instant it moves next.
//
// ok is writeTarget's boolean, and it is the whole reason this function takes
// one. A writer of 0 with ok true is pod 0; a writer of 0 with ok false is "no
// pod could be chosen", and the two must never read alike — the second is the
// state in which the Service was deliberately left pointing where it already
// pointed, and a condition implying it had been repointed would send a reader
// looking for a move that did not happen.
//
// next is the instant the writer changes. A zero time means no next boundary is
// known, and the message says so rather than rendering year 1 into a
// user-visible string.
//
// A nil cluster yields a condition with no ObservedGeneration, as
// rotationConfiguredFor does.
func writeRotationCondition(
	sc *ldapv1alpha1.SlapdCluster, writer int32, ok bool, next time.Time, lead time.Duration,
) metav1.Condition {
	cond := metav1.Condition{
		Type:               writeEndpointCondition,
		LastTransitionTime: metav1.Now(),
	}
	if sc != nil {
		cond.ObservedGeneration = sc.Generation
	}

	// With a lead in force `next` is the instant the ENDPOINT MOVES, which is
	// one lead before the slot boundary the purge grid turns on — so it is
	// named as a move rather than as a rotation, and the lead is spelled out.
	// Without one the two instants coincide and the wording is left exactly as
	// it shipped before leadTime existed: a cluster that set no lead must read
	// byte-identically to one running the version that had no such field.
	when := "the next rotation instant is not known"
	if !next.IsZero() {
		when = "next rotation at " + next.UTC().Format(time.RFC3339)
	}
	if lead > 0 {
		when = "the next endpoint move instant is not known"
		if !next.IsZero() {
			when = "next endpoint move at " + next.UTC().Format(time.RFC3339)
		}
		when += fmt.Sprintf(", %s ahead of the slot boundary", lead)
	}

	if !ok {
		cond.Status = metav1.ConditionFalse
		cond.Reason = reasonNoEligibleWriter
		// Names no pod, deliberately. writeTarget answers 0 when nothing was
		// eligible, and rendering that zero as "writes are going to <cluster>-0"
		// is how a total outage comes to read as a healthy rotation.
		cond.Message = "no pod was eligible to take writes this pass, so the write endpoint is " +
			"unchanged: it still selects whatever it selected before, and writes keep landing " +
			"there. A Service selecting nothing would be a total write outage, which is worse " +
			"than an endpoint that stopped moving (ADR-037). " + when
		return cond
	}

	cond.Status = metav1.ConditionTrue
	cond.Reason = reasonWriterSelected
	target := fmt.Sprintf("pod ordinal %d", writer)
	if sc != nil {
		target = statefulSetPodName(sc.Name, max(writer, 0))
	}
	cond.Message = fmt.Sprintf("the write endpoint selects %s; %s", target, when)
	return cond
}

// writerSlot returns the slot the write endpoint steers by: the slot whose
// purge is current at now+lead rather than at now (ADR-037, spec.rotation.leadTime).
//
// The lead exists because the handover is where the cost is. With no lead the
// endpoint moves AT the boundary, so the pod that stops taking writes begins
// purging at the same instant and every connection that has not aged out
// delivers into an active purge — measured as 99.9 % of a 2.8 % throughput loss
// landing in the first 30 s after a boundary. Reading the grid one lead FORWARD
// moves the endpoint that much earlier, which is the whole mechanism.
//
// It is a lookahead and only a lookahead: the purge grid is rotationSlot's and
// is untouched, so a lead changes who takes writes and never when anybody
// purges. A zero lead is rotationSlot exactly, which is the compatibility
// guarantee the field rests on.
//
// A NEGATIVE lead would read the grid backwards — the endpoint moving LATE,
// strictly worse than today — and the rotation gate refuses one with a
// condition before it can reach here. This is pure arithmetic and defines the
// case rather than guarding it: now+lead is simply earlier, and the answer
// stays a valid slot.
func writerSlot(now time.Time, lead, t time.Duration, n int32) int32 {
	return rotationSlot(now.Add(lead), t, n)
}

// nextWriterMove returns how long until the write endpoint moves next: one lead
// before the next slot boundary.
//
// The requeue has to fire when the WRITER changes, not when the purge grid
// turns over. Asking nextSlotBoundary about now+lead gives exactly that — the
// next boundary after now+lead is B, the writer changes at B−lead, and the wait
// is B−lead−now, which is what nextSlotBoundary already returned from now+lead.
//
// Every property nextSlotBoundary guarantees carries through unchanged, and the
// strictly-positive one is the load-bearing one: a non-positive requeue is a hot
// loop against the API server. Exactly one lead before a boundary the answer is
// a full slot, not zero — the move has just happened and the next one is a whole
// slot away.
func nextWriterMove(now time.Time, lead, t time.Duration) time.Duration {
	return nextSlotBoundary(now.Add(lead), t)
}
