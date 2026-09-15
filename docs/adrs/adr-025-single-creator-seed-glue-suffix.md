# ADR-025: Seed is single-creator mesh-wide; a glue suffix is an unrestorable-artifact class

**Status:** Accepted
**Date:** 2026-09-13

## Context

A multi-site e2e run failed four restore specs plus one base-structure spec.
The autopsy uncovered a defect class that had been latent since multi-site
seeding existed, made visible by a nondeterministic race at cluster bring-up:

- Every restore Job exited 1 with
  `slapadd: dn="dc=example,dc=org" (line=1): (65) attribute 'dc' not allowed`,
  the cluster held at 0 replicas (the ADR-014 machine's deliberate
  failure posture).
- The backup artifact in S3 literally began with a **glue** suffix entry:
  `objectClass: top` + `glue`, `structuralObjectClass: glue`, **no `dc`, no
  `o`** — and a fresh `entryUUID` that no healthy pod carried.
- On the mesh, exactly **one** pod (the backup source, pod-0 of one site) held
  that glue; the other eight RW pods held the real entry
  (`structuralObjectClass: organization`) with a *different* `entryUUID`.
- An ordinary base search of the suffix on the glued pod returned **nothing**
  — the pod was silently broken for every client doing a base search — while
  `status.phase` read `Running`, every external peer read `Synced`,
  `DataPresent` read `True`, and the backup completed "successfully".
- The site's read-only pod carried the **same glue with the same
  `entryUUID`**: it had initial-synced from the glued provider and replicated
  the glue faithfully.

Not a regression: the same code went fully green on a fresh bring-up hours
earlier. The trigger is a race at seed time.

### Root cause, layered

