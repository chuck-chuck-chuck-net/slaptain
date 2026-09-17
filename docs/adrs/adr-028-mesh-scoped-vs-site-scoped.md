# ADR-028: The mesh is a layer of its own — `SlapdMesh`, and what "mesh-scoped" means

**Status:** Proposed
**Date:** 2026-09-17

## Context

Every slaptain CRD is namespaced and describes one Kubernetes cluster.
`SlapdCluster`, `SlapdDatabase` and `SlapdSchema` say nothing about the other
sites and cannot: the sites are independent clusters sharing no API server,
which is the architecture (ADR-007, ADR-016).

A multi-site deployment is nevertheless one distributed system, and it works
only while N independent clusters agree on a set of facts. Nothing holds those
facts today. They live in `tests/e2e.sh` — a test harness — as an ordered
sequence of `kubectl` and `helm` calls, so standing up a mesh outside the lab
means reading 1500 lines of shell and reproducing what it does by hand.

The agreement is larger than it looks. From the current code:

| fact | rule across sites | enforced by |
|---|---|---|
| `SlapdDatabase` name and `suffix` | identical | nothing |
| `spec.replication.ridBase` | identical per database across sites | nothing |
| `spec.replication.ridBase` | unique between databases within a site | nothing (`docs/BACKLOG.md`) |
| `<db>-credentials` / `replication-password` | identical (ADR-008) | nothing; `e2e.sh` pre-creates the Secret before the CR exists |
| `SlapdSchema` set | identical, or a superset everywhere | nothing |
| `spec.replication.serverIDBase` | **differs** — one decade per site | nothing |
| `spec.seed` | exactly one site carries it | ADR-025's withhold belt, partially |
| CA cross-trust Secrets | N×(N−1) | nothing |
| `externalPeers` | N×(N−1) | nothing |

Eight invariants, no enforcement, and every violation fails silently rather than
loudly. Two of them have already cost us: the 19-day dead `db2` link was a
violated uniform-password invariant, and the ADR-025 glue suffix a violated
single-seed invariant.

Two failure modes deserve stating, because "the resource is simply missing at one
site" understates both:

- A missing `SlapdDatabase` is not an absence but a broken link. The other sites'
  stanzas name that suffix, the search returns `noSuchObject`, and because a
  failed logbase search halts delta-syncrepl outright (ADR-019, corrected
  Consequences), the link stops rather than degrades.
- A missing `SlapdSchema` diverges the DIT. Entries replicate in carrying an
  objectClass that site does not know, the add fails, and that site silently
  lacks entries the others have.

## Options considered

**A management cluster running a mesh controller.** Rejected. It reintroduces the
central control plane the architecture exists without, makes one site special,
and puts a new availability dependency in front of a system whose selling point
is per-site autonomy.

**One flat `SlapdMesh` carrying sites, databases and schemas.** Rejected once
several `SlapdCluster`s can share a set of sites: the site inventory would be
duplicated per cluster, and "whose databases?" has no coherent answer in a mesh
object that several clusters reference. The decomposition below removes the
duplication instead of relocating it.

**Pure GitOps with hand-maintained per-site overlays.** Rejected as the *whole*
answer, kept as part of it. Per-site Kustomize or Helm reconciles steady state
correctly, but it does not establish the pairwise trust bootstrap, and nothing in
that stack knows the eight invariants — a mistyped `serverIDBase` renders,
applies and reconciles cleanly.

**The operator fans out: create missing `SlapdDatabase`/`SlapdSchema` objects on
peer clusters.** Rejected, and recorded because it is the obvious idea. It writes
to state on a cluster the operator does not own (ADR-026 R2), it has no
single-writer rule so N operators race to create the same object, and it makes
every site's control plane a dependency of every other's. Fan-out belongs in the
GitOps layer, which already does it well.

**Promote `tests/e2e.sh` to a user-facing installer.** Rejected. A test fixture
carries test assumptions — throwaway CAs, hardcoded NodePorts, PVC wipes, seed
blocks stripped with `awk` — and promoting it means maintaining a shell program
against a contract it was never written to hold.

## Decision

### 1. Four layers, each referencing the one above

    SlapdMesh  ←meshRef—  SlapdCluster  ←clusterRef—  SlapdDatabase / SlapdSchema

`SlapdMesh` describes the sites and the fabric between them, and nothing else:

- `sites[]` — name, API endpoint for remote peer discovery, kubeconfig Secret name
- `network` — `pod-routed` | `multus`, plus the NAD reference
- trust — per-site CA Secret names, or a cert-manager issuer

Trust belongs here because trust is between *sites*: site B's CA is site B's CA
whichever LDAP cluster is talking to it. Per-cluster server certificates are
issued from it.

`SlapdCluster` gains `meshRef` and a `sites` selector (defaulting to every site
in the mesh), so a cluster may span a subset — cluster X over A+B, cluster Y over
A+B+C, one mesh. Without the selector, overlapping topologies would need
overlapping meshes, reintroducing the duplication this layering removes.

