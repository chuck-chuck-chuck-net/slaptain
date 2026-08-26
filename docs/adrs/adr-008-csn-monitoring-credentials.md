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

## Amendment (2026-08-26): what CSN comparison can and cannot observe

This ADR settled *which credentials* the CSN check binds with. It did not state the
limits of CSN comparison as a health signal, and those limits caused real confusion
while debugging a cross-site failure. Recording them so the next reader does not
have to re-derive them — and does not mistake the check for something it is not.

**The check is consumer-side and one-directional.** `checkPeerCSNConvergence` runs
on *our* operator, queries the peer's `contextCSN` over the addresses *we*
discovered, and compares the newest remote CSN against our own newest. That answers
"am I current with respect to this peer". It does **not** answer "is this peer
current with respect to me". Nothing here is fixable: delta-syncrepl is pull-based
and a provider keeps no consumer registry, so a provider structurally cannot observe
that a consumer stopped consuming from it. Only the consumer can.

The practical consequence is that **mesh health is not readable from one site**. In
a three-site mesh, site A's `status.externalPeerStatuses` describes A's *inbound*
links. If B has stopped consuming from A, that shows up on **B**, as B's peer entry
for A — and A will correctly and simultaneously report `Synced`. Both are right.
Anything that reports on the mesh as a whole has to read every site's CR. (Our own
e2e cannot: the runner's kubeconfig is minified to the first context, which is why
`tests/e2e/helpers_test.go` uses a functional write-here-read-there probe instead of
peer status.)

**Lag is only observable while writes are flowing.** `csnSyncThreshold` is 5 s, so
under traffic a broken link is reported as `Lagging` quickly. On an *idle* database
neither side's CSN advances, the difference is zero, and the state reads `Synced`
across an arbitrarily broken link. This is not a false reading — everything that
could be replicated has been — but it means the field cannot be used as a liveness
signal on a quiet directory. The first symptom of a break that began during an idle
period is a stale read after somebody finally writes.

**`Unreachable` means something narrower than it sounds.** It is set when *every*
remote CSN query failed. Because those queries use `status.DiscoveredAddresses`, the
same list the syncrepl stanzas are built from, stale addresses do surface here — a
peer whose addresses have gone stale (ADR-016 amendment of the same date) reports
`Unreachable` on the side that holds them. That is the useful half of the signal, and
it is the half that lives on the consumer.

### Considered and deferred: heartbeat writes

The only way to prove the whole path end-to-end while idle is to create traffic — a
per-site canary entry, rewritten on an interval, so each direction becomes
independently measurable. Deferred rather than rejected on technical grounds:

- it would have the operator write into the **user's data tree**, a boundary this
  project has deliberately not crossed ("directory content beyond what
  `SlapdDatabase` seeds is the user's responsibility");
- every heartbeat is a real write — journalled in the accesslog (ADR-019),
  replicated to every peer, present in every backup, forever, on a directory that
  may be idle by design, and interacting with `accesslogPurge` retention;
- it buys liveness detection only during idle periods, which is exactly when nothing
  is at stake.

If it is ever wanted it should be **opt-in**, and one entry per site (each site
writing its own) so that every direction is separately observable rather than a
single shared entry whose ownership is ambiguous under multi-master.

Cheaper and strictly-better-value alternatives, tracked in `docs/BACKLOG.md`:
comparing the addresses a stanza actually names against the addresses currently
discovered — both already known locally, no writes, and it catches the stale-address
class even while idle.
