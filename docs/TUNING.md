# Tuning and sizing a slaptain cluster

An unset field means slaptain's opinion, not slapd's: where upstream's default is
sized for a demo directory and we have a defensible value, we write ours
([ADR-024](adrs/adr-024-tunable-placement.md) R5). Every tunable has a placement
class that decides whether changing it converges on a running cluster, waits for the
pod's next restart, or needs the database rebuilt — and a handful of them can only
be *reported*, because slapd crashes when the operator tries to write them live. The way back to bare OpenLDAP
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
| syncrepl stanza | no keepalive, no timeouts | `keepalive=240:3:30`, `network-timeout=10` | An idle `refreshAndPersist` connection dropped by a stateful firewall used to look healthy indefinitely |
| `olcLogLevel` | 256 (stats) | 16640 (stats + consumer-side sync) | A replication incident is diagnosed from what slapd logged while it was going wrong |
| `olcTLSProtocolMin` | unset | `3.3` (TLS 1.2 floor) | Unset means "whatever this image's OpenSSL permits", a policy that moves on a base-image bump |
| `olcPasswordHash` | `{SSHA}` compiled in | `{SSHA}`, written | Same value, stated and converged rather than inherited from the build |
| `olcToolThreads` | 1 | 2 | `slapadd` runs during a restore with the cluster scaled to zero, so its runtime is downtime |

`olcDbNoSync` did not change value — it is still `false` — but it changed class:
editing it on a live `SlapdDatabase` used to be a silent no-op and now converges.

## The knobs

### Placement classes

Every field below is one of four ([ADR-024](adrs/adr-024-tunable-placement.md)):

- **converged** — read, compared and written on every pod on every reconcile.
  Edit the CR and it applies. This is the default class.
- **bootstrap-time** — written into a pod's generated config when its `/config`
  volume is first bootstrapped, and never again. Editing the field changes nothing
  on existing pods, and a pod bootstrapped later picks up the new value while its
  peers keep the old one.
- **startup-only** — written into a pod's config by the init container at *every*
  pod start, offline, while nothing holds the configuration open. Editing the field
  changes nothing until a pod restarts; the fleet converges one restart at a time,
  and the operator reports which pods are still waiting
  ([ADR-034](adrs/adr-034-startup-only-configuration.md)).
- **recreate-required** — written when the database is created, compared
  afterwards, and a divergence is *reported*, never applied.

The last class exists because slapd dies on the live write. An `ldapmodify` of
`olcDbMaxSize` against a running back-mdb database segfaults the process (OpenLDAP
2.7.1, exit 139, captured mid-MOD); LMDB's `mdb_env_set_mapsize` may not run with
transactions active, and a live slapd always has some. `olcDbEnvFlags` fails the
same way once `writemap` enters the list. Editing either field sets
`TunablesConverged=False` on the `SlapdDatabase`, reason `RecreateRequired`, with
the current value, the desired value and the change path in the message. The change
path is always the same: back up, delete the database, restore into a fresh one
([ADR-014](adrs/adr-014-s3-backup-restore.md)).

Bootstrap-time and startup-only read alike from the CR — in both, an edit does
nothing right now — and cost very differently. A bootstrap-time value is written
once, into a config volume being created for the first time, so two pods on either
side of an edit disagree forever and the way back is a recreate. A startup-only
value is rewritten at every pod start, so the way back is a restart.

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
| `spec.replication.syncprovSessionlogSource` | `InMemory` | **startup-only** |
| `spec.replication.syncprovCheckpoint` | `100 10` | converged |
| `spec.replication.accesslogPurge.enabled` | `true` | converged |
| `spec.replication.accesslogPurge.retention` | `168h` (7 days) | converged |

`spec.maxSize` takes a Kubernetes quantity (`32Gi`) or a bare byte count; the
operator converts. A value it cannot parse is an error, not a fallback.

`syncprovCheckpoint` and `accesslogPurge` are converged on every reconcile: editing
either reaches pods whose overlays already exist, and clearing the field restores the
operator's default. Both were verified live-modifiable on a running slapd before being
converged at all — replace and delete, no crash, no hang — which is the precondition
ADR-024 now puts on this class. (They used to be written only at overlay creation,
so edits were silently ignored; that debt is paid.)