`SlapdDatabase` and `SlapdSchema` keep `clusterRef` unchanged and are thereby
transitively mesh-aware.

`SlapdMesh` is namespaced, like everything else: uniform RBAC, and two teams can
run independent meshes in one cluster.

### 2. "Mesh" is the noun for the sites; "full mesh" stays an adjective

The corpus already uses "mesh" two ways: as a noun for the multi-site deployment
("three-site mesh", "mesh-wide" — the dominant use, and what ADR-025 and ADR-027
mean throughout), and inside `docs/REPLICATION.md` as a noun for the in-cluster
pod topology ("the RW mesh", "a three-pod mesh"). The industry term for the first
is the one we adopt — Cilium's ClusterMesh names exactly this — so the CR is
`SlapdMesh` and the noun is reserved for it.

The adjective survives untouched: "full mesh" is standard topology vocabulary and
"This is a full mesh: 6 persistent connections for 3 nodes" stays as written. We
only stop using bare "mesh" as a *noun* for the in-cluster topology — "the RW
mesh" becomes "the RW pods", "no in-cluster mesh" becomes "no in-cluster syncrepl
stanzas". A handful of phrases, worth fixing before the CRD name lands, because
afterwards every stale use reads as a reference to the object.

### 3. A mesh-scoped resource carries no per-site fields

This is the rule that makes parity achievable rather than merely desirable. A
mesh-scoped CR must be byte-identical at every site; therefore it may not contain
a field whose value differs per site, and any per-site behaviour must be
expressed by *naming* a site rather than by editing the object.

`spec.seed` is the only current violation. Today it is present at the founder and
stripped everywhere else — not a per-site value but a per-site *edit*, performed
by `strip_seed_block`'s `awk` in `tests/e2e.sh`, which is the clearest possible
evidence that the field is modelled wrong. It becomes a declaration, `seed.site:
site-a`, read identically everywhere, with each operator comparing that name
against its own identity. ADR-025 stops being a deployment procedure and becomes
a property of the spec.

`bootstrapFrom` is *not* a violation. Restore is create-everywhere by design: the
slapadd-all-pods machine loads every RW pod directly rather than letting syncrepl
carry the data (ADR-014, amendment 2026-06-09), and the same reasoning extends
across sites. `slapcat` emits `entryUUID` and `entryCSN` and `slapadd` preserves
them, so N sites restoring one artifact produce identical entries rather than the
independent creations that produce an ADR-025 glue suffix. Argued, not measured —
multi-site `bootstrapFrom` has never been run.

→ Identical-across-sites becomes checkable as existence plus a hash, with no
field-by-field comparison to get wrong.

### 4. The operator derives the wiring; one per-site fact lives outside all CRs

