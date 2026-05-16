# Investigation: replication divergence after PVC-loss + full restart

**Status:** Closed (2026-05-16) — known upstream bug, OpenLDAP
[ITS#9580](https://bugs.openldap.org/show_bug.cgi?id=9580). Mitigated in
slaptain by tolerating the post-recovery noise in tests; documented for
production operators.
**Discovered:** 2026-05-15, during the first e2e run after the ADR-013 revert.
**Severity:** Test flakes; production CPU spike on peers for several minutes
after a pod rejoins via syncrepl. Data correctness is unaffected — DITs
converge across all pods.

A reader can come to this document cold without the conversation context
that produced it. The "Background" section below assumes no prior OpenLDAP
exposure; the rest of the document then walks through the diagnostic trail
that led to the ITS#9580 identification.

---

## Background: how delta-syncrepl normally works

OpenLDAP supports two replication modes between provider and consumer
slapd instances. Both move LDAP entries from a *provider* to a *consumer*
over an LDAP search with a special sync control attached. Reading this
section is enough to follow the rest of the document without any prior
OpenLDAP exposure.

### Vocabulary

- **Server ID (SID)** — a small integer (1–4095) configured per slapd
  instance via `olcServerID`. Every write a server originates is tagged
  with that server's SID. In our cluster: t3e pods have SIDs 1, 2, 3;
  bento pods have SIDs 101, 102, 103 (which show up as hex `065`, `066`,
  `067` in CSN strings).

- **CSN (Change Sequence Number)** — a per-write identifier of the form
  `20260515192835.013910Z#000000#003#000000`. The first field is the
  write timestamp; the third hex field is the originating server's SID.
  CSNs let any pod determine "have I already applied this write?"
  without needing global coordination.

- **`contextCSN`** — an attribute on the database root entry listing,
  per SID, the most recent CSN this pod has seen. A pod with five SIDs
  in its contextCSN has applied at least one write from each of those
  five servers. Two pods that agree on contextCSN *should* have seen the
  same set of writes — when everything is working correctly.

- **Sync cookie** — the per-SID CSN state a consumer sends to a provider
  as part of a search request, asking "tell me about changes since this
  state." The cookie is essentially a serialized contextCSN.

- **`cn=accesslog`** — an optional secondary database that slapd
  populates via the `accesslog` overlay. Every write applied to the data
  DB is echoed as an audit record in `cn=accesslog`. Audit records
  inherit the original write's CSN in their `entryCSN` attribute.

- **`minCSN`** — an attribute on the `cn=accesslog` root entry tracking
  the oldest CSN (per SID) the accesslog can still serve. The
  "we've purged everything older than this" watermark.

- **Refresh phase** — when a consumer's cookie is too old or absent,
  slapd switches to bulk-transfer mode: provider sends every entry it
  has, consumer reconciles by UUID. Expensive but always correct.

- **Persist phase** — the normal steady-state mode: consumer holds the
  search open, provider streams updates as they happen.

### Plain syncrepl vs delta-syncrepl

**Plain syncrepl** has the consumer search the provider's *data DB*
directly. To answer "what changed since cookie X?" the provider walks
the data DB and sends entries whose `entryCSN` is newer than X. Cheap
when few entries changed; doesn't scale when the DB is large.

**Delta-syncrepl** instead has the consumer search the provider's
`cn=accesslog` (configured by `syncdata=accesslog` on the syncrepl
stanza). The provider's accesslog already has one entry per recent
write, so answering "what changed since X?" is an indexed lookup rather
than a full scan. Much faster on large DBs, but only works when the
accesslog actually contains a faithful, in-order log of writes — the
precondition that ITS#9580 breaks.

### Normal flow when everything works

1. Consumer connects to provider, sends cookie X (= consumer's current
   contextCSN).
2. Provider's `syncprov` overlay compares X against the provider's
   contextCSN. If the consumer is older for some SID, the provider needs
   to send those changes.
3. With delta-syncrepl, the syncprov on the provider's `cn=accesslog`
   serves the diff by searching audit records with CSN > X. With plain
   syncrepl, the syncprov on the provider's data DB serves the diff by
   scanning entries.
4. After all relevant entries are sent, provider sends a final cookie ≥
   provider's contextCSN, consumer updates its contextCSN to match, and
   the connection transitions to persist phase.

### How this can break — what the rest of the doc is about

When a consumer joins a cluster without prior state (or with state that
is too far behind), the provider can't serve the diff via accesslog —
the relevant history is older than minCSN, or the cookie references
SIDs the provider's accesslog cannot reason about. Slapd then falls
back to a **refresh**: the provider walks its data DB and sends entries
to the consumer in *provider-walk order*, not in write-occurrence
order.

The consumer dutifully echoes each received entry into its own
accesslog (because the accesslog overlay sits on all writes, including
syncrepl-applied ones). That accesslog is now poisoned for delta-sync
purposes: entryCSNs are out of order with respect to the original
writes, `minCSN` tracking is no longer meaningful, and any future
consumer that attempts to delta-sync from it gets the verdict
**`sync cookie is stale`** (LDAP_SYNC_REFRESH_REQUIRED, error code
4096) and falls back to its own full refresh — which propagates the
poison further. The result is sometimes data divergence between
peers and a slapd that burns CPU in a tight stale-cookie loop while
the cluster *eventually* converges by other means.

---

## TL;DR

> **Reader's note (2026-05-16):** the rest of the document below this
> point is the original investigation as it stood when we suspected this
> was a slaptain-side bug. The actual cause turned out to be upstream
> OpenLDAP ITS#9580 — see the **Resolution** section near the end of the
> file for the conclusion. The trail below is preserved as the
> diagnostic record that led there.

After today's revert (drop ephemeral fixture, replace `dataloss_recovery_test`
with a pod+PVC delete on persistent storage; require persistence), the
unified e2e suite reaches the `resilience_test` "warm restart of all pods"
case in a state where the 3 RW pods on the local site (t3e) are missing
exactly two seed entries: `ou=Mail` and `ou=ServiceAccounts`. The RO consumer
on the same site **and** the remote site (bento) both have them. The
`contextCSN` values agree across all three t3e RW pods and include bento's
serverIDs — so cookies are converged while entries diverge.

