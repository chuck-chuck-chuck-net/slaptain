# Slaptain Migration Plan

High-level plan for migrating an existing OpenLDAP deployment onto a slaptain-managed
Kubernetes cluster — the strategy, the phased approach, and the decision points. It
delegates the details rather than restating them:

- **Source-side requirements** → [`MIGRATION-LEGACY-SOURCE.md`](MIGRATION-LEGACY-SOURCE.md):
  what your existing deployment must provide before slaptain can consume from it.
- **Replication decisions** → ADR-010 (replication modes) and ADR-011 (hot-migration
  topology contract).
- **Hot-migration runbook** → [`MIGRATION-HOT.md`](MIGRATION-HOT.md): the executable,
  stage-by-stage procedure.

## Two migration modes

**Cold migration** — a maintenance-window cutover: freeze writers, `slapcat` each
database on the source, `slapadd` into slaptain, cut clients over. Simple and robust,
but it costs downtime for the window. It is always available — it asks nothing of the
source's live replication topology.

**Hot migration** — slaptain joins the source's syncrepl mesh as a peer, takes traffic
gradually, and the source nodes drain and retire. Zero maintenance window. It requires a
source that can be reconfigured to replicate with slaptain (ADR-011), and it is the only
path that scales to large, always-on directories without a noticeable service window.

Choose by:

- **downtime tolerance** — if a maintenance window is acceptable, cold is far simpler;
- **directory size / availability** — large or always-on directories favour hot;
- **source reconfigurability** — a source whose replication config is frozen leaves only
  cold.

→ Cold is the floor and the fallback; hot is the preferred outcome where the constraints
allow it. Cold stays available even mid-hot-migration — ADR-011 keeps a rollback path
until the last source node is drained.

## Prerequisites

**Source side.** The source must satisfy the contract in
[`MIGRATION-LEGACY-SOURCE.md`](MIGRATION-LEGACY-SOURCE.md): a `syncprov` provider on each
replicated database, a replication bind identity with read access (including
`userPassword` and operational attributes), network reachability, TLS trust, ServerID
hygiene, and schema parity. Run its pre-flight script before scheduling anything.

**Slaptain side.** Reproduce the source's directory shape on slaptain before the first
sync or import:

- **Schemas** — every non-core schema the source uses, as `SlapdSchema` CRs, applied
  *before* the initial refresh/import (a missing schema fails the load).
- **ACLs** — the source's access rules translated into `SlapdDatabase.spec.acls` (the
  same `access to …` syntax).
- **Service accounts** — the bind identities your applications use, as `SlapdUser` CRs
  (ADR-009).
- **Indexes** — an index baseline matching the source's query patterns.
- **Sizing** — `olcDbMaxSize` and container resources sized for the dataset, validated
  under load (Phase B).

## Phases

**A — Prepare slaptain.** Translate the source's schemas, ACLs, service accounts, and
indexes onto slaptain (above), and stand up a cluster shaped like the target.

**B — Validate at representative scale.** Load a dataset representative of the source's
size; baseline write throughput and search latency (with and without indexes); tune
`olcDbMaxSize`, accesslog purge cadence, syncprov checkpoints, and fd/memory limits.
Record the baselines — they set the sizing for the real cutover.

**C — Execute the chosen path.**

- *Cold:* import each database (offline `slapadd` on a quiesced pod for large dumps, or
  online `ldapadd` for small ones), verify entry counts and sample binds per DB, then cut
  clients over.
- *Hot:* follow the ADR-011 stages, driven by [`MIGRATION-HOT.md`](MIGRATION-HOT.md) —
  stand up slaptain `consumer-only` pulling from the source, soak until converged,
  promote to `peer` (the source adds reciprocal wiring), soak bidirectionally, cut
  clients over one at a time, then drain the source nodes.

**D — Drill first.** Rehearse the chosen path end-to-end in a lab that mirrors the source
(same schemas, ACLs, representative data) before touching the real deployment. Measure
downtime (cold) or soak/lag behaviour (hot), and fix the runbook from what you learn.

## Rollback

- **Cold:** re-point clients at the source; slaptain becomes a read-only consumer until
  you re-freeze and retry.
- **Hot:** reversible per-stage until the last source node is drained. ADR-011 documents
  the per-stage rollback and the freeze-window caveat that applies once slaptain has
  started accepting writes.

## Known caveats

- **OpenLDAP upstream ITS#9580.** After a pod loses its data volumes and rejoins via
  syncrepl, slapd can spin in a `sync cookie is stale` / `delta-sync lost` loop for
  several minutes — burning CPU — before the DIT reconverges. This is an **upstream
  OpenLDAP issue, not a slaptain defect**; any OpenLDAP-based system inherits it. Because
  migration leans heavily on syncrepl, exercise failure-recovery (pod / PVC loss) in your
  drill. Details:
  [`INVESTIGATION-replication-divergence-after-dataloss-and-restart.md`](INVESTIGATION-replication-divergence-after-dataloss-and-restart.md).
