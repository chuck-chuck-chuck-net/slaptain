# Changelog

Release notes for each tagged version. The publish workflow prepends the
section matching the tag to the generated release notes, so **a release with no
section here ships without one** — write it before you tag.

Headings are the git tag verbatim (`## v0.4.0`), with the `v`. Everything
downstream of the tag — images, chart `version`, chart `appVersion` — carries
the bare version; see `docs/VERSIONING.md`.

## This file versus `docs/UPGRADING.md`

They answer different questions and the line between them is worth keeping,
because the same change naturally wants writing up in both and then the two
copies drift.

| | this file | `docs/UPGRADING.md` |
|---|---|---|
| answers | what changed, and why | what it does to a mesh you already run |
| audience | everyone, including fresh installs | only someone crossing versions |
| read it | to decide whether to upgrade | before you actually do |

So a change is described **here** — what it is, why it was made, what it costs,
what is and is not established — and the **procedure** lives there: when it
lands, which conditions go False while it does, the exact opt-out, and the
order to do things in.

One deliberate exception: for a BREAKING change this file still carries the
single line that keeps today's behaviour, because a release note that makes a
reader go somewhere else to learn what to set is a release note that failed.

## v0.6.0

### One rolling restart, for two reasons

Two changes here touch the slapd Pod template — the default spread constraint
that arrives with `spec.placement`, and the cache labels the operator now
stamps on everything it creates. They land in the same release, so an upgrade
replaces every slapd Pod **once**, not twice. Both fleets roll: read-write,
and read-only where `readReplicas > 0`.

Nothing moves. Every pod comes back on the node it was already on, because its
PVC is `ReadWriteOnce` on local storage and the PV's node affinity admits
exactly one node. ADR-035 permits an operator upgrade to change the pod
template once; this is the release doing it on purpose, and
[`docs/UPGRADING.md`](docs/UPGRADING.md) is the procedure.

### New: `spec.placement`, and pods spread across nodes by default (ADR-043)

`spec.placement` passes `nodeSelector`, `affinity`, `tolerations` and
`topologySpreadConstraints` to both pod templates verbatim, and adds exactly
one thing of its own: when `topologySpreadConstraints` is unset, the operator
writes a single constraint per fleet — `maxSkew: 1` over
`kubernetes.io/hostname`, `whenUnsatisfiable: ScheduleAnyway`, selecting that
fleet of that cluster alone. Writers spread among writers, readers among
readers; one combined group would let every writer share a node while the
per-node totals still looked even.

**Unset means one constraint, not none.** A list you supply replaces the
generated one entirely and is never merged with it: Kubernetes rejects two
constraints on one pod sharing a `{topologyKey, whenUnsatisfiable}` pair and
ignores `labelSelector` when deciding that, so a merge would make your own
hostname constraint unschedulable from a value you cannot see. An explicit
empty list (`[]`) asks for no constraint at all.

**`ScheduleAnyway` is deliberate.** The first scheduling decision is the only
one this workload gets — with `WaitForFirstConsumer` local storage the volume
binds where the pod first lands and pins it there for the life of the PVC. A
hard constraint can strand a pod permanently, because the only node it may use
is the one being refused. So the default protects the *next* pod created from
scratch — a scale-up, a rebuild, a fresh cluster. It does not rebalance an
existing cluster and cannot.

Full guide: [`docs/PLACEMENT.md`](docs/PLACEMENT.md).

### The operator caches only what it labelled (ADR-044)

The operator used to cache every Secret, ConfigMap, Service, StatefulSet, Job and Pod in the cluster.
On a cluster with 84 namespaces that came to 553Mi against the chart's 128Mi limit,
and the operator was OOM-killed before it reconciled anything.

Everything the operator creates now carries `app.kubernetes.io/managed-by: slaptain`
and `ldap.chuck-chuck-chuck.net/cluster: <SlapdCluster name>`,
and the cache holds those six kinds only when they carry the second label.
A Secret you bring is read from the API server instead, and the operator never labels it.
Label it `ldap.chuck-chuck-chuck.net/cluster: <cluster name>` yourself if you want it cached,
or `multi_cluster` if several clusters use it.

The operator's memory with the filtered cache is not measured yet.

### The data database gets a syncprov checkpoint — and the reason is not ITS#9398 (ADR-045)

