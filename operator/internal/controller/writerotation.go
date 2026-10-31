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
	"context"
	"fmt"

	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The reconcile wiring for the rotating write endpoint (ADR-037 decision 2,
// PURGE-ROTATION-PLAN Phase 4 items 2-4 and 6).
//
// writeservice.go holds the decisions; this file holds the two readings they
// need from the cluster, the application of the result, and the requeue that
// makes any of it happen a second time. Each of the three is a place where the
// feature can be arithmetically perfect and still never fire.

// statefulSetPodName spells a pod name the way the StatefulSet controller does,
// which is what statefulset.kubernetes.io/pod-name carries.
func statefulSetPodName(cluster string, ordinal int32) string {
	return fmt.Sprintf("%s-%d", cluster, ordinal)
}

// readyOrdinals reads per-pod readiness for a cluster's read-write fleet.
//
// Pure, over a pod list the caller fetched — the shape discoverReplicationNetworkIPs
// already uses. Per pod, because the operator's only existing readiness reading
// is sts.Status.ReadyReplicas, which is a count and names nobody; the rotation
// has to know WHICH pod.
//
// The ABSENCES carry as much as the entries: eligibleForWrites reads a pod
// missing from this map as not ready, so a pod that could not be listed is a
// pod the write endpoint will not be pointed at. The map is empty rather than
// nil when there is nothing to say, for the same reason — an empty map makes
// nobody eligible, writeTarget then reports no writer, and the selector stays
// exactly where it was.
//
// A terminating pod is not ready whatever its Ready condition says: a pod being
// deleted keeps Ready=True through its grace period while its endpoint has
// already been withdrawn, and steering writes there sends them nowhere.
func readyOrdinals(pods []corev1.Pod, cluster string) map[int32]bool {
	ready := make(map[int32]bool, len(pods))
	for i := range pods {
		p := &pods[i]
		// providers.go's canonical parse, then a check that the name it parsed
		// is this cluster's. Read from the right alone, "<cluster>-readonly-0"
		// is ordinal 0, and the write endpoint would be steered at a consumer
		// replica that refuses every write it is given.
		ord, err := podOrdinal(p.Name)
		if err != nil || statefulSetPodName(cluster, ord) != p.Name {
			continue
		}
		ready[ord] = p.DeletionTimestamp == nil && podReportsReady(p)
	}
	return ready
}

// podReportsReady reports whether the pod carries Ready=True. A pod with no
// Ready condition at all is not ready: absence of evidence is not evidence.
func podReportsReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// requeueAfterFor returns the pass's RequeueAfter: the sooner of the
// controller's own steady-state cadence and the rotation's next slot boundary.
//
// The rotation term is merged here rather than returned from inside the
// replication branch, and that is the whole point of the function. The
// steady-state requeue is GATED on spec.replication.enabled — a Running cluster
// without replication returns a bare ctrl.Result{} and nothing schedules
// another pass at all (ADR-002's amendment says so in as many words). A
// rotation requeue returned from inside that gate would rotate replicated
// clusters and silently never rotate the others.
//
// Zero means no requeue, and is what both terms answer when they have nothing
// to ask for: with the rotation off the result is byte-identical to the
// behaviour before this feature existed.
func requeueAfterFor(sc *ldapv1alpha1.SlapdCluster, rotation time.Duration) time.Duration {
	base := time.Duration(0)
	switch {
	case sc == nil:
	case sc.Status.Phase != ldapv1alpha1.PhaseRunning:
		base = 10 * time.Second
	case sc.Spec.Replication.Enabled:
		base = csnCheckInterval
	}
	return mergeRequeue(base, rotation)
}

// mergeRequeue returns the sooner of two requeue requests, reading a
// non-positive duration as "nothing to ask for".
func mergeRequeue(a, b time.Duration) time.Duration {
	switch {
	case a <= 0:
		return max(b, 0)
	case b <= 0:
		return a
	default:
		return min(a, b)
	}
}

// reconcileWriteService applies the rotating write endpoint and returns how
// long until the writer moves. Zero means the rotation asks for no requeue.
//
// Errors from the readiness reading are not propagated: a pass that could not
// list pods has read no readiness, every pod is therefore ineligible,
// writeTarget reports no writer and the selector is left exactly where it is.
// That is the designed answer to unreadable evidence, not a swallowed failure —
// and it is reported, as WriteEndpointRotating=False/NoEligibleWriter.
func (r *SlapdClusterReconciler) reconcileWriteService(
	ctx context.Context, sc *ldapv1alpha1.SlapdCluster, plan rotationPlan, now time.Time,
) (time.Duration, error) {
	if sc == nil {
		return 0, nil
	}
	if !plan.WriterActive {
		meta.RemoveStatusCondition(&sc.Status.Conditions, writeEndpointCondition)
		return 0, r.retireWriteService(ctx, sc)
	}

	log := logf.FromContext(ctx)

	ready := map[int32]bool{}
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(sc.Namespace),
		client.MatchingLabels(selectorLabels(sc.Name)),
	); err != nil {
		log.Info("could not read pod readiness; the write endpoint is left where it is",
			"cluster", sc.Name, "err", err)
	} else {
		ready = readyOrdinals(podList.Items, sc.Name)
	}

	// The slot the endpoint steers by, read one LEAD ahead (ADR-037,
	// spec.rotation.leadTime). With no lead this is the slot current at `now`
	// and the whole pass is what it was before the field existed; with one, the
	// endpoint has already moved to the next slot's writer while the current
	// slot's purge is still running, so a connection no older than the lead
	// never delivers into the purge that starts at the boundary.
	slot := writerSlot(now, plan.LeadTime, plan.SlotLength, plan.Replicas)
	// heldReadOnly is ADR-036's authorship gate. It is Proposed and populates
	// nothing, so the term is inert; it is passed rather than omitted so the
	// day it lands is a change at one call site.
	writer, ok := writeTarget(slot, plan.Replicas, plan.SlotsFromWriteToPurge, eligibleForWrites(ready, nil))

	// The requeue fires when the WRITER changes — one lead before the next slot
	// boundary — not when the purge grid turns over.
	requeue := nextWriterMove(now, plan.LeadTime, plan.SlotLength)
	setCondition(&sc.Status.Conditions, writeRotationCondition(sc, writer, ok, now.Add(requeue), plan.LeadTime))

	svc, apply := r.writeService(sc, writer, ok)
	if !apply {
		log.Info("no pod is eligible to take writes; the write endpoint keeps the selector it has",
			"cluster", sc.Name, "slot", slot, "readyPods", len(ready))
		return requeue, nil
	}

	moved, err := r.writeSelectorMoved(ctx, sc, svc)
	if err != nil {
		return requeue, err
	}
	if err := r.applyWriteService(ctx, sc, svc); err != nil {
		return requeue, err
	}
	if moved {
		log.Info("write endpoint repointed",
			"cluster", sc.Name, "service", svc.Name, "slot", slot,
			// The pod purging in the slot the writer was chosen AGAINST. With a
			// lead that slot is the one about to begin, not the one running —
			// calling it "purgingPod" would misname it at exactly the instant
			// the lead exists for.
			"purgingPodOfSlot", statefulSetPodName(sc.Name, slot),
			"leadTime", plan.LeadTime,
			"writer", svc.Spec.Selector[labelStatefulSetPodName],
			"nextRotation", now.Add(requeue).UTC().Format(time.RFC3339))
	}
	return requeue, nil
}

