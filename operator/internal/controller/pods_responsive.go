/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	ldap "github.com/go-ldap/ldap/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PodsResponsive — a pod that stops answering says so on the CR.
//
// During the 874 s freeze (docs/BACKLOG.md, "The `cn=config` pause freeze") the
// operator dialled the frozen pod every 60 s and timed out every time: 106
// consecutive failures, logged at V(1), reaching nobody. The evidence was
// gathered and thrown away, and `kubectl get slapdcluster` reported Running
// throughout.
//
// ADR-030's probe now removes such a pod from the Service endpoints, so
// readyReplicas finally moves. This condition answers the next question —
// WHICH pod, and what the operator saw — from readings that already exist. It
// costs no extra queries.
//
// Deliberately observability only (the ADR-025 DataPresent posture): it never
// drives reconcile behaviour. The phase already tells the truth now that
// readyReplicas can fall.

const podsResponsiveCondition = "PodsResponsive"

// podsResponsiveVerdict is what one pass of readings says about reachability.
type podsResponsiveVerdict struct {
	Status  metav1.ConditionStatus
	Reason  string
	Message string
}

// evaluatePodsResponsive judges whether every read-write pod answered LDAP.
//
// Scope is the read-write fleet, because that is what checkLocalCSNConvergence
// dials; read-only pods are covered by their own readiness probe and by
// DataPresent's RO clause (ADR-025), not here.
//
// Three rules, each one a discrimination that makes the condition mean
// something narrower and therefore more useful:
//
//  1. A pod is responsive if ANY of its queries got an answer. A pod silent on
//     one database but answering on another is up — that is a per-database
//     problem, and ReplicationConverged already reports it as
//     CSNQueriesIncomplete. Reporting it here too would make this condition
//     fire for reasons that have nothing to do with the pod.
//  2. An LDAP result code is an answer. A bind rejection means the server is up
//     and saying no; calling that unresponsive sends someone to the wrong
//     place. Same rule as ADR-030's probe.
//  3. A pod that produced no reading at all is silent, not fine. Absence of
//     evidence never counts toward the good verdict — the correction ADR-008's
//     2026-09-14 amendment made for CSN readings.
func evaluatePodsResponsive(readings []csnReading, replicas int32) podsResponsiveVerdict {
	if len(readings) == 0 {
		return podsResponsiveVerdict{
			Status:  metav1.ConditionUnknown,
			Reason:  "NoPodsQueried",
			Message: "no contextCSN queries were made this pass, so pod reachability is unobserved",
		}
	}

	answered := map[string]bool{}
	lastErr := map[string]string{}
	for _, r := range readings {
		if r.Answered {
			answered[r.Pod] = true
			continue
		}
		if _, seen := answered[r.Pod]; !seen {
			answered[r.Pod] = false
		}
		if r.Err != "" {
			lastErr[r.Pod] = r.Err
		}
	}

	var silent []string
	for i := range replicas {
		pod := fmt.Sprintf("%s-%d", podBaseName(readings), i)
		if ok, seen := answered[pod]; seen && ok {
			continue
		} else if seen {
			silent = append(silent, fmt.Sprintf("%s (%s)", pod, lastErr[pod]))
		} else {
			// Rule 3: never dialled, or dialled and the result never recorded.
			silent = append(silent, pod+" (no reading)")
		}
	}
	sort.Strings(silent)

	if len(silent) == 0 {
		return podsResponsiveVerdict{
			Status:  metav1.ConditionTrue,
			Reason:  "AllPodsAnswering",
			Message: fmt.Sprintf("all %d read-write pods answered LDAP", replicas),
		}
	}
	return podsResponsiveVerdict{
		Status: metav1.ConditionFalse,
		Reason: "PodsNotAnswering",
		Message: fmt.Sprintf("%d of %d read-write pods did not answer LDAP: %s",
			len(silent), replicas, strings.Join(silent, "; ")),
	}
}

// podBaseName recovers the StatefulSet's pod-name prefix from the readings, so
// the verdict can name a pod that produced no reading of its own.
func podBaseName(readings []csnReading) string {
	for _, r := range readings {
		if i := strings.LastIndex(r.Pod, "-"); i > 0 {
			return r.Pod[:i]
		}
	}
	return ""
}

// serverAnswered reports whether an LDAP query error still proves the server is
// up: a protocol-level result code came back over a working connection.
//
// go-ldap wraps both kinds in *ldap.Error, so the discriminator is the result
// code itself — the synthetic ones it invents when there was no conversation
// (ErrorNetwork and friends) are exactly the cases where nothing answered.
func serverAnswered(err error) bool {
	if err == nil {
		return true
	}
	var lerr *ldap.Error
	if !errors.As(err, &lerr) {
		return false // a bare net.Error: dial refused, timeout, TLS stall
	}
	// go-ldap numbers the codes IT invents from 200 up (ErrorNetwork = 200 and
	// the client-side errors after it); everything below is a result code that
	// came back over the wire from a server that was listening and talking.
	// The boundary is the whole discriminator, and it holds for codes added
	// later, which an explicit case list would not.
	return lerr.ResultCode < 200
}