`olcSpCheckpoint: 100 10` is now written to every replicated **data** database's
syncprov overlay unless the `SlapdDatabase` declares its own value, and
converged onto overlays that already exist. v0.5.0 did the same for the
*accesslog* database at `1 1`; that value is unchanged and this is a different
attribute on a different overlay.

**Why the absent state was not "OpenLDAP's behaviour".** It was OpenLDAP's
behaviour minus the startup repair upstream assumes is there. The Admin Guide
says a database is scanned at startup for `entryCSN` values greater than the
stored `contextCSN`, and names shortening that scan as a reason to configure a
checkpoint. That scan is `FIND_MAXCSN`, gated `!SLAP_DBCLEAN(be)`, and
`back-mdb` sets that flag unconditionally — `/* mdb is always clean */`,
identical in 2.6.10 and 2.7.1. On the only backend slaptain ships the scan never
runs. So the documented *cost* of omitting a checkpoint is one we never pay, and
the undocumented *consequence* — a stale, unrepaired advertised `contextCSN` —
is one we always do.

**Measured** (single node, `slapd 2.7.2-0+pure1`, back-mdb, `SIGKILL`, restart):
with no checkpoint `contextCSN` was absent on disk and **regenerated ahead of
all content**; with a checkpoint the stale on-disk value was served unrepaired
with entries beyond it. On a two-node mesh the stale own-serverID value
self-healed on the first outbound refresh — the staleness is itself what makes
the peer believe it has changes to send. A **non-replicated** cluster has no
such path at all, and a pod that came back with a regenerated `contextCSN` will
discard its own entries when a peer later replays them.

**Why `100 10` and not `1 1`.** `1 1` closes the window on the operation itself,
which is exactly why the accesslog database takes it — there the window is a
permanent correctness cliff. On the data database it doubles writes into the
data environment on the hot path, unmeasured; the accesslog's value was bought
with a load-harness arm and this one would not be. `100 10` is what the
ecosystem converged on (Symas, Ubuntu, Bitnami). Our own charts and fixtures
shipped `500 15`, which is the man page's *example* and has no provenance beyond
that — they no longer set it, so the default is the shape the suite runs.

**Cost:** one suffix-entry modify in the data environment per 100 writes. None
in the accesslog environment: `syncprov_checkpoint` sets `o_dont_replicate` and
`accesslog_response` skips such operations, so ADR-037's scarce write path is
untouched. It is a converged `cn=config` attribute, so it rolls no pod.

**The window narrows, it does not close.** A checkpoint is only tested after a
successful write, so the last write before an idle period is never flushed at
any setting. What the default removes outright is the
regeneration-ahead-of-content case, because a checkpointed database always has a
`contextCSN` on disk to load.

**To decline it:** `syncprovCheckpoint: none`. See
[`docs/UPGRADING.md`](docs/UPGRADING.md) for what it does to a mesh you already
run, and ADR-045 for the full reasoning.

### slapd's open-file limit no longer comes from the node (ADR-048)

The slapd images gain an entrypoint, a small C launcher, that caps
`RLIMIT_NOFILE` at 65,536 before exec'ing slapd. slapd sizes its connection
table from the soft limit at startup — `ch_calloc(dtblsize, sizeof(Connection))`,
indexed by descriptor number — and the `FD_SETSIZE` clamp applies only to the
`select(2)` build, so on Linux nothing bounds it. An inherited limit of 2²⁰
costs roughly 720 MiB of connection table before the first client connects; a
container runtime configured with `LimitNOFILE=infinity` is the usual way to
inherit one.

The launcher **lowers** an inherited limit above 65,536 and leaves a lower one
alone, so a node whose soft default is 1024 still gives slapd 1024 slots. It
passes its arguments through unchanged, so nothing about how the operator
starts slapd changes — but it is an `ENTRYPOINT`, and a pod spec that sets
`command:` bypasses it. The operator sets `args:` only.

### Also in this release

- **Go 1.26.11** in all three modules and both builder images, closing
  GO-2026-6859 in `net/http` — the one vulnerability our code actually reached.
- **`golang.org/x/net` 0.62.0 and `cel-go` 0.30.0**, the module findings that
  have a released fix. Nothing here calls either; `x/crypto/openpgp`'s advisory
  has no release to move to and stays open deliberately.
- **e2e runs in CI**: a smoke tier on every push to a pull request labelled
  `dev images`, and a slow tier behind a second label, both on a kind cluster
  against that commit's branch-tagged images.