// writeSelectorMoved reports whether the Service about to be applied names a
// different pod than the one in the cluster does.
//
// The apply itself is unconditional — an SSA apply of an unchanged object is a
// no-op at the server, and making it conditional would mean a Service somebody
// deleted by hand never came back. This is only about the log line: one entry
// per rotation is the audit trail of the feature; one per pass is noise from a
// controller whose real duty cycle is a handful of writes a day.
func (r *SlapdClusterReconciler) writeSelectorMoved(
	ctx context.Context, sc *ldapv1alpha1.SlapdCluster, desired *corev1.Service,
) (bool, error) {
	cur := &corev1.Service{}
	err := r.getManaged(ctx, client.ObjectKey{Name: desired.Name, Namespace: sc.Namespace}, cur)
	if errors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("get write Service: %w", err)
	}
	return cur.Spec.Selector[labelStatefulSetPodName] !=
		desired.Spec.Selector[labelStatefulSetPodName], nil
}

// applyWriteService is the SSA patch every Service on this controller takes.
func (r *SlapdClusterReconciler) applyWriteService(
	ctx context.Context, sc *ldapv1alpha1.SlapdCluster, svc *corev1.Service,
) error {
	if err := controllerutil.SetControllerReference(sc, svc, r.Scheme); err != nil {
		return err
	}
	ac, err := applyConfiguration(svc)
	if err != nil {
		return err
	}
	return r.Apply(ctx, ac, client.ForceOwnership, client.FieldOwner(fieldManager))
}

// retireWriteService is what happens to <cluster>-write when the rotation is
// off — switched off deliberately, or declared so badly the gate refused it.
//
// Two things it does NOT do, each of them a decided rejection:
//
//   - it does not CREATE the Service. A cluster that never rotated must not
//     grow an endpoint because the operator was upgraded; off has to be
//     byte-identical to before the feature existed (ADR-035's reading of
//     ADR-013). So this runs only against a Service that already exists, which
//     makes it self-limiting: only a cluster that once rotated carries one.
//   - it does not DELETE it. Clients were told to follow this name; removing it
//     turns an operator-side configuration change into a write outage at every
//     client that took the advice, which is the one outcome ADR-037's write
//     path forbids outright.
//
// What it does instead is drop the pod-name entry, which leaves the endpoint an
// alias of the client-facing Service: writes keep flowing and they spread over
// the whole read-write fleet. The alternative — leaving the selector frozen on
// whichever pod held it last — keeps every write pinned to one pod whose purge
// is no longer phased by anybody, which is precisely the stall ADR-037's third
// amendment measured, arriving at the moment the user turns the feature OFF and
// believes they have returned to the old behaviour.
func (r *SlapdClusterReconciler) retireWriteService(
	ctx context.Context, sc *ldapv1alpha1.SlapdCluster,
) error {
	name := writeServiceName(sc.Name)
	if name == "" {
		return nil
	}

	cur := &corev1.Service{}
	err := r.getManaged(ctx, client.ObjectKey{Name: name, Namespace: sc.Namespace}, cur)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get write Service: %w", err)
	}
	if cur.Spec.Selector[labelStatefulSetPodName] == "" {
		return nil
	}

	logf.FromContext(ctx).Info("rotation is off; the write endpoint stops naming one pod and "+
		"becomes an alias of the client Service", "cluster", sc.Name, "service", name,
		"wasSelecting", cur.Spec.Selector[labelStatefulSetPodName])
	return r.applyWriteService(ctx, sc, r.fleetWriteService(sc))
}

// fleetWriteService is <cluster>-write with the rotation's pod-name entry
// dropped: the same object, the same ports, selecting the whole read-write
// fleet. See retireWriteService for why that is the retired shape.
func (r *SlapdClusterReconciler) fleetWriteService(sc *ldapv1alpha1.SlapdCluster) *corev1.Service {
	svc, ok := r.writeService(sc, 0, true)
	if !ok {
		return nil
	}
	svc.Spec.Selector = selectorLabels(sc.Name)
	return svc
}
