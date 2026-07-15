# Reconcile & Replication Fixes

Log of bugs found and fixed in the operator's reconciliation loop and replication setup.
Kept to prevent running in circles when debugging similar issues in the future.

---

## 2026-07-15: standalone → HA transition leaves pre-existing pods without modules, schema, and serverID

**Symptom:** A cluster deployed standalone (replicas=1, replication off) and later
flipped to HA (replicas=3, readReplicas=1, `replication.enabled=true` — the demo's
Act-6 walk) never converges. `slctl inspect` shows `naming-contexts FAIL: slapd-0:
missing cn=accesslog` while everything else looks green — pod-0 has syncrepl
stanzas and `multiProvider: TRUE`. Operator logs show, every 10s:

```
ensure syncprov overlay at slapd-0...: add syncprov overlay:
LDAP Result Code 21 "Invalid Attribute Syntax": objectClass: value #1 invalid per syntax
```

Consumers additionally log `syncrepl_message_to_entry: rid=101 mods check
(objectClass: value #0 invalid per syntax)` / `do_syncrepl: rid=101 rc 21 retrying`
for entries that use a custom schema, and pod-0's contextCSN carries sid `#000#`.

**Root cause — three instances of one mechanism:** bootstrap.sh's entire config
generation is guarded by `if [[ ! -d "$CONFIG_DIR/slapd.d/cn=config" ]]` and the
config dir lives on a PVC — so it runs exactly once per volume lifetime. Anything
replication-related that only that block writes is a function of the cluster's
shape *at first boot*, and cn=config is node-local (ADR-002), so no peer supplies
it later. Pod-0 (bootstrapped standalone) was restarted at scale-up — the init
container ran again and skipped everything. Concretely missing on pod-0:

1. **Modules** (`moduleload accesslog/syncprov` gated on replication-at-boot):
   without them slapd doesn't know `olcSyncProvConfig`/`olcAccessLogConfig`, every
   overlay add fails with error 21, `reconcilePodDatabase` aborts before
   `ensureAccesslogDB` → pod-0 never becomes a provider → writes on pod-0 don't
   replicate out, consumers can't initial-sync.
2. **Custom schemas**: the SlapdSchema controller only watched its own CR — schema
   reached `Applied` when the cluster had one pod, and nothing re-triggered it when
   pods 1/2/RO appeared. Entries using the custom objectClass then failed syncrepl
   on the consumers (rc 21).
3. **olcServerID** (`serverID` directives gated on replicas>1 at boot): pod-0
   stamped CSNs as sid 0, outside the ADR-011 scheme (peers believed pod-0 was
   sid 1). Also implied: plain N→M scale-out leaves every pre-existing pod with a
   stale N-entry list.

