# ITS#9398: the accesslog refresh storm, and how to see it

A delta-syncrepl consumer is told `sync cookie is stale`, full-refreshes,
reconnects with the same cookie, and is told the same thing again — forever. The
pods burn CPU at a few hundred percent and every health signal slaptain has
reads green.

This document records the **mechanism**, traced to slapd 2.7.1 source, and a
**local detector** found on a live reproduction on 2026-10-06. The detector is
the new part: the storm was previously believed to be invisible to every shipped
tool, diagnosable only by counting stale-cookie lines in a pod log.

---

## The signature

One entry, on one pod, contradicts itself. On the accesslog database:

```
# site-1 slapd-2, cn=accesslog-example-db
minCSN:     …172852…#001  …173048…#002  …173939…#003  …174857…#066  …210850…#011  …211026…#013
contextCSN: …211428…#001  …211428…#002  …211629…#003                              …211026…#013
```

`minCSN` names six serverIDs. `contextCSN` tracks four. **`minCSN` claims the log
can replay for sids `011` and `066`, which `contextCSN` says this pod does not
track at all.**

That is the whole trigger. The predicate is one-directional:

> **A `minCSN` sid that is absent from `contextCSN` on an accesslog DB ⟹
> permanent refresh storm.**

The reverse (`minCSN` ⊂ `contextCSN`, i.e. the log knows about fewer sids than
the DB tracks) is benign and common — it was observed on healthy, idle pods in
the same mesh.

---

## Why it is permanent — the test the provider cannot pass

`servers/slapd/overlays/syncprov.c`, OpenLDAP 2.7.1, inside
`syncprov_op_search`:

**Pass 1 — line 3320**, comment *"If there are SIDs we don't recognize in the
cookie, drop them"*:

```c
for (i=0; i<srs->sr_state.numcsns; ) {
    for (j=i; j<numcsns; j++)
        if ( srs->sr_state.sids[i] <= sids[j] ) break;
    if ( j == numcsns || srs->sr_state.sids[i] != sids[j] ) {
        /* drop this sid from the consumer's cookie */
```

`sids[]`/`numcsns` are the **provider's own** contextCSN (set at 3289–3294 from
`si->si_sids`). So every sid in the consumer's cookie that the provider does not
track is deleted from the consumer's state. In the capture above that removes
`011` and `066`.

**Pass 2 — line 3479**, the `ad_minCSN` branch:

```c
for ( i=0, j=0; i < a->a_numvals; i++ ) {        /* a = the log DB's minCSN */
    while ( j < numcsns && minsids[i] > sids[j] ) j++;
    if ( j < numcsns && minsids[i] == sids[j] &&
            ber_bvcmp( &a->a_vals[i], &srs->sr_state.ctxcsn[j] ) <= 0 )
        continue;
    /* else → LDAP_SYNC_REFRESH_REQUIRED, "sync cookie is stale" */
```

Read the `&&` chain in evaluation order, because the first term is the one that
fails. `sids[]`/`numcsns` here are **still the provider's own contextCSN**, the
same snapshot pass 1 used — *not* the pruned cookie. The consumer's values enter
only as `srs->sr_state.ctxcsn[j]`, indexed by the **provider's** `j`.

So for a `minCSN` sid the provider's own `contextCSN` lacks, the `j`-walk stops
at the next larger sid, `minsids[i] == sids[j]` is false, and the chain
short-circuits: **`ber_bvcmp` is never evaluated**. Take the second sample's
numbers — `minsids[i] = 0x12 = 18`, provider `sids[] = {1,2,3,17,19,33}`: the
walk halts at `19`, `18 == 19` fails, refused. Nothing the consumer could have
presented changes that, which is why every consumer is refused identically:

```
syncprov_op_search: consumer not within recorded mincsn for DB's mincsn=20261006090530.902309Z#000000#012#000000
SEARCH RESULT tag=101 err=4096 … text=sync cookie is stale
```

**The verdict is reached on the provider's own two attributes alone. The
consumer is named in the message and had no say in it.**

