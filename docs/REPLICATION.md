# Replication Architecture

## Overview

Slaptain uses OpenLDAP's **N-way multi-master delta-syncrepl** for both in-cluster and
cross-cluster replication. Every RW pod is simultaneously a provider (serves changes to
others) and a consumer (pulls changes from others). There is no designated primary.

## Core concepts

### Consumer-initiated, real-time streaming

OpenLDAP syncrepl has two modes: `refreshOnly` (periodic polling) and `refreshAndPersist`
(persistent connection with real-time push). We use **`refreshAndPersist`**.

The terminology can be confusing because LDAP literature calls syncrepl "pull-based" — but
that refers to **who initiates the connection**, not how data flows:

1. The **consumer** opens a persistent outbound connection to the provider (the "pull" part —
   the provider never initiates connections)
2. Initial catch-up: the consumer requests all changes since its last CSN (refresh phase)
3. Once caught up: the provider **pushes new changes in real-time** over the same connection
   (persist phase) — no polling, sub-second propagation

Think of it as a persistent subscription: the consumer subscribes, then the provider streams
changes as they happen. There is no "forwarding" — each consumer independently subscribes to
its configured providers.

### CSN deduplication (mirrormode / olcMultiProvider)

A Change Sequence Number (CSN) is a globally unique identifier assigned to every write
(`timestamp#count#serverID#modcount`). When a consumer receives a change via replication
and applies it locally, that change gets the same CSN as the original. `olcMultiProvider: TRUE`
tells OpenLDAP: "do not re-replicate changes that arrived via syncrepl." This prevents
infinite loops in the N-way mesh.

### Delta-syncrepl and the accesslog

Plain syncrepl sends the full entry on every change. **Delta-syncrepl** sends only the diff
(which attributes changed), read from the `cn=accesslog` database. The accesslog overlay
records every modification to the data database, including modifications received via
replication. This is what makes the pull chain work: a change that replicates in from another
pod appears in the local accesslog, making it available to any consumer pulling from this pod.

## In-cluster replication (single site)

With `replicas: 3` and `replication.enabled: true`, every pod gets `N-1` syncrepl stanzas
pointing directly at its peers via headless DNS:

```
slapd-0 pulls from:  slapd-1.slapd-headless.<ns>  (rid=002)
                     slapd-2.slapd-headless.<ns>  (rid=003)

slapd-1 pulls from:  slapd-0.slapd-headless.<ns>  (rid=001)
                     slapd-2.slapd-headless.<ns>  (rid=003)

slapd-2 pulls from:  slapd-0.slapd-headless.<ns>  (rid=001)
                     slapd-1.slapd-headless.<ns>  (rid=002)
```

This is a full mesh: 6 persistent connections for 3 nodes. Every write is visible on all
pods within seconds (typically <1s for delta-syncrepl over a local network).

### RID scheme

| RID range | Purpose |
|---|---|
| 001–099 | In-cluster peers (pod ordinal + 1, skip self) |
| 101–199 | External peers (one per `spec.replication.externalPeers` entry) |

## Cross-cluster replication (multiple sites)

When `spec.replication.externalPeers` is configured, **every** RW pod on the local site gets
an additional syncrepl stanza (rid=101+) pointing at the remote site's endpoint.

### Topology with 2 sites, 3 replicas each

```
           siteA                                 siteB
      ┌─────────────────────────┐          ┌─────────────────────────┐
      │  slapd-0  slapd-1  slapd-2 │      │  slapd-0  slapd-1  slapd-2 │
      │    ↕         ↕         ↕    │      │    ↕         ↕         ↕    │
      │  (full mesh, rid 001-003)   │      │  (full mesh, rid 001-003)   │
      └────┬────────┬────────┬──────┘      └────┬────────┬────────┬──────┘
           │        │        │                  │        │        │
           │   rid=101 each  │                  │   rid=101 each  │
           │        │        │                  │        │        │
           └────────┼────────┘                  └────────┼────────┘
                    │          ldaps://                   │
                    └────────────────────────────────────┘
```

Key points:

- **All 3 pods on siteA independently pull from siteB** (via a load-balanced endpoint such
  as a NodePort). There is no single "gateway" pod that replicates externally and then
  distributes internally.
- The load-balanced endpoint pins each persistent TCP connection to one of siteB's pods.
  Different siteA pods may end up connected to different siteB pods — this is fine because
  all siteB pods have the same data and the same accesslog.
- **Redundancy**: if one siteA pod's external connection drops, the other two still pull from
  siteB. The disconnected pod catches up via internal replication from its site peers.

### How a write propagates across sites

Example: a client writes an entry on siteB-pod-1.

```
1. siteB-pod-1  ← client write (entry gets CSN, recorded in accesslog)

2. siteB-pod-0  ← pulls from siteB-pod-1 (in-cluster, rid=002)
   siteB-pod-2  ← pulls from siteB-pod-1 (in-cluster, rid=001)

3. siteA-pod-0  ← pulls from siteB (external, rid=101) — may hit any siteB pod
   siteA-pod-1  ← pulls from siteB (external, rid=101) — may hit any siteB pod
   siteA-pod-2  ← pulls from siteB (external, rid=101) — may hit any siteB pod

4. Steps 2 and 3 happen concurrently. siteA pods that pull the entry from siteB
   record it in their local accesslog. Other siteA pods may also pull it from
   their siteA peers (in-cluster rid=001-003), but CSN deduplication means it's
   applied only once.
```

There is no "forwarding." Every pod independently pulls from its configured providers.
The full mesh within each site plus the all-pods-to-external connection means there are
multiple paths for every change to reach every pod. CSN deduplication ensures exactly-once
application regardless of which path delivers first.