**Layer 1 — each site seeds the same DNs independently.** ADR-012 made seed
one-shot *per cluster* and pinned it to pod-0 — which serialises seed writers
**within** a site. In a multi-site mesh, every site's `SlapdDatabase` carried
the same `spec.seed`, so N sites performed N independent creates of the same
DNs. `entryUUID` is server-generated (NO-USER-MODIFICATION), so the "same"
suffix entry exists as N different identities. This is exactly the ADR-012
multi-pod seed race, one level up: a per-site latch answering a per-mesh
question — the same class as the 2026-09-13 bind-identity finding ("a value on
the cluster axis answering a per-database question"), on the site axis.

**Layer 2 — slapd's conflict handling can demote a loser's suffix entry to a
permanent glue.** syncrepl identifies entries by `entryUUID`. Two paths in the
2.7.1 sources manufacture a glue at the suffix:

- *Non-present delete of a non-leaf.* During a refresh with a present phase,
  the local suffix entry's UUID is not in the provider's present set (the
  provider has the same DN under a different UUID). `syncrepl_del_nonpresent`
  tries to delete it; the suffix has children, `be_delete` returns
  `LDAP_NOT_ALLOWED_ON_NONLEAF`, and slapd's fallback **replaces the entry's
  objectClass with `{top, glue}` and its structuralObjectClass with `glue`**
  in place (`servers/slapd/syncrepl.c:5112-5151` in 2.7.1).
- *Child before parent.* When a child entry arrives before any suffix entry
  exists locally, `syncrepl_add_glue_ancestors` creates the missing ancestors
  — including the suffix — as fresh entries carrying **only**
  `objectClass: top` + `glue` (`syncrepl.c:5195-5333`; the glue template at
  `:4938`). The captured artifact matches this path exactly: no RDN attribute
  at all, a locally minted `entryUUID`, and an `entryCSN` inherited from the
  triggering operation.

Which pod loses is an interleaving artifact across refresh sessions in the
mesh — hence nondeterminism.

**Layer 3 — the glue is permanent, and CSN comparison is structurally blind to
it.** Measured live: the glue's `entryCSN` was **identical** to the winning
real entry's `entryCSN` (same CSN, different UUID). Mesh-wide, the DN is
"converged" at that CSN. Delta-syncrepl only ships changes newer than the
consumer's cookie, so no peer ever re-sends the suffix entry; and even a full
refresh does not heal it, because `dn_callback` treats an incoming entry with
an **identical CSN as a no-op** (`syncrepl.c:6127`; the glue-bypass branch at
`:6105` only fires when an entry for that DN is actually delivered again).
Nothing in normal operation ever writes the suffix entry, so nothing ever
revisits the DN. Every CSN-based signal — `ReplicationConverged`, peer
statuses, `slctl` CSN checks — sees a healthy cluster.

**Layer 4 — every guard along the way accepted the corrupt state.**

- slapd hides glue entries from ordinary searches at the frontend (not via
  ACLs — the rootDN sees nothing either); only a **ManageDSAIT** base search
  (RFC 3296) reveals them.
- `DataPresent` returned True if the root entry was visible on *any* pod.
- `slapcat` dumps the glue faithfully, so the artifact's first line is a
  correct-looking `dn: <suffix>`.
- Restore preflight checked only that DN line — the DN is present, the entry
  is a corpse. `slapadd` then rejects it (glue permits no attributes, and the
  RDN attribute must be present), **after** the cluster has been scaled to 0.
- A control experiment pinned the blame: `slapadd` of the canonical
  `top`+`dcObject`+`organization` entry against the *same* config PVC with the
  *same* init image succeeded (rc 0) — config, schema and images were healthy;
  only the artifact was poisoned.

**Named class:** *a one-shot guard scoped narrower than the invariant it
protects* (per-site seed latch vs. mesh-wide single-creator), compounded by
*guards whose evidence can be a plausible lie* (a DN line that exists, a
root entry visible somewhere).

### Evidence chain (recorded so it need not be re-derived)

All from the 2026-09-13 incident on a three-site lab mesh (three RW pods per
site plus one RO pod on the affected site); lab-internal names and addresses
withheld per the References policy:

1. **Restore-Job logs** captured racing the operator's ADR-018 Job reaping
   (the reaper deletes finished Jobs; the logs had to be pulled from the
   failing pods before the terminal-phase reap): the exact
   `(65) attribute 'dc' not allowed` signature on `dn="<suffix>" (line=1)`.
2. **Artifact fetch** from the S3 backend: the gzipped LDIF's first entry is
   the glue (objectClass top+glue, structuralObjectClass glue, no RDN
   attribute, entryUUID `1a914f3a-…`, entryCSN
   `20260913185308.559566Z#000000#065#000000`, createTimestamp in the seed
   window).
3. **ManageDSAIT live reveal** on the glued pod: ordinary base search returns
   0 entries with result 0; `ldapsearch -M` returns the glue. Verified again
   during this fix.
4. **Identical-CSN permanence proof:** a healthy peer pod's real suffix entry
   carries entryUUID `1b03268c-…` with the **same** entryCSN as the glue —
   the frozen conflict.
5. **RO-pod propagation:** the site's read-only pod shows the **same glue,
   same entryUUID** as its provider — a consumer initial-syncing from a glued
   provider replicates the glue.
6. **Control experiments:** (a) `slapadd` of the canonical suffix entry on the
   same config PVC/image → rc 0; (b) the restore machine's own posture —
   Jobs at BackoffLimitExceeded, cluster held at 0 — was the designed
   destroy-last failure mode firing too late.

## Decision

**1. Seed is single-creator mesh-wide (founder-only).** In a replicated
multi-site deployment, exactly **one** site's `SlapdDatabase` carries
`spec.seed`; every other site deploys the same CR *without* `seed` and
receives the DIT via syncrepl — the same cold-start delivery ADR-014 relies on
for a fresh peer, and the mesh-wide form of ADR-012's "replication propagates
the seed from pod-0 to peers". `tests/e2e.sh` applies fixtures seed-stripped
to every context after the first (`strip_seed_block`); the founder rule is
documented in `docs/BOOTSTRAP.md`.

**2. Operator belt: withhold-create on positive evidence of a foreign
creator.** Before applying seed, the SlapdDatabase controller reads the suffix
entry's `entryCSN` on pod-0. If the entry exists and its serverID lies outside
this cluster's own sid range (`serverIDBase+1 … serverIDBase+replicas`,
ADR-011/ADR-017), the DIT already has a creator elsewhere in the mesh: the
seed is withheld wholesale and `SeedApplied` latches. This is the **opposite
direction** of the reverted `verifySeedExists` (see the ADR-012 amendment):
that one *re-created on absence* and masked data loss; this one *declines to
create on presence*. Unknown evidence (absent entry, unreadable CSN) proceeds
with the normal idempotent seed — an absence that could mean "could not read
it" never suppresses a seed. The belt narrows the race (two sites can still
both seed before either replicates); the founder-only model is the fix, the
belt is defence in depth.

