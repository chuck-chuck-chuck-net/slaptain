# ADR-011: Hot Migration Topology Contract

**Status:** Accepted
**Date:** 2026-05-04
**Accepted:** 2026-05-13 — plain-syncrepl interop + per-pod ServerID coordination implemented; e2e-verified via `tests/e2e-migration.sh`. Runbook side (stage transitions, source-side decommissioning) tracked under `MIGRATION-PLAN.md §2.5` and `MIGRATION-HOT.md`.

## Context

The hot-migration goal (`docs/MIGRATION-PLAN.md` §2) is to retire an existing
OpenLDAP cluster by adding slaptain as a peer, rolling traffic over gradually, and
draining the old nodes one by one. Zero maintenance window.

This requires slaptain to coexist with the source cluster's existing replication
topology long enough to migrate. Coexistence is not a runtime concern alone — it is an
**interoperation contract** that constrains slaptain's identifiers, syncrepl wire
format, bind credentials, and stage-transition semantics.

This ADR codifies that contract so the implementation, the runbook
(`docs/MIGRATION-HOT.md`), and the source-side configuration changes have a single
source of truth.

## Scope and the generality principle

We hot-migrate from any source that speaks standard OpenLDAP syncrepl — plain or
delta. Because delta-syncrepl is, on the provider side, a superset of plain syncrepl, a
delta source serves both delta and plain consumers — so a delta source is no harder a
case than a plain one. slaptain consumes either with the matching `syncMode`: `delta`
by default, `plain` against a plain-only source (see §"Delta vs. plain syncrepl").
Beyond that we rely only on:

- OpenLDAP's own multi-master rules — a `syncprov` provider, CSN/ServerID conflict
  resolution, per-server `contextCSN`;
- the source reachable over `ldap`/`ldaps`, offering a bind identity with read access
  to the replicated data.

We do not depend on the source's node count, site layout, or identifier numbering —
anything expressible in vanilla syncrepl migrates.

Cold migration — a maintenance-window `slapcat` → `slapadd` — is always available and
asks nothing of the source's live topology. Hot migration is the optimization that
removes the maintenance window; it adds a few requirements of its own on top of
"speaks syncrepl":

- ServerID coordination. ServerIDs are global to a topology (embedded in every CSN,
  tracked in `contextCSN`); slaptain's must not overlap the source's, or writes from
  one silently vanish into the other's bucket. slaptain shifts its range with
  `serverIDBase` and declares the source's via `foreignServerIDs`. (RIDs are
  consumer-local — never on the wire — so they need no coordination; the source's RIDs
  are irrelevant.)
- The source must be reconfigurable to add slaptain as a peer (Stage 2). A frozen
  source config leaves only cold migration.
- Reachability in both directions. syncrepl connections are consumer-initiated, and in
  multi-master each side consumes from the other — so at Stage 2 slaptain dials the
  source *and* the source dials slaptain, and each must be able to open connections to
  the other. (Consumer-only Stage 1 needs only slaptain → source.)
- Phase 1 prerequisites on slaptain before the initial refresh: compatible schema,
  ACLs, a syncrepl bind identity.

→ A source that cannot meet these either has the process extended for its case, or
falls back to cold migration.

## Delta vs. plain syncrepl — the source can be either

slaptain replicates *internally* with **delta-syncrepl**: each pod runs an `accesslog`
overlay on the data DB and `syncprov` on both the data DB and the accesslog DB, and
in-cluster consumers pull a compact change journal (`syncdata=accesslog`). Delta is an
optimization layered on the ordinary syncprov mechanism — it changes *what a consumer
pulls*, not the underlying CSN/cookie semantics.

Two consequences matter for migration, and they are asymmetric:

- **A delta provider is also a valid plain provider.** Because every slaptain pod still
  runs `syncprov` on the *data* DB (needed for the initial full refresh), an ordinary
  plain consumer can bind and pull full entries from it, ignoring the accesslog. So a
  source node can consume *from* slaptain with a vanilla plain-syncrepl stanza — the
  source needs no delta support.
- **A plain provider is not a delta provider.** A source running plain syncrepl exposes
  no accesslog for slaptain to pull deltas from. So when slaptain consumes *from* the
  source, its `ExternalPeer` stanza must be plain (`syncMode: plain`) and pull full
  entries via the source's data-DB `syncprov`.

## Assumed source

Kept deliberately general — a representative source is simply:

- multiple nodes, possibly spread across multiple sites;
- each node with a ServerID unique within the source topology (OpenLDAP requires this
  for multi-master);
