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
	"os"
	"strings"
)

// ResolveSiteName determines which site of a mesh this operator instance runs
// at — the single per-site fact in the whole system (ADR-028 §4). Every other
// mesh-scoped object is byte-identical at every site, so this one deliberately
// lives in the operator's own installation config (the SITE_NAME env var, set
// from the operator chart's `siteName` value) rather than in any CR: putting it
// in a CR would destroy that byte-identical property.
//
// An unset or empty SITE_NAME is legal and returns "" — it means "no mesh
// features", which is what every single-site deployment wants. It is not an
// error and never defaults to a guessed site: getting the identity wrong at two
// sites collides their serverID decades (ADR-017), so a wrong guess is worse
// than no identity at all.
//
// The name is not validated against a site inventory here, because no SlapdMesh
// type exists yet. That cross-check arrives with the type.
func ResolveSiteName() string {
	return normalizeSiteName(os.Getenv("SITE_NAME"))
}

// normalizeSiteName is the pure decision behind ResolveSiteName: trim
// surrounding whitespace (a values file may carry a stray newline, and a name
// that compares unequal to every real site would silently disable mesh
// behaviour), and treat a whitespace-only value as unset rather than as a site
// literally named " ". Interior characters pass through verbatim.
func normalizeSiteName(raw string) string {
	return strings.TrimSpace(raw)
}
