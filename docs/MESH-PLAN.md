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
3. Phases are sequential by dependency. Phases 1 and 2 need no new CRD and can
   land alone, ahead of the mesh type.

## Two hazards to read before writing any code

**1. `seed.site` is a prerequisite, not a follow-up.** The feature's whole point
is one chart applied identically to every site. Apply today's `SlapdDatabase` —
which carries `spec.seed` — identically to N sites and every site seeds, which is
the ADR-025 multi-site seed race: one pod ends up with a permanent hidden glue
suffix, invisible to ordinary searches and clean on every CSN health read. The
operator's withhold belt is a belt, and ADR-025 says explicitly that the fixture
must not rely on winning that race. So `seed.site` (Phase 2) is not optional, and
it sits behind operator self-identity (Phase 1) because it decides by comparing
against it.

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

## Phase 1 — Operator self-identity

Ordering correction, 2026-09-17: this was Phase 3 in the first draft, behind
`seed.site`. That was wrong — `seed.site` decides by comparing a declared site
name against *the operator's own identity*, so identity has to exist first.
Caught while writing the implementation brief, before any code was written.

- The operator learns its own site name from its installation config (Helm value
  → env var on the Deployment, alongside the existing `OPERATOR_IMAGE` /
  `OPERATOR_IMAGE_TAG` precedent). It does **not** come from any CR — that is what
  keeps every mesh-scoped object byte-identical.
- Unset is legal and means "no mesh features". Nothing consumes the identity yet
  in this phase; it is plumbing plus a pure resolver.
- No `SlapdMesh` type exists yet, so the resolver validates only what it can:
  present/absent and well-formed. Cross-checking the name against `sites[]`
  arrives with the type in Phase 3.
- Tests: unit red-first on the pure resolver (present, absent, empty string,
  whitespace).
- Done when: `helm upgrade` can set it, the operator reports it, and nothing else
  has changed.

*Amended during implementation, 2026-09-17:* this phase originally also asked for
"a check that the operator logs its identity once at startup". Dropped. `cmd` has
no test harness in this repo (0.0% coverage), so asserting a log line means either
building one or extracting the message choice into a pure function that exists
only to be asserted — machinery out of proportion to one log statement. The
identity's real consumers get tested where they consume it, from Phase 2 onward.
The startup log is verified by reading, and is stated here as untested rather than
quietly counted as covered.

## Phase 2 — `seed.site` (unblocks the byte-identical chart)

- Add a site selector to the seed spec on `SlapdDatabase`. Semantics: the seed is
  applied only where the operator's own site identity (Phase 1) matches; every
  other site withholds and receives the DIT by replication.
- Unset keeps today's behaviour, so single-site deployments and the existing
  fixture are untouched.
- **Decided 2026-09-17: withhold.** A `seed.site` naming some site while this
  operator has no identity configured withholds the seed. A database that never
  seeds is loud and recoverable; a glue suffix is silent and permanent (ADR-025),
  so the safe direction is the one that may do nothing. Unreadable identity never
  counts as a match — the same rule ADR-008 applies to CSN evidence.
- Interaction to preserve: ADR-025's withhold belt (a foreign suffix creator
  suppresses the seed) stays as the belt. This is the braces.

**Split into 2a and 2b, decided 2026-09-17.** The operator side lands and is
proven on a single-site lab before the e2e fixture changes, so that a failure in
the fixture rewrite cannot be confused with a failure in the seed decision.

*Phase 2a — operator side.* The API field, the pure decision
(`shouldSeed(selector, identity)`), the controller wiring, unit tests red-first
covering unset selector, match, mismatch and unknown identity. Validated live on
a single-site lab: a matching `seed.site` seeds, a mismatching one withholds,
and an unset one behaves exactly as today. `tests/e2e.sh` is NOT touched.

*Phase 2b — fixture.* Delete `strip_seed_block` and its call site, move the
fixtures to `seed.site`, and prove it with a multi-site cycle. Only after 2a is
merged and green.

- Done when (2a): the three cases are observed on a live single-site cluster, not
  only in unit tests. Done when (2b): `strip_seed_block` is gone and a three-site
  run is green.

## Phase 3 — `SlapdMesh` types + the derivation seam (no behaviour change)

- Scaffolding first: `kubebuilder create api` for `SlapdMesh` (namespaced, group
  `ldap.chuck-chuck-chuck.net`, v1alpha1), RBAC for the new kind, regenerated
  `config/rbac/role.yaml`, CRD synced into `charts/operator/crds/`.
- Types: `sites[]` (name, endpoint, kubeconfig secret name), `network`
  (`pod-routed` | `multus` + NAD), trust (CA secret names / issuer ref).
