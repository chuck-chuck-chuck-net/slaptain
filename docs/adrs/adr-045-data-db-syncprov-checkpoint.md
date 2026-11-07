# ADR-045: The data database's syncprov checkpoint defaults on, for a reason that is not the accesslog's

**Status:** Proposed
**Date:** 2026-11-07

## Context

`SlapdDatabase.spec.replication.syncprovCheckpoint` carries no operator default,
and slapd's own default is no checkpointing at all — `slapo-syncprov(5)` says so
("Checkpointing is disabled by default"), and `syncprov_op_response` gates the
whole checkpoint block on `if ( si->si_chkops || si->si_chktime )`, both zero
when the directive is absent. The widely quoted `500 15` is the man page's
example. ADR-024's 2026-10-23 amendment defaulted the *accesslog* database's
checkpoint to `1 1` on ITS#9398 grounds and left this one alone, calling its
absence "a performance trade, not a correctness cliff" — and left the asymmetry
otherwise unexplained.

Two things make that worth reopening. The asymmetry has an explanation, and the
absent state is worse than "a performance trade".

### contextCSN reaches disk by three paths, not two

A provider's `contextCSN` is maintained in memory. It is written to the database
by a syncprov checkpoint, by a clean shutdown (`syncprov_db_close`) — and by the
*consumer* side of the same pod: `syncrepl_updateCookie` (`syncrepl.c:5438`) does
a real `be_modify` of the whole `contextCSN` vector on every cookie advance, and
`check_syncprov` (`:1022`) refreshes that vector from syncprov's live in-memory
values first. So inbound replication traffic flushes the pod's own serverID too.

That third path is the asymmetry. A log database has no consumer: nothing ever
replicates *into* `cn=accesslog-<db>`, so a checkpoint or a clean stop are the
only ways its `contextCSN` is ever written, and ITS#9398's damage is permanent.
A data database has a repair path the log database structurally cannot have, and
its damage is self-healing. The two defaults differ in kind, not in taste.

### The repair upstream documents does not run on back-mdb

The Admin Guide states that at startup, "when a contextCSN value is read, the
database will still be scanned for any entryCSN values greater than it, to make
sure the contextCSN value truly reflects the greatest committed entryCSN", and
names shortening that scan as a reason to configure a checkpoint.

That scan is `FIND_MAXCSN`, and its one call site is gated
`if ( si->si_ctxcsn && !SLAP_DBCLEAN( be ) )` (`syncprov.c:4096`), while
`back-mdb/init.c:117` reads `/* mdb is always clean */` and sets the flag
unconditionally. On back-mdb the scan never runs. Identical in 2.6.10 and 2.7.1.

→ documentation and implementation disagree for the only backend we ship, and
they disagree in the direction that matters: the documented *cost* of omitting a
checkpoint (a slow startup scan) is one we never pay, and the undocumented
*consequence* (an advertised `contextCSN` that is stale and never repaired) is
one we always do.

### Measured

Single node, `slapd 2.7.2-0+pure1`, back-mdb, 20 entries, `SIGKILL`, restart
(2026-10-31):

| arm | on disk after the kill | max `entryCSN` | served after restart |
|---|---|---|---|
| no checkpoint | absent | `…113346.443433Z#…#001#…` | `…113347.552085Z` — **newly generated, ahead of all content** |
| `5 1` | `…113352.958530Z` | `…113352.967942Z` | `…113352.958530Z` — **the stale value, unrepaired** |

Two-node multi-provider mesh, no checkpoint anywhere, writes on both, then
`SIGKILL` and restart of one: its own serverID's on-disk value was stale by five
writes, and it served the correct value again after its first outbound refresh.
The peer never logged an error in three runs. The staleness is itself what makes
the peer believe it has changes to send, so the cookie that repairs the restarted
pod arrives unprompted.

### What the absent state actually costs

Ranked, with evidence named:

1. A provider whose own serverID is behind a consumer's cookie answers
   `LDAP_UNWILLING_TO_PERFORM` / "consumer state is newer than provider!"
   (`syncprov.c:3384`, reached only for `sids[j] == slap_serverID`) — the exact
   post-crash state. *Measured as self-healing* within the first refresh. The
   residual is the startup race, and a mesh where no peer holds newer information
   and nothing writes, where it persists until the next write anywhere — reasoned,
   not measured, and invisible to monitoring for the reason ADR-008's amendment
   already records about idle links.
