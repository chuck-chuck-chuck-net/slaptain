# ADR-036: A pod that has not recovered its own authorship is read-only

**Status:** Proposed
**Date:** 2026-10-16

## Context

A pod that loses its volumes and is rebuilt under the same `olcServerID` never
gets back **the entries it wrote itself** — if anything writes to it before it
has caught up. Everything authored by its peers arrives normally. The gap is
invisible to every signal slaptain publishes.

Measured on a three-pod single-site lab, 2026-10-16, two rebuilds of the same
pod differing in one write:

| | rebuild kept **write-free** | rebuild **written to** on first answer |
|---|---|---|
| own-sid entries recovered | **50 of 50** | **0 of 50** |
| its `contextCSN` for its own sid | back at the **pre-crash** value | stands at the **trigger write** |

The written-to rebuild ended **1260 entries short** of its peers — its whole
authorship history — while `ReplicationConverged` read `True/CSNsMatch` and
`slctl inspect` reported 15 passed, 0 failed.

### The mechanism, confirmed in the 2.7.1 sources

Three lines, in order:

1. **A consumer's sync cookie is its own database's `contextCSN`**
   (`syncrepl.c:1051-1075`, reading `si_contextdn`, which is the backend's own
   suffix — `syncrepl.c:2282`). A local write stamps `contextCSN[own sid]`.
2. **A delta-syncrepl consumer falls back to a full refresh of the main
   database only when its cookie has zero CSNs** (`syncrepl.c:734-736`):
   `si_logstate == SYNCLOG_LOGGING && !si->si_syncCookie.numcsns`. Zero writes
   → full refresh → the whole DIT, own authorship included. **One** write →
   non-zero → it stays in log mode and asks only for a delta. This boolean is
   the entire difference between the two columns above; the window is not a
   race, it closes permanently the moment anything writes.
3. **The provider then suppresses, per sid** (`syncprov.c:3043-3056`,
   `/* Don't send old entries twice */`): any entry whose `entryCSN` carries a
   sid present in the consumer's cookie, with a value `<=` the cookie's value
   for that sid, is dropped from the response and never sent.

The consumer-side counterpart (`do_syncrep2: … CSN too old, ignoring`,
`syncrepl.c:1282`) logged **zero** times during the incident, which is the
measurement that places the loss at step 3 rather than at delivery: the entries
are not offered and refused, they are never sent.

**The class: a watermark used as a receipt.** `contextCSN[s]` means "I have seen
everything from `s` up to this point" — true for a consumer that has only ever
*received* `s`'s writes, false for a consumer that *is* `s` and has lost its
disk. Suppression reads the cookie, not the contents.

### Why no CSN-based check can see it

After the refresh the rebuilt pod's `contextCSN` is **genuinely identical** to
its peers': the suppressed entries are below the watermark by construction, and
the own-sid value is the trigger write, which the peers replicated *from* the
pod. The vectors are not merely equal-looking, they are correct. So
`ReplicationConverged`, `slctl inspect`'s `csn-convergence`, peer `Synced` and
`DataPresent` are structurally blind to this — not under-tuned, blind. It is the
ADR-025 lesson again, with a different corpse.

### Why this is not a corner case

- **ADR-012 case 2 is overstated as written.** "Lose one pod's data, syncrepl
  recovers it" holds for everything except that pod's own authorship — on a
  multi-provider cluster taking writes everywhere, a share of the directory
  proportional to that pod's traffic.
- **ADR-013's deferral rests on a premise this refutes.** It deferred the
  convergence probe because the class it protects against is "a UX wart during
  rolling updates (brief stale-or-empty reads), not data correctness". It is
  data correctness, and it is permanent.
- **ADR-023 would do it on purpose, once per ordinal.** Its per-step gate is
  `ReplicationConverged` — the blind signal — so rolling replacement would
  declare each rebuilt pod converged while it is short its own history.
- **Whole-site recovery is the worst instance, not a footnote.** A rebuilt site
  reuses its `serverIDIndex` and therefore its sids (ADR-028), and remote sites
  still carry `contextCSN` entries for them — nothing ever prunes a sid from a
  vector. One write on the returning site's first pod tells every remote site
  "I am current for `011`", and that site's entire history becomes unsendable,
  mesh-wide.
