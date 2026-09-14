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

### Amendment (2026-09-11): the idle-periods premise was wrong — heartbeats stay deferred anyway

The bullet above — "it buys liveness detection only during idle periods, which
is exactly when nothing is at stake" — is refuted. It stays in the text above;
this section is the correction, not a rewrite.

A syncrepl cookie carries one CSN per serverID. `syncprov` cannot look CSNs up
by serverid, so it falls back to the *minimum* CSN across all SIDs as its
accesslog lookup key (`servers/slapd/overlays/syncprov.c`, upstream TODO
around line 3460 in 2.6.13: "dormant serverids in the cluster become mincsns
and more likely to make `syncprov_findcsn(,FIND_CSN,)` fail -> triggering an
expensive refresh"). A dormant SID — a pod or site that has not written in a
long time — pins that minimum arbitrarily old.

Idleness itself breaks nothing while a persistent connection stays up. The
failure fires on reconnection — pod restart, IP change, network blip: if the
provider's accesslog can no longer produce an entry at that old minCSN
(purged by `accesslogPurge`, wiped, or contaminated by a prior refresh), it
answers `err=4096` "sync cookie is stale", the consumer full-refreshes, and
that refresh contaminates its own accesslog for its own consumers in turn. The
loop cascades. This is the same mechanism as the live-measured ITS#9580 storm
in `docs/INVESTIGATION-replication-divergence-after-dataloss-and-restart.md`
(~14k connections in 27 s, hundreds of millicores), not a separate incident.

So the idle/at-stake framing had it backwards: no-traffic is the amplifier —
it is what lets a SID go dormant and its minCSN go stale — reconnect is the
trigger, and an accesslog that can't answer at the old minCSN is the
ammunition. `spec.replication.keepalive` only touches the trigger: TCP
keepalive stops a connection from going silently dead, but advances no CSN,
so it does nothing about the amplifier.

Heartbeats would have worked against this. A per-pod write at an interval
comfortably inside the `accesslogPurge` maxage keeps every SID's CSN findable
in every peer's log — exactly the condition dormancy violates. So the original
reasoning was wrong in the other direction too: this was a real gap, not a
harmless one.

They stay deferred anyway, for different reasons now:

1. We chose two better mitigations instead. The OpenLDAP 2.7 line ships the
   ITS#9580 present-phase cookie-flush fix (MR 472, commit `414866b8`, merged
   2022, first released in 2.7.0 on 2026-08-06) — see ADR-021. The syncprov
   sessionlog's replay path does a per-SID viability check ("SID not present
   == new enough") that neutralizes dormant SIDs for the reconnects it can
   serve — see ADR-022. Both act on the defect's mechanism (the 2.7 fix
   partially, by upstream's own account; the sessionlog as hardening); a
   heartbeat only avoids triggering it.
2. The boundary objection from the original deferral is unchanged: the
   operator would still be writing into the user's data tree.
3. A heartbeat is probabilistic masking of an upstream defect, not a fix for
   it — it lowers the odds a SID goes dormant enough to matter, no more. With
   ADR-021/ADR-022 addressing the defect directly, masking buys little.

If we ever build it regardless: opt-in, one entry per pod, not per site — the
mechanism keys on SID, and RW pods within a site each hold their own — at an
interval well inside `accesslogPurge` maxage.

## Amendment (2026-09-13): the per-database identity is now also the syncrepl stanzas' default

The external syncrepl stanzas derive the same per-database identity this ADR
chose for CSN monitoring — `cn=replication,<suffix>` with the database's own
`replication-password` — whenever `ExternalPeer.bindDN`/`bindPasswordSecretName`
are unset; the peer fields are the explicit override (ADR-011 foreign sources).
See the ADR-019 amendment of the same date for why the identity is per-database
(a cluster-level value spans every SlapdDatabase and can be right for at most
one), and `docs/reconcile-loop-fixes.md` (2026-09-13) for the breakage that
proved it. The uniform-password assumption above carries over unchanged: it now
underwrites the stanzas' default bind, not just monitoring.

## Amendment (2026-09-14): the convergence verdict is per database, and unreadable evidence is its own state

This ADR chose the credentials; the 2026-08-26 and 2026-09-11 amendments bounded
what the comparison can see. Neither said what the unit of comparison *is*, and
the implementation had it wrong: every `(pod × database)` `contextCSN` vector was
flattened into one set-identity comparison.

**A `contextCSN` vector is a property of one database on one pod.** Two databases
have different vectors by construction — independent write histories, disjoint
serverID activity, last writes at unrelated times — so a comparison across
suffixes asks a question with no true answer. On any cluster with two
`SlapdDatabase` CRs the `ReplicationConverged` condition therefore read
`False/CSNsDiverged` permanently, reporting a "lag" that was the age gap between
two databases' last writes. Measured on a healthy three-pod mesh (2026-09-14):
`local CSN divergence: 0.0s lag across 3 pods` while both databases were
byte-identical on all three pods; captured earlier at `372.8s` and `578.7s`, the
latter propagated onto a completed backup as `SourceConverged=False`.

Decisions:

1. **Convergence is judged per database and ANDed.** A database is converged when
   its own readable pods report identical vectors; the cluster condition is the
   AND over databases, naming the offending database(s) and their per-database
   lag. The same grouping is the cross-site baseline: a peer's database is
   compared against *that* database here, never against whichever of ours wrote
   last.

2. **Unreadable evidence never counts toward the good verdict, and does not
   vanish.** Locally, any unreadable `(pod, database)` pair caps the condition at
   `Unknown/CSNQueriesIncomplete`, naming the pairs; an observed divergence still
   wins as `False` (a problem we can see is reported as a problem). Previously a
   failed query only decorated the message with a count, and a run where fewer
   than two readings survived left the previous verdict standing untouched.

3. **Peer-side, "some databases verified, some not" is its own state:
   `PartiallyVerified`.** Per-database CSN queries are independent LDAP
   operations with per-database bind identities (this ADR's amendment of
   2026-09-13), so one database can fail while another on the same host succeeds.
   That is not theoretical: on 2026-09-13 a peer's db1 bound and answered while
   db2's bind returned `err=49` against the same pod, and the peer verdict was
   computed from db1 alone and read `Synced` — the "evidence that degrades to
   empty on a read failure" class. Folding it into `Unreachable` would state
   something false (the peer *was* reached) and would seed a wrong decision in
   anything acting on the field; folding it into `Synced` is the defect. Severity
   order: `Unreachable` (nothing answered) and `Lagging` (a measured lag on a
   database that *was* read) both outrank `PartiallyVerified`, which applies only
   when every verified database is within threshold and at least one database has
   no readable evidence. `lastError` names the unverifiable databases. The
   `connected` derivation is unchanged (`state != Unreachable`), so a
   `PartiallyVerified` peer reads `connected: true` — LDAP-level contact did
   happen.

**What this does not change:** the bounds above stand unaltered. Per-database
equality on an idle database still means "everything replicable has replicated",
not "the link works", and the check remains consumer-side and one-directional —
mesh health is still only readable by reading every site's CR.