- **Getting a BuildKit worker** in [`docs/DEVELOPMENT.md`](docs/DEVELOPMENT.md):
  the three kinds of worker, the rootless `buildkitd` systemd user service, and
  the three ways it bites.
- `docs/BACKLOG.md` records that the accesslog checkpoint's convergence does not
  retrofit onto an overlay that already exists — it constructs the overlay DN
  while slapd stores the indexed one it assigned.

## v0.5.0

Three defaults move in this release. Two of them change which binary your pods
run, and one of them is the answer to v0.4.0's known issue.

**This page is what changed and why.**
[`docs/UPGRADING.md`](docs/UPGRADING.md) is what it does to a mesh you are
already running — when each change lands, which conditions go False while it
does, and the exact opt-out for each. Read it before upgrading an existing
deployment; a fresh install does not need it.

### The accesslog database gets a syncprov checkpoint — ITS#9398

`olcSpCheckpoint: 1 1` is now written to every accesslog database's syncprov
overlay, and **converged onto overlays that already exist** — which is all of
them, because the attribute has never been written before.

This addresses the precondition for the stale-cookie storm listed as a known
issue in v0.4.0. A log database's `contextCSN` is written only by a checkpoint
or a clean shutdown, while its `minCSN` is durable from the first write
carrying a serverID the log has not seen; an unclean stop in between leaves
`minCSN` naming a serverID `contextCSN` has never heard of, and that pod then
refuses **every** consumer of that log, permanently.

**What is established, and what is not.** The controls are unambiguous — with
`100 5` a `SIGKILL` produced a 1473/s storm, with `1 1` it produced none. Two
consecutive three-site mesh cycles have now run clean, against six prior
reproductions. That is a series starting, not a proof; the failure is a race
and a run that does not hit it proves nothing on its own.

**`1 1` is not a dial.** The checkpoint has to fire on the log entry's own
operation, so that `contextCSN` reaches disk before `minCSN` gains the new
serverID. Any larger value reopens the window between two adjacent operations,
which is exactly what `100 5` measured.

**Cost**, measured on a load harness as its own arm: no throughput or latency
penalty — 270,277 ops/s at p99 16.9 ms. Replication stays current to
255,903 ops/s, where the same setup without the checkpoint stays current to
roughly 7 % higher before lag reaches one second. A ceiling moving at a quarter
of a million operations per second, on bare metal with fast local storage.
Slower or shared storage is not measured, which is what the opt-out is for.

### Breaking: the default slapd image is now the PURE build

The unsuffixed `slapd`/`slapd-init` tags used to carry slaptain's behaviour
patches. They now carry the pure build — the same upstream OpenLDAP 2.7.1, from
the same packaging fork, with none of our patches. The patched pair moves to
`<tag>-ol2.7.1-slaptain`.

**The tag string does not change, so an unpinned cluster rolls onto a different
binary on operator upgrade without its pod template looking any different.**

Only clusters using `spec.rotation` with a phase-staggering mode are affected:
the per-pod purge phase is a third field of `olcAccessLogPurge` that upstream
slapd rejects. The journal stays bounded, the pods purge in step instead of on
the grid, and the operator says so with
`AccesslogPurgeConverged=False/PurgePhaseUnsupported`. To keep today's
behaviour, ask for the flavour by name:

```yaml
spec:
  images:
    select:
      flavour: slaptain
```

Nobody should have to run a patched slapd in production without having chosen
to, and until now everybody did without being told.

### New: `spec.images.select` — pick a build by what it governs

```yaml
spec:
  images:
    select:
      storageFormat: lmdb1.0   # what can open your volumes — lmdb1.0 | lmdb0.9
      flavour: pure            # whether you run our patches — pure | slaptain
```

Four slapd/slapd-init pairs, selected by two axes named for what they decide
rather than by a tag you have to decode. It drives **both** images from one
declaration, so ADR-021's "pin both or neither" stops being a rule you can
violate. An explicit `spec.images.slapd.tag` still wins and is used verbatim.

Keyed on the storage format rather than a version, deliberately: newer slapd
builds keep shipping under the same selection, and the only thing that can
strand a volume is the axis you pinned yourself.

**What the two formats actually are.** `lmdb0.9` is OpenLDAP **2.6**, `lmdb1.0`
is OpenLDAP **2.7**. They are named for the on-disk format because that is the
property that decides whether a given build can open your volumes — the two are
mutually unreadable, which is why crossing them is a dump-and-reload rather
than an upgrade.

