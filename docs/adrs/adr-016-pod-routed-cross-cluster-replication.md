# ADR-016: Direct native pod-IP routing for cross-cluster replication

**Status:** Proposed
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