- Pure functions, no client, no I/O:
  - `serverIDBaseForSite(mesh, siteName) (int32, error)` — **must** reproduce
    site-index × 100; see hazard 2.
  - `externalPeersForSite(mesh, cluster, siteName) []ExternalPeer` — honours the
    `sites` selector on `SlapdCluster`, excludes self, produces exactly the peer
    set `e2e.sh` produces today.
  - `validateSelfSite(identity, mesh) error` — extends Phase 1's resolver now
    that `sites[]` exists: a configured identity absent from the mesh is a loud
    error, never a silent default to the first site.
- Tests: table-driven unit tests red-first, including the lab's three-site values
  as a golden case, subset selectors, a single-site mesh, and a self-site not
  present in the mesh.
- Done when: the functions exist and are tested; nothing calls them yet.

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

*Corrected during Phase 3, 2026-09-17 — read this before writing the e2e.* The
control to compare against is **not** today's fixture output. `peer.Name` is
baked into `cn=config`: the CA is mounted at
`/etc/openldap/tls/peers/<name>/ca.crt` (`slapdcluster_controller.go:1049`) and
that path is written into the stanza's `tls_cacert`
(`slapddatabase_controller.go:2405`). Derived peers are named after **mesh
sites**, while `tests/e2e.sh` names them after kube contexts — and the lab's
context names cannot become mesh names, because the fixtures are public and the
References policy forbids them (which is why Phase 2b chose `site-N`). So
byte-identity against the current lab output is unreachable by construction.
Compare instead against a hand-configured control whose peer names are the mesh
site names; equivalence then holds exactly. What Phase 6 does to a running lab is
therefore a peer **rename** — a stanza rewrite and a volume remount, i.e. a
cluster roll. Not a data event, but not invisible either: plan it as a roll.

- Done when: a three-site lab deployed via `meshRef` is indistinguishable, in
  `cn=config`, from one deployed with hand-written peers **using the same site
  names**.

*Landed 2026-09-17.* Equivalence shown at `cn=config` level on a single-site lab
(derived vs hand-written: identical `olcServerID` and `olcSyncRepl`, modulo the
per-installation pod IP and the per-database password). The existing path is
unchanged and proven by a full three-site cycle with no `meshRef` anywhere.
**Multi-site `meshRef` is not yet exercised end-to-end** — nothing deploys a
mesh-driven cluster across sites until Phase 6.

Two findings from the implementation, both structural:

- The derivation is applied **in memory only** and never written back to the
  spec. Materialising it would make each site's `SlapdCluster` differ from its
  neighbours', destroying the byte-identical property §3 rests on. The cost is
  that every consumer of the derived fields must resolve `meshRef` itself.
- Consequently the `SlapdDatabase` controller resolves too: it fetches the
  cluster independently and builds the external stanzas and the per-pod
  `olcServerID` from `sc.Spec.Replication`. Without that call site, `meshRef`
  would produce CA mounts and a serverID base but **zero syncrepl stanzas**.

### Phase 6 prerequisite — the diagnostics must stop lying first

`slctl status`, `slctl inspect` and `internal/controller/backup_source.go` read
`Spec.Replication.ExternalPeers` directly, without resolving `meshRef` — eight
call sites. On a mesh-driven cluster they report **no external peers** while
`cn=config` carries the stanzas. Harmless while nothing sets `meshRef`;
unacceptable the moment the lab moves, because `slctl inspect` is the tool
reached for when a mesh misbehaves. Fix these before Phase 6, not after.

*Done 2026-09-17 as Phase 6a.* The fix is **not** to re-derive in `slctl`. That
would make the CLI a second authority on a derivation whose output is baked into
replicated data, free to disagree with the operator you are using it to debug —
and it is not even sufficient, because a peer's discovered addresses come from
the operator's live queries to remote API servers and no local derivation
produces them. `slctl` reads back `status.externalPeerStatuses` (which the
operator writes *after* applying the wiring) and reports `MeshResolved` as the
provenance. No `SITE_NAME` in the CLI, no new flag, no new RBAC, and agreement
with the operator by construction; the cost is freshness, bounded by the
operator's last status write and stated in the output.

Where the identity cannot be determined, the output says so — peers labelled
`LAST KNOWN` or `UNKNOWN, not empty`, and `inspect` fails a new
`mesh-resolution` check. An empty list that means "I could not tell" was the
defect being fixed; reintroducing it one layer up would have been worse.

Following the same thread found a defect worse than the display bug:
`backup_job.go`'s `mountsWithAccesslog` calls `NeedsAccesslogVolume()` — which
reads `Replicas > 1 || len(ExternalPeers) > 0` — on an **unresolved** cluster. On
a single-replica mesh member the cluster controller resolves and mounts
`/accesslog`, while the backup Job would not, and `slapcat` validates every
`olcDbDirectory` in `cn=config` including the accesslog DB's. That is a failing
backup, not a cosmetic one. Both it and the `SourceConverged` heuristic are
repaired by resolving in the backup reconciler. An audit of every other
`SlapdCluster` fetch found no further unresolved readers.