- **The operator pulls the trigger itself.** `ensureReplicationUser` creates
  `cn=replication,<suffix>` — a write into the *replicated data tree* — on the
  first RW pod that answers, which on a rebuilt pod-0 is the blank one, in the
  reconcile step before the syncrepl stanzas are written. That is removed
  separately under the ADR-027 amendment of this date; this ADR assumes it gone
  and does not depend on it.

### The shape of the problem, for readers who have run Galera

Readers who have operated Galera will recognise the shape of
`safe_to_bootstrap` / `galera_new_cluster`: **authority cannot be inferred while
the network that holds the answer is missing**, so a human supplies it. The
difference is what happens while we wait — Galera refuses to *start*; a slaptain
pod starts, serves reads and keeps replicating, withholding only writes. The
names are deliberately not reused (below).

## Options considered

### A. Per-pod content comparison (entry counts) — rejected

`cn=monitor`'s `olmMDBEntries` is an O(1) `mdb_stat` of the backend's `id2entry`
(`back-mdb/monitor.c:251-256`), so comparing entry counts across pods is cheap
and sees contents rather than watermarks. Rejected as a **check**:

- Under deletion churn the sign flips. A pod behind on applied deletes holds
  *more* entries than its peers while missing own-sid adds; the two errors
  cancel, and a poisoned pod can read equal — or high — indefinitely. The count
  is a confounded measure of two independent lags.
- A count sees missing entries, never **stale** ones: a suppressed modify leaves
  the entry present and the count equal.
- Decisively: this is a defect class *defined by* every signal reading clean.
  Shipping a heuristic verdict into it manufactures one more guard whose
  evidence can be a plausible lie — this project's own named failure (ADR-025),
  authored by us.

It survives as an **observation** in `debug-dump`, for a human to interpret, with
no verdict attached.

### B. `olmSRSyncPhase` as the release gate — rejected

slapd publishes the consumer's phase per syncrepl instance — `Refresh`,
`Fallback Refresh`, `Persist` (`syncrepl.c:7286`, `:7446-7460`), present in both
2.7.1 and 2.6.14 so neither image lacks it — and `Fallback Refresh` is the
server naming the very state at `syncrepl.c:734` that this defect turns on.
Tempting, and wrong in both directions:

- **Not sufficient.** A poisoned pod reaches `Persist` quickly. The refresh did
  not fail; it completed, correctly, having been told there was nothing to send.
  `Persist` is a true statement about the *exchange* and says nothing about the
  *contents*.
- **Not necessary.** A pod can satisfy the real condition mid-refresh, and can
  sit in `Persist` without ever satisfying it.

Kept as **diagnostics**: phase plus `olmSRLastCookieRcvd` / `olmSRLastContact`
is what a human wants when asking why a pod is still held.

### C. A data-convergence readiness probe — rejected

ADR-013 deferred it and ADR-030 left it deferred. It stays rejected, on sharper
grounds than "expensive":

- **A kubelet-local probe cannot decide this.** The only purely local signal —
  "my `contextCSN` is empty" — is equally true of pod-0 on a brand-new cluster,
  and under `OrderedReady` a pod-0 that never goes Ready means pod-1 is never
  *created*. That is ADR-030's own reason for rejecting `readinessGates`, and it
  applies unchanged.
- **Readiness is the wrong blast radius anyway.** It removes a pod from the
  ClusterIP Service only. A direct pod IP, a per-pod NodePort, the toolkit,
  `slctl ldapmodify --pod` and the operator itself all still write — and ADR-030
  decision 3 (headless publishes NotReady addresses) *guarantees* the operator
  keeps its path.

### D. A fresh serverID for a rebuilt pod — rejected

Then its peers' entries are foreign to it and arrive normally. But ADR-017 makes
identity a function of the fixed ordinal, write-once-at-boot; ADR-028's hex
stride caps `podsPerSite` at 15, so there are no spare slots to burn per
rebuild; and every retired sid lingers in every peer's `contextCSN` forever —
the "dormant serverID" tax that upstream's own TODO (`syncprov.c`, the
`FIND_CSN` comment) names as a cause of expensive refreshes.

### E. Rebuild from data, not from replication — not an answer, but kept

ADR-014's restore path (`slapcat` a peer, `slapadd` the new pod) consults no
cookie and has no own-sid blind spot. It is correct and it remains the
documented alternative — but it is an outage path and does nothing for the
unplanned PVC loss this ADR is about.

### F. The data database is read-only until it has recovered its own authorship — chosen

## Decision

### 1. A data database is created read-only

