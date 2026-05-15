# Investigation: replication divergence after PVC-loss + full restart

**Status:** Open — needs verification with sync-level logging
**Discovered:** 2026-05-15, during the first e2e run after the ADR-013 revert
**Severity:** Test-only (so far) — only reproduces under the specific test ordering
introduced by ADR-013's revert. No production manifestation observed.

This document is the parking handoff for a problem we surfaced but did not
fully root-cause. A reader can come to it cold without the conversation
context that produced it.

---

## TL;DR

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
