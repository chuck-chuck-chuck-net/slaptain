# ADR-016: Direct native pod-IP routing for cross-cluster replication

**Status:** Accepted (impl + e2e green across three routed-pod-CIDR sites 2026-07-06)
**Date:** 2026-07-06

## Context

ADR-007 added a dedicated replication network (a Multus secondary NIC) as a
cross-site transport that improves on NodePort. Its discovery machinery is
transport-agnostic: the operator lists remote slapd pods via a remote kubeconfig,
emits one `ldaps://<ip>:1025` syncrepl stanza per remote pod, and sets
`tls_reqcert=allow` because the IP isn't in the cert SAN. Multus only decides
*which* IP the operator reads per pod (the `net1` address).

Where the cluster's primary pod network is natively routed across sites, each
pod's primary IP is already reachable from the other cluster, so a secondary NIC
is redundant — the operator can point syncrepl directly at remote primary pod IPs.

## Decision

Add a third cross-cluster transport that reuses ADR-007's discovery path but reads
each remote pod's primary IP (`pod.status.podIP`) instead of its Multus `net1` IP.
Everything downstream is unchanged.

`spec.replication.network` gains a mode:

- `mode: pod-routed` (**default**) — reads the primary pod IP; needs no
  `multusNetwork`, no NAD, and no operator secondary NIC. Prerequisite: pod CIDRs
  routed between sites.
- `mode: multus` — a dedicated Multus secondary network; requires `multusNetwork`,
  and is the inferred default when `multusNetwork` is set.

Multus is niche at this stage, so pod-routed is the default; naming a
`multusNetwork` (without an explicit mode) still selects multus, so existing
Multus configs keep working unchanged.

`useForInCluster` does not apply to `pod-routed`; in-cluster syncrepl stays on
headless DNS, only cross-cluster stanzas use pod IPs.

ClusterMesh-style global Services were considered and rejected for this path: a
global Service load-balances across all backends behind one VIP, but N-way
multi-master must address each specific peer pod (per serverID/RID).

## Consequences

- (+) CNI-agnostic — needs only routed pod CIDRs.
- (+) No secondary NIC, NAD, or operator network attachment.
- (+) Reuses the whole ADR-007 discovery/stanza/TLS path — small, low-risk.
- (+) Backward compatible; default behavior unchanged.
- (−) No physical traffic isolation (a Multus-specific benefit).
- (−) Primary pod IPs are ephemeral — handled by existing reconcile-time rediscovery.
- (−) Requires cross-site pod-CIDR routing; where absent, use Multus or NodePort.

## Related

- ADR-003: Operator owns all syncrepl configuration
- ADR-007: Multus dedicated replication network (discovery machinery reused here)
- ADR-011: Hot migration topology (serverID/RID coordination, transport-independent)

## Amendment (2026-07-06): discovery depends on node-network reachability, not just pod routing

First green run of the full cross-cluster suite on real cross-site infrastructure
confirms the transport works. Debugging the initial bring-up surfaced a dependency
worth recording explicitly:

Peer **discovery** and the replication **data path** have *different* routability
requirements. The data path is pod-to-pod (`ldaps://<remote-podIP>:1025`) and is
what "routed pod CIDRs" in the Decision refers to. Discovery, however, reuses
ADR-007's remote-kubeconfig machinery, which reaches the **remote Kubernetes API
server** — published on the remote node/management network, *not* the pod network.
So pod-routed replication needs the remote API endpoint reachable from the operator
pod as a separate prerequisite; routed pod CIDRs alone are not sufficient.

In practice both usually ride the same cross-site fabric and succeed or fail
together. A fabric fault during bring-up blackholed transit for both networks
simultaneously (BGP control-plane routes present and symmetric in both node
kernels, but the site router forwarded nothing beyond itself) — presenting as the
operator's `remote peer discovery failed: context deadline exceeded` reaching the
remote API server, and downstream `no remote addresses available` /
`replicationState: Unreachable`. The failure was entirely in the underlay, not in
slaptain, Cilium's datapath, or the kubeconfig — a reminder that those operator
statuses faithfully report an unroutable environment rather than an operator bug.

Prerequisites for `mode: pod-routed`, restated:
1. Pod CIDRs routed between sites (data path).
2. Each remote cluster's API server reachable from the operator pod (discovery path).

## Amendment (2026-08-26): replacing a pod invalidates every peer's syncrepl addresses

A consequence of naming pod IPs that the Decision above does not draw out, found by
a multi-site e2e failure and then measured.

Under this transport a peer's `olcSyncRepl` stanzas name our **pod IPs**. A pod IP
is not stable across pod replacement, so **anything that replaces a pod silently
breaks the "peer consumes from us" direction** until that peer's operator
rediscovers the new address and rewrites its stanzas. Nothing local looks wrong
while this lasts: the StatefulSet is Ready, the cluster is `Running`, local CSNs
converge. The break is entirely in the peer's configuration, and the peer is the
only party that can see it (ADR-008 amendment of the same date).

The operations that do this are more numerous than "a pod crashed":

- any pod restart, rolling or simultaneous;
- a pod losing its PVCs and being recreated;
- **the restore state machine** — `SlapdDatabase.spec.bootstrapFrom` and
  `SlapdRestore` both scale the cluster to 0 and back up (ADR-014), which replaces
  *every* pod and therefore invalidates *every* address a peer holds for us. A
  restore on a mesh member is also a cross-site replication outage for the length
  of the rediscovery window.

### Recovery latency, measured

Rediscovery runs inside the SlapdCluster reconcile, and for a `Running` cluster the
only requeue available is `csnCheckInterval` — **60 s**
(`slapdcluster_controller.go`). The stanza rewrite that follows is *not* a second
60 s hop: the SlapdDatabase controller `Watches(&SlapdCluster{})`, so the status
write enqueues it immediately. The consumer then reconnects on its syncrepl retry
(`retry: "10 +"` → 10 s). One clean cycle is therefore ≈ 70 s.

Observed recovery, deleting all of one site's pods and timing a write there until it
appeared on a peer, on a three-site pod-routed lab: **147 s, 140 s, 268 s, 130 s**.

The excess over 70 s is not distance or the number of sites — it is **repeated ticks
against a moving target**. Discovery collects pods in `Status.Phase == PodRunning`
with a non-empty `PodIP`; during a multi-pod restart the replacements come back
sequentially, so a tick landing mid-restart observes a *partial* set, writes stanzas
naming that partial set, and must then wait a full 60 s for the next tick to correct
it. Land badly two to four times and the outage is minutes:

    recovery ≈ (ticks needed to observe a stable, complete set) × 60 s + 10 s

By contrast, replacing a **single** pod recovered in 5–10 s, because the peer still
had a live provider among its other stanzas, and PVC loss on one pod in 5 s–1 m 22 s.
The multi-minute case is specifically "every provider address a peer holds went
stale at once".

This is latency, not damage: it converges by itself, with no intervention and no
data loss. Recorded because nothing stated it, because it is a property of *this*
transport and not of `multus` mode with stable NAD addressing, and because an
operator seeing a restore stall cross-site replication for four minutes should be
able to find out why. Shortening it is tracked in `docs/BACKLOG.md` — the cause is
that address discovery has no vote in the requeue decision, not that 60 s is wrong
for CSN monitoring.

e2e note: whoever replaces a pod must wait for the peer sites to rediscover it
before yielding to the next spec, or the next cross-site assertion inherits the
window. `tests/e2e/helpers_test.go` (`waitForCrossSiteReplication`) enforces that
for all four pod-replacing specs.