`olcReadOnly: TRUE` goes into the ADD by which the operator creates the data
database on a pod. There is no window before it: before that ADD the backend
does not exist, so nothing can write to it. The invariant — *no local write
before the pod's own history is back* — holds from the database's first instant
rather than from whenever a controller gets round to it.

This is a total belt, our own writes included: `olcReadOnly` sets
`be_restrictops |= SLAP_RESTRICT_READONLY` (`bconfig.c:2086-2088`), enforced by
`backend_check_restrictions`, which is called **only from the frontend operation
handlers** (`add.c:286`, `delete.c:149`, `modify.c`, `bind.c`, `compare.c`,
`extended.c`) and exempts **no identity, rootDN included**
(`backend.c:1385-1397`). Syncrepl applies entries by calling `op->o_bd->be_add` /
`be_modify` directly on the backend (`syncrepl.c:4502`, `:4838`, `:3374`),
bypassing the frontend entirely — which is why a read-only database keeps
filling, and why ADR-010's consumer-only mode works at all.

### 2. The release condition: own-sid dominance, per database

> For a pod with serverID `s`, and **per database**: release when every peer's
> `contextCSN[s]` is `<=` this pod's `contextCSN[s]`.

Per database, because `contextCSN` is per database and `olcReadOnly` is per
database — a pod may legitimately have one database released and another held.

Three properties earn this its place over everything in the Options:

- **It measures the quantity actually at risk** — the pod's own authorship —
  rather than a proxy for it.
- **It cannot be satisfied by cheating, because decision 1 forbids the cheat.**
  `contextCSN[s]` advances only by writing locally (blocked) or by *receiving*
  entries authored under `s`. Satisfaction is therefore proof of recovery, not
  evidence of it.
- **Green-field falls out of it.** No peer holds a value for `s` ⇒ vacuously
  satisfied ⇒ released. No latch, no flag, no special branch.

The 2026-10-16 A/B run is this condition measured on both sides: the write-free
rebuild's own-sid `contextCSN` "came back at the pre-crash value" (satisfied by
recovery); the written-to rebuild's "stands at the trigger write" (satisfied by
the cheat decision 1 removes).

### 3. Evidence must be complete, and the unit is the declared peer

Absence of evidence is never evidence — the rule already load-bearing in
`CSNQueriesIncomplete` and `PartiallyVerified`. A declared peer we could not ask
is not a peer that said no. So:

> Release when **every declared peer answered** and none reports a
> `contextCSN[s]` above this pod's. Any unreachable declared peer ⇒ incomplete
> ⇒ hold.

**Declared peers, not `meshRef`.** `meshRef` is a derivation mechanism, not a
fact about the topology — ADR-028's whole point is that `externalPeers` is what
the operator derives *from* a mesh. By the time this gate runs, a hand-wired
`externalPeers` list and a mesh-derived one have produced the same thing: an
inventory of peers that may remember this pod's sid. Gating on how it was
spelled would be ADR-028's own mistake inverted. Single-site is the degenerate
case — zero declared external peers — not a special branch.

**The completeness unit is the peer *site* for remote peers, and every expected
RW pod for the local cluster.** Per-pod completeness across sites is airtight but
makes a recovering site hostage to any single remote pod restart, which is
routine in a three-site mesh. The residual is stated in Consequences and it is
bounded, not catastrophic.

### 4. Release is one-way per database incarnation

Once released, a database is not re-gated. After release the pod's cookie is
honest and its own-sid history is legitimately its own, so re-applying read-only
would cost availability for nothing. A rebuilt volume means a new ADD under
decision 1, so the gate returns exactly when the database is new again — the
incarnation, not the pod, is the unit.

### 5. The undecidable case gets a human, and an auditable one

Whole-site loss **plus** partition from every other site is **undecidable from
inside the site**: the pods are empty and the only witnesses to whether they have
a past are the sites that cannot be reached. There is no local fact to consult —
the volume that might have held one is the thing that was lost, which is why we
have strictly less to work with than Galera's persisted hint, and why the human
assertion is the whole of the answer rather than a fallback.

It is narrow. One pod rebuilt with in-cluster peers up: decidable. A site
rebuilt with any other site reachable: decidable. Only "my site is gone *and* I
cannot reach anyone who remembers it" needs a person — already a
declare-a-disaster moment.