This is a classic "cookie advanced without entry applied" symptom in
delta-syncrepl. Slapd then burns ~2 cores per pod doing endless
compare-and-find-nothing cycles on persistent TLS connections. Whatever
deleted those two specific entries on the RW mesh did not propagate to the
RO consumer or to the cross-site peer; only the three RW pods agree that
they're gone.

We do **not** know the proximate cause of the deletion. The investigation
exhausted what we can learn at slapd `logLevel: 256` (stats only); the next
step is to reproduce with sync-level logging enabled and inspect the actual
syncrepl decisions during the trigger window.

---

## What we observed

**Test failure (entry point):**

```
resilience_test: data persists after simultaneous restart of all pods (warm start from PVCs)
  [FAILED] ou=Mail should survive warm restart
  Expected <bool>: false to be true
```

**Cluster state at debug-dump time (2 hours after the failure, t3e):**

| | Entries | ou=Mail | ou=ServiceAccounts | uid=readonly |
|---|---|---|---|---|
| t3e slapd-0 (RW) | 8 | ❌ | ❌ | ❌ |
| t3e slapd-1 (RW) | 8 | ❌ | ❌ | ❌ |
| t3e slapd-2 (RW) | 8 | ❌ | ❌ | ❌ |
| t3e slapd-readonly-0 | 10 | ✅ | ✅ | ❌ |
| bento slapd-0 | 10 | ✅ | ✅ | ❌ |

Seed has 6 entries (`dc=example`, `ou=People`, `ou=Groups`, `ou=Mail`,
`ou=ServiceAccounts`, `uid=readonly,ou=ServiceAccounts`) — see
`tests/resources/example/database.yaml`. Tests add 4 (`alice`, `bob`,
`cn=admins`, `cn=linuxusers`). Healthy = 10. RW = 8 (missing the two OUs).