**3. Restore preflight rejects an unrestorable suffix entry** (ADR-014
amendment). `backup.Preflight` reads the suffix entry's body and fails —
before anything is scaled down or wiped — on any of: `objectClass` contains
`glue`; `structuralObjectClass` is `glue`; the suffix's RDN attribute is
absent. This restores the destroy-last guarantee for this class: the 
poisoned artifact costs neither downtime nor data loss.

**4. A backup records its source's suffix health** (`SourceSuffixHealthy`
condition on `SlapdBackup`): True/`SuffixEntryVisible`, False/`GlueSuffix`
(this artifact will fail restore preflight), False/`SuffixMissing`,
Unknown/`CheckFailed`. Probed via ordinary-then-ManageDSAIT base search as
`cn=replication` (ADR-008 identity). Record-only, best-effort — **a backup
always takes a backup** (ADR-014 2026-09-12 amendment stands).

**5. `DataPresent` requires the root entry visible on every reached RW pod.**
`False/DataMissingOnPods` names the hiding pods and hints at ManageDSAIT.
Still observability-only per ADR-012 — it never drives operator action.

**6. `slctl inspect` gains `suffix-visibility` and `suffix-uuid-agreement`.**
Per-pod ordinary + ManageDSAIT probes; a verified glue or per-pod
visible/hidden divergence fails, uniform invisibility warns (anonymous ACLs
are not divergence); pods disagreeing on the suffix entry's `entryUUID` fail —
the divergence CSN comparison cannot see.

**7. No auto-heal.** The operator never rewrites a glue into the real entry.
Healing is a documented human runbook (below).

## Options considered

**Pin the seed's `entryUUID` so all sites create the same identity.**
Rejected: `entryUUID` is NO-USER-MODIFICATION; an ordinary `ldapadd` cannot
set it. Not expressible through the seed path.

**Operator-elected founder (cross-cluster coordination).** Rejected on the
same grounds ADR-014 declined hub-and-spoke orchestration: it introduces a
control plane over other sites, in direct tension with the per-site autonomy
requirement. Choosing the founder is a one-line deployment decision; the
operator enforces the consequence (decision 2), not the election.

**Auto-heal the glue.** Rejected (for now, and deliberately). A glue cannot be
deleted (non-leaf); converting it in place means the operator writing into the
user's data tree — the boundary ADR-012 deliberately does not cross — and a
ManageDSAIT modify of `structuralObjectClass` needs the relax control, whose
behaviour here is unverified. Under multi-master, two sites healing
concurrently would mint two fresh CSNs for the same DN and re-run the
conflict. Prevention plus loud detection plus a human runbook is the sane
version of this operator's job.

**Refuse or gate backups from a glue-suffix source.** Rejected: "a backup
always takes a backup" is load-bearing (ADR-014). The record (decision 4) and
the restore-time rejection (decision 3) carry the safety.

**Forbid seeded databases on clusters with external peers (CRD validation).**
Rejected: a founder site legitimately has both `seed` and `externalPeers` —
the founder rule is about *how many* sites seed, not whether a seeded site may
peer. It would also bake a slapd behaviour into the API surface (the ADR-019
argument).

## Consequences

- Multi-site deployments must choose a founder. Peer sites' `SlapdDatabase`
  CRs omit `seed`; their databases reach `Running` empty and fill via
  syncrepl. New user-facing note in `docs/BOOTSTRAP.md`.
- A peer site whose seed was withheld (decision 2) latches
  `SeedApplied=true` without writing anything — correct: the DIT has a
  creator, and the latch's meaning is "seeding is settled", not "we wrote it".
