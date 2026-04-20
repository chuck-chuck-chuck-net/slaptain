# ADR-007: Multus-Based Dedicated Replication Network for Cross-Site Traffic

**Status:** Proposed
**Date:** 2026-04-20

## Context

Cross-cluster replication (Phase 3, ADR-003) currently relies on NodePort services for
cross-site connectivity. Each site exposes slapd via a NodePort (e.g., `slapd-external` on
port 30636), and `ExternalPeer` URIs point at node IPs: `ldaps://<NodeIP>:30636`.

This works but has significant drawbacks:

1. **No traffic isolation.** Replication traffic shares the primary cluster network with all
   other workloads. Replication bursts (initial sync, bulk updates) compete with service traffic.
2. **NodePort limitations.** Port range (30000–32767) is shared and finite. Per-pod access
   requires additional per-pod NodePort services (30400+ordinal hack in `e2e-multisite.sh`).
3. **Indirect routing.** Traffic flows: pod → kube-proxy/iptables → NodePort → node → pod.
   This adds latency and makes troubleshooting harder.
4. **No physical isolation.** A NIC failure or network saturation on the primary interface
   affects both service traffic and replication.

This ADR proposes extending slaptain to natively support Multus-attached replication networks
for cross-site traffic, eliminating the NodePort dependency.

## Decision

### 1. Operator Injects Multus Annotation on Pod Template

When `spec.replication.network` is configured on a SlapdCluster, the operator adds the Multus
network annotation to the StatefulSet pod template:

```yaml
metadata:
  annotations:
    k8s.v1.cni.cncf.io/networks: infra/replication-net
```

The annotation references a `NetworkAttachmentDefinition` (NAD). The operator does **not**
create or manage NADs — they are infrastructure-level resources managed by the platform team
(same as StorageClasses or CertManagers).

**NAD scoping:** Multus does not have a cluster-scoped NAD resource — all NADs are
namespace-scoped. However, a replication network is physical infrastructure shared across
workloads and namespaces, so duplicating the NAD per namespace is wrong. Instead, use
Multus's cross-namespace reference: create one NAD in a shared namespace (e.g., `infra`)
and reference it as `infra/replication-net`. This works by default when Multus runs with
`namespaceIsolation: false` (the default). With `namespaceIsolation: true`, the NAD's
namespace must be listed in `--global-namespaces`.

The `multusNetwork` field accepts both `<name>` (same namespace) and `<namespace>/<name>`
(cross-namespace) formats. Cross-namespace is the recommended pattern.

The annotation on the pod template does NOT include static IPs. IP assignment is the
responsibility of the NAD's IPAM plugin (see §5 below). The operator is IPAM-agnostic.

### 2. Operator Discovers Multus IPs from Pod Status

After Multus assigns a `net1` interface, it writes the result to a pod annotation:

```yaml
k8s.v1.cni.cncf.io/network-status: |
  [{
    "name": "cbr0",
    "interface": "eth0",
    "ips": ["10.244.1.5"],
    "default": true
  }, {
    "name": "slaptain-testing/replication-net",
    "interface": "net1",
    "ips": ["192.168.99.10"]
  }]
```

The SlapdDatabase controller (which manages syncrepl stanzas per ADR-003) reads each pod's
`network-status` annotation to discover `net1` IPs. It matches entries by NAD name
(`spec.replication.network.multusNetwork`).

### 3. In-Cluster Syncrepl Uses Replication Network (Optional)

When `spec.replication.network.useForInCluster` is true, in-cluster syncrepl stanzas use the
discovered Multus IPs instead of headless DNS names:

```
# Before (headless DNS):
provider=ldaps://slapd-1.slapd-headless.ns.svc.cluster.local:1025

# After (Multus IP):
provider=ldaps://192.168.99.11:1025
```

When false (default), in-cluster replication continues to use headless DNS. Cross-site
replication always uses the Multus network when configured.