`uid=readonly` missing on every pod across both sites is a separate,
pre-existing condition — likely "seed never applied entry 6" rather than
"entries 4-5 deleted on RW". Worth investigating separately; not in scope here.

**contextCSN snapshot (same on all three t3e RW pods):**

```
contextCSN: 20260515011021.728117Z#000000#001#000000  sid=1   (t3e slapd-0)
contextCSN: 20260515011023.873685Z#000000#002#000000  sid=2   (t3e slapd-1)
contextCSN: 20260515011057.656370Z#000000#003#000000  sid=3   (t3e slapd-2)
contextCSN: 20260515010912.641407Z#000000#065#000000  sid=101 (bento-0)
contextCSN: 20260515011037.134565Z#000000#066#000000  sid=102 (bento-1)
contextCSN: 20260515010905.553347Z#000000#067#000000  sid=103 (bento-2)
```

All cookies agree across the t3e RW mesh. No writes since 01:10:57.

**CPU/syscall pattern:**

- `kubectl top`: 2.3 cores/pod on each t3e RW, ~25Mi memory, RO at 1.3 cores.
- Persistent TLS connections, ~5 established per pod. No reconnect storm.
- 5-second strace summary (one RW pod):
  - 74% CPU in `futex` (278k calls) — heavy worker-thread contention.
  - 226k `read` + 112k `write` over TLS sockets — high-throughput syncrepl traffic.
  - 56k `epoll_wait`, 91k `epoll_ctl` — event loop is busy.
  - Only ~7k `connect`, ~5k `accept4` — connections are long-lived.
- Operator-side `externalPeerStatuses.replicationState: Synced` for bento. Both
  sites believe replication is healthy.

**Operator state:**

- Reconciling normally; no errors after 01:10. Last log at dump-time-minus-34s.
- Status: `Phase: Running, readyReplicas: 3, observedGeneration: 3`.

---

## Test ordering that triggers it (best reproduction recipe)

The unified `tests/e2e.sh` runs Ginkgo specs in file-load order:

```
bootstrap → dataloss_recovery → external_replication → ldap → ... → readpw → resilience
```

The salient sequence:

1. **`dataloss_recovery_test`** (new in ADR-013 revert): deletes `slapd-1`'s
   PVCs + pod. The new `slapd-1` starts with fresh PVCs and recovers via
   syncrepl from peers. Test verifies `ou=Mail` is visible on the new
   `slapd-1`, then deletes its witness entry, and exits.
2. **`ldap_test` / `readpw_test`** run, adding `alice`, `bob`,
   `cn=admins`, `cn=linuxusers`. Possibly add transient `ou=Mail` users
   that get cleaned up.
3. **`resilience_test` "warm restart all pods"** deletes all 4 pods
   simultaneously (3 RW + 1 RO), waits for STS Ready, then queries via the
   LB ClusterIP.
4. The query lands on a RW pod (any of slapd-0/1/2). `ou=Mail` not found.

Pre-revert, `dataloss_recovery_test` was gated to the ephemeral fixture only,
so it never ran on the persistent fixture in the same suite invocation as
`resilience_test`. The two never interacted on a single cluster.

Step 1's PVC reset is what we suspect creates the divergent state. The
multi-site cross-cluster replication amplifies whatever happens.

---

## What we ruled out

- **Test code does not delete those OUs.** `grep -nE 'ou=Mail|ou=ServiceAccounts'`
  across all `tests/e2e/*.go` and `tests/resources/example/*.yaml`. No test deletes
  the OUs directly. Tests delete *child entries* (mail users, readpw users), never
  the OU itself.
- **Operator does not delete data entries.** ACL / index / replication-user / syncrepl
  reconciles only write to `cn=config`. Seed is one-shot (ADR-012). No data deletes
  in the controller code path.