- A glue-suffix pod is now loud three ways: `DataPresent=False/
  DataMissingOnPods`, `slctl inspect` FAILs naming pods and UUIDs, and any
  backup from it carries `SourceSuffixHealthy=False/GlueSuffix`. A restore of
  such an artifact fails in preflight with the cluster still serving.
- **Residual risks, stated honestly:**
  - Two sites deployed simultaneously with seeds (violating the founder rule)
    can still race inside the window before either site's writes replicate;
    the belt cannot close a cross-site TOCTOU. The e2e fixture no longer
    exercises that shape.
  - A seed racing *inbound replication* on the founder's own pod-0 remains
    possible in principle (children-before-parent glue during initial refresh,
    Layer 2 path 2). If a glue forms *while entries are still flowing*, the
    real suffix entry usually still arrives and replaces it (the
    `dn_callback` glue bypass); permanence needs the cookie to settle past the
    suffix CSN first. Detection (decisions 5/6) is the guard.
  - The withhold probe cannot see a glue (hidden from its ordinary search);
    a seed against a glued pod-0 then aborts loudly on the existing
    added-but-not-visible verification and the database sits Degraded —
    a human looks. That is the chosen failure direction.

### Manual heal runbook (glue suffix on one site)

The safe heal is ADR-012 case 2 — rebuild the affected pods from the mesh —
because a glue cannot be deleted (non-leaf) and in-place conversion is
unverified LDAP surgery. Order matters: heal RW before RO (an RO pod
re-initial-syncing from a still-glued provider replicates the glue again).

1. Confirm the glue and note the affected pods (RW and RO):
   `slctl inspect -n <ns> <cluster>` → `suffix-visibility` /
   `suffix-uuid-agreement`, or per pod:
   `ldapsearch -M -b <suffix> -s base '(objectClass=*)' structuralObjectClass entryUUID`.
2. Verify the mesh is otherwise converged and at full redundancy
   (`slctl inspect`; every peer `Synced`).
3. For the glued **RW** pod: delete the pod and its PVCs — the StatefulSet
   recreates it blank and it initial-syncs the full DIT from healthy peers:
   `kubectl delete pod <pod> pvc config-<pod> data-<pod> accesslog-<pod> -n <ns>`
   (ADR-018: verify no finished Job pod still pins those PVCs).
4. Wait for the pod to converge, then verify: an ordinary base search of the
   suffix on that pod returns the real entry, and its `entryUUID` matches the
   peers'. If the re-sync lost the race again (rare; Layer 2 path 2), repeat
   step 3.
5. Then the same for the glued **RO** pod(s).
6. Cross-site note (ADR-016, pod-routed): replacing pods invalidates the
   addresses peers hold for them; expect the measured minutes-scale
   rediscovery window before "peer consumes from us" recovers.
7. Re-run `slctl inspect` on every site; take a fresh backup and confirm
   `SourceSuffixHealthy=True`.

## Amendment (2026-09-14): partition safety stays convention-plus-detection; migration clusters simply omit `spec.seed`

**Cross-site seed withhold rejected as an accepted tradeoff.** A stronger belt
was considered after acceptance: a seed-carrying SlapdDatabase withholds while
any *reachable* external peer already holds the suffix under a foreign
entryUUID, and defers seeding while a configured peer is unreachable. Rejected.
The deferral half is undecidable at bootstrap — "peer not yet created" and
"peer down" look identical from the founder, so the guard that closes the
partitioned-double-seed window also deadlocks every legitimate first
bring-up (a guard that denies a legitimate own state is a permanent stall).
The reachable-peer half alone closes nothing the existing per-pod belt
(Decision 2) does not already cover once the link is up.