### Both syncprov checkpoints default on, and not to the same value

`syncprovCheckpoint` is the **data** database's `olcSpCheckpoint`;
`accesslogSyncprovCheckpoint` is the **accesslog** database's. They default to
`100 10` and `1 1` respectively, and the difference is not taste (ADR-045).

A `contextCSN` reaches disk by three paths: a checkpoint, a clean shutdown, and
— on a data database only — the pod's own consumer side, which rewrites the whole
vector on every inbound cookie advance. A log database has no consumer, so for it a
checkpoint and a clean stop are the only two, and the damage from having neither is
permanent: that is ITS#9398, which is why its default is as tight as `1 1` can make
it and why that value is not a dial.

The data database's staleness is bounded and self-healing under replication, so it
takes the ecosystem's `100 10` rather than paying `1 1` on the hot write path. It is
still a default rather than nothing, because unset is not slapd's own behaviour: the
startup scan upstream documents as the repair (`FIND_MAXCSN`) is gated
`!SLAP_DBCLEAN`, and back-mdb — the only backend slaptain ships — sets that flag
unconditionally. The scan never runs. Measured on a single node after a `SIGKILL`:
with no checkpoint `contextCSN` was absent on disk and regenerated at restart *ahead
of all content*. On a **non-replicated** cluster there is no third path to repair
that, and such a pod will later discard its own entries when a peer replays them.

Opt out of either with the sentinel:

```yaml
spec:
  replication:
    syncprovCheckpoint: none            # restores slapd's own behaviour
```

`1 1` on a data database is neither forbidden nor recommended. The window above
`<ops> = 1` is open by design anyway — checkpoints are only tested after a
successful write, so the last write before an idle period is never flushed at any
setting — and closing it costs a write amplification nobody has measured on the data
path. Whoever needs it measures it first (ADR-039).

### `olcAccessLogPurge` has three owners, and you set two of them

`logpurge <age> <interval> [<phase>]` is one slapd directive and three quantities
with three different owners (ADR-024 R6). slaptain splits them accordingly, and the
value you read back in `cn=config` is one nobody typed:

```
olcAccessLogPurge: 7+00:00:00 18:00:00 06:00:00
                   ^retention ^interval ^phase
```

| quantity | what it decides | where you set it |
|---|---|---|
| retention (`age`) | how far behind a consumer of **this** database may fall | `SlapdDatabase.spec.replication.accesslogPurge.retention` |
| interval | how often the purge cost is paid — and the cost is the **pod's**, since all of a pod's databases purge in one window | `SlapdCluster.spec.accesslogPurge.interval`, or derived from `spec.rotation` |
| phase | which pod pays it when | derived per pod; nobody declares it |

So: **what to keep is the database's, when to purge is the cluster's.** A database
cannot declare an interval, which is what makes a mode-dependent field — and the
outage it generated — impossible rather than merely discouraged.

`enabled` sits beside the *action*, not beside the duration, on purpose:
`accesslogRetention: {enabled: false}` would read as "no retention — purge
everything", and here it has to mean the opposite. `enabled: false` is "never purge,
keep forever", and you take responsibility for the growth. It replaced the old
`"none"` sentinel.

The cluster declares the schedule in exactly one of two forms:

```yaml
# form A — one schedule, every pod purging together
spec:
  accesslogPurge:
    interval: 24h

# form B — staggered phases, plus the rotating write endpoint (ADR-037)
spec:
  rotation:
    enabled: true
    slotLength: 6h          # interval becomes replicas × slotLength
```

**Setting both is refused**, never silently resolved by precedence — a field that is
quietly never in force is the class of defect this split exists to remove. The
condition is `RotationConfigured=False/ScheduleDeclaredTwice` and it names both
fields. Meanwhile the operator's default 24h interval stays in force, so nothing goes
unpurged while you work out which one you meant.