From `meshRef` plus its own identity the operator derives `serverIDBase` (from the
site's index), `externalPeers` (from `sites[]`), the network mode and the trust
wiring. All four drop out of the user-facing spec.

Two `SlapdCluster`s on one mesh may safely share `serverID` values: a CSN is
compared only within one replication topology, and clusters with different
suffixes never exchange CSNs. That stops holding if two such clusters are ever
merged, and a decade-per-site scheme tops out near 40 sites against
`olcServerID`'s 4095.

The operator must still answer "which site am I?". That fact belongs in the
operator's own installation config, not in any CR — putting it in `SlapdMesh`
would destroy the byte-identical property the whole design rests on. It is then
the single per-site fact in the system, and getting it wrong at two sites
collides their `serverID` decades, so it is the first thing `verify` asserts.

### 5. The packaging unit is a chart, not a bigger CR

"One thing to apply" is a packaging property, not a data-model property. We ship
`charts/slapd-mesh` containing all four kinds, applied with identical values to
every site — one chart, or one HelmRelease per site pointing at the same chart
and values. The layered model stays clean and the single-artifact convenience is
kept.

We accept what this gives up against a single fused CR: one object would be
atomic and validatable in the API, so an incoherent bundle could be refused at
admission and coherence would be a property of etcd. With a chart, coherence is
established at apply time and verified afterwards.

### 6. Parity checking: a continuous signal and an out-of-band prober

The operator owns the standing signal. Given the mesh's site list and peer
credentials it lists peer `SlapdDatabase`/`SlapdSchema` objects and surfaces a
condition — a peer missing a database, a divergent `ridBase`. `slctl mesh verify`
owns diagnosis, reading N contexts from wherever it runs, which is what you want
precisely when the operator cannot reach its peers.

Both read the same predicate, following `internal/suffixprobe` (one classifier,
several call sites) rather than growing a second copy of the comparison.

Every invariant in the table above is decidable from the Kubernetes API alone —
confirmed by running all nine against the three-site lab on 2026-09-17, where no
check needed an LDAP bind. `mesh verify` therefore works while replication is
broken, which is when it is worth having. It also draws the line against existing
tooling: `mesh verify` compares *specs* across sites over the k8s API, `slctl
inspect` compares *runtime state* within a site over LDAP. Neither subsumes the
other.

Both are read-only, permanently. A mesh has no single writer by construction, so
a repair write is exactly the corrective action on unowned state that ADR-026 R2
forbids. Report and stall.

Peer API access exists only in ADR-007 discovery mode; in `uri` mode the operator
has no remote credentials and must report parity as **unknown** rather than as
passing. Unreadable evidence never counts toward a pass — the same rule ADR-008's
CSN judging already follows.

### 7. Imperative bootstrap, declarative steady state

Cross-cluster trust is pairwise and circular — site A needs site B's CA before
site B exists — and no declarative system establishes it without a management
cluster. Every comparable project reaches the same conclusion: Cilium ClusterMesh
connects sites with a CLI and requires a unique cluster ID per cluster, Istio and
Submariner do the equivalent. (Recalled, not cited — check before quoting.)

We adopt the split rather than fighting it. Bootstrap — trust, CAs, shared
credentials, operator install — is a CLI run once per site pair. Steady state is
the chart above, applied by whatever the user already runs.

For the two bootstrap problems we adopt the ecosystem rather than growing our
own: cert-manager and trust-manager for the CA and its distribution, External
Secrets or documented SOPS for the shared `replication-password`. ADR-027 already
made the Secret the source of truth and made rotation work, so the operator side
is ready. This deletes `gencert.sh` and `setup_cross_trust` rather than porting
them, and buys CA rotation, which today's throwaway self-signed certificates do
not have.

## Consequences

Of the eight invariants, four stop existing — `serverIDBase`, `externalPeers`,
network mode and trust wiring are derived. The other four survive as properties
of an artifact: identical `SlapdDatabase`, `SlapdSchema`, `seed.site` and
credential references, applied everywhere by one chart and checked by a hash.
Construction for the first four, detection for the rest, and the difference is
real: a derived `serverIDBase` cannot be wrong, whereas a site that never
received the chart is *detectable* rather than impossible.

The mesh becomes documentable. A user gets one chart, one values file and a
bootstrap command instead of a test script, and the invariant table becomes user
documentation rather than tribal knowledge.

`verify` is load-bearing rather than a convenience, since it is the only
mechanism covering the four surviving invariants. Both silent-divergence bugs in
this project's history would have been caught in seconds by a check that already
knew what to compare.

`slctl` grows a multi-context mode. It is single-context today, so credential
resolution, context iteration and partial-failure reporting are new design, and a
site that cannot be reached must report as unknown rather than folding into a
pass.

Mesh changes need skew tolerance. Adding a site means editing the mesh everywhere,
and during the rollout sites run different generations — site A already knows a
database that site B does not. Mesh spec changes must therefore be additive and
tolerant, the same requirement ADR-027's one-directional migration window
imposed. This is a design constraint on every future mesh field, not an
implementation detail.

Blast radius grows. One bad mesh definition applied everywhere hits every site at
once, where per-site manifests would localise the mistake. More visible, and more
dangerous.

Continuous parity checking needs RBAC we do not have: the operator reads CRs on
peer clusters, so `scripts/create-remote-kubeconfig.sh` must grant CR reads
alongside pod discovery.

The e2e suite is rewritten onto the tooling, which is the proof the tooling is
real. It is also the project's most load-bearing asset, so we port it
incrementally behind a flag — not as a big-bang swap, and not immediately before
a deadline that depends on it.

Nothing here is implemented. The invariant table was derived from reading the
code and then **audited by hand against the three-site lab on 2026-09-17**: 21
checks over nine invariants, all green, so the lab holds every invariant today
and none of them is hypothetical. The audit corrected the table once — `ridBase`
is two invariants, not one — and established that the whole set is decidable from
the Kubernetes API. The prototype is ~90 lines of shell, which bounds what the Go
implementation has to do.

What the audit does not cover: it reads specs, so it would not catch a spec that
is correct everywhere while a pod's `cn=config` has drifted from it. That is
`slctl inspect`'s half, and neither tool is a substitute for the other.

## Open questions

- Multi-site `bootstrapFrom` is argued create-everywhere from `entryUUID`
  preservation and never measured. Worth a deliberate test before anyone relies
  on it.
- Whether `SlapdMesh` should eventually validate the bundle at admission — the
  one property §5 knowingly gives up — or whether `verify` covers it adequately.
- Whether the operator's parity condition belongs on `SlapdCluster` or on each
  `SlapdDatabase`. The latter is more precise and noisier.

## Related

- ADR-004: multi-resource CRD architecture — the layering this extends upward
- ADR-007, ADR-016: cross-site transports — the trust this bootstraps, and the discovery credentials parity checking reuses
- ADR-008: uniform replication password; unreadable evidence never counts toward a pass
- ADR-014: slapadd-all-pods restore — why `bootstrapFrom` is create-everywhere
- ADR-019: a failed logbase search halts replication — why a missing database is a broken link
- ADR-025: single-creator seed, mesh-wide — becomes `seed.site`
- ADR-026: R2, no corrective action on unowned state — why parity checking is read-only
- ADR-027: the Secret is the source of truth — what makes externally-managed secrets viable
