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

// seedSiteDecision is the verdict of the ADR-028 §3 site gate: may THIS
// operator apply the seed its SlapdDatabase declares?
//
// Three values rather than a bool, because the two ways of saying "no" have to
// latch differently. See decideSeedSite.
type seedSiteDecision int

const (
	// seedSiteApply: no site is declared (today's behaviour), or this operator
	// is the declared founder. Proceed to the ADR-025 evidence belt.
	seedSiteApply seedSiteDecision = iota
	// seedSiteWithhold: a site is declared and it is not this one. Permanent by
	// nature — this operator will never be the founder for this database — so
	// the caller latches SeedApplied. The DIT arrives by replication.
	seedSiteWithhold
	// seedSiteUnknownIdentity: a site is declared, but this operator has no
	// identity to compare it against (SITE_NAME unset). Withhold too, but this
	// is a MISCONFIGURATION rather than a fact about the topology: the caller
	// must requeue and stay loud rather than latch, so that setting SITE_NAME
	// later still seeds.
	seedSiteUnknownIdentity
)

// decideSeedSite is the ADR-028 §3 site gate, pure.
//
// A mesh-scoped resource carries no per-site fields, so a seed may not be
// present at one site and stripped at the others — a per-site *edit* of an
// object that is supposed to be byte-identical everywhere. It becomes a
// DECLARATION instead — seed.site: site-a — read identically at every site,
// with each operator comparing that name against its own identity (ADR-028 §4,
// resolved by ResolveSiteName from the operator's own installation config).
//
// selector is spec.seed.site; identity is the operator's SiteName. Both are
// normalised the way ResolveSiteName normalises the env var, so a stray newline
// in a values file cannot silently turn a founder into a non-founder. Beyond
// that the comparison is verbatim: case is NOT folded, because a site is
// whatever the operator chart was given and folding would make two distinct
// SITE_NAME values collide.
//
// Withholding on an unknown identity is deliberate and was decided explicitly
// (2026-09-17, MESH-PLAN Phase 2): unreadable identity never counts as a match,
// the same rule ADR-008 applies to CSN evidence. A database that never seeds is
// loud and recoverable; the opposite failure — several sites seeding one suffix
// independently — manufactures the ADR-025 glue suffix, which is silent,
// permanent, reads clean on every CSN health check, and makes every backup
// taken from that pod unrestorable.
//
// This gate is the braces; ADR-025's seedCreatorIsForeign belt is untouched and
// independent of it. The gate acts on what the spec DECLARES, the belt on what
// the directory shows has already HAPPENED. Both still fire.
func decideSeedSite(selector, identity string) seedSiteDecision {
	site := normalizeSiteName(selector)
	if site == "" {
		return seedSiteApply
	}
	self := normalizeSiteName(identity)
	if self == "" {
		return seedSiteUnknownIdentity
	}
	if site != self {
		return seedSiteWithhold
	}
	return seedSiteApply
}