Form B is a whole feature rather than a second spelling of form A: it also moves the
write endpoint, and it only helps deployments whose clients re-dial.
[`PURGE-ROTATION.md`](PURGE-ROTATION.md) is the guide — read it before enabling it.

Both durations are Go durations (`168h`, `24h`, `30m`) and must land on a whole
second: `olcAccessLogPurge`'s fields are `[ddd+]hh:mm[:ss]` and there is nowhere to
put a millisecond. The API server checks only the *grammar* — `0s` and `500ms` both
parse — so the operator reports the rest and writes its default rather than leaving a
journal unbounded.

**Migration — do this BEFORE you upgrade the operator.** The old `accesslogPurge`
was a two-token string, `"7+00:00 1+00:00"`. It is **refused, never reinterpreted**:
silently re-reading an old value as a retention would be undebuggable. Rewrite it as
`retention:`, and move the interval to the cluster if you meant to keep it.

That sentence covers a *new* apply. It says nothing about the `SlapdDatabase` objects
already **stored** in your API server, and those are the dangerous ones. A stored
string does not decode against the object schema, and the damage is not confined to
the object carrying it. Measured against a real API server (envtest, k8s 1.37, the
pre-M7 CRD upgraded in place to the current one, one legacy object and two healthy
ones):

| what | result |
|---|---|
| applying the new CRD over the old one | **accepted** — nothing warns, nothing refuses |
| typed read of the legacy object | fails: `json: cannot unmarshal string into Go struct field DatabaseReplicationConfig.spec.replication.accesslogPurge of type v1alpha1.AccesslogPurgeConfig` |
| typed read of a healthy object | fine |
| typed **list** of the namespace, and cluster-wide | **fails, with the same error** — the healthy objects are collateral |
| typed list of a namespace holding no legacy object | fine |
| the operator's **cache syncing** `SlapdDatabase` | **never syncs** (`WaitForCacheSync` still false after 20 s) |
| `kubectl get` / any unstructured read | fine, and shows the stored string verbatim |

So the blast radius is **every list whose result set contains the object**, and the
operator's informer lists `SlapdDatabase` cluster-wide. One un-rewritten CR, in any
namespace, is enough to stop the operator reconciling **every** `SlapdDatabase` in
the cluster — not just its own, and not just its namespace. Nothing about the symptom
points at the CR that caused it.

Before upgrading, find them and clear the field — the cluster now owns the interval
anyway, and an unset retention is the 7-day default:

```bash
# find every SlapdDatabase still storing the string form
kubectl get slapddatabases -A -o yaml | yq '
  .items[] | select(.spec.replication.accesslogPurge | tag == "!!str")
  | .metadata.namespace + "/" + .metadata.name + "  " + .spec.replication.accesslogPurge'

# clear it, one per line of that output
kubectl patch slapddatabase <name> -n <namespace> --type=merge \
  -p '{"spec":{"replication":{"accesslogPurge":null}}}'
```

Clearing is the pre-upgrade move, not rewriting: under the **old** CRD the field is
still typed `string`, so patching in the object form is rejected outright
(`spec.replication.accesslogPurge in body must be of type string`). Set the retention
you want after the upgrade, when the schema can hold it.

If you upgraded first and the operator has gone quiet, you are not stuck and nothing
needs restoring. `kubectl` still reads and writes these objects — only the *typed*
decode fails — so the same `yq` sweep still finds them, and a merge patch into the
new object form is accepted and repairs the decode immediately (measured: the typed
list went back to clean on the next call):

```bash
kubectl patch slapddatabase <name> -n <namespace> --type=merge \
  -p '{"spec":{"replication":{"accesslogPurge":{"retention":"168h"}}}}'
```

`spec.replication.syncprovSessionlogSource` chooses *where* the data database's
syncprov session log lives. `InMemory` (the default, and what an unset field means)
is the in-memory log sized by `syncprovSessionlog`. `Accesslog` points syncprov at
this database's own change journal instead — one copy of the recent history, on
disk, surviving a restart, where the in-memory one starts empty after every restart.
The two are exclusive: `Accesslog` requires `syncprovSessionlog` unset or `0`, and
needs `replication.enabled` with `deltaSync`, since without a journal there is
nothing to read. It does **not** put a session log on the accesslog database —
ADR-022 still forbids that, and it is a different attribute on a different database.

