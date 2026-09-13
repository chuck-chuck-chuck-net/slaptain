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
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// podPresence is one RW pod's answer to "is the suffix's root entry visible to
// an ordinary base search?". present is only meaningful when reached is true.
type podPresence struct {
	pod     string
	reached bool
	present bool
}

// aggregateDataPresent computes the DataPresent verdict from per-pod probes.
//
// Pure observability (ADR-012: never drives reconciler action). Every reached
// pod must show the root entry (ADR-025): a suffix demoted to a hidden GLUE
// entry is invisible to ordinary searches on exactly one pod while its peers
// look healthy, so an any-pod-visible verdict reads True over a silently
// broken pod — the exact blindness the 2026-09-13 incident rode on.
func aggregateDataPresent(suffix string, results []podPresence) (metav1.ConditionStatus, string, string) {
	var present, missing, unreached []string
	for _, r := range results {
		switch {
		case !r.reached:
			unreached = append(unreached, r.pod)
		case r.present:
			present = append(present, r.pod)
		default:
			missing = append(missing, r.pod)
		}
	}

	uncheckedNote := ""
	if len(unreached) > 0 {
		uncheckedNote = fmt.Sprintf(" (unreachable, not assessed: %s)", strings.Join(unreached, ", "))
	}

	switch {
	case len(present) == 0 && len(missing) == 0:
		return metav1.ConditionUnknown, "NoReachablePod",
			"could not reach any RW pod to verify data presence"
	case len(missing) == 0:
		return metav1.ConditionTrue, "RootEntryVisible",
			fmt.Sprintf("root entry %s visible on all %d reached RW pod(s)%s",
				suffix, len(present), uncheckedNote)
	case len(present) == 0:
		return metav1.ConditionFalse, "DataMissing",
			fmt.Sprintf("root entry %s not visible on any reachable RW pod — possible data loss; "+
				"this condition is informational and does not trigger operator action (see ADR-012)%s",
				suffix, uncheckedNote)
	default:
		return metav1.ConditionFalse, "DataMissingOnPods",
			fmt.Sprintf("root entry %s hidden or absent on pod(s) %s while visible on %s — "+
				"likely a hidden glue suffix entry from a multi-site seed race (ADR-025); "+
				"verify with a ManageDSAIT base search; informational only (ADR-012)%s",
				suffix, strings.Join(missing, ", "), strings.Join(present, ", "), uncheckedNote)
	}
}
