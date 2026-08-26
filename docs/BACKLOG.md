# Backlog (non-migration tech debt)

Cross-cutting cleanup items that aren't tied to the migration plan
(`docs/MIGRATION-PLAN.md`) or a single ADR. Keep entries small and actionable.

## Deprecation cleanup: `client.Apply` → `client.Client.Apply()` / `SubResource().Apply()`

**What:** controller-runtime deprecated the package-level `client.Apply` patch
type (staticcheck `SA1019`). All controllers currently use it for server-side
apply, e.g.:

```go
r.Patch(ctx, obj, client.Apply, client.ForceOwnership, client.FieldOwner(...))
r.Status().Patch(ctx, p, client.Apply, client.ForceOwnership, client.FieldOwner(...))
```

`make -C operator lint` flags this in `slapdcluster_controller.go` (×6),
`slapddatabase_controller.go`, `slapdschema_controller.go`, and
`slapdbackup_controller.go` — a handful of `SA1019` hits.

**Why deferred:** it's a uniform, mechanical migration to the newer
`client.Client.Apply()` / `client.Client.SubResource("status").Apply()` API
across every controller. Doing it wholesale in one focused change is cleaner
(and easier to review) than touching it inline inside unrelated feature work.
We are not ignoring it — it's parked here deliberately.

**How:** migrate every `client.Apply` call site in one pass; re-run
`make -C operator lint` to confirm the `SA1019` count drops to zero. Verify the
field-manager ownership semantics are unchanged (same `FieldOwner`,
`ForceOwnership`).

**Note:** `make -C operator lint` currently reports broader pre-existing debt
too (errcheck, gofmt, modernize, unused, gocyclo, lll — ~70 findings as of
2026-06-08). Worth a separate sweep, but out of scope for this entry.

## Add a README to the operator Helm chart (`charts/operator/`)

**What:** `charts/operator/` has no `README.md`. The project docs
(`README.md`, `docs/BACKUP.md`, ADRs) live in the repo, so a consumer who
installs the chart from a registry (`helm install … oci://…/slaptain-operator`)
has no docs at the point of use — values, CRDs, and the backup/restore feature
set are undiscoverable from the chart alone.

