# Mesh Layer Implementation Plan (ADR-028)

Engineering breakdown for the `SlapdMesh` layer decided in
[`docs/adrs/adr-028-mesh-scoped-vs-site-scoped.md`](adrs/adr-028-mesh-scoped-vs-site-scoped.md).
Read the ADR first for the *why*; this file is the *how* and the *order*.

Phases are ordered so each one leaves the tree green and shippable, and so that
the two genuinely risky steps (serverID derivation, the e2e port) land late,
after the cheap ones have proven the model.

## Resuming this work

1. Read ADR-028 (the four layers, the no-per-site-fields rule, what is rejected).
2. Read ADR-025 (single-creator seed), ADR-017 (bare-integer `olcServerID`) and
   ADR-013 (a template change rolls the cluster) — Phases 1, 3 and 4 each turn
   on one of them.
3. Phases are sequential by dependency, except Phase 1 which is independent and
   can land alone.

## Two hazards to read before writing any code

**1. `seed.site` is a prerequisite, not a follow-up.** The feature's whole point
is one chart applied identically to every site. Apply today's `SlapdDatabase` —
which carries `spec.seed` — identically to N sites and every site seeds, which is
the ADR-025 multi-site seed race: one pod ends up with a permanent hidden glue
suffix, invisible to ordinary searches and clean on every CSN health read. The
operator's withhold belt is a belt, and ADR-025 says explicitly that the fixture
must not rely on winning that race. So Phase 1 is not optional and not reorderable.

**2. `olcServerID` is baked into every CSN ever written.** A CSN is
`timestamp#count#sid#mod`. If the mesh derivation gives a running pod a different
`serverID` than it has today, its historical CSNs stay under the old sid while new
ones appear under a new one, and per-site CSN tracking is no longer comparable
with itself. The derivation must therefore be **value-compatible with the current
scheme** — site index × 100, then ADR-017's `serverIDBase + ordinal + 1` — for any
cluster that already exists. Treat a derivation that changes an existing pod's
`serverID` as a defect, not a migration. Assert it in a unit test with the lab's
actual values (`scm-s1`→0, `scm-s2`→100, `scm-s3`→200).

## Conventions this plan follows

- Test-first, red-first (CLAUDE.md "Test Discipline"). Every derivation is a pure
  function so the red is fast; the controller wiring is a thin shell over it.
- Replication-path changes see a multi-site e2e cycle before merge, or the commit
  says plainly that multi-site is unvalidated. Phases 4 and 6 are replication-path
  changes.
- `make operator-generate operator-manifests` after every type change; commit the
  generated CRD and the `charts/operator/crds/` copy together.
- Both paths work simultaneously from Phase 4 until Phase 7. Nothing is removed
  until the e2e runs green on the new path.

## Phase 0 — Scaffolding

- `kubebuilder create api` for `SlapdMesh` (namespaced, group `ldap.chuck-chuck-chuck.net`, v1alpha1).
- RBAC for the new kind; regenerate `config/rbac/role.yaml`.
- Done when: `make build` and `make operator-manifests` are green and the CRD
  appears in `charts/operator/crds/`.

## Phase 1 — `seed.site` (independent; unblocks the byte-identical chart)

- Add a site selector to the seed spec on `SlapdDatabase`. Semantics: the seed is
  applied only where the operator's own site identity matches; every other site
  withholds and receives the DIT by replication.
- Unset keeps today's behaviour, so single-site deployments and the existing
  fixture are untouched.
- Interaction to preserve: ADR-025's withhold belt (a foreign suffix creator
  suppresses the seed) stays as the belt. This is the braces.
- Tests: unit red-first on the pure decision (`shouldSeed(site, selector, identity)`),
  covering unset, match, mismatch, and unknown-identity. e2e: the founder seeds,
  the peers do not, `DataPresent` goes True everywhere by replication.
- Done when: `tests/e2e.sh` no longer needs `strip_seed_block`, and deleting that
  function is part of this phase, not a later cleanup.

## Phase 2 — `SlapdMesh` types + the derivation seam (no behaviour change)

- Types: `sites[]` (name, endpoint, kubeconfig secret name), `network`
  (`pod-routed` | `multus` + NAD), trust (CA secret names / issuer ref).
- Pure functions, no client, no I/O:
  - `serverIDBaseForSite(mesh, siteName) (int32, error)` — **must** reproduce
    site-index × 100; see hazard 2.
  - `externalPeersForSite(mesh, cluster, siteName) []ExternalPeer` — honours the
    `sites` selector on `SlapdCluster`, excludes self, produces exactly the peer
    set `e2e.sh` produces today.
  - `resolveSelfSite(env, mesh) (string, error)` — errors loudly on a name absent
    from `sites[]`; see Phase 3.
