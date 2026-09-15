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

**Still open:** the broader pre-existing lint debt this entry always excluded —
errcheck 40, lll 31, modernize 15, goconst 13, prealloc 13, gocyclo 9, unused 4,
unparam 2, revive 1 (uncapped counts, unchanged by this change). Worth a separate
sweep.

## `make -C operator lint` cannot run under Go 1.27

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

- **Cost:** every create and delete changes the StatefulSet's `DATABASE_DIRS`
  and rolls the cluster (ADR-013's accepted wart). The container pays that four
  times — ~8-10 minutes of a suite that otherwise runs in ~13.
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

Still open: `syncprov-sessionlog-source` (the persistent, accesslog-backed
sessionlog) was rejected in ADR-022 because it reads the change journal — the
artifact that is contaminated or purged in exactly the scenarios where a
persistent log would pay off. Revisit once the journal's trustworthiness across
refresh/purge transitions is settled (upstream ITS#9580 work, ADR-021).

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
- **No `cn=monitor`, no native metrics** (hygiene; *attempted 2026-09-13 and
  withdrawn*). The monitor backend module is not loaded, no monitor database
  exists, `olcMonitoring` is never set — so there are no per-database operation
  counters and nothing an exporter can read. Monitoring today is the operator's
  CSN polling, which by ADR-008's own amendment cannot see an idle-but-broken
  link.

  An implementation landed and was reverted the same day. Individually every
  piece works on a running 2.7.1 pod: `olcModuleLoad: back_monitor` is accepted,
  `olcDatabase=monitor` can be added live, `cn=Monitor` answers immediately, and
  an ACL granting read to the existing `cn=replication,<suffix>` identity
  (ADR-008 reuse) enforces correctly — anonymous gets `No such object`. What
  does **not** work is converging it: with a monitor database present, an
  `ldapmodify` of its `olcAccess` hangs slapd with the same signature as the
  connection timeouts above (CSN queued, never graduates, pod dead to all
  clients), observed on three of four pods. The operator rewrote that ACL every
  reconcile because its comparison of what it wrote against what slapd stores
  never matched — so steady state was one modify away from bricking the cluster.

  *Surface when picked up again:* fix the ACL comparison FIRST and prove the
  operator reaches a no-write steady state, then establish whether an `olcAccess`
  modify on a monitor database is safe at all. Until both are answered this is
  not a hygiene feature, it is an outage generator. The e2e assertions (monitor
  database present per pod; readable by the replication identity; denied
  anonymously) are worth restoring from commit `07b89af`'s parent.
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

## The replication credential lives in two stores with no reconciliation between them

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
