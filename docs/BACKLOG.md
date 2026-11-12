# Backlog (non-migration tech debt)

Cross-cutting cleanup items that aren't tied to the migration plan
(`docs/MIGRATION-PLAN.md`) or a single ADR. Keep entries small and actionable.

## ~~Deprecation cleanup: `client.Apply` → `client.Client.Apply()` / `SubResource().Apply()`~~ — DONE (2026-09-14)

All twelve package-level `client.Apply` call sites are migrated to
`client.Client.Apply()` (six object patches in `slapdcluster_controller.go`) and
`r.Status().Apply()` — i.e. `SubResource("status").Apply()` — (six status
patches across the cluster/restore, database, schema, backup and scheduled-backup
controllers). `SA1019` is 0; the two `ldap.Dial` → `ldap.DialURL` sites in
`slctl` went with it, being a literal substitution on a plaintext
`localhost:<port>` port-forward.

The new API takes a `runtime.ApplyConfiguration` rather than a typed object.
Generated apply configurations exist for the core kinds but not for our CRDs, so
the objects are adapted through `client.ApplyConfigurationFromUnstructured` by
the one `applyConfiguration()` helper in `internal/controller/apply.go`. Both
paths send `json.Marshal` of the payload as an `application/apply-patch+yaml`
body, and `apply_test.go` pins that the unstructured form marshals to the same
JSON as the typed object — so the field set recorded for each field manager is
unchanged, as are `FieldOwner` and `ForceOwnership` (both option types implement
`ApplyToApply` / `ApplyToSubResourceApply` identically).

The broader pre-existing lint debt this entry always excluded is paid down too
(2026-10-17, #17): the operator module is at zero golangci-lint findings under an
unchanged `.golangci.yml`, so the CI gate's whole-module fallback (taken when a PR
diff is too large for the API) no longer fails on debt a PR did not add.

## ~~Three paths hand `Replace("olcSyncRepl")` a short set with no error~~ — DONE (2026-10-05, ADR-031 M4b)

`olcSyncRepl` is installed with one atomic
`modReq.Replace("olcSyncRepl", desired)`, so a set shorter than the pod's
current one is a **deletion**, not a partial update. Three paths skip a peer
and carry on, handing `Replace` a set missing that peer's stanzas:

- a peer whose bind-password Secret cannot be read;
- a discovery peer that has not resolved any addresses yet;
- `len(ep.URIs) == 0` inside the builder.

Each is "withhold means delete" in the shape RID-PLAN hazard 2 names: a
transiently unreadable Secret or a peer mid-discovery tears that peer's
cross-site stanzas off every pod, and replication for that link stops rather
than pausing. Nothing reports it — the pass is not marked skipped and the
database stays `Running`.

**Found 2026-10-03** by the ADR-031 Phase 5 conformance review on the withdrawn
branch, which verified all three exist unchanged on `main`.

**Two new triggers, 2026-10-05**, from ADR-031 M1's correctness review. Both
feed the discovery bullet above:

- A remote pod name that will not parse, or two remote pods claiming one
  ordinal, now refuse the reduction — and a refusal is published as an *empty
  provider list*, which is exactly the state the consumer already treats as
  benign.
- `discoveredAddresses` was removed with no compatibility read, so the first
  reconcile after an operator upgrade sees an unpopulated `discoveredProviders`
  and takes the same path. It self-heals on the next pass (the database watches
  the cluster), at the cost of one syncrepl session restart per cross-site link
  — which is more than ADR-031's "one `cn=config` write per pod per database"
  promises, and is an unwelcome shape while ITS#9398 is open.

Both are latent rather than live: the remote selector matches only RW
StatefulSet pods, whose names always parse and are unique. **Neither may ship:**
M1 and M4 land in the same release, and M4's hazard-2 withhold closes the window
before any of it is published. That makes the fix a release gate, not a
nice-to-have.

**Fix:** treat an unresolvable peer the way the RID gate treats an unassigned
column — withhold that pod's stanza convergence and report, rather than
converging to a set that omits it. Red-first: a cluster with two external peers,
one with an unreadable bind-password Secret, must leave both peers' stanzas on
the pod.

**Related:** the same review found `syncreplMatch` and `applySyncreplToPod` have
no test coverage at all, so the atomic `Replace` that makes this dangerous is
itself guarded only by prose.

**CLOSED by ADR-031 M4b.** All three paths now withhold the database's whole
stanza convergence and report, instead of skipping the peer: an unreadable
bind-password Secret, a discovery peer with no providers, and a peer that
resolved to no address, each with its own reason so a human is told which to
fix. The builder's `len(URIs) == 0` guard became unreachable and was converted
to an error rather than deleted, because being wrong about unreachability is
silent in one direction and loud in the other.

Two things the fix carried with it, both worth knowing. The refusal now blocks
**in-cluster** stanzas too, so a permanently broken peer stops more than its own
links — recorded in ADR-031's Consequences, with the "preserve the unresolved
peer's stanzas" refinement in this file as the follow-up. And the related gap
above is unchanged: `syncreplMatch` and `applySyncreplToPod` still have no
tests, so the atomic `Replace` remains guarded by prose.

---

## A rotating write endpoint has no answer for read-after-write clients

Idea raised 2026-10-23. With `spec.rotation` on, a client that needs
"my own write is visible to my next read" can lose that guarantee at a slot
boundary, and the window is not small.

**The mechanism, and it is measured rather than hypothetical.** `<name>-write`
moves to a different pod every slot. A Service never re-wires an established
connection, so a client only follows the move when it re-dials — which is the
documented contract. But at the moment it *does* re-dial it lands on a pod that
may not yet carry the write it made seconds earlier on the previous one, and
the previous pod is by construction the one that just started purging. ADR-037's
twelfth amendment measured exactly that residue: `contextCSN` spread alternates
between **0.01 s and 96-150 s** on the slot rhythm. So the stale window is not
a replication hiccup, it is up to two and a half minutes, arriving on a
schedule.

The corner the reporter named is the sharp one: a connection TTL expiring
*just* as the endpoint moves. Nothing is wrong, every component behaves as
designed, and the client reads its own write back as absent.

**Today there is no sanctioned answer.** The operator creates `<name>` (whole
RW fleet, round-robin), `<name>-write` (rotating, one pod), and the two
read-only Services. Nothing offers "the same pod, every time", and
`sessionAffinity` is not set anywhere.

### What a fix would have to trade

This cannot be solved by routing. Read-after-write across an asynchronous
multi-master mesh is a consistency property, and a Service can only decide
*where* a request goes. So any option here buys consistency by giving
something up, and the entry should say which:

| option | gives you | costs |
|---|---|---|
| `sessionAffinity: ClientIP` on `<name>` | stickiness without new API | wrong granularity — NATed or proxied clients share one IP and land together; affinity has its own timeout; still re-balances when the endpoint set changes |
| document the existing pod DNS (`<name>-<n>.<name>-headless`) | zero new surface, works today | the client hardcodes an ordinal and gets no failover; it is pod identity, not a service |
| per-pod ClusterIP Services (`<name>-0`, …) | a real endpoint per pod, failover-free but nameable | N more Services per cluster, and the client still picks |
| a second, NON-rotating `<name>-sticky` | one name, stable | it must pin to *some* pod, and that pod purges 1/n of the time — so this client gets the full write stall the rotation exists to avoid |

The last row is the honest core of it: **a client that refuses to move is a
client that will sit on a purging pod.** That is not an argument against the
feature — it is the trade the user should be making deliberately, and ADR-037
already frames the rotation as a recommendation rather than an enforcement
("writes sent elsewhere stay legal and cost one stall").

### Worth checking before building anything

- **Does `leadTime` already cover most of it?** The lead exists so a connection
  no older than `d` never delivers writes into the purge that starts at the
  boundary. A client whose TTL is comfortably under `leadTime` re-dials to the
  new writer *before* the old one starts purging, so its write is replicated
  out of a pod that is not yet busy. If that closes the practical case, the
  answer is documentation plus a sizing rule, not a Service.
- **What the client actually needs.** "Read-after-write" is often satisfied by
  reading back on the same *connection*, which already works — the Service
  does not move an established one. The problem is specifically clients that
  pool, expire and re-dial between the write and the read.

If it is built, `docs/PURGE-ROTATION.md` is where it belongs: that guide
already leads with the client-connection contract, and this is the same
contract read from the other side.

## The CRD samples are unreadable as a starting point

Reported 2026-10-23, from somebody who wanted a cluster and a database without
the e2e script and without the mesh chart, and sat looking at the samples
unable to find the spec in them.

The reaction is the evidence:

```
cat ldap_v1alpha1_slapdcluster.yaml  | yq '... comments=""' > lab-cluster.yaml
cat ldap_v1alpha1_slapddatabase.yaml | yq '... comments=""' > lab-database.yaml
cat ldap_v1alpha1_slapdschema.yaml   | yq '... comments=""' > lab-schema.yaml
```

Stripping every comment leaves a *usable* file. That is the measurement: the
commentary is not annotating the example, it is burying it. The cluster sample
is roughly four fifths comment, most of it fields that are commented OUT — so
a reader scanning for "what do I actually have to set" has to distinguish
prose from disabled YAML from live YAML, three kinds of line that all start
with whitespace.

**This is a consequence of a decision that was right.** The samples are
deliberately "one commented example per CRD — the shape of every field, with
the non-obvious ones explained inline" (README). That is genuinely useful as a
reference, and it is where several hard-won warnings live. The defect is that
the same file is also advertised as the thing to `kubectl apply`, and it is bad
at that.

**What exists already, and why it does not close this.** README's Quick Start
carries a perfectly good minimal replicated cluster — but as fenced YAML in
prose, not as a file. So the copy-pasteable thing is not applicable and the
applicable thing is not readable.

### Shape of a fix

A `operator/config/samples/minimal/` (or `docs/examples/`) holding the smallest
manifests that actually stand a cluster up: a `SlapdCluster`, one
`SlapdDatabase`, optionally one `SlapdSchema`. Only fields that are REQUIRED or
whose default is wrong for a real deployment; no commented-out alternatives; a
one-line header pointing at the annotated sample for everything else. The
existing samples stay exactly as they are and get a line saying which file to
start from.

Two things to decide rather than assume:

- **Whether it is checked.** A sample that drifts is worse than none. The
  cheapest guard is to have the samples parsed and validated against the CRDs
  in CI — which would also have caught the stale `tag: 0.2.1` that sat in the
  cluster sample until 0.5.0. A stronger one applies them to a kind cluster.
- **Where it lives.** Under `operator/config/samples/` keeps one place to look;
  under `docs/` puts it next to the walkthrough. The README links both either
  way.

Related: `docs/demo/` already holds applyable manifests for the walkthrough, and
may be most of this already — worth checking whether it just needs pointing at
rather than duplicating.

## A SlapdCluster cannot influence where its pods are scheduled

Observed 2026-10-23 on a four-node lab, a three-replica cluster plus one RO
replica:

```
slapd-0            node-a
slapd-1            node-b
slapd-2            node-b
slapd-readonly-0   node-a
```

Two of three read-write pods on one node, and the read-only replica sharing a
node with a writer. Losing `node-b` takes two thirds of the write fleet; losing
`node-a` takes a writer and the entire RO fleet. Nothing about that was chosen —
the scheduler had no reason to prefer otherwise.

**The operator exposes no placement control whatsoever.** `SlapdCluster` has no
`affinity`, no `tolerations`, no `nodeSelector`, no
`topologySpreadConstraints`, and the pod-template builder sets none. The only
affinity anywhere in the API is `SlapdBackup.podAffinity`, which does the
opposite job — it *pins* a backup Job to its target pod's node. The operator's
own Helm chart has `nodeSelector`/`affinity` values, but those place the
**operator**, not slapd.

**Why this is sharper here than for a typical workload.** The PVCs are RWO and,
in every environment this project runs in, backed by local storage
(`openebs.io/local`, `local.csi.openebs.io`) with `WaitForFirstConsumer`. The
volume binds wherever the pod first lands, and from then on the pod is pinned
to that node for the life of the PVC. **A bad spread is therefore permanent,
not something the next reschedule fixes** — correcting it means deleting a
pod's PVCs and rebuilding it from replication, which is ADR-012 case 2 and
carries everything that comes with it (ADR-036's authorship gate, and the
stale-cookie exposure that shape has historically produced).

So the usual reasoning — "scheduling is the cluster operator's business, not
the workload's" — is weaker than normal: the first scheduling decision is the
only one, and it is made before anyone notices.

### Two shapes, and they are not exclusive

1. **Pass-through fields.** `spec.affinity`, `spec.tolerations`,
   `spec.nodeSelector`, `spec.topologySpreadConstraints`, handed to the pod
   template verbatim. Cheap, unopinionated, and the precedent is already here —
   `spec.resources`, `spec.securityContext` and `spec.imagePullSecrets` are
   exactly this. It solves nothing for anyone who does not know to set it.
2. **A default spread.** Most operators ship a *preferred* (soft) pod
   anti-affinity on `kubernetes.io/hostname` so replicas land apart when they
   can and still schedule when they cannot. Soft rather than required matters:
   a required constraint on a cluster with fewer nodes than replicas leaves
   pods Pending forever, and a three-node lab running `replicas: 3` plus RO
   pods is a normal shape here.

The RO fleet wants its own treatment either way — spreading RW pods apart does
not stop every RO pod landing on a writer, which is what happened above.

### What it costs to add a default

A default changes the rendered pod template for every existing cluster, so it
is one roll on operator upgrade. ADR-035 says that is allowed — an operator
upgrade may change the template once — and ADR-013's rule is about side-effect
rolls, not about a deliberate version change. But it is a roll of every slapd
pod in every cluster, so it belongs in a release that is already rolling them
and wants an `UPGRADING.md` entry saying so.

Worth deciding together with the pass-through fields: if a default lands, the
pass-through must be able to **replace** it rather than merge with it, or a
user who sets their own `affinity` gets ours silently ANDed onto theirs.

## The accesslog purge stalls the pod that runs it, and we pin writes to one pod

**What:** `accesslog_purge()` collects every expired entry with one uncapped
search and deletes them in an unpaced loop, each delete its own transaction.
Those deletes take the accesslog environment's single LMDB writer, and every
client write appends its own log entry to that same environment — so a
provider's purge blocks that provider's **writes**. Reads keep flowing at ~75 %.

Measured on a 2+2 cluster at 100k ops/s (0.3 % writes), one hour per arm,
`cn=config` set asymmetrically with reconciliation suspended:

| | purge on the write target | purge on the peer only |
|---|---|---|
| write stall per cycle | 30 s | **0 s** |
| iterations dropped per cycle | 969,786 | **0** |
| peak p99 | 3,384 ms | **1.3 ms** |
| peak replication lag | 40.8 s | 34.8 s |

Purging the write target reproduces the ordinary symmetric deployment. Purging
only the peer makes the event invisible to clients: the write rate holds
295–305/s through every cycle where the symmetric case collapses to ~5/s.

**Consequence:** the disturbance is not inherent to purging, it is inherent to
purging *where the writes are*. Today both providers purge simultaneously —
they restart together, and the runqueue task is scheduled from startup — so
every deployment runs the worst case: the write path stalling at the same
instant the peer's replay does.

**The idea:** rotate the write endpoint between providers, and purge only the
one not currently taking writes. Client-visible write latency is then never
behind a delete loop. The cost moves to the inactive provider's replication
lag, which it works off before becoming the write target again.

A second benefit falls out. A provider's own sid in `contextCSN` is restored
only by a **local** write, so pinning writes to one provider leaves the other's
sid stale — which is part of why an unclean death needs a write under the
absent sid to recover. Rotation keeps both sids advancing on their own.

**What is proven and what is not.** Proven: the mechanism (the table above, one
clean run per arm, plus a symmetric control that matches the write-target arm);
and that purge *phase* is choosable — `logpurge` has no time-of-day setting,
changing the policy on a live task only edits `re->interval.tv_sec` and does
not move the next fire time, but removing the attribute deletes the task and
setting it again inserts a fresh one that fires promptly (under 11 s against a
one-minute cycle). So "`none`, then set, at T" places a pod's purge at T.

Not proven: the rotation itself. We never moved the write target — both runs
pinned writes to the same pod and varied only which pod purged. Unanswered:
how long the inactive provider needs to catch up before it may take writes;
what happens to in-flight writes at the switch; and whether a rotation interval
exists that keeps both logs bounded without ever purging the active node.

**What the operator would need:**

1. **Per-pod purge policy.** `olcAccessLogPurge` is reconciled from the CR to
   every pod, so an asymmetry survives only under `spec.suspend` today.
2. **Phase re-established after every restart** — a restart resets the task to
   the restart instant, so an offset is standing operator work, not a value.
3. **Write-endpoint rotation** coordinated with (1), plus a readiness condition
   that refuses to promote a provider still working off its purge backlog.

**Why it may not be needed at your write rate.** The stall is entries-deleted ÷
delete-rate, and the delete rate is ~2,500/s under load, ~2,800/s idle. At
10k ops/s (30 writes/s) a cycle deletes ~9,850 entries: zero iterations
dropped, replication lag peaking at 3.3 s, no stall resolvable at 10 s
granularity (two runs, agreeing to 0.1 s). At single-digit writes/s it is
sub-second. The feature earns its place on write-heavy deployments, not on
ordinary ones — and the cheaper intervention for those is simply to **stagger**
the two providers' purges, which needs (1) and (2) but not (3).

Those 10k figures were measured on the **`both` arm** — accesslog checkpoint
plus accesslog-sourced session log — not on `sessionlog` as first written here.
A sequence killed mid-arm had left the checkpoint on the server and the runs
inherited it. It makes the bound *conservative* rather than wrong: `both` is
the more expensive arm at every load where the two separate (+19 % peak
replication lag at 190k, +12.9 % at 100k), so the harmlessness at 10k is
measured on the worse configuration. There is no clean `sessionlog`
measurement at 10k; the 50k figures above are `sessionlog` and unaffected.

**One hazard found while measuring this:** an oversized purge takes a provider
out of service. Draining ~250,000 entries at a one-minute cycle restarted both
providers — no crash captured, matching the accesslog hang where slapd stops
answering and readiness fails. The same oversized cycle produced 255 s of
replication lag. Whatever schedules purges must not let one get large.

## `olcAccessLogPurgeBatch` paces the purge, and we have never set it

OpenLDAP 2.7 carries a knob that attacks the stall above at its cause rather
than moving it: `olcAccessLogPurgeBatch` (`OLcfgOvAt:4.8`, SINGLE-VALUE
integer, "Pause purge task after every N entries have been purged"). The purge
loop reads it as `li_yield_factor` and, after every N deletes, sleeps for the
measured mean duration of one delete. Overhead is therefore about `1/N`, so
**small N paces hard** — N=1 runs the purge at roughly half speed, N=100 barely
touches it — and because the sleep is derived from measured elapsed time it
scales itself to how slow deletes are on that storage.

It defaults to 0, meaning off: no sleeping, just a `thread_yield()` after each
delete. That is what every slaptain cluster runs today. The operator never sets
it, there is no spec field for it, and it appears in neither the man page nor
`CHANGES`.

Safe to converge live: the set and delete paths are a plain integer assignment,
with no task manipulation and no free, so none of ADR-024's live-modify hazards
apply. If adopted it is an ADR-024 R1 tunable wanting a `SlapdDatabase` field.

**2.7-only.** 2.6's purge loop has no such setting and paces not at all — it
calls `ldap_pvt_thread_pool_pausewait()` in the same position, but that yields
to a thread-pool pause wanting quiescence, not to client work. So "unpaced" is
not even the same loop in the two versions, which is worth knowing before any
measured purge figure is carried across a major version.

**Blocked, on upstream rather than on us.** Measuring it needs a read-heavy
load against 2.7.x, and 2.7.x frees a shared `entryUUID` buffer from an
unlocked path (ITS#10597): every client search that completes inside a write's
window frees that write's copy, which under a read-dominated profile is
continuous. `master` fixes it structurally by moving the value into a
per-operation structure; no release carries that yet, and a backport is a
refactor of how the value threads through the response callback rather than a
cherry-pick. So the measurement costs a custom image built from an unreleased
tree — and would measure a slapd nobody deploys.

Setting `olcAccessLogOld` is **not** a way around it. It would take a branch
that skips the shared buffer, but only for deletes and modifies — adds are not
in that branch and still populate it — and it would break sourcing the session
log from the accesslog (ADR-034): syncprov requires `reqEntryUUID` on those
entries and fails with `LDAP_CONSTRAINT_VIOLATION` without it. The two are
coupled on 2.7.x and cannot both be had; one attribute selects between them.

Pick this up when a release carries the ITS#10597 fix. Full reasoning, the
measured prior results and the alternative it competes with (a rotating write
target, which needs none of this and works on either version) are in
[ADR-037](adrs/adr-037-purge-off-the-write-path.md), option A.

## The operator writes and checks `{SSHA}` verifiers — move them to a KDF

Three places hash or verify a password as `{SSHA}` (salted SHA-1), and CodeQL
reports each as `go/weak-sensitive-data-hashing` (high):

- `generateSSHAHash` (`internal/controller/slapddatabase_controller.go`) — every
  verifier the operator writes: the data database's `olcRootPW`, the legacy
  `cn=replication,<suffix>` entry and the node-local identity (ADR-027);
- `sshaMatches` (`internal/controller/authdb.go`) — checks the node-local
  identity's stored verifier against the Secret to decide whether to rewrite it;
- `internal/backup/s3.go` — the restore preflight checks the replication password
  against the `replication-pw-hash` a backup stored in its object metadata.

`{SSHA}` is not wrong today: it is a scheme slapd verifies natively and the
documented default of `spec.ldap.passwordHash` (ADR-024), and the secrets the
operator generates are 24 to 32 random bytes, far beyond any brute force. It is
weak for a password a person chose, which ADR-027 rotation now allows.

The forward fix is a key-derivation scheme slapd can verify. `{ARGON2}` is the
obvious candidate: the 2.7.1 images already ship slapd's `argon2` module (core
since 2.7), and `golang.org/x/crypto/argon2` produces the verifier. To decide:

- module loading: `argon2` must be loaded on every pod before any verifier in
  that scheme is written, including RO pods and the `-ol26` pair (check which
  argon2 module, if any, its Debian 2.6 build carries);
- backups: artifacts taken before the change carry an `{SSHA}` hash, so the
  preflight must keep verifying both schemes;
- the default: whether `spec.ldap.passwordHash` follows, an ADR-024 amendment.
  `docs/TUNING.md` still says slaptain does not ship an argon2 module; that is
  out of date for the 2.7.1 images.

The CodeQL alerts stay open until the scheme changes; they are not dismissed.

## ~~A site that cannot read its OWN databases reports every external peer as disconnected~~ — DONE (2026-10-06)