```
SlapdForceWritable (sfw)          # imperative, one-shot, spec immutable
  spec.clusterRef
  spec.databases[]                # optional; default all
  spec.assertion                  # NoPriorHistory | AcceptHistoryLoss
  spec.reason                     # free text → event + status
  status.phase                    # Pending|Honoured|Refused
  status.evidence                 # which declared peers were unreachable, and
                                  #   every contextCSN[s] read, AT HONOUR TIME
```

- **`NoPriorHistory`** — mesh genesis. **Checkable, and checked:** if any
  reachable peer reports a `contextCSN[s]` for a pod being released, the
  assertion is contradicted by positive evidence and the request is **refused**.
  The common path carries a belt.
- **`AcceptHistoryLoss`** — the disaster case. Uncheckable by nature, which is
  precisely why it exists as its own spelling.

`status.evidence` is recorded at honour time either way, because after an
`AcceptHistoryLoss` nothing else will hold a record of what was discarded.

A **standing spec field was rejected**, on ADR-023's stated grounds and with
more force: a field whose blast radius is "silently discard a site's history",
read months later in a spec nobody re-reads before a patch bump, is a loaded
gun. Replacement of lost authority is an event, not a policy.

### 6. The names are not taken from Galera

The analogy belongs in Context, where its edges can be stated; the API names our
own mechanic. Two reasons, the second decisive:

- The semantics differ in the dangerous direction. `galera_new_cluster` is
  required on **every** cluster's first start; this is required only in the
  undecidable case. An almost-right name imports expectations silently, and
  nobody re-reads the doc to find the clause where the analogy breaks.
- **"Bootstrap" is already taken four times in slaptain** — `docs/BOOTSTRAP.md`,
  the init container's `bootstrap.sh`, `status.phase=Bootstrapping`, and
  `SlapdDatabase.spec.bootstrapFrom` — all of them about bringing a thing into
  existence, which is exactly the wrong connotation for a gate that releases
  writes on a running pod.

The kind names the effect (what you grep a cluster for); `spec.assertion` names
the stake (what you must write out by hand to mean it).

### 7. A held pod says so, and says why

A condition on the cluster naming the pod, the database, its serverID, the
`contextCSN[s]` it is waiting to reach, and — when evidence is incomplete —
exactly which declared peers could not be asked. A frozen cluster must be
legible, not mute; the whole defect being fixed here is one that said nothing.

## Consequences

- **A bounded, stated residual from per-site completeness.** A remote pod that is
  itself behind on `s` under-reports, so the max over what we reach is a *lower
  bound*. Releasing at bound `B` when the truth is `T` loses entries authored by
  `s` in `(B, T]` — the remote site's own convergence lag — rather than the whole
  history. That is a real silent loss and is recorded as one. It is also strictly
  better than today, which has no gate at all.