- Tests: table-driven unit tests red-first, including the lab's three-site values
  as a golden case, subset selectors, a single-site mesh, and a self-site not
  present in the mesh.
- Done when: the functions exist and are tested; nothing calls them yet.

## Phase 3 — Operator self-identity

- The operator learns its own site name from its installation config (Helm value
  → env var on the Deployment, alongside the existing `OPERATOR_IMAGE` /
  `OPERATOR_IMAGE_TAG` precedent). It does **not** come from any CR — that is what
  keeps every mesh-scoped object byte-identical.
- Unset is legal and means "no mesh features"; a `meshRef` with no identity is an
  error surfaced on the CR, never a silent default to the first site.
- Tests: unit on `resolveSelfSite`; a controller test that a `meshRef` without
  identity reports an error condition rather than guessing.
- Done when: `helm upgrade` can set it and the operator logs its identity once at
  startup.

## Phase 4 — `meshRef` resolution in the `SlapdCluster` controller

**Replication-path change — needs a mesh cycle before merge.**

- Add `meshRef` and the `sites` selector to `SlapdClusterSpec`.
- When `meshRef` is set, derive `serverIDBase`, `externalPeers`, network mode and
  trust wiring from the mesh + self-identity. When it is unset, behave exactly as
  today.
- Explicitly specified values and `meshRef` together: reject with a clear
  condition rather than silently preferring one. Ambiguity here is how a
  `serverID` collision gets introduced quietly.
- A missing or unreadable `SlapdMesh` is an error condition on the cluster, never
  a fallback to defaults.
- Tests: unit on the resolution decision; e2e asserting a `meshRef` cluster
  produces byte-identical stanzas to an explicitly configured one — that
  equivalence is the phase's real acceptance criterion.
- Done when: a three-site lab deployed via `meshRef` is indistinguishable, in
  `cn=config`, from one deployed the current way.

## Phase 5 — `charts/slapd-mesh`

- One chart containing `SlapdMesh`, `SlapdCluster`, `SlapdDatabase`s and
  `SlapdSchema`s, applied with identical values to every site.
- The only per-site input is the operator's own site identity, which lives in the
  *operator's* chart — not this one. If a per-site value appears in this chart's
  `values.yaml`, the design has been violated; treat it as a red flag, not a
  convenience.
- Done when: `helm template` with one values file produces byte-identical output
  for every site, verifiable with `sha256sum`.

## Phase 6 — Port `tests/e2e.sh`

**Replication-path change — needs a mesh cycle before merge.**

- New path behind a flag (e.g. `E2E_MESH=1`), defaulting off. Both paths live
  until Phase 7.
- What the new path deletes: `setup_slapd_clusters`' peer wiring loop, the
  `serverIDBase` arithmetic, `strip_seed_block` (already gone in Phase 1), and
  eventually `setup_cross_trust`.
- What it does **not** touch yet: trust bootstrap and shared credentials stay as
  they are. cert-manager and ESO adoption is out of scope here.
- Done when: a full-gate three-site run is green on `E2E_MESH=1` and produces the
  same 92/93 the current path does.

## Phase 7 — Flip the default; docs

- Default the e2e to the mesh path, delete the old path once one more full cycle
  is green.
- User docs: the invariant table from ADR-028 as user-facing documentation, plus
  a getting-started for a multi-site install.
- Move ADR-028 to Accepted, recording what was validated and what was not.

## Out of scope (separate work, deliberately)

- `slctl mesh verify` — independently useful and independently shippable; the
  hand-audit prototype is its executable spec.

  The prototype (~90 lines of shell, read-only, secrets compared by sha256 and
  never printed) was used to audit the lab on 2026-09-17: 21 checks over nine
  invariants, all green. It lands on the implementation branch as
  `hack/mesh-verify.sh` — a temporary artifact, deleted in the same commit that
  makes `slctl mesh verify` pass the same nine checks. It is not on `main`: it
  exists to be replaced, and an unowned shell script in a repo that is otherwise
  moving *away* from shell is exactly the debt this feature exists to remove.
- Continuous parity checking in the operator, and the peer-CR RBAC it needs.
- cert-manager / trust-manager and External Secrets adoption.
- Admission-time validation of the bundle (ADR-028 open question).

## Known risks

- Adding an env var to the operator Deployment rolls the operator; adding a field
  to the StatefulSet template rolls the slapd pods (ADR-013's accepted wart).
  Sequence Phase 3 and 4 so a single roll covers both.
- The `cn=config` pause freeze (2026-09-16 ledger entry) is unfixed. Phases 4 and
  6 write syncrepl stanzas, so a mesh cycle can surface it. A pod that stops
  serving while `Ready` is that entry, not a new defect — check `etime` before
  concluding otherwise.
- Multi-site `bootstrapFrom` is argued create-everywhere and never measured
  (ADR-028 open question). Do not let a chart-driven install be the first thing
  that tests it.