What pass 1 is actually *for*, then, is index alignment — it makes
`srs->sr_state.ctxcsn[j]` correspond to `sids[j]` so the value comparison is
meaningful in the normal case where the sid *is* tracked. The sid at the heart
of the storm does get pruned there, for the very same reason it fails pass 2
(the provider lacks it), but that prune is a sibling symptom, not the cause of
the refusal. (An earlier version of this document had it the other way round.)

The code has no branch for `minCSN ⊄ contextCSN`: it treats the two attributes
as describing one sid set, and ITS#9398 is exactly the unclean shutdown that
makes `minCSN` the larger one — it is written on every write, `contextCSN` only
at checkpoint or clean exit.

That is also why the predicate in *The signature* is not a correlate of the bug
but **the code's own failure condition**, restated as something you can query.

Nothing converges *by this path*: the consumer full-refreshes, reconnects, sends
a cookie, and the provider's unsatisfiable test runs identically. There is no
window and no backoff, which is why the CPU cost is flat rather than bursty.

It is not unconditionally permanent, though — see *It can clear by itself*. A
full refresh carries the provider's whole `contextCSN`, and a subsequent write
under the absent sid restores it on the provider side — which is the vendor's
documented cure, and the one a retired serverID makes impossible. Whether it
happens appears to depend on which link is storming.

---

## Why we reach that branch at all

Line 3457 is the **third** arm of a chain:

```c
if      ( !BER_BVISNULL( &si->si_logbase ) )  syncprov_play_accesslog(…)
else if ( si->si_logs )                       syncprov_play_sessionlog(…)
else if ( ad_minCSN != NULL && si->si_nopres && si->si_usehint )   /* ← here */
```

The storm is on the syncprov overlay of the **accesslog database itself** — the
DB a delta-syncrepl consumer searches with `base="cn=accesslog-<db>"`. That
database has no `logbase` of its own, and per **ADR-022** it deliberately
carries no sessionlog. So both earlier arms are skipped and adjudication falls
to the minCSN hint, with the prune/indict contradiction above.

ADR-022's reasoning is unchanged and still correct: a successful sessionlog
replay on an accesslog DB would displace the minCSN guard and under-replicate
silently. What was never recorded is the **consequence** — that the choice
routes all accesslog-DB syncrepl into this one branch. Whether a sessionlog on
the accesslog DB would avoid the storm, and at what cost to the guard ADR-022
protects, is an open question (see *Open questions*).

---

## Measuring it: the first live reproduction (2026-10-06, pre-0.3.0 fixture)

Three-site mesh, `slaptain-testing`, after a full-gate e2e run. All 18
`slctl inspect` checks passed throughout.

| pod | sid | `minCSN` sids | `contextCSN` sids | verdict |
|---|---|---|---|---|
| s1 slapd-2 | 3 | 001,002,003,011,013,**066** | 001,002,003,013 | **storm source** |
| s1 slapd-0 | 1 | 001,002,003,011,013,066 | = same set | ok |
| s1 slapd-1 | 2 | 001,002,003,011,013 | ⊃ (has 066 too) | ok (benign direction) |
| s2 slapd-0/1/2 | 17/18/19 | 001,002,003,011,013,066 | = same set | ok |
| s3 slapd-0/1/2 | 33/34/35 | 001,002,003,011,013,066 | = same set | ok |

Only `example-db` is affected; `example-db2`'s log is clean on all nine pods.

CPU at the same moment:

```
site-1  slapd-0 410m   slapd-1 408m   slapd-2 1681m
site-2  slapd-0 438m   slapd-1 439m   slapd-2 1685m
site-3  slapd-0   4m   slapd-1   7m   slapd-2  727m
```

### Two instances in one mesh, not one

The table above samples a single moment, and it is **later** than the CPU
reading. Read together with the pod log, the mesh held **two** broken providers:
`s1 slapd-2` *and* `s2 slapd-2`, mutually refusing each other and each also
refusing its own in-cluster consumers.

600 lines off `s1 slapd-2` show it as victim and culprit at once. As **provider**
it issued 30 stale refusals to four distinct consumers:

```
14x  cookie=rid=023,sid=002     s1 slapd-1          (in-cluster)
12x  cookie=rid=023,sid=001     s1 slapd-0          (in-cluster)
10x  cookie=rid=029,sid=023     sid 0x023 = 35 = site-3 pod 2
 9x  cookie=rid=029,sid=013     sid 0x013 = 19 = site-2 pod 2
```

As **consumer** it restarted refresh ten times, all on one rid:

```
10x  ldap_sync_search: rid=030 starting refresh
```

`slctl rid decode 030` resolves to segment 1, peer site index 1 — **site-2**. At
`replicasPerPeer: 1` ADR-031 maps local pod `i` to remote pod `(i+k) % N`, so the
provider refusing it is `s2 slapd-2`.

**The CPU distribution is the clearest evidence, and the idle pods carry it:**

| | pods 0/1 | pod 2 | RO |
|---|---|---|---|
| site-1 | **409 / 411** | 1698 | 412 |
| site-2 | **437 / 443** | 1696 | 4 |
| site-3 | **11 / 13** | 735 | 6 |

Sites 1 and 2 each host a broken provider, so their in-cluster consumers burn
~400m being refused. Site-3 hosts none, so its pods 0 and 1 sit at 11-13m, and
only its pod 2 is hot — the one whose cross-site link lands on a broken
provider elsewhere.

A single-source model cannot produce that. If `s1 slapd-2` were the only broken
pod, site-2's pods 0/1 had no reason to burn 440m while site-3's identically
placed pods idled at 11m: both sites are equidistant from site-1. **Count the
sites whose ordinals 0/1 are busy — that is the number of broken providers.**

So CPU *does* localise the fault, better than first thought — but at SITE
granularity, not pod granularity. Within a site the busiest pod is as likely to
be a refused consumer as the broken provider; only the `minCSN`/`contextCSN`
predicate separates those two.

### It can clear by itself

`s2 slapd-2` reads clean (6 = 6) in the table above, yet the log shows it
refusing `s1 slapd-2` minutes earlier, and a one-off read of that same pod
during the investigation returned **7 minCSN against 6 contextCSN**. The most
likely reading is that it was broken and **healed between samples**.

That matters twice over:

- the storm is **not** an unconditional steady state, so "it never recovers" is
  too strong a claim. A plausible mechanism is the full refresh itself: it
  delivers the provider's complete `contextCSN`, which would restore the
  consumer's missing sid and dissolve the contradiction;
- it makes a single clean sample **non-conclusive for absence**. One sample
  still proves a storm when the predicate fires, but a clean read only means
  clean *now*. Sample on the CPU signal, not on a schedule.

The inverted question is then the interesting one: not why these pods storm, but
why `s1 slapd-2` did **not** heal, when its in-cluster links to pods 0 and 1
were working throughout — only `rid=030`, the cross-site link, was restarting.

### Unexplained

`s2 slapd-readonly-0` sat at **4m** while site-1's RO burned 412m. If
`s2 slapd-2` was refusing its in-cluster consumers the way `s1 slapd-2`
demonstrably was, that RO should have been refused too. No evidence was
captured either way before the lab was wiped.

---

## The detector

Per accesslog DB, per pod, read as `cn=admin,cn=config` (ADR-020 denies the data
rootDN):

```bash
slctl ldapsearch -n <ns> --database <db> --pod <ord> --as config \
      -b cn=accesslog-<db> -s base '*' +
```

and assert **every `minCSN` sid appears in `contextCSN`**. It needs no cross-pod
comparison and no log scraping: the contradiction is local to one entry.

A firing sample is conclusive. A **clean** sample is not — the condition can
clear between reads (see above), so absence of the signature means "not right
now", not "never happened". Pair it with the CPU signal: a site whose pod
ordinals 0 and 1 are busy has a broken provider in it.

Implemented 2026-10-11 as the `slctl inspect` check **`accesslog-mincsn`**: it
fails naming the pod, the log and the missing sids, warns when a log could not
be read, and passes only when every log was read clean. `tests/e2e.sh` runs it
on every site after the suite (`check_no_refresh_storm_every_site`), so a run
that leaves a pod storming ends red. Its first live reading:

```
[FAIL] accesslog-mincsn  slapd-0 cn=accesslog-example-db2: minCSN sid 003,013 not in contextCSN;
                         slapd-0 cn=accesslog-example-db: minCSN sid 012 not in contextCSN — ITS#9398: …
```

on a pod that had answered about 4,000 operations a second for half an hour,
while `csn-convergence` on the same run read OK and a full three-site e2e had
just gone 104/0.

Still open: `dataloss_recovery_test.go` counts stale-cookie **log lines**
instead of asserting this predicate. That spec's gating claim is separately
stale — it reads as if the storm were `-ol26`-only, which the ITS#9398
investigation disproved.

---

## Second sample — the predicate used predictively (2026-10-06, post-0.3.0)

The first e2e run after tagging 0.3.0 failed on the stale-cookie spec: 4075
lines on `slapd-0` of site-1, limit 50. The mesh was left running and the
detector was applied **before** any log was read, to all nine RW pods × both
databases. It fired exactly once:

| site | pod | db | `minCSN` sids | `contextCSN` sids | verdict |
|---|---|---|---|---|---|
| s1 | slapd-0 | example-db | 001,002,003,011,**012**,013,021 | 001,002,003,011,013,021 | **storm source** |
| s1 | slapd-1 | example-db | 001,002,003,011 | ⊃ (all 7) | ok (benign direction) |
| s1 | slapd-2 | example-db | all 7 | = | ok |
| s2, s3 | all | example-db | all 7 (s2 pods 0/1: 6) | ⊇ | ok |
| all | all | example-db2 | 001,002,013,021 | = | ok |

The log then named the same sid, 7190 times in the ten seconds the container
had retained:

```
syncprov_op_search: consumer not within recorded mincsn for DB's mincsn=20261006090530.902309Z#000000#012#000000
SEARCH RESULT tag=101 err=4096 … text=sync cookie is stale
```

Every one of the 7190 refusals carried that same `mincsn=` value. **The
predicate picked the culprit out of nine pods with no log access and no
cross-pod comparison.**

### One broken provider, five refused consumers

Unlike the first sample, this instance is single-source, and the fan-out is
exact. `s1 slapd-0` logged **817 stale refusals/s and zero refresh restarts of
its own** — pure culprit, not also a victim. Five consumers looped, each on
exactly one rid, and every one of those rids resolves to `s1 slapd-0`:

| consumer | rid | resolves to | restarts/s |
|---|---|---|---|
| s1 slapd-1 | 021 | local ordinal 0 | 291 |
| s1 slapd-2 | 021 | local ordinal 0 | 302 |
| s1 slapd-readonly-0 | 021 | local ordinal 0 | 314 |
| s2 slapd-0 | 029 | peer site index 0 = site-1 | 271 |
| s3 slapd-0 | 029 | peer site index 0 = site-1 | 274 |

The CPU map follows from that list and nothing else:

```
site-1  slapd-0 1538m  slapd-1 457m  slapd-2 455m  RO 400m
site-2  slapd-0  373m  slapd-1  10m  slapd-2   9m  RO   6m
site-3  slapd-0  369m  slapd-1   4m  slapd-2   4m  RO   6m
```

This refines the site-granularity heuristic from the first sample: the busy set
is *the broken provider plus its consumers*, and `replicasPerPeer: 1` means each
peer site contributes exactly one. Site-1 is busy wall-to-wall because all three
of its local consumers (including the RO) hit the broken pod; sites 2 and 3
contribute one pod each. The culprit is the pod at **3-4× the others**, and it
is the one with no `starting refresh` lines of its own.

### Renumber residue is not required

The 0.3.0 fixture was recreated under the hex stride, so no `066` orphan exists
anywhere in this mesh. The indicted sid `012` is **hex 0x12 = 18 = site-2
slapd-1** — confirmed by reading `olcServerID` off that pod, which was idle at
10m and healthy throughout. A single live sid is sufficient; the first sample's
orphan was indeed a passenger.

### The data DB stayed perfect, and every check stayed green

On the storming pod, the *data* database tracked `012` normally
(`20261006090533.923138Z…#012`), byte-identical to both peers. Only the
accesslog DB's own `contextCSN` lacked it, three seconds earlier in wall time
than the data DB's value for the same sid.