This is opt-in because:
- Headless DNS is inherently stable (doesn't depend on IPAM).
- In-cluster latency via Cilium is already excellent.
- Mandating Multus for in-cluster traffic adds a hard dependency on the secondary network
  for basic cluster operation.

### 4. ExternalPeers Extended for Per-Pod Addressing

The current `ExternalPeer` model uses a single `URI` per remote site:

```yaml
externalPeers:
  - name: site-b
    uri: "ldaps://10.0.1.50:30636"  # NodePort — hits any pod
```

This works with NodePort (load-balanced across pods) but doesn't map well to direct
pod-to-pod Multus connectivity. With the replication network, each remote pod has a known,
stable IP. The operator should create a syncrepl stanza per remote pod, not per site.

`ExternalPeer` gains a `podAddresses` field, mutually exclusive with `uri`:

```yaml
externalPeers:
  - name: site-b
    podAddresses:          # Each remote pod's replication-network IP
      - "192.168.99.128"   # site-b slapd-0
      - "192.168.99.129"   # site-b slapd-1
    port: 1025             # Container port (not NodePort)
    tlsSecretName: site-b-ca
    bindDN: "cn=replication,dc=chuck-chuck-chuck,dc=net"
    bindPasswordSecretName: shared-repl-creds
```

**RID allocation for per-pod external peers:** Each address in `podAddresses` gets its own
RID. The existing scheme (`ridBase + 50 + j + 1` where j is the external peer index) extends
naturally: peer 0 address 0 → `ridBase+51`, peer 0 address 1 → `ridBase+52`, etc. For
multiple external peer entries, the second peer's addresses start after the first's.

**Backward compatibility:** The single `uri` field continues to work unchanged. Clusters not
using Multus keep their current NodePort-based setup.

### 5. IPAM Strategy: Start Simple, Evolve

The operator is deliberately IPAM-agnostic — it discovers IPs from pod status, not from
configuration. This decouples the IP assignment strategy from the operator implementation.

**Phase 1 (now):** `host-local` IPAM with per-node range partitioning. IPs are semi-stable
(same pod gets the same IP if it restarts on the same node before the IP is reclaimed).
For single-node-per-site setups (home lab), this is effectively deterministic. For
multi-node setups, IPs may change on pod rescheduling → the operator detects the change via
network-status annotation and updates syncrepl stanzas.

The `ExternalPeer.podAddresses` are manually maintained. When a remote pod's Multus IP
changes (rare with `host-local` and stable scheduling), the remote site operator must update
the `podAddresses` on the local SlapdCluster and vice versa.

**Phase 2 (future):** `whereabouts` IPAM for cluster-wide allocation with persistent IPs.
Eliminates per-node range partitioning. IPs are stable across rescheduling.

**Phase 3 (future, speculative):** Dynamic DNS registration — a controller watches Multus
network-status annotations and creates DNS A records. `ExternalPeer` URIs use hostnames
instead of IPs, decoupling the operator from IP management entirely.

### 6. slapd Requires No Changes

slapd binds to `0.0.0.0:1024` (LDAP) and `0.0.0.0:1025` (LDAPS). It accepts connections on
any interface, including `net1`. No slapd configuration, image, or init container changes are
needed.

### 7. TLS: CA Verification Without IP SANs

IP SANs cannot use wildcards (RFC 5280), and Multus IPs are not known until pods are running
— after the TLS cert must already exist. Rather than requiring IPs upfront (which would need
either brittle IP anticipation or a mutating webhook for per-pod static IPs), the operator
sets `tls_reqcert=allow` on syncrepl stanzas whose provider URI is an IP address.

This means: the CA chain is verified (the peer's cert is signed by a trusted CA), but the
IP is not matched against the cert's SANs. The connection is still encrypted and
CA-authenticated. Combined with bind credentials (`binddn` + `credentials`), this provides
three layers of authentication: CA trust, TLS encryption, and LDAP bind — on a physically
isolated network.

`tls_reqcert=allow` is only added for IP-based provider URIs (Multus). DNS-based URIs
(headless service names, external hostnames) continue to use the default `tls_reqcert=demand`
with full hostname verification.

## CRD Changes

### SlapdCluster

```go
type SlapdReplicationConfig struct {
    Enabled       bool                       `json:"enabled,omitempty"`
    ExternalPeers []ExternalPeer             `json:"externalPeers,omitempty"`
    Network       *ReplicationNetworkConfig  `json:"network,omitempty"`      // NEW
    Keepalive     string                     `json:"keepalive,omitempty"`
    Retry         string                     `json:"retry,omitempty"`
}

// ReplicationNetworkConfig configures a dedicated replication network via Multus.
type ReplicationNetworkConfig struct {
    // multusNetwork is the NetworkAttachmentDefinition reference.
    // Supports cross-namespace format: "namespace/name" (recommended) or plain "name"
    // (same namespace as SlapdCluster). The NAD must exist before the SlapdCluster is created.
    MultusNetwork string `json:"multusNetwork"`
    // useForInCluster controls whether in-cluster syncrepl uses the Multus network
    // instead of headless DNS. Default false.
    // +kubebuilder:default=false
    UseForInCluster bool `json:"useForInCluster,omitempty"`
}

type ExternalPeer struct {
    Name                   string   `json:"name"`
    URI                    string   `json:"uri,omitempty"`           // Existing (NodePort/LB mode)
    PodAddresses           []string `json:"podAddresses,omitempty"` // NEW (Multus mode)
    Port                   int32    `json:"port,omitempty"`          // NEW (default 1025)
    TLSSecretName          string   `json:"tlsSecretName,omitempty"`
    BindDN                 string   `json:"bindDN,omitempty"`
    BindPasswordSecretName string   `json:"bindPasswordSecretName,omitempty"`
}
```

### Validation

- `uri` and `podAddresses` are mutually exclusive on each ExternalPeer.
- `port` defaults to 1025 when `podAddresses` is set.
- `network.multusNetwork` is required when any ExternalPeer uses `podAddresses`.

### Status

```go
type SlapdClusterStatus struct {
    // ... existing fields ...
    // replicationNetworkIPs reports the discovered Multus IPs per pod.
    // Key: pod name, Value: IP on the replication network.
    ReplicationNetworkIPs map[string]string `json:"replicationNetworkIPs,omitempty"`
}
```

## Operator Implementation Changes

### SlapdCluster Controller

1. **`reconcileStatefulSet`**: When `spec.replication.network` is set, add the Multus
   annotation `k8s.v1.cni.cncf.io/networks: <multusNetwork>` to the pod template metadata.

2. **`reconcileStatus`**: Read each pod's `k8s.v1.cni.cncf.io/network-status` annotation,
   extract the `net1` IP matching the configured NAD name, and populate
   `status.replicationNetworkIPs`.

### SlapdDatabase Controller

3. **`buildSyncreplStanzas` (in-cluster)**: When `useForInCluster` is true and the pod's
   Multus IP is known (from SlapdCluster status), use `ldaps://<multusIP>:1025` instead of
   `ldaps://<pod>.<headless>.<ns>.svc.cluster.local:1025`.

4. **`buildSyncreplStanzas` (external)**: When an ExternalPeer has `podAddresses`, generate
   one syncrepl stanza per address (instead of one per ExternalPeer). RID allocation:
   `ridBase + 50 + (peerOffset + addressIndex) + 1`.

5. **Requeue on IP change**: If a pod's discovered Multus IP changes (detected during
   reconcile), the SlapdDatabase controller must rewrite syncrepl stanzas on all pods that
   reference the changed IP.

### ExternalPeerStatus

6. **Connectivity checks**: When `podAddresses` is used, test connectivity to each individual
   pod address (not just a single URI). Report per-address status.

## Example: Two-Site Home Lab with Multus

### Prerequisites (platform team)

Deploy Multus and a single NAD per cluster in a shared namespace (this is infrastructure,
not slaptain's concern):

```yaml
# On both clusters — ONE NAD in a shared namespace, used by all workloads:
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: replication-net
  namespace: infra          # shared namespace, not per-workload
spec:
  config: |
    {
      "cniVersion": "0.3.1",
      "type": "macvlan",
      "master": "enp194s0",
      "mode": "bridge",
      "ipam": {
        "type": "host-local",
        "subnet": "192.168.99.0/24",
        "rangeStart": "192.168.99.10",
        "rangeEnd": "192.168.99.50"
      }
    }
```

All workloads (LDAP, Galera, Cassandra) in any namespace reference this single NAD via
`infra/replication-net`. The IPAM pool is shared — no per-namespace CIDR slicing needed.
With `host-local`, uniqueness is per-node. With `whereabouts` (future), uniqueness is
cluster-wide.

### Site A (t3e cluster)

```yaml
apiVersion: ldap.chuck-chuck-chuck.net/v1alpha1
kind: SlapdCluster
metadata:
  name: slapd
  namespace: slaptain-testing
spec:
  replicas: 2
  replication:
    enabled: true
    network:
      multusNetwork: infra/replication-net
    externalPeers:
      - name: site-b
        podAddresses:
          - "192.168.99.128"   # bento slapd-0
          - "192.168.99.129"   # bento slapd-1
        port: 1025
        tlsSecretName: site-b-ca
        bindDN: "cn=replication,dc=chuck-chuck-chuck,dc=net"
        bindPasswordSecretName: shared-repl-creds
  # ... images, tls, persistence as before
```

### Site B (bento cluster)

```yaml
apiVersion: ldap.chuck-chuck-chuck.net/v1alpha1
kind: SlapdCluster
metadata:
  name: slapd
  namespace: slaptain-testing
spec:
  replicas: 2
  replication:
    enabled: true
    network:
      multusNetwork: infra/replication-net
    externalPeers:
      - name: site-a
        podAddresses:
          - "192.168.99.10"    # t3e slapd-0
          - "192.168.99.11"    # t3e slapd-1
        port: 1025
        tlsSecretName: site-a-ca
        bindDN: "cn=replication,dc=chuck-chuck-chuck,dc=net"
        bindPasswordSecretName: shared-repl-creds
  # ...
```

### What Changes vs. Current NodePort Setup

| Aspect | Current (NodePort) | Multus |
|---|---|---|
| Cross-site URI | `ldaps://NodeIP:30636` | `ldaps://192.168.99.128:1025` |
| Service needed | `slapd-external` NodePort | None (direct pod-to-pod) |
| Traffic path | pod → kube-proxy → NodePort → node → pod | pod `net1` → wire → pod `net1` |
| Traffic isolation | None | Physical (dedicated NIC) |
| Granularity | Per-site (LB across pods) | Per-pod (true mesh) |
| slapd changes | None | None |
| TLS cert SANs | Node IPs + DNS | Multus IPs + DNS |

## Alternatives Considered

### A. Operator Manages Static IPs via ipBase + Ordinal

The operator computes `ipBase + podOrdinal` and injects static IPs into the Multus
annotation:

```yaml
k8s.v1.cni.cncf.io/networks: '[{"name":"replication-net","ips":["192.168.99.10"]}]'
```

**Rejected because:** StatefulSet pod templates are identical for all pods. The operator
cannot set different annotations per pod via the template. Workarounds (mutating webhook,
post-creation pod patching) add complexity that contradicts the "start simple" goal. The
discovery-based approach (read IPs from network-status) achieves the same result without
fighting the StatefulSet model.

### B. Keep Single URI per ExternalPeer, Point at a Multus VIP

Deploy a load balancer (e.g., MetalLB) on the replication network to provide a virtual IP
that distributes across pods.

**Rejected because:** Adds another component (LB on the replication network), and
load-balanced syncrepl is suboptimal — each consumer should connect to a specific provider
for delta-syncrepl to work correctly. The per-pod addressing model aligns with how syncrepl
actually works.

### C. Per-Namespace NADs with Sub-CIDR Slicing

Create a separate NAD per namespace, each with its own IPAM range carved from the
replication subnet (e.g., `.10-.50` for slaptain, `.60-.100` for galera).

**Rejected because:** A replication network is physical infrastructure — duplicating the NAD
per namespace is the wrong abstraction. It also forces pre-allocation of IP ranges per
namespace, which is fragile and wastes address space. Multus's cross-namespace NAD reference
(`namespace/name`) avoids the duplication entirely. All workloads share one NAD and one IPAM
pool; uniqueness is handled by the IPAM plugin, not by namespace isolation.

### D. In-Cluster Replication Always Uses Multus

Remove the `useForInCluster` toggle and always use Multus IPs for in-cluster syncrepl
when a replication network is configured.

**Rejected (for now) because:** This makes the replication network a hard dependency for
cluster operation. If the secondary NIC fails, both cross-site AND in-cluster replication
break. Keeping in-cluster replication on headless DNS by default provides resilience: local
HA survives a replication network outage.

## Consequences

- (+) Replication traffic is physically isolated from service traffic.
- (+) No NodePort services needed for cross-site replication.
- (+) Direct pod-to-pod connectivity — lower latency, simpler troubleshooting.
- (+) Per-pod addressing matches syncrepl's pull-based model (each consumer connects to
  specific providers).
- (+) Backward compatible — existing NodePort-based ExternalPeers continue to work.
- (+) slapd requires zero changes.
- (+) No IP SANs needed in TLS certs — `tls_reqcert=allow` for IP-based providers means
  Multus IPs don't need to be known at cert generation time. Pure post-deploy discovery.
- (-) `ExternalPeer.podAddresses` must be updated when remote pod IPs change.
  Acceptable at current scale; future IPAM improvements (whereabouts, DNS) will address this.
- (-) NADs must exist before SlapdCluster creation — the operator cannot validate this at
  admission time (NAD is in a different API group).
- (-) In-cluster Multus usage (`useForInCluster`) requires stable IPAM; if IPs change,
  there's a brief replication interruption while the operator rewrites stanzas.

## Related

- ADR-003: Operator owns all syncrepl configuration (mechanism for writing stanzas)
- ADR-004: Multi-resource CRD architecture (SlapdDatabase controller manages syncrepl)
