/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// StartupConfigConverged: does every pod we could read run the startup-only
// configuration this database declares? (ADR-034 R3, as the 2026-10-15
// amendment renamed it.)
//
// Named positively, like every other condition this project publishes: True is
// the good case. R3's original ConfigRestartRequired inverted that, and a
// reader who has learned TunablesConverged / DataPresent / PodsResponsive reads
// a True as "fine" without checking which way this one points.
//
// Observation only, and that is not a stylistic choice. olcSpSessionlogSource
// cannot be written to a running slapd at all; putting it into a converging
// loop would make the operator a crash loop firing on the next reconcile after
// anyone edits the field. The change path is a pod restart, and the operator
// does not take it either — restarting a provider fleet is an ADR-016 topology
// event at every peer (130-268 s of address rediscovery each), far too much to
// happen behind a user's back because they changed a tunable. An opt-in
// operator-driven roll belongs to ADR-023.
const startupConfigConvergedCondition = "StartupConfigConverged"

// startupAttrDivergence is one attribute on one pod whose live cn=config value
// is not the declared one. Want "" means the attribute must be absent — the
// state the converger produces for a database that dropped the field, and a
// divergence like any other until the pod restarts.
type startupAttrDivergence struct {
	Attribute string
	Want      string
	Got       string
}

// startupConfigObservation is one pod's answer.
//
// assessed is the ADR-025 rule in a field: a pod that could not be dialled,
// bound, or whose syncprov overlay could not be read is not evidence, and must
// never make the condition False. diverged is only meaningful when assessed.
//
// Read-only consumer pods produce no observation at all — they carry no
// syncprov overlay (ADR-019 R3), so there is nothing on them to compare. That
// reads as "nothing to say", never as divergence.
type startupConfigObservation struct {
	pod      string
	assessed bool
	diverged []startupAttrDivergence
}

// compareStartupAttr decides whether one pod's live values for one startup-only
// attribute match what the table declares. Pure.
//
// want "" means "must be absent". Values are compared case-insensitively after
// trimming: every startup-only value today is a DN, slapd normalises DN case on
// the way in, and a comparison that called cn=accesslog-default and
// CN=Accesslog-Default different would report an eternal, unfixable divergence
// — a condition that can never go True is worse than no condition.
//
// More than one live value is a divergence whatever the values are: these
// attributes are single-valued, and a second one is a configuration nobody
// declared.
func compareStartupAttr(attr, want string, live []string) (startupAttrDivergence, bool) {
	var got string
	switch len(live) {
	case 0:
	case 1:
		got = strings.TrimSpace(live[0])
	default:
		return startupAttrDivergence{
			Attribute: attr, Want: want, Got: strings.Join(live, " | "),
		}, true
	}

	if strings.EqualFold(got, strings.TrimSpace(want)) {
		return startupAttrDivergence{}, false
	}
	return startupAttrDivergence{Attribute: attr, Want: want, Got: got}, true
}

// declaresStartupOnlyAttributes reports whether this database asks for anything
// in the startup-only table. Today exactly one question: does its data DB want
// its syncprov session log sourced from its own change journal?
//
// Derived from desiredSessionlog, the single resolution the ConfigMap writer
// uses as well, so the condition cannot disagree with the table the pods are
// handed about which database is sourced or with which DN.
func declaresStartupOnlyAttributes(sd *ldapv1alpha1.SlapdDatabase) bool {
	return desiredSessionlog(sd).Kind == sessionlogKindAccesslog
}