Sourcing the log changes the replay window's *units*. In memory the window is an
operation count — the last 5000 writes, however long that takes. From the journal it
is the journal's retention, i.e. `spec.replication.accesslogPurge.retention`, seven
days by default. Which is larger depends entirely on your write rate; size the purge window
on purpose before relying on it (see *Journal sizing* below).

This field is **startup-only**, and that has consequences worth reading twice.
`olcSpSessionlogSource` cannot be written to a running slapd at all — doing so is a
double free in every released OpenLDAP, 2.5.20 through 2.7.1 and master, and it has
already killed live providers — so the operator never writes it over the wire. It
reaches `cn=config` only through the init container, which converges it offline at
pod start. Editing the field therefore changes nothing until each pod restarts.
`kubectl get slapddatabase <name> -o yaml` shows `StartupConfigConverged`: `False`
with reason `RestartRequired` names the pods still running the old configuration,
`True` means every pod that could be read carries the declared one. The operator
does **not** restart them for you: restarting a provider fleet is a topology event
at every peer — 130–268 s of address rediscovery each, measured
([ADR-016](adrs/adr-016-pod-routed-cross-cluster-replication.md)) — far too much to
happen behind your back because you edited a tunable. Delete the pods, one at a
time, when it suits you.

Through all of that the database keeps exactly one session log. Until a pod actually
carries the source, its in-memory log is left alone, and the two are exchanged in a
single offline write at the restart — never dropped first and replaced later. A
cluster's first generation of pods is the same story from the other end: the operator
creates the database and its overlays on a running server, where the attribute cannot
go, so a brand-new pod runs the in-memory default and picks the source up one restart
later.

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
| `spec.persistence.{config,data,accesslog}.size` | 1Gi / **5Gi** / 1Gi | StatefulSet `volumeClaimTemplates` — **create-only in practice**: the templates are immutable, so a changed size needs the StatefulSet recreated |
| `spec.rotation.enabled` | off | converged into every pod's `olcAccessLogPurge` |
| `spec.rotation.slotLength` | none, and **required when enabled** | converged |
| `spec.rotation.slotsFromWriteToPurge` | `1` | signed, taken mod `replicas`; `v mod replicas = 0` is refused and reported |
| `spec.rotation.leadTime` | `0` — the endpoint moves at the boundary | not written to slapd at all; it shifts only *when* the write Service is repointed |

`spec.ldap.passwordHash` governs only what slapd hashes on a client's behalf. The
root passwords the operator generates are hashed by the operator before they reach
slapd. Stronger schemes need their module in the runtime image, and slaptain's does
not ship `pw-argon2` today — asking for `{ARGON2}` produces a slapd that rejects
every password write.

### The purge rotation lives in its own guide

`spec.rotation` is form B of the schedule above: instead of one interval shared by
every pod, each read-write pod gets its own purge **phase** on one absolute grid —
with `n = spec.replicas` and a slot length `t`, interval `T = n·t` and pod `k` at
phase `k·t` — and a second Service, `<cluster>-write`, follows the grid so the pod
taking writes is never the pod purging.

It is opt-in and off by default, and the placement facts are all that belong here:
the four fields are converged per pod (`leadTime` excepted — it is never written to
slapd at all, and only shifts *when* the write Service is repointed), and the third
value of `olcAccessLogPurge` is a slaptain patch rather than upstream's, so it needs
a patched image — `slapd -VV` reporting a version ending in `+slaptain<N>`. A
build ending in `+pure<N>` is the same OpenLDAP release carrying none of our
patches and will refuse the phase.

Everything operational — **the client-side precondition that decides whether the
feature helps you at all**, how to turn it on, the sizing arithmetic, the lead-time
window and how it closes as the write rate rises, and every condition reason the
operator publishes — is in **[`PURGE-ROTATION.md`](PURGE-ROTATION.md)**. Read it
before setting `spec.rotation.enabled`: a rotation whose clients hold their
connections, or whose `leadTime` is mis-sized for the write rate, measures *worse*
than no rotation at all.