**And they are built very differently**, which is worth knowing before you pick
one:

| selection | where the slapd binary comes from |
|---|---|
| `lmdb0.9` + `pure` | **Debian's own `slapd` package**, installed from the archive at a pinned version. Not built by us at all. |
| `lmdb0.9` + `slaptain` | Debian's own **source** package, rebuilt with our patches applied on top. |
| `lmdb1.0` + `pure` | **Our build**, from pristine upstream 2.7.1 with Debian's packaging forward-ported onto it. No patches of ours. |
| `lmdb1.0` + `slaptain` | The same build, with our patches. |

So on the 2.6 line you can have the distribution's own binary, byte for byte,
and that is the most conservative thing slaptain can offer. The 2.7 line has no
such option: **no distribution packages OpenLDAP 2.7 yet.** It is released and
supported upstream, but it is recent and not widely deployed, so shipping it at
all means maintaining a forward-ported packaging fork — which is what
`images/openldap-deb/` is.

**Why carry that cost, if 2.6 is what the world runs?** Two reasons, and the
second is the one that made it the default:

1. This project leans forward. Anyone running a young operator for multi-master
   OpenLDAP is not waiting on a distribution to package 2.7.
2. **It avoids a silent default flip later.** The two formats cannot read each
   other, so whichever line is the default, every cluster that said nothing is
   eventually billed a dump-and-reload when that default moves. Defaulting to
   2.7 now — while 2.7 is where the format line is going anyway — means
   **whoever takes the default never migrates**, and anyone on 2.6 is there
   because they chose it and can plan the move. The alternative was to default
   to 2.6 and change our minds later, on everyone else's maintenance window.

`slapd -VV` now discriminates the flavours — a patched build reports a version
ending in `+slaptain<N>`, a pure one `+pure<N>`. Before this release both
reported the same string, which made the `PurgePhaseUnsupported` message's own
advice unfollowable.

### The session log is sourced from the journal by default

`SlapdDatabase.spec.replication.syncprovSessionlogSource` unset now means
`Accesslog` where it meant `InMemory`. The in-memory session log does not
survive a restart, so a consumer reconnecting after one falls outside the
window and takes a full-DIT refresh it did not need; the journal is already on
disk and already written.

The attribute is startup-only, so this **lands at each pod's next restart
rather than at upgrade**, and every database reports
`StartupConfigConverged=False/RestartRequired` until then. That is expected
rather than an error, and `docs/UPGRADING.md` says what to do about it (mostly:
nothing).

`syncprovSessionlogSource: InMemory` keeps the old behaviour.

### New: `spec.rotation.mode`

The purge rotation's two halves — staggering each pod's purge, and moving the
write endpoint off the pod that is purging — are separately settable:
`PurgeAndWrite` (unset, and the only shape measured end to end), `PurgeOnly`,
and `WriteOnly`, which needs no patched image because repointing a Service is
not something slapd has to understand. The latter two are tier 2 and carry less
evidence; [`docs/PURGE-ROTATION.md`](docs/PURGE-ROTATION.md) states which
quantity was measured for each.

### Also in this release

- **ADR-039** (two tiers of support): "supported" was doing two jobs — *have we
  validated this* and *will the operator produce a valid cluster if you set it*.
  Conflating them withholds a knob rather than labelling it. Tier-2 surfaces are
  now labelled where they are offered, and the evidence travels with the knob.