2. The pod's own consumer cookie is stale by the same amount, so peers replay
   their accesslogs from that point and hand the pod its own recent writes back.
   Bounded by the staleness; if the staleness outruns `accesslogPurge.retention`
   it degrades to a present phase — a full refresh. Reasoned; the retention
   interaction is unmeasured.
3. A cluster with no replication has no third path at all, so every unclean stop
   loses `contextCSN` entirely and slapd generates a fresh one **ahead of its own
   content** (measured, arm 1). The consumer-side "not new enough, ignored" check
   is per-serverID, so such a pod will discard its own entries when a peer replays
   them — ADR-036's hole, reached without any PVC loss. Reasoned beyond the
   regeneration itself.
4. ADR-036's gate, when built, holds a pod read-only while a peer reports a higher
   `contextCSN[own sid]`. Without a checkpoint that is the state after *every*
   unclean restart rather than an exceptional one.
5. Nothing reports any of it. `ReplicationConverged` and `slctl inspect` read a
   stale own-serverID value as divergence, not as "unflushed".

### Everyone else defaults one, and we are inverted on both databases

| | data DB | accesslog DB |
|---|---|---|
| OpenLDAP 2.6 Admin Guide, delta-syncrepl example | `syncprov-checkpoint 1000 60` | none |
| Symas knowledge base, delta-syncrepl | `100 10` | none |
| Ubuntu Server documentation | `olcSpCheckpoint: 100 10` | — |
| Bitnami `openldap` image | `100 10` (default) | — |
| slaptain today | **none** | **`1 1`** |

Nobody checkpoints a log database, because nobody else measured ITS#9398. Everybody
checkpoints the data database. We do the opposite of both.

We also already ship the opinion without holding it: `charts/slapd-mesh/examples/three-site.yaml`,
`docs/demo/03-database.yaml`, `operator/config/samples/` and **every e2e fixture**
set `"500 15"`. The unset state — a minimal `SlapdDatabase` — is the one shape the
suite never runs.

## Options considered

**A. Keep no default.** Rejected. It is not OpenLDAP's behaviour that the user
gets, it is OpenLDAP's behaviour minus the startup repair that upstream assumes
is there; and it is the only shape we do not test.

**B. Default `1 1`, as on the accesslog database.** Rejected. `1 1` closes the
window completely — `si_chkops = 1` checkpoints on the operation itself — but on
the data database it doubles writes into the data environment, on the hot path,
unmeasured. The accesslog's value was bought with a load-harness arm; this one
would not be. Anyone who wants it can set it.

**C. Default `500 15`, matching our own charts and fixtures.** Considered, and it
has the pleasant property that the default would be an identity for everyone
following our documentation. Rejected on the merits: the number has no provenance
beyond being the man page's example, which is how it reached our fixtures in the
first place, and it bounds the staleness five times as loosely as the value the
ecosystem converged on for no stated gain.

**D. Default `100 10`.** Chosen. Ecosystem-aligned, and the staleness it admits —
up to 100 writes, or the writes of the last 10 minutes of write activity — is a
replay nobody will notice at any size we target.

**E. Write it only when the overlay is created.** Not an option: ADR-024 R4 already
made this field converge on every reconcile, and a create-only default is exactly
the shape the backlog suspects of having kept the accesslog default off the wire
on existing clusters.

## Decision

**R1. `spec.replication.syncprovCheckpoint` defaults to `"100 10"`** when unset,
on every replicated data database, converged per pod like the value it replaces
(ADR-024 R1/R4).

**R2. The opt-out stays and keeps its spelling.** `""` or the explicit sentinel
`"none"` writes no `olcSpCheckpoint` at all, restoring slapd's own behaviour —
the same two spellings `accesslogSyncprovCheckpoint` already honours.

**R3. The justification is not durability tuning.** It is that the absent state
regenerates `contextCSN` ahead of content on a non-replicated cluster, is never
repaired at startup on back-mdb contrary to upstream's documentation, and is the
one configuration our suite does not exercise. The field documentation says this;
it does not repeat the accesslog's ITS#9398 reasoning, which does not apply here.