The residual is therefore **accepted and named**: founder-only seeding is
enforced by configuration (`spec.seed` on exactly one site's CR — the same
distribution-discipline class as ADR-008's replication-credentials Secret),
not by construction. Two seed-carrying CRs bootstrapped under a partition
re-create the same-DN/different-UUID conflict on heal. What has changed since
the incident is that this state is now **loud** at every layer: `DataPresent`
degrades on the first pod that hides the suffix, restore preflight rejects the
artifact before scale-to-0, backups carry `SourceSuffixHealthy=False`, and
`slctl inspect` names the glued pods and disagreeing UUIDs. Loud-and-rare was
chosen over a guard whose false-positive mode is a silent-forever stall.

(`slapadd`-everywhere with fabricated identities was re-examined in the same
discussion and stays rejected — see Options considered; in short: it moves
bootstrap offline, mints a dormant SID (the ITS#9580 surface ADR-021 exists to
avoid), and its drift failure mode — identical CSN, different content — is
silent, which is strictly worse than the loud class accepted here.)

**Migration scenario (ADR-011): the legacy provider is the founder.** In a hot
migration, no slaptain site seeds at all — the control is spec-level and
already exists: **omit `spec.seed`** on the slaptain-side SlapdDatabase and
let the DIT replicate in from the legacy provider (this is exactly what
`tests/e2e-migration.sh` does: only the fake legacy provider's database
carries `seed:`). Deliberately never auto-detected — whether a cluster is
joining existing data is a statement of intent that belongs in the CR, not an
inference. The Decision-2 withhold additionally covers the misconfiguration:
a seed present while the legacy DIT replicated in first is withheld and
latched with a log line referencing this ADR.

**Recorded limitation** (docs/BACKLOG.md): on a never-seeded database,
`Status.SeedApplied` never latches, so `DataPresent` stays
`NotSeeded/Unknown` and the per-pod suffix-visibility check (Decision 3)
never engages — migration-shaped clusters currently forgo that signal.
*Closed by the amendment below.*

## Amendment (2026-09-14): `DataPresent` engages on every database, seeded or not

The limitation recorded above turned out to be wider than "migration-shaped
clusters", and measurable. On a healthy three-site mesh, right after a fully
green suite, with founder-only seeding in effect:

| site | databases | `DataPresent` | `seedApplied` |
|---|---|---|---|
| founder | `example-db`, `example-db2` | True/`RootEntryVisible` | true |
| peer 2 | `example-db`, `example-db2` | **Unknown/`NotSeeded`** | unset |
| peer 3 | `example-db`, `example-db2` | **Unknown/`NotSeeded`** | unset |

Four of six databases on a fully replicated, fully healthy mesh had this ADR's
own Decision 5 detector switched off — **because they had followed Decision 1**.
Peers omit `spec.seed`, so `Status.SeedApplied` never latches, and
`evaluateDataPresent` returned early on `!SeedApplied` before probing anything.
The same shape covers hot migration (ADR-011), consumer-only clusters
(ADR-010), and `bootstrapFrom`-restored databases (ADR-014), which are never
seeded by construction — a database the operator had itself just loaded with a
full DIT reported "has not been seeded yet" forever.

The root cause is a guard scoped narrower than its invariant, one level down
from this ADR's own: `SeedApplied` answers *"did we write the initial data"*,
which stopped being the same question as *"is data expected here"* the moment
data began arriving by replication — i.e. the moment Decision 1 was adopted.

**Decision 5 is restated:** the per-pod suffix probe runs on every reconcile of
every database, regardless of how (or whether) it was seeded. The seed latch is
no longer a gate on the probe. What needs memory is only the *all-empty*
reading, and only to tell two states apart that look identical on the wire:

- `True/RootEntryVisible` — visible on every assessed RW pod.
- `False/GlueSuffix` — a ManageDsaIT probe positively identified a glue on at
  least one pod. Absolute: it needs no healthy peer to contrast with and no
  knowledge of the database's history, so it fires on a whole site that
  initial-synced from a glued provider (evidence item 5). The operator's probe
  is now the same one `slctl inspect` and the backup source check use.
- `False/DataMissingOnPods` — visible on some assessed pods, absent on others.
- `False/DataMissing` — absent everywhere on a database known to have held data.
- `Unknown/NoDataYet` — absent everywhere on a database that has never been
  seeded, restored, or observed holding data: a peer site waiting for its first
  refresh. **Not an alert** — nothing is known to have been lost. This replaces
  `NotSeeded`, and it is the only branch the seed latch still informs.
- `Unknown/NoReachablePod` — nothing could be assessed.

"Known to have held data" is `seedApplied || restoreApplied || dataObserved`,
the last being a new one-way latch on `SlapdDatabase.status`, set the first time
the root entry is seen on any pod and never cleared. A glue does not set it (it
is the corpse of an entry, not data), and neither does an unreadable pod —
positive evidence only. There is deliberately **no timer**: what makes emptiness
alarming is evidence that data once existed, never elapsed time.

**The ADR-012 boundary is unchanged and now has a test.** `DataObserved` is
memory of data's *presence*, which is exactly the input that could resurrect the
reverted `verifySeedExists` (re-create on *absence*). Nothing on the write path
reads it: `seedNeeded` takes `spec.seed` and `SeedApplied` alone, and a unit
control pins both directions — a seedless database whose data was observed and
then lost must not seed, and a founder whose data is already visible must not
have its first seed suppressed either (withholding remains Decision 2's job,
keyed on a foreign creator's serverID).

**Known gap, recorded rather than closed:** `DataPresent` still probes RW pods
only, while a glue provably propagates to RO pods (evidence item 5). `slctl
inspect` and the per-pod e2e spec do cover RO pods. Including them here widens
the transient `False` window during a legitimate RO initial sync, so it was left
as a separate decision (docs/BACKLOG.md). *Closed by the amendment below.*

## Amendment (2026-09-14): Decision 5's all-pods rule includes the read-only fleet

The gap recorded immediately above is closed. **"Every reached pod" now means
every RW pod *and* every read-only consumer pod** (`spec.readReplicas > 0`);
with `readReplicas: 0` — the common case — nothing changes, not even a probe.

The reason to close it is evidence item 5 itself: the incident site's RO pod
carried the **same glue with the same entryUUID** as its provider. A consumer
does not validate what it initial-syncs; it reproduces it. So an RO-only glue —
the shape you get when the RO fleet is rebuilt against a glued provider, or when
one site's RO pod syncs from that site's glued pod — was invisible to the only
*standing* signal, even though `slctl inspect` and the per-pod e2e spec would
have caught it in a human-initiated look. Reach, not divergence, but the reach
is the point of a standing detector.

**The deferral's concern was real and is answered by a distinct reason, not by
suppression.** An RO pod legitimately lacks the suffix while it performs its
initial sync, and the all-pods rule turns that into a False. Rather than
excusing it (a timer, or an exemption while the pod is young — both forbidden:
ADR-024's "no timers as safety mechanisms", and an exemption keyed on age cannot
distinguish a slow sync from a broken one), the verdict is reported honestly and
**named separately**:

- `False/DataMissingOnReadOnlyPods` — every reached *writable* pod has the root
  entry, at least one read-only pod does not. Expected transiently during an RO
  initial sync; persistent means that replica is broken or its syncrepl stanzas
  are not converging.
- `False/DataMissingOnPods` — unchanged, and it **outranks** the RO reason: any
  writable pod hiding the entry reports this whatever the RO fleet shows.
- `False/GlueSuffix` — unchanged and role-blind. A glue is corruption, never a
  sync stage, so a glued RO pod reports `GlueSuffix` and names itself.

The split is what makes the wider window affordable: an operator's alert rule
can page immediately on `DataMissingOnPods` / `GlueSuffix` and give
`DataMissingOnReadOnlyPods` a fuse longer than an initial sync. A single
undifferentiated reason would have forced the whole condition down to the
slowest tolerable fuse — which is the real cost the deferral was worried about.

**The RO verdict does not affect the `SlapdDatabase` phase**, following the
`readOnlyReadyReplicas` precedent (informational, phase-neutral). Nor does
anything here become a write: ADR-012's boundary is untouched, `dataObserved`
still latches on positive evidence only (an RO sighting is evidence that data
exists here — the latch is role-blind by design, and any later disappearance it
makes alarming is genuinely alarming).

Messages stay byte-identical on a cluster without read-only replicas, so a
`readReplicas: 0` deployment sees no churn at all; the pod-list construction is
a pure function (`suffixProbeTargets`) pinned by that positive control.

## Amendment (2026-09-15): the RO fuse exists, and the first measurement of it

The 2026-09-14 read-only amendment predicted that an alert rule would "page
immediately on `DataMissingOnPods`/`GlueSuffix` and give
`DataMissingOnReadOnlyPods` a fuse longer than an initial sync". The e2e
post-suite gate (`check_data_present_every_site` in `tests/e2e.sh`) is the first
concrete instance of that rule, and building it produced two numbers and one
mechanism worth recording here.

**The budgets are reason-aware.** Reasons that cannot be legitimately transient
after a completed suite — `GlueSuffix`, `DataMissingOnPods`, `DataMissing`,
`NoDataYet`, `NoReachablePod`, an absent condition, anything unrecognised — fail
in 60s. `DataMissingOnReadOnlyPods` gets 600s. Both still end in a failure:
never failing on the RO reason would restore precisely the blind spot evidence
item 5 measured. Note this made the gate *stricter* where it matters — a genuine
glue suffix previously enjoyed the same three-minute grace period as a benign
re-sync.

**Measured, on a single-site lab with a few-dozen-entry DIT:** an RO replica
whose data was wiped and whose syncrepl was blocked reported
`False/DataMissingOnReadOnlyPods` within **5s** of coming up empty, stayed False
for the whole 78s the block stood (a broken replica does not self-heal — the
fuse genuinely fires), and recovered to `True/RootEntryVisible` **18s** after
the block was lifted. The 600s budget is therefore ~33x the only recovery ever
measured — and it remains **a guess for a production-sized DIT**, which the
big-DIT lane in `docs/BACKLOG.md` is what would calibrate.

**A reproduction subtlety worth keeping:** deleting an RO pod and its PVCs — what
the ADR-014 restore specs do — does *not* produce this reason. During the
rebuild the pod is unreachable, which the probe scores as "not assessed", and by
the time it serves, a small DIT has already synced. The reason requires a pod
that is **up and serving but empty**.

**And a consequence of ADR-002's cadence that any consumer of this condition
must know:** `DataPresent` is phase-neutral observability, so a healthy
`SlapdDatabase` re-evaluates it only on the 5-minute resync floor. A `False`
that has already healed can therefore sit in status for minutes with nothing
wrong anywhere. Any alerting rule (or gate) with a budget under five minutes is
measuring condition staleness rather than cluster state unless it forces a fresh
evaluation first. The e2e gate pokes each pending database with a metadata write
to do exactly that; a production alert rule should instead set its fuse longer
than the resync interval.

## Related

- ADR-012 — seed one-shot, pod-0-pinned (the intra-site version of this
  invariant); amended by this ADR for the withhold belt.
- ADR-014 — backup/restore; amended by this ADR (preflight glue rejection,
  `SourceSuffixHealthy`); the 2026-09-12 source-honesty amendment is the
  pattern decision 4 extends.
- ADR-008 — the `cn=replication` identity the probes bind with; the CSN
  blindness amendments this class rides through.
- ADR-011 / ADR-017 — the serverID arithmetic the withhold belt keys on.
- ADR-016 — pod replacement invalidates peer addresses (runbook step 6).
- ADR-018 — Job reaping (evidence-capture race; runbook step 3 caveat).
- ADR-019 — "never reuse an `olcDatabase={N}` DN across a delete": prior art
  for slapd-internal conflict/renumbering behaviour becoming operator law.

## References

All public. slapd line numbers are from the openldap-2.7.1 release tarball.

- `servers/slapd/syncrepl.c` — non-present delete demotes a non-leaf to glue
  (`:5112-5151`); `syncrepl_add_glue_ancestors` creates bare glue ancestors
  (`:5195-5333`, glue objectClass template `:4938`); `dn_callback` skips the
  newer-CSN guard for glue but no-ops on identical CSN (`:6100-6135`);
  cookie/"not new enough" gate (`:4478-4487`).
- RFC 3296 — ManageDsaIT control (the only way to see a glue entry).
- RFC 4533 — LDAP Content Synchronization (entryUUID as sync identity).
- OpenLDAP Admin Guide 2.7, Replication:
  <https://www.openldap.org/doc/admin27/replication.html>
