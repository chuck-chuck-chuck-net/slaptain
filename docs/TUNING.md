# Tuning and sizing a slaptain cluster

An unset field means slaptain's opinion, not slapd's: where upstream's default is
sized for a demo directory and we have a defensible value, we write ours
([ADR-024](adrs/adr-024-tunable-placement.md) R5). Every tunable has a placement
class that decides whether changing it converges on a running cluster or needs the
database rebuilt — and a handful of them can only be *reported*, because slapd
crashes when the operator tries to write them live. The way back to bare OpenLDAP
behaviour is asking for it (`0`, `""`, an explicit number), never leaving the field
unset.

Read this before you size anything larger than a fixture. The values that break a
large directory are structurally invisible in a small one.

## What changed in the defaults

The two tunables batches of 2026-09-12 and 2026-09-13 moved a set of values that
earlier slaptain releases left at slapd's own. Upgrading the operator changes the
behaviour of an existing cluster, in every case towards "works at scale":

| Attribute | Was (slapd's own) | Now (slaptain's default) | Why |
|---|---|---|---|
| `olcSizeLimit` | 500 entries | `unlimited` | A client enumerating a real subtree got a silently truncated answer with a result code most libraries never surface |
| `olcTimeLimit` | 3600 s | `unlimited` | The cap aborted exactly the bulk sweeps a large directory exists to serve |
| `olcLimits` on the replication identity | none (so 500) | `size`/`time` unlimited, soft and hard | A consumer's syncrepl search is an ordinary search: without the exemption a directory stopped replicating past its 500th entry while reporting itself `Synced` ([ADR-020 amendment](adrs/adr-020-accesslog-access-control.md)) |
| `olcDbMaxSize`, data DB | ~10 MB (back-mdb's) | 32Gi | A database that outgrows its map stops accepting writes with `MDB_MAP_FULL` |
| `olcDbMaxSize`, accesslog DB | ~10 MB | 8Gi | A journal that fills stops advancing — and it fills on write *rate*, not data volume |
| `olcDbIndex`, data DB | whatever `spec.indices` said | `objectClass`, `entryCSN`, `entryUUID` always present | `entryCSN` and `entryUUID` are on syncrepl's hot path; unindexed, each is a full scan |
| `olcDbCheckpoint` | unset | `1024 5` data, `2048 15` accesslog | Turning `noSync` on becomes a one-field change that is already safe |
| `olcDbRtxnSize` | 10000 | 10000, written explicitly | Same value; visible in `cn=config` and pinned against a base-image bump |
| `olcSpSessionlog` | unset (off) | 5000 operations, data DB only | A reconnect inside the window is answered from memory instead of a walk of the whole database ([ADR-022](adrs/adr-022-syncprov-sessionlog.md)) |
| syncrepl stanza | no keepalive, no timeouts | `keepalive=240:3:30`, `network-timeout=10 timeout=300` | An idle `refreshAndPersist` connection dropped by a stateful firewall used to look healthy indefinitely |
| `olcLogLevel` | 256 (stats) | 16640 (stats + consumer-side sync) | A replication incident is diagnosed from what slapd logged while it was going wrong |
| `olcTLSProtocolMin` | unset | `3.3` (TLS 1.2 floor) | Unset means "whatever this image's OpenSSL permits", a policy that moves on a base-image bump |
| `olcPasswordHash` | `{SSHA}` compiled in | `{SSHA}`, written | Same value, stated and converged rather than inherited from the build |
| `olcToolThreads` | 1 | 2 | `slapadd` runs during a restore with the cluster scaled to zero, so its runtime is downtime |

`olcDbNoSync` did not change value — it is still `false` — but it changed class:
editing it on a live `SlapdDatabase` used to be a silent no-op and now converges.

## The knobs

### Placement classes

Every field below is one of three ([ADR-024](adrs/adr-024-tunable-placement.md)):

- **converged** — read, compared and written on every pod on every reconcile.
  Edit the CR and it applies. This is the default class.
- **bootstrap-time** — written into a pod's generated config when its `/config`
  volume is first bootstrapped, and never again. Editing the field changes nothing
  on existing pods, and a pod bootstrapped later picks up the new value while its
  peers keep the old one.
- **recreate-required** — written when the database is created, compared
  afterwards, and a divergence is *reported*, never applied.

The third class exists because slapd dies on the live write. An `ldapmodify` of
`olcDbMaxSize` against a running back-mdb database segfaults the process (OpenLDAP
2.7.1, exit 139, captured mid-MOD); LMDB's `mdb_env_set_mapsize` may not run with
transactions active, and a live slapd always has some. `olcDbEnvFlags` fails the
same way once `writemap` enters the list. Editing either field sets
`TunablesConverged=False` on the `SlapdDatabase`, reason `RecreateRequired`, with
the current value, the desired value and the change path in the message. The change
path is always the same: back up, delete the database, restore into a fresh one
([ADR-014](adrs/adr-014-s3-backup-restore.md)).

### Per database — `SlapdDatabase`

| Field | Default | Class |
|---|---|---|
| `spec.maxSize` | 32Gi | **recreate-required** |
| `spec.envFlags` | none | **recreate-required** |
| `spec.sizeLimit` | `unlimited` | converged |
| `spec.timeLimit` | `unlimited` | converged |
| `spec.limits` | none (the replication exemption is prepended and is not yours to set) | converged |
| `spec.noSync` | inherits `SlapdCluster.spec.tuning.noSync`, itself `false` | converged |
| `spec.checkpoint` | `1024 5` | converged |
| `spec.rtxnSize` | 10000 | converged |
| `spec.indices` | none beyond the operator's baseline | converged, **additive only** |
| `spec.replication.syncprovSessionlog` | 5000 | converged |
| `spec.replication.syncprovCheckpoint` | none | written at overlay creation |
| `spec.replication.accesslogPurge` | **none — the journal grows unbounded** | written at accesslog overlay creation |

`spec.maxSize` takes a Kubernetes quantity (`32Gi`) or a bare byte count; the
operator converts. A value it cannot parse is an error, not a fallback.

`syncprovCheckpoint` and `accesslogPurge` are written when the operator creates the
overlay and are not converged afterwards — editing either on a database that already
has its overlays changes nothing, and nothing reports it. That is debt against
ADR-024 R4 — [`docs/BACKLOG.md`](BACKLOG.md) records it for `syncprovCheckpoint`;
`accesslogPurge` has the same shape. Set both when you create the database.

`spec.indices` is additive: the operator adds index definitions the database is
missing and never removes one, because back-mdb rejects a second definition for an
attribute that already has one. Dropping an entry from the list therefore leaves the
index in place. Index changes made through `cn=config` are rebuilt online by
back-mdb — no `slapindex` run, no downtime.

### Per cluster — `SlapdCluster`

| Field | Default | Class |
|---|---|---|
| `spec.tuning.noSync` | `false` | converged (per database) |
| `spec.tuning.toolThreads` | 2 | converged |
| `spec.ldap.tls.protocolMin` | `3.3` (TLS 1.2), written whether or not TLS is on | converged |
| `spec.ldap.tls.cipherSuite` | none — OpenSSL's own list | converged when set |
| `spec.ldap.passwordHash` | `{SSHA}` | converged |
| `spec.logLevel` | 16640 | **rolls the pods** — slapd takes `-d` at startup only |
| `spec.replication.keepalive` | `240:3:30`; `none` disables | converged into the stanzas |
| `spec.replication.retry` | `10 +` | converged into the stanzas |
| `spec.backend.idlExponent` | none — slapd's 16 | **bootstrap-time** |
| `spec.persistence.{config,data,accesslog}.size` | 1Gi each | StatefulSet `volumeClaimTemplates` |

`spec.ldap.passwordHash` governs only what slapd hashes on a client's behalf. The
root passwords the operator generates are hashed by the operator before they reach
slapd. Stronger schemes need their module in the runtime image, and slaptain's does
not ship `pw-argon2` today — asking for `{ARGON2}` produces a slapd that rejects
every password write.

### Deliberately not tunable

- **`olcIdleTimeout` and `olcWriteTimeout`.** slapd's default for both is *never
  close*, so a pod behind a stateful firewall accumulates dead connections until it
  runs out of descriptors. The gap is real and there is no field for it, because an
  `ldapmodify` of either against a running 2.7.1 does not fail and does not crash —
  it **hangs** the process. The CSN is queued and never graduates, the pod answers
  nothing at all, not even an anonymous rootDSE, and `SIGTERM` sticks. Reproduced
  twice on a healthy three-pod cluster; recovery was restarting every pod. Both
  values are fine when they come from the boot config, so the open path is
  bootstrap-time via the init container. Until that lands there is no field: offering
  one the operator cannot honour is what ADR-024 R4 forbids. See
  [`docs/BACKLOG.md`](BACKLOG.md).
- **Thread and buffer counts** (`olcThreads`, `olcListenerThreads`,
  `olcConcurrency`, `olcSockbufMaxIncoming*`, `olcConnMaxPending`). Upstream's
  defaults are defensible and we have no measurement that asks for a different one.
  `spec.tuning` is the home when one arrives.
- **`cn=monitor`.** Implemented and withdrawn the same day: modifying an existing
  monitor database's `olcAccess` hangs slapd with the same signature as the
  connection timeouts. Monitoring today is the operator's CSN polling.
- **The replication identity's DN, its ACL and its limits.** One contract the
  operator owns end to end (ADR-024 R7). A user who cannot change the DN has no
  business capping its searches.

## Sizing: from lab to production

### The map size is a reservation; the volume is the bound

`olcDbMaxSize` is an address-space reservation, not an allocation — LMDB grows the
file sparsely inside it. So the number that actually stops a database is the size of
the `/data` PVC, not `spec.maxSize`. Size the map high and the volume to your data;
the volume is the limit you can see, alert on and expand, and the map size is the one
you cannot change afterwards without recreating the database.

That is why slaptain's default map is 32Gi against a default `persistence.data.size`
of 1Gi (5Gi in the `slapd-cluster` chart, and `tests/values.slapd-persistent.yaml`
takes the chart's value). The fixtures are deliberately lopsided: a lab cluster runs
out of PVC long before it runs out of map, which is exactly the failure you want in a
lab — it is visible, and it is fixable by expanding the volume. In production the same
relationship holds, at different absolute numbers.

Work it in this order:

1. Estimate the directory's byte volume: entries × average entry size, plus the
   indices. Index overhead depends on how many attributes you index and with which
   match types, so measure it on a representative load rather than guessing a ratio —
   an `slapadd` of a sample LDIF into a throwaway database answers it in minutes.
2. Size `persistence.data.size` to that, with headroom for growth and for LMDB's
   free-page churn under write load. This is the number you will revisit.
3. Set `spec.maxSize` comfortably above the largest volume you ever expect to give
   this database — several times over, since the reservation costs address space and
   nothing else. Getting this wrong is the expensive mistake: too low and the fix is a
   backup-delete-restore cycle.

→ Decide `spec.maxSize` once, before the data is loaded. Treat
`persistence.data.size` as the knob you tune afterwards.

### Journal sizing: write rate × purge window

The accesslog journal is not a copy of the directory. It holds change records, and it
only ever needs to hold a purge window's worth of them, so its size follows from write
*rate*, not entry count:

    journal bytes ≈ writes per day × purge window in days × bytes per change record

The operator gives every accesslog DB an 8Gi map and a `2048 15` checkpoint, neither
of them configurable — the journal is a derived, purged artefact, so losing its tail
costs a consumer a full refresh rather than data.

`spec.replication.accesslogPurge` has **no default**. Unset means no purge at all and
a journal that grows until the volume or the map runs out. Set it. The example fixture
uses `"2+00:00 1+00:00"` — purge records older than two days, check daily — which is a
sane starting point for a cluster whose consumers are never offline longer than that.
The window has to cover your worst realistic consumer outage: a consumer whose cookie
predates the purge falls back to a full refresh.

Size `persistence.accesslog.size` from the same arithmetic, with the same headroom
logic as the data volume.

### idlExponent: decide before the data lands

`spec.backend.idlExponent` bounds how many entry IDs one index slot holds before
back-mdb degrades the slot to a range (valid 16–30; slapd's default 16 means 65536
IDs). On a directory where a common index slot exceeds the cap — a single
`objectClass` value shared by a million entries is the classic case — every search
using that slot reads far more candidates than it needs. Raising it a few steps is the
standard large-directory adjustment; the cost is memory per index page.

This one is **bootstrap-time**, and it is the field most likely to bite. It governs
on-disk index layout, so it deliberately carries no operator default: moving one later
would split a cluster into pods bootstrapped before and after the change, with nothing
able to repair the difference. Set it, or don't, before the cluster is created.
Changing it afterwards means recreating the config volumes pod by pod (letting each
re-bootstrap), or rebuilding the cluster and restoring.

### Index strategy

The operator's baseline — `objectClass`, `entryCSN`, `entryUUID`, equality — is not
yours to remove and implements replication, not query performance. `spec.indices` is
where your query patterns go: index the attributes your clients actually filter on,
with the match types they actually use (`uid eq,sub`, `cn eq,sub`). An index that
serves no filter costs write throughput and buys nothing.

### Durability: noSync and the checkpoint

`noSync` disables the per-write fsync and is a cluster-wide posture, not a per-database
detail — set `spec.tuning.noSync` and let databases inherit it; the per-database
`spec.noSync` is the exception, not the entry point. The argument for turning it on is
that the replication mesh plus the delta journal are the redundancy that an fsync
would otherwise provide.

`spec.checkpoint` ("`<kbyte> <min>`", default `1024 5`) is what bounds the window
`noSync` opens. Per `slapd-mdb(5)` it only takes effect while `noSync` is in force,
and the operator writes it unconditionally so that turning `noSync` on is a one-field
change that is already safe.

The one combination the operator refuses is `noSync: true` with `checkpoint: ""`:
nothing schedules a flush, so an unclean shutdown loses an unbounded window of writes.
The `SlapdDatabase` goes to `phase=Error`, reason `UnsafeDurability`, with the fix in
the message.

### Connections and timeouts

What landed is on the syncrepl side: every stanza carries `network-timeout=10` (bounds
the TCP connect and TLS handshake, so a provider whose node is gone is noticed in ten
seconds rather than at the kernel's TCP timeout), `timeout=300` (bounds the bind and
the refresh phase only — verified in the 2.7.1 source, because a wrong reading would
abort every persistent connection), `retry=10 +` and `keepalive=240:3:30`. The
keepalive idle time sits deliberately under the five-minute mark where stateful
firewalls and cloud load balancers commonly drop an idle flow, because a
`refreshAndPersist` connection is idle by design between writes.

What did not land is the client-facing side: slapd will hold a dead client connection
forever, and there is no field for it yet. If your clients sit behind a NAT or a
firewall that drops idle flows, budget file descriptors accordingly and track the
BACKLOG entry.

## Validating a sizing

Fixture-sized coverage cannot see any of this. A single-digit-entry LDIF is how a
500-entry replication cap survived a green test suite for the whole life of the
project.

- `E2E_SCALE=1 ./tests/e2e.sh test <context>` runs the many-entries fixture
  (`tests/e2e/scale_test.go`): a generated seed of 1200 entries (`E2E_SCALE_ENTRIES`)
  and a churn loop of 700 writes (`E2E_SCALE_CHURN`) that pushes the change journal
  past the same cap. It asserts convergence across every RW pod, an unbounded client
  enumeration, the replication identity reading its own journal past 500 records, and
  the structural attributes (map sizes present on both databases, `entryCSN`/
  `entryUUID` indexed). Raise the entry count to approximate your real directory.
- `tests/e2e/tunables_test.go` runs ungated and guards the converged values.
- `slctl inspect -n <ns> <cluster>` is the health check: per-pod LDAP queries plus
  CSN convergence, syncrepl stanza counts, RID uniqueness and external-peer state.
  `--short` for CI; it exits non-zero on a failed check.
- `kubectl get slapddatabase <name> -o yaml` for the `TunablesConverged` condition.
  `False` with reason `RecreateRequired` means a value you edited cannot be applied to
  a live database, and the message carries current, desired and the change path.

## Related

- [ADR-024](adrs/adr-024-tunable-placement.md) — the placement doctrine and both
  amendments (the segfault and the hang).
- [ADR-022](adrs/adr-022-syncprov-sessionlog.md) — the sessionlog's placement and its
  cost model.
- [ADR-020](adrs/adr-020-accesslog-access-control.md) — accesslog access control and
  the limits amendment.
- [ADR-019](adrs/adr-019-per-database-accesslog.md) — one journal per database.
- [OpenLDAP versions](OPENLDAP-VERSIONS.md) — the 2.6/2.7 image pairs. Every live
  modifiability result quoted here was established on 2.7.1.
- [Backup & Restore](BACKUP.md) — the recreate path every bootstrap-time and
  recreate-required tunable points at.
- [`docs/BACKLOG.md`](BACKLOG.md) — what is deliberately not tunable yet, with the
  evidence.