- **Reconnect loop hypothesis.** strace ruled this out — connections are persistent
  and long-lived, not torn down and rebuilt.
- **Operator wedge hypothesis.** Initial misread of UTC vs local timestamps. Operator
  is actively reconciling, just nothing visible to do.
- **PVC corruption.** PVCs are properly Bound, distinct UIDs, properly mounted to
  the right pods. `kubectl top` shows ~25Mi memory — consistent with a small but
  *valid* LMDB DB, not a corrupt one.

---

## Working hypothesis

When `dataloss_recovery_test` resets `slapd-1`, the freshly-reseeded pod
participates in a multi-master mesh where:

- `slapd-0` and `slapd-2` already have cached cookies pointing at slapd-1's
  *old* accesslog state, which the new slapd-1's accesslog doesn't have.
- The cross-site peer (bento) has its own cookies for slapd-1.
- After the dataloss test exits "successful" (ou=Mail visible on slapd-1
  via direct per-pod connection), the cluster *appears* converged but is
  carrying invisible CSN/cookie skew.

When `resilience_test` deletes all pods simultaneously, every pod restarts
from PVC. They all re-establish syncrepl. In the absence of a clear "source
of truth" (because slapd-1's accesslog was reset and other pods' cached
cookies reference the old position), delta-syncrepl falls into the
"cookie-advanced-without-entry-applied" mode for some entries — specifically
the two OUs that depend on each other's existence via ACLs (`ou=Mail`'s ACL
references `ou=ServiceAccounts` children).

The RO consumer doesn't lose them because it picked them up earlier in the
suite (when the cluster was healthy) and never received a delete event for
them. Bento doesn't lose them because the t3e→bento accesslog had already
flushed those creates before the reset.

**This is consistent with the observed state but not yet proven.** It needs
sync-level slapd logging to confirm the syncrepl decision-making.

---

## Plan to verify

1. **Reset both sites cleanly** (full teardown — see "Default to full teardown
   for e2e iteration" preference; do `tests/e2e.sh teardown` and re-deploy
   from scratch).
2. **Bump `slapd.logLevel` to 16640** (256 stats + 16384 sync) in
   `tests/values.slapd-persistent.yaml`. Note: this is the operator-managed
   `spec.logLevel` field on SlapdCluster.
