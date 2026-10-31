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

## v0.5.1

### The operator's cache holds what it manages, and nothing else (issue #28, ADR-042)

A fix for memory that grew with the size of the Kubernetes cluster rather than
with the number of slapd deployments. On a shared cluster with 84 namespaces
the operator was OOM-killed at its 128Mi default while managing one two-pod
cluster; with the limit raised it sat at 553Mi.

The manager was built without cache options, so every informer behind an
`Owns()` or a cached `Get` held **every** object of its kind in the cluster —
every Secret (Helm release records included, a megabyte each), every Pod,
every Service. Now every core-kind informer is filtered to
`app.kubernetes.io/name=slapd`, the label the slapd pods have always carried,
and every object the operator creates carries it too. Secrets are read
straight from the API server, always, because the Secrets a user supplies —
TLS, `cnConfigCredentials`, bring-your-own credentials, S3, kubeconfigs — carry
no label of ours and the operator never labels what it does not own.

Measured on a kind cluster running only the operator, 72 MiB of foreign
Helm-typed Secrets: heap +97.5 MiB and RSS 51 → 153 MiB before, heap +1.6 MiB
and RSS 46 → 53 MiB after.

**What it changes for a running cluster:** nothing rolls — the label chosen
is the one the pods already carry. Services, StatefulSets and ConfigMaps gain
the label through their next apply; the `<cluster>-config-password` Secret is
patched once (one log line). The default memory limit stays at 128Mi. See
`docs/UPGRADING.md`.

**Harness:** `tests/e2e.sh` now installs the operator with
`metrics.enabled=true`, because the new `cache_scope_test.go` reads the
operator's own `go_memstats` through the API server's pod proxy to assert
that a namespace full of foreign Secrets leaves its heap alone.

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