- **ADR-040** (data-plane image identity) and **ADR-041** (a pod says what
  format its volume is). ADR-041 is designed, not built: a wrong
  `storageFormat` still crashloops with slapd's own `MDB_INVALID: File is not
  an LMDB file … Restore from backup!`, which is wrong and destructive advice
  on a healthy volume, and the stuck pod then blocks its own replacement.
  `docs/OPENLDAP-VERSIONS.md` says so plainly and gives the manual recovery.
- **ITS#9398, corrected**: the purge is not what opens the window. A sweep that
  matches nothing writes nothing; `accesslog_response` advances `minCSN` on the
  first logged write carrying a new serverID, with no sweep involved. The
  upstream-facing reproduction document is corrected throughout.
- The README explains the four images, the selector and the purge rotation, and
  no longer justifies OpenLDAP 2.7 with a reason that had been retracted.

## v0.4.0

### Breaking: `accesslogPurge` becomes an object

`SlapdDatabase.spec.replication.accesslogPurge` was a string; it is now an
object, and **the purge schedule moves to the cluster**. This affects you even
if you never touch the new feature.

A *stored* string value fails the typed LIST for the whole result set, so the
`SlapdDatabase` controller stops reconciling **cluster-wide, in every
namespace** — not just for the database holding it. Clear the field on every
`SlapdDatabase` at every site **before** upgrading the CRDs; under the old CRD
the field is still typed `string`, so patching in the object form is rejected.

`docs/UPGRADING.md` has the exact order, the commands, and what to do if you
upgraded first.

### New, and EXPERIMENTAL: the accesslog purge rotation (ADR-037)

Off by default. The accesslog purge stalls writes on the provider running it;
this gives each pod its own purge *phase* on an absolute grid and moves a
`<cluster>-write` Service endpoint ahead of the grid by a declared `leadTime`,
so the pod taking writes is never the pod purging.

**This is a tier-2 surface (ADR-039): configurable, honestly reported, and not
yet asserted fit for production.** What is missing is operating experience, not
code. Three things make it unlike a conventional slapd deployment:

- **It needs a slapd carrying our `accesslog-purge-phase` patch.** The per-pod
  phase is a third value of `olcAccessLogPurge` that upstream OpenLDAP does not
  have. A stock build refuses it, the operator falls back to the two-value form
  and reports `AccesslogPurgeConverged=False/PurgePhaseUnsupported` naming the
  version you need — nothing breaks, but nothing rotates.
- **It couples your write endpoint to each provider's internal purge
  schedule.** Rotating the write target of a multi-provider cluster is ordinary
  OpenLDAP; doing it in step with the purge is not something conventional
  deployments do, and field experience is thin everywhere.
- **Its benefit is conditional on your clients.** A deployment whose write
  connections outlive `leadTime` measures *worse* with this on than off.

Known use cases are narrow: migrations, and bulk provisioning with short-lived
write clients. **Feedback wanted, positive or negative** — what you ran and what
you measured. ADR-037's seventeenth amendment lists the arms nobody has run.

User guide: `docs/PURGE-ROTATION.md`. Design record: ADR-037 (still `Proposed`,
deliberately).

### Note on the default slapd image

The default `slapd` image carries our `accesslog-purge-phase` patch on top of
OpenLDAP 2.7.1, built from the vendored packaging fork in
`images/openldap-deb/` and versioned `2.7.1-0+slaptain3` (`slapd -VV`). It is a
behaviour patch, not packaging, and `debian/changelog` records what it changes
and why — including one deliberate change to stock behaviour it could not avoid.

### Known issue: ITS#9398 after a pod loses its disk

A pod that is rebuilt from replication (ADR-012 case 2) can come back with a
`minCSN` on its accesslog database naming a serverID its `contextCSN` does not
track. That pod then answers **every** consumer of that log with `sync cookie
is stale`, permanently, while CSN convergence and every other health signal
read green. Measured again on a three-site mesh on 2026-10-22: 9358
stale-cookie lines on one pod.

This is **ITS#9398**, filed upstream in 2020 and fixed in no release including
master — the contemplated fix is blocked behind an explicitly postponed
enhancement (ITS#9225). It is not new in 0.4.0 and not caused by the purge
rotation.

**Detect it**: `slctl inspect` reports it as `accesslog-mincsn`, and
`tests/e2e.sh` runs that check at every site after the suite. **Clear it**: one
write to the affected suffix from a pod carrying each missing serverID — note
that serverID often lives at *another site*. `docs/its9398/its9398.sh heal`
does this; measured taking a storming pod to zero in under 20 s. Rebuilding the
pod also clears it. Background: `docs/ITS-9398-STORM-SIGNATURE.md` (the
detector), `docs/ITS-9398-REPRODUCTION.md` (the mechanism),
`docs/ITS-9398-HANDLING.md` (the options).

### Also in this release

- The ITS#9398 investigation: `docs/ITS-9398-REPRODUCTION.md` (the bug in pure
  slapd terms, for upstream) and `docs/ITS-9398-HANDLING.md` (how the ecosystem
  copes, and the mitigation options). ADR-021's stated reason for defaulting to
  OpenLDAP 2.7 is **retracted**: the storm is ITS#9398, open since 2020 and
  fixed in no release including master — not ITS#9580. The packaging and LMDB
  reasons for 2.7 stand on their own.
- ADR-039 (two tiers of support) and ADR-040 (data-plane image identity), both
  `Proposed`.