**Fixed**, together with a second defect of the same family found while writing
the fix — a database with no local baseline read as `Synced`, because the
remote-ahead clamp turned a zero-time subtraction into zero lag. Both halves and
the measured evidence are in `docs/reconcile-loop-fixes.md`; `PartiallyVerified`
is widened to "either side" by the ADR-008 amendment of the same date. Red-first:
three table cases observed failing before the fix, and the deliberate asymmetry
(a zero on the PEER's side against a readable local baseline stays `Lagging`)
carries its own guard case, proven by mutation.

Two follow-ups came out of it, below. The original entry follows for its
evidence.


`slapdcluster_controller.go:410` gates the peer CSN check on local evidence:

```go
if len(dbInfos) > 0 && len(localNewest) > 0 {
    r.checkPeerCSNConvergence(ctx, sc, &ep, &status, dbInfos, localNewest)
}
status.Connected = status.ReplicationState != "" && status.ReplicationState != ldapv1alpha1.ReplicationUnreachable
```

When every local contextCSN read fails, `localNewest` is empty, the peer check
is skipped, and `Connected` is then derived from an **unset** `ReplicationState`
— so the site publishes `connected: false` for every peer it never contacted,
with no `replicationState`, no `lastError`, and no `lastChecked` (the status
struct is rebuilt fresh each pass at `:389`, so the previous timestamp is
dropped rather than going stale).

The reader is pointed at the wrong site. Nothing in the peer entry says the
fault is local, and `connected: false` is the same value a genuinely dead link
produces.

**Measured 2026-10-06** on a three-site lab while validating the monitoring
work. Inbound LDAP to site-3's slapd pods was blocked for 2.5 minutes:

- site-1 and site-2 reported site-3 `Unreachable` **with** the verdict — they
  dialled and failed. Correct.
- site-3 reported site-1 and site-2 `connected=false` with an empty
  `replicationState`, for the five scrape intervals 13:13:30–13:15:30, with
  `lastChecked` absent across exactly that window. Its own pods were `Ready`
  throughout (exec probes on loopback are unaffected by a network policy) and
  its status carried `PodsResponsive=False/PodsNotAnswering` and
  `ReplicationConverged=Unknown/CSNQueriesIncomplete` — the two conditions that
  *did* tell the truth.

This is ADR-008's rule broken one level up: unreadable evidence does not merely
fail to count toward healthy, it is converted into a positive claim about a
remote peer. ADR-033 decision 5 names the same failure for the metric surface.

**Fix shape:** unreadable local evidence must produce an explicit unknown, not
`false`. `PartiallyVerified` already exists for "the peer answered but some
evidence is missing"; this is the sibling case, "we never asked", and wants its
own reason rather than reuse. `Connected` cannot express it at all — it is a
bool derived for backward compatibility — so either it stops being derived from
an unset state, or the peer entry grows something that can say "not assessed".
Red-first: a cluster whose local CSN reads all fail must NOT publish
`connected: false` for a peer that is up.

**Monitoring already works around it**, which is not a fix: the shipped
`SlaptainPeerUnverified` rule keys on `peer_connected == 0 unless on (site,
peer) peer_last_checked_seconds` and names the local site. Note the shape — the
signal is the *absence* of the timestamp, so the obvious staleness comparison
(`time() - lastChecked > 300`) can never catch it.

---

## The cross-site CSN baseline is gated on `ready >= 2`, so a 1-replica cluster reads PartiallyVerified forever

`slapdcluster_controller.go:383`:

```go
if sc.Spec.Replication.Enabled && ready >= 2 && len(dbInfos) > 0 {
    localNewest = r.checkLocalCSNConvergence(ctx, sc, dbInfos)
}
```

Two pods are needed to judge local *divergence*. They are **not** needed to hold
a *baseline* for cross-site comparison — one pod's newest contextCSN is a
perfectly good operand. But one gate serves both purposes, so a single-replica
cluster never gets a `localNewest` at all.

Before the 2026-10-06 fix that meant "peer check skipped" (and the bare
`connected: false` that entry describes). After it, every external peer at such a
site reads `PartiallyVerified` with every database named, permanently. Strictly
more honest than before and it fails in the safe direction — but it is permanent
yellow for topologies that are not broken:

- the ADR-011 hot-migration shape, which is single-replica by design;
- any `consumer-only` site (ADR-010);
- any cluster scaled to 1.

**Fix shape:** split the gate. The divergence verdict keeps `ready >= 2`; the
baseline needs only `ready >= 1`. That changes what `ReplicationConverged` is
written on a 1-replica cluster, which no committed test covers, so it wants its
own red — which is why it was left out of the fix rather than folded in.

**Found 2026-10-06** by the implementer of that fix, reported rather than taken.

---

## `evaluatePeerConvergence` cannot tell a failed local read from an empty database

`csnReading.Answered` is true whenever `Err` is empty
(`csn_convergence.go:45-52`), so a database **read fine and genuinely empty** is
distinguishable from one whose read **failed**. But `localVerdict`
(`csn_convergence.go:56-66`) carries only `NewestBySuffix`, and both collapse to
an absent key.

The 2026-10-06 fix therefore treats them alike, which is safe but coarse, and has
two costs:

1. A never-written database makes every external peer read `PartiallyVerified`
   for as long as it stays empty — on a healthy cluster. The kind of standing
   warning that trains people to ignore the field.
2. **A third false-`Synced` survives, and it is the most reachable of the
   family.** A local database that is empty-but-readable while the peer HAS data
   still reports `Synced`, via the same clamp: we hold no baseline, so the
   remote's CSN looks ahead, and the clamp zeroes the deficit. That is every new
   site's initial-sync window — the peer holds entries we do not, and the verdict
   says in sync. No failure required, just a fresh site joining a mesh.

**The three-way rule:**

| local | remote | verdict |
|---|---|---|
| read FAILED | anything | unverified (what the 2026-10-06 fix does for all of these) |
| answered, EMPTY | empty | `Synced` — both agree there is nothing |
| answered, EMPTY | has data | `Lagging` — we are missing what the peer has |
| answered, has data | empty | `Lagging` (unchanged) |

Cost: a field on `localVerdict`, a signature change on
`evaluatePeerConvergence`, and one case in the red commit `01f8091` would have to
change — it expects `PartiallyVerified` where the refined rule says `Synced`.
Deferred deliberately rather than rewriting a committed red.

---


---

## A pod with an empty contextCSN makes the divergence message report a 292-year lag

`newestCSN` (`csn.go:135-152`) errors only when a CSN was present but unparseable:

```go
if newestRaw == "" && len(csns) > 0 {
    return time.Time{}, "", fmt.Errorf("no parseable CSN in %v", csns)
}
return newest, newestRaw, nil
```

So an **empty** slice returns the zero time with **no error** — a legitimate
reading, as far as the caller can tell.

`evaluateLocalConvergence` then treats the two bounds asymmetrically
(`csn_convergence.go:114-119`):

```go
if t.After(v.NewestBySuffix[r.Suffix]) { v.NewestBySuffix[r.Suffix] = t }   // guarded
if o, seen := oldestBySuffix[r.Suffix]; !seen || t.Before(o) { ... }        // NOT guarded
```

A zero time can never displace a real value as *newest*, but it always wins as
*oldest*. The divergence message at `:128-129` then subtracts it:

```
CSN divergence on 1 of 1 databases across 2 pods: dc=example,dc=org (9223372036.9s lag)
```

9223372036.9 s is a saturated `time.Duration` — about 292 years. The same input
one pod later, with a real older CSN, reads `(146.7s lag)`.

**It fires on exactly the shape a reader most needs a trustworthy number for**: a
pod that never received the database — the ADR-025 glue / never-synced case. The
existing case `a pod with no contextCSN diverges from pods that have one` passes
today because it only asserts the suffix is named, not the figure.

**Found 2026-10-06** by M2's test-author while building the per-database readout,
reported rather than taken: the message belongs to a verdict that milestone was
forbidden to change, and under Fix Discipline a wrong number wants its own
red-first case.

**Fix shape:** guard the oldest bound the way the newest one is guarded, or have
`newestCSN` distinguish "no CSN at all" from "a CSN I could read" so the caller
can skip the reading entirely. The second is better — it removes the class rather
than the instance — but it changes a shared helper with four call sites, so it
needs its own blast-radius pass. Red-first: a reading with an empty CSN vector
must not contribute a bound.

**Not a false-health claim**, unlike its three siblings on this branch — a
nonsense figure is at least visibly nonsense. Filed with them because it is the
same cause: evidence that is absent being used as though it were a measurement.
The per-database readout added in the same milestone is immune by construction
(such a pod counts as attempted-and-unread, so it yields no spread).

---

## `judgeExternalPeers` refuses a duplicate `siteIndex` but not a duplicate peer `name`

`ExternalPeers` carries no `+listMapKey=name`, so the API server will not reject
two peers sharing a name, and `judgeExternalPeers` checks `siteIndex` only. A
duplicate name is therefore an accepted spec.

It is not harmless. Anything that joins spec to status **by name** gets a
non-injective map: measured during the ADR-033 Surface 2 review, two peers named
`dup` with 3 and 0 discovered providers produced a reading where the peer with
three reported **zero**. That is a *wrong* value rather than a missing one, which
is the one failure mode a monitoring surface has no way to look suspicious while
committing.

Surface 2 now joins by index, which is reliable because the status list is
rebuilt from the same spec slice in the same order every pass, and
`TestClusterObservationJoinsPeersByIndexNotByName` pins it. But the join is only
one consumer: the defect is that the spec admits the state at all.

**Fix shape:** refuse a duplicate `name` the way a duplicate `siteIndex` is
already refused — same place, same shape of message. A `+listMapKey=name` marker
on the field would additionally let the API server reject it, which is better
still, but it is a CRD change and wants its own blast-radius pass (it also makes
the list a merge key for server-side apply, which this operator uses).

**Consequence worth stating:** once refused, the Surface 2 fixture becomes
unconstructible from a real spec, and those tests turn into the record of *why*
the join is index-keyed rather than coverage of a reachable state. They stay
correct either way — the fixture builds the spec directly rather than through
validation — so tightening validation cannot rot them.

**Found 2026-10-06** during the Surface 2 correctness review, reported rather
than taken: it is a validation change with its own blast radius, not part of a
metrics milestone.

---

## Surface 1 does not export three things ADR-033 decision 2 says it does

Decision 2 names `status.restore.phase`, `SourceConverged` and
`SourceSuffixHealthy` among the CR-state quantities kube-state-metrics exports.
It exports none of them.

`monitoring/kube-state-metrics-values.yaml` declares two
`groupVersionKind` resources, `SlapdCluster` and `SlapdDatabase`. `SlapdBackup`
is not among them, and `SourceConverged` / `SourceSuffixHealthy` are
**SlapdBackup** conditions — so they have no series at all. Confirmed against
the live Prometheus: the whole `slaptain_database_condition` family carries only

```
['DataPresent', 'Ready', 'SyncreplStanzasReady', 'TunablesConverged']
```

`status.restore.phase` is a `SlapdCluster` field and so is reachable, but no
metric is declared for it either — the config exports `phase`, not the restore
sub-phase.

**Why it matters more than a missing gauge.** ADR-033's whole architecture is
that a quantity belongs to exactly one surface, and Surface 2 deliberately does
**not** export these three *because decision 2 assigned them to Surface 1*. The
restore sub-phase in particular was explicitly withdrawn from Surface 2 on that
basis (2026-10-07 amendment). So right now they are exported by neither surface,
and the gap is invisible precisely because each side can point at the other.

**Fix shape:** both are config-only and blocked by nothing. Add `SlapdBackup`
(and `SlapdScheduledBackup`, `SlapdRestore`) to the CRS resource list — the RBAC
`extraRules` in that same file already grant list/watch on all of them, so only
the metric declarations are missing — and declare a metric for
`status.restore.phase`. The kinds were granted and then never used, which is
itself the tell.

**Found 2026-10-07** while amending ADR-033: the agent reading decision 2 against
the shipped config noticed the inventory did not match. No ADR change is needed —
decision 2 is right about where these belong; the config simply never caught up.

---
## A pod that loses its disk never gets back the entries IT wrote, and every health signal says it did

ADR-012 case 2 says a pod that loses its data is restored by replication. It is
restored **except for the entries that pod itself authored**, and the gap is
invisible to every check slaptain has.

### Measured (t3e, 2026-10-16, single site, 3 RW pods, OpenLDAP 2.7.1)

Under a ~10 writes/s load driven to all three pods, `slapd-0`'s three PVCs were
deleted and the pod force-replaced. It resynced, went Ready, and settled
**313 entries short** of its peers — stably, over the following fifteen minutes:

```
slapd-0: 5236      slapd-1: 5549      slapd-2: 5549
```

Every one of the 40 sampled missing entries carries `entryCSN …#001#` — the
**rebuilt pod's own serverID** — and was written by the previous incarnation of
that pod shortly before its disk was destroyed. The peers hold all of them.
Nothing is missing in the other direction.

Meanwhile:

- `ReplicationConverged = True/CSNsMatch`, and every pod reports an **identical
  contextCSN** for the database;
- `slctl inspect --short`: **15 passed, 0 warnings, 0 failed**, including
  `csn-convergence`, `suffix-visibility` and `suffix-uuid-agreement`;
- new writes replicate to the rebuilt pod normally — probed explicitly, a fresh
  entry written on `slapd-1` appeared on `slapd-0` within ten seconds.

So the cluster is healthy by every signal it publishes, and permanently short
of data on one pod.

### Mechanism — confirmed by an A/B rebuild (2026-10-16)

The rebuilt pod logs **zero** `do_syncrep2: … CSN too old, ignoring` lines
(`syncrepl.c:1282`, at this fixture's sync log level), so the entries are not
offered and refused — they are never sent. Two rebuilds of the same pod, alike
in every respect except one write, say why:

| | rebuild kept **write-free** until resynced | rebuild **written to** the instant it answered |
|---|---|---|
| own-sid entries recovered | **50 of 50** | **0 of 50** |
| its `contextCSN` for its own sid | came back at the **pre-crash** value | stands at the **trigger write** |

In the second run the trigger write landed 40 s after the last entry it was
meant to recover, under the same serverID:

```
uid=caseB-50     entryCSN 20261016165209.962239Z#000000#003#000000   (lost)
uid=trigger-write entryCSN 20261016165249.621216Z#000000#003#000000   (the one local write)
```

**So the cookie's per-sid watermark is the whole mechanism.** A pod that comes
back empty and writes nothing asks its peers for everything, its own prior
authorship included, and gets it. One local write stamps its own serverID with a
CSN *later* than everything its predecessor wrote — and a provider has nothing
to send for a sid the consumer claims to be current on. The window is not a
race measured in milliseconds: it stays open until the pod is caught up, and it
closes permanently the moment anything writes.

That second rebuild left the pod **1260 entries short** of its peers (4391 vs
5651) — its entire authorship history, not merely the markers — while
`ReplicationConverged` read `True/CSNsMatch` and `slctl inspect` reported
15 passed, 0 failed.

### Why it matters beyond this experiment

- **ADR-012 case 2 is overstated as written.** "Lose one pod's data, syncrepl
  recovers" holds for everything except that pod's own authorship — which, on a
  multi-provider cluster taking writes on every pod, is a share of the data
  proportional to that pod's traffic.
- **ADR-023 rolling replacement rebuilds pods one ordinal at a time.** If a
  replaced pod returns with the same serverID and takes a write before it is
  fully caught up, this is the same situation, deliberately, once per pod.
- **It is the ADR-025 lesson again**: CSN health reads clean while the data
  diverges. Our strongest convergence signal compares a watermark, and a
  watermark cannot see entries nobody will ever send.

### Directions, none of them taken yet

1. **Detect it.** A per-pod entry count, or an entryUUID-set comparison, in
   `slctl inspect` — something that compares *contents* rather than
   watermarks. Cheap, and it would have caught this in seconds.
2. ~~**Test the hypothesis.**~~ **Done, 2026-10-16** — see the A/B table
   above. Which promotes the rule to a design question slaptain can actually
   answer: **a pod that lost its data must stay out of the client Service until
   it has converged.** Readiness is exactly that lever, and ADR-030 deliberately
   left it alone — it chose "Ready means slapd answers LDAP" and deferred
   data-convergence readiness where ADR-013 had already parked it. This is the
   argument for picking it up: a NotReady pod leaves the ClusterIP Service and
   takes no client write, while headless DNS still publishes it (ADR-030
   already made that true), so replication keeps feeding it. The cost is the
   one ADR-013 named — defining "converged" — and the benefit is no longer
   hypothetical.
3. **Give a rebuilt pod a fresh serverID.** Then its peers' entries are foreign
   to it and arrive normally. This collides with ADR-017/ADR-028, where the
   serverID is derived from the ordinal — so it is a real design question, not
   a switch.
4. **Rebuild from data, not from replication.** ADR-014's restore path
   (`slapcat` a peer, `slapadd` the new pod) does not consult cookies and has
   no own-sid blind spot. That may be the honest answer for a pod that lost its
   disk, with syncrepl only catching up the tail. Developed in its own entry
   below — four open hazards and two measurements that would decide it.

### What this is not

It is not ITS#9398. That hunt is what produced this: six rounds of torture
(unclean kills, PVC loss, a new serverID killed before checkpoint, a
scale-down retiring two sids) failed to provoke a single `sync cookie is stale`
line on a single site, with and without the ADR-034 session-log source and
accesslog checkpointing. The storm stayed away; this came out instead.

## Rebuild a lost pod from its own backup instead of a full refresh — four open hazards, and no measured problem yet

Direction 4 of the entry above, developed. A pod that loses its PVCs today
recovers by full refresh from a peer: the seed plus the whole DIT over the
wire. If the cluster already takes scheduled backups, the artifact plus a delta
is the cheaper shape — `slapadd` what S3 holds, then let syncrepl carry the
tail. `SlapdDatabase.spec.bootstrapFrom` already loads an artifact offline, so
the format and the loader exist.

**The API question that prompted this.** `SlapdScheduledBackup.spec.databaseRef`
points database-ward, one schedule per SlapdDatabase. The proposal adds the
reverse link — a `scheduledBackupRef` on `SlapdDatabase` (and, as the natural
extension, on `SlapdCluster`, applying to every database it owns).

Two things to settle before that field exists:

- **Availability is already observable.** The operator can list `SlapdBackup`s
  and filter on `databaseRef` — which is exactly what the scheduled-backup
  controller does for retention. So "the operator needs to know a backup
  exists" does not justify the field. What justifies it is **intent**: which
  artifact is this database's recovery source when several schedules or buckets
  exist. Same distinction `slctl aws --from <kind>/<name>` is built on, and it
  should be argued that way or not at all.
- **Inline, rejected.** Inlining a schedule into `SlapdDatabase` rebuilds the
  fused-CR shape ADR-028 refused (packaging unit is a chart, not a fused CR),
  and on a mesh the CR is byte-identical at every site — so an inline S3 target
  silently commits every site to one bucket. A ref keeps that a decision.

### Prerequisites, agreed

- **ADR-036 first.** A restored pod starts with a non-zero cookie, so it never
  takes the `syncrepl.c:734-736` full-refresh fallback. ADR-036's
  create-`olcReadOnly` gate is what keeps it from stamping its own sid before
  it has converged.
- **Backup age must sit inside the accesslog retention window**, or the
  provider answers stale-cookie and the restore buys nothing. This couples the
  cron period to `accesslogPurge.retention` for the first time — and since
  ADR-037 the *schedule* half lives on the cluster while retention stays on the
  database, so the two halves of the constraint are declared in two places.
- **The artifact is the local site's.** Cross-site restore stays a human
  operation; whole-site loss is ADR-036's explicitly undecidable case, where
  `SlapdForceWritable` carries the human assertion.

**One correction worth recording**, because the opposite is the intuitive
reading: a restored artifact is **not** a false receipt by construction.
`syncprov.c:3043-3056` suppresses per sid at `value <= cookie`, and the
artifact's vector describes exactly what the artifact contains, so entries the
rebuilt pod authored *after* the backup sit above its own-sid watermark and are
sent normally. The artifact is a genuine watermark. It becomes a lie only if
the pod takes a local write between `slapadd` and first convergence — which is
the ADR-036 window, and the whole reason the gate is a prerequisite rather than
a neighbour.

### Open hazards, in the order they could reshape the design

1. **A poisoned artifact propagates the ADR-036 loss.** ADR-036 produces a pod
   whose `contextCSN` is correct and whose contents are short. Back that pod up
   and the artifact inherits the shortfall *with a watermark asserting
   completeness*, and it sits in S3 for the retention period; restore from it
   and the loss is re-injected into a healthy pod, which replicates the
   watermark onward. ADR-014 deliberately does not gate on convergence
   (`requireConverged` designed and deferred), and `SourceSuffixHealthy` covers
   the analogous ADR-025 glue case — there is no counterpart here, because per
   ADR-036 no CSN check can see it. Latent today, since artifacts are only read
   by explicit human restores; an automatism makes it the default recovery path,
   which is how a latent corruption becomes a propagation mechanism. **What
   makes an artifact trustworthy enough to auto-restore from is unanswered.**
2. **Where the offline `slapadd` runs.** It is offline-only and single-writer,
   and ADR-014 gets that by scaling the whole cluster to 0 — which inverts the
   point here. So it has to happen inside the rebuilt pod's own startup, an
   init-container step, shape-compatible with ADR-034's offline converger. Two
   constraints follow: the source cannot arrive as pod-template env (ADR-013's
   roll-trigger count is zero) and must come by ConfigMap-by-name or be
   discovered by the init container; and it must not be a Job, because any pod
   object naming the PVC is an ADR-018 deletion lease blocking pod re-roll.
3. **What detects "this PVC was lost".** Nothing today distinguishes *fresh
   pod, empty PVC, green-field cluster* from *rebuilt pod, empty PVC, cluster
   with history*. ADR-025's one-way `status.dataObserved` latch answers a
   neighbouring question at cluster level, not per pod. Getting the predicate
   wrong in the green-field direction `slapadd`s a stale artifact over a new
   install, automatically, with no human in the loop — the one failure here
   that cannot be taken back, so it is the central decision, not an
   implementation detail. Smaller and related: `bootstrapFrom` is one-shot via
   `restoreApplied`, a guard whose shape does not carry to something that fires
   on every loss — a crash-looping pod must not restore on every restart.
4. **Every refusal needs a defined degradation.** ADR-025's destroy-last glue
   preflight and ADR-014's own preflights are fine terminal states for a human
   restore; here the pod still has to come up. Each refusal must fall back to
   full refresh, and the condition text has to make "I recovered the slow way,
   and why" legible — otherwise silent slow recoveries are indistinguishable
   from fast ones. Same for the retention window: older than the log, fall
   back, never fail.

### What would decide it — both unmeasured

- **The full-refresh cost at target DIT size.** We have canary propagation from
  `docs/UPGRADING.md` and nothing on refresh duration. Four minutes makes this
  a lot of new failure surface for nothing; four hours makes it obvious. This
  number belongs in the Context of any ADR before its Decision — ADR-037's
  lesson, where a plausible optimisation (shortening the purge interval) was
  *refuted* by measurement rather than merely rejected.
- **Whether it changes the ITS#9398 storm.** `dataloss_recovery_test.go`
  measures the storm red on 2.7.1 after exactly this scenario, reproduced twice
  more on the three-site mesh (2473 and 9237 lines) with the purge established
  as irrelevant. So the storm lives in the baseline path this feature replaces.
  Better (shorter present phase), worse, or unchanged is unknown — and an
  accidental mitigation would be a far stronger argument for the feature than
  recovery speed.

### What this is not

Not a replacement for `SlapdRestore` or `bootstrapFrom`, both of which stay
human-initiated cluster-level operations. Not a cross-site recovery path. And
not scoped yet: the per-pod rebuild *operation* does not exist — `bootstrapFrom`
shares the artifact format and the offline load with it and essentially nothing
else — so this may well belong to ADR-023, whose rolling replacement already
rebuilds pods one ordinal at a time and needs a seed step regardless.

## ~~A database cannot ask for its sessionlog to come from the accesslog~~ — DONE (2026-10-15, ADR-034)

`spec.replication.syncprovSessionlogSource: Accesslog` on `SlapdDatabase` asks
for it. The attribute is never written to a running slapd — a guard refuses it
before the wire — and the init container converges it offline at every pod
start; the operator reports the gap as `StartupConfigConverged=False` /
`RestartRequired` and leaves the restart to a human. The in-memory log is handed
over rather than dropped, so the database always has exactly one session log.
Decision, the double free and all four amendments:
[ADR-034](adrs/adr-034-startup-only-configuration.md); the placement class is
ADR-024's fourth, the user-facing field is in `docs/TUNING.md`.

The analysis below is kept as the evidence for why the design is shaped this
way, and reads as of the time it was written.

A data DB should not carry an **in-memory** sessionlog where an accesslog
already exists for that suffix: syncprov can read its session log from there
instead — `syncprov-sessionlog-source` / `olcSpSessionlogSource`, pointed at
the accesslog DN. One copy of the history, on disk, surviving a restart,
instead of a second one in RAM that does not.

Note what this is NOT: it is not a sessionlog on the accesslog DB. ADR-022
still holds — the accesslog DB carries none, and a successful sessionlog replay
there would displace the minCSN guard. This is the **data** DB's sessionlog
being *sourced from* the accesslog, which is a different attribute on a
different database.

Today the operator cannot express it. `spec.syncprovSessionlog` sizes the
in-memory log and `0` disables it, but there is no way to say "and read it from
the accesslog instead", so turning the in-memory one off just leaves the
provider without a session log at all.

### The constraint that shapes the whole feature

**`olcSpSessionlogSource` must be written when `cn=config` is CONSTRUCTED, and
never modified on a running `slapd`.** Setting it at runtime is a double free:

```c
case SP_LOGDB:
    if ( CONFIG_ONLINE_ADD( c ) ) {          /* true ONLY for a runtime set */
        if ( !select_backend( &c->value_ndn, 0 ) ) { ...; break; }
        ch_free( c->value_ndn.bv_val );      /* syncprov.c:3905 — frees it */
    }
    si->si_logbase = c->value_ndn;           /* 3907 — stores the freed pointer */
```

`si_logbase.bv_val` is dangling from assignment; the delete path frees it again
at `syncprov.c:3814`. Reproduced 60/60 under ASan, both frees in `sp_cf_gen`.
`CONFIG_ONLINE_ADD` is false when the directive is read from the configuration
at startup, and that path is clean — 0 reports over 200 writes plus a runtime
delete. Full investigation, stacks and reproduction: `slapload/docs/FINDINGS.md`,
"A double free in syncprov, reproduced and located".

It is not a known ITS and it is **not fixed in any version**: the code is
byte-identical in 2.5.20, 2.6.0, 2.6.10, 2.6.15, 2.7.1 and master. There is no
release to upgrade to, so the operator has to avoid the path rather than wait
for a fix.

This already cost two providers. The attribute was set and removed by hand on
a live 2+2 cluster and both pods died — one of them while completely idle —
leaving an ITS#9398 precondition behind on one accesslog.

### What that means for the implementation

- A spec field on `SlapdDatabase` — `syncprovSessionlogSource`, or a mode on
  the existing sessionlog knob, since the two are mutually exclusive (`slapd`
  itself warns that configuring both "results in wasteful operation"). Asking
  for the source should imply the in-memory log is absent, not merely
  recommend it.
- It must be emitted by whatever builds a pod's `cn=config` **before slapd
  starts**, alongside the other syncprov settings a fresh database gets.
- `ensureSyncprovTunables` must NOT converge it. It currently read-compare-
  writes `olcSpSessionlog` and `olcSpCheckpoint` on the data DB's syncprov
  overlay on every reconcile; adding the source attribute to that loop turns
  the operator into a crash loop that fires on the next reconcile after
  anyone changes the field. A reconcile that finds the attribute wrong should
  report a condition, not an `ldapmodify`.
- Changing it on an existing database is therefore a **rolling restart with a
  rebuilt config**, not an online convergence — the same shape as any other
  startup-only setting. Worth deciding explicitly whether the operator does
  that automatically or refuses and says so.
- A guard is cheap and worth having regardless of this feature: refuse to
  `Replace`/`Add` `olcSpSessionlogSource` against a live pod anywhere in the
  codebase, so no later change reintroduces the path by accident.

### Why it matters now

It blocks a measurement: the cost of sourcing the sessionlog from the
accesslog, against a baseline, under the usual gate (errors zero, p99 under
threshold, replication lag stationary). That experiment needs the setting
applied from startup on both providers, which is exactly what the operator
cannot currently do.

*(It can now. The measurement itself is still outstanding — ADR-034 ships the
mechanism, not the number.)*

## A stale CRD prunes a new spec field silently, and nothing notices

A cluster whose CRD predates a field accepts a `kubectl apply`/`patch` naming
it and **drops the field on the way in**: the API server prunes what the schema
does not know, the write reports success, and the only evidence is the
behaviour never changing. Every downstream reading says "the feature does
nothing" rather than "the field never arrived", which is a long way from the
cause.

Seen twice, on two clusters, a month apart:

- 2026-09, the measurement cluster: its CRD predated
  `spec.syncprovSessionlog` and `spec.syncprovCheckpoint`, and a `kubectl patch`
  naming either was accepted and pruned — found only by reading `cn=config`
  back and seeing the old attribute still there.
- 2026-10-15, t3e: `spec.replication.syncprovSessionlogSource` did not reach
  the cluster through `e2e.sh setup`, for the reason in the entry below.

The entry below is one *instance* of this — Helm not re-applying `crds/` on an
upgrade — and fixing it fixes nothing on a cluster upgraded by any other route.
What is missing here is the general answer: how a CRD/operator version skew
gets *noticed*. Candidates, none decided: the operator comparing the served CRD
schema against the version it was built with and reporting the gap; the charts
refusing to install an operator newer than the CRD; `slctl inspect` carrying the
check, since it is the thing people run when a feature appears to do nothing.

## `tests/e2e.sh setup` never re-applies CRDs, so a new API field is silently dropped

`e2e.sh setup` installs the operator chart with Helm, and Helm applies a
chart's `crds/` **only on the first `install`, never on an `upgrade`** — the
trap CLAUDE.md already documents for `make operator-helm-install`, which is why
that target depends on `operator-crd-apply`. `e2e.sh` has no such dependency.

So a lab cluster that has ever had the operator installed keeps the CRD it was
first given. A newly added spec field is then **pruned by the API server**: the
`kubectl apply` succeeds, prints `Warning: unknown field`, and the object lands
without it. Every downstream assertion reads as "the feature does nothing"
rather than "the field never arrived", which is a long way from the cause.

Found while landing ADR-034 (2026-10-15): the `syncprovSessionlogSource` field
did not reach t3e through `e2e.sh setup`, and the run only moved after a manual
`make operator-crd-apply CONTEXT=t3e`.

Fix: `e2e.sh setup` applies the CRDs the way the Makefile does, server-side,
before the chart. Cheap, and it removes a failure mode whose symptom points
nowhere near its cause. It closes this install path only — the detection
question is the entry above.

## `ensureServerIDs` will renumber a live pod if the derivation ever moves again

`ensureServerIDs` converges `olcServerID` per pod: it compares the derived value
against the live one and `Replace`s when they differ (ADR-017). That was written
when the derivation could not move, so the Replace was a no-op for every
correctly-booted pod and only ever repaired a sid-0 pod or a pre-ADR-017 URL
list.

The serverID stride change of 2026-10-05 (ADR-028 amendment) moved it. On a mesh
upgraded across that release the operator renumbers every pod at index ≥ 1,
once, silently — and a renumbered pod's history stays filed under its old sid in
every peer's `contextCSN` vector, forever, without advancing. `docs/UPGRADING.md`
says do not do it; nothing stops someone doing it.

The gap is that the operator cannot tell "this pod booted wrong and needs
repair" from "this pod is correct and my derivation changed underneath it". Both
look like `current != desired`.

Worth considering, in rough order of cost:

- refuse to Replace a non-zero, non-URL-list `olcServerID` with a *different*
  non-zero bare integer, and report it as a condition instead — that is exactly
  the renumber case, and it leaves both legitimate repairs (sid 0, URL list)
  untouched;
- or record the derivation's generation somewhere node-local so the operator can
  distinguish the two.

Not urgent while there is one stride and no intent to change it again. It
becomes urgent the moment a second stride is contemplated — which is also when
it is too late to add cheaply.

## A scaled-away pod's mkdir Job is never reaped, only TTL'd

`reapSupersededDirJobs` is called from exactly one place — inside
`ensureDirJob`, for a pod the operator is currently ensuring a Job *for*
(`dirjob.go:109`). So the reap only ever visits pods that still exist.

Scale a cluster up and back down and the departed pod's completed mkdir Job
survives with its pod, which under ADR-018 is a **deletion lease on that pod's
PVCs**: `slapd-3-mkdir-… (succeeded) holds data-slapd-3, accesslog-slapd-3`.
Nothing reaps it, because nothing ever calls `ensureDirJob("slapd-3")` again.

**Bounded, not permanent.** `dirJobTTL` is 300 s and the file calls it "the
ADR-018 R4 crash backstop", so the lease clears after ~5 minutes. But ADR-018's
contract is that the operator reaps every Job it creates and TTL is a backstop
for a crash, not the mechanism — and for five minutes the PVCs of a scaled-away
pod cannot be deleted, which is exactly the window ADR-012 case-2 recovery
needs.

**Fix:** reap by cluster rather than by pod. When reconciling, list Jobs
carrying `dirJobLabel` for this cluster and reap any whose pod no longer exists
— the same list the per-pod reap already does, widened by one selector. Note
`reapSupersededDirJobs` is deliberately pod-label-scoped so it can never touch a
backup or restore Job; preserve that.

**Found 2026-10-05** by a manual full-gate e2e run (`E2E_BACKUP=1
E2E_SCALEUP=1 E2E_SCALE=1 E2E_RESILIENCE=1`, seed `1791221440`). It is
**pre-existing and unrelated to ADR-031** — `dirjob.go` is untouched by that
work, and both specs involved predate it. It needs two things to surface in one
suite: the ungated `scale-topology` spec scaling 3→4→3 to create and orphan the
Job, and `E2E_BACKUP=1` to enable `job_lease_test`, which the ADR-031 runs never
set. Spec order decides whether it fires at all, so it is **flaky by
construction** until fixed — a green run proves nothing here.

---

## `spec.replicas` above the RID layout's `P` has no cluster-level diagnosis

ADR-031 decision 5 says exceeding an axis is "a refusal naming which one and
what the ceiling is". The pod axis is only ever checked per *coordinate*, deep
in a stanza derivation — so a cluster scaled to 9 pods under `podsPerSite: 8`
surfaces as **every database withholding every stanza**, with an
`ErrOrdinalOutOfRange` buried in a log line, and nothing on the `SlapdCluster`
naming `podsPerSite` as the thing to change.

The refusal itself is correct and tested; it is the diagnosis that is missing,
and the symptom points at the databases rather than at the cluster that caused
it.

**Fix:** a `SlapdCluster` condition checking `spec.replicas` and the mesh's
`readReplicas` against the layout's `P`, named so the message says which to
change — the replica count or the layout. Red-first: a cluster at `replicas: 9`
under `P = 8` reports it on the cluster, not only as a per-database withhold.

Found 2026-10-05 by the M4a test author, turning ADR-031 decision 5 into cases.
It belongs to no milestone in `docs/RID-PLAN.md`.

---

## `ExternalPeer`'s "exactly one of uri, podAddresses, or discovery" is unenforced

`slapdcluster_types.go:364` documents the invariant in a comment and no CEL
rule implements it, so a peer with none of the three is admitted and fails at
reconcile instead. ADR-031 M4b makes that failure legible (`PeerAddressesEmpty`
withholds and names the peer), which is the important half — but admission is
where it belongs, and the comment currently claims a rule that does not exist.

**Fix:** an `XValidation` on the `ExternalPeer` type — per item, so it costs
nothing against the CEL budget that an unbounded list rule blew earlier.
Red-first: a peer with no mode is refused at `Create`.

Found 2026-10-05 by the M4b test author, checking whether its third refusal
reason was reachable from an admitted CR. It is.

---

## Preserve an unresolved peer's stanzas instead of withholding the whole pass

ADR-031 M4b withholds a database's entire stanza convergence when any external
peer fails to resolve, in-cluster stanzas included, because a set omitting that
peer is a deletion under the atomic `Replace`.

The coordinate makes something better possible: `ridcoord.Decode` recovers the
peer site from any stanza's rid, so the operator could read the pod's current
`olcSyncRepl`, keep the stanzas whose rid decodes to the unresolved peer's
remote slot, and `Replace` with the union — converging everything else while
deleting nothing. A permanently broken peer would then stop costing in-cluster
replication.

**Not taken in M4b** because it introduces a dependency on decoding live
`cn=config` state, and M4b lands immediately before the design's first contact
with a real slapd. Revisit after M5.

---

## A discovery refusal is invisible outside the operator log

`reconcileExternalPeerStatuses` writes a discovery error into
`status.lastError`, and `checkPeerCSNConvergence` then runs for the same peer
and unconditionally overwrites it with its own verdict — which, for a peer with
no addresses, is the generic `no remote addresses available`. The message naming
the offending pod survives only in the operator log.

Pre-existing mechanism; ADR-031 M1 is what gives it a message worth preserving,
since its done-criterion is "a malformed pod name refuses that peer **with the
pod named**" and `kubectl get sc -o yaml` does not name it.

**Fix:** do not clobber a non-empty `status.lastError` with the generic
no-addresses verdict, or give discovery its own field the CSN pass does not own.
Found 2026-10-05 by the M1 correctness review.

**This is also what blocks a finer diagnosis downstream.** ADR-031 M4b reports
every discovery failure as one reason, `PeerProvidersUndiscovered`, because
discovery publishes all five causes — first bringup, a bad kubeconfig, remote
RBAC, an unparseable remote pod name, a duplicate remote ordinal — identically,
as an empty provider list. Separating them cannot be done downstream; it has to
be carried out of discovery, which is this entry.

---

## A reinstall over surviving PVCs produces a healthy-looking, unconfigurable cluster

Deleting a `SlapdCluster` garbage-collects `<name>-config-password` (the
operator sets `SetControllerReference` on it); the PVCs survive on purpose. A
redeploy into the same namespace then mints a new `cn=config` password that
every pod rejects, because `/config` still hashes the old one. Full mechanism
and evidence in the ADR-012 amendment of 2026-09-21.

What makes it backlog-worthy rather than a documentation note: **nothing
reports it**. The operator logs `Invalid Credentials` on every pass — 358 times
in the observed case — and sets no condition, no event, no phase change. The
`SlapdCluster` says `Running`, pods are Ready, data replicates, and
`slctl inspect` indicts three downstream symptoms (zero stanzas,
`olcMultiProvider` unset, missing external stanzas) without naming the cause.
A user reaches the right answer only by reading operator logs.

Two candidate fixes, not mutually exclusive:

1. **Align the lifetimes: stop owning the Secret.** `/config` outlives the CR
   by design, and the password baked into it should too. Dropping the
   ownerReference makes a reinstall ADOPT the existing Secret — the create-only
   path already handles that — and the mismatch becomes unreachable. Cost: a
   `helm uninstall` leaves a Secret behind, so "delete the namespace" becomes
   the only complete wipe. That is already what ADR-012 recommends, and the
   Secret goes with the namespace either way.

2. **Detect and say so.** A bind that fails with err=49 against `cn=config` is
   not a transient error and never recovers on its own: report it as a
   condition (`ConfigCredentialMismatch`) with the repair, rather than looping
   silently. Cheap, and it also covers a hand-edited `cnConfigCredentials`
   Secret, which produces the same dead end by a different route.

3. **Converge it, and stop calling it create-only.** `olcRootPW` on
   `olcDatabase={0}config` is node-local and runtime-modifiable — a textbook
   ADR-024 R1 candidate — yet nothing writes it after `slaptest` renders it at
   first bootstrap (`grep` finds exactly one `olcRootPW` write in the operator,
   and it creates a *data* database). So the Secret is not merely create-only,
   it is **unreconciled**: editing it locks the operator out of every pod at
   once, with no repair path and no signal. Converging it per pod would make
   (1)'s reinstall self-heal and make rotation an ordinary operation — exactly
   what ADR-027 decision 6 did for the replication identity two metres away, and
   the same defect class it retired. One ordering trap makes this more than a
   one-liner: the operator must bind with the OLD password to write the new
   hash, so during the change some pods hold the new one and some the old.
   Try-new-then-old, the shape ADR-008's monitoring bind already uses.

(1) removes the common cause; (2) catches every other route to the same state;
(3) removes the class. Worth doing (2) regardless, because "the operator logged
it 358 times and told nobody" is the part that cost the afternoon.

---

## `lab.yaml`'s `vms:` is libvirt-specific, unread, and inside `sites[]`

`lab.yaml` began as a description of one lab for `tests/e2e.sh` and is now the
input to four user-facing bootstrap scripts. `sites[].vms` did not make that
transition well: it names libvirt domains and `qemu+ssh://` hypervisor URIs, it
is **read by nothing today** (`grep` finds no consumer — it is documented as
"planned: ITS#9580 dormancy repro"), and it sits inline in `sites[]` next to the
fields every user must fill in. A stranger reading the sample cannot tell that
four fields are required and the fifth is an unimplemented convenience for one
hypervisor.

Verified, so the awkwardness is cosmetic rather than functional: a `lab.yaml`
carrying only `sites[].{name,serverIDIndex,context,endpoint}` — no `vms`, no
`nodeAccessIP`, no `slaptain:` section — works with all four `scripts/mesh-*.sh`
and with `e2e.sh config`. Nothing requires the field.

Options, in the order they appeal:

1. **Drop it until the repro needs it.** It describes a test that has not been
   written. Re-adding it costs one commit, and the format stops promising
   something nothing reads.
2. **Nest it under a clearly-optional, clearly-scoped key** — `disruption:` or
   `platform: {libvirt: …}` — so the file separates "facts the bootstrap needs"
   from "facts only outage testing needs, on one hypervisor".
3. Leave it and document the split in the sample's comments. Cheapest, and the
   one that leaves a reader guessing which fields matter.

Whatever is chosen, the underlying rule is worth stating in the sample: this
file has two kinds of content, and only one of them is required to stand a mesh
up.

---

## The e2e fixture's founder is hardcoded `site-1`, and `e2e.sh config` claims it regardless

`tests/resources/example/database{,2}.yaml` declare `seed.site: site-1`, so the
suite requires a site literally named `site-1`. Since `lab.yaml`'s
`sites[].name` became authoritative, a lab whose sites are named anything else —
`site-a`, `ams`, `fra`, the names the documentation itself suggests — renders a
mesh in which no site matches the declared founder.

The chart catches it, which is the good half: `slapd-mesh` refuses at render with
*"database … names seed.site … which is not a site in mesh.sites"*. Verified
against a two-site `site-a`/`site-b` lab file.

The bad half is that `e2e.sh config` prints the founder as a constant:

    site identities:      kube-a=site-a kube-b=site-b (founder: site-1, per spec.seed.site)

which names a site that does not exist in that lab, in the one command whose job
is to show you the resolved configuration before you run anything.

Two candidate fixes, and the choice is a real one:

- **Validate and refuse early.** `e2e.sh` checks that every founder named by the
  fixtures exists among the site names, and dies naming both. Honest, no hidden
  rewriting, and it makes the constraint explicit — but it means the suite only
  runs on labs whose sites are named `site-1..N`.
- **Substitute when building the directory values.** The suite already
  transforms the fixture CRs into chart values; it could map the declared
  founder onto the first site's actual name. More flexible, at the cost of the
  generated values differing from the committed fixture — a transformation of
  the kind ADR-028 Phase 2b deliberately removed when `strip_seed_block` died.

Either way `config` must stop printing a founder it has not verified.

---

## `NeedsAccesslog` and `NeedsAccesslogVolume` have identical bodies

`operator/api/v1alpha1/slapdcluster_types.go:951` and `:978` are byte-identical:
`return sc.Spec.Replicas > 1 || len(sc.Spec.Replication.ExternalPeers) > 0`. Two
names for one predicate, so a change to the rule has to be made twice and a
reader cannot tell which distinction was intended.

Cost observed 2026-09-18: a reviewer mutation-testing the accesslog-mount guard
patched the first occurrence, saw the test still pass, and nearly recorded a
coverage gap that did not exist — the guard depends on the *second*. A
duplicated predicate wastes verification, which is exactly when you least want
to be misled.

Decide whether the two concepts are genuinely distinct (then the bodies should
diverge, or one should call the other) or the same (then one should go).

*Update 2026-10-07:* still open, and the quoted bodies have since drifted apart
(`NeedsAccesslogVolume` now also tests `Replication.Enabled` and the
`AccesslogEnabled` override), so the line numbers above are stale. The
observed-cost paragraph is now history rather than a live trap: the
accesslog-*mount* guard it refers to no longer exists — ADR-013's amendment of
2026-10-07 made the mount unconditional on RW pods, because gating it put it in
the pod template. `NeedsAccesslogVolume` now serves only the ephemeral
backup/restore/mkdir Jobs, and the name is wrong for that: nothing it decides is
a volume any more.

---

## Two cross-site transports have no e2e coverage any more

ADR-028 Phase 7 deleted the hand-wired e2e path, and with it the only coverage of
two transports the operator still supports:

- cross-site **NodePort `uri` peers** (`externalPeers[].uri`)
- **static Multus `podAddresses`** (ADR-007, no discovery)

Neither is expressible through a `SlapdMesh` — the mesh names sites and lets the
operator discover addresses, which is the whole point of the layer — so the
coverage cannot be recovered by porting the specs. A multi-site run now refuses
to start without `pod-routed` or `multus` discovery.

The product still offers both on a mesh-less `SlapdCluster`, so the options are:
drop them from the supported set (they are then dead code to delete, not debt),
or give them a small dedicated fixture outside the mesh path. Do not leave the
third state — supported, documented, untested — standing indefinitely.

Note the related gap while deciding: the `multus` branch of the mesh path itself
is written but untested; every run so far has been `pod-routed`.

---

## ~~`make -C operator lint` cannot run under Go 1.27~~ — DONE (2026-10-10)

Re-pinned to v2.12.2, the version `.github/workflows/lint.yml` runs too, so
local and CI lint by one ruleset; verified against the go 1.26 toolchain in
`operator/go.mod`. The debt itself is untouched: an uncapped run at v2.12.2
on the Go 1.26 tree reports 581 findings (goconst 407, modernize 67, errcheck
47, lll 36, gocyclo 8, unparam 7, unused 5, prealloc 3, revive 1 — the
modernize count was 28 on Go 1.25; the 1.26 rewrites added the rest). Until
the sweep, CI's gate is `only-new-issues` — it holds the line rather than
drawing it. A golangci-lint built with an older Go than the module targets
refuses to run at all ("the Go language version used to build golangci-lint
is lower than the targeted Go version"); `make -C operator lint` installs it
under the module's toolchain, so delete a stale `operator/bin/golangci-lint-*`
after a toolchain bump.

The sweep landed on 2026-10-17 (#17), and the exporter module followed with
ADR-033: both are at zero, so CI's gate is now "have none" (`only-new-issues`
is off) and lints `operator/` and `exporter/` alike.


**What:** the pinned `golangci-lint` v2.7.2 cannot read Go 1.27's export data
(`export data version 4 is greater than maximum supported version 2`). It fails
with 5 bogus `typecheck` errors and never reaches the real linters, so the
target is unusable on a machine whose default toolchain is 1.27.

**Workaround in use:** run `operator/bin/golangci-lint` with `GOROOT`/`PATH`
pointed at a go1.26.x toolchain (one is already in the module cache).

**Why it matters:** the lint gate silently reports nothing useful rather than
failing loudly as a version problem, so a contributor can believe the tree is
lint-clean when the linters never ran. Found 2026-09-14 while closing the
`client.Apply` deprecation, where the before/after counts had to be produced
through the workaround.

**How:** bump the pinned golangci-lint to a release that supports the current
Go export format, and re-pin deliberately rather than floating. While there,
note that golangci caps identical messages at 3 by default — the uncapped
counts (`--max-same-issues=0 --max-issues-per-linter=0`) are the honest ones,
and the remaining debt is 128 findings (errcheck 40, lll 31, modernize 15,
goconst 13, prealloc 13, gocyclo 9, unused 4, unparam 2, revive 1).

---

## Deleting a replicated database live, then re-creating it, crashes slapd 2.7.1

Measured 2026-10-08 on the 2.7.1 build slaptain ships (`0.3.0` images). If a slapd
process deletes a replicated database from `cn=config` while it is running, and that
same process later re-creates a database with the same suffix, the process can crash.
This is what `cleanupPolicy: Delete` followed by re-adding the SlapdDatabase does, and
the cleanup-policy e2e does exactly that.

**Demonstrated**

- *Local reproduction, two slapd processes in one container.* The provider holds the
  suffix with syncprov plus accesslog. Each iteration on the consumer, all through
  `cn=config` over LDAP:
  1. ADD the data mdb database, then ADD the accesslog mdb database.
  2. ADD syncprov on the log DB, then syncprov and accesslog on the data DB.
  3. MOD `olcSyncRepl` (three delta-syncrepl stanzas: one valid provider, one with a
     wrong password, one to a closed port) together with `olcMultiProvider: TRUE`.
  4. Wait 0.05–0.5 s, then replace the stanza set.
  5. Delete deepest-first: the data DB's overlays, the data DB, the log DB's overlay,
     the log DB.

  Same suffix every iteration, fresh directories each time. Result: **7 crashes in 7
  runs, with or without a monitor database**. Every crash came at iteration 2–6 and
  never at iteration 1; a delete earlier in the same process is needed.
- *Two crash signatures, both seen in both arms:*
  - SIGABRT (exit 134): `back-mdb/id2entry.c:828: mdb_opinfo_get: Assertion '!rc'`,
    reached via `mdb_entry_get` ← `overlay_entry_get_ov` ← `fe_acl_attribute` ←
    `backend_attribute`.
  - SIGSEGV (exit 139): a call through a NULL or garbage pointer under
    `mdb_cursor_open` ← `mdb_dn2id` ← `mdb_dn2entry` ← `mdb_entry_get` ←
    `overlay_entry_get_ov` ← syncprov or accesslog.

  No back-monitor frame appeared on any crashing thread.
- *Variants that did not crash:*
  - 0 crashes in 50 iterations of a fresh process re-creating the databases on the
    leftover LMDB files, with no delete in that process.
  - 0 crashes in 120 iterations of the same with two live peers consuming from the
    re-created databases.
  - 0 crashes in 1,200 live `olcSyncRepl` replacements with peers attached.

  Each count spans both arms, monitor database present and absent.
- *Reproduced through the operator* on a throwaway three-replica cluster, three rounds
  of {add a SlapdDatabase with `cleanupPolicy: Delete`, delete it, scale 3→2→3}. One
  pod exited 139 during the round-2 re-add. That process had deleted the same database
  in round 1. It died about 10 ms after the `olcSyncRepl` + `olcMultiProvider` MOD on
  the re-created data database, which sat at the same `{N}` positions the deleted one
  had used.

**Inferred, not reproduced**

A fixture pod crashed (exit 139) during the cleanup-policy spec the same day. Its
process had issued no delete; it was re-creating a database on leftover state from an
earlier run (old CSNs applied, peers answering "consumer has state info but provider
doesn't"). Treated as the same class by inference only.

**Reasoned, not proven**

The stacks point at a dead LMDB environment or transaction, reached through a
reference that outlived the deleted database: the accesslog overlay's log-DB pointer,
syncprov's per-database state, or syncrepl's write backend. Our build ships no debug
symbols (the vendored packaging produces no dbgsym packages), so the backtraces stop
at exported symbols.

**Upstream:** ITS#10607 (a cn=config use-after-free under *concurrent*
modify/modrdn/delete, fixed after 2.7.1) was checked and does not match: every
crashing sequence here was sequential on one connection, and that ITS leaves ADD
unaffected. No ITS for this class was found.

**Coverage gap:** the cleanup-policy e2e passed while the pod crashed underneath it,
because it never looks at `restartCount`. A spec that deletes databases on live pods
should fail when a slapd restarts during it.

*Candidate mitigation, needs its own design:* do not re-create a suffix in a slapd
process that deleted a database earlier, without a pod restart in between. That is in
the spirit of ADR-019's "never reuse an olcDatabase={N} DN across a delete". Whether it
is enough, and what it costs in rolls (ADR-013), is open.

## A one-replica replicated cluster never gets its accesslog directory

Observed once, on a throwaway cluster on the three-site lab (2026-10-08, data plane
0.3.0, operator sha-69e1981): `replicas: 1`, `readReplicas: 1`,
`replication.enabled: true`, one SlapdDatabase with `replication.deltaSync: true`.
The data database came up on both pods. The accesslog database never did. Every
ADD of `cn=accesslog-<db>` on the RW pod failed with

    LDAP Result Code 80 "Other": olcDbDirectory: value #0: invalid path: No such file or directory

and the operator went round a loop: "creating per-database directories", a mkdir
Job that completes, the ADD, the same error. About 20 rounds in seven minutes, with
the SlapdDatabase stuck at `Degraded`, `Ready=False/PartiallyApplied` ("applied to
1 pods, 1 failed or pending"). `/accesslog/<db>` evidently never exists where slapd
looks. The same shape with `replicas: 2` came up clean, accesslog included.

*Cause unconfirmed.* Not investigated. The candidates are whether the accesslog
volume is mounted at all when replication is configured but inactive
(`replicas: 1`), and what the mkdir Job writes into in that case. Nor is it settled
which side is wrong: should a one-replica cluster want an accesslog database, or
should it get the directory?

## `tryDialPodConfig` dials every read-only pod as its RW namesake

`tryDialPodConfig` / `dialPodConfigEventually` (`tests/e2e/accesslog_test.go`)
always resolve the address through `E2E_POD_NODEPORT_BASE`. A pod name like
`slapd-readonly-0` therefore lands on the NodePort of `slapd-0`, the RW pod with the
same ordinal. Nothing errors. The read-only loops in `tunables_test.go` and
`authidentity_test.go` think they inspect the RO fleet but read slapd-0 a second time,
so an RO-only divergence cannot turn them red. Found by the ADR-033 S3-a test author,
whose `cn=monitor` specs bypass the helper and pick the base by pod kind
(`anyPodAddr`, `E2E_RO_POD_NODEPORT_BASE`). *Fix:* move that choice into
`tryDialPodConfig` itself, then watch the RO loops go red once against an RO pod that
has been made to diverge, so the fix is shown to bite.

## e2e framework: specs cannot provision their own topology

**What:** the Go e2e suite cannot stand up the environment it runs against. The
primary fixture — namespace, operator install, TLS, NodePort services, node
access IPs, cross-site kubeconfigs, versitygw — is provisioned by
`tests/e2e.sh` before `go test` starts, and `BeforeSuite` simply assumes it is
there. Specs *can* create `SlapdCluster` CRs (`restore_test.go`,
`restore_inplace_test.go`, `restore_replication_test.go` each do), but there is
no framework for "give me a cluster with topology X, reachable, with
credentials": every such spec hand-rolls the same boilerplate — clone images and
pull secrets from the source cluster, mirror the suffix, expose a NodePort,
read the generated password, wait for readiness — and depends on `E2E_NODE_IP`
being exported by the shell.

**Why it matters:** any behaviour that only appears in a *particular topology*
is effectively untestable without either bending the shared fixture or
duplicating that boilerplate again. It is why the whole suite leans on one
shared, mutable `slapd` cluster that ~15 specs read from and three actively
damage, and why cross-spec interference is a recurring failure mode (see the
`FAIL_FAST` / `E2E_SEED` knobs added for exactly this reason).

**Why deferred:** it is a refactor of the test framework, not a change to the
product, and doing it inline inside feature work would bury it. It wants a
deliberate design pass: what a "cluster fixture" helper looks like, whether
`e2e.sh` shrinks to bootstrapping only the operator and the S3 target, and how
per-topology fixtures are torn down without leaking PVCs.

**How:** introduce a fixture helper that provisions a `SlapdCluster` +
`SlapdDatabase` of a requested shape (replica count, replication on/off,
external peers or none, TLS or not), exposes it, returns a connected client, and
registers its own cleanup. Port the three existing hand-rolled cases onto it
first — they are the specification for what the helper must do.

### 2026-09-14: the `cleanupPolicy` spec is the newest instance, and it cost a run

`cleanup_policy_test.go` (ADR-005 coverage, landed today) needed three
throwaway `SlapdDatabase`s and had nowhere to put them, so it creates them on
the **shared** `slapd` fixture. Two concrete taxes followed, both of which a
fixture helper would have removed:

- **Cost:** every create and delete changed the StatefulSet's `DATABASE_DIRS`
  and rolled the cluster (ADR-013's accepted wart). The container paid that four
  times — ~8-10 minutes of a suite that otherwise runs in ~13. *Removed
  2026-09-28: the list moved off the pod template (ADR-013's second amendment
  that day), so the container now rolls nothing. The argument for the fixture
  helper stands on its other leg — the shared cluster is still shared.*
- **A wrong-tree failure that only a live run could catch:** the fixtures were
  first written with suffixes *under* the shared fixture's own suffix
  (`dc=cleanupdel,dc=example,dc=org` under `dc=example,dc=org`). slapd refuses a
  database whose naming context is already served by a preceding one
  (LDAP 80, "already served by a preceding mdb database"), so the databases
  could never be created and `BeforeAll` timed out after 8 minutes. The spec was
  red — for the wrong reason. A helper that owns its own cluster would have
  given the spec its own naming-context namespace and made the collision
  impossible rather than merely detectable.

Reading for whoever picks this up: the collision is *not* a slaptain rule, it is
slapd's — any fixture that invents a suffix on a shared cluster has to pick a
tree no existing database serves.

### 2026-09-15: and the restore specs pay it as a timing coupling

The restore specs read the shared fixture's DIT (their artifact is a slapcat of
it) and time the restore machine against it, so `E2E_SCALE=1` — which inflates
that fixture to 1200+ entries and, more importantly, the operator's per-tick
workload — changed how long their waits needed to be. Their fixed 8-minute
budgets failed a restore that was still progressing. Fixed spec-side by waiting
on progress instead (see "e2e needs a liveness assertion class"), but the
coupling itself is this entry's: a spec that owned its own small cluster would
not have had its budget moved by an unrelated spec's write volume.

### 2026-09-15: and teardown could not see what the specs had created

A three-site run deadlocked in `./tests/e2e.sh teardown`. An earlier suite's
in-place restore spec had failed and kept its evidence by design
(`E2E_KEEP_ON_FAILURE`), leaving a spec-created `SlapdCluster` + `SlapdDatabase`
standing — and therefore a running pod. Teardown deleted the resources *it* had
created, then ran `kubectl delete pvc --all`; a pod object naming a PVC is a
deletion lease on it (ADR-018), so those PVCs went `Terminating` and stayed
there, the namespace never drained, and — the context loop being serial — the
other two sites were never torn down at all.

The ordering fix landed in `tests/e2e.sh` (owners before storage, by `--all`
rather than by fixture name; bounded waits; a loud line naming any leftover it
destroys). But the *cause* is this entry's: teardown has no inventory of what
the specs provisioned, because provisioning is hand-rolled per spec. A fixture
helper that registered its own cleanup would have removed both the debris and
teardown's need to guess at it — the `--all` sweep is a safety net over an
unknown set, not knowledge of it. Note the second-order cost too: the sweep now
deletes kept-on-failure evidence that somebody may have wanted, which is the
right default for a teardown but a worse trade than never needing the sweep.

## Permanent e2e coverage for in-place restore topologies

**What:** `SlapdRestore` has three topologies with different meanings (ADR-014,
amendment 2026-08-24), and automated coverage exists for only one:

| Topology | Meaning | Coverage |
|---|---|---|
| `replicas: 1`, replication off | true rollback | `restore_inplace_test.go` — green 2026-08-24 |
| N≥2, no external peers | true rollback; the accesslog wipe matters here | **verified manually only** |
| mesh member (external peers) | local re-seed, mesh repairs | none — and none is wanted as a *rollback* assertion |

`restore_replay_test.go` is the spec for row 2 — it guards the accesslog-wipe fix
(`fix(restore): wipe the accesslog LMDB on restore under replication`). It
currently runs against the **shared** primary cluster, so on a multi-site
deployment it asserts rollback semantics in the one topology where they are not
promised, and fails deterministically. It needs its own N≥2 cluster with no
external peers.

**Why deferred:** blocked on the e2e framework item above — relocating it means
hand-rolling a fourth bespoke cluster, which is the duplication that item exists
to remove. Until then the spec's red on multi-site runs is *expected* and should
not be read as a product regression.

**How:** once the fixture helper exists, move the spec onto a dedicated N≥2,
no-external-peers cluster. That restores its validity, closes the row-2 gap, and
removes the most destructive spec from the shared fixture as a side effect.

## Flake: bootstrapFrom restore intermittently never sets restoreApplied

`restore [It] restores a backup into a fresh cluster via bootstrapFrom` has failed
**2 of 5 observed runs** (2026-08-25/26), always the same way: timeout after 480 s
waiting for `restoreApplied=true`.

What is established:

- `slapd-restore-0` refuses connections on 1024 for the entire window
  (`dial ...:1024: connect: connection refused`, logged by the SlapdDatabase
  controller every 10 s).
- Because the cluster therefore never reaches `Running`, the restore state machine
  **never enters preflight** — the SlapdCluster controller logs a single line for
  that cluster for the whole run, where a healthy run shows
  `entering restore (preflight)` → `preflight passed; scaling down`.
- So `restoreApplied` was never going to flip. The timeout is a symptom; the pod
  not starting is the fault.

What is **not** established: why slapd does not start. An initial theory —
orphaned PVCs from a previous run being rebound with populated `/config` and
`/data` — is **refuted**: a later session cleared the orphans before every run and
the failure still occurred once. The `slapd-ip` and `slapd-rr` restore clusters in
the same runs start fine.

Why it had resisted diagnosis: the spec's cleanup deletes the SlapdCluster and
SlapdDatabase, so by the time anyone looked there was no pod, no CR, and no pod log
left to inspect.

### 2026-09-12: the failure is now inspectable; the flake did not reproduce

Both halves of "make it inspectable" have landed in the restore specs
(`restore_test.go` hosts the helpers; `restore_inplace_test.go` and
`restore_replay_test.go` use them):

- **Autopsy at failure time** (`dumpRestoreAutopsy`, an `AfterEach` that fires only
  on a failed spec, before any teardown): SlapdCluster + SlapdDatabase status YAML,
  every pod's phase/conditions/per-container state, the `init` and `slapd` container
  logs (plus the *previous* container's log when it has restarted), and the
  namespace events for `<cluster>*`. It runs regardless of the keep setting, so even
  a run that must leave nothing behind prints its own post-mortem.
- **Keep-on-failure** (`keepOnFailure`, default **on**; `E2E_KEEP_ON_FAILURE=0`
  restores unconditional teardown): a failed restore spec leaves its cluster,
  database, backup, service and PVCs standing and prints what was kept, the
  `slctl debug-dump -n <ns> <cluster>` one-liner, and the cleanup commands.
- **PVC leak fixed spec-side** (`deleteRestorePVCs`): green runs now delete the
  `volumeClaimTemplates` PVCs by `app.kubernetes.io/instance`. Operator behaviour is
  untouched — ADR-005's Retain default stands.

Verified by mutation (the tooling's red): the `restoreApplied` timeout was
temporarily cut to 6 s so the spec failed exactly the way the flake does — with
`slapd-restore-0` not yet serving. The autopsy printed the full init-container log,
the pod's container states, 32 events, and the keep banner; the cluster was still
standing afterwards. Mutation reverted.

**Hunt: 16 consecutive green runs, no reproduction** (scm 3-site lab, build
`fd9b420`, OpenLDAP 2.7.1): 10 runs of the bootstrapFrom spec alone
(`E2E_LABEL_FILTER=restore-bootstrap`, ~40 s each) and 6 runs of the whole
`restore` label group (~172 s each, 4 specs, replay skipped as expected on a mesh).
Every run left zero `slapd-restore*` / `slapd-ip*` / `slapd-rr*` objects behind.
So: unreproduced on *this* substrate, which is fast enough that the whole spec
finishes in 37 s against a 480 s timeout — i.e. the failing runs were roughly an
order of magnitude off normal, not marginally slow.

Still **not established**: why slapd does not start. Candidate mechanisms to check
the next time it fires, now that the evidence survives — in the order the autopsy
answers them:

1. Pod never scheduled or stuck mounting (events: `FailedScheduling`,
   `FailedAttachVolume`, `FailedMount`). The lab's storage is node-local RWO
   (`openebs-lvm`), and ADR-018's deletion-lease rule means any pod object still
   naming one of the three PVCs pins the volume to a node.
2. Init container looping or exiting non-zero (init log + `state=terminated(exit=…)`).
3. slapd crash-looping (`restarts>0` plus the *previous* container log — the reason
   a slapd that dies at startup leaves an empty current log).
4. Pod healthy but unreachable, i.e. a service/DNS/routing fault rather than a slapd
   fault (pod `Ready=True` while the SlapdDatabase reports `NoPodsReachable`).

Note that (4) is the normal *transient* state for the first ~10 s of every run — the
SlapdDatabase legitimately passes through `phase=Error`/`NoPodsReachable` before the
pod serves. The flake is that state persisting, not its appearance.

---

## Cross-site recovery after a pod-IP change: the bottleneck is downstream of discovery

### 2026-09-28: it is also the dominant term in e2e wall-clock, and it varies 50x

Two full three-site runs an hour apart, same code, same gates, same 98/99:
**953 s and 1352 s**. The 42 % swing is almost entirely this entry. Per-spec:

| spec | run A | run B |
|---|---|---|
| resilience, simultaneous restart of all pods | 5 s | **4m20s** |
| resilience, one pod restarted | 5 s | **45 s** |

4m20s is inside the 130-268 s band this entry already records. The *5 s* readings
are the anomaly: they mean Kubernetes happened to give the pods back their
previous IPs, so no peer had anything to rediscover. Nothing in the fixture
controls that, so any single measurement of "how long does a restart cost" is a
coin flip between ~5 s and ~5 min.

Two consequences worth keeping:

- **A runtime regression hunt should start here.** The obvious suspects for a
  suite that got 400 s slower — a new probe, a new condition, an added wait —
  were all wrong; the answer was IPAM luck in one spec. Comparing per-spec
  `• [N seconds]` lines finds it in a minute, where comparing totals does not.
- **A live demo should not restart a pod and expect cross-site convergence.**
  On the unlucky side of that coin it is five minutes of nothing happening.

A third, smaller confound from the same comparison, unrelated to this entry:
Ginkgo's seed changes the spec order, and `job-lease`'s "no terminated pod holds
a slapd PVC" check landed straight after the `replicated restore` specs in one
run (90 s waiting for their Job pods to be reaped) and after a quiet spec in the
other (0.02 s). Both pass; the budget is doing its job. It is a reminder that
this suite's specs share one cluster and its timings are not independent.


Supersedes the earlier item "Peer address discovery has no vote in the requeue
decision", whose premise **measurement refuted**.

Under ADR-016 pod-routed transport, replacing a pod invalidates every peer's syncrepl
addresses and recovery takes minutes. The theory was that peer address discovery rides
on the 60 s `csnCheckInterval` and cannot ask for anything faster. That was
implemented — a 15 s discovery cadence decoupled from CSN monitoring, plus a 3 s
settle-tightening while the observed address set is still changing — and measured on
three pod-routed sites:

| build | full site restart | single pod |
|---|---|---|
| baseline | 146 s, 353 s, 308 s | 31 s, 6 s |
| decoupled cadence | 189 s, 360 s, 145 s | 75 s, 34 s |

Indistinguishable, with the mechanism verifiably firing (15 s reconcile spacing,
discovery-only passes logged). Reverted (`e692190`); implementation preserved in
`11dc2c4` if it is ever wanted as a component of a real fix.

A decomposition trace of one full-restart recovery:

    t+7s    siteA Ready with new pod IPs
    t+148s  siteB's status carries all the new IPs
    t+289s  a siteA write is visible on siteB
    t+432s  siteB's syncrepl stanzas name the new IPs

**The dominant term is downstream of discovery and is still unidentified** —
stanza-rewrite scheduling, or slapd's own consumer reconnect. The trace is also
internally inconsistent (stanzas appearing to be rewritten *after* the write was
already replicating), and the probe that produced the stanza timing was unreliable:
siteB names one provider per peer, so an "all IPs present" check can never pass.

**Next step for whoever takes this: instrument the stanza-rewrite path, do not tune
another interval.** Specifically, timestamp (a) the `SlapdCluster` status write
carrying new addresses, (b) the `SlapdDatabase` reconcile that consumes it, (c) the
`olcSyncRepl` modify landing on each pod, and (d) the consumer's first successful
connection. Note the baseline spans 146-353 s, so any claim of improvement needs
distributions rather than single samples.

Worth checking early, because it would explain the gap: the `SlapdDatabase`
controller's requeue is 10 s only on *unmet preconditions*, and a pod whose database
reconcile fails is skipped for stanza writes entirely (the ADR-019 R8 stanza-deferral
fix). During a mass pod replacement, pods legitimately fail that reconcile for a
while — so stanza rewrites may be deferred for reasons unrelated to discovery.

---

## SlapdDatabase and SlapdSchema need an explicit periodic resync

Prerequisite for any watch filtering, and a gap in its own right. See the ADR-002
amendment and `docs/reconcile-loop-fixes.md`, both 2026-08-26.

Neither controller requeues on its success path, so neither re-examines per-pod
`cn=config` on any schedule. The resync that exists today is **incidental**:
`checkPeerCSNConvergence` stamps `lastChecked` every 60 s, that changes the
`SlapdCluster` status, and the unfiltered watch re-reconciles every database in the
namespace. Everything that converges without a CR change rides on it — drift, hand
edits, re-applied ACLs.

*(Amended 2026-09-14: the ADR-019 R8 migration used to be the fourth item on
that list and is gone with R8. This does **not** unblock the watch filter — the
reverted `5dd5e3f` failed four specs, three of which had nothing to do with R8,
and the remaining three reasons are unchanged.)*

Two things follow:

- **Filtering that watch is unsafe until the resync is explicit.** Already attempted
  and reverted: a predicate faithful to the four fields the controller reads failed
  four e2e specs.
- **A cluster with `replication.enabled: false` has no periodic resync at all**, since
  the `SlapdCluster` controller only requeues periodically when replication is
  enabled. Standalone-cluster drift is corrected only if some unrelated event fires.
  Nobody intended this and no e2e covers it.

The work: give both controllers their own `RequeueAfter` sized for drift correction
(independent of `csnCheckInterval`, which exists for CSN monitoring), add an e2e that
proves drift is corrected without any CR change — hand-edit an ACL on one pod, wait,
assert it is restored — and only then revisit the watch predicate. That e2e is the
piece with real value: it would have caught the reverted change directly rather than
via four unrelated failures.

---

## Cross-site replication health is not readable from one site

Not a defect — a documentation and tooling gap, recorded so nobody builds
monitoring on a false assumption. See the ADR-008 amendment of 2026-08-26.

`status.externalPeerStatuses` is **consumer-side**: it describes this site's
*inbound* links. Because delta-syncrepl is pull-based and a provider keeps no
consumer registry, a provider cannot observe that a consumer stopped consuming from
it. So if B stops consuming from A, that is visible on **B**, and A correctly reports
`Synced` at the same time. Reading mesh health means reading every site's CR.

Two follow-ups worth considering:

- **`slctl` has no cross-site view.** `slctl inspect` is single-cluster. Something
  that takes several contexts and joins each site's peer statuses into one mesh
  verdict would turn "read four CRs and correlate by hand" into one command. This is
  the natural home for the join, and it needs no operator change.
- **Lag is only observable under traffic.** `csnSyncThreshold` is 5 s, so a broken
  link shows as `Lagging` promptly *while writes flow*; on an idle database both
  sides sit at the same CSN and the state reads `Synced` across an arbitrarily broken
  link. Heartbeat writes would close that, and per the ADR-008 amendment of
  2026-09-11 would also close a real gap: idleness lets a serverID go dormant,
  which is what makes `syncprov`'s minCSN fallback pick a stale lookup key and
  trigger the ITS#9580 "sync cookie is stale" full-refresh storm on reconnect
  (see `docs/INVESTIGATION-replication-divergence-after-dataloss-and-restart.md`).
  They stay deferred anyway: the OpenLDAP 2.7 upgrade (ADR-021) and the syncprov
  sessionlog's per-SID viability check (ADR-022) act on that defect directly, and
  the objection that the operator would be writing into the user's data tree
  still applies to a heartbeat, which would only mask it. This is the second
  time the system has turned out to depend on periodic activity nobody
  deliberately generates — the first was the incidental 60 s-tick resync
  found in the ADR-002 amendment (2026-08-26).

---

## e2e cannot pin an old `slapd-init` image, so one migration failure mode has no guard

The 2026-08-25 "syncrepl stanzas written to a pod whose accesslog DB does not
exist" fix is proven only by hand-run live scenarios. A permanent regression test
needs pods whose init container predates the per-database accesslog directory
creation, because the missing thing is a *directory on a PVC the operator cannot
touch* (ADR-018) — unlike a `cn=config` shape, which a spec can manufacture by
hand. (The gated ADR-019 R8 migration spec did exactly that trick; it was
removed with R8 on 2026-09-14, and manufacturing a `cn=config` pre-state against
a live operator is part of what went wrong there — see ADR-026 C1.)

What is missing is a fixture capability: deploying a cluster with `spec.images`
pinned to a chosen older init tag, then upgrading the operator underneath it.
That overlaps with "e2e framework: specs cannot provision their own topology"
above and should probably be solved with it rather than separately.

---

## Cross-site orchestration (hub-and-spoke) — explicitly undecided

**What:** automating the mesh-wide rollback runbook (`docs/BACKUP.md`, "Rolling
back a replicated deployment") so it becomes one declarative action instead of a
careful manual sequence across N clusters.

**Why this is not just a feature:** it requires one actor to drive *other*
clusters — quiesce them, run restores there, and sequence the result. Today the
operator reaches across clusters only to **read**: it discovers peer pod
addresses through a remote kubeconfig (ADR-007 amendment, ADR-016). Driving a
remote site is categorically different. It introduces a control plane, and with
it hub failure, partition behaviour, and arbitration — and it stands in direct
tension with this project's second architectural requirement, that each site be
autonomous.

**Why deferred, and deliberately undecided:** this is a fundamentals question,
not a backlog chore. It is recorded here so the option is not lost, *not* as an
agreed direction. The current answer — a human-operated distributed runbook — is
legitimate and is how comparable systems document the same operation. Nothing
about it is broken; it is manual.

**If pursued:** it gets its own ADR, deciding the topology (a control-plane
cluster? a peer-elected coordinator? a CLI-driven sequence with no new
controller?) and the failure semantics before any code. See the ADR-014
amendment (2026-08-24), section "Not decided: hub-and-spoke orchestration".

---

## ADR-022 follow-ups: the persistent sessionlog source

**The checkpoint half is DONE (2026-09-13).** `olcSpCheckpoint` now converges on
every reconcile alongside `olcSpSessionlog`, off a single search of the syncprov
overlay; the `planSessionlog` seam did generalize, as `planOverlayAttr`. See the
R4-debt entry above.

**The source half is DONE (2026-10-15, ADR-034)** —
`spec.replication.syncprovSessionlogSource: Accesslog`, opt-in and startup-only.
ADR-022's reservation (it reads the journal, the one artifact purged or
contaminated in exactly the scenarios a persistent log would pay for) is why it
is opt-in rather than defaulted, and it still stands as a reservation: a
consumer whose cookie predates the purge still takes a full refresh.

Still open from this entry: the measurement — what sourcing the log actually
costs against an in-memory baseline, under the usual gate.

---

## ridBase cross-CR uniqueness is documented but not enforced

`spec.replication.ridBase` must be unique across the SlapdDatabase CRs of one
cluster (colliding values produce colliding RIDs, which corrupts per-consumer
cookie state). The field's doc comment used to claim "the operator validates
this" — it never did; the only admission rule is the CEL presence check. Found
2026-09-12 while retiring the foreignRIDs entry.

The fix is a reconcile-time check in the SlapdDatabase controller (list sibling
SlapdDatabases of the same cluster, refuse with phase=Error on a ridBase whose
stanza range overlaps another's), plus a red-first e2e: two databases with the
same ridBase, assert the second reports the collision instead of writing
colliding stanzas. A webhook would also work but the project has none — do not
grow one just for this.

---

## An unhealthy pod blocks its own replacement, so a bad image change is a one-way door

Found 2026-10-23 while measuring ADR-041's case, and it is the half that
ADR-041 deliberately does NOT fix.

Apply a bad image (wrong LMDB format, a tag that does not exist, anything that
stops the pod reaching Ready) and the StatefulSet rolls it. Then correct the
spec. The template updates — and **the pod stays on the old image**. Under
`podManagementPolicy: OrderedReady` the controller will not replace a pod that
is not Ready, so the broken pod blocks the very update that would fix it.

Measured: `storageFormat` put back to the correct value, StatefulSet template
confirmed back on the 2.7 image, pod still running the `-ol26` image through
five restarts with no progress. `kubectl delete pod slapd-2` recovered it in
under 15 s with both databases intact.

So the user reaches this with one CR edit and cannot leave it with another.
ADR-041's preflight makes the state much harder to ENTER; nothing makes it
easier to LEAVE, and the format mismatch is only one of the ways in.

**Why it is not obviously the operator's to fix.** Deleting a user's pod is a
destructive act on state the operator did not create, and "the pod is not Ready
and its image differs from the template" is also true, transiently, during
every normal roll. A fix has to distinguish "mid-roll" from "wedged", and the
honest signal for that is time plus restart count, which is a heuristic.

Shape of a fix, in increasing order of nerve:

1. **Say so.** A condition naming the pod, its image, the template's image, and
   the fact that correcting the spec will not move it — with the
   `kubectl delete pod` escape. Pure observability, no new risk, and it closes
   the gap between "the operator knows" and "the user knows".
2. **Offer it.** An explicit, imperative, one-shot request (the
   `SlapdRollingReplace`/`SlapdForceWritable` shape) that carries the human's
   assertion and does the delete.
3. **Do it.** Automatically delete a pod that is crashlooping on a stale
   revision past some threshold. Needs the heuristic above to be right, and
   gets it wrong during a slow image pull.

1 is clearly worth building and is a small amount of work. 2 fits the existing
pattern. 3 wants evidence nobody has.

Documented meanwhile in `docs/OPENLDAP-VERSIONS.md`, which now tells the reader
that correcting the spec is necessary but not sufficient.

## A version-crossing image change on a populated cluster fails without explanation

Editing a populated 2.6 cluster's `spec.images` to the 2.7 pair (or back)
crashloops every pod on volumes the new slapd cannot open (LMDB 1.0 format
break, ADR-021). The operator neither refuses the change nor explains the
failure — the user gets CrashLoopBackOff and has to find the runbook.

Honest constraints: image tags are free-form (nothing reliable marks a tag as
"2.6" or "2.7"), and ADR-018 means the operator cannot inspect the volumes. So
a hard admission guard is likely impossible without new API surface (e.g. an
explicit `spec.images.generation` the deployer asserts). The realistic minimum
is observability: when pods crashloop after an image change, surface the slapd
startup error (the LMDB version complaint is in the container log) into a
SlapdCluster condition with a pointer to docs/OPENLDAP-VERSIONS.md. Decide the
shape before building; do not grow a webhook for this.

---

### MEASURED 2026-10-23, and it is worse than this entry assumed

The "new API surface" this entry asked for now exists:
`spec.images.select.storageFormat` (ADR-040). So the operator knows what format
the user *declared*, which is half the problem. The other half was measured on
a live single-site cluster: three RW pods populated on OpenLDAP 2.7, then
`storageFormat: lmdb0.9` applied and the roll watched.

**1. The failure message is not merely terse, it is WRONG about the remedy.**

```
mdb_db_open: database "cn=slaptain-auth" cannot be opened:
MDB_INVALID: File is not an LMDB file (-30793). Restore from backup!
backend_startup_one (type=mdb, suffix="cn=slaptain-auth"): bi_db_open failed! (-30793)
slapd stopped.
```

The volume is perfectly healthy. "Restore from backup!" is advice that, if
followed, performs a risky and pointless operation on good data — and on a
cluster where the operator is *also* mid-roll, a restore is the ADR-014
scale-to-0 machine. This is the single strongest argument for the preflight:
the native failure does not merely fail to explain, it points at a destructive
action.

**2. It is `MDB_INVALID`, not `MDB_VERSION_MISMATCH`** — which a source read
predicted and the measurement refuted. LMDB 1.0 changed the meta page enough
that 2.6's reader does not recognise it as a meta page at all, so it fails
*before* the version check it would otherwise have reported. Anyone searching
the error gets corruption advice, not version advice. (Direction tested:
2.6 image against 2.7 volumes. The reverse — a 2.6 volume under a 2.7 image,
which is the direction ADR-021's migration runbook travels — is UNMEASURED and
may well report differently.)

**3. It names `cn=slaptain-auth`**, the ADR-027 node-local auth database,
because it is the first mdb opened. That is an internal database the user never
declared and will not recognise.

**4. The init container passes cleanly.** It only touches `/config` (LDIF), so
nothing in the current startup path opens an LMDB data environment before
slapd does. A preflight has to be added; there is no existing check to extend.

**5. Containment is good.** `OrderedReady` halts the roll at the first bad pod:
pods 0 and 1 stayed Running on 2.7 and the cluster served at 2/3, reporting
`Degraded`, `Ready=False NotReady: 2/3`, and `PodsResponsive=False` naming
`slapd-2` with "connection refused". Accurate, and says nothing about why.

**6. Reverting the spec does NOT heal it — this is the trap.** Setting
`storageFormat` back to `lmdb1.0` updated the StatefulSet template, and the pod
**stayed on the wrong image indefinitely**. Under `OrderedReady` the controller
will not replace an unhealthy pod, so the broken pod blocks its own
replacement. Measured: template back on 2.7, pod still `-ol26`, five restarts,
no progress. A `kubectl delete pod slapd-2` recovered it in under 15 s with
both databases intact (`DataPresent=True` on each).

So a user can reach a stuck state with one CR edit and **cannot leave it with
another**. That converts the preflight from "nicer diagnostics" into "the
difference between a refusal and a manual recovery", and it deserves its own
treatment regardless of the preflight: the operator could notice a pod whose
image does not match the template and whose container is crashlooping, and say
so — or the unhealthy-pod-blocks-its-own-replacement interaction could be
handled directly.

**Shape of the preflight, now that the mechanism is known.** `slaptest` WITHOUT
`-u` is the instrument: `slapcommon.c:936` runs `slap_startup(be)`, which opens
every database through back-mdb — the same binary set, the same code path, no
new packaging, perfect parity by construction (the project already relies on
the `-u` distinction in the openldap-deb build gate, which passes `-u`
precisely to avoid opening databases). Run it in the init container after the
config is in place; on failure write a message naming the declared format, the
image, and the two ways out (`storageFormat` or the
`docs/OPENLDAP-VERSIONS.md` dump/reload) to `/dev/termination-log`, and have
the operator surface `initContainerStatuses[].lastState.terminated.message`
on the CR. The operator never touches the volume, so ADR-018 is not engaged.

Rejected on the way: reading the LMDB meta page directly (reimplements a struct
and hardcodes a format table — and would have missed a truncated file), and
packaging `mdb_stat` (OpenLDAP never builds liblmdb as a library —
`back-mdb/Makefile.in:52-56` compiles `mdb.c` straight into the module — so
there is nothing to link against and the flags would have to be re-derived).
`mdb_stat` remains independently worth packaging for `slctl debug` on-disk
inspection, but it is not the preflight.

---

## `kubectl rollout status` returns before readiness on slaptain StatefulSets

Observed during the v0.1.0 demo rehearsal: `kubectl rollout status
statefulset/slapd` completes in ~0.1 s with "partitioned roll out complete"
while the pod is still ContainerCreating — the documented wait gates were
decorative. The controller sets no `updateStrategy` explicitly, so the
partition involved is API-server defaulting; whether kubectl's partitioned
branch (which checks `updatedReplicas`, never readiness) should trigger on a
defaulted partition of 0 needs root-causing before blaming anyone. Fix
Discipline applies: reproduce, read the kubectl branch condition, then decide
whether the operator should set an explicit strategy or the docs should simply
never use `rollout status` as a readiness gate (the demo now waits on
`status.readyReplicas`, which cannot lie).

### 2026-09-16: `readyReplicas` *can* lie, and did

The parenthesis above is wrong: the slapd container has no probes at all, so
`status.readyReplicas` is no more trustworthy than `rollout status` was.
Promoted to its own entry — see "The slapd container has no `readinessProbe`"
below.

---

## ~~The slapd container has no `readinessProbe` — `readyReplicas` can lie~~ — DONE (2026-09-28, ADR-030)

*Promoted 2026-09-27 out of a subsection of "`kubectl rollout status` returns
before readiness", where it was easy to miss. The finding itself is 2026-09-16.*

The slapd container defines **no `readinessProbe` and no `livenessProbe`**, so
"Ready" means only that the process exists. A slapd frozen for 874 s — accepting
TCP, answering nothing, logging nothing — held `readyReplicas: 3`,
`phase=Running` and its place in the Service endpoints for the entire freeze
(see "The `cn=config` pause freeze" below). Every wait built on `readyReplicas`,
in the demo and in e2e, inherits that blindness.

**Why it was never urgent, which is the reusable part.** ADR-013 (2026-05-14)
asked the *hard* question — should readiness mean "slapd has converged data"? —
and deferred it for reasons that still hold (what "converged" means; the
implementation surface). The easy question — "does slapd answer LDAP at all?" —
has none of those problems and was never asked separately. It rode along in the
deferral of the ambitious one. That is how a trivial guard goes missing for four
months: not by being rejected, but by being bundled.

**Wanted:** a `readinessProbe` doing a real rootDSE query, with a
`failureThreshold` sized well above a normal sub-second `cn=config` pause so
legitimate config writes never flap pods out of the Service, and **no**
liveness probe — restarting a pod mid-freeze would invalidate every peer's
syncrepl addresses again under ADR-016, turning a recoverable stall into a
topology event. Sizing that threshold is the whole design question and wants
measured pause durations, not a guess.

**Four constraints to design against:**

1. **`publishNotReadyAddresses` is not set on the headless Service** (verified in
   `reconcileHeadlessService`; it defaults false). Every per-pod operation the
   operator performs resolves `<name>-<ordinal>.<name>-headless…`. A NotReady pod
   leaves that DNS, so the operator cannot dial it, cannot converge it, and
   ADR-027's gate then withholds syncrepl site-wide. ADR-013 already named the
   fix and it never shipped, because the probe it was written for never shipped.
   Land it **on its own, before the probe**: today nothing is ever NotReady, so
   it is a literal no-op, and shipping it in the same commit as the probe is how
   the deadlock gets discovered the hard way.
2. **`OrderedReady` turns a wrong probe into "no cluster at all."** Ready is
   trivially true today, so the StatefulSet always proceeds. With a probe, a
   pod-0 that never goes Ready means pod-1 is never *created* — and the operator
   cannot help, because there is no pod to converge.
3. **Distroless has no exec probe.** kubelet offers HTTPGet, TCPSocket, Exec.
   slapd speaks no HTTP; TCPSocket is what we have and is *proven* insufficient
   (the frozen pod accepted TCP throughout); Exec needs a binary in the image —
   the cost of ADR-013's option C', resurfacing, and the second item in the class
   `BUG-ANALYSIS-database-dirs-rolling-restart.md` opened. `spec.readinessGates`
   driven by the operator avoids touching the image and reuses an observation it
   already makes every 60 s, but an unset gate defaults False, which is
   constraint 2 wearing a different hat. Rule it out on the record, not by
   omission.
4. **Any LDAP answer is success**, `err=50` / `err=32` included: anonymous access
   to the rootDSE may legitimately be denied by ACLs (ADR-025 treats uniform
   invisibility as normal, not as divergence). Only timeout and
   connection-refused are failures. What is measured is "the server answers",
   never "the server answers *me*".

**Free first step, independent of all four.** The operator already makes this
observation and throws it away: 106 consecutive bind timeouts against the frozen
pod, logged at INFO, reaching nobody. Promoting that to a status condition does
not fix Service endpoints, but it makes the failure *visible* — most of what was
missing — at zero risk to bootstrap.

**Treat the probe as a replication-path change** and run one multi-site cycle
before merging. Specs waiting on `readyReplicas` will start genuinely waiting
instead of passing spuriously; that is the point, and it will surface latent
timing assumptions.

### 2026-09-28: shipped as ADR-030, and the hard constraint dissolved

All four constraints survived contact, and constraint 3 — "distroless has no
exec probe" — turned out to be cheaper than written: the slapd image's build
stage **already installs `ldap-utils`** from the vendored fork (ADR-021), so a
version-matched LDAP client was one `cp` away. No Go stage, no repo-root build
context, no resurrection of ADR-013's option C'.

`ldapwhoami`, not `ldapsearch`, and that turns constraint 4 from a requirement
into a non-problem: WhoAmI is an extended operation that reads no entry, so the
probe cannot be broken by a user's ACLs at all. There is nothing to special-case
about `err=50` / `err=32` because they cannot arise. (`ldapsearch` would also
have needed exit-code post-processing that a shell-free image cannot do.)

Constraint 1 landed **alone and first**, as this entry insisted, while it was
still a literal no-op. Constraint 2 is answered by a 300 s `startupProbe`, so a
slow LMDB open is waited out rather than turned into a cluster that never forms.
`readinessGates` are ruled out on the record in ADR-030 rather than by omission.

Thresholds: startup 300 s, readiness 60 s. The 60 s is reasoned from both ends
(well above a routine sub-second `cn=config` pause, far below the 874 s freeze).
The 300 s is a **guess** — see the big-DIT entry below, whose fourth consumer
this now is.

**The "free first step" shipped too**, as the `PodsResponsive` condition on
SlapdCluster. Same readings the 60 s CSN tick already takes, so no extra
queries; the same answer/silence rule as the probe, so a bind rejection is not
mistaken for a dead pod; and a pod unreadable on one database but answering on
another stays responsive, because that case already has a reporter
(`CSNQueriesIncomplete`). The condition's `lastTransitionTime` is the duration
the 106 log lines never gave anyone.

---

## The `cn=config` pause freeze: mechanism known, trigger unreproduced

**What:** any write to `cn=config` pauses the whole server until every other
threadpool task is idle (`slap_pause_server` → `ldap_pvt_thread_pool_pause`,
`libraries/libldap/tpool.c:1304-1307`). A task blocked on a silent peer
therefore freezes the entire slapd for its own duration, and the operator
issues those writes *precisely* when a peer has become unreachable, because
ADR-016 stanzas name pod IPs. Measured once, on two sites in the same window:
**874.016 s** and **864.310 s**, each ending with the completion of the
operator's own `MOD olcSyncRepl`.

**What is NOT known:** what held a task that long. Four targeted reproductions
on a three-site lab all completed their config write in milliseconds — graceful
mass pod delete, permanent peer removal, a no-RST cross-site blackhole, and the
same blackhole applied mid-refresh with a 6000-entry transfer in flight. They
share one property: the consumers were idle, and an idle persist-phase consumer
polls with `tout={0,0}` and yields, holding no task. So routine pod churn does
not arm this. The state that does is a consumer blocked *mid-operation* when its
peer goes silent; producing that deliberately is the open problem.

**Why it matters anyway:** while it lasts, the pod serves nothing, Kubernetes
cannot see it (no probes), and the ADR-027 gate turns it into a site-wide
replication stall (next entry).

**How to pick it up — instrument before theorising.** Land the liveness
assertion class first ("e2e needs a liveness assertion class" above): parse
`etime` from slapd stats logs and assert no all-pods-silent gap during
convergence. The next occurrence then arrives with evidence instead of
inference. Two instrument traps cost three wrong readings during the original
investigation and will cost them again: `kubectl logs --since` reads **only the
current file**, and these pods rotate every ~90 s under `logLevel: 16640`
(use the node's `/var/log/pods/.../0.log*`); and an `ipBlock`-only NetworkPolicy
denies **in-cluster** pod traffic too, because Cilium matches pod sources by
identity rather than CIDR (add a `namespaceSelector: {}` allow rule).

**Explicitly rejected until it is reproducible:** shortening the shipped
`keepalive` default from `240:3:30`. The 874 s observation sits just under the
e2e fixture's non-default `300:10:60` bound (900 s), which is suggestive
arithmetic and nothing more. Full reasoning in the ledger entry.

---

## ADR-027's identity gate is all-or-nothing per site

**What:** `slapddatabase_controller.go:302-320` withholds syncrepl configuration
while *any* RW pod is missing its node-local identity entry. The invariant is
sound — point a consumer at a provider lacking the entry and the bind DN does
not resolve there, so it fails with `err=49` forever — but the scope is wider
than the invariant needs.

**What it cost, measured:** one wedged pod (unable to answer the identity probe)
withheld **every stanza for every database on every pod at that site**, for the
whole 874 s freeze. The site therefore never repointed its external stanzas at a
peer's new pod IPs, and the e2e cross-site recovery spec failed at its 480 s
budget. The freeze was the cause; the gate's scope is what turned one sick pod
into a site-wide replication stall.

**Wanted:** withhold only the stanzas that *name* an unconverged pod. A stanza
pointing at a healthy provider is not made unsafe by a third pod's missing
entry. That preserves the ordering guarantee the cutover rests on while
removing the amplification.

**Note it is a guard narrowing, not a bug fix:** the gate does what ADR-027 says
it should. Changing it needs an ADR-027 amendment recording why the narrower
scope still satisfies the cutover's ordering property, and what an operator
should see instead (today the withholding is an INFO line and nothing surfaces
on the CR — arguably the worse half of this entry).

---

## Rotating `replication-password` does not revoke the leaked credential

**What:** `ensureReplicationUser` is create-only — it searches for
`cn=replication,<suffix>`, finds it, and returns without comparing anything
(`slapddatabase_controller.go:2013`). ADR-027 made the *node-local* identity
converged, so a rotation of the Secret's `replication-password` does move
`cn=repl-<db>,cn=slaptain-auth` and every stanza's `credentials=` (measured 52 s
across four pods). The legacy entry is not touched: it keeps the **old**
password, and ADR-027 steps 2+3 deliberately keep its `olcAccess` read-all and
its `olcLimits` exemption on both the data DB and the journal.

**Consequence:** rotation today is hygiene, not containment. If the password
leaked, the leaked value still binds as `cn=replication,<suffix>` afterwards,
with read over the whole data tree *and* over the accesslog — which is the
ADR-020 bypass surface (an anonymous read of a shared journal once returned
`reqMod: userPassword:+ {SSHA}…` for a user the data DB denies; the legacy
identity is explicitly granted there). There is no revocation path that the
operator performs.

**Why it is not the DONE item above:** that entry closed the two-store
*drift* problem and records this window's caveats as an availability concern —
"rotating the Secret mid-window strands the create-only legacy entry", i.e. a
not-yet-upgraded peer locks out. The security reading is sharper and is written
down nowhere: the stale copy is not merely useless, it is a live credential the
rotation was supposed to kill.

**The real fix is already scheduled:** ADR-027 migration step 4 drops the legacy
grants, which makes the stale entry inert (deleting the entry itself stays a
manual cleanup — ADR-026 R2, it is replicated state the operator does not own).
Until that release lands, an incident cannot wait, so what is missing is the
interim answer.

**Options considered:**

1. **Converge the legacy entry too** — a small change to `ensureReplicationUser`,
   and rejected: it writes into the replicated customer tree on every pass,
   which is exactly the shared multi-writer state ADR-026 R2 forbids and half of
   why ADR-027 moved the identity out.
2. **Runbook** (preferred): rotate the Secret, then delete or repair
   `cn=replication,<suffix>` by hand, accepting that un-upgraded peers break
   until they upgrade. Matches the ADR-019 R8 precedent — a manual runbook in
   preference to an unsafe automated write.
3. **Make step 4 conditional** on observing that every peer is upgraded. The
   operator cannot see a remote site's auth database, so cross-site this
   collapses back into (2).

**Wanted:** an ADR-027 amendment stating the leaked-credential case explicitly
(rotation ≠ revocation while the legacy grants stand), plus the (2) runbook
written down where an operator under time pressure will find it. Step 4 then
closes it for good.

---

## Designed, deferred: opt-in `SlapdBackup.spec.requireConverged` with a bounded wait

A backup always `slapcat`s pod-0, so on a replicating cluster that is briefly
behind, a write ACKed elsewhere can legitimately be missing from the artifact
(demonstrated 2026-09-12; ADR-014 amendment of that date). That amendment fixed
the *silence*: every backup now records `status.sourcePod`,
`status.sourceContextCSN` and a `SourceConverged` condition. It deliberately did
**not** add a gate.

The opt-in gate is designed and parked here until someone asks for it:

- `spec.requireConverged: true` (never default) plus `spec.convergenceTimeout`;
- while the target `SlapdCluster` reports `ReplicationConverged != True`, the
  backup stays `Pending` with a reason saying what it is waiting for;
- on timeout it **takes the backup anyway** and records that it waited and gave
  up. A backup that declines to run is a backup you do not have; "requireConverged"
  must never become "no artifact exists".

Rejected outright, and not to be revisited without new information: a strict
CSN-dominance gate (refuse unless the source's CSN vector dominates every
peer's). A replica of a heavily-written provider never dominates, and under
multi-master two vectors can be mutually incomparable — the predicate is both
unusable and ill-defined.

Also unbuilt, cheap, and independent: stamping `sourcePod` + the converged flag
into the S3 **object metadata** at upload, so a restore can surface them without
the CR. `backup.Upload` already accepts a metadata map; what is missing is a way
for the Job's uploader to carry arbitrary pairs (`manager backup-upload
--meta k=v`).

---

## Scale and operations tunables — from the production-config review, 2026-09-12

A review of slaptain's generated `cn=config` against a large production OpenLDAP
platform produced 18 findings. The five that break a large cluster outright are
have **landed** (2026-09-12); **ADR-024 fixes where each of these lives**
(converged per pod / bootstrap-time / create-only-by-nature, and which CR owns
it), so the entries below record the gap and the severity, not the design. Pick
any of them up by reading ADR-024's placement table first — the class is already
decided.

Already done, listed so nobody re-raises them: the replication identity's
`olcLimits` exemption on the data and accesslog databases (ADR-020 amendment —
this was a silent 500-entry replication cap), `olcDbMaxSize` on both with
operator defaults and a converge-on-change path that refuses a shrink out loud,
per-database `sizeLimit`/`timeLimit`/`limits`, the `entryCSN`/`entryUUID`
baseline indices on data databases, and `spec.backend.idlExponent`. The
many-entries fixture class ADR-024 asks for is `tests/e2e/scale_test.go`, gated
`E2E_SCALE=1` — every one of the five is structurally invisible without it.

**Landed 2026-09-13** (the degrades-and-hygiene half): `olcDbCheckpoint`,
`olcDbNoSync` (now converged and inheriting a cluster-wide
`spec.tuning.noSync`), `olcDbRtxnSize`, `olcDbEnvFlags` (create-only and
reported), the syncrepl stanza timeouts and a default keepalive,
`slapadd -q` + `spec.tuning.toolThreads` for restores,
`spec.ldap.tls.{protocolMin,cipherSuite}`, `spec.ldap.passwordHash`, and a
`logLevel` default of 16640. Guarded by `tests/e2e/tunables_test.go` (ungated).
Three of the review's items did NOT survive contact with a live cluster and are
re-entered below with their evidence.

- **Connection lifetimes cannot be converged — `olcIdleTimeout` and
  `olcWriteTimeout`** (degrades; was the urgent half of the review's finding
  10). slapd's default for both is *never close*, so a pod behind a stateful
  firewall accumulates connections whose peer is long gone until it runs out of
  file descriptors. The gap is real. The **placement was wrong**: an
  `ldapmodify` of either attribute against a running OpenLDAP 2.7.1 does not
  fail and does not crash — **it hangs the server**. The modify's CSN is queued
  and never graduates, the pod then answers nothing at all (not even an
  anonymous rootDSE), and `SIGTERM` sticks in `slapd shutdown: waiting for 2
  operations/tasks to finish`:

  ```
  conn=1050 op=2 MOD dn="cn=config"
  conn=1050 op=2 MOD attr=olcIdleTimeout
  slap_get_csn: conn=1050 op=2 generated new csn=…
  slap_queue_csn: queueing …
  <nothing, ever>
  ```

  Reproduced twice on a healthy three-pod cluster (three of four pods the first
  time, all three RW pods the second); recovery was restarting every pod. The
  mechanism is the daemon thread: a non-zero `global_idletimeout` arms
  `connections_timeout_idle`, which walks the connection table from the daemon
  loop while the modify that armed it is still executing inside one of those
  connections. Both values are **fine when they come from the boot config** — a
  pod that *started* with `olcIdleTimeout 3600` / `olcWriteTimeout 300` served
  normally for twenty minutes. *Surface:* ADR-024 **R2**, bootstrap-time —
  `idletimeout`/`writetimeout` directives in the init container's generated
  `slapd.conf`, with R2's documented recreate path. No CRD field until then: a
  field the operator cannot honour is what R4 forbids.
- **Thread and buffer counts** (degrades, unmeasured). `olcThreads`,
  `olcListenerThreads`, `olcConcurrency`, `olcSockbufMaxIncoming`,
  `olcSockbufMaxIncomingAuth`, `olcConnMaxPending` are neither set nor exposed.
  Upstream's defaults are defensible and the reference production platform
  leaves them alone too, so these wait for a measurement that asks for one.
  `spec.tuning` is the home when one does.
- ~~**No `cn=monitor`, no native metrics**~~ (*resolved by the ADR-033 amendment
  of 2026-10-08*). The monitor database is converged on every pod with a constant
  ACL, it is ensured before every mdb database ADD so back-mdb's per-database
  statistics attach, and at rest the operator writes nothing. The withdrawal on 2026-09-13 was blamed on the monitor
  ACL modify. The amendment re-measured that and puts the hang on the `timeout=`
  freeze instead. See `docs/adrs/adr-033-three-monitoring-surfaces.md` and
  `docs/TUNING.md`.
- **TLS: no CRL policy, no `olcLocalSSF`/`olcSecurity`** (hygiene; the protocol
  floor and cipher policy landed 2026-09-13 as `spec.ldap.tls.protocolMin` /
  `.cipherSuite`). `olcTLSCRLCheck` and a minimum-SSF statement are still
  unstated, so a revoked peer certificate is accepted and there is nothing
  forcing a client onto TLS. *Surface:* the same converged block.
- **A stronger password-hashing scheme is not available** (hygiene; the policy
  field landed 2026-09-13 as `spec.ldap.passwordHash`, pinned to `{SSHA}` and
  converged). The module-availability question the review asked is answered:
  slaptain's runtime image does **not** ship `pw-argon2`, so `{ARGON2}` cannot be
  selected today — asking for it would produce a slapd that rejects every
  password write. Nor is `olcPasswordCryptSaltFormat` exposed. *Surface:* build
  the module into `images/slapd/Containerfile` first; the CRD field is already
  there to point at it.
- **Overlay surface beyond replication** (hygiene, document-only). We manage
  `syncprov` and `accesslog` and nothing else; the production platform also runs
  password-policy, uniqueness, dynamic-list and member-of overlays plus
  last-bind tracking. Not a scale defect — it is the difference between a
  replicating store and a directory an application stack can sit on. Worth a
  stated position (explicitly out of scope, or an ADR for a generic overlay
  surface on `SlapdDatabase`) rather than silence.

**Stale prior comparison.** An older written-down comparison lists
`olcSpCheckpoint`, `olcSpSessionlog`, `olcAccessLogPurge`,
`olcAccessLogOps`/`Success`, syncrepl `retry`, the accesslog indices and
per-database accesslog separation as missing. They are not: ADR-019, ADR-022 and
the syncprov overlay code cover all of them. Do not re-raise them from that
document.

---

## ~~ReplicationConverged compares CSN sets ACROSS databases — structurally ~always False on a multi-DB cluster~~ — DONE (2026-09-14)

Fixed per database, together with the two sibling instances in the same
functions (query errors that only decorated the message; the peer verdict
computed from whichever database answered). New `replicationState:
PartiallyVerified` for a peer whose evidence is incomplete. Unit red-first on
both seams plus a live e2e red; see `docs/reconcile-loop-fixes.md` (2026-09-14)
and the ADR-008 amendment of the same date. The original entry follows for its
evidence.

**What:** `checkLocalCSNConvergence` (`slapdcluster_controller.go`) appends every
(pod × database) contextCSN set into one flat list and `csnConverged` compares
them all pairwise. Two databases have different CSN sets *by construction*
(independent write histories), so on any cluster with two replicated
SlapdDatabases the condition compares db1's CSNs against db2's and reads
`CSNsDiverged` with a meaningless "lag" — the age difference between two
different databases' last writes. Single-site included; the two-database
`example` fixture triggers it everywhere.

**Evidence (2026-09-13, while root-causing the db2 cross-site breakage):** a
run's backup spec recorded `SourceConverged=False (ClusterDiverged: …
CSNsDiverged … local CSN divergence: 372.8s lag across 3 pods (2 query
errors))`; the live lab showed `8.8s lag across 3 pods (1 query errors)` while
db1 was genuinely converged on all pods. 372.8 s and 8.8 s are db1-newest minus
db2-newest, not replication lag. The same flattening feeds
`checkPeerCSNConvergence`'s `localNewestTime`, so peer lag can also mix
databases; and a failing database's queries are silently `continue`d, so the
peer verdict is computed from whichever database answers.

**Impact:** every backup on a multi-database cluster records
`SourceConverged=False` regardless of health (ADR-014 amendment 2026-09-12
consumes this condition), and the designed-but-deferred `requireConverged` gate
would wait forever. The condition is a shipped status field reporting a falsehood.

**How:** converge per database — group CSN sets by database, a database is
converged when its own sets match across pods, the condition is the AND with a
per-database message. Red-first unit on the grouping (two healthy DBs' distinct
sets must read converged; one DB diverged across pods must not). Class: one
structure answering two questions — a per-database verdict flattened into a
per-cluster list. Separate fix by decision (2026-09-13): it visibly changes
backup `SourceConverged` semantics and deserves its own red-first pass.

---

## ~~`slctl inspect`'s CSN check only ever looks at the FIRST data suffix~~ — DONE (2026-09-14)

Fixed per database, mirroring the operator-side fix above: `inspect` probes
`contextCSN` for every non-`cn=` naming context, the verdict moved into a pure
seam (`cmd/slctl/cmd/csncheck.go`) keyed by suffix, and the cluster verdict is
the worst per-database one, naming the database it indicts. `--json` carries
`pods[].contextCSNBySuffix` (replacing the flat `contextCSN` array, which only
ever held the first suffix's vector) and the per-pod display gets one section
per database. The `dataSuffixFromNamingContexts` helper — the trap itself — is
deleted, and `debug-dump`'s identical first-suffix-only dump is fixed with it.
Also reworded the misleading "never synced?" on a pod with no contextCSN: a
hidden glue suffix entry takes `contextCSN` with it (ADR-025) and this probe is
anonymous, so the check no longer asserts a cause it cannot know. Unit
red-first, live-verified on a two-database cluster. The original entry follows
for its evidence.


**What:** `inspect.go` picks one suffix via `dataSuffixFromNamingContexts`
(first non-`cn=` naming context) and reads `contextCSN` for that one only. So
the `csn-convergence` check — and the per-pod `contextCSN` shown in the
detailed output — covers db1 and is blind to every other database on the
cluster. The two-database fixture has had no slctl CSN coverage for db2 since
it landed (2026-08-25).

**Why it matters:** same class as the operator-side defect fixed 2026-09-14
(a per-database quantity handled on a single-value axis), and slctl is the tool
a human reaches for when the operator's condition says something is wrong —
it currently cannot confirm or deny it for any database but the first.

**Why deferred:** found while fixing the operator half; folding it in would
blur a controller fix with a CLI change in a different binary, and the CLI
wants a decision of its own (per-suffix sections in the human output, a
`contextCSN` map in `--json`, and what `--short` prints).

**How:** probe every non-`cn=` naming context, key the check by suffix, and
report per-database (pass/fail naming the suffix). The ADR-025 suffix-health
probes in the same function already iterate all naming contexts — follow that
shape.

---

## ~~The replication credential lives in two stores with no reconciliation between them~~ — DONE (2026-09-15, ADR-027)

**What:** `cn=replication,<suffix>`'s password exists as (a) a per-site k8s
Secret (`<dbname>-credentials`, create-only, ADR-008) and (b) a `userPassword`
on an entry INSIDE the replicated DIT. The DIT converges mesh-wide on its own
(CSN conflict resolution picks winners); the Secrets don't, and nothing detects
or repairs the skew: `ensureReplicationUser` is create-if-missing on the entry's
existence — it never verifies or converges the password, so a stale, foreign, or
even **passwordless** entry (observed live 2026-09-13: an ACL-stripped copy with
no `userPassword` at all — a state that locks every consumer out and satisfies
the existence check forever) is permanent until the DIT is wiped.

**Consequences, all currently by-design-unsupported rather than handled:**
rotation of `replication-password` breaks the mesh silently; re-running
`tests/e2e.sh setup` over retained DITs rotates the Secrets but not the entries
(same class — the supported reset is a full teardown including PVCs); and a
mesh that ever ran with divergent Secrets (the 2026-09-13 db2 breakage) cannot
self-heal because the repair channel is the broken channel.

**Why deferred:** a converge-the-password path is its own design pass —
multi-master needs a single-writer rule (every site rewriting the entry to its
own Secret is a churn storm; the passwords must already be uniform per
ADR-008), a bind-probe as the drift detector, and decided rotation semantics.
ADR-008 explicitly says create-only/never-rotates today; changing that is an
ADR amendment, not a bug fix.

**How, when picked up:** detect first (a per-database self-bind as
`cn=replication,<suffix>` from the operator; surface a condition on
SlapdDatabase when it fails — cheap, no writes, catches all of the above
loudly), and only then decide whether/where a repair write belongs.

### 2026-09-14: the detector now has a pattern to copy, and a rule to obey

Two things landed today that make the "detect first" half cheaper and better
defined than when this was written:

- **The pattern exists.** `internal/suffixprobe` is a shared per-pod LDAP probe
  consumed by the operator (`DataPresent`), the backup controller
  (`SourceSuffixHealthy`) and `slctl` — one classifier, three call sites, no
  duplicated predicate. A replication-credential probe is the same shape: bind
  as `cn=replication,<suffix>` per database per pod, surface a condition, write
  nothing. Copy that structure rather than inventing a fourth.
- **ADR-026 constrains the repair half.** R2: a destructive or corrective action
  on state the operator does not exclusively own may not be authorised by
  evidence it does not own. The `cn=replication` entry is *mesh-shared* state
  whose competing writers include every other site and any human with
  `ldapmodify`, so a repair write needs an explicit single-writer rule — which
  site, which pod, on what authority — before it can be safe. That is precisely
  the design pass this item defers, and R2 is now the frame for it: report and
  stall is the cheap correct default; repairing needs ownership.

---

### 2026-09-15: closed by removing the store, not by reconciling the two

ADR-027 moved the replication identity to `cn=repl-<db>,cn=slaptain-auth` in a
per-pod, never-replicated auth database on the data PVC, converged from the
Secret on every reconcile. That kills this item at its cause rather than
answering it: the duplication is inherent to simple bind, but the verifier copy
is no longer in the *replicated customer tree*, so it is no longer shared
multi-writer state — no single-writer rule is needed, no drift detector, and
rotation is supported (measured 52 s across four pods; worst case is the
SlapdDatabase resync floor of 5 minutes).

The "detect first" plan was therefore overtaken, not executed. The two live
findings that motivated this entry — the 19-day dead db2 link and the
password-stripped copy — are both impossible in the new store: it is node-local,
so nothing replicates a foreign or ACL-mangled copy into it, and the entry is
converged rather than create-if-missing.

**What remains, and it is not this item:** the legacy `cn=replication,<suffix>`
entry and its grants are deliberately retained for the staged-upgrade window
(ADR-027 migration steps 2+3). Dropping the grants is step 4, a separate
release; deleting the entry itself is a documented manual cleanup, because it is
a write into replicated state the operator does not own (ADR-026 R2). Two
window caveats are recorded in CLAUDE.md and ADR-027: the migration window is
one-directional (a NEW consumer against an OLD provider fails — pin
`externalPeers[].bindDN` meanwhile), and rotating the Secret mid-window strands
the create-only legacy entry.
That second caveat has a security half that is its own open item — see
"Rotating `replication-password` does not revoke the leaked credential".

---

## ~~R4 debt: syncprovCheckpoint and accesslogPurge are silently write-once; purge has no default~~ — DONE (2026-09-13)

Both now converge on every reconcile through a shared pure planner
(`planOverlayAttr`, generalising `planSessionlog`) plus a thin LDAP executor, on
both the syncprov and the accesslog overlay. `accesslogPurge` gained the R5
default `"7+00:00 1+00:00"` (7-day window, daily sweep) with `"none"` as the
explicit opt-out sentinel, consistent with `keepalive`.

Both attributes were put through the live-modifiability probe first — the
precondition ADR-024's amendments made mandatory for this class — and both
**passed**: replace and delete, each instantaneous, the pod still serving and
still accepting data writes afterwards, zero restarts. So no R2/R3 downgrade was
needed and ADR-024 needed no further amendment; the probe confirmed the class
rather than refuting it.

## ~~`DataPresent` does not cover read-only replica pods~~ — DONE (2026-09-14)

The probe now visits the `<name>-readonly-N` pods too, so an RO-only glue
(ADR-025 evidence item 5: a consumer initial-syncing from a glued provider
replicates the glue with the same entryUUID) is visible to the standing signal
and not only to `slctl inspect`. The deferred call — that including RO pods
widens the transient False window during a legitimate initial sync — was
answered by a distinct reason rather than by suppression:
`False/DataMissingOnReadOnlyPods` when every writable pod has the entry and a
read-only one does not, so an alert rule can hold it to a longer fuse than
`DataMissingOnPods` / `GlueSuffix`. Phase-neutral (the `readOnlyReadyReplicas`
precedent), observability-only (ADR-012), and byte-identical behaviour at
`readReplicas: 0`. See the ADR-025 amendment of the same date.

---

## ADR-005 step 4 is unimplemented for `cleanupPolicy: Retain`

**What:** ADR-005's teardown sequence says the operator removes the database's
syncrepl stanzas **in both cases** — `Delete` and `Retain`. The `Delete` path
now does the right thing by construction: the stanzas are attributes of the data
DB entry and die with it (verified 2026-09-14 while fixing the non-leaf delete).
The `Retain` path does not. The CR is finalized, the operator stops managing the
database, and the stanzas it previously wrote stay on every pod.

**Why nothing dangles today:** the stanzas name *this* database, and under
`Retain` this database stays. The retained configuration is self-consistent and
slapd keeps replicating it — which is arguably what `Retain` should mean. The
gap is between the ADR's wording and the code, not (yet) between the code and a
broken cluster.

**Where it could bite:** a retained database is orphaned configuration — nothing
converges it any more. If the cluster's replica count later changes, those
stanzas still name the old peer set, because no controller owns them; the
database then replicates against a topology that no longer exists. Whether that
is "as intended" for a deliberately abandoned database or a leak is a decision
nobody has made. ADR-005's own text implies the latter.

**Decide first, then implement.** Does `Retain` mean *"stop managing, leave it
exactly as it is"* — then amend ADR-005 step 4 to say so and this closes as a
doc fix — or *"leave the data, unwire the replication"* — then implement stanza
removal for `Retain` and the ADR stands as written? The second is the larger
job and needs care: removing stanzas is a `cn=config` write against a database
the operator has just disowned, so ADR-026 R2 applies — it must be authorised by
the operator's own record of what it wrote, never by inspecting what happens to
be there at teardown time.

**Found:** 2026-09-14, while fixing `cleanupPolicy: Delete` on replicated
databases; deliberately not folded into that change.

---

## e2e needs a liveness assertion class (found via the timeout= freeze)

The suite asserts end states through Eventually with generous budgets, so a
self-resolving full-server freeze passes green: the timeout= regression's three
~299 s silence gaps per convergence pass — etime≈300 sitting right there in the
logs — tripped nothing, and was found by a human noticing silence in a manual
run. Wanted: assertions that bound liveness during convergence — operator LDAP
op etimes under a threshold, and/or a probe writer asserting no
all-pods-silent gap longer than N seconds during setup/convergence. Cheap
first cut: parse etime from slapd stats logs in the tunables spec.

### 2026-09-15: the first instance landed, from the opposite direction

The restore specs now wait on **progress**, not on a deadline
(`tests/e2e/restore_wait_test.go`): any change in the restore machine's
observable state — cluster phase, `status.restore` sub-phase and message,
StatefulSet replicas, restore Job and Job-pod state, `restoreApplied` — resets
the clock; 5 min with nothing changing fails, 15 min while a restore Job pod is
*Running* (slapadd is silent, so that step is opaque) fails, and a 30 min
ceiling is the loud backstop. Terminal negative evidence (a Job at
`BackoffLimitExceeded`, `status.restore.phase=Failed`) fails immediately instead
of waiting out any budget. Pure policy in `restoreBudget.verdict`, unit-tested
red-first off the measured incident trace.

Same signal as this entry wants, opposite failure mode: this entry is about a
stall that currently passes green, that one about a slow-but-advancing machine
that failed red (a `E2E_SCALE=1 E2E_BACKUP=1` run, 2026-09-15: the in-place
spec's bootstrapFrom wait timed out at 480 s on a restore that completed
afterwards). Whoever picks up the convergence half should reuse the shape —
sample a fingerprint, treat change as liveness, keep the trail for the failure
message — rather than invent a second mechanism.

Numbers worth keeping, because they say where restore time actually goes:
of the failing 480 s, ~347 s was spent before the first restore Job existed, in
coarse operator ticks (107 s CR → pod, 60 s → Ready, 60 s → preflight, 120 s in
preflight). The same specs against an idle single-site operator: 20-25 s
end-to-end. And `slapadd -q` — the term the artifact size actually drives — is
~19 µs/entry (1202 entries 25 ms → 250 002 entries 4.8 s, measured in the
slapd-init image), i.e. sub-second for the 1200-entry E2E_SCALE fixture. So an
inflated fixture lengthens restores through the *operator's* per-tick workload,
not through the bulk load, and a per-entry timeout coefficient would have been
fitted to the wrong variable.

## PRIORITY RAISED: the big-DIT initial-sync e2e

Deferred twice; the tax keeps arriving. A >500-entry (now: multi-second
refresh-window) initial full sync exercises exactly the long refresh phases
where the timeout= freeze had its collision window, on top of its original
purpose (the sizelimit class). E2E_SCALE seeds entries into a LIVE mesh (small
deltas); this lane must instead create a FRESH consumer against a populated
provider — pod-recreate or bootstrapFrom against a big artifact. It is also the
lane that would calibrate the restore specs' `workingStall` budget, which is
currently ~19 µs/entry extrapolated rather than measured against a DIT big
enough to matter (see the liveness entry above). Next e2e
investment, before further replication-path changes.

**Second consumer, 2026-09-14 — an unmeasured number is now user-visible.**
`DataPresent` was extended to the read-only fleet (ADR-025 D5 amendment), so an
RO pod doing a legitimate initial sync makes the condition read
`False/DataMissingOnReadOnlyPods` until that sync completes. On fixture-sized
data that is seconds; **on a production-sized DIT nobody has measured it**, and
the answer decides whether the reason needs a documented "expected during RO
bring-up" caveat, an alerting-delay recommendation, or nothing at all. This lane
is what produces the number: stand up a populated provider, add an RO replica,
and time the window from the condition's own transitions. The same run answers
the older question — how long a fresh consumer's refresh phase actually is —
against the same fixture.

**Third consumer, 2026-09-15 — the number is now a budget in `tests/e2e.sh`.**
The post-suite `check_data_present_every_site` gate got reason-aware budgets:
60 s for every reason that is never legitimately transient after a completed
suite (`GlueSuffix`, `DataMissingOnPods`, `DataMissing`, `NoDataYet`,
`NoReachablePod`, an absent or unrecognised one), 600 s for
`DataMissingOnReadOnlyPods`, overridable as `E2E_DATAPRESENT_TIMEOUT` /
`E2E_DATAPRESENT_RO_TIMEOUT`. **The 600 s is a guess and is labelled as one in
the script**, for exactly the reason this entry exists: fixture-sized numbers
are all anybody has. Measured on a single-site lab on 2026-09-15, forcing the
shape by wiping the RO pod's data PVC behind an egress NetworkPolicy that
denied its syncrepl: the condition read `False/DataMissingOnReadOnlyPods`
within 5 s of the pod coming up empty, stayed False for as long as the block
stood (78 s, bounded only by when it was lifted — a broken replica does not
self-heal, so the fuse does end in a failure), and returned to
`True/RootEntryVisible` **18 s** after the block was removed. 600 s is ~33x
that, on a DIT of a few dozen entries. This lane replaces the guess with a
number; until then, raise the env var rather than trusting the default on a
fixture that has grown.

**Fourth consumer, 2026-09-28 — a probe threshold now depends on it.** ADR-030
gives the slapd container a `startupProbe` with a 300 s budget, sized to "far
beyond any fixture" because that is all the evidence there is: every e2e pod
opens a few-dozen-entry LMDB, and `E2E_SCALE` seeds into a LIVE mesh rather than
opening a big one cold. How long slapd takes to answer its first LDAP request on
a production-sized LMDB is unmeasured, and the consequence of guessing low is
sharper than for the other three consumers: under `OrderedReady` a pod-0 that
never reaches Ready means pod-1 is never created, so the cluster does not form
at all. Fixture-sized measurement for scale: 3-5 s from container start to Ready
across all twelve pods of the three-site lab, i.e. ~60-100x headroom on a DIT of
a few dozen entries. This lane produces the number that matters.

A second finding from the same session, worth more than the budget: the gate
now **pokes** each pending `SlapdDatabase` with an annotation every 30 s,
because `DataPresent` is phase-neutral observability and a healthy database
reconciles on the 5-minute `databaseResyncInterval` floor. Without that, a
`False` that had already healed could sit in status for up to five minutes with
nothing wrong anywhere — so any budget shorter than five minutes would have
been measuring condition staleness, not cluster state. Anything else that
gates on a `SlapdDatabase` condition from outside the operator inherits the
same problem.

---

## `readReplicas: 1 → 0` leaves the read-only fleet running

**What:** the three read-only reconcilers *skip* on `spec.readReplicas == 0`
instead of removing what an earlier non-zero value created —
`reconcileReadOnlyStatefulSet` (`operator/internal/controller/slapdcluster_controller.go:766`),
`reconcileReadOnlyService` (`:712`) and `reconcileReadOnlyHeadlessService` (`:671`)
each `return nil` immediately. Skipping is the right behaviour for a cluster that
never had RO replicas; it is the wrong behaviour for one that is being scaled back
to zero. The `<name>-readonly` StatefulSet keeps its old replica count, its pods
keep consuming via delta-syncrepl, and both RO Services keep resolving — until the
whole `SlapdCluster` is deleted and the owner reference finally reaps them.

**Observed 2026-09-20** on a single-site lab: `slapd` at `spec.replicas: 1`,
`spec.readReplicas: 0`, yet `slapd-readonly` reads `1/1` with `slapd-readonly-0`
Running and `config-slapd-readonly-0` / `data-slapd-readonly-0` Bound and in use.
Nothing in the CR says that pod should exist.

**Why it matters:** the declarative contract is broken in the direction users
least expect — an orphan replica that still holds a syncrepl consumer slot on
every RW pod, still answers on `<name>-readonly`, and still pins its PVCs (so its
node, ADR-018). It is also invisible from the CR: `status.readOnlyReplicas`
reports the spec's 0 while a pod serves. Somebody reducing an RO fleet as a cost
or blast-radius measure gets no warning that nothing happened.

**Not this entry:** the stale `{config,data,accesslog}-slapd-{1,2}` PVCs left by a
`replicas: 3 → 1` scale-down. `volumeClaimTemplates` PVCs are never garbage
collected by the StatefulSet controller — that is upstream Kubernetes behaviour
and matches ADR-005's Retain default. Deleting them is the user's call.

**How:** on `readReplicas == 0`, delete the RO StatefulSet and both RO Services
rather than returning early — pods first so their PVC deletion leases are released
(ADR-018), and idempotently, since the usual case is that none of them exist. Decide
explicitly what happens to the RO PVCs: the consistent answer is Retain (ADR-005),
which means the scale-to-zero leaves storage behind and should say so. Red-first
per Test Discipline: an e2e that deploys with `readReplicas: 1`, flips it to 0, and
asserts the StatefulSet, both Services and the pod are gone — it is red against
current code for the right reason today.

---

## The operator's control channel has no stated TLS posture

Every connection the operator makes to configure a pod is **plain `ldap://` on
port 1024**. Verified across all nine `cn=config` call sites —
`slapdcluster_tunables.go:119`, `slapddatabase_controller.go:552/1993/2099/2243/2905/3116`,
`slapddatabase_authdb.go:164`, `slapdschema_controller.go:182` — each a bare
`ldap.DialURL("ldap://"+addr)`, no StartTLS. Only the replication path (1025) and
the schemed readers in `csn.go` / `slapdbackup_controller.go` ever use `ldaps://`.

So the `<name>-config-password` Secret **authenticates** the operator; it does not
**encrypt** anything. What crosses the pod network in the clear, on every pod, on
every convergence pass:

- the `cn=admin,cn=config` password itself, in a SIMPLE bind;
- every `olcSyncRepl` value written by `applySyncreplToPod` — and a stanza carries
  `credentials=<replication password>` in plaintext by construction (ADR-027
  Consequences: "inherent to simple bind");
- whatever a seed carries, on the one pod-0 pass that applies it.

Nothing restricts who may reach that port, either: the only NetworkPolicy in the
repo is `charts/operator/templates/networkpolicy.yaml`, which gates **metrics
ingress to the operator** and says nothing about slapd. Port 1024 on every pod is
reachable by anything the CNI's default allows.

**This is not a bug report.** In-cluster control traffic over a trusted pod network
is what most database operators do, and it may well be the right call here. The
gap is that it was never *decided* in writing. The posture appears exactly once in
the repo, as an annotation inside a log walkthrough
(`docs/BOOTSTRAP.md:350`, "← plaintext LDAP, port 1024"), and nowhere as a
statement anyone could agree or disagree with. ADR-029 argues TLS should be on by
default for client traffic and does not mention the control channel at all; ADR-027
option C counts "no plaintext secret anywhere in `cn=config`" as a benefit mTLS
would buy, without noting that the write which puts it there is itself plaintext.
A reader auditing slaptain's TLS story today has to read Go to find this out.

**Wanted:** a stated posture, in `docs/TLS.md` and as an ADR if the answer is
anything other than "yes, deliberately, forever". The three candidate answers, with
their real costs:

1. **Keep it, and say so.** Cheapest and possibly correct. Then say what it assumes
   (a trusted pod network) and what would make it false (shared/multi-tenant
   clusters, CNIs without encryption), and pair it with a shipped NetworkPolicy for
   1024 so the assumption has something enforcing it.
2. **`ldaps://` on 1025.** slapd already listens there. The operator would have to
   verify a certificate whose SAN covers the pod's headless FQDN — which
   `tests/gencert.sh` mints but a user-supplied Secret need not — and it can only
   apply when `spec.ldap.tls.enabled`, so the plaintext posture survives for
   precisely the clusters that opted out of TLS. That conditionality is the design
   question, not the dialling.
3. **StartTLS on 1024.** Same certificate question, no second port, and a graceful
   downgrade path. Probably the smallest step that changes the answer.

Whichever wins, note that it does not remove the plaintext `credentials=` from
`cn=config` on disk — that is ADR-027 option C's territory, deferred separately —
and it does not touch the diagnostics leak recorded there (`slctl debug-dump`
collects stanzas verbatim).

---

## An operator upgrade rolls every slapd pod in every cluster it manages

When `spec.images` is unset — the documented, recommended shape — both
data-plane images resolve from the operator's own reference:
`defaultDataPlaneRepo(r.OperatorImage, "slapd"|"slapd-init")` for the repository
and `r.DefaultImageTag` (the `OPERATOR_IMAGE_TAG` env, injected by the chart
from `Chart.AppVersion`) for the tag. So upgrading the operator rewrites the
`slapd` and `slapd-init` image strings in **every pod template of every
StatefulSet of every SlapdCluster it manages**, read-only fleets included, and
the StatefulSet controller rolls all of them.

The *defaulting* is deliberate and good (CloudNativePG's pattern, described in
CLAUDE.md): operand images track the operator's version instead of drifting. The
**consequence** is stated nowhere. `helm upgrade slaptain` is, today, a rolling
restart of every directory in the cluster, and nothing in the docs says so.

**Found by accident, 2026-09-28:** it masked the first verification of the
`LDAP_REPLICAS` fix — scaling right after an operator redeploy rolled everything
and looked like the fix had failed. The giveaway was `slapd-readonly-0` rolling
too, which `LDAP_REPLICAS` could not explain because it was never set on RO
pods. Anyone testing pod-template churn must let the operator settle first; that
is now recorded in `docs/reconcile-loop-fixes.md`.

**Why this is heavier here than for a single-site database operator.** The pods
keep their PVCs, so there is no refresh storm (ADR-021 is not in play). What
*is* in play is ADR-016: every replaced pod invalidates the syncrepl addresses
each remote site holds for it, measured at 130-268 s of cross-site outage per
event. An operator upgrade on a three-site mesh therefore produces one
rediscovery window per pod per site — with **none** of the gating ADR-023
specifies for precisely this operation (proceed only while `minAvailable`
*current* copies survive; wait for `ReplicationConverged`; wait for peer
rediscovery to settle). The StatefulSet controller's own gate is pod Readiness,
and Readiness here means "the process exists" — see "The slapd container has no
`readinessProbe`" above, which makes this roll able to outrun convergence.

**Wanted:** a decision, then documentation to match it. Three candidates:

1. **State it and leave it.** Cheapest. Document that an operator upgrade rolls
   the fleet, and recommend pinning `spec.images` on mesh deployments so
   data-plane upgrades become a separate, deliberate act. Cost: pinned images
   drift from the operator, which is the property the defaulting exists to
   provide, and ADR-021's dual-image rules make "always pin both or neither" a
   trap someone will step in.
2. **Default at creation only.** Resolve the tag when the StatefulSet is first
   written and leave whatever is there afterwards. Keeps first-run convenience,
   removes the surprise — and removes the upgrade-tracking property entirely,
   silently, which is worse than either explicit choice.
3. **Sequence it.** Reuse the ADR-023 gates for an operator-initiated roll:
   ordinal at a time, gated on convergence and peer rediscovery. Correct and the
   most work; note the machinery already has to exist for ADR-023 anyway, so the
   marginal cost is the trigger and the bookkeeping, not the state machine.

Whichever wins, the demo and `docs/DEVELOPMENT.md` should stop implying that
`make operator-helm-install` is a control-plane-only operation.

---

## Hot migration e2e never wires the source back, so Stage 2 is untested

**What:** `tests/e2e-migration.sh` stands up `fakeprod` (peer mode,
`deltaSync: false` — a stand-in legacy plain-syncrepl provider) and `slaptain`
(consumer-only, `externalPeers` → fakeprod), and `tests/e2e/migration_test.go`
asserts seven things about it: the seeded entry arrives by plain syncrepl,
`entryUUID` matches the source, `DataPresent` holds on a never-seeded database
(ADR-025 D5), consumer-only rejects writes, `mode: peer` promotes **in place**
(pod UID and slapd container `StartedAt` unchanged), writes are accepted after
promotion, and `entryUUID`s survive the promotion.

`apply_fake_prod()` gives fakeprod **no `externalPeers`**. Nothing points back at
slaptain, which is exactly ADR-011 Stage 2's "source-side configuration changes:
add reciprocal syncrepl stanzas pointing at slaptain pods". So the fixture never
reaches Stage 2 — it reaches the slaptain half of it.

**Why it matters:** the post-promotion spec asserts the write is *accepted*
(`migration_test.go:215`); it never asserts it *arrives* at fakeprod, and no spec
writes on the source after promotion. Stage 2's verification clause — "lag stable
< 5 s in **both** directions" — has nothing behind it, and bidirectional
dual-running is the property that makes the migration hot rather than a cutover.
ADR-011's status line reads "e2e-verified via `tests/e2e-migration.sh`", which is
true of the interop and the stage transitions and overstates the rest; it wants a
one-line amendment either way.

Bidirectionality is covered *elsewhere* — `external_replication_test.go`
(`E2E_EXTERNAL_REPL=1`) exercises two-way delta-syncrepl between two slaptain
clusters. The untested combination is specifically **bidirectional against a
plain-syncrepl, non-slaptain provider**, i.e. the only topology hot migration
exists for.

**Three further gaps in the same fixture**, smaller and separable:

- **Foreign ServerID coexistence.** Stage 2 puts slaptain's serverIDs on the
  source's `olcServerID` list. That is the CSN-collision hazard ADR-011's
  "Foreign ServerID declaration" decision exists to prevent, and nothing
  exercises it.
- **Demotion.** ADR-010's in-place peer → consumer-only demotion is the
  documented rollback for Stage 2 (`docs/MIGRATION-HOT.md:137`). Only the
  promotion direction has a spec.
- **Single pod on both sides** (`replicas: 1`), so no per-pod ServerID
  coordination under a migration and no mesh member migrating.

Stages 3–5 (client cutover, source drain, source retirement) are runbook-only by
design — human and ops steps, not operator behaviour. They are not part of this
entry.

**Wanted, cheapest first:** after the promotion spec, patch `fakeprod`'s
SlapdDatabase/SlapdCluster to add an `externalPeers` entry pointing at
`slaptain-0` with `deltaSync: false`, then assert the post-promotion write
appears on `srcConn` — the spec already holds a connection to fakeprod for the
`entryUUID` comparison, so the machinery is there. Red-first is free here: the
assertion is red against the current fixture for the right reason. That converts
the largest untested claim into a spec without needing a real legacy slapd.

Foreign-serverID and demotion are separate pieces; the multi-pod variant depends
on the same "specs cannot provision their own topology" framework gap as the
`slapd-init` pinning entry above.

---

## `cross-site-csn` Unreachable is a hard `fail`, so a bootstrap blip red-exits `slctl inspect`

Carried out of GitHub issue #1 when it was closed (2026-10-06). Everything else
that issue raised has landed; this one item it filed as "owner unsure — needs a
decision", and the decision was never taken.

`slctl inspect` grades the `cross-site-csn` check as `warn` when a peer is
**Lagging** (data is flowing, just behind) and `fail` when it is **Unreachable**
(`operator/cmd/slctl/cmd/inspect.go:873`). `fail` sets the exit code, so any
pass with an Unreachable peer is red in CI and e2e.

The problem is that Unreachable has two populations behind it, and the check
cannot tell them apart:

- **Transient.** Measured on a fresh three-site pod-routed bring-up: every
  remote pod CSN query timed out for ~1 minute while cross-site routing
  converged, and `cross-site-csn` hard-failed — while replication itself was
  never broken (the local `contextCSN` already carried the remote serverIDs).
  It self-healed to 13 passed / 0 failed with no code or config change. The
  probe was wrong about the thing it was probing.
- **Persistent.** A genuinely dead cross-site link, which is exactly what the
  check exists to catch and should stay a hard fail.

**The open question is not "warn or fail"** — it is what distinguishes the two
without a timer that lies. Candidates, none costed:

- A bootstrap/grace window: suppress to `warn` while the cluster is young or
  while `status.externalPeerStatuses[].lastChecked` shows the peer has never yet
  been reached. Cheap, but "young" is a guess, the same class of admitted guess
  as ADR-030's startup budget.
- Grade on **duration**, not state: the peer status already carries
  `lastChecked`, and a never-reached peer is distinguishable from one that was
  Synced five minutes ago. This is the honest version and needs a status field
  that records when the peer was last *reachable*, which does not exist today.
- Leave `fail` and fix the caller: have e2e tolerate the window explicitly
  rather than teaching the diagnostic to be vague. Keeps `inspect` honest at the
  cost of pushing the judgement into every consumer.

Note that `PartiallyVerified` (ADR-008 amendment, 2026-09-14) already solved the
*adjacent* case — the peer answered but some database yielded no readable
contextCSN — and deliberately did not touch the nothing-answered-anywhere case,
which stays Unreachable. Whatever lands here should keep that split intact.

---

## `replication.enabled: true → false` half-tears-down a live cluster, silently

Nothing rejects the edit — there is no admission webhook — and the operator then
does half a teardown, because the two halves of the replication config are gated
differently:

| | on `enabled: false` |
|---|---|
| syncprov overlay, accesslog overlay | removed (`wantsSyncProv` includes `Replication.Enabled`) |
| accesslog DB `cn=accesslog-<db>` | **dropped — the change journal is destroyed** |
| `olcSyncRepl` stanzas | left in place (`slapddatabase_controller.go` step 10 is `if enabled { … }`, no else) |
| `olcMultiProvider` | left `TRUE` (only ever written on the enabled path) |

Every pod is left consuming from peers that no longer serve syncrepl — slapd's
*"got search entry without Sync State control"*. Replication stops by breaking
rather than by being switched off, and the journals do not survive it.

Found 2026-10-07 while answering a plain question: why would anyone ever set
`replication.enabled: false` — to stop replication for maintenance? The honest
answer is "at creation, to mean standalone". The field is a creation-time
declaration and a one-way on-ramp; `false → true` is designed, live, covered by
`scaleup_test.go`, and (since the ADR-013 amendment of the same date) rolls no
pod. `true → false` is not its inverse, and nothing says so at the point of use.

Note the *designed* demotion already works and has none of this problem:
`mode: consumer-only` keeps `enabled: true`, so `reconcileReplication` still runs
and **converges** the stanza set (in-cluster providers emptied,
`olcMultiProvider: FALSE`, data DB `olcReadOnly: TRUE`) instead of abandoning it.

### Open — needs a proper look

1. **Surface it.** A condition — `ReplicationConfigStale`, or a reason on an
   existing one — when `enabled` is false and pods still carry `olcSyncRepl`.
   The operator reads every pod's `cn=config` every pass, so the evidence is
   already in hand; this is reporting, not new machinery. Cheapest real
   mitigation.
2. **Decide whether a disable path should exist at all.** Deliberately not
   written on speculation. It would have to perform exactly the destructive
   short-set `Replace` on `olcSyncRepl` that ADR-031 decision 6 exists to
   prevent, against node-local `cn=config` the operator cannot prove it is the
   only writer of (ADR-026 R2). ADR-005 is the standing lesson on what teardown
   of replicated state costs when it is added quietly — it was wrong for a year
   and nobody noticed. If this is built, it is an ADR, not a patch.
3. **Consider whether the overlay teardown should be gated the same way as the
   stanza write.** The split is the actual defect: one half honours "a withhold
   must leave the pod's config alone" and the other half does not. Making the
   overlay/accesslog teardown as conservative as step 10 would turn a destructive
   half-state into a benign no-op, and is a much smaller change than (2). Worth
   costing before (2) is even considered.

Prior art to read first: ADR-031 decision 6 (short set = deletion), ADR-026 R2
(do not destroy shared `cn=config` state), ADR-019 amendment 2026-09-14 (the
withdrawn R8 migration that stranded an `olcAccessLogDB` and made a pod
permanently unbootable — the closest thing to this hazard that has actually
bitten).

Background written up on branch `fix/pod-template-roll-on-replication-enable`:
ADR-010 amendment of 2026-10-07 and the `spec.replication.enabled` field
description. This item is parked here on purpose so it is not gated on that
branch merging.

---

## A fresh SlapdDatabase reads `Error/NoPodsReachable` while its mkdir Job runs

For the first few seconds after a `SlapdDatabase` is created, its phase is
`Error` with reason `NoPodsReachable` and message *"Could not apply database to
any pod"*. It turns `Running` on a later pass without anyone doing anything.
Seen while walking `docs/demo/` (2026-10-10): `Error` at 0 s, `Running` at 15 s.

The mechanism, from the code: on the first pass the database's directories do
not exist yet, so `ensureDatabaseDirs` (`dirjob.go`) creates the mkdir Job and
returns `errDatabaseDirsPending`. The per-pod apply returns that error, so
the pod lands in `failedPods`, and with no pod in `appliedPods`
`databasePhase` (`databasephase.go`) ranks the pass as precedence 1, "no pod
could be reached at all".

The pods *were* reached: the operator dialled and bound before it asked for the
directories. The status points a human at the network when the right action is
to wait. Waiting on directories is pending work, which `databasePhase` already
has a rank for, below the error case.

Why nothing caught it: the e2e suite and the demo both wait for `Running`, and
a transient `Error` on the way there is invisible to a wait.

### Shape of a fix (not written)

- Carry "directories pending" out of the per-pod apply as a distinct outcome
  instead of a generic failure, and let `databasePhase` rank it as pending
  (`Pending` or `Degraded`, reason e.g. `DirectoriesPending`) rather than as
  `NoPodsReachable`.
- Red first: a `databasePhase` unit case where every pod is directory-pending,
  observed to return `Error/NoPodsReachable` before the change.
- e2e: a fresh database never reports `Error` on its way to `Running` (poll the
  phase from creation, not just the end state).
- Once fixed, the explanatory paragraph in `docs/demo/README.md` step 3 can
  shrink to a line or go.

## Adding or removing a mesh site rolls every pod at every other site

Found during the v0.4.0 validation, 2026-10-22, as the cause of two unrelated
e2e failures rather than as a question anyone asked.

`external_replication_test.go` removes a site from the `SlapdMesh`, asserts the
peer's syncrepl stanza disappears, and restores it. Both edits rolled the whole
fleet: the StatefulSet revision moved from `85c68597c9` to `58c8fdf8d9` and back,
and all three RW pods were replaced twice in 35 seconds.

**Why**: a peer's CA is mounted into the pod template
(`slapdcluster_controller.go:1196`, volume `peer-tls-<peer>`), and under
`meshRef` the peer list is **derived** from the mesh (`mesh_derive.go:425`). So
the set of volumes is a function of mesh membership, and changing membership is
a pod-template change. Nothing is wrong with any individual step; the roll is
the composition.

**Why it matters.** ADR-013's 2026-09-28 amendment put the sanctioned
pod-template roll-trigger count at **zero**, and names the triggers it retired.
This one is not in that list and was not decided — it was inherited when ADR-028
made the peer set derived. On a production mesh, adding a site restarts every
pod at every existing site, one ordinal at a time, for a change that alters no
data and no local configuration.

**Collateral, measured.** Two specs failed purely because they ran while the
rolls were in flight: `database_hotadd` snapshotted pod UIDs mid-roll (its
revision check passed — the template had already settled, the pods had not), and
`monitor_sidecar` scraped a pod that had not finished reopening its databases.
Neither is a defect in what it tests.

**Shape of a fix (not written).** The CA bundle could be a Secret the operator
maintains — one volume whose *contents* change with membership rather than one
volume per peer — which is the ADR-013 amendment's own pattern: the
database-dirs ConfigMap moved off the pod template for exactly this reason, and
`slapd` already re-reads `olcTLSCACertificateFile` on a converged modify. That
would make a membership change a cn=config convergence rather than a roll. Cost
unknown; the per-peer mount is also what makes `tls_cacert` legible per peer.

## ~~A pure slapd build reports the same version as a patched one~~ — DONE (2026-10-23)

Found 2026-10-22 by the ADR-040 cycle-B validation, before the alias inversion
that would have made it dangerous.

`SLAPTAIN_PATCHES=0` changes which patches are applied and nothing else, so the
package version comes from `debian/changelog` either way. Measured on the
running pure pod:

```
pure:     @(#) $OpenLDAP: slapd 2.7.1-0+slaptain3 ...
patched:  openldap (2.7.1-0+slaptain3) unstable; urgency=medium
```

Identical. Two things depend on them not being.

**ADR-040 decision 3** names `slapd -VV` as the *second independent route* to an
image's provenance, the one that corroborates the OCI labels. On the axis that
actually matters today — patched versus pure — there is no second route,
because both answers are the same string.

**And the shipped condition message is actively misleading.** When a stock
slapd refuses the three-value `olcAccessLogPurge`, the operator reports
`AccesslogPurgeConverged=False/PurgePhaseUnsupported` and tells the reader to
pin an image carrying the patch, *"2.7.1-0+slaptain2 or later — `slapd -VV`"*
(`accesslog_purge_converged.go:290`). A pure build satisfies that test while
lacking the patch entirely. An operator following the instruction verifies the
version, concludes the image is correct, and is left with no next step —
the condition and the check disagree, and the check is the one that looks
authoritative.

Today this is latent: the default build is patched, so nobody meets a pure
slapd unless they ask for one. ADR-040 decision 2 inverts that, and on the day
it does, the default image carries a version string asserting it is patched.

**Shape of a fix.** The `+slaptainN` revision should mean "carries slaptain
patches", which is what a reader already assumes it means. A pure build is then
plain `2.7.1-0`, or carries a revision that says so, and the build sets it from
the same `SLAPTAIN_PATCHES` argument that decides the patches — derived, not
written twice (ADR-040 decision 3's own rule). The condition message's version
test becomes true again. The OCI label is the belt; this is the braces, and it
is the one an operator reads first because the condition tells them to.

---

### Fixed 2026-10-23

**`N` is not derived, and the shape above overstated what could be.** `N` is a
hand-maintained packaging revision with a hand-written changelog entry saying
what moved, and it covers packaging work as well as patches — `+slaptain1`
carried no behaviour patches at all. Nothing can compute it, and a build that
tried would be inventing provenance. What is derived is which **token** it
wears, from the same `SLAPTAIN_PATCHES` argument that decides the patches:

| build | version |
|---|---|
| `SLAPTAIN_PATCHES=1` (default) | `2.7.1-0+slaptain3` |
| `SLAPTAIN_PATCHES=0` | `2.7.1-0+pure3` |

Both flavours share `N`, because both are that revision of this packaging fork.
The rewrite happens before `dpkg-buildpackage`, so the `.deb` filenames and the
compiled-in string move together, and it **refuses rather than guesses**: a
changelog version not ending in `+slaptainN` fails the pure build instead of
silently no-opping.

`pure`/`slaptain` are ADR-040's two `select.flavour` values, chosen so the
version string names the axis in the same words the API does. Plain `2.7.1-0`
was rejected: it reads as stock Debian packaging, which this is not (vendored
fork, `DFSG_NONFREE`, our symbols file, the `override_dh_strip` hardlinks).

**A symmetric agreement gate** runs after `dpkg-buildpackage` on the artifacts
just produced — each flavour asserting its own token and the absence of the
other's, so one of the two always runs. Two routes that fail differently: the
`.deb` control metadata, and the binary's compiled-in string.

**Measured, both arms:**

```
pure:     dpkg-query 2.7.1-0+pure3
          @(#) $OpenLDAP: slapd 2.7.1-0+pure3 (Oct 20 2026 10:00:00) $
patched:  version gate: OK (slaptain, package and binary agree on 2.7.1-0+slaptain3)
          logpurge phase gate: OK
```

The gate's logic was taken red and green against synthetic versions before
being believed: it passes the two legitimate combinations and fails all five
illegitimate ones, including `patches=0` with `+slaptain3` — the defect it
exists to catch, and what this entry describes.

**One trap, worth keeping.** The first version of the binary route grepped the
whole `slapd -VV` output for the other flavour's name. That would have failed
**every legitimate pure build**: `-VV` also prints the Maintainer line, and
that says `slaptain` on both flavours because the packaging is ours either way
(confirmed on the built pure image). The binary route now asserts *equality*
with the metadata version instead, and the Containerfile says why so nobody
retries it.

**No user-visible change.** The default build's version is byte-identical; only
the pure pair moves, and it is not in `all` or `push` yet. The consumer —
`AccesslogPurgeConverged=False/PurgePhaseUnsupported` — now names the token
rule rather than a version floor that the ADR-040 tag scheme is about to make
meaningless. Same wording in both chart `values.yaml`, the rotation example,
`docs/PURGE-ROTATION.md` and `docs/TUNING.md`.

**This unblocks ADR-040's `pure` default** (amendment prerequisite 2), which
was the only remaining blocker of the two.

## ~~Is the purge a NECESSARY step into the ITS#9398 fatal state, or only a sufficient one?~~ — ANSWERED (2026-10-23): only sufficient, and not even the usual route

Two measurements, taken by two lines of work three weeks apart, disagree. Both
are sound for what they tested; neither tested the other's claim.

**The podman repro says the purge is the trigger** (2026-10-04,
`docs/ITS-9398-REPRODUCTION.md`, "The slapcat test"). Read off disk, a purge
sweep advanced `minCSN` to a new write while on-disk `contextCSN` stayed where
it was — so the purge is not a checkpoint in disguise, it is what drives
`minCSN` *past* the durable `contextCSN`. It is step 2 of a four-step recipe
(a serverID new to the log DB since the last `contextCSN` persist; a purge
sweep persisting `minCSN` with it; an unclean stop; restart) that was then
executed deliberately and produced the fatal shape on demand, with a repair
that cleared it.

**The mesh run says the purge was not involved** (2026-10-19, recorded on the
purge-rotation branch). The storm reproduced twice on a three-site mesh — 2473
and 9237 stale-cookie lines — where the oldest journal entry on the affected
pod was the pod's own birth. With a 2 d/7 d retention on a one-hour-old
cluster **the purge had never deleted a single entry**, and the storm appeared
identically with the purge rotation on and with a plain fleet-wide interval.

**These are compatible only if a purge sweep that deletes nothing still writes
`minCSN`** — or if there is a second route into the fatal state that needs no
purge at all. Nobody has checked which.

**A third data point, 2026-10-22**, from the v0.4.0 release validation: the
storm appeared again on a three-site mesh (9358 lines on one pod), and on that
pod the journal's oldest surviving entry was `20261022071312Z` while the pod
itself started at `07:33:26Z` — the log reaches back *past the pod's own birth*,
on a cluster under an hour old with a 48h retention. Nothing had been deleted,
and no scheduled sweep could have run: the interval was 18h under the rotation
and 24h while it was refused. Whether a sweep *ran* without deleting is still
unmeasured — a log-level check for it was attempted and thrown out as unsound.

That makes three independent runs in which the storm appeared with the purge
having deleted nothing, which puts real weight on a sentence in
`docs/ITS-9398-REPRODUCTION.md`: *"The exposure is not continuous. It is tied
to purge sweeps… That moves the 'how do we avoid this' question away from
'checkpoint the log DB' and toward the purge interval."* If the exposure is not
tied to sweeps, that pivot loses its basis, and the mitigation question moves
back to the log DB — which is where the same document notes nobody configures a
checkpoint: not upstream's guide, not `slapo-accesslog(5)`, not Symas, and not
upstream's own regression fixtures.

**The configuration is confirmed susceptible by construction.** On the validated
fixture the syncprov overlay on both accesslog databases is bare — no
`olcSpCheckpoint`, no session log — while `syncprovCheckpoint: 500 15` lands on
the *data* databases, and `syncprovSessionlogSource` is unset. A log DB's
`contextCSN` is therefore written only on clean shutdown, which is the
precondition exactly. The two databases that stormed were those two bare log
DBs.

That is exactly the branch's own Open question 4, still unanswered: *"Does
`logpurge` reopen the window on a steady-state server? The purge path advances
`minCSN` on disk with a real `be_modify` on its own timer
(`accesslog.c:656-684`, `:760-787`), with no corresponding `contextCSN` write…
Read from source; not measured."* The first half is answerable from source
alone and cheaply — does the sweep reach the `be_modify` when its delete set is
empty?

**Why it matters now.** ADR-037 ships a feature whose entire subject is when
the purge runs. If the purge reopens the exposure window on every cycle, the
rotation's slot length is also a storm-exposure knob and the ADR does not say
so. If it does not, the four-step recipe needs a different step 2 and the
reproduction document overstates the purge's role.

**Also needs narrowing when the purge branch merges**: its `CLAUDE.md` line
says the 2026-10-19 run "established what the purge has to do with it:
**nothing**". That is stronger than the evidence — what was established is that
the storm does not require the purge to have *deleted* anything, and that it is
indifferent to rotation. Say that instead.

---

### Answered 2026-10-23, from source, and the two measurements never disagreed

**A sweep that deletes nothing writes nothing.** `log_old_lookup` is the
per-entry search callback; it may set `pd->mincsn_updated`, and it
*unconditionally* appends the DN and increments `pd->used` from the same
matching entry (`accesslog.c:679-691`). Both the `minCSN` `be_modify` and the
deletes sit inside `if ( pd.used )` (`:752`). So `mincsn_updated` can only be
set by a callback invocation that also incremented `pd.used`, and
`pd.used == 0` means the sweep does nothing at all. The hypothesis this entry
was built on is **false**.

**The second route exists and is in the same file.** `accesslog_response` has
its own `minCSN` writer, entirely outside the purge (`accesslog.c:2113-2159`;
2.6.10 `:1983`): any logged write whose CSN carries a serverID not already in
the `minCSN` set triggers an `LDAP_MOD_ADD` of `minCSN` against the log DB's
suffix entry. That is step 1 of the four-step recipe — "a serverID new to this
log DB" — persisting `minCSN` by itself, with no sweep. The path is present in
**both 2.6.10 and 2.7.1**, which matches the storm reproducing on both lines
and on a build carrying none of our patches.

So the two measurements were never in conflict. The podman repro used a sweep
because it had one; the mesh runs reached the same state by the ordinary write
path. The asymmetry worth keeping: `minCSN` is written *before* the deletes
(`:760-785`, deletes `:787`), so a sweep that matched entries and then failed
every delete still persisted `minCSN` — but it must have matched.

**Confirmed independently by the controls, which nobody had read this way.**
All three `docs/its9398/control-*.log` arms run `logpurge 07+00:00 01+00:00`
over a run of under a minute, so no sweep could fire. `control-sigterm.log`
shows the window wide open at step 1, before anything is stopped: on-disk
`contextCSN` **empty**, on-disk `minCSN` already carrying both serverIDs.
`control-checkpoint-100-5.log` shows disk `minCSN` `001 002` against disk
`contextCSN` `001`, then 1473/s after a `SIGKILL`, with the server naming the
culprit — `mincsn=…#002#…`, exactly the serverID present in one and absent
from the other.

**Consequences, all written up in `docs/ITS-9398-REPRODUCTION.md`:**

- the four-step recipe's step 2 no longer requires a sweep;
- §"Two consequences" is reversed — the exposure is continuous from the first
  write of a new serverID, not tied to sweeps — and the pivot it performed away
  from "checkpoint the log DB" has no basis. §"Controls" in the same document
  already said this and was never reconciled with the front of it;
- the mitigation question returns to the log DB's `syncprov-checkpoint`, which
  is where upstream's own `ITS#9580 FIXME` puts it (`accesslog.c:2076-2082`:
  *"Right now we depend on syncprov's checkpoint to clean up after."*);
- **ADR-037's purge-staggering half was never a lever on ITS#9398**, and the
  `CLAUDE.md` line above should be narrowed to say so rather than to claim the
  purge has nothing to do with it;
- `syncprov-checkpoint 1 1` wins by **ordering**, not by a narrower race: the
  log entry is added before `minCSN` is modified, so with `si_chkops = 1` the
  checkpoint fires on that operation (`syncprov.c:2607`, `:2615`) and persists
  `contextCSN` first. Read from source — neither control attempts a kill inside
  the window — and its cost on a write-heavy log DB is unmeasured. That is now
  open question 5 in the reproduction document, and the next thing worth
  measuring.

## `stale_on()` in the ITS#9398 controls can fail to produce a number

Found 2026-10-23 while reading the control logs properly.

`docs/its9398/controls.sh` ends each run by reporting a storm rate:

```
stale_on() { grep -ac 'err=4096' "$SRV/$1.slapd.log" 2>/dev/null; true; }
...
n0=$(stale_on A); sleep 10; n1=$(stale_on A)
echo "err=4096: $n0 -> $n1 => $(( (n1-n0)/10 ))/s"
```

In `control-sigterm.log` this produced a shell arithmetic error instead of a
rate — bash echoed a two-line expression back, so one of `n0`/`n1` held
something other than a single integer. The cause is **undetermined**; the same
code produced clean numbers in the other three arms, and guessing at it from
the log alone would be inventing a mechanism.

**Why it matters more than a shell bug.** That arm is the row in the
reproduction document's §"Controls" table asserting a clean `SIGTERM`. Its
verdict happens to be right on other evidence — the on-disk CSN state after the
clean stop, and a successful write replicating afterwards — but the number the
table implies does not exist. A missing reading and a zero reading print
identically here, which is the `CLAUDE.md` "reading that cannot be
distinguished from absence" class in the instrument we would cite upstream.

**Fix:** make `stale_on` emit exactly one integer unconditionally and guard the
arithmetic, so a broken reading fails loudly instead of silently becoming a
verdict. Re-run all four arms afterwards, because none of their storm numbers
has been produced by a checked instrument.

## ITS#9398 stale-cookie storm on OpenLDAP 2.7.1 (filed upstream in 2020, still open)

ADR-021 adopted 2.7.1 specifically because ITS#9580 is fixed in no 2.6.x
release. On 2026-10-03 the `dataloss_recovery` spec's no-storm assertion fired
on **2.7.1**, on a three-site mesh: 5991 `sync cookie is stale` lines on
`slapd-0`, 0 on the other two, against a limit of 50.

Mechanism, from the provider's own log:

```
syncprov_op_search: got a persistent search with a
  cookie=rid=001,sid=066,csn=…#065…;…#066…;20261003140300…#067#…;20261003135937…#0c9#…
syncprov_op_search: consumer not within recorded mincsn
  for DB's mincsn=20261003144533.051273Z#000000#065#000000
SEARCH RESULT tag=101 err=4096 … text=sync cookie is stale
```

The spec wipes `slapd-0`'s PVCs, so its accesslog refills from scratch and its
`mincsn` starts at the recovery moment. The consumers' cookies still carry SIDs
`067` and `0c9`, last written ~50 minutes earlier — **dormant SIDs**. Provider
answers stale, consumer full-refreshes, loop.

This is the dormancy shape ADR-008's 2026-09-11 amendment describes and that
ADR-021 expected 2.7 to resolve. Two facts bound it: the identical spec
**passed** on the same code and images in an earlier run the same day, and the
failing run was a *second* destructive pass over a mesh that had been standing
for ~50 minutes — outside the one-pass-per-setup envelope the suite assumes.

So the open question is not whether slaptain is wrong but whether ADR-021's
premise is complete: is ITS#9580 fixed in 2.7.1 for the dormant-SID case, or
only for the case the upstream ticket describes? Found during ADR-031 Phase 8
validation and deliberately **not** acted on there — weakening an ADR-021 guard
on a single observation would be exactly the wrong move. It belongs to ADR-021
and ADR-008.

**Second observation, 2026-10-03, and it refutes the bounding above.** It fired
again on 2.7.1 on a **fresh** mesh — first run after setup, not a second
destructive pass, no accumulated dormancy. 3911 lines, and still producing
~20/s **25 minutes** after the event, connection ids past 324 000: a persistent
hot loop, not a settling transient. Provider-side on the recovered pod,
answering remote consumers binding as `cn=repl-<db>,cn=slaptain-auth` from the
other two sites.

So the "outside the suite's one-pass envelope" explanation does not survive. The
open question sharpens to: **is the 2.7.1 fix for ITS#9580 simply absent for the
PVC-loss-on-a-mesh case?** The spec's own text says 2.7.1 "should be ~0
everywhere", and on this evidence it is not.

**ANSWERED, and the question was built on a wrong attribution.** There is no
2.7.1 fix to be absent: this failure is ITS#9398, not ITS#9580, and it is fixed
in no release of either line. 2.7 was never going to make it "~0 everywhere".
What addresses it is the accesslog `syncprov-checkpoint` that 0.5.0 turns on by
default, on both version lines.

Ruled out as a cause, with evidence: the ADR-031 RID ledger. The consumers' RIDs
for the recovered site never changed — sites 2 and 3 both held
`external/site-1/0 = 1` from start to finish — and the renumbering happened only
in the recovered site's *outbound* direction, which is not the side issuing
these cookies.

**Root cause found, and it is upstream's ITS#9398 — see
`docs/ITS-9398-REPRODUCTION.md`.** Filed by a maintainer in 2020, still
UNCONFIRMED, its intended fix blocked behind the suspended ITS#9225. We spent
weeks citing ITS#9580, which is a different ticket; the FIXME comments we were
reading sit in code *that* ticket added. Upstream documents nowhere that the log
DB needs a `syncprov-checkpoint`, and its own regression fixtures omit it.
Impact is resource exhaustion, not data loss: measured on a live mesh, entries
still reach all nine pods because the consumer's fallback full refresh succeeds
— it is the delta optimisation that is gone, at ~1400 full-DIT refreshes per
second per consumer. It is
not a dormancy artefact, not data loss, and not ours. On a delta-syncrepl log
DB, `minCSN` is written to disk eagerly while `contextCSN` is written **only by
a syncprov checkpoint** — and no delta-syncrepl guide tells you to configure one
on the log DB, so slaptain does not. The on-disk invariant is therefore violated
from the first write onward on a perfectly healthy node, invisibly, and **any
unclean stop makes it permanent**: `write, write, SIGKILL, start` reproduces it
on two servers with no fault injection. `SIGTERM` is safe, because
`syncprov_db_close` checkpoints — which is why routine restarts never show it
and why it looked random.

**For slaptain, three options, none yet taken** (see also the heartbeat note
below):

1. `syncprov-checkpoint 1 1` on every accesslog DB. Proven to close it, but
   `100 5` was proven *not* to — a batched checkpoint is a lottery — so the
   sound setting costs a `contextCSN` write per operation on a write-heavy
   database.
2. Detect and cure: the operator can read the invariant per log DB per pod, and
   the cure is one write from the server whose serverID is missing. Cheap to
   detect; the cure needs that *specific* node to write, which the operator
   cannot arrange cross-site (ADR-026 R2 forbids the fan-out).
3. **Make every serverID write periodically.** That is ADR-008's deferred
   heartbeat, and its deferral rested on ITS#9580 being fixed in 2.7 — a premise
   this work refutes. The case should be reopened on the new evidence.

**Earlier characterisation, superseded in its causal account but not in its
observations:** The
invariant is now known and is nothing to do with data loss, dormancy or
Kubernetes: *on a delta-syncrepl provider, the log DB's `minCSN` names a
serverID its `contextCSN` does not contain*. Every consumer is then told the
cookie is stale on every connection, forever, and retries with no backoff.
Proven by removal: three sites unnecessary, losing `/data` or `/config`
unnecessary, dormancy unnecessary. Proven sufficient: two LDAP adds that
restored the missing SIDs ended a 22-minute storm within 90 s. Mechanism traced
to `syncprov.c:3320` (trim the cookie against `contextCSN`) versus `:3480`
(validate it against `minCSN`), with the two sets permitted to differ — and all
line references verified against the 2.7.1 tree.

**Still unproven:** that it predates ADR-031. Nobody has run the PVC-loss
scenario on a multi-site mesh under the pre-ADR-031 scheme, so "pre-existing" is
the reasonable reading rather than a demonstrated one. **A baseline run on
`v0.2.2` would settle it in one pass** and is the cheapest next step.

**Then:** reproduce deliberately and control the dormancy variable, rather than
taking it as a side effect of whichever spec happens to wipe a PVC.

## The pod-name label is a constant in one place and a literal in two

`statefulset.kubernetes.io/pod-name` is now `labelStatefulSetPodName` in
`operator/internal/controller/writeservice.go`, and still an inline literal in
`backup_job.go` (the backup Job's pod affinity) and `dirjob.go` (the mkdir
Job's). Three spellings of one key, in code whose failure modes are all
silent: a drifted constant gives the write Service a selector matching no pod
— a total write outage — while the two Jobs keep working against the literal
and nothing reports a disagreement.

`goconst` does not force the fold: it triggers at three occurrences and the
literal appears twice.

Found while mutation-testing ADR-037's write Service. The mutation that
revealed it is worth repeating anywhere a label key is asserted: misspell the
constant and watch the suite. A table that asserts against the same constant
the code builds from stays green through it, because both sides moved
together — the key has to be spelled out literally on the test side at least
once. `writeservice_test.go` does that now; nothing else does.

### Shape of a fix (not written)

- Fold the two literals onto `labelStatefulSetPodName`, and keep exactly one
  test asserting the literal spelling.
- Red first: misspell the constant, confirm the build is clean, and confirm
  that test is the one that goes red.

## The suite has never swept a cluster that was upgraded into rotation

Every mesh cycle so far bootstrapped its clusters with the purge schedule
already declared — rotation on (form B) or a fleet-wide interval (form A). The
shape a real deployment takes is the other one: a cluster that has been running
without any of this, which then has rotation enabled on it.

The mechanism that differs is which code path writes `olcAccessLogPurge`. An
overlay created *with* a phase takes the ADD path; an overlay that already
exists takes the convergence path. **Both are covered**, including on a mesh —
a database created on an already-rotating three-site mesh was born with a
correct three-field value, and the `write-rotation` specs turn `spec.rotation`
on and off on a *running* mesh cluster and assert the result.

So what is missing is narrow: the **full suite**, swept end to end, against a
cluster created without rotation and then upgraded into it. The question it
answers is not "does the purge value converge" — that is answered — but
"is anything *else* in the suite sensitive to having been upgraded into
rotation rather than born with it".

Judged thin, and recorded rather than run: nothing else in the suite reads
`olcAccessLogPurge`, and the paths that write it are covered. But it is not
zero, and it is the shape the real upgrade takes — which makes it a
pre-release check rather than a pre-merge one.

### Shape of a check (not written)

- Bring a three-site mesh up with **no** purge schedule declared at all.
- Let it settle; confirm the two-field operator default is in force.
- Enable rotation through the chart, identically at every site.
- Run the full suite, and compare its result to the fresh-with-rotation cycle
  already in the record rather than reading it in isolation.
- Watch the same things the fresh cycles watched: the delta-sync lost-sync
  grep, `accesslog-mincsn` per site, `DataPresent`, and the invariant that no
  replicated database is ever left without a purge value.

## The rotation-aware read Service — deferred a third time, now on topology rather than on numbers

ADR-037's second amendment designs it (`adr-037`, "The reads") and
`docs/PURGE-ROTATION-PLAN.md` put it out of scope deliberately: reads go to the
read-only replicas **plus the current writer**. Nothing in the rotation work
built it, and reads still go to `<cluster>` (the whole read-write fleet) and
`<cluster>-readonly`, with no Service spanning both.

**Deferred again on 2026-10-25, and this time for a different reason.** The two
numbers the earlier entry waited on are no longer what blocks it; a topology
decision does.

### What changed: the contradiction is resolved

The earlier entry said to resolve this before sizing anything — slapload's
record says *"a purge stall blocks reads, not just writes"*, ADR-037 says reads
flow at ~75 % while writes block. Both measured.

They are **not in conflict**, and ADR-037 had already recorded the hole they
fall into (`adr-037`, Context): *"the controlled runs show writes stalling while
reads continue, the field reports full unresponsiveness. That discrepancy is
unresolved and it matters."* slapload is the third data point and the first
instrumented one, on the field side. Its record (`docs/FINDINGS.md`, and the
rules in its `CLAUDE.md`) adds the mechanism: a purge does not make reads
**fail**, it makes them **wait**, so the signal is dropped iterations, not
errors.

Mean capacity and tail behaviour are different quantities, and both readings
can be true at once: a pod can average ~75 % of its read throughput across a
purge window while containing multi-second intervals in which it serves
nothing.

### An arithmetic check nobody has closed

ADR-037 states *"Reads keep flowing at ~75 %"* bare in Context, with no run
cited and no scope declared. 75 % is exactly 3/4, which is also what a four-pod
fleet reports when one pod serves nothing — so the figure cannot be told apart
from a fleet aggregate by reading the ADR.

slapload's provider-vs-consumer pair is an independent route at it. Three
read-write providers served 220k ops/s; three read-only consumers served 224k —
98.2 %. For a three-pod fleet with staggered purges, expected fleet capacity is
`1 − (n·P/T)·(1−f)/n` where `f` is the purging pod's retained fraction. At
`t = 600 s`, `P ≈ 140 s`, `T = n·t`: `f = 0` predicts 92.2 % and `f = 0.75`
predicts 98.1 %.

If slapload's slot length and purge duration in that ramp were near those
values, the ~75 % is a **per-pod** property measured twice by instruments that
fail differently, and the two records never disagreed. **This needs that run's
`t` and `P` to close, and until it does neither side should quote 75 % as the
purging pod's read capacity.** All figures here come from one platform and are
not transferable; slapload's are its own.

### Why it is deferred anyway

The feature harvests read capacity from providers while dodging the purge. The
intended topology does not want it:

- **`n` stays small.** N-way multi-master grids want to be small, and the
  deployment shape is two read-write pods per site across three sites. Read
  scale-out is by **consumers**, not by adding providers.
- **Providers scale only for write capacity** — to satisfy `P < (n−1)·t` during
  bulk provisioning. Steady-state write rates are low; bulk windows run against
  an agreed write-rate ceiling, so a wider provider grid is an exception, not a
  growth path.

At `n = 2` the read-specific mathematics collapses onto the write mathematics:

- the band where writes are healthy and reads are structurally squeezed,
  `t < P < (n−1)·t`, is **empty**, because `(n−1)·t = t`;
- the recovery asymmetry scales as `n−1`, so it is a factor of one. Writes get
  `(n−1)·t − P = t − P`; reads get `t − d − P`. They differ only by the lead.

And the capacity it would harvest is **anti-correlated with write load**: `P`
grows with the write rate, so during the bulk windows that are the only reason
to widen the provider grid, the purge is longest and the one non-purging
provider is also the write target. A read path resting on that capacity loses
it exactly when it is needed.

Under "reads go to consumers", purge exposure on the read path is already zero
and `<cluster>-readonly` **is** the read Service. There is nothing left for the
hybrid to buy at this topology.

### Reopen on any of

- **Three or more read-write pods per site.** The empty band opens, the
  asymmetry becomes a factor of `n−1`, and the question is live again.
- **`readReplicas: 0` becoming a shape worth supporting for reads.** Then
  providers carry reads by necessity and exposure is `P/T` per pod with no
  alternative.
- **Consumers failing to carry the read load** for a reason other than their
  own thread pool — which is its own entry below and must be ruled out first,
  since per-pod read capacity is currently a config default rather than a
  hardware property.

### If it is ever built

It is **one parameter, not two designs.** Exclude the purging pod for `m` slots:

- `m >= ceil((d+P)/t)` is forced — a pod still purging cannot be re-admitted;
- `m = 1` is "every pod that is not purging", maximum read capacity;
- `m = n−1` is ADR-037's stated default, RO replicas plus the current writer.

Two further notes for whoever picks it up. A Service selector is label
*equality*, so "not pod-k" is not expressible — it needs an operator-written
pod label driven by the grid (a write to Pod objects, not to the pod template,
so it rolls nothing; whether the StatefulSet controller reconciles such a label
away is unverified). And the read lead wants to sit **just above** the client
connection TTL rather than comfortably above it, the opposite of the write
lead, because every second of lead comes out of the trailing margin
`t − d − P`, which is what absorbs the just-purged pod's replication catch-up.

### Also parked with it

- `slotsFromWriteToPurge: 2`, the cooldown slot — only meaningful at `n >= 3`,
  and every measured arm so far has been 2+2. Unknown rather than guessed.
- Upstreaming the `logpurge` phase patch. It is ours and local; the packaging
  fork carries it.
- **Documentation, which survives the deferral.** `docs/PURGE-ROTATION.md`
  tells a client *"Reads stay on `<cluster>` (or `<cluster>-readonly`)"* — two
  names and no guidance. Under the topology above the answer is
  `<cluster>-readonly` whenever `readReplicas > 0`, and a load generator that
  defaulted to the read-write Service read from providers while consumers sat
  idle for a full day of runs before a dashboard showed it. Say which name, and
  say why.

## A context the lab file does not describe can collide with a real site

`tests/e2e.sh`'s `assign_site_names` gives an undescribed context `site-<idx>`
by position. With a lab file present, `config <described-ctx> <undescribed-ctx>`
therefore names the second one `site-2` at index 1 — which collides with the
lab's own `site-2` if it has one. Two sites, one name, one serverID slot.

Pre-existing, found while fixing the adjacent topology disagreement, and **now
at least visible** in the `config` dump rather than silent — which is why it is
recorded here rather than fixed in passing.

### Shape of a fix (not written)

- Refuse a context the lab file does not describe when a lab file is in use, or
  assign it a name provably outside the lab's own set.
- Red first: `config` over one described and one undescribed context, asserted
  to produce two distinct site names and two distinct `serverIDIndex` values.

## The 2.6 build's version string no longer discriminates

`images/openldap-deb/Containerfile.ol26src` stamps the package with
`dch --local '+slaptainphase'`, a fixed suffix. Adding
`accesslog-purge-logging.patch` to `local-patches/` therefore produced a
binary that reports `2.6.10+dfsg-1+slaptainphase1` — **identical to the build
without it**. `slapd -VV` can no longer tell the two apart, and neither can
anything else: the `-ol26src` images carry no label naming their patch set.

Found when a measurement session rolled a cluster onto the new build and had
no way to confirm from the running pod which patches it carried. The 2.7 path
does not have this problem: it bumps a vendored Debian revision
(`2.7.1-0+slaptain2` → `+slaptain3`) per patch-set change.

This matters more than a cosmetic version string, because the two builds differ
in what they can be asked to do — one emits purge timings and one is silent —
and `docs/OPENLDAP-VERSIONS.md` already warns that `-ol26` and `-ol26src`
differ by three characters with a quiet failure mode.

### Shape of a fix (not written)

- Derive the `dch --local` suffix from the patch set rather than hard-coding it
  — a short hash of the sorted `local-patches/*.patch` contents would do, and
  changes exactly when the patches do.
- Or stamp an OCI label naming the applied patches, which the 2.7 images also
  lack.
- Red first: build twice with a patch added in between, assert the reported
  version differs.

## `olcThreads` has no CRD field, and the condition for adding one is now met

`spec.tuning.toolThreads` is `olcToolThreads`, which only slapadd and slapindex
read. The **runtime worker pool**, `olcThreads`, is neither set nor exposed, so
the single directive that sets per-pod throughput cannot be expressed through
the operator's API. Changing it means hand-editing `cn=config`, outside the
operator, subject to whatever it reconciles and whatever the init container
converges at pod start.

That omission was deliberate and conditional. `slapdcluster_types.go` (above
`SlapdTuningConfig`), `docs/TUNING.md` and this file all say the same thing:

> upstream's defaults are defensible, the reference production platform leaves
> them alone too, and a thread count we cannot measure is a knob we would be
> guessing at. They stay recorded in `docs/BACKLOG.md` as escape hatches to add
> **when a measurement asks for one**.

**A measurement now asks for one.** Measured 2026-10-25 against a 3 read-write
+ 3 read-only cluster (one platform, figures not transferable):

| read targets | total | per pod |
|---|---|---|
| 4 pods (2 rw + 2 ro) | 276k ops/s | 69k |
| 3 pods (the providers) | 220k ops/s | 73k |
| 3 pods (the consumers) | 224k ops/s | 75k |

One ceiling per pod across three cluster shapes, with
`openldap_threads_active` at 16 of 16 on every pod, `openldap_threads_pending`
climbing to 16,577, and ~7.8 container cores used on nodes with far more. Not
CPU-bound, not I/O-bound, not write-contended — out of worker threads at
OpenLDAP's default `olcThreads 16`, which is independent of the machine the pod
lands on.

The providers-vs-consumers row is the one that rules out the alternative
explanation: moving every read off the providers onto dedicated consumers
removes all write, replication-replay and accesslog work from the read path and
changed throughput by 2 %.

**What it costs to not have it.** "How do we add read capacity" has exactly one
answer today: more consumers — a PVC, a pod, and a full syncrepl refresh
measured in hours for a large DIT. With `olcThreads` reachable it has two, and
the second is a restart. On large nodes the second is often right.

It also distorts how the read path is reasoned about: per-pod read capacity
looks like a hardware property and is not. Spreading reads over more pods
multiplies a default while the nodes idle.

### Shape of a fix (not written)

- **Determine the placement class first, empirically, before any field
  exists.** This family has already produced two directives that hang slapd
  outright on a live `ldapmodify` — `olcIdleTimeout` and `olcWriteTimeout`,
  which is why they have no field either. Test `olcThreads` against a live pod,
  then pick ADR-024 R1 (converged), R2 (bootstrap-time) or ADR-034
  (startup-only, converged offline at pod start). Offering a field the operator
  cannot honour is what ADR-024 R4 forbids.
- Then `spec.tuning.threads`, with **no default and no recommended value** —
  the `leadTime` precedent. Unset means slapd's 16.
- **Do not quote slapload's old `olcThreads` table** (16 → 16,350 ops/s, 64 →
  41,362, 128 → ~37,900). Those were taken before its uid index existed, when
  every search was a full scan and 16 threads pinned 16.0 cores. Indexed, the
  same 16 threads occupy 7.8 cores and wait on something other than CPU. The
  mechanism transfers; the values do not, and the old optimum was found in a
  regime that no longer exists.
- Red first: a unit assertion that the rendered `cn=config` carries the
  requested value, and a live check that a pod which started with it serves
  normally.

## No thread-saturation rule, and the refutation named the wrong metric

`monitoring/prometheus-rules.yaml` records, correctly:

> No thread-saturation rule. `cn=Pending,cn=Threads > 0` sustained was the
> proposed signal, and live it read Pending > 0 with only 2-9 of 16 threads
> active: Pending counts queued operations, not exhausted workers, so it cannot
> say "every thread is busy".

That refutes `Pending > 0`. It does not refute `Active == Max`, which is the
saturation signal — and `docs/MONITORING.md` **already says so in prose**:
*"Saturation is a queue that grows while Active sits at Max."* We export both
`openldap_threads_active` and `openldap_threads_max`. We have the metrics, we
have the correct formulation written down, and there is no rule.

The 2026-10-25 run is exactly that shape: 16 of 16 active with pending climbing
to 16,577, sustained, on a pod that was neither CPU- nor I/O-bound.

This is ADR-039's own rule — *a refutation names what it tested* — biting for
the third time, after ADR-037's seventeenth amendment and the ~75 % scoping
question above. The pattern each time: a measurement refutes one formulation
and gets filed as refuting the phenomenon.

### Shape of a fix (not written)

- Rule on `openldap_threads_active / openldap_threads_max` at 1 sustained,
  with `openldap_threads_pending` rising, over a window longer than a purge.
- Red first: a promtool test with a series at Active == Max, and a second at
  Pending > 0 with Active well below Max that must **not** fire — the case the
  original note correctly rejected.

## `openldap_waiters{direction="read"}` may be `connections_current` renamed

Reported 2026-10-25: `direction="read"` tracks `openldap_connections_current`
sample for sample, because every idle open connection sits in the event loop
waiting to be read. `direction="write"` stayed at 0 through a sweep peaking at
278k ops/s.

If that holds, we export one quantity twice, which ADR-033 forbids ("No
quantity is exported twice; the one sanctioned overlap is `up`"), under a name
that reads as contention. `docs/MONITORING.md` documents the family and the
dashboard graphs it in the collapsed "Threads and waiters" panel, where a
reader will take it for queue depth.

### Shape of a fix (not written)

- Confirm by our own route first: graph
  `openldap_waiters{direction="read"} - openldap_connections_current` over a
  run. Flat zero settles it; this is a reading, so it wants a second instrument
  rather than a mutation.
- If confirmed, **keep the metric** — it is a faithful reading of
  `cn=Read,cn=Waiters` — and fix what claims it means: say in
  `docs/MONITORING.md` that read waiters are open connections, and drop it from
  the panel that implies otherwise. Only `direction="write"` carries contention.

## A purge is invisible to every instrument slaptain currently has

A purge stall does not make operations fail, it makes them wait. Three columns,
and only one of them sees it (slapload's record, 2026-10-25):

| instrument | during a purge stall | why |
|---|---|---|
| error rate | ~0 | a stalled operation is not a failed one |
| p99 latency | flat, or **down** | stalled operations never complete, so the percentile is computed from the fast ones that did |
| dropped iterations | **the signal** | requests the client could not issue, because every worker is parked |

The honest reading of a zero error rate during a purge is "no reads failed",
never "reads were unaffected".

**Our own side is worse, because we have no client at all.** The exporter
collects on scrape with a 5 s socket deadline and, per ADR-033, unreadable
evidence is **absent, never 0**. So a stalling pod reports nothing rather than
something bad. The only rule watching that is `SlaptainSlapdExporterDown` at
`for: 5m`; a purge of order 140 s produces a silent gap with no alert,
indistinguishable from a scrape blip. The PodMonitor default interval is 30 s,
which is about four samples across such a purge — enough to establish presence,
not shape, and useless against the few-second stalls inside it.

There is an inversion worth naming: ADR-033's "absent, never 0" exists to stop
absence masquerading as a healthy zero. Here absence **is** the signal, and the
principle converts it back into "unknown", which on a dashboard reads as fine.

**And two different phenomena look alike.** A 25k/s dropped-iteration figure
seen the same day turned out to be a ramp knee at ~275k ops/s, not a purge:
drops rising monotonically with offered rate, no periodicity, errors ~0. A
purge shows as **periodic bursts at a fixed offered rate, on the purge cycle**.
Any future claim resting on drop rates has to carry that discriminator.

### Shape of a fix (not written)

- **The operator exports a per-pod "purging now" gauge.** It already computes
  the epoch-anchored grid for the write endpoint, so this is ADR-033 Surface 2
  by the book — the operator observes it directly rather than relaying it, and
  it needs no new reading from slapd. It turns an invisible event into a
  labelled window, after which any metric, ours or a load generator's, can be
  split in-purge versus out-of-purge.
- Without it, confirming anything about purge impact means correlating by wall
  clock against a grid reconstructed by hand.
- Red first: a unit assertion that the gauge is 1 exactly on `[k·t, k·t+P)` for
  pod `k` and 0 elsewhere, against the same pure grid function the write
  endpoint uses.

## OpenLDAP 2.7.1 segfaults reliably under load testing — ITS#10597, almost certainly, and shipped fixed in 2.7.2

**Update 2026-10-29.** This has a name, and the default moved to 2.7.2, which
carries its fix. The identification is strong but **not confirmed**: the arms
have not been re-run on 2.7.2 and no stack was ever captured.

ITS#10597, "slapo-accesslog handling of per-op info state": the overlay freed a
shared `entryUUID` from a path **non-write** operations reach via `goto skip`
before taking `li_op_rmutex`, while writes take it properly. Concurrent
searches against a provider carrying an accesslog double-free it. The reporter
measured 10 SIGSEGVs in 30 minutes unpatched and 0 after. Present in 2.7.0,
2.7.1, 2.6.14 and 2.6.15; fixed in 2.7.2 and 2.6.16.

Everything matches: the right versions, the accesslog overlay present, a
read-dominated workload (0.3 % writes), a reliable crash under sustained load.

**And we had already written it down.** ADR-037's option-A section says purge
pacing is *"blocked on upstream: measuring it needs read-heavy load against
2.7.x, which frees a shared entryUUID from an unlocked path (ITS#10597)"*. The
explanation was in this repository while this entry called the crashes
unexplained — two records, one fact, no link between them.

**Three consequences:**

1. **The bearing on ADR-040 decision 8 is gone.** The amendment held 2.7 as
   the default on the argument that ITS#10597 was *"temporary and
   self-resolving (2.7.2, inside the same format line, no migration)"*. 2.7.2
   released 2026-10-21, five days before v0.5.0. The trade was called
   correctly and this entry no longer weighs against it.
2. **ADR-037 option A is unblocked.** The pacing measurement was parked
   *"until a release carries the fix"*. It does now, on a stock build rather
   than a custom one nobody deploys.
3. **The 2.6 scale-down SIGSEGV below is NOT this bug** — that image is 2.6.10,
   which predates ITS#10597's introduction in 2.6.14.

**What is left of this entry:** re-run the load arm on 2.7.2. If it is clean,
the identification holds and this closes. If it is not, there is a second
crash and the material below still applies.

---

### The original entry, as written 2026-10-26


Observed 2026-10-25 during load-tooling work: 2.7.1 segfaults quite reliably
under test load, and the measurement arms moved back to the `-ol26` 2.6.10 pair
as a result. **Not investigated** — deliberately deferred, because that session
was building load tooling rather than debugging slapd. No core, no stack, no
isolated arm, no statement of which operation mix triggers it.

Recorded because the detail is perishable and because it is **material to
ADR-040 decision 8**, which holds OpenLDAP 2.7 as the default on the argument
that ITS#10597 is temporary and self-resolving (fixed in 2.7.2, inside the same
storage-format line, no migration) while a move to 2.6 would be a permanent
migration — the wrong trade, since whoever takes the default never migrates.

A reproducible segfault under load is a different fact from a known upstream
bug with a fix in flight, and it was not in that pricing. It does not reverse
the decision on its own — an uncharacterised crash is not a comparable to a
read ITS — but the decision was taken on a balance that now has an unweighed
term on one side.

**And it may not be a 2.7-specific fact.** The entry below records a SIGSEGV on
**2.6** (`-ol26src`) with a demonstrated trigger: `olcAccessLogPurge` rewritten
while that database's purge was running. Before weighing this against ADR-040
decision 8, check whether the 2.7.1 crashes correlate with purges, with
rotation being on, or with scale events — if they do, these are one defect with
two version labels, and this entry's bearing on the default-version decision
evaporates.

### Shape of a fix (not written)

- Capture one under the `-asan` image before the detail erodes
  (`docs/DEBUG-IMAGES.md`): libasan names an invalid free or use-after-free at
  the moment it happens, and the in-process symbolizer works in the distroless
  image.
- Record the operation mix, the offered rate and whether the pod was purging —
  the three things that would let anyone else reproduce it.
- Then, and only then, revisit ADR-040 decision 8 with it weighed.

## slapd segfaults when `olcAccessLogPurge` is rewritten while that database's purge is running

**Observed once, 2026-10-25, on `slapd:sha-56d22dd-ol26src` (OpenLDAP 2.6,
source-built).** Not investigated — recorded with enough detail to try to
reproduce it.

A `spec.replicas` scale-down from 3 to 2 on a rotation-enabled cluster
(`{enabled: true, mode: PurgeAndWrite, slotLength: 300s, leadTime: 180s}`)
crashed `slapd` on pod 0 with **exit code 139 (SIGSEGV)**. The pod was not
replaced — the container restarted in place, so the only outward sign was
`RESTARTS 1` on a pod 35 h old.

### Why a scale changes the purge schedule at all

Under rotation the purge interval is `replicas × slotLength`, so **a replica
change necessarily rewrites `olcAccessLogPurge` on every surviving read-write
pod** — here `T` went 900 s to 600 s, and the per-pod phases moved with it.
There is no timing luck available: the rewrite is what a scale event *is*,
under rotation.

### The sequence, read from the two pods' logs

Pod 0 logged two purge **starts** (`accesslog_purge: updating minCSN with 3
values`) and two **completions** (`accesslog_purge: dn=… etime=… nentries=…`):

| time (UTC) | pod 0 |
|---|---|
| 21:00:00 | purge #1 **starts** |
| 21:00:28.1c | `MOD dn="olcOverlay={1}accesslog,olcDatabase={3}mdb,cn=config" attr=olcAccessLogPurge` |
| 21:00:28.1e | purge #2 **starts** — same second as the MOD, while #1 is still running |
| 21:00:28.21 | `MOD attr=olcSyncRepl` (dropping the departed peer's stanza) |
| 21:00:30 | purge #1 completes, `etime=29.960435 nentries=84377` |
| 21:00:32 | purge #2 completes, `etime=4.034848 nentries=10857` |
| 21:00:34 | **SIGSEGV** |

Two `accesslog_purge()` executions overlapped on one accesslog database for
about two seconds, and slapd died two seconds after the second returned.

**Pod 1 received the identical pair of modifies in the same reconcile pass and
did not crash.** Its difference is the whole discriminator: its purge started
*after* the MOD (start 21:00:28.2a, completion 21:01:01, `etime=33.334573
nentries=89464`). It was not mid-purge when the schedule was rewritten.

### Where to look first — this may be our patch, not upstream's

The operator writes the attribute as a plain single `Replace`
(`ensureOverlayAttr`, `slapddatabase_controller.go`), not as the
delete-then-re-set that ADR-037 decision 5a describes. On stock 2.7.1 sources
ADR-037 read that a Replace on an existing task *"only assigns
`re->interval.tv_sec` and leaves `next_sched` alone"* — which would **not**
fire a second purge. So the observed second purge is not explained by the
upstream behaviour the ADR recorded.

What is different here is that this build carries
`images/openldap-deb/local-patches/accesslog-purge-phase.patch`, ours, which
rewrites exactly that config path. In the live-update branch it now takes
`rq_mutex` and writes `re->interval.tv_sec`, `re->phase.tv_sec` and
`re->aligned`; the sibling branch inserts a **new** runqueue task when
`re == NULL`. Two questions to answer first, both cheap and both from source:

- Is `li->li_task` non-NULL for the whole time a purge is *running*? If the
  runqueue clears or detaches it while the task executes, a config modify
  arriving mid-purge takes the `else if ( li->li_open )` branch and **inserts a
  second task**, which is exactly the shape observed.
- Does the resched arithmetic (`next_sched = ph + ((now − ph)/iv + 1)·iv`) ever
  land in the past or at `now` when `interval` and `phase` both change under a
  running task?

The patch's own comment records that this path was already racy before it
(*"leaving `re->interval.tv_sec` to a racing unlocked write"*), so it is a
known-delicate site that we narrowed rather than one we introduced from
nothing — but it is ours, locally maintained, and in the crash's path. Treat
"slapd has a bug" as the second hypothesis, not the first.

**Narrowed 2026-10-29, and the obvious suspect is excluded.** Two candidates
are now ruled out, which is most of the value this entry had left to give:

- **Not the unlocked phase/aligned write.** The `2.7.1-0+slaptain3` changelog
  entry (2026-10-20) fixes exactly that race — it takes `slapd_rq.rq_mutex`
  around the live update of an existing purge task, and it was written from a
  SIGSEGV observed 13 s after a three-value to two-value `olcAccessLogPurge`
  change, which is this crash's shape. The image that crashed on 2026-10-25
  was built from a tree carrying that fix. So the remaining defect is **not**
  the one already found and fixed on this path.
- **Not ITS#10597.** Upstream's accesslog double-free was introduced in 2.6.14
  and the crashed image is 2.6.10, which predates it. The one upstream
  candidate that would have explained a crash in this overlay does not apply.

What is left is a race in the same path that survives the `rq_mutex` fix, or
something else entirely. Both exclusions are from reading dates and versions,
not from a reproduction — the recipe below is still unattempted and still the
next step.

### Shape of a reproduction (not attempted)

The window is the purge duration inside the interval — here ~30 s in 600 s, so
roughly 5 %. Widen it rather than retrying blindly: short `retention`, a high
write rate and a large journal make `P` most of the cycle.

1. Rotation-enabled cluster, `n >= 2`, on a build carrying both local patches
   (the logging one is what makes any of this visible: `-ol26src`, or a 2.7
   build which has its own timing line).
2. Drive writes until a purge takes tens of seconds. Watch for
   `accesslog_purge: updating minCSN with N values` — that line marks the
   **start**; `accesslog_purge: dn=… etime=…` marks the **completion**. Both
   are needed to see an overlap.
3. While a purge is in flight, force the attribute to be rewritten. Either
   route should do it, and the second one isolates the operator out of the
   picture:
   - change `spec.replicas` or `spec.rotation.slotLength` (the operator then
     Replaces `olcAccessLogPurge` on every read-write pod); or
   - `slctl ldapmodify --as config --pod <n>` a Replace of `olcAccessLogPurge`
     on `olcOverlay={1}accesslog,olcDatabase={N}mdb,cn=config` directly, with a
     changed interval.
4. Expect a second purge start within the same second as the MOD, overlapping
   the first, and watch for exit 139.

Run it under the `-asan` image (`docs/DEBUG-IMAGES.md`) from the start — libasan
names an invalid free or use-after-free at the moment it happens and the
in-process symbolizer works in the distroless image, which is the difference
between this entry and a root cause.

### Why it matters beyond one restart

- It sits on the rotation feature's path and is triggered by an **ordinary
  operation**. ADR-013's rule is that scaling must not roll pods; this does not
  roll one, it crashes one.
- **The operator cannot avoid it today.** ADR-037 decision 5a establishes that
  `back-monitor` has no runqueue subsystem — *"Phase cannot be read back"* — so
  the operator has no way to know a purge is in flight and defer the write.
- It may be a third member of a family ADR-024 already names: *never modify
  `olcDbMaxSize`/`olcDbEnvFlags` on a live database — slapd segfaults*. If
  `olcAccessLogPurge` cannot be modified safely while its purge runs, its
  ADR-024 R6 placement is in question and ADR-034 (startup-only, converged
  offline at pod start) is the obvious alternative — at the cost of making a
  replica change require a restart, which ADR-016 prices as a topology event at
  every peer.
- It is **2.6**, which means it is not the 2.7.1 crash recorded above and the
  two entries should not be merged until someone checks whether they are one
  defect.

## The accesslog syncprov checkpoint does not retrofit, and the data DB has no default at all

Two findings from one observation, 2026-10-31, pointing in opposite directions.
Both have since been answered: the first is a diagnosed defect waiting for its
fix, the second a design question ADR-045 decided.

**Observed.** Two clusters on one lab Kubernetes cluster, same operator build,
same `SlapdDatabase` shape — one created 2026-10-13 under a pre-0.5.0
operator, one created 2026-10-31 under 0.5.0. Read from `cn=config` on both:

| | data DB (`dc=example,dc=org`) | accesslog DB (`cn=accesslog-<db>`) |
|---|---|---|
| older cluster | `olcSpCheckpoint: 500 15` (set in the CR) | **absent** |
| newer cluster | absent | `olcSpCheckpoint: 1 1` |

Exactly inverted, and neither CR sets `accesslogSyncprovCheckpoint`.

### 1. Defect, diagnosed 2026-11-07: the checkpoint never converges onto an accesslog overlay that already exists

**Fix this next.** Not urgent to the hour — the one measurement harness that
trips over it carries a workaround — but every accesslog overlay created before
the `1 1` default shipped still has no checkpoint, and nothing the operator does
will ever give it one. That is the ITS#9398 exposure the default exists to
close, left open on exactly the population the convergence code was written for.

**Mechanism, source-confirmed.** `ensureAccesslogSyncprovCheckpoint`
(`slapddatabase_controller.go:2285`) builds the overlay DN as
`"olcOverlay=syncprov," + dbDN` and base-searches it. slapd assigns an ordering
index on the ADD and stores the entry as `olcOverlay={0}syncprov,<dbDN>`; it
accepts the unindexed form as an ADD target and refuses it as a search base,
answering `No such object (32)` with `matchedDN: <dbDN>`. The handler reads that
code as *"the overlay is not there yet, the next pass creates it with the
checkpoint in the ADD"* and returns nil. The function therefore returns success,
writes nothing and logs nothing, on every accesslog overlay that exists.

**Measured, 2026-11-07**, on a 2.6 cluster with two providers and four
consumers. `accesslogSyncprovCheckpoint: "none"` was applied; the CR read back
`none`, the operator log carried neither `converging accesslog syncprov
checkpoint` nor `removing accesslog syncprov checkpoint` across the whole
reconcile window, and the live attribute was unchanged on both providers before
and after a rolling restart. In the same reconcile the **data** database
converged and said so (`removing overlay attribute dn=olcOverlay={0}syncprov,…
attr=olcSpCheckpoint`). Two attributes, two overlays, one reconcile: one
converged and one silently skipped — which is what pointed at the DN rather than
at the plumbing. Confirmed directly by two base searches against the same pod:
the constructed DN refuses, the indexed DN returns the entry.

The rest of the chain is sound and never reached: `noneSentinel` and
`desiredAccesslogSyncprovCheckpoint` resolve `"none"` to `""`, and
`planOverlayAttr` maps `want == ""` with a live value to `overlayAttrRemove`.

**Why it stayed invisible.** The ADD at `:2258` uses the same constructed DN and
works, so a freshly created overlay takes its checkpoint from the creation
request and looks entirely correct. Only the path for overlays that already
exist is dead. A reader of the CR cannot tell the difference — the field reads
as the default, and the default is not in force — and nothing reports it:
`csn-convergence` stays green, and the storm predicate (`minCSN` above
`contextCSN`) only becomes visible after an unclean death. The cluster in the
table above carried `1 1` because its overlay was created afterwards; the bug
was hidden by the cluster being new. How much of the installed base is affected
is **not established**: it depends entirely on whether a given overlay predates
the default.

#### Shape of a fix

Resolve the DN, never construct it — which the data path next door already does
(`ensureSyncprovTunables`, `:1424-1434`: one-level search for
`(objectClass=olcSyncProvConfig)`, then `overlayDN := sr.Entries[0].DN`, with a
comment saying in so many words that the `{N}` prefix is slapd's to assign).

Cheaper here than there: `reconcileAccesslogDB` **already** reads the log DB's
children one level down (`:2220-2237`) and walks their object classes to compute
`hasSyncprov`, so the real DN is in hand. Capture it in that loop and pass it in,
and `ensureAccesslogSyncprovCheckpoint` loses its search, its DN construction and
its `NoSuchObject` branch together — keeping the "one search, at most one write"
its own doc comment promises, which a second search would quietly break. The
empty case becomes "no syncprov child", which is a different fact from "the
server did not recognise the name I built"; today they are the same code.

Leave the ADD at `:2258` alone. It works, and it is the only reason any cluster
has a checkpoint at all — but it is the source of the asymmetry that made this
hard to see, and a comment there naming it is cheap.

**Red first, and a mock will not do it.** A mocked connection answers whatever
DN it is asked for. The spec has to run against a real slapd whose overlay
**already exists**: create the overlay, *then* change the desired value, *then*
assert the live attribute moved. `tests/e2e/sessionlog_test.go:174` already
asserts every log DB carries an `olcSpCheckpoint` whose ops half is 1 — green,
and toothless here, because the fixture's overlays were created by the ADD path
that works. It is the create-and-assert-in-one-pass case, which is exactly the
case that cannot fail.

**How it was found**, for whoever writes that spec: not by reading the operator.
A measurement harness put a cluster into a declared posture and verified it by
two routes that fail differently — the CR read back, and the live `cn=config` on
every provider. The first passed, the second refused. A CR read-back proves the
API server took the value and says nothing about whether slapd has it.

**Workaround in force, to be removed by the fix.** The slapload harness writes
the attribute itself, resolving the overlay DN by search and reading it back off
every provider. It deliberately leaves the data DB checkpoint and the sessionlog
alone, which the operator does converge and where a second writer would race it.
Recorded on that side as a workaround rather than as a design, so that this fix
deletes it instead of someone finding it years later and wondering why a load
harness writes server configuration.

**Repairing an already-deployed overlay by hand is not automatically benign.**
Rewriting `olcAccessLogPurge` on a running server has produced a SIGSEGV in this
environment (its own entry above), so treat a live `cn=config` write on a
provider as a hazard: one pod at a time, readiness checked between. With that
said, writing `olcSpCheckpoint` to the overlay's real DN as the config identity
was observed once on two providers under load, accepted, with neither pod
restarting. One observation, not a safety claim.

### 2. Design question — answered by ADR-045, 2026-11-07

`syncprovCheckpoint` — the **data** database's — has **no default**, and slapd's
own default is no checkpointing at all. The question was whether slaptain should
have an opinion here at all. **It should: ADR-045 defaults it to `100 10`.**

Two of the three objections raised here did not survive the research and are
corrected rather than left standing:

- the write amplification is not ADR-037's. `syncprov_checkpoint` sets
  `o_dont_replicate = 1` and `accesslog_response` skips such operations
  (`accesslog.c:1659-1661`), so a data-database checkpoint never reaches the
  accesslog environment at all;
- changing the default later is not ADR-040 decision 8's forced migration. The
  attribute is converged per pod (ADR-024 R4), so a move rewrites one value on
  one entry per pod.

The third — the unexplained asymmetry with the accesslog database's `1 1` — has
an answer, and it is the ADR's central one: `contextCSN` reaches disk by a third
path the log database structurally lacks, so the two failures differ in kind.

**ADR-045 R1 is not blocked by finding 1.** That defect is the accesslog path
constructing a DN slapd refuses to search; the data database's checkpoint
resolves its overlay DN from the server and converges on every reconcile, so a
`100 10` default reaches existing overlays on existing clusters. The two fixes
are independent and want separate commits.

## A hand-applied cluster lands in `StartupConfigConverged=False/RestartRequired` and stays there

Apply a `SlapdCluster`, then a `SlapdDatabase`, and the cluster comes up with a
False condition that only a human can clear. Observed 2026-10-31 on a freshly
created cluster: pod created at `10:18:44`, `SlapdDatabase` at `10:18:56` —
twelve seconds apart, and enough.

```
olcSpSessionlogSource: live <absent>, declared "cn=accesslog-example-db"
```

**The mechanism, and it is nobody's bug in isolation.** The `SlapdCluster`
controller builds `<cluster>-startup-config` from the `SlapdDatabase` CRs that
exist *at that moment*. Apply the cluster alone and that table is EMPTY, the
StatefulSet is created, and the init container converges nothing. The database
arrives seconds later, the operator adds `olcSpSessionlogSource` to the
ConfigMap — and nothing re-runs the init container. The attribute is
startup-only (ADR-034), so it cannot be written to a running slapd, and the
operator deliberately will not restart a pod to apply it, because a provider
restart is a topology event at every peer (ADR-016).

Every rule involved is behaving as designed. The result is still that
**applying custom resources does not converge the cluster**, which is the
contract users expect of a declarative API.

### Why this was not caught

**It is order-dependent, and the validated path takes the lucky order.**
`docs/RELEASE-PLAN.md` records of the 0.5.0 mesh cycle: *"First mesh exposure
of the session-log default, and it converged: the pods did take
olcSpSessionlogSource on a fresh three-site cluster."* The mesh chart applies
`SlapdMesh`, `SlapdCluster`, `SlapdDatabase` and `SlapdSchema` **together**, so
the database CR already exists when the cluster controller builds the table and
the pods start converged. Apply the two separately — which is what a user
reading the README's `kubectl apply` examples does, and what every hand-built
lab cluster does — and they do not.

So the e2e and the mesh cycles pass while the documented manual path fails, and
the two differ only in whether one `kubectl apply` or two produced the objects.

### Why it is worse since 0.5.0

Before, `syncprovSessionlogSource` was unset and resolved to `InMemory`, which
is slapd's own default — there was nothing to converge and the table was empty
for everyone. 0.5.0 made `Accesslog` the default, so **every** cluster now has
a startup-only attribute to converge, and every cluster built the two-step way
now lands in `RestartRequired`. A release note covered the upgrade path for
existing clusters; the first-run experience of a NEW one was not considered.

### Shape of a fix — a decision, not a patch

The obvious repair is for the operator to roll the StatefulSet when the
startup-config table changes. That collides head-on with two accepted rules:
ADR-013 forbids rolling a fleet because another resource's lifecycle moved
(adding a database is exactly its example), and ADR-016 prices a provider
restart as a topology event at every peer. **ADR-035 says a collision is itself
a decision, taken on the merits and recorded in both** — so this wants that
treatment rather than a quiet change to either side.

Options worth costing when someone takes it, none chosen here:

- roll on startup-config change, accepting the ADR-013/ADR-016 cost, possibly
  only for the FIRST transition out of an empty table (a cluster with no
  databases has no peers to disturb, which weakens ADR-016's objection
  considerably for exactly the case that bites);
- make the init container's convergence reachable without a full pod restart;
- converge nothing and make the condition's remediation a documented install
  step — honest, and an admission that the API is not declarative here;
- have the cluster controller withhold the StatefulSet until the first
  database's startup-only table is known, which trades a manual restart for a
  slower first start and needs a rule for clusters that legitimately have no
  database.

Meanwhile the workaround is to apply all CRs in one go, or to
`kubectl rollout restart` once after the first database lands.

## `spec.placement: Invalid value: "null"` on one `helm upgrade --reset-values`

Observed **once**, 2026-10-30, upgrading the mesh release during ADR-043 validation:

```
spec.placement: Invalid value: "null": spec.placement in body must be of type object
```

**Not reproduced**, and recorded for that reason rather than chased. A clean
chart install / upgrade / remove cycle did not show it, nor did the analogous
`monitoring` key, and the same upgrade carried an unrelated pre-existing SSA
conflict on `example-db .spec.replication.accesslogPurge.retention` — so there
is more than one candidate for what actually produced it.

**Why it is worth a line anyway.** If it is real it is not a property of
placement at all, but of **every conditionally-rendered object key in these
charts** — `monitoring`, `rotation`, `accesslogPurge` and now `placement` are
all emitted only when set, and all would be exposed to the same mechanism.
That makes it worth recognising on sight rather than re-diagnosing from
scratch, and it means a fix would belong to the chart idiom rather than to one
key.

### Shape of an investigation (not attempted)

- Reproduce deliberately: install with the key set, upgrade with
  `--reset-values` and the key absent, against a CRD that has the field.
- The hypothesis to test first is that `--reset-values` renders the key as an
  explicit `null` rather than omitting it, and that the structural schema
  rejects null for an object. If so, the question is whether the chart should
  be emitting the key at all in that state — which is the same `kindIs`
  reasoning ADR-043 forced for `topologySpreadConstraints`, one level up.
- Red first: a `helm template` case asserting the key is ABSENT, not null,
  when the value is unset — cheap, and it would have caught this class before
  a cluster ever saw it.