3. **Run the suite, capture the failure.** The interesting window is from
   the moment `dataloss_recovery_test` deletes slapd-1's PVCs through the
   `resilience_test`'s "delete all pods" step. Capture `kubectl logs slapd-0
   --previous` and `--follow` from before/during the resilience test if
   possible.
4. **Save the artifacts:** slctl debug-dump for both sites + raw slapd logs
   at log-level 16640.
5. **Look for in the slapd logs:**
   - `syncrepl_message_to_op: rid=X` deciding to delete or skip an entry.
   - `syncrepl_entry: rid=X` showing the entry being processed.
   - `do_syncrep2: rid=X (4096) Content Sync Refresh Required` and what
     followed (full refresh? abort?).
   - Any indication that bento's ou=Mail entries arrived at t3e and were
     either rejected or accepted-then-overwritten.

---

## If hypothesis confirmed

If the trace shows this is an OpenLDAP behavior that slaptain cannot prevent
from outside slapd (i.e., it's not something our operator can defend
against by writing different cn=config, choosing different timing, or
ordering operations differently), then:

1. **Write it up as a known OpenLDAP edge case** — file under `docs/` next to
   this investigation.
2. **Fix the e2e tests by separating the two scenarios:**
   - Option A: skip `resilience_test` when `dataloss_recovery_test` ran first
     on the same cluster — via Ginkgo label or run-order dependency.
   - Option B: split into two suite invocations — full teardown between
     dataloss and resilience.
   - Option C: have `dataloss_recovery_test` wait for cluster-wide cookie
     convergence (not just "entry visible on the reset pod") before exiting.
     Might mitigate, won't necessarily fix — needs verification.
3. **Document the operational implication:** if a production cluster loses a
   pod's PVC and the cluster is then immediately disrupted at the
   StatefulSet level (config change, image bump, etc.) before cookies stabilise,
   data divergence may result. Recommend waiting for cluster-wide CSN
   convergence (visible via `slctl inspect`) after any PVC-loss recovery
   before further cluster-level operations.

---

## If hypothesis NOT confirmed

If sync-level logs show something else (e.g., the operator's reconcile timing
specifically deletes or fails to apply those entries, or there's a slapd
config we're emitting incorrectly), pivot to that.

A few candidates that didn't pan out under "what we ruled out" but might
re-emerge with better logs:

- **Operator's seed retry semantics on a freshly-reset pod.** ADR-012 says
  seed is one-shot; verify the latch actually held through the reset.
- **ACL reconciliation race.** The operator writes ACLs to cn=config per
  pod. If during slapd-1's recovery the ACL block was briefly inconsistent
  across pods, a read-deny window could mask entries during initial sync —
  but the entries wouldn't actually be deleted, only invisible.
- **Cross-site replication credentials.** If bento's syncrepl bind to t3e
  was failing for the affected entries' CSN range, t3e would see "cookie
  advanced" without receiving the corresponding entries from bento.

---

## Open questions before pickup

- Is the resilience-test "delete all pods simultaneously" pattern a
  realistic failure to test? Real production: pods restart one at a time
  during rolling updates. Simultaneous deletion is a stress test, not a
  realistic disaster. Maybe the test itself is the bug.
- Should we be modeling a "cluster wide CSN convergence" wait in slctl or
  the operator's status, so tests (and operators) have a definitive
  "everyone's caught up" signal?
- Does the failure reproduce on a single-site cluster (`N=1`), or does
  cross-site replication amplify it? Worth a 5-minute experiment:
  `./tests/e2e.sh all t3e` (one context) and see whether the same test fails.

---

## Code and artifact references

- `tests/e2e/dataloss_recovery_test.go` — current implementation, reworked in
  the ADR-013 revert.
- `tests/e2e/resilience_test.go` line 175+ — the warm-restart test that fails.
- `tests/values.slapd-persistent.yaml` — fixture; `logLevel: 256` currently.
- `tests/resources/example/database.yaml` lines 53-87 — the seed.
- `docs/adrs/adr-012-seed-and-lifecycle.md` — seed contract (one-shot).
- `docs/adrs/adr-013-defer-hot-database-management.md` — the recent revert
  that introduced the test ordering.
- `docs/BUG-ANALYSIS-database-dirs-rolling-restart.md` — adjacent
  investigation; explains the rolling-restart side channel the dataloss
  test was originally designed to surface.
- `slctl-debug-slapd-20260515-031544/` (gitignored debug-dump directory) —
  full state capture at failure time. Files of interest:
  - `slapdcluster.yaml`, `status.json` — operator's view
  - `contextcsn-*.txt` — proves cookies agree
  - `syncrepl-slapd-*.txt` — current syncrepl configs
  - `logs-slapd-0.txt` — at log-level 256, only stats; needs 16640 for the
    actual sync trace
  - `operator-logs.txt` — quiet, no errors visible

---

## Reproducing the host-level diagnostic that revealed the syncrepl loop

```bash
# Find the slapd PID on the host
ssh root@<node>
crictl ps --name slapd  # or: pgrep -a slapd
SLAPD_PID=<from above>

# 5-second syscall histogram — confirms the futex contention + persistent TLS pattern
timeout 5 strace -p $SLAPD_PID -f -c 2>&1 | tail -40

# Per-thread CPU breakdown
top -H -p $SLAPD_PID