### Cross-site connection models

With `refreshAndPersist`, each syncrepl stanza creates one persistent TCP connection. The
provider streams changes in real-time to the consumer over that connection. In the current
design, every pod on siteA independently pulls the same changes from siteB — each change
crosses the WAN N times (once per pod).

Three models are possible:

```
All-pods (current)         Hub (1 pod)              Hub pair (2 pods)
──────────────────         ───────────              ─────────────────
siteA-0 ──► siteB          siteA-0 ──► siteB        siteA-0 ──► siteB
siteA-1 ──► siteB          siteA-1 → internal       siteA-1 ──► siteB
siteA-2 ──► siteB          siteA-2 → internal       siteA-2 → internal
(same in reverse)          (same in reverse)        (same in reverse)
```

| | All-pods (current) | Hub (1 pod) | Hub pair (2 pods) |
|---|---|---|---|
| Cross-site connections (bidirectional) | 2N | 2 | 4 |
| WAN bandwidth per change | Nx | 1x | 2x |
| Cross-site latency | Direct | +1 internal hop | Direct for hubs, +1 hop for others |
| Single point of failure | None | Hub pod down = cross-site stops | Survives 1 hub failure |
| Operator complexity | Simple (all pods identical) | Must assign hub role | Must assign hub role |

**Current choice: all-pods.** For 3-pod clusters the bandwidth overhead is negligible (3x a
few KB per change). The simplicity of treating all pods identically outweighs the savings.
At 5+ pods per site, a hub-pair model becomes worth implementing — only the external peer
stanzas would be selectively assigned; the internal full mesh stays unchanged.

**Important caveat for hub-and-spoke**: if only designated hub pods have external syncrepl
stanzas, every pod must still be reachable as a provider by the hub pods (via the internal
full mesh). Otherwise, writes on non-hub pods may not propagate to the remote site. The
internal full mesh guarantees this — hub pods pull from all local peers, so writes on any
local pod reach the hub and then cross the WAN.

### Reconnection and CSN resume

When a cross-site connection drops (backend pod restart, network partition, firewall timeout),
the syncrepl consumer:

1. Detects the failure (via TCP keepalive or connection error)
2. Retries per the `retry` parameter (`retry="5 +"` = every 5 seconds, indefinitely)
3. Reconnects through the load-balanced endpoint — may route to a different backend pod
4. Presents its last CSN cookie to the new provider
5. The provider sends all changes since that CSN (delta or full, depending on accesslog state)

No manual intervention needed. If the CSN is still within the provider's accesslog window,
only the delta is sent. If the accesslog has been purged past the consumer's CSN, a full
refresh (`SYNC_REFRESH_REQUIRED`) is triggered — this is automatic and self-healing, though
it transfers more data.

## Cross-cluster access via NodePort

For Kubernetes-based deployments, the `e2e.sh` setup script creates a
`slapd-external` NodePort service on each cluster:

| Port | NodePort (default) | Target | Purpose |
|---|---|---|---|
| 389 | 30389 | container:1024 | Plain LDAP (test runner access) |
| 636 | 30636 | container:1025 | LDAPS (cross-cluster syncrepl) |

The selector matches all RW pods: `app.kubernetes.io/name: slapd`,
`app.kubernetes.io/instance: slapd`.

Cross-cluster syncrepl uses `ldaps://` with mutual TLS:
- The consumer presents its own site's TLS cert (`tls_cert`, `tls_key`)
- The consumer verifies the provider's cert against the remote site's CA (`tls_cacert`)
- CA cross-trust is established by the setup script (each site's kube CA is distributed to
  all other sites as a `site-<ctx>-ca` Secret)

Other exposure methods (LoadBalancer, VPN, direct routing) work equally well — the
`externalPeers[].uri` in the SlapdCluster CR accepts any routable LDAP URI.

## Read-only replicas

Read-only replicas (`spec.readReplicas`) are pure consumers. They pull from all RW masters
on the same site but have no external peer stanzas and no `olcMultiProvider`. They reject
writes with `LDAP_UNWILLING_TO_PERFORM (53)`.

## Startup and convergence

On fresh deployment, the syncrepl channels take time to establish:

1. StatefulSet ordered rollout: pod-0 starts first, pod-1 after pod-0 is Ready, etc.
2. The operator bootstraps directory entries on pod-0. Pod-1 and pod-2 pull them via syncrepl.
3. External peer syncrepl connections are established after all pods are running.
4. Delta-syncrepl may initially require a full content sync (`SYNC_REFRESH_REQUIRED`) before
   switching to incremental mode. This is transient and self-healing.

Cross-cluster convergence typically takes 10–30s after all pods are ready, depending on
network latency and the initial sync volume.

## Plain syncrepl vs delta-syncrepl

Slaptain uses **delta-syncrepl** (changes sent as diffs via the accesslog). The alternative
is **plain syncrepl** (full entry sent on every modification). The trade-offs:

| | Plain syncrepl | Delta-syncrepl (slaptain) |
|---|---|---|
| Bandwidth | Full entry per change (wasteful for large entries with small modifications) | Only the diff (attribute-level changes) |
| Infrastructure | No accesslog DB needed | Requires accesslog DB + overlay + syncprov on accesslog |
| Accesslog management | N/A | Must configure purge interval to prevent unbounded growth |
| Failure recovery | Full refresh on any gap | Falls back to full refresh only if accesslog is purged past consumer's CSN |
| Complexity | Simpler | More moving parts |

Delta-syncrepl is the better choice for environments with large entries and frequent small
modifications (e.g., updating a single attribute on a user entry). Plain syncrepl is simpler
and may be sufficient for small directories with infrequent changes.
