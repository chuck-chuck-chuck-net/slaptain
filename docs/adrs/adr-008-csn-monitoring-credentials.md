# ADR-008: CSN Monitoring Uses Replication Bind Credentials (Uniform Password)

**Status:** Accepted
**Date:** 2026-04-28

## Context

The SlapdCluster controller monitors replication health by querying `contextCSN` on both
local and remote slapd pods. This attribute lives on the data-tree root entry
(e.g. `dc=example,dc=org`). The query must succeed despite ACLs that may deny anonymous
read access.

Three credential strategies were considered for these queries.

## Options Considered

### Option A: Anonymous access

The simplest approach: connect without binding, issue a base-scoped search for `contextCSN`.

**Rejected.** The SlapdDatabase controller manages data-tree ACLs declaratively. There is no
guarantee that anonymous access to operational attributes on the suffix entry is permitted.
In practice, production deployments restrict anonymous access, which causes silent empty
results or `Insufficient Access` errors. This was the original implementation and it failed
in the lab.

### Option B: cn=config admin password (cn=admin,cn=config)

Use the config admin credential (from `<name>-config-password` Secret) which the operator
already has access to.

**Rejected.** `cn=admin,cn=config` is the `olcRootDN` of the `cn=config` database only. It
has no special authority over data databases. OpenLDAP's rootDN bypass is per-database: the
config rootDN bypasses ACLs on `cn=config`, not on `dc=example,dc=org`. Without an explicit
ACL granting it access, queries against the data tree fail with `Insufficient Access` — the
same problem as anonymous.

### Option C: Replication bind DN (cn=replication,<suffix>) — chosen

Use the `cn=replication,<suffix>` DN with the `replication-password` from the
`<dbname>-credentials` Secret. This DN already exists on every pod (created by the
SlapdDatabase controller) and has an explicit ACL:

```
to * by dn.exact="cn=replication,<suffix>" read by * break
```

This grants read access to all attributes in the data tree, including `contextCSN`.

## Decision

CSN monitoring queries bind as `cn=replication,<suffix>` using the `replication-password`
from the local `<dbname>-credentials` Secret. The same credentials are used for both local
pod queries (via headless DNS) and remote peer queries (via Multus IPs or URIs).

**Uniform-password assumption:** the operator assumes that all sites sharing a replication
relationship use the same `replication-password`. This holds because:

1. The `<dbname>-credentials` Secret is create-only — the SlapdDatabase controller generates
   it once and never rotates it.
2. In the standard multisite setup, the Secret is copied between clusters (or created from
   the same source) before deploying the SlapdDatabase CR.
3. The `cn=replication` entry's `userPassword` hash is replicated between sites via
   delta-syncrepl, so all pods end up with the same hash regardless of which site created
   the entry first.

The `ExternalPeer` type carries `bindDN` and `bindPasswordSecretName` fields for future use.
When these are set, the CSN monitoring code should prefer them over the SlapdDatabase-derived
credentials. This override path is not yet implemented — it will be needed if/when
heterogeneous replication passwords become a requirement (e.g. credential rotation,
federation with third-party LDAP servers).

## Consequences

- **Simplicity:** No additional Secrets or configuration needed for CSN monitoring — it
  reuses credentials that already exist for syncrepl itself.
- **Race during bootstrap:** The `cn=replication` entry may not exist on all remote pods
  when the first CSN check runs (the SlapdDatabase controller creates it asynchronously).
  This produces transient `LDAP Result Code 49 "Invalid Credentials"` errors. The periodic
  requeue (60s) ensures the check self-heals once the entry propagates.
- **Credential scope is minimal:** `cn=replication` has read-only access to the data tree.
  It cannot modify data, cannot access `cn=config`, and cannot escalate privileges.
- **Future work:** Wire up `ExternalPeer.bindDN` / `bindPasswordSecretName` as an override
  for remote peers when the uniform-password assumption no longer holds.

## Related

- ADR-002: cn=config is node-local, operator-managed
- ADR-003: Operator owns all syncrepl configuration
- ADR-007: Multus replication network (TLS + bind credential layering, §7)