- replication via plain (or delta) syncrepl over `ldap`/`ldaps`;
- a syncrepl bind identity — e.g. `cn=syncuser,ou=config,<suffix>` — with read access;
- cross-site links typically `ldaps://` (often with permissive peer-cert checking such
  as `tls_reqcert=allow`); same-site links plain `ldap://` on a trusted network.

## What slaptain provides

- N pods per cluster; each pod's ServerID is `serverIDBase + ordinal + 1`
  (default `serverIDBase: 0`, so IDs `1..N`).
- **Delta syncrepl internally.**
- Bind DN `cn=replication,<suffix>` for in-cluster peers; `ExternalPeer.bindDN` is
  fully customizable for external peers.
- Cross-site `ldaps://` with strict CA validation per `ExternalPeer.tlsSecretName`.

## The migration interoperation contract

The concrete capabilities slaptain implements to meet the §"Scope and the generality
principle" requirements; the CRD fields behind them live in §Decisions.

1. **Consume from the source.** One `ExternalPeer` per data DB, `syncMode: plain`
   against a plain source (§"Plain-syncrepl mode on ExternalPeer"), with `bindDN` set to
   the source's syncrepl identity rather than slaptain's default
   `cn=replication,<suffix>`. slaptain trusts the source's CA via a cross-trust Secret;
   a permissive `tls_reqcert` on the source also spares slaptain from publishing its own
   CA to the source.
2. **Provide to the source.** No extra work: slaptain already runs `syncprov` on the
   data DB, so a source node consumes from it with a plain stanza. The source binds in
   as its own syncrepl identity, which must exist on slaptain as a directory entry with
   read access — created via SlapdUser (ADR-009) in Phase 1.
3. **Avoid ServerID collisions.** `serverIDBase` shifts slaptain's range clear of the
   source and `foreignServerIDs` declares the source's — see §"Foreign ServerID
   declaration".

## Decisions

### Plain-syncrepl mode on ExternalPeer

`SlapdCluster.spec.replication.externalPeers[].syncMode: delta|plain`, default `delta`.

- **`plain`** — generated stanza omits `syncdata=accesslog`, `logbase=...`, and other
  delta-specific options. Bind, TLS, retry, keepalive are unchanged.
- **`delta`** — current behavior (`syncdata=accesslog`, `logbase=cn=accesslog`).

A given external peer is either delta or plain. If the source ever adds delta-syncrepl,
flip that peer's `syncMode` to match.

### Foreign ServerID declaration

`SlapdCluster.spec.replication.serverIDBase: int` (default 0) — slaptain pods get
ServerIDs `serverIDBase+1 .. serverIDBase+N`.

`SlapdCluster.spec.replication.foreignServerIDs: []int` declares the ServerIDs already
in use on external peers; validation rejects a configuration whose slaptain range
overlaps them.

```yaml
spec:
  replicas: 4
  replication:
    enabled: true
    serverIDBase: 500                 # slaptain pods get 501..504
    foreignServerIDs: [ ... ]         # the ServerIDs the source already uses
```

### Stage transitions

Each stage is a stable cluster configuration. Transitions are explicit user actions (CR
edits or runbook steps); the operator does not auto-progress. Each transition documents
its reversibility.

#### Stage 0 — source alone

The source runs unchanged. Slaptain not present.

#### Stage 1 — slaptain consumer-only

- Slaptain provisioned, `mode: consumer-only` (ADR-010). `replicas: 1` initially to
  bound initial-refresh WAN traffic; scale up after refresh.
- One `ExternalPeer` per data DB pointing at the source's providers (`syncMode: plain`,
  `bindDN: cn=syncuser,ou=config,<suffix>`, `bindPasswordSecretName: <source-syncuser>`,
  `tlsSecretName: <source-ca>`).
- `serverIDBase` shifted clear of the source; `foreignServerIDs` declared.
- Schemas (Phase 1.1), ACLs (Phase 1.3), and service users (Phase 1.4) already in place
  on slaptain — needed before the initial refresh runs cleanly.

**Verification:** entry counts per DB match the source after initial refresh; CSN
convergence reaches steady state; lag stable < 5s.

**Reversibility:** total. Tear down slaptain; the source is unaffected (no source node
consumes from slaptain yet).

#### Stage 2 — peer mode + source-side reciprocal wiring

- Edit slaptain CR: `replication.mode: peer`. Operator performs in-place promotion
  (ADR-010). Status condition `ReplicationModePeerWiringRequired` surfaces.
