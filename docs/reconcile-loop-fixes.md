# Reconcile & Replication Fixes

Log of bugs found and fixed in the operator's reconciliation loop and replication setup.
Kept to prevent running in circles when debugging similar issues in the future.

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