**Why deferred + open question:** unclear what shape it should take. Options:
a copy of the top-level `README.md` (drifts — two sources of truth); a thin
chart-specific README (values reference + "see the project README/docs for
concepts" links); or generated from `values.yaml` via a tool like
`helm-docs`. Decide the approach before writing it — a verbatim copy of the
top-level README is explicitly *not* wanted.

**How:** pick the approach (lean: a short chart README documenting `values.yaml`
knobs + image/CRD notes, linking back to the repo docs rather than duplicating
them), then keep it from drifting (helm-docs in `make operator-manifests`, or a
CI check).

## Remove the `foreignRIDs` field + its overlap validation (RIDs are consumer-local)

**What:** `SlapdDatabase.spec.replication.foreignRIDs` (and the validation that
rejects a DB whose computed RID range overlaps it) lets a deployer declare RIDs
"in use on the other side" of an external-peer relationship, to avoid a
cross-cluster RID collision. Remove it — the collision it guards against cannot
happen. A `rid` is the *consumer's* local handle for a syncrepl directive: it
keys that consumer's own replication cookie state and is never exchanged on the
wire. slaptain and any peer/source each number their own stanzas independently,
in disjoint per-node namespaces, so cross-cluster RID coordination is meaningless.

**Why deferred / why it's inconsistent right now:** ADR-011 was rewritten to drop
the cross-cluster RID discussion (RIDs are invisible across clusters), but that
rewrite deliberately did **not** touch the operator code. So the `foreignRIDs`
field + webhook currently outlive the ADR that justified them — no ADR endorses
the field anymore. This entry is the reconciliation reminder. (Contrast:
`foreignServerIDs` **stays** — ServerIDs *are* global, embedded in the CSN and
tracked in `contextCSN`, so cross-cluster ServerID collision is real.)

**How:** drop `foreignRIDs` from `SlapdDatabase` types + deepcopy + CRD; delete
the RID-overlap validation; keep `ridBase` (that's slaptain's *own* intra-cluster
RID uniqueness — still valid and still needed). Regenerate manifests; `grep -r
foreignRIDs` → 0. Update any docs/tests that referenced it.

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

Why it has resisted diagnosis: the spec's cleanup deletes the SlapdCluster and
SlapdDatabase, so by the time anyone looks there is no pod, no CR, and no pod log
left to inspect. **Anyone picking this up should first make the failure
inspectable** — keep the cluster on failure (skip cleanup when the spec failed, or
gate cleanup behind an env var) and capture the init-container and slapd logs from
the pod that will not start. Without that, this is unfixable by inspection after
the fact.

Related but separate: the restore specs leak their PVCs. Their cleanup deletes the
CRs but not the volumes, and StatefulSet `volumeClaimTemplates` PVCs are never
garbage-collected (no `persistentVolumeClaimRetentionPolicy` is set anywhere).
Orphaned `*-slapd-restore-0` PVCs accumulate across runs. That is not the cause of
this flake, but it does make `e2e.sh test` re-runs dirty and should be fixed
regardless — deleting the PVCs in the specs' cleanup, not by changing operator
behaviour, since leaving PVCs on SlapdCluster deletion is defensible (ADR-005's
Retain default).

---

## Peer address discovery has no vote in the requeue decision

Under ADR-016 pod-routed transport, replacing a pod invalidates every peer's
syncrepl addresses, and recovery is measured in minutes (147/140/268/130 s on a
three-site lab — see the ADR-016 amendment of 2026-08-26). The cause is not that
60 s is the wrong cadence for CSN monitoring. It is that **peer address discovery
rides on the CSN-monitoring tick by accident**, and cannot ask for anything faster.

`SlapdCluster.Reconcile` has exactly two exits:

    not Running                  → RequeueAfter 10s
    Running + replication.enabled → RequeueAfter csnCheckInterval (60s)

Discovery runs earlier in the same pass and updates
`status.externalPeerStatuses[].discoveredAddresses`, but there is no path by which
"this peer's address set just changed under me" selects the faster exit. So a
discovery tick landing mid-restart writes a partial address set and then waits a
full 60 s to correct it — which is where the multi-minute outages come from.

**The fix is small, and the idiom already exists in this codebase.** "Tight requeue
while a transition is in flight, loose once settled" is used throughout —
`slapdcluster_restore.go` runs a 2 s/5 s/10 s/30 s ladder across its phases, and the
database, schema and backup controllers all use a 10 s retry on unmet
preconditions. This is a concern that is not wired into an existing decision, not a
mechanism that needs inventing.

The state needed is **already persisted**: `discoveredAddresses` lives in status, so
the reconcile can compare the previous set against the freshly-discovered one before
overwriting it. No new field, no new watch, no extra remote connection — a changed
set selects the 10 s exit, and it backs off to 60 s once the set stops changing.

Expected effect: the "landed badly two to four times" multiplier largely disappears,
since a mid-restart tick then costs ~10 s of staleness rather than 60 s.

Care needed on two points: do not let a flapping peer hold the cluster in a 10 s
loop indefinitely (bound the fast phase, or require N stable observations before
backing off), and keep CSN monitoring itself on its own 60 s cadence — the two
concerns should be separated by this change, not merged harder.

Also worth covering while in here: the same window opens on every
`bootstrapFrom`/`SlapdRestore`, because the restore state machine scales to 0 and
back up. A restore on a mesh member is currently also a multi-minute cross-site
replication outage, and nothing says so at the API surface.

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
  link. Heartbeat writes would close that, and are deferred with reasons in the
  ADR-008 amendment — the objection is that the operator would be writing into the
  user's data tree, not that it wouldn't work.

---

## Accesslog index set is incomplete

The per-database accesslog DBs (ADR-019) are created with
`olcDbIndex: default eq` + `reqEnd,reqResult,reqStart eq`. Upstream indexes
`entryCSN,objectClass,reqEnd,reqResult,reqStart,reqDN`. Note `index default eq`
indexes nothing on its own — it only sets the default *type*.

`reqDN` is the one that matters: multi-provider out-of-order modify resolution
searches the local log with `(&(entryCSN>=…)(reqDN=…)…)` on **every** conflicting
write (`syncrepl.c`), so on a write-contended mesh that is an unindexed
attribute assertion on a hot path. `entryCSN` and `objectClass` are cheap wins.

Deliberately out of scope for ADR-019 (it changes performance, not correctness,
and folding it in would have muddied that ADR's blast radius). Independent of it:
the fix is a one-line change to the index list plus an e2e that asserts the
resulting `olcDbIndex`.

---

## e2e cannot pin an old `slapd-init` image, so one migration failure mode has no guard

The 2026-08-25 "syncrepl stanzas written to a pod whose accesslog DB does not
exist" fix is proven only by hand-run live scenarios. A permanent regression test
needs pods whose init container predates the per-database accesslog directory
creation, because the missing thing is a *directory on a PVC the operator cannot
touch* (ADR-018) — it is not manufacturable from `cn=config` the way the legacy
shared log is (`E2E_ACCESSLOG_MIGRATION=1` does exactly that trick).

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