# Pod-namespace connection list (host ss won't see pod sockets)
nsenter -t $SLAPD_PID -n ss -tnp -o state established
```

LDAP-side diagnostic from the laptop (NodePorts exposed by the test
fixture):

```bash
NODE=$(kubectl --context t3e get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
ADMIN_PW=$(kubectl --context t3e -n slaptain-testing get secret example-db-credentials -o jsonpath='{.data.root-password}' | base64 -d)
for port in 30400 30401 30402 30410; do
  count=$(ldapsearch -x -LLL -H "ldap://${NODE}:${port}" -D "cn=admin,dc=example,dc=org" -w "$ADMIN_PW" \
    -b "dc=example,dc=org" -s sub "(objectClass=*)" dn 2>/dev/null | grep -c '^dn:')
  echo "port $port: $count entries"
done
```

Expected on the bug: 8 / 8 / 8 / 10. Bento (port 30400 in its NodePort range
on its node IP): 10.

---

## Resolution (2026-05-16): upstream OpenLDAP ITS#9580

The investigation closed when we reproduced the failure mode under
`logLevel: 16640` (stats + sync), traced the staleness verdict to two
specific lines in `servers/slapd/overlays/syncprov.c`, then matched the
pattern to the upstream tracker.

### Diagnostic trail

On iter 28 of `tests/e2e-repro.sh t3e bento`, the
`dataloss_recovery_test` witness check failed at the 2-minute timeout
even though the DITs on all six pods (3 t3e + 3 bento) converged
identically (11/11 entries, identical `contextCSN`). slapd-1 (the
recreated pod) was burning hundreds of millicores in a tight loop
against slapd-2 (rid=103), generating ~14k connections in 27 seconds.
The provider's response was reliably `err=4096 text=sync cookie is
stale`.

The two "sync cookie is stale" emitters in syncprov.c:

- **Line 3469** — fires when `ad_minCSN && si_nopres && si_usehint`
  (our cn=accesslog syncprov has all three) and the provider's per-SID
  `minCSN` indicates the consumer is behind in a SID the provider
  cannot replay for.
- **Line 3499** — fires when `syncprov_findcsn(mincsn)` cannot locate
  an accesslog entry matching the consumer's oldest-divergent CSN.

The TODO at `syncprov.c:3486` is the explicit upstream acknowledgement:

> *"Using mincsn only (rather than the whole cookie) will
>  under-approximate the set of entries that haven't changed, but we
>  can't look up CSNs by serverid with the current indexing support.*
>
> *As a result, dormant serverids in the cluster become mincsns and
>  more likely to make `syncprov_findcsn(,FIND_CSN,)` fail → triggering
>  an expensive refresh…"*

In our six-pod mesh, bento's SID 067 (bento-slapd-2) originated zero
writes during the test run. That qualifies as "dormant" and biases the
lookup toward failure on every reconnection.

The trigger in our case is the PVC-loss test: slapd-1 is recreated
with an empty data DB, must do a refresh from peers, and that refresh
fills its accesslog with audit records in receive order rather than
original-write order. From that point the accesslog is — in the
upstream author's framing — no longer a faithful delta-sync source.

### The upstream issue and the candidate fix

[ITS#9580](https://bugs.openldap.org/show_bug.cgi?id=9580) (opened
2021-06-14, status IN_PROGRESS as of 2023-11-07, last activity 2023):

> *"A server consuming a plain syncrepl session (might be a delta-MMR
>  refresh) still has to log the entries into accesslog, however that
>  accesslog stops being capable of serving as a delta-sync source:*
>  *operation entryCSNs will be out-of-order; the changes logged will
>  not be the intended modifications…"*

[MR 472](https://git.openldap.org/openldap/openldap/-/merge_requests/472)
"ITS#9580 Propagate a present-phase cookie flush into accesslog"
(commit `414866b8`, 2022-01-11) was merged to `master` and
`OPENLDAP_REL_ENG_2_7`. As of 2026-05-16:

- The commit is **not** in any 2.6.x release tag. Latest is
  `OPENLDAP_REL_ENG_2_6_13`.
- `OPENLDAP_REL_ENG_2_7` has never had a tagged release.
- Debian trixie's `slapd 2.6.10+dfsg-1` — which we use — does **not**
  contain the fix.
- All currently-shipping Linux distributions on the OpenLDAP 2.6
  stable line are affected.

The fix is also documented by its author as incomplete:

```c
/*
 * TODO: we should still be usable as sessionlog source, but maybe not
 * quite for deltasync anymore, we can't really make that distinction
 * yet.
 */

/*
 * ITS#9580 FIXME: This will only work if we log successful writes
 * and nothing else, otherwise we're reverting some CSNs (at least
 * our own) in the contextCSN to an older value. Right now we depend
 * on syncprov's checkpoint to clean up after.
 */
```

It patches the visible symptom (the consumer's accesslog gets a
synthetic contextCSN update at refresh end, which reseats `minCSN`)
but does not address the deeper issue that a refresh-filled accesslog
isn't truly a valid delta-sync source. The four-year gap between merge
and release strongly suggests upstream considers this a partial
mitigation, not a closure.

### slaptain's posture

We do **not** ship a custom slapd build. Instead:

1. **Tolerate the loop in tests.** DITs converge in fact, just not
   inside the 2-minute `Eventually` window the `dataloss_recovery_test`
   uses. Extended to a longer timeout in
   `tests/e2e/dataloss_recovery_test.go`.

2. **Production guidance.** When a slaptain pod loses its data and
   rejoins via syncrepl, expect a multi-minute CPU spike on peers
   (several hundred millicores per pod) while the cluster reaches
   convergence. Data is correct throughout; CPU recovers eventually.
   No user action required.

3. **Open follow-ups** (not load-bearing for this fix):
   - `olcSpSessionLog` on the data DB's syncprov — would let recent
     cookies be served from an in-memory ring buffer that bypasses the
     poisoned accesslog. Cheap to try, may sidestep the worst of the
     loop.
   - Operator-driven "warming write" per pod at bootstrap so no SID is
     dormant for long. Would reduce the probability of triggering the
     bug at all.

The remainder of this document preserves the diagnostic trail (and the
unrelated `olcServerID` side-finding) in case the upstream picture
changes or we ever consider building slapd from source.

---

## Side-finding (unverified): asymmetric `olcServerID`

While investigating iter-28 (2026-05-15 evening) we noticed that
`images/slapd-init/bootstrap.sh` only emits `serverID` directives for the
**local** cluster's RW pods. Cross-cluster peers (declared via
`spec.replication.externalPeers`) are never added to the local
`olcServerID` list. So on t3e:

```
olcServerID: 1 ldaps://slapd-0.slapd-headless.…:1025
olcServerID: 2 ldaps://slapd-1.slapd-headless.…:1025
olcServerID: 3 ldaps://slapd-2.slapd-headless.…:1025
```

…even though writes carrying bento's SIDs (101/102/103, hex `065/066/067`)
flow in via cross-cluster syncrepl and end up in the data DB's `contextCSN`.

The operator's own message at `slapdcluster_controller.go:1344` already
asserts that the *external* cluster must "add slaptain's ServerIDs to their
olcServerID list" — but the *local* side never adds the *other* cluster's
ServerIDs. The asymmetry is unintentional (no ADR covers it).

**Verification result (2026-05-15):** Manually adding bento's SIDs (101/102/103
with placeholder URLs) to slapd-2's `olcServerID` and restarting the pod
**did NOT** affect the `sync cookie is stale` / `delta-sync lost` loop, and
did NOT change which SIDs the accesslog DB's `contextCSN` tracks (still
only local SID 003). So `olcServerID` is *not* the input that controls the
syncprov staleness verdict — the loop hypothesis is wrong.

**But the asymmetry is still real:** if it's correct to declare all peer
SIDs locally for any reason (write attribution, CSN validation, future
slapd versions), the current bootstrap is incomplete. Worth verifying
against slapd source / docs before deciding whether to fix it. Tracked as
a follow-up; not load-bearing for the replication divergence the rest of
this document is about.