### `cn=monitor`: always present, nothing to set

Every pod, read-write and read-only, carries slapd's monitor database. The operator
converges it on every pass (ADR-033 amendment 2026-10-08). It adds
`olcDatabase=monitor` with `olcRootDN: cn=admin,cn=config` and keeps its one ACL equal
to a constant. The monitor backend is built into slapd in every slaptain image, so
there is no module to load:

```
to * by dn.children="cn=slaptain-auth" read
     by dn.children="cn=peercred,cn=external,cn=auth" read
     by * none
```

There is no field and no switch. At rest none of this writes anything: the operator
reads, compares and moves on. It writes again only when the ACL differs, after a hand
edit or after an upgrade that changes the constant.

Who can read `cn=Monitor`:

- **any node-local replication identity**, `cn=repl-<db>,cn=slaptain-auth`, with that
  database's `replication-password`. Any database's identity works, not only the
  one for the database you are looking at;
- **`cn=admin,cn=config`**, which is the monitor database's rootDN;
- **a process inside the pod on the ldapi socket** (SASL EXTERNAL), whatever its UID
  (measured for the ADR amendment);
- **nobody else.** Anonymous and every data-tree identity get `No such object`. That
  includes the legacy `cn=replication,<suffix>`, so `slctl ldapsearch --as
  replication` cannot read it. Use `--as config`, plus `--database <db>` when the
  namespace holds more than one database.

```bash
slctl ldapsearch -n <ns> --as config --database <db> --pod 0 -b cn=Monitor -s one '(objectClass=*)'
ldapsearch -x -H ldap://<pod>:1024 -D cn=repl-<db>,cn=slaptain-auth -y <pwfile> \
  -b 'cn=Connections,cn=Monitor' '(objectClass=*)' '+'
```

**Per-database statistics depend on order.** back-mdb attaches its `olmMDB*`
attributes to `cn=Database <N>,cn=Databases,cn=Monitor` when it opens a database,
and only if the monitor database exists at that moment. A cn=config ADD opens the
database immediately. Monitoring is on by default in back-mdb, so no `olcMonitoring`
setting is involved. Measured on 2.7.1:

- an mdb database ADDed after the monitor database gets them at once, whether the
  monitor database was there at start or was itself ADDed at runtime;
- an mdb database that was open before the monitor database existed gets them only
  after slapd restarts. Setting `olcMonitoring: TRUE` live does not help.

So the operator makes sure the monitor database exists immediately before it ADDs a
data or accesslog database, on read-write and read-only pods alike. The node-local
auth database is added after the monitor database in the same pass. A pod created by
this operator version has statistics for every database from the start. A pod whose
databases predate the monitor database (an upgrade from an operator without it) gets
them at its next restart. The global counters (`cn=Operations`, `cn=Connections`, …)
answer immediately either way.

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
of 5Gi. The ratio is deliberately lopsided: a cluster runs out of PVC long before it
runs out of map, which is exactly the failure you want — it is visible, it can be
alerted on, and it is fixable by expanding the volume, whereas exhausting the map is
none of those things. In production the same relationship holds, at different
absolute numbers.

The 5Gi is the OPERATOR's default and lives in exactly one place
(`internal/controller/persistence_defaults.go`). The charts deliberately do not
restate it: a size written into a `SlapdCluster` is frozen there at creation time, so
a chart that repeated the number would pin every cluster it installed to whatever the
default was on the day of install. Unset means "this operator version decides".

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

`spec.replication.accesslogPurge.retention` defaults to `168h` — keep seven days of
journal. Unset gets that default; `enabled: false` opts out of purging entirely.

How often the sweep runs is the **cluster's**, not this database's: the operator's
default of 24h, or `SlapdCluster.spec.accesslogPurge.interval`, or
`replicas × slotLength` under `spec.rotation` (ADR-024 R6 — see *`olcAccessLogPurge`
has three owners* above). The sizing arithmetic below is unchanged; only who writes
the interval moved.

