# Hot Migration Runbook

**Status:** Draft — mechanism implemented and e2e-verified; not yet drill-tested at scale.

This is the operational runbook for a **zero-maintenance-window** migration from an
existing OpenLDAP cluster (the *source*) to a slaptain-managed Kubernetes cluster.
Slaptain joins the source's syncrepl mesh as a peer, takes traffic gradually, and the
source nodes drain and retire.

It is the execution-level companion to the design docs — read those first if you have
not:

- `ADR-010` — slaptain replication modes (`peer` / `consumer-only`) and the in-place
  promotion/demotion mechanics this runbook drives.
- `ADR-011` — the hot-migration topology contract (stage definitions, ServerID
  coexistence, plain-syncrepl interop, soak/abort windows).
- `MIGRATION-LEGACY-SOURCE.md` — the **source-side** pre-flight: what the source cluster
  must look like before slaptain points at it.
- `MIGRATION-PLAN.md` — where this runbook sits in the overall migration strategy.

> **⚠ Known upstream caveat.** OpenLDAP upstream bug
> [ITS#9580](https://bugs.openldap.org/show_bug.cgi?id=9580) reproduces in slaptain's
> `dataloss_recovery_test`: after a pod loses its PVCs and rejoins via syncrepl, slapd
> can spin in a `sync cookie is stale` / `delta-sync lost` loop for minutes. DITs
> converge, but the CPU spike and recovery latency make a production hot migration risky
> until the trigger conditions are understood. This is an **upstream OpenLDAP issue, not
> a slaptain defect** — any OpenLDAP-based system inherits it. See
> `docs/INVESTIGATION-replication-divergence-after-dataloss-and-restart.md`. Exercise
> failure-recovery (pod / PVC loss) in your drill before running this against production;
> it is safe to drill in a lab.

---

## What this runbook does and does not cover

**Covers (slaptain side):** every action you take on the slaptain cluster — CR edits,
verification commands, the promotion flip, and source decommissioning sequencing.

**Flags but does not execute (source side):** the reciprocal wiring on the source cluster
(adding slaptain's ServerIDs and syncrepl stanzas) is **not under slaptain's control** —
it is manual or config-managed work on the source, per ADR-011 §Consequences. This runbook
marks every such step **`[SOURCE-SIDE]`** and gives the LDIF you'll need, but someone with
admin on the source cluster runs it.

**Two implementation gaps to know before you start:**

1. **ServerID collision validation is not shipped.** Only
   `spec.replication.serverIDBase` (SlapdCluster) and `spec.replication.ridBase` (per
   SlapdDatabase) exist; there is no webhook that rejects a slaptain ServerID overlapping
   the source's. Pick `serverIDBase` **by hand** to clear the source's ServerIDs and
   verify it manually (§2) — getting this wrong silently stops replication (ADR-011 §3,
   `MIGRATION-LEGACY-SOURCE.md §5`). RIDs need no such care: they are consumer-local and
   never cross the cluster boundary (ADR-011), so slaptain's RIDs and the source's cannot
   collide.
2. **`slctl import-data` is not shipped.** It is not needed for the hot path — syncrepl
   does the data transfer — but you cannot use it as a fallback seeding mechanism here.

---

## Prerequisites

Before Stage 1, all of these must hold:

| # | Prerequisite | Where |
|---|---|---|
| 1 | Source is syncrepl-ready (syncprov, bind user, ACLs, TLS, reachability) | `MIGRATION-LEGACY-SOURCE.md §1–4`, run `scripts/check-source-ready.sh` |
| 2 | Source ServerIDs recorded; slaptain `serverIDBase` chosen to avoid them | `MIGRATION-LEGACY-SOURCE.md §5`, this doc §2 |
| 3 | Schema parity: every non-core source schema applied as a `SlapdSchema` CR | `MIGRATION-LEGACY-SOURCE.md §6` |
| 4 | ACLs and service users (`cn=syncuser`, app accounts) declared on slaptain | `MIGRATION-PLAN.md` (Prerequisites — slaptain side) |
| 5 | Network path slaptain pods → source LDAP port, **and** (for promotion) source → slaptain pods | `MIGRATION-LEGACY-SOURCE.md §4`, ADR-007 |
| 6 | Replication lag observable (via `slctl inspect` / status / metrics) | this doc §"Verification" |
| 7 | A spot-check write/audit procedure agreed for both directions | this doc §"Abort criteria" |

Schema parity (3) is the most common silent failure: a missing schema makes the initial
refresh reject entries and you start over after wiping the data PVCs. Apply schemas
**before** Stage 1.

---

## 2. Identifier plan (do this on paper first)

Replication breaks silently if slaptain and the source share a **ServerID** (ADR-011 §3):
ServerIDs are global to a replication topology, embedded in every CSN, and a shared one
makes each side treat the other's writes as its own and drop them. Because the collision
webhook is not shipped, plan and verify ServerIDs by hand.

RIDs need no coordination at all — a `rid` is a consumer's *local* handle for one syncrepl
directive and never crosses the wire, so slaptain and the source number their own stanzas
independently and cannot collide (ADR-011). `ridBase` remains slaptain's *internal* per-DB
concern (below), unaffected by the source.

**Record the source's ServerIDs** (from `MIGRATION-LEGACY-SOURCE.md §5`):

```bash
# [SOURCE-SIDE] list source ServerIDs
ldapsearch -x -H ldaps://source.example.com -D "cn=admin,cn=config" -w "$ROOT_PW" \
  -b "cn=config" -s base "(objectClass=*)" olcServerID
```

**Pick a non-colliding `serverIDBase`.** Worked example — a source using per-site
ServerIDs 101–104 (site A) and 201–204 (site B):

```yaml
# SlapdCluster
spec:
  replicas: 4
  replication:
    enabled: true
    mode: consumer-only
    serverIDBase: 500          # slaptain pods get 501, 502, 503, 504 — clear of 1xx/2xx
```

`ridBase` is independent of the source. Give each slaptain database a distinct `ridBase`
(the conventional defaults `100`/`200`/`300` are fine) so slaptain's *own* databases don't
overlap each other — there is nothing to coordinate with the source here.

**Verification (manual, since there's no webhook):** write the source ServerIDs and the
computed slaptain range into your per-cutover runbook and eyeball that they're disjoint.
After Stage 1, confirm no collision actually occurred:

```bash
slctl inspect -n <ns> <cluster>     # flags CSN/topology inconsistencies
```

---

## Stage map

Each stage is a stable configuration. The operator never auto-advances — every transition
is a deliberate CR edit or `[SOURCE-SIDE]` action. ADR-011 §"Stage transitions" is the
authority; this is the executable form.

| Stage | slaptain state | Source state | Reversible? |
|---|---|---|---|
| 0 | not present | authoritative, serving all clients | n/a |
| 1 | `consumer-only`, read-only, pulling from source | unchanged | **total** |
| 2 | `peer`, bidirectional | reciprocal stanzas added | yes, with source-side undo |
| 3 | `peer`, taking client traffic | losing client traffic | per-client |
| 4 | `peer`, authoritative | draining node-by-node | until last node drains |
| 5 | authoritative, alone | retired | forward only |

---

## Stage 1 — slaptain as consumer-only

**Goal:** slaptain fully syncs the source's data without advertising itself as a provider.
The source is untouched and unaware.

### Actions (slaptain side)

1. Provision the source-bind Secret and CA Secret (`MIGRATION-LEGACY-SOURCE.md §2, §4`).
2. Apply the SlapdCluster in consumer-only mode. Start at `replicas: 1` to bound
   initial-refresh WAN traffic; scale up after the first refresh completes.

```yaml
apiVersion: ldap.chuck-chuck-chuck.net/v1alpha1
kind: SlapdCluster
metadata:
  name: slapd
  namespace: slaptain
spec:
  replicas: 1                       # scale to N after initial refresh
  replication:
    enabled: true
    mode: consumer-only
    serverIDBase: 500
    externalPeers:
      - name: legacy-source-1
        uri: ldaps://10.1.1.20:636
        syncMode: plain             # source uses plain syncrepl, not delta
        bindDN: cn=syncuser,ou=config,o=example
        bindPasswordSecretName: legacy-source-bind-pw
        tlsSecretName: legacy-source-ca
      - name: legacy-source-2       # pull from ≥2 source nodes for redundancy
        uri: ldaps://10.1.2.20:636
        syncMode: plain
        bindDN: cn=syncuser,ou=config,o=example
        bindPasswordSecretName: legacy-source-bind-pw
        tlsSecretName: legacy-source-ca
```

3. Apply the matching `SlapdDatabase` and `SlapdSchema` CRs **before** the cluster
   finishes its first refresh.

In `consumer-only` the operator configures each pod with `olcReadOnly: TRUE`, no
`accesslog`/`syncprov` overlays on the data DB, no in-cluster mesh — only the
`externalPeers` syncrepl stanzas (ADR-010 §`consumer-only`).

### Verification

```bash
slctl status  -n slaptain slapd          # phase=Running, replicationMode=consumer-only
slctl inspect -n slaptain slapd          # CSN convergence, per-peer lag

# entry counts match the source, per DB
slctl ldapsearch -n slaptain --database example -b o=example '(objectClass=*)' dn | grep -c '^dn:'
# compare to: [SOURCE-SIDE] same search against the source

# writes are rejected (proves read-only)
echo 'dn: uid=canary,ou=People,o=example
changetype: add
objectClass: inetOrgPerson
cn: canary
sn: canary
uid: canary' | slctl ldapmodify -n slaptain      # expect: unwillingToPerform
```

Check `status.externalPeerStatuses[].replicationState == Synced` and `lagSeconds` stable
and small.

### Soak / exit

Initial refresh complete + ≥24h of stable CSN convergence (ADR-011 soak table). Then scale
`replicas` to the target N and re-verify each pod independently syncs and stays converged.

### Rollback

**Total.** Delete the SlapdCluster (and its PVCs, per ADR-005/ADR-012). The source is
unaffected — no source node consumes from slaptain.

---

## Stage 2 — promote to peer + source-side reciprocal wiring

**Promotion is a single CR field edit; the operator does the rest in place.**

### Action (slaptain side) — the promotion

```bash
kubectl patch sc slapd -n slaptain --type merge \
  -p '{"spec":{"replication":{"mode":"peer"}}}'
# or: kubectl edit sc slapd -n slaptain   → set spec.replication.mode: peer
```

When the operator observes the mode change it performs an **in-place reconfiguration of
each pod's `cn=config`** (ADR-010 §"In-place promotion"), with no pod restart and no data
re-sync:

1. Creates the accesslog DB on each pod (the volume was provisioned in Stage 1 precisely
   so this needs no StatefulSet change).
2. Adds `overlay accesslog` + `overlay syncprov` to the data DB.
3. Drops `olcReadOnly: TRUE` — the cluster starts accepting writes.
4. Adds the in-cluster mesh syncrepl stanzas (one per peer pod).
5. Sets `status.replicationMode: peer` and raises `ReplicationModePeerWiringRequired=True`.

### Verify the promotion stayed in-place

```bash
slctl status -n slaptain slapd      # replicationMode flips to peer within ~90s

# in-place guarantees (the migration e2e asserts both):
kubectl get pod slapd-0 -n slaptain -o jsonpath='{.metadata.uid}'        # unchanged
kubectl get pod slapd-0 -n slaptain \
  -o jsonpath='{.status.containerStatuses[?(@.name=="slapd")].state.running.startedAt}'   # unchanged

# writes now accepted
echo 'dn: uid=post-promote,ou=People,o=example
changetype: add
objectClass: inetOrgPerson
cn: post-promote
sn: post-promote
uid: post-promote' | slctl ldapmodify -n slaptain      # expect: success
```

Entry `entryUUID`s are preserved across the flip (no re-key, no full re-sync) — this is
what makes promotion cheap and safe to do at the sensitive moment of the migration.

### Action `[SOURCE-SIDE]` — reciprocal wiring (config-managed / manual)

Until the source is wired up, slaptain advertises as a provider but **no source node
consumes from it**. The condition tells you what's owed:

```bash
kubectl get sc slapd -n slaptain \
  -o jsonpath='{.status.conditions[?(@.type=="ReplicationModePeerWiringRequired")].message}'
```

On the source, for **every** source node and **every** replicated DB:

1. Add slaptain's ServerIDs to the source's `olcServerID` list:

```ldif
dn: cn=config
changetype: modify
add: olcServerID
olcServerID: 501 ldaps://slapd-0.slapd-headless.slaptain.svc.cluster.local:1025
olcServerID: 502 ldaps://slapd-1.slapd-headless.slaptain.svc.cluster.local:1025
olcServerID: 503 ldaps://slapd-2.slapd-headless.slaptain.svc.cluster.local:1025
olcServerID: 504 ldaps://slapd-3.slapd-headless.slaptain.svc.cluster.local:1025
```

(Use whatever address the source can actually reach slaptain on — pod DNS only works if
the source is in-cluster; cross-site uses the Multus/routed IPs from ADR-007 or a
NodePort/LB. Decide per migration window.)

2. Add reciprocal `olcSyncRepl` stanzas pointing at each slaptain pod, using the source's
   existing `cn=syncuser` bind and **plain** syncrepl (no `syncdata=accesslog`), matching
   `syncMode: plain` on slaptain's side.

3. Roll source nodes one at a time to pick up the config.

### Verification

Bidirectional replication is live: a write on slaptain appears on the source and vice
versa within the lag bound. Watch both directions:

```bash
slctl inspect -n slaptain slapd       # ReplicationConverged=True; per-peer lag < 5s
# [SOURCE-SIDE] confirm slaptain ServerIDs appear in the source's contextCSN set
```

### Soak / exit

48h of bidirectional traffic with lag < 5s in **both** directions, no `unwillingToPerform`
from slaptain, no CSN divergence.

### Rollback (demotion)

Possible, but **asymmetric** — drop the source's reciprocal stanzas *first*, then demote
slaptain:

```bash
# [SOURCE-SIDE] remove the reciprocal olcSyncRepl stanzas + slaptain olcServerIDs, roll nodes
kubectl patch sc slapd -n slaptain --type merge \
  -p '{"spec":{"replication":{"mode":"consumer-only"}}}'   # operator demotes in place
```

Order matters: demoting before the source stops consuming would leave a window where the
source pulls from a cluster that has gone read-only. **Caveat:** after 48h of bidirectional
traffic, slaptain may hold writes the source hasn't yet replicated. Demotion itself is safe
(slaptain stops accepting writes), but if you then tear slaptain down, those writes are
lost unless you `slapcat` them back to the source first. ADR-011 Stage 2 calls this the
"freeze window" for rollback.

---

## Stage 3 — gradual client cutover

**Goal:** move client traffic (Postfix, Dovecot, and other directory clients) from the
source's LB/VIP to slaptain's Service, one client at a time.

### Actions

- Repoint one client at slaptain's ClusterIP/LB Service (`slapd.slaptain.svc` or the
  external endpoint).
- Soak that client ≈24h before flipping the next.
- Bidirectional sync (Stage 2) keeps both sides consistent regardless of where a given
  client writes.

### Verification / abort

- Per-client error rate flat after cutover; lag stable (`slctl inspect`, lag metrics).
- **Abort criteria** (ADR-011): lag > 60s sustained > 5 min in either direction, CSN
  divergence, replication errors spiking, or a spot-check write missing on one side after
  2× the lag bound. On abort: flip that client back to the source, freeze further
  cutovers, investigate.

### Rollback

Flip individual clients back to the source. No cluster-level change needed.

---

## Stage 4 — drain source nodes

**Goal:** with all clients on slaptain, retire the source nodes one at a time.

### Per node, in order

1. `[SOURCE-SIDE]` Remove the node from the source's cluster config (its `olcServerID`, its
   outbound/inbound syncrepl stanzas), roll the remaining nodes, decommission the node.
2. On slaptain, drop the corresponding `externalPeers` entry:

```bash
kubectl edit sc slapd -n slaptain    # remove the externalPeers[] entry for the drained node
```

### Verification

After each drain: `slctl inspect` shows `ReplicationConverged=True`, no orphan
ServerIDs, lag stable. 12h soak per node (ADR-011 soak table) before draining the next.

### Rollback

While **≥1 source node remains**, re-add its stanzas on both sides to re-grow the mesh.
Once the **last** source node is drained, this is forward-only — recovery then means a
backup restore (ADR-014, `docs/BACKUP.md`).

---

## Stage 5 — slaptain alone + source decommissioning

This is the expansion of `MIGRATION-LEGACY-SOURCE.md §11` ("Decommissioning the source
after cutover").

The last source node is retired; slaptain is the authoritative cluster. Tidy up both
sides.

### `[SOURCE-SIDE]` final source teardown

Once you are certain slaptain is authoritative and no client or peer still reads from the
source:

1. Remove the replication-bind ACL grant (the `to * by dn.exact="cn=syncuser,…" read`
   entry added per `MIGRATION-LEGACY-SOURCE.md §3`).
2. Remove the replication/sync bind user, **or** leave it inert (no ACL grant means it can
   bind but read nothing).
3. Drop the source's `olcServerID` list and any remaining `olcSyncRepl` stanzas.
4. Stop the source `slapd`; decommission the host.

Keep a final `slapcat` of each source DB as a cold archive before stopping slapd — cheap
insurance, and it's the only artifact left if a missed write surfaces later.

### slaptain side — optional cosmetic cleanup

The migration identifiers keep working forever; renumbering is cosmetic and not required
for correctness (ADR-011 Stage 5). In a maintenance window you *may*:

- Drop `serverIDBase: 500` to renumber pods back to `1..N`.
- Remove the now-dead `externalPeers` entries (should already be gone from Stage 4).
- Rotate or remove the `legacy-source-bind-pw` / `legacy-source-ca` Secrets.

Renumbering ServerIDs forces a syncrepl re-evaluation across the cluster — do it
deliberately, watch `slctl inspect`, and weigh it against "the high-numbered IDs are
harmless" before bothering.

---

## Field & condition reference (what's actually implemented)

| CR field | Effect |
|---|---|
| `SlapdCluster.spec.replication.mode` | `peer` (default) or `consumer-only`. Editing it triggers in-place promotion/demotion. |
| `SlapdCluster.spec.replication.serverIDBase` | Shifts pod ServerIDs to `base+ordinal+1`. Set to avoid source collision. |
| `SlapdCluster.spec.replication.externalPeers[].syncMode` | `delta` (default) or `plain`. Use `plain` for a source that doesn't run delta-syncrepl. |
| `SlapdCluster.spec.replication.externalPeers[].{uri,bindDN,bindPasswordSecretName,tlsSecretName}` | Source connection. |
| `SlapdDatabase.spec.replication.ridBase` | Per-DB RID base — slaptain-internal uniqueness only; independent of the source. |

| Status / condition | Meaning |
|---|---|
| `status.replicationMode` | Observed mode; lags `spec` briefly during a transition. |
| `status.externalPeerStatuses[]` | Per-peer `replicationState` (`Synced`/`Lagging`/`PartiallyVerified`/`Unreachable`), `lagSeconds`, `discoveredAddresses`. `PartiallyVerified` = the peer answered and everything read is current, but at least one database yielded no readable `contextCSN` (`lastError` names it) — treat it as "not verified", not as Synced. |
| condition `ReplicationConverged` | Local CSN convergence across pods, judged **per database** and ANDed; `Unknown` when a pod×database pair could not be read. |
| condition `ReplicationModePeerWiringRequired` | `True` after promotion — message lists the `[SOURCE-SIDE]` wiring owed. |
| condition `ReplicationModeDemoteWiringRequired` | `True` in consumer-only with external peers — the demote-side handoff. |

**Not implemented (plan for, don't reference in manifests):** a ServerID-collision webhook
(`foreignServerIDs`) — coordinate ServerIDs by hand until it ships; `slctl import-data`.

---

## Abort & rollback quick reference

| Where you are | Fastest safe rollback |
|---|---|
| Stage 1 (consumer-only) | Delete the SlapdCluster. Source untouched. |
| Stage 2 (peer, pre-traffic) | `[SOURCE-SIDE]` drop reciprocal stanzas → demote to `consumer-only`. Slapcat slaptain-only writes back if tearing down. |
| Stage 3 (client cutover) | Flip the affected client back to the source LB. |
| Stage 4 (draining), ≥1 source node left | Re-add the drained node's stanzas on both sides. |
| Stage 4, last node drained / Stage 5 | Forward only — recover via backup restore (ADR-014). |

Universal abort triggers (any stage with dual-running): lag > 60s sustained > 5 min, CSN
divergence, replication error spikes, or a spot-check write missing on one side after 2×
the lag bound. Stop the in-progress transition, freeze client cutovers, investigate before
proceeding.

---

## Related

- ADR-010 — replication modes and in-place promotion/demotion
- ADR-011 — hot-migration topology contract (the authority for stages, IDs, soak/abort)
- ADR-007 — Multus replication network (one option for source↔slaptain reachability at Stage 2)
- ADR-014 — backup/restore (the recovery path once rollback is forward-only)
- `MIGRATION-LEGACY-SOURCE.md` — source-side pre-flight and §11 decommissioning
- `MIGRATION-PLAN.md` — the overall migration strategy this runbook executes