Consequently, while `s1 slapd-0` burned 1538m and refused 817 searches/s:

- `slctl inspect --short` → **18 passed, 0 warnings, 0 failed**, `csn-convergence`
  included;
- `SlapdCluster` status → `Running`, `Ready=True`, `PodsResponsive=True`,
  `ReplicationConverged=True CSNsMatch`, both external peers `Synced`.

### The storm destroys its own evidence

Retained container log on each pod, measured at the same moment:

| pod | retained window |
|---|---|
| s1 slapd-0 (culprit) | **3 s** |
| s1 slapd-1 / slapd-2 / RO | 10 / 14 / 14 s |
| s2 slapd-0, s3 slapd-0 | 25 s, 38 s |
| every idle pod | 297 s (the full `--since=5m`) |

At ~14k lines/s the pod rolls its log away in seconds. This has a direct
consequence for the e2e: `countInSlapdLogs` counts "since `lossStartedAt`", but
cannot see further back than retention, so the spec's **4075 is a floor imposed
by log rotation, not a measurement**. The real figure over the ~10 minutes
between the PVC loss and the assertion is on the order of 10⁵–10⁶. Any future
threshold work should read this as a binary signal, not a magnitude — or better,
assert the `minCSN ⊄ contextCSN` predicate instead.

### What this sample says about the origin

The PVC-loss pod was `slapd-1` (its three PVCs were recreated at 09:09:44); the
broken accesslog is on `slapd-0`, whose PVCs date from 08:58:10 and which
restarted at 09:06:38 with the rest of the cluster during the cleanup-policy
spec's roll. The stale `minCSN` value itself is stamped 09:05:30, i.e. **before
both events**.

So the damage is not on the refreshed pod but on a *peer* of it, and it predates
the refresh. That narrows open question 2 without closing it: the dataloss spec
is in the frame as a trigger for the refusals, not as the writer of the
contradiction.

---

## What this is *not*

`066` is decimal **102** — site-2 pod 1's serverID under the pre-2026-10-05
decimal stride, frozen at 17:54:21Z. It is renumber residue (ADR-028 amendment),
and it is a **passenger here, not the cause**: the indicted sid in the log is
`011` (17), which is live, and every healthy pod in the table carries `066` in
both lists while idling at single-digit CPU. The second sample settles it — a
mesh with no renumber residue at all stormed just as hard, on a live sid.

It does make recovery strictly worse. Sid 102 is dead, so once a pod's
`contextCSN` loses `066` while its `minCSN` retains it, that half of the
contradiction can never be repaired by replication — no entry will ever be
written under 102 again. A live sid can heal; an orphaned one cannot.

---

## Open questions

1. **Should the accesslog DB carry a sessionlog after all?** This is the third
   time a problem has turned out to be specific to its absence. A sessionlog
   would take arm 2 of the chain instead of the minCSN hint, which is the arm
   that contains the contradiction. Against it stands ADR-022's reason, which is
   real and was proven: a successful replay there displaces the minCSN guard and
   under-replicates *silently* — a worse failure than a loud storm. The
   question is whether a sessionlog sized so that it reliably *misses* (and thus
   falls through to a correct full refresh) buys anything over the current
   behaviour, or whether arm 2 and arm 3 fail the same way for the same reason.
   Needs reading `syncprov_play_sessionlog` with the same care applied to
   `syncprov_op_search` here.
2. **How does the broken pod's accesslog `contextCSN` lose a sid its own
   `minCSN` still carries?** Both runs included the dataloss-recovery spec,
   which deliberately destroys a pod's PVCs. The second sample narrows it: the
   contradiction sat on a *peer* of the refreshed pod and its `minCSN` value
   predated both the refresh and the pod's own restart — so the refresh is a
   plausible trigger of the refusals, but not the writer of the contradiction.
   Still unverified.
3. **Is there a repair short of rebuilding the pod?** `minCSN` is maintained by
   slapd, not by the operator, and ADR-026 R2 forbids the operator from
   correcting state it does not own on evidence like this.