**Fix (ADR-003 amendment):** the operator owns all replication-related cn=config
state at runtime, per-pod: `ensureModulesLoaded` (additive, desired-minimum — live
`ldapmodify` on `cn=module{0}`, no restart; surplus modules are never removed since
slapd cannot unload live), `ensureServerIDs` (exact-match Replace of the full list;
**sid-1-per-default**: every RW pod carries `serverID <base+ordinal+1> <url>` from
birth, even standalone, so no transition ever changes a pod's own sid), and a
`Watches(SlapdCluster)` on the SlapdSchema controller (same map-func pattern as the
2026-04-20 SlapdDatabase fix). bootstrap.sh keeps writing all of it at fresh
bootstrap as a fast path; values are byte-compatible so healthy clusters reconcile
to a no-op. e2e: `tests/e2e/scaleup_test.go` (gated `E2E_SCALEUP=1`) walks the full
transition with a custom-schema entry written pre-scale-up.

**Why it was hard to spot:** the pod looks healthy from every angle that doesn't
compare against topology-derived desired state: 1/1 Running, stanzas present
(applied by a later step that doesn't depend on the failed one), multiProvider
TRUE, and `slctl inspect`'s csn-convergence check passed on "all **1** pods report
identical CSN" — the two pods with no contextCSN at all simply weren't counted.
Every prior e2e deployed with replication enabled from the start; the demo was the
first thing to walk the upgrade path.

**Lesson:** bootstrap.sh is a one-shot seeding mechanism, not a reconciled
artifact — any cn=config state it writes conditionally WILL go stale across a
lifecycle transition that changes the condition. When adding replication-related
cn=config state, add it to the operator's per-pod runtime reconcile first and to
bootstrap.sh second (fast path only). And: controllers whose applied state depends
on cluster topology must watch SlapdCluster, not just their own CR — this is the
second controller caught by that (SlapdDatabase/externalPeers was 2026-04-20).

---

## 2026-07-05: hardcoded `cluster.local` breaks every pod FQDN on non-default-domain clusters

**Symptom:** On a fresh multi-replica cluster whose Kubernetes DNS domain is *not* `cluster.local` (e.g. `k8s.example`), every slapd pod crashloops at startup:

```
read_config: no serverID / URL match found. Check slapd -h arguments.
slapd stopped.
```

StatefulSet never reaches ready; `e2e.sh setup` times out. The hardcoded domain is long-standing — it stays invisible on `cluster.local` clusters and only surfaces on a non-default domain, so it is not a recent regression.

**Root cause:** `bootstrap.sh` and 14 operator call sites built pod FQDNs with a literal `svc.cluster.local`. That is only the *default* cluster DNS domain. `bootstrap.sh` writes `olcServerID <id> ldaps://<pod>.<headless>.<ns>.svc.cluster.local:1025`; at startup slapd self-matches that URL against its listeners. The match normally succeeds off `/etc/hosts` alone (kubelet writes the pod's own FQDN there, so no DNS/readiness is needed) — but here `/etc/hosts` holds `…svc.k8s.example`, and CoreDNS is authoritative for `k8s.example` (forwards `cluster.local` upstream → NXDOMAIN). No match → exit. The operator's per-pod LDAP connections and syncrepl provider URIs had the same bug, so even past serverID nothing would have worked; TLS SANs (minted for the real domain by `gencert.sh`) wouldn't have matched a `cluster.local` name either.

**Fix (ADR-015):** Resolve the domain once at operator startup — `CLUSTER_DOMAIN` env → `svc.<domain>` from `/etc/resolv.conf` → `cluster.local` — inject it into all three reconcilers, and pass `LDAP_CLUSTER_DOMAIN` to the init container. `bootstrap.sh` defaults `${LDAP_CLUSTER_DOMAIN:-cluster.local}`. Never hardcode the domain again; use `r.ClusterDomain`.

**Why it hid so long:** every prior multi-replica e2e ran on a default-domain cluster (all the Multus multi-site runs), where the hardcoded value was accidentally correct. The single-cluster/no-Multus path on a non-default domain was the first to exercise the assumption. The Multus "we used plain IPs" recollection was real but tangential — that substitution only affects syncrepl provider URIs, never serverID.

**Lesson:** Kubernetes clusters do not all use `cluster.local`. Any FQDN the operator emits must use the resolved cluster domain. When debugging a slapd startup crash, check `/etc/hosts` and `/etc/resolv.conf` in the pod first — the self-FQDN and search domain reveal a domain mismatch immediately, and rule out readiness/DNS-publishing theories (self-match needs neither).

---

## 2026-04-16: go-ldap attribute name case sensitivity

**Symptom:** `syncreplChanged: true` and `mirrorModeChanged: true` on every reconcile cycle, causing the operator to rewrite identical syncrepl stanzas every 10 seconds.

**Root cause:** go-ldap v3.4.12's `GetAttributeValues()` uses **exact string comparison** (`attr.Name == attribute`), not case-insensitive matching. OpenLDAP returns attributes under their canonical schema names:

| We requested | OpenLDAP returned | Match? |
|---|---|---|
| `olcSyncRepl` | `olcSyncrepl` | no (capital R vs lowercase r) |
| `olcMirrorMode` | `olcMultiProvider` | no (completely different name, OL 2.5+ rename) |

Both reads returned empty, so the operator always saw a diff and replaced.

**Fix:** Switched all `GetAttributeValues()` calls to `GetEqualFoldAttributeValues()` (case-insensitive). Changed `olcMirrorMode` reads/writes to `olcMultiProvider` (the canonical name in OpenLDAP 2.5+).

**Why it was invisible:** The init container (`bootstrap.sh`) sets up working syncrepl independently of the operator. The operator's constant rewrite was functionally a no-op (same values). The e2e replication tests verify data propagation (write pod-0, read pod-1), not cn=config idempotency.

**Lesson:** When reading cn=config attributes via go-ldap, always use `GetEqualFoldAttributeValues()`. LDAP attribute names are case-insensitive per RFC 4512, but go-ldap's default getter is not.

---

## 2026-04-16: Operator stops reconciling before all pods are configured

**Symptom:** Pod-2 (and sometimes pod-1) never gets ACLs, schemas, or syncrepl configured. The replication user `cn=replication` doesn't exist on unconfigured pods, causing `rc 49` (invalid credentials) errors from pods trying to sync from them.

**Root cause:** `reconcileACLs()`, `reconcileSchemas()`, and `reconcileReplication()` silently swallowed per-pod errors (logged and skipped unreachable pods, returned `nil`). The main `Reconcile()` function then checked StatefulSet readiness (3/3 pods Running), reached `PhaseRunning`, and returned `ctrl.Result{}` — no more requeues. Pods that were unreachable during early startup (DNS not yet propagated for the headless service) were never retried.

**Fix:** Changed the three functions to return `(skipped bool, err error)`. The main loop now requeues after 10s if any pod was skipped, regardless of phase.

**Why it was hard to spot:** The operator logs said "will retry on next reconcile" but there was no next reconcile — `PhaseRunning` suppressed the requeue. The slapd pods appeared healthy (Running, Ready) because the init container handled the base setup; only the operator-managed layers (ACLs, schemas, syncrepl stanzas) were missing.

**Lesson:** Any reconcile step that skips work due to transient errors must propagate that fact to the requeue decision. "Log and skip" without forcing a retry is a silent failure mode.

---

## 2026-04-17: User ACLs block userPassword replication

**Symptom:** Readpw users exist on all pods but `userPassword` is empty on pods 1 and 2.
Only pod-0 (where the bootstrap job ran) has the password hashes. Bind attempts against
pods 1/2 fail with error 49 (Invalid Credentials). `slctl inspect` shows no replication lag
(CSNs match) — the entries replicated, just without the password attribute.

**Root cause:** The user-specified ACL rule:
```
to attrs=userPassword by self write by anonymous auth by * none
```
The syncrepl consumer binds as `cn=replication,<domain>`, which matches `by * none`. The
provider's ACL evaluation strips `userPassword` from the search results sent to the consumer.
The entry replicates, but the password attribute is silently excluded. This affects any
attribute that user ACLs deny to `*`.

**Why it was hard to spot:**
- CSN convergence showed no lag — replication was working perfectly, it was faithfully
  replicating what the consumer could *see*, which excluded `userPassword`.
- The entries existed on all pods, so `ldapExists()` checks passed.
- With `kubectl port-forward`, all test connections went through one tunnel to one pod
  (the bootstrap pod that had the data). The issue only manifested with NodePort routing,
  where each connection could hit a different pod.
- Error 49 (Invalid Credentials) is indistinguishable from "user doesn't exist" vs "wrong
  password" vs "password attribute missing" — OpenLDAP returns the same error for all three.

**Fix:** When replication is enabled, `reconcileACLs` now automatically prepends an ACL rule
granting the replication bind DN unconditional read access:
```
to * by dn.exact="cn=replication,<domain>" read by * break
```
The `by * break` passes control to the next rule for all other users, so user-specified ACLs
are unaffected. Users never need to include the replication user in their ACLs — the operator
handles it transparently.

**Lesson:** The replication user must have read access to ALL attributes, including those that
user ACLs deny to `*`. Any attribute invisible to the replication bind DN will not replicate.
This is a property of syncrepl itself — the provider's ACLs apply to the consumer's bind DN.
The rootDN bypasses ACLs, but the replication user is not the rootDN (by design — it has a
narrower role). The operator must ensure the replication user's access is not constrained by
user-specified ACLs.

---

## 2026-04-18: No LDAP operation timeout — hung slapd blocks operator permanently

**Symptom:** Operator stops reconciling entirely. Operator logs end mid-operation (e.g.
"Adding schema" with no follow-up). `slctl inspect` shows some pods fully configured, others
with 0 syncrepl stanzas and no custom schemas. The operator pod is running but produces no
further log output. Restarting the operator temporarily unblocks it until the same pod hangs
again.

**Root cause:** All four LDAP-speaking reconcile steps (`reconcileBootstrap`,
`applyACLsToPod`, `applySchemaToPod`, `applyReplicationToPod`) used a 5-second **dial
timeout** (`net.Dialer{Timeout: 5s}`) but set no **request timeout** on the LDAP connection.
Once connected, operations like `conn.Add()`, `conn.Modify()`, or `conn.Search()` blocked
indefinitely waiting for slapd's response. If slapd deadlocked or hung for any reason, the
calling goroutine — the operator's single reconcile worker (ADR-001) — blocked forever. No
further reconcile runs for any `SlapdCluster` in any namespace.

**Trigger observed:** A duplicate schema ADD (`cn=ox,cn=schema,cn=config`) while active
syncrepl threads were running caused slapd to deadlock (confirmed via `/proc/PID/task/*/stack`
showing all threads in `futex_do_wait`, zero strace output over 5 seconds, and `ldapsearch`
hanging indefinitely). The operator's `conn.Add()` never returned.

**Fix:** Added `conn.SetTimeout(ldapRequestTimeout)` (10 seconds) after every `ldap.DialURL`
call. go-ldap's `SetTimeout` applies to all subsequent operations on the connection (Bind,
Search, Add, Modify). If any operation exceeds 10 seconds, go-ldap returns a timeout error.
The reconcile step logs the error, skips the pod, and continues — the next reconcile retries.

**Why 10 seconds:** Normal LDAP operations against cn=config complete in single-digit
milliseconds. 10 seconds is generous enough to never false-positive, but short enough that
a deadlocked pod only costs one 10-second stall per reconcile cycle instead of permanent
blockage.

**Lesson:** Dial timeouts only protect against unreachable hosts. A connected-but-hung server
is equally dangerous to an operator with a single reconcile worker. Every network client in a
controller needs request-level timeouts, not just connection-level ones.

---

## 2026-04-18: Schema existence check never worked — duplicate ADD deadlocks slapd

**Symptom:** On the second reconcile after initial deployment, the operator tries to add
`cn=ox,cn=schema,cn=config` to slapd-0 even though it was successfully added 5 seconds
earlier. slapd deadlocks (all threads in `futex_do_wait`, zero syscall activity, LDAP
completely unresponsive). With the timeout fix above, the operator would eventually recover,
but slapd stays deadlocked until the pod is restarted.

**Root cause:** The `schemaExistsByCN` function searched `cn=schema,cn=config` with filter
`(cn=ox)` to check if the schema already existed. The code comment stated that "OpenLDAP's
config backend strips the {N} ordering prefix for filter evaluation." This is **wrong** for
OpenLDAP 2.6.10 — verified empirically:

```
ldapsearch -b "cn=schema,cn=config" -s one "(cn=core)"        → 0 results
ldapsearch -b "cn=schema,cn=config" -s one "(cn={0}core)"     → 1 result
```

The `{N}` prefix is part of the cn attribute value and is not stripped during filter
evaluation. The search `(cn=ox)` never matches `cn={4}ox`. This means the existence check
returned "not found" on every reconcile, causing a duplicate ADD every 10 seconds.

The first reconcile succeeded because the schema genuinely didn't exist yet — the search
correctly returned 0 entries, and the ADD created it. On the second reconcile, the search
again returned 0 (filter bug), and the ADD of the already-existing schema triggered a
deadlock inside slapd when syncrepl threads were concurrently active (likely a lock ordering
conflict between the cn=config write path and the syncrepl retry threads).

**Fix:** Replaced the broken filter-based existence check with `listSchemaCNs`: a one-level
search of `cn=schema,cn=config` with `(objectClass=*)` that retrieves all schema entries,
reads their `cn` attribute values, and strips the `{N}` ordering prefix ourselves
(`stripOrderingPrefix`). This builds a `map[string]bool` of existing schema names. Only
schemas not in this set are ADDed.

The existence check is critical — not just an optimisation. A duplicate ADD while syncrepl
threads are active deadlocks slapd permanently (confirmed on two independent clusters with
different hardware and OS). slapd never returns an error — it hangs before it can respond.
Error 68 (`EntryAlreadyExists`) and error 80 (`Duplicate`) handlers are retained as defence
in depth but cannot be the primary idempotency mechanism because they would never fire in
the deadlock scenario. The LDAP request timeout (10 seconds) protects the operator from
blocking, but slapd itself would remain deadlocked until the pod is restarted.

**Why not just ADD and handle errors?** Because the error never arrives. The slapd deadlock
occurs inside the ADD processing, before any LDAP result is sent back. Relying on error
handling alone would deadlock slapd on every fresh deployment: the first ADD succeeds
(no syncrepl threads yet), syncrepl stanzas are applied, and the next reconcile's ADD
deadlocks slapd. The existence check prevents the duplicate ADD from ever being issued.

**Lesson:** When a server-side bug causes a hang (not an error), error handling cannot be the
primary defence. You must avoid triggering the bug in the first place. Error handling and
timeouts are defence in depth, not substitutes for correct pre-conditions.

---

## 2026-04-19: PhaseRunning set before root entry replicated to all pods

**Symptom:** The slapd-test bootstrap Job starts after the CR reaches `PhaseRunning` /
`Ready=True`, but hits a pod that doesn't have the root entry yet. The Job fails with
"No such object" because the ClusterIP service routes to a pod where replication hasn't
converged. Workaround: an `until` poll loop in `charts/slapd-test/files/bootstrap.sh`
that waits for the root entry before proceeding.

**Root cause:** `reconcileBootstrap` set `bootstrapComplete = true` as soon as it
successfully seeded the root entry on **pod-0 only**. For multi-replica clusters, the
root entry still needed to replicate to pods 1..N-1 via delta-syncrepl. The phase/Ready
logic gated on `bootstrapComplete` and `podsSkipped`, but the bootstrap step itself
never reported "skipped" — it returned a plain `error`, not `(bool, error)`. So even
though pods 1..N-1 didn't have the root entry yet, `bootstrapComplete` was already true,
and if ACLs/schemas/replication were also done, the CR went straight to `PhaseRunning`.

In operator logs this was ~14 seconds of convergence time (3 replicas + 1 read-only),
during which the CR was already advertising readiness.

**Fix:** Changed `reconcileBootstrap` signature from `error` to `(bool, error)`. After
seeding (or confirming) the root entry on pod-0, the function now queries each peer pod
(1..N-1) via headless DNS for the root entry. If any peer is unreachable or doesn't have
it yet, returns `(true, nil)` — feeding into `podsSkipped`, keeping the CR in
`PhaseBootstrapping` with `Ready=False`. Once all pods confirm the root entry,
`bootstrapComplete` is set and the function short-circuits on all future reconciles.

For single-replica clusters the convergence check is skipped entirely (pod-0 having the
entry is sufficient). Pod-0 seeding still uses the pod IP (already available from the
k8s API, avoids depending on DNS at a point where it may not have converged yet). The
convergence check uses headless DNS — consistent with how all other per-pod reconcile
steps (ACLs, schemas, replication) address pods.

Removed the `until` poll loop from `charts/slapd-test/files/bootstrap.sh`. Downstream
consumers can now `kubectl wait --for=condition=Ready` on the CR.

**Why it was hard to spot:** The race window was ~14 seconds on a 3-replica cluster.
In production (long-lived clusters) this only matters once at initial deployment. It
manifested reliably in CI/e2e where teardown/setup cycles are frequent and the bootstrap
Job is deployed immediately after the CR.

**Lesson:** `bootstrapComplete` must mean "bootstrap is complete on the **cluster**, not
on one pod." Any status flag that downstream consumers depend on must reflect the state
of the entire system, not just the first node that was touched.

---

## ADR-004 Refactor: Audit of Fix Preservation (2026-04-19)

The multi-resource CRD refactor (ADR-004) split the single SlapdCluster controller into
three controllers: SlapdCluster (infrastructure), SlapdSchema, and SlapdDatabase. This
section documents how each previously fixed bug is preserved in the new architecture.

| Fix | New Controller | How Preserved |
|---|---|---|
| **go-ldap case sensitivity** | SlapdDatabase, SlapdSchema | All attribute reads use `GetEqualFoldAttributeValues()`. All syncrepl/mirrormode references use `olcMultiProvider` (not `olcMirrorMode`). |
| **Skipped pods suppress requeue** | SlapdDatabase | `pendingWork` flag tracks any incomplete step (failed pods, seed not applied, replication skipped). Phase stays Degraded until all work completes, forcing requeue. |
| **Skipped pods suppress requeue** | SlapdSchema | `allApplied` flag — requeues after 10s when any pod failed. |
| **Replication ACL prepend** | SlapdDatabase | `applyACLs` prepends `cn=replication` read ACL when replication or external peers are enabled, identical to the original fix. |
| **No LDAP request timeout** | All three | Every `ldap.DialURL` is followed by `conn.SetTimeout(ldapRequestTimeout)`. |
| **Schema deadlock on duplicate ADD** | SlapdSchema | Uses `Modify` (LDAP_MOD_ADD on existing `cn=schema,cn=config`) instead of `Add` (create new sub-entry). This avoids the deadlock entirely — no new schema entry is ever created, only attributes are added to the existing entry. Existence check reads all `olcAttributeTypes`/`olcObjectClasses` values and matches by NAME (case-insensitive, `{N}` prefix stripped). |
| **PhaseRunning before convergence** | SlapdDatabase | `seedApplied` is set after successful seed on one pod. Replication convergence to other pods is handled by syncrepl (not polled). The database phase gates on all per-pod operations completing, not just seed. |

---

## 2026-04-19: Missing `cn=replication` bind entry after ADR-004 split

**Symptom:** Fresh multi-replica deployment comes up with all pods Running, syncrepl
stanzas configured, but replication never converges. Consumer pod logs show repeated
`rc=49 (Invalid Credentials)` errors from every peer. `slctl inspect` reports CSN
divergence that never resolves.

**Root cause:** Pre-refactor, the slapd-init container created the
`cn=replication,<suffix>` bind entry in the data tree on first bootstrap. The ADR-004
split moved data-tree bootstrapping (root entry, indices, seed) to the SlapdDatabase
controller, but the replication bind entry was not migrated. The controller happily
wrote syncrepl stanzas referencing `cn=replication,<suffix>` even though the entry
itself never existed on any pod.

**Fix:** Added `ensureReplicationUser` step to the SlapdDatabase reconcile loop
(`slapddatabase_controller.go` — step 9, before syncrepl stanza reconciliation). It
binds to any reachable RW pod as the data rootDN, checks whether
`cn=replication,<suffix>` exists, and if not, adds it as a
`simpleSecurityObject`/`organizationalRole` with an SSHA-hashed password from the
database's `replication-password` Secret key. Idempotent; handles
`EntryAlreadyExists` as success.

**Why it was invisible initially:** Single-replica clusters work fine — no syncrepl
means no bind attempts. ACL-only tests also pass — the data tree looks healthy. The
failure only manifests when two or more RW pods try to sync from each other, and it
produces the same `rc=49` as a wrong password or a non-existent user, which makes it
easy to chase the wrong hypothesis.

**Lesson:** When moving bootstrap responsibilities between components, audit every
piece of state each component was creating. "The controller adds syncrepl stanzas" is
only useful if something else also adds the identities those stanzas bind as. The
refactor checklist missed that `cn=replication` lives in the data tree, not in
`cn=config`, and so belongs to the SlapdDatabase controller's domain, not the
init container's.

---

## 2026-04-19: Missing `overlay accesslog`/`overlay syncprov` on data DB after ADR-004 split

**Symptom:** With replication enabled, writes to the provider pod never propagate
to consumers. The accesslog DB (`cn=accesslog`) exists but stays empty — no entries
are ever written to it. `contextCSN` on consumers never advances past their initial
value. `slctl inspect` shows persistent CSN divergence.

**Root cause:** Delta-syncrepl requires two overlays on the **data** database (not the
accesslog DB): `overlay accesslog` to log writes into the accesslog DB, and
`overlay syncprov` to expose the data DB as a syncrepl provider. Pre-refactor, the
init container added these to `slapd.conf` when `LDAP_REPLICATION_ENABLED=true`.
Post-ADR-004, the init container no longer touches data databases at all (they're
created by the SlapdDatabase controller). The overlay setup was never ported.

**Fix:** Added `ensureReplicationOverlays` to the per-pod database reconcile path.
When `sd.Spec.Replication.DeltaSync == true` and the pod is not a read-only replica,
the controller searches for existing overlays under the data DB's `cn=config` entry,
and adds `olcOverlay=accesslog,<dataDN>` and `olcOverlay=syncprov,<dataDN>` if
missing. `olcAccessLogDB=cn=accesslog`, `olcAccessLogOps=writes`,
`olcAccessLogSuccess=TRUE`, plus optional `olcAccessLogPurge` and
`olcSpCheckpoint` from the CR spec. Idempotent via pre-check and
`EntryAlreadyExists` tolerance.

**Why it was hard to spot:** The accesslog DB itself exists (init container creates
it) and `overlay syncprov` *on the accesslog DB* is set up by the init container
too — so cursory inspection shows a healthy accesslog infrastructure. Only a careful
`ldapsearch -b "olcDatabase={N}mdb,cn=config" -s one "(objectClass=olcOverlayConfig)"`
on the **data** database reveals the missing overlays. `slctl inspect` didn't check
for these.

**Lesson:** Delta-syncrepl needs overlays on both databases — accesslog overlay on
the data DB (producer side) and syncprov on both (provider-side exposure of both the
data DB for initial sync and the accesslog DB for incremental updates). Splitting
"init container owns cn=config" from "controller owns data DB" needs to account for
overlays that logically belong to the data DB even though they're registered as
`cn=config` sub-entries.

---

## 2026-04-19: Seed data lost on rolling restart, not re-applied

**Symptom:** After editing a SlapdCluster or SlapdDatabase field that triggers a
StatefulSet rolling restart, pods come back up with empty data databases —
`ldapsearch` returns "No such object" for the root DN. The SlapdDatabase CR still
shows `status.seedApplied: true`, so the controller never retries seeding. The
cluster appears healthy (Running, Ready=True) but has no data.

**Root cause:** The SlapdDatabase controller sets `status.seedApplied = true` the
first time `applySeedData` succeeds and short-circuits on all future reconciles.
`seedApplied` is a one-way latch: set once, never re-evaluated. Any scenario that
leaves the CR intact but empties the data DB — pod replacement onto a fresh data
PVC, `forceRebootstrap=true`, manual PVC intervention, or a StatefulSet rollout
that happens to recreate LMDB state — produces a Running cluster with no data, and
the controller never re-seeds because the status flag says the work is already
done.

**Fix:** When `status.seedApplied == true`, call `verifySeedExists` before
short-circuiting. It binds to each RW pod as the data rootDN and checks whether the
first seed entry's DN exists. If it's missing on all reachable pods, set
`needsSeed = true` and reapply. Returns true early when the seed spec has no
entries (nothing to verify). On failure to reach any pod, assume missing
(conservative — retry on next reconcile).

**Why it was hard to spot:** Day-to-day operation doesn't trigger it — rolling
restarts are rare and seed data is usually idempotent at the LDAP level. The CR
status looks correct (`seedApplied: true`). The only failing signal is that
directory queries return no data. Initial deployment works because
`seedApplied` starts false.

**Lesson:** Status fields that gate idempotency checks (`seedApplied`,
`bootstrapComplete`, etc.) must be validated against current cluster reality on
every reconcile, not just set-and-forget. "We already did X" is only valid if the
effect of X is still visible.

> **Postscript 2026-05-14 — this fix was conceptually wrong and has been reverted.
> See ADR-012.** The lesson generalised correctly for operator-owned declarative
> state (cn=config — ACLs, schemas, syncrepl stanzas) but should never have been
> applied to seed data. Seed entries are the user's one-time initial-conditions
> sketch, not operator-owned state. After first apply, the directory's actual
> contents are the users' accumulated data — millions of entries the operator
> never knew about. Re-applying seed on "data missing" creates two failure modes
> that are strictly worse than not re-applying:
>
> 1. **Multi-pod write race during initial bootstrap.** Each reconcile after a
>    pod-readiness change re-evaluated `verifySeedExists` against the first
>    reachable pod, which during startup is a moving target (replication hasn't
>    propagated yet). `applySeedData` then wrote the seed to a *different* pod
>    each time. Three independent same-DN adds with three different `olcServerID`s
>    created a CSN-conflict storm; entries 4-6 of the example seed routinely
>    went missing in the resolution race. Observed in the 2026-05-13 multi-site
>    e2e run with `seedApplied=true` but `ou=Mail` and `ou=ServiceAccounts` gone
>    from the directory.
>
> 2. **False recovery masking data loss.** A single-pod cluster losing its PVC
>    would trigger `verifySeedExists=false`, the operator would re-seed, and the
>    cluster would report `seedApplied=true` and `Ready=True` again — while
>    thousands of user entries accumulated over the cluster's lifetime were gone.
>    Strictly worse than failing loudly.
>
> The replacement: `Status.SeedApplied` is now a one-way latch (set on first
> verified success, never re-evaluated). `applySeedData` targets pod-0
> deterministically with per-entry post-add verification. Recovery from data
> loss is replication's job (multi-pod) or backup-restore-or-redeploy
> (single-pod / total). A `DataPresent` informational condition on SlapdDatabase
> surfaces the situation for monitoring but does **not** drive operator action.
> See ADR-012 for the full reasoning and the alternatives considered.
>
> **Do not re-introduce `verifySeedExists` or anything functionally equivalent.**
> If a future scenario seems to need "the operator should re-apply seed because
> X," the answer is almost always either "X is genuine data loss and needs a
> human" or "the operator should fix the underlying declarative state via a
> separate, non-seed code path."

---

## 2026-04-20: SlapdDatabase controller missed externalPeers changes on SlapdCluster

**Symptom:** Editing `spec.replication.externalPeers` on a SlapdCluster (adding or
removing a cross-cluster peer) had no effect on syncrepl stanzas until a SlapdDatabase
CR was also modified. Removing a peer left its stanza in place; adding a peer didn't
create one. `slctl inspect` kept showing stale external peer topology. Tests in
`external_replication_test.go` that relied on prompt convergence were flaky.

**Root cause:** The SlapdDatabase controller owns all syncrepl stanzas, including
external peer stanzas (per ADR-003), but its `SetupWithManager` only called
`For(&SlapdDatabase{})`. Changes to a SlapdCluster CR never enqueued the
SlapdDatabases that reference it. Reconciliation only happened when the
SlapdDatabase itself changed, or on the controller's periodic requeue (which only
fires while there is pending work — a fully-converged database sits idle).

**Fix:** Added a `Watches(&SlapdCluster{}, EnqueueRequestsFromMapFunc(...))` to
`SetupWithManager`. The mapper lists all SlapdDatabases in the SlapdCluster's
namespace and enqueues every one whose `spec.clusterRef` matches. Corresponding RBAC:
`+kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapdclusters,verbs=get;list;watch`.

**Why it was hard to spot:** A reconcile fires on every CR update *by the user*, but
when the SlapdCluster changes, the *SlapdDatabase* is unchanged — so its controller
sees no event. External peer changes are rare, and the usual debugging reflex (save
the CR again, watch the controller log) accidentally triggers a SlapdDatabase
reconcile that fixes the state, masking the root cause.

**Lesson:** When controller A owns state derived from multiple CRs, it needs a
Watch on every CR kind it reads from — not just the one it lists under `For(...)`.
A cross-CR dependency without a cross-CR Watch is a silent liveness bug: it looks
like it works because every manual poke causes a reconcile.