- **A cluster that lost every pod's data freezes read-only**, because no pod can
  ever reach dominance. That is the correct outcome — the data is gone and a
  human must restore from backup (ADR-012's "inaction with surfaced
  observability") — and decision 7 is what keeps it from being a mystery.
- **Multi-site genesis gains one ceremony**; single-site gains none. A new mesh's
  founder applies one `SlapdForceWritable` with `NoPriorHistory` as step 0 of
  `docs/MULTI-SITE.md`. Single-site clusters and the whole single-site e2e suite
  declare no external peers and are unaffected by construction. The multi-site
  e2e lane pays the one object.
- **`olcReadOnly` gains a second driver and must keep one writer.** ADR-010 sets
  it from cluster mode (consumer-only); this ADR sets it from pod state. They
  compose as a max — consumer-only ⇒ always read-only; peer ⇒ read-only while
  held — and that must be **one predicate in one place**, never two writers of
  one attribute (ADR-026 R2's shape, applied to our own state).
- **Seeding is ordered behind release.** Read-only exempts no identity, so a
  seed cannot land on a held database. On a green-field cluster nothing holds it
  (decision 2, vacuous), so the ordinary path is unchanged; on a mesh the
  founder's genesis assertion precedes its seed, which is the correct order
  anyway.
- **`status.dataObserved` is deliberately NOT an input.** It was the obvious
  "is there anything to catch up" oracle and it is unsound for the case that
  matters most: a site rebuilt along with its Kubernetes state has a fresh CR,
  reads `dataObserved: false`, and would be released immediately — answering
  "go ahead" in exactly the situation that destroys a site's history. Decision 2
  derives the same branch from the peers, who remember what local state cannot.
  This is also why the gate is not a return of `verifySeedExists` (ADR-012): it
  **withholds writes on absent evidence**, it never **creates on absent
  evidence**. The directions are opposite and that distinction is the whole of
  ADR-025's withhold belt.
- **ADR-013's deferral premise is withdrawn**, and ADR-012 case 2 needs its
  qualifier. Both are amended alongside this ADR. Per ADR-035 the collision is
  decided on the merits: ADR-013 deferred a *readiness probe* on a cost argument
  that still holds — this ADR does not build one (option C stays rejected) — but
  its "UX wart, not data correctness" premise is refuted by measurement and
  cannot survive.
- **ADR-023 must adopt this gate.** Its per-ordinal "`ReplicationConverged`
  holds" step is the blind signal; it becomes "the replaced pod's databases have
  been released". ADR-023 is Proposed and unimplemented, so this costs an
  amendment now rather than a redesign later.
- **ADR-030's convergence question is answered, but not where it looked.** Ready
  still means "slapd answers LDAP" — unchanged, and option C keeps it that way.
  Data convergence is answered by a different lever, on a different object, with
  a different failure mode: a held pod serves reads and refuses writes, loudly,
  instead of vanishing from a Service.
- **A held pod is a legible operational state, not an outage.** It answers
  searches, it binds, it replicates, it appears in Service endpoints. Writes get
  `unwillingToPerform` with a condition that says why. The failure direction is
  deliberate: a loud stall over a silent destruction.
- **What does not change:** `cn=config` is still node-local and operator-owned;
  the RID scheme, serverID derivation, accesslog design, syncrepl stanza shape,
  credential model, PVC layout, pod template, RBAC and reconcile cadence are all
  untouched. No image change. Nothing in the steady-state write path of a
  released database is different from today.

### Test discipline

Red before green, per tier:

1. **The defect's own repro, as a committed e2e** — destroy **pod-0**'s volumes
   (today's `dataloss_recovery_test.go` deliberately targets `slapd-1`), write
   to it the instant it answers, and assert that entries authored by the
   destroyed pod **under its own sid** come back. No spec in the suite asserts
   that today. It must be shown red against current code first; it is the
   2026-10-16 measurement turned into a test.
2. **The release predicate as a pure function** — a table over (own sid, local
   `contextCSN`, per-peer `contextCSN`, which peers answered) returning
   hold/release/undecidable. Assertions written from this ADR before the
   implementation, including the positive control that must still **release**,
   so a hold-everything implementation cannot pass vacuously.
3. **The genesis and disaster assertions** — `NoPriorHistory` refused against a
   peer that reports a `contextCSN[s]`; `AcceptHistoryLoss` honoured with
   `status.evidence` populated.

Replication-path change: this gets a multi-site cycle before merge, or the
commit says plainly that multi-site is unvalidated.

## Related

- ADR-008 — per-database CSN comparison; this ADR consumes the same per-sid
  `contextCSN` readings and inherits its "unreadable evidence never counts"
  rule.
- ADR-010 — `olcReadOnly` as consumer-only's lever; the second driver this ADR
  must compose with.
- ADR-012 — seed is one-shot; case 2's "replication recovers it" is the claim
  this ADR qualifies, and `verifySeedExists` is the shape decision 5 is
  explicitly not.
- ADR-013 — deferred the convergence probe; its premise is refuted here, its
  cost argument against a probe is upheld.
- ADR-017 / ADR-028 — serverID is a function of the fixed ordinal and the
  declared site index; why option D is not available, and why a rebuilt site
  returns on the same sids.
- ADR-023 — rolling replacement; its per-ordinal gate must become this one.
- ADR-025 — the glue-suffix class: CSN health reading clean while data diverges,
  and the withhold-on-positive-evidence belt decision 5 copies.
- ADR-026 — R2; why `olcReadOnly` must have one writer, and why the operator
  does not repair the data tree.
- ADR-027 — the node-local replication identity, whose placement keeps three of
  the four operator write paths out of the data tree; its amendment of this date
  removes the fourth.
- ADR-030 — Ready means slapd answers LDAP; unchanged, and why readiness is the
  wrong lever here.
- ADR-035 — how the ADR-013 collision above is decided.
- `docs/BACKLOG.md` — "A pod that loses its disk never gets back the entries IT
  wrote, and every health signal says it did": the measurement, the A/B table
  and the evidence chain.