## Phase 5 — `charts/slapd-mesh`

- One chart containing `SlapdMesh`, `SlapdCluster`, `SlapdDatabase`s and
  `SlapdSchema`s, applied with identical values to every site.
- The only per-site input is the operator's own site identity, which lives in the
  *operator's* chart — not this one. If a per-site value appears in this chart's
  `values.yaml`, the design has been violated; treat it as a red flag, not a
  convenience.
- Done when: `helm template` with one values file produces byte-identical output
  for every site, verifiable with `sha256sum`.

*Landed 2026-09-17.* Identical render confirmed across three release names and
three namespaces (one `sha256`), reproduced independently by review. The chart
deliberately does **not** mirror the sibling's `.Release.Name`-derived `fullname`
helper — object names come from the values — and the only release value that
reaches a rendered object is `.Release.Service`, which Helm always sets to the
literal `Helm`. No Secret, no `randAlphaNum`, nothing per-install.

Eight render-time guards refuse what the operator would otherwise accept and
regret: duplicate `serverIDIndex`, duplicate `ridBase`, duplicate site name,
missing index, a selector naming an unknown site, a `seed.site` naming an unknown
site, a database without a suffix, and — the one that matters most — a seeded
database with no `seed.site` **once a second site is declared**. That last makes
the ADR-025 race unreachable by packaging rather than merely documented.

Three of those guards were themselves defective on first write and were fixed
only because each was driven to red: `has` compares with `reflect.DeepEqual`, so
an `int64` from `--set` never matched an `int` from `values.yaml` and duplicates
passed silently; and `hasKey` returns true for an explicitly null key. A
duplicate-detector that silently misses duplicates is the exact failure the chart
exists to prevent, so the red was load-bearing, not ceremony.

## Phase 6 — Port `tests/e2e.sh`

**Replication-path change — needs a mesh cycle before merge.**

- New path behind a flag (e.g. `E2E_MESH=1`), defaulting off. Both paths live
  until Phase 7.
- What the new path deletes: `setup_slapd_clusters`' peer wiring loop, the
  `serverIDBase` arithmetic, `strip_seed_block` (already gone in Phase 2), and
  eventually `setup_cross_trust`.
- What it does **not** touch yet: trust bootstrap and shared credentials stay as
  they are. cert-manager and ESO adoption is out of scope here.
- Done when: a full-gate three-site run is green on `E2E_MESH=1` and produces the
  same result the current path does.

*Landed 2026-09-18.* **Multi-site `meshRef` is proven end-to-end** — the claim
every phase since 3 deferred. `MeshResolved=True` at all three sites, the
`SlapdCluster` byte-identical everywhere with nothing per-site in it, decades
reproducing the hand-wired scheme exactly (`olcServerID` 1 / 101 / 201), stanzas
naming logical site names, and `slctl inspect --short` 17/17 at every site.

    mesh path     87 of 93, 0 failed
    default path  88 of 93, 0 failed

The one-spec difference is the single deliberate skip below. **Baseline
correction:** with this gate set the comparable number is 88/93, not the 92/93
this plan originally quoted — 92 required `E2E_SCALE=1` as well.

The port exposed a defect of exactly the Phase 6a class, one layer further out:
four places in `tests/e2e/*.go` read `spec.replication.externalPeers` directly.
Two failed, one silently skipped, and one **passed vacuously** — with an empty
list every assertion block was skipped. The vacuous pass is the worst of the
four and the best argument for the exercise: a suite that passes by asserting
nothing is worse than one that fails. All four now go through one shared reader
that returns the *derived* set and, crucially, **fails the spec rather than
returning empty when the mesh has not resolved** — treating unknown as empty was
the original bug and would have been reintroduced one layer up.

One deliberate coverage difference, documented in code and in `tests/README.md`:
the peer-*removal* spec is skipped on the mesh path. Its premise — patch peers
out of the spec — is inexpressible on a mesh cluster, where peers are derived and
writing them is refused by design.

### Follow-ups this phase opened

- **Re-express peer removal against the mesh**: drop a site from the `SlapdMesh`
  and assert the stanza disappears. Better than the spec it replaces, because it
  also exercises the operator's watch on the mesh object. Until then the
  hand-wired path is the only coverage of peer removal.
- **`E2E_ACCESSLOG_MIGRATION=1` is a no-op** — no such gate exists anywhere in
  the suite (presumably retired with ADR-019 R8), yet it has been carried in the
  documented invocation line. Drop it, or implement what it promises.
- Untested on the mesh path: the `multus` branch of `build_mesh_values`,
  `imagePullSecrets` in the generated values, and `TEST_RESOURCES=lab`.

