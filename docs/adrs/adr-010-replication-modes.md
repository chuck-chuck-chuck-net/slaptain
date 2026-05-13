# ADR-010: SlapdCluster Replication Modes (peer / consumer-only)

**Status:** Accepted
**Date:** 2026-05-04
**Accepted:** 2026-05-13 — implemented and e2e-verified (`tests/e2e/migration_test.go`, 6/6 specs green)

## Context

Slaptain today supports exactly one replication topology: every RW pod is a full
multi-master peer. Each pod runs `accesslog` + `syncprov` overlays, has
`olcMultiProvider: TRUE` on the data DB, and consumes from every in-cluster peer plus
declared `ExternalPeer`s. The result: writes accepted on any pod propagate everywhere.

This is the right default once a cluster is "the" authoritative source. It is the
**wrong** default during a hot migration from an external LDAP cluster (e.g. prod
VMs), for two reasons:

1. **Write-history forking.** If prod and slaptain both accept writes during dual
   running, two independent CSN trees exist. Bidirectional syncrepl converges them,
   but every conflict resolution is one more thing that can go wrong, and confidence
   that no writes were lost requires per-DN auditing across both sides.

2. **Initial refresh asymmetry.** Slaptain joining prod's mesh as a peer means prod
   needs to know about slaptain's pods (reciprocal stanzas, ServerIDs added to
   `olcServerID` list) *before* slaptain starts producing accesslog entries — otherwise
   slaptain's first writes are not propagated to prod.

The clean answer: slaptain has a *consumer-only* mode that pulls from prod via
`ExternalPeer` without advertising itself as an authoritative source. Once initial
refresh completes and prod is ready to receive from slaptain, the cluster is
*promoted* to *peer* mode in place — no data re-sync, no StatefulSet replacement.

## Options Considered

### Option A: Reuse `readReplicas` + `ExternalPeer` (rejected)

Treat the consumer-only cluster as a stack of read-only replicas (the existing
`spec.readReplicas` machinery), with their syncrepl source pointed at prod via
`ExternalPeer`.

**Rejected.** RO StatefulSet pods are structurally not RW peers. To "promote" the
cluster at cutover, we would tear down the RO StatefulSet and stand up a new RW
StatefulSet. That means:

- Re-syncing all data from scratch over the WAN. For prod-scale (≈10M entries,
  ≈100GB+), this is hours of WAN traffic at the most sensitive moment of the
  migration.
- Two periods of vulnerability instead of one (initial refresh, then re-sync after
  promotion).
- StatefulSet identity churn (PVC names, pod ordinals) at cutover, which is exactly
  when we want maximum stability.

The RO StatefulSet's purpose is in-cluster read scale-out. Repurposing it for
cross-cluster migration confuses two unrelated concerns.

### Option B: Mode flag on SlapdCluster (chosen)

Add `SlapdCluster.spec.replication.mode: peer|consumer-only` (default `peer`). The
*same* RW StatefulSet runs in either mode; what differs is the cn=config state
(overlays, `olcReadOnly`, syncrepl stanzas). Promotion is an in-place reconfiguration
of cn=config — data on disk and PVC identity are preserved.

**Chosen.** Promotion is fast, reversible, and observable via the existing CSN
monitoring. The data DB content does not change when overlays are toggled; only
metadata in cn=config does.

## Decision

`SlapdCluster.spec.replication.mode` selects the replication mode. Two values:

### `peer` (default — current behavior)

Per pod (in cn=config):

- Data DB: `overlay accesslog`, `overlay syncprov`, `olcMultiProvider: TRUE`,
  `olcReadOnly: FALSE` (or unset).
- Accesslog DB: `overlay syncprov` with `olcSpNoPresent: TRUE`,
  `olcSpReloadHint: TRUE`.
- Syncrepl stanzas: N-1 in-cluster peers + every declared `ExternalPeer`.

This is the existing behavior; nothing changes for clusters without `mode` set or
with `mode: peer`.

### `consumer-only`

Per pod (in cn=config):

- Data DB: **no `accesslog` overlay**, **no `syncprov` overlay**, `olcMultiProvider`
  may be TRUE or FALSE (irrelevant when readOnly is set), **`olcReadOnly: TRUE`**.
- Accesslog DB: **not provisioned** (init container skips it under `consumer-only`).
- Syncrepl stanzas: **only `ExternalPeer`** stanzas. **No in-cluster mesh** — pods
  do not pull from each other; each pod independently consumes from the configured
  external peer(s).

The cluster appears to clients as read-only. Writes are rejected with
`unwillingToPerform`. The cluster is observable: reads work normally, monitoring and
the operator's CSN checks function unchanged.

### In-place promotion: `consumer-only → peer`

When the operator observes `mode` change from `consumer-only` to `peer`, it:

1. Adds the accesslog DB on each pod (init-container path is not invoked at runtime;
   the operator performs the equivalent operations via LDAP modify on cn=config).
   **Note:** this requires the operator to run the same logic the init container
   currently runs at first boot. We will refactor that logic out of `bootstrap.sh`
   into the operator (or into a shared helper invoked by both) as part of the 2.3
   implementation. (See "Consequences" for why this is acceptable scope.)
2. Adds `overlay accesslog` + `overlay syncprov` to the data DB.
3. Drops `olcReadOnly: TRUE` from the data DB.
4. Adds in-cluster syncrepl stanzas (one per peer pod).
5. Surfaces a `Condition: ModePromoted=True` and a `ReplicationModePeerWiringRequired`
   condition that explicitly requests the **prod side** to add reciprocal stanzas
   and slaptain ServerIDs to its `olcServerID` list. This is the human handoff in
   the migration runbook.

After step 5, slaptain pods are technically advertising as providers, but until prod
adds its reciprocal stanzas, no prod node consumes from them. Once prod is wired up,
bidirectional replication begins automatically.

### In-place demotion: `peer → consumer-only`

The reverse, for rollback:

1. Surface a `ReplicationModeDemoteWiringRequired` condition asking prod to drop
   reciprocal stanzas pointing at slaptain ServerIDs.
2. Once prod has stopped consuming, the operator removes in-cluster syncrepl stanzas,
   sets `olcReadOnly: TRUE`, removes the syncprov + accesslog overlays from the data
   DB, drops the accesslog DB.
3. The cluster is now consumer-only again.

The asymmetry (we wait for prod's confirmation before demoting) prevents a window
where prod still consumes from slaptain after slaptain has stopped accepting writes.

### Validation rules

- `mode: consumer-only` requires `replication.enabled: true` AND at least one
  `externalPeers` entry. Without external peers, consumer-only is meaningless.
- `mode: consumer-only` is incompatible with `readReplicas > 0`. Read-only replicas
  exist to scale read traffic in a peer-mode cluster; in consumer-only mode the
  whole cluster is already read-only.
- Mode transitions are gated by all pods being Ready. A partial cluster cannot
  promote or demote — the operator requeues until every pod is reconcilable.

### Status

```yaml
status:
  replicationMode: peer|consumer-only        # observed mode (may lag spec during transition)
  conditions:
    - type: ReplicationConverged             # existing, unchanged
    - type: ModeTransition                   # True during in-place promotion/demotion
      reason: PromotingToPeer|DemotingToConsumerOnly|Stable
    - type: ReplicationModePeerWiringRequired   # set when consumer-only→peer, expects prod-side action
    - type: ReplicationModeDemoteWiringRequired # set when peer→consumer-only, expects prod-side action
```

## Consequences

- **In-place promotion preserves all data on disk.** The MDB files in `/data` and
  `/accesslog` (the latter only after promotion) are not touched during mode
  transitions. The data DB content's CSN history is preserved; the cluster's
  contextCSN does not jump.
- **Consumer-only is genuinely safe to roll back.** During Stage 1 of the hot
  migration (slaptain consumes from prod), tearing down the slaptain cluster has
  zero effect on prod, because no node on prod consumes from slaptain.
- **The operator gains a runtime cn=config rewrite path.** Today, accesslog setup
  happens once in the init container (per-pod, on first boot). For 2.3, the operator
  must apply the same configuration *to a running pod*. This is a net-positive
  refactor — the init container's logic becomes operator logic with init-container
  re-invocation as a fallback — but it is non-trivial scope to call out.
- **Read-only consumer mode is exposed to clients.** While in `consumer-only`,
  attempts to write to slaptain via its Service get `unwillingToPerform`. This is
  the desired behavior during Stage 1 (clients still write to prod). At Stage 3, as
  clients are flipped, they must already be configured to talk to slaptain *and*
  the cluster must already be promoted to peer.
- **`olcMultiProvider` flag is unaffected.** The flag stays TRUE in both modes; what
  changes is whether writes are accepted (gated by `olcReadOnly`) and whether
  changes are advertised (gated by overlay presence).
- **Validation rules cannot be enforced for legacy clusters.** A SlapdCluster
  created on an older operator without `mode` defaults to `peer` on upgrade. No
  data migration is required.
- **Reversibility window during cutover.** Demotion requires prod to first stop
  consuming from slaptain. If prod is already retired (Stage 4+ in the hot
  migration runbook, ADR-011), demotion is no longer possible — the only path
  forward is to keep the cluster in peer mode. The hot migration runbook flags this
  explicitly.

## Related

- ADR-003: Operator owns all syncrepl configuration (mode flag is a controller
  concern, not a chart concern)
- ADR-004: Multi-resource CRD architecture (mode lives on SlapdCluster, not
  SlapdDatabase, because it affects the whole cluster's replication shape)
- ADR-008: CSN monitoring uses replication bind credentials (works identically in
  both modes)
- ADR-011: Hot migration topology contract (consumes this ADR's mode-transition
  semantics)
