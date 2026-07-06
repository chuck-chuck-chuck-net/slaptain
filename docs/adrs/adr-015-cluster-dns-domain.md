# ADR-015: The cluster DNS domain is resolved, never hardcoded

**Status:** Accepted
**Date:** 2026-07-05

## Context

slaptain builds pod FQDNs in three places that all cross the network:

1. **serverID URLs** — `bootstrap.sh` writes `olcServerID <id> ldaps://<pod>.<headless>.<ns>.svc.<domain>:1025` per RW pod. At startup slapd self-identifies by matching one of these against its own listen addresses; the match succeeds off `/etc/hosts` alone (kubelet writes the pod's own FQDN there), needing no DNS and no readiness — **provided the domain in the URL equals the pod's real domain.**
2. **Operator → pod LDAP connections** — the SlapdCluster / SlapdDatabase / SlapdSchema controllers dial each pod by FQDN over the headless service to manage cn=config (ACLs, schemas, overlays, syncrepl stanzas, CSN checks).
3. **syncrepl provider URIs** — the `olcSyncRepl` stanzas name peer providers by FQDN (except where a Multus replication-network plain IP is substituted, ADR-007).

Every one of these baked in `svc.cluster.local`. That is only the *default* Kubernetes DNS domain; a cluster can be installed with any domain (e.g. `k8s.example`). On such a cluster:

- The serverID URL resolves to neither `/etc/hosts` (which holds the real `…svc.k8s.example` FQDN) nor CoreDNS (authoritative for `k8s.example`, forwards `cluster.local` upstream → NXDOMAIN). slapd logs `read_config: no serverID / URL match found` and exits → CrashLoopBackOff.
- Even past that, the operator's per-pod connections resolve to NXDOMAIN, and the TLS cert SANs (already minted for the real domain by `tests/gencert.sh`) would not match a `cluster.local` name anyway.

The bug hid for a long time because every prior multi-replica e2e ran on default-domain clusters, where the hardcoded value happened to be correct. The single-cluster/no-Multus path on a non-default domain was the first to exercise the wrong assumption.

## Options considered

- **Keep hardcoding `cluster.local`.** Rejected: silently broken on any non-default-domain cluster, which is a supported Kubernetes configuration and the reality of this lab.
- **Config-only (`spec`/Helm value, no discovery).** Rejected as the sole mechanism: correct but hostile UX — every deployer on a non-default domain must know to set it, and forgetting reproduces the exact crashloop with no hint.
- **Discovery-only (parse resolv.conf).** Rejected as the sole mechanism: no escape hatch for running the operator off-cluster (`make run`) or on non-standard resolver setups.
- **Bare per-pod serverID (`olcServerID: <id>`, no URL).** Viable for serverID alone (cn=config is node-local per ADR-002, so a pod only needs its own ID) and would drop the serverID DNS dependency entirely — but it does nothing for cases 2 and 3, which need the real domain regardless. Noted as a possible future simplification, not the fix.

## Decision

The operator resolves the cluster DNS domain **once at startup** and threads it everywhere a pod FQDN is built. Resolution precedence (`ResolveClusterDomain`):

1. `CLUSTER_DOMAIN` environment variable (explicit override; Helm value `clusterDomain`).
2. The `svc.<domain>` entry in the operator pod's `/etc/resolv.conf` search list — kubelet injects this into every container regardless of base image. Mirrors the discovery already in `tests/gencert.sh`, so cert SANs and operator FQDNs agree.
3. `cluster.local` (default).

The resolved value is injected into the SlapdCluster/SlapdDatabase/SlapdSchema reconcilers (a `ClusterDomain` field) and passed to the init container as `LDAP_CLUSTER_DOMAIN` for the serverID URL. `bootstrap.sh` defaults `${LDAP_CLUSTER_DOMAIN:-cluster.local}` so an older/standalone init image still works.

**Rule for future code: never write `svc.cluster.local` (or any literal domain). Use the reconciler's `ClusterDomain`, or thread it as a parameter into free functions.**

## Consequences

- Zero-config on any cluster; explicit override for off-cluster runs and odd resolvers.
- The default stays `cluster.local`, so all existing default-domain deployments (including every prior Multus run) are unaffected — this is a strict superset of the old behavior.
- A non-default-domain cluster (`k8s.example`) becomes a standing regression test for the domain path; the default path stays covered by every other cluster.
- One known gap left as a follow-up: `tests/e2e-migration.sh` (gated `E2E_MIGRATION=1`) still hardcodes `cluster.local` for the fake-prod fixture URI. It only matters for the migration e2e on a non-default-domain cluster.

## Related

- ADR-002: cn=config is node-local (why each pod carries its own serverID and the operator manages each pod individually by FQDN)
- ADR-003: Operator owns all syncrepl configuration (the FQDNs in stanzas are operator-generated)
- ADR-007: Multus replication network (plain-IP substitution in syncrepl provider URIs — the one place a FQDN is *not* used)
- ADR-011: Hot migration topology (serverID coordination; the URL-form serverID whose self-match depends on the correct domain)