// aggregateStartupConfigConverged computes the verdict from per-pod
// observations. Pure.
//
// Three rules:
//
//   - A pod that was not read is noted, never indicted. ADR-025's amendments
//     set this for the suffix probe and the reason carries over unchanged:
//     unreadable evidence never counts toward a verdict. A fleet where one pod
//     is mid-restart must not flip a cluster-wide condition to False — the
//     restart is the very thing the condition asks for.
//   - A database that declares nothing is converged by definition and says so,
//     rather than going Unknown for want of evidence it does not need. The
//     condition is always published, including then: setCondition does not
//     remove conditions, so a database that DROPS the field would otherwise
//     keep a stale False forever.
//   - A leftover value is still a divergence. Dropping the field does not take
//     the attribute off disk until the next pod start, and until then the live
//     configuration is not the declared one — which is exactly what the reader
//     needs to know to decide whether a restart is due.
//
// Self-clearing without a special case (ADR-001): the whole verdict is computed
// from this pass's observations, so a restarted fleet reads Converged on the
// next reconcile with nothing to reset.
func aggregateStartupConfigConverged(
	declared bool, results []startupConfigObservation,
) (metav1.ConditionStatus, string, string) {
	var assessed, unread, details []string
	for _, r := range results {
		if !r.assessed {
			unread = append(unread, r.pod)
			continue
		}
		assessed = append(assessed, r.pod)
		for _, d := range r.diverged {
			details = append(details, fmt.Sprintf("%s (%s: live %s, declared %s)",
				r.pod, d.Attribute, quoteOrNone(d.Got), quoteOrNone(d.Want)))
		}
	}

	unreadNote := ""
	if len(unread) > 0 {
		unreadNote = fmt.Sprintf(" (not assessed, and therefore not counted against the "+
			"verdict: %s)", strings.Join(unread, ", "))
	}

	switch {
	case len(details) > 0:
		return metav1.ConditionFalse, "RestartRequired",
			fmt.Sprintf("the running cn=config differs from the declared startup-only table on "+
				"%s. These attributes cannot be written to a running slapd; the init container "+
				"converges them offline at pod start, so the change lands when the pod(s) "+
				"restart (ADR-034). The operator does not restart them — a provider restart is "+
				"a topology event at every peer (ADR-016)%s",
				strings.Join(details, ", "), unreadNote)

	case !declared && len(assessed) == 0:
		return metav1.ConditionTrue, "NoStartupOnlyAttributes",
			"this database declares no startup-only configuration (ADR-034), so there is " +
				"nothing a pod restart would change"

	case !declared:
		return metav1.ConditionTrue, "NoStartupOnlyAttributes",
			fmt.Sprintf("this database declares no startup-only configuration (ADR-034), and "+
				"none is present on the %d pod(s) read%s", len(assessed), unreadNote)

	case len(assessed) == 0:
		return metav1.ConditionUnknown, "NoReachablePod",
			fmt.Sprintf("could not read the syncprov overlay on any pod, so the declared "+
				"startup-only configuration could not be verified%s", unreadNote)

	default:
		return metav1.ConditionTrue, "Converged",
			fmt.Sprintf("the declared startup-only configuration is in place on all %d pod(s) "+
				"read%s", len(assessed), unreadNote)
	}
}

// quoteOrNone renders a value for the condition message, making an absent one
// visibly absent rather than an empty pair of quotes the reader has to squint
// at.
func quoteOrNone(v string) string {
	if v == "" {
		return "<absent>"
	}
	return fmt.Sprintf("%q", v)
}

// observeStartupConfig records one pod's live startup-only values against what
// this database declares. Nil-safe: a read-only pod is passed nil, because it
// carries no syncprov overlay to read.
//
// Called from ensureSyncprovTunables, off the search that function already
// does. Marking the observation assessed here — after the overlay was found and
// read — is what makes "unreadable" the default: every earlier return, the dial
// included, leaves it false.
func observeStartupConfig(
	obs *startupConfigObservation, decision sessionlogDecision, live []string,
) {
	if obs == nil {
		return
	}
	obs.assessed = true

	want := ""
	if decision.Kind == sessionlogKindAccesslog {
		want = decision.SourceDN
	}
	if d, diverged := compareStartupAttr(startupAttrSessionlogSource, want, live); diverged {
		obs.diverged = append(obs.diverged, d)
	}
}

// startupConfigConvergedFor builds the published condition from this pass's
// observations.
func startupConfigConvergedFor(
	sd *ldapv1alpha1.SlapdDatabase, results []startupConfigObservation,
) metav1.Condition {
	status, reason, msg := aggregateStartupConfigConverged(
		declaresStartupOnlyAttributes(sd), results)
	return metav1.Condition{
		Type:               startupConfigConvergedCondition,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: sd.Generation,
	}
}