## Phase 7 — Flip the default; delete the old path; docs

- Default the e2e to the mesh path, delete the old path once one more full cycle
  is green.
- User docs: the invariant table from ADR-028 as user-facing documentation, plus
  a getting-started for a multi-site install.
- Move ADR-028 to Accepted, recording what was validated and what was not.

*Landed 2026-09-18.* There is one deployment path and no flag selects it.
`E2E_MESH` is gone, and with it `setup_slapd_clusters`' peer-wiring loop, the
`site_idx * 100` arithmetic, the dual context-vs-site naming helpers
(`peer_name` in full; `peer_ca_secret` and `peer_kubeconfig_secret` survive as
one-line statements of the operator's convention), `discover_multus_ips`,
`configure_multus_external_peers*` and the `STATIC_PODADDRESSES` knob.
`tests/e2e.sh` goes +170/−371.

    single-site  one context, plain `all`                       65 of 93, 0 failed
    multi-site   three sites, E2E_RESILIENCE/SCALEUP/BACKUP=1   88 of 93, 0 failed

88/93 is the **default-path** number Phase 6 measured, not the 87/93 the mesh
path scored there: re-expressing peer removal closed the one-spec gap, so the
mesh path now matches the path it replaced spec for spec. The single-site run
is a one-site mesh (index 0 → decade 0) and behaves exactly as a standalone
cluster always has, teardown included.

**Peer removal was re-expressed against the mesh FIRST**, because deleting the
old path would otherwise have deleted the only coverage of it. The new spec
(`mesh-peer-removal`) drops the last site from the `SlapdMesh` and asserts that
exactly that peer's stanza leaves every RW pod while the survivors' stanzas
stay, then restores the site and waits for the stanza to return. It covers more
than the spec it replaces: the whole derivation path runs, and it is the only
thing in the suite that exercises the operator's **watch on the SlapdMesh** —
the mechanism that makes a mesh edit reach `cn=config` before the five-minute
`SlapdDatabase` resync floor.

The behaviour already existed, so the spec could not go red honestly. Two
mutation checks gave it teeth instead, each observed failing for its own reason
on the live three-site lab:

- *the mesh edit is not applied* → `slapd-0 still carries the dropped peer's
  stanza (rid=152)`;
- *every peer site is dropped, not just the last* → `slapd-0 lost surviving peer
  "site-2" (rid=151) as well — the derivation collapsed rather than dropping one
  site`.

Unmutated it passes in 45 s standalone and 20 s inside the full suite.
Dropping the **last** peer is load-bearing: external
peer RIDs are positional (`ridBase+50+j+1`), so removing one in the middle
renumbers the peers after it and a RID assertion would be reading a live peer's
number. Removing a peer also changes the pod template (its CA mount), so the
`SlapdCluster` rolls — twice over the spec — which the budgets allow for.

### What the deletion cost

Two cross-site transports lost their e2e coverage, and neither has a `SlapdMesh`
expression to gain it back in: **NodePort `uri` peers** and **static Multus
`podAddresses`**. A mesh describes sites and reaches them through their API
servers, so it derives discovery peers and nothing else — those two are
hand-configured shapes, still supported by the operator and still reachable
through `spec.replication.externalPeers` on a mesh-less `SlapdCluster`, but
nothing exercises them any more. A multi-site `e2e.sh` run now refuses to start
without `POD_ROUTED` or `MULTUS_NETWORK` rather than producing a peerless
cluster. Recorded here rather than quietly absorbed; `tests/README.md`'s
transport table names both as uncovered.

### Follow-ups from Phase 6, resolved

- **Re-express peer removal against the mesh** — done, above.
- **`E2E_ACCESSLOG_MIGRATION=1` is a no-op** — closed by inspection. No such
  gate exists in `tests/` and no committed invocation line carries it; the
  remaining mentions are in `docs/reconcile-loop-fixes.md`, which is a
  historical log of a gate that was retired with ADR-019 R8. Nothing to delete.
- **Untested on the mesh path** — unchanged and still true: the `multus` branch
  of `build_mesh_values`, `imagePullSecrets` in the generated values (exercised
  incidentally by any lab with a pull secret, not asserted), and
  `TEST_RESOURCES=lab`.
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
  Sequence Phases 1, 3 and 4 so a single roll covers the operator env var and the
  StatefulSet template change.
- The `cn=config` pause freeze (2026-09-16 ledger entry) is unfixed. Phases 4 and
  6 write syncrepl stanzas, so a mesh cycle can surface it. A pod that stops
  serving while `Ready` is that entry, not a new defect — check `etime` before
  concluding otherwise.
- Multi-site `bootstrapFrom` is argued create-everywhere and never measured
  (ADR-028 open question). Do not let a chart-driven install be the first thing
  that tests it.