- **Source-side configuration changes** (out of slaptain's scope):
  - Add slaptain's ServerIDs to the source's `olcServerID` list on every source node.
  - Add reciprocal syncrepl stanzas pointing at slaptain pods (one per slaptain pod,
    per data DB), using the source's syncrepl bind, plain syncrepl.
  - Roll source nodes one at a time to pick up the new config.

**Verification:** bidirectional replication active; lag stable < 5s in both directions
for ≥48 hours; no `unwillingToPerform` from slaptain (the cluster is now RW); no CSN
divergence.

**Reversibility:** demotion is possible (ADR-010 §"In-place demotion") provided the
source's reciprocal stanzas are dropped first. After bidirectional traffic, slaptain
may hold writes the source has not replicated yet; the demotion itself is safe (slaptain
stops accepting writes), but a teardown after demotion would lose those writes.
**Rollback at this stage requires a freeze window** to `slapcat` any slaptain-only
writes back to the source.

#### Stage 3 — gradual client cutover

- Flip clients one at a time to point at slaptain's Service instead of the source's LB.
- Wait a per-client soak window (24h recommended) before flipping the next.
- Lag metrics (Phase 0.4) drive abort criteria: if lag > 60s sustained, or a client
  error rate spikes, flip back and investigate.

**Verification:** all clients running healthily against slaptain; the source's write
rate trending down as clients move; lag stable.

**Reversibility:** flip individual clients back to the source. Bidirectional sync keeps
both sides consistent.

#### Stage 4 — drain source nodes

- Once all clients write to slaptain, the source is functionally a hot standby.
- Per node, in order: remove it from the source's node inventory / configuration
  management, drop its `olcServerID` and reciprocal syncrepl stanzas everywhere,
  decommission the node.
- Same on the slaptain side: drop the corresponding `ExternalPeer` entry.

**Verification:** after each drain the topology is consistent (no orphan ServerIDs);
slaptain's `ReplicationConverged` condition stays True.

**Reversibility:** while ≥1 source node remains, the cluster can be re-grown by
re-adding stanzas. Once all source nodes are drained, only forward.

#### Stage 5 — slaptain alone

- Last source node retired. Slaptain is the authoritative cluster.
- Optionally, in a maintenance window: renumber ServerIDs back to `1..N` (drop
  `serverIDBase`) and clear `foreignServerIDs`. Cosmetic — the shifted IDs keep working
  — and not required for correctness.

### Soak windows (recommended, not enforced)

| Transition | Minimum soak before next stage |
|---|---|
| Stage 1 → 2 | Initial refresh complete + 24h CSN convergence |
| Stage 2 → 3 | 48h bidirectional with lag < 5s |
| Stage 3 (per client) | 24h per client |
| Stage 4 (per node drain) | 12h per node |

These are runbook recommendations. The operator does not enforce them — that would turn
slaptain into a migration scheduler, which it explicitly is not.

### Abort criteria (during dual-running)

- Lag > 60s sustained for > 5 minutes in either direction.
- CSN divergence detected (e.g. by `slctl inspect`).
- Replication errors above threshold on either side.
- A spot-check write on the source missing from slaptain after twice the expected lag
  bound, or vice versa.

On any abort: stop the in-progress transition, freeze further client cutovers,
investigate.

## Consequences

- **Slaptain ships the contract, not the schedule.** The operator implements
  `syncMode`, `serverIDBase`, `foreignServerIDs`, and the `mode` transitions from
  ADR-010. The schedule (when to flip client X, when to drain node Y) is a runbook
  concern, executed manually with metric guardrails.
- **Delta/plain interop is asymmetric in detection.** slaptain knows it is plain toward
  the source because we set `syncMode: plain`; the source just sees a `syncprov` on the
  data DB and consumes from it, unaware slaptain is delta internally. Both directions
  are covered explicitly by the Phase 2.1 e2e.
- **Rollback stays available until the source is fully drained.** Through Stage 4 —
  while ≥1 source node remains — falling back to the source is a documented procedure;
  past Stage 5, recovery is a backup restore like any other.

## Related

- ADR-002: cn=config is node-local (slaptain does not replicate cn=config; a source may
  partially replicate it on some nodes — irrelevant during migration since the two run
  in different cn=config domains)
- ADR-003: Operator owns all syncrepl configuration (ServerID scheme is
  operator-managed; foreign declarations are deployer-supplied facts about the other
  cluster)
- ADR-007: Multus replication network (one network-plumbing option for Stage 2)
- ADR-009: SlapdUser lifecycle (Stage 1 prerequisite: the syncrepl bind identity must
  exist on the slaptain side too, per `tests/resources/example-multidb/`)
- ADR-010: SlapdCluster replication modes (consumes this ADR's stage definitions for
  promotion/demotion semantics)