**R4. `1 1` is not forbidden and is not recommended.** The residual window after
any `<ops>` above 1 is open by design, and the last write before an idle period is
never checkpointed at all — checkpoints are only tested after a successful write
(man page, and the block sits inside `syncprov_op_response`). Closing that window
costs a write amplification nobody has measured on the data path; whoever needs it
measures it first.

**R5. The examples drop the explicit value.** `charts/slapd-mesh/examples/`,
`docs/demo/`, `operator/config/samples/` and the e2e fixtures stop setting
`syncprovCheckpoint`, so that the default is the tested path and the fixture that
tests the field explicitly sets something *other* than the default.

**R6. Red first.** A fixture that sets nothing must still show `olcSpCheckpoint`
on every RW pod's data database — `overlay_tunables_test.go` already owns that
query shape and currently asserts the inverse.

## Consequences

**Two objections in the backlog entry do not survive and are retired here.**

The write-amplification objection, which cited ADR-037, is void.
`syncprov_checkpoint` sets `o_dont_replicate = 1`, and `accesslog_response` skips
such operations (`accesslog.c:1659-1661`), so a data-database checkpoint never
writes to the accesslog environment. ADR-037's scarce resource is untouched. What
remains is one suffix-entry modify in the data environment per `<ops>` writes — 1 %
at 100.

The forced-migration objection, which cited ADR-040 decision 8, is also void. This
is a converged attribute, proven live-modifiable before it was converged at all.
Moving the default later rewrites one value on one entry per pod. There is no
on-disk format and no roll.

**The convergence path was in doubt; it is not any more, and R1 does not
inherit the defect.** The backlog's accesslog checkpoint — present on a cluster
created after the default shipped, absent on an older one under the same
operator — was diagnosed on 2026-11-07.
`ensureAccesslogSyncprovCheckpoint` constructs the overlay DN as
`olcOverlay=syncprov,<dbDN>` and base-searches it, while slapd stores the entry
under the indexed name it assigned on the ADD, `olcOverlay={0}syncprov,…`, and
answers the constructed form with `No such object`. The function reads that
refusal as "no overlay yet, the creation request will carry the checkpoint" and
returns success, so the convergence is dead on every overlay that already
exists — and only there, which is why a freshly created cluster looks correct.

The data database's checkpoint goes the other way. `ensureSyncprovTunables`
searches one level under the database for `(objectClass=olcSyncProvConfig)` and
takes the DN the server gave it (`slapddatabase_controller.go:1424-1434`), on
every reconcile, whether or not the same pass created the overlay. A `"100 10"`
default therefore reaches existing overlays on existing clusters. The accesslog
defect is its own entry in `docs/BACKLOG.md` and its own fix; it does not gate
this one.

**The window narrows, it does not close.** After an unclean stop a pod still comes
back behind its own content, by up to `<ops>` writes. Failure modes 1 and 2 above
stay reachable, smaller. Failure mode 3 — regeneration ahead of content on a
non-replicated cluster — is the one the default actually removes, because a
checkpointed database always has a `contextCSN` on disk to load.

**ADR-036 is unaffected as a design and improved as an experience.** Its gate still
needs building; a checkpointed cluster simply gives it less spurious evidence to
act on.

**One e2e shape appears and one disappears.** The unset path becomes covered; the
"user set a value" path needs a fixture that no longer coincides with the default.

## Related

- ADR-024 — placement classes; its 2026-10-23 amendment defaulted the accesslog
  checkpoint to `1 1` and declined this one. This ADR supplies the explanation
  that amendment said was missing and reverses its conclusion for the data
  database only; `1 1` on the accesslog database is untouched.
- ADR-019 — one accesslog database per data database, which is why the two
  checkpoints sit one `grep olcSpCheckpoint` apart.
- ADR-036 — authorship recovery; failure mode 3 is its mechanism reached by a
  different route.
- ADR-037 — the write path is the scarce resource. Not spent here: the checkpoint
  is internal and never journalled.
- ADR-039 — the default is tier 1; `1 1` on a data database is neither offered nor
  measured, and R4 says so where the field is documented.
- `docs/ITS-9398-REPRODUCTION.md` — the log database's version of the same
  mechanism, and the measurement that produced its `1 1`.