There is a default because "no opinion" is not a neutral setting here. An unpurged
journal grows until it reaches its 8Gi map ceiling, and the accesslog overlay sits in
the **data** database's write path — so the moment the journal cannot take a write,
neither can the data. That is a slow-motion outage rather than a tuning wart, which is
why the field is defaulted rather than left to be remembered.

With `syncprovSessionlogSource: Accesslog` the same window is also the session log's
replay window, so the purge policy stops being only a sizing decision and becomes
how far behind a reconnecting consumer may be and still be answered from a delta.

Seven days is chosen to cover a weekend-plus consumer outage. The window is precisely
how long a consumer may stay away and still resume from a delta: one whose cookie
predates the purge falls back to a full refresh. Lengthen it if your worst realistic
outage is longer, and size `persistence.accesslog.size` to match.

A consumer that stays away long enough can come back with a cookie the provider
can no longer answer. This paragraph used to say the 2.7 images mitigate that
and `-ol26` carries "elevated exposure" — **retracted**: the storm slaptain
actually hits is ITS#9398, it is present in both version lines, and what
addresses it is the accesslog `syncprov-checkpoint` that 0.5.0 turns on by
default on both. Neither line is the safer one here.

What still holds is the sizing advice above: treat a consumer outage
approaching the purge window as something to verify resynced rather than
assume, on whichever images you run.

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
seconds rather than at the kernel's TCP timeout), `retry=10 +` and
`keepalive=240:3:30`. The keepalive idle time sits deliberately under the five-minute
mark where stateful firewalls and cloud load balancers commonly drop an idle flow,
because a `refreshAndPersist` connection is idle by design between writes.

What is deliberately **not** in the stanzas is `timeout=`. It shipped briefly as
`timeout=300` and was withdrawn the same day on live evidence: supplying it flips
slapd's refresh-phase waiting discipline from a non-blocking peek to a blocking wait
on a threadpool thread, and a `cn=config` write pauses the whole pool — so while any
consumer was in refresh phase, every config write the operator made froze the entire
server for up to 300 seconds (three such total-silence freezes measured per
convergence pass, cascading across a three-site mesh). The expiry detects nothing —
slapd just goes back to listening — so the wait bought no protection at any value.
Failure detection belongs to the socket-level mechanisms above, which act without
occupying a worker thread. See the 2026-09-13 entry in
[`docs/reconcile-loop-fixes.md`](reconcile-loop-fixes.md) and the ADR-024 amendment
of the same date. `timelimit=` is likewise never emitted — that one is a server-side
search time limit and would abort the persistent search itself.

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
  a live database, and the message carries current, desired and the change path. The
  same command's `StartupConfigConverged` condition answers the startup-only
  question: `False` / `RestartRequired` names the pods whose running configuration is
  not the declared one, and a pod that could not be read is reported as not assessed
  rather than counted as diverged.

## Related

- [ADR-024](adrs/adr-024-tunable-placement.md) — the placement doctrine and its
  amendments (the segfault, the hang, and the fourth class).
- [ADR-022](adrs/adr-022-syncprov-sessionlog.md) — the sessionlog's placement and its
  cost model, amended with the accesslog-sourced variant.
- [ADR-034](adrs/adr-034-startup-only-configuration.md) — the startup-only class:
  why the init container converges it offline, and why the restart is yours to take.
- [ADR-020](adrs/adr-020-accesslog-access-control.md) — accesslog access control and
  the limits amendment.
- [ADR-019](adrs/adr-019-per-database-accesslog.md) — one journal per database.
- [OpenLDAP versions](OPENLDAP-VERSIONS.md) — the 2.6/2.7 image pairs. Every live
  modifiability result quoted here was established on 2.7.1.
- [Backup & Restore](BACKUP.md) — the recreate path every bootstrap-time and
  recreate-required tunable points at.
- [Pod placement](PLACEMENT.md) — a different sense of "placement": which NODE a
  pod runs on, rather than which `cn=config` entry a tunable lives in.
- [`docs/BACKLOG.md`](BACKLOG.md) — what is deliberately not tunable yet, with the
  evidence.
