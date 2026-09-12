# ADR-023: Rolling volume replacement — rebuild a pod from the mesh

**Status:** Proposed
**Date:** 2026-09-12

## Context

ADR-021 put OpenLDAP 2.7.1 on the plain image tag. LMDB 1.0 breaks the on-disk
format, so 2.7 slapd cannot open a `/data` or `/accesslog` volume that 2.6 wrote:
there is no in-place upgrade, and pointing an existing cluster at the new images
is a crashloop, not a migration.

The supported runbook today rides ADR-014: `SlapdBackup` on 2.6, pin the 2.7
tags, wipe every volume, `SlapdRestore`. It works — `docs/OPENLDAP-VERSIONS.md`
spells it out step by step — and it costs a full outage. The cluster is
unavailable from the moment the tags are pinned until the restore completes,
which for a large directory is a planned window, per site, with a human driving
seven manual steps.

We already own a primitive that does the same job without the outage. ADR-012
case 2: a pod that loses its volumes rejoins the mesh empty, the init container
re-bootstraps `cn=config`, and syncrepl pulls the entire DIT from the surviving
peers. No re-seed, no operator-curated data path.
`tests/e2e/dataloss_recovery_test.go` proves it on the persistent fixture —
including that a user-added witness entry
written *after* the seed comes back, which is what distinguishes real replication
recovery from a fake re-seed.

Because syncrepl carries entries rather than pages, the wire is
**format-agnostic**: a refreshing consumer writes what it receives through its
own slapd, in whatever storage format that binary produces. A pod brought back on
the 2.7 images with empty volumes therefore lands the DIT in LMDB 1.0, with no
storage-level conversion anywhere. `docs/OPENLDAP-VERSIONS.md` already names this
as "the alternative" and warns it off — because it is a manual procedure. It
should be an operator-driven one.

Version crossing is the headline, but nothing in the mechanism is about versions.
"Rebuild this pod from the mesh" is also how you migrate to a different
`storageClassName`, grow volumes on a provisioner that cannot expand in place,
or change the filesystem underneath. That primitive outlives the 2.7 transition;
the API should not be named or shaped as if it were an upgrade tool.

What we must not do is infer the operation. `docs/BACKLOG.md` states the
constraint plainly: image tags are free-form, nothing reliably marks a tag as 2.6
or 2.7, and ADR-018 means the operator cannot inspect the volumes to find out.
The operator cannot tell a version-crossing image change from an ordinary
same-version bump — and the two demand opposite handling.

## Options considered

### 1. Trigger the replacement automatically on a `spec.images` change

Rejected. Most image edits are same-version bumps — a rebuild, a CVE patch, a
tag move — where the StatefulSet's normal in-place rolling update is exactly
right and keeps the data. Since the operator cannot infer version crossing (the
BACKLOG constraint above), automatic replacement would mean wiping every volume
in the cluster on every tag edit. Silently destroying state in response to a
one-character spec change is the worst surprise this operator could produce.

### 2. A declarative `spec.updateStrategy: Replace` on SlapdCluster

Rejected. It reads better than option 1 — the user opts in — but it leaves a
standing setting behind that converts every *future* image edit into a volume
wipe. A field whose blast radius depends on a decision someone made months ago,
in a spec nobody re-reads before a patch bump, is a loaded gun. Replacement is
an event, not a policy: it happens once, for a stated reason, and then the
cluster is back to ordinary rolling updates.

### 3. Offline dump and reload per pod (the ADR-014 runbook)

This is the status quo, and it stays. It is correct, it is the only option that
works at `replicas: 1`, and its slapadd-all-pods data path deliberately avoids
the syncrepl refresh entirely (ADR-014 amendment, 2026-06-09). Its cost is the
outage. It remains the documented fallback, and the only supported path for
single-replica clusters.

### 4. An imperative, one-shot replacement request (chosen)

One object carries the whole intent of one replacement operation; the operator
drives it pod by pod, gated on redundancy. It is as explicit as option 3, as
automated as option 1, and leaves no standing setting behind like option 2.

## Decision

### API: `SlapdRollingReplace`

An imperative, immutable-once-created request, in the shape ADR-014's
`SlapdRestore` established:

```
SlapdRollingReplace (srr)      # imperative, spec immutable
  spec.clusterRef              # the SlapdCluster to roll
  spec.images                  # optional: the target image set (same shape as
                               #   SlapdCluster.spec.images). When set, the
                               #   operator writes it onto the SlapdCluster spec
                               #   as the first step of the operation.
  spec.minAvailable            # optional: surviving current copies required to
                               #   proceed (default 2)
  status.phase                 # Pending|Preflight|Replacing|Completed|Failed
  status.pods[]                # per pod: name, state Pending|Replacing|Converged
  status.startedAt/completedAt/message/conditions
```

`spec.images` lives on the request, not only on the cluster, because one object
should carry the whole intent: "move to these images, and rebuild the volumes to
get there" is a single decision and should be a single, auditable record of it.
The alternative — the user patches `SlapdCluster.spec.images` first and the
request only orders the rebuild — was considered and rejected for splitting one
operation across two edits, with a crashlooping cluster in between (exactly the
window `docs/OPENLDAP-VERSIONS.md` step 3 warns about). It stays optional, so a
storage-class or resize replacement can order the rebuild with no image change
at all.

The **SlapdCluster controller** watches and drives it. No new reconciler: this is
the house pattern from the ADR-014 amendment, where the cluster owns the
StatefulSet and therefore owns every machine that scales, replaces or reloads its
pods. One replace per cluster at a time, and a replace serialises against a
restore — the two machines both move pods and PVCs, and must never interleave.

### The machine

Per pod, in ordinal order, RW StatefulSet first:

1. Delete the pod's PVCs (`config-`, `data-`, `accesslog-`). They only get a
   `deletionTimestamp`: `kubernetes.io/pvc-protection` holds them while any pod
   object names them (ADR-018).
2. Delete the pod. The finalizer releases, the PVCs garbage-collect, and the
   StatefulSet recreates the pod from the current — possibly just-updated — pod
   template with fresh volumes from `volumeClaimTemplates`.
3. The init container bootstraps `cn=config`, the SlapdDatabase controller
   recreates the databases, and syncrepl performs a full refresh from the
   surviving peers.

This is `dataloss_recovery_test`'s trigger, promoted from a failure simulation to
an operation. The seed latch is untouched throughout: `SeedApplied` stays true
and the operator never re-seeds (ADR-012).

### Redundancy gate

Proceed only while at least `minAvailable` **current** copies survive — Ready and
converged, i.e. holding the DIT, not themselves mid-refresh. Default 2, which
requires `replicas >= 3`.

`minAvailable: 1` lets a two-replica cluster roll with a single surviving copy.
That is a real risk — one node failure during the window and the directory is
gone. We put it on the request as an explicit field precisely so that whoever
accepts it signs for it, once, in an auditable object.

Single-replica clusters are refused at preflight, with a message pointing at the
ADR-014 runbook. There is no mesh to rebuild from.

### Per-step gates

Before touching the next ordinal, all of:

- the replacement pod is Ready;
- `ReplicationConverged` holds (local CSN equality across the RW pods);
- where external peers exist, peer address rediscovery has settled.

The third gate is not caution, it is measurement. Per the ADR-016 amendment,
replacing a pod invalidates every address a peer's syncrepl stanzas hold for it,
and recovery on a pod-routed mesh was measured at 130–268 s. Roll the next
ordinal before that settles and the replacements stack: a peer whose stanzas name
a partial set of live providers waits a further discovery tick, and the window
grows by minutes per badly-landed tick. The roll is therefore *slow* on a mesh by
design — a gate, not a delay loop.

On any step failure the machine **halts**: it does not proceed, so redundancy is
preserved at whatever level it had reached. `status.pods[]` names the ordinal it
stopped at and `status.message` says why. The request is resumable once the fault
is cleared, by the operator retrying or by a human fixing the cause; it is not
auto-aborted and it does not roll back (there is nothing to roll back to — the
already-replaced pods are correct).

### Read-only replicas

The `<name>-readonly` StatefulSet rolls the same way, after the RW pods, under
the same gates. RO pods are pure consumers with no accesslog volume and no
journal, so their replacement is cheaper and cannot affect anyone else's
delta-sync source.

## Consequences

- Each replacement is precisely the ITS#9580 trigger from ADR-021 — a pod
  refreshing from empty volumes — yet on an upgrade *to* 2.7 the mesh gets
  monotonically safer as the roll proceeds. Every replaced pod's syncprov carries
  the cookie-flush fix (upstream `414866b8`, released only in 2.7.0/2.7.1), so it
  stops answering its own consumers `sync cookie is stale`. In the other
  direction, a 2.7 consumer refreshing from 2.6 providers took 0–3 isolated stale
  answers even under engineered dormancy, each self-healing with one full refresh
  (ADR-021's repro record). Ordering is what changes the arithmetic against the
  manual pod-by-pod wipe: replaced pods are fixed providers, so exposure shrinks
  with every step rather than being paid once per pod.
- A pod rebuilt from empty volumes holds no cookie, so the syncprov sessionlog
  (ADR-022) cannot help it: the refresh walks the whole database, once per
  replaced pod, per provider. At millions of entries that dominates the cost of
  the operation, and it is why ADR-014's slapadd-all-pods restore stays the
  faster path wherever an outage is acceptable.
- A mesh member rolling itself is intermittently a replication outage for its
  peers — once per ordinal, for the rediscovery window, because every replacement
  invalidates the addresses they hold. The gates keep that correct; they do not
  make it quick.
- Mixed-version mesh operation is argued, not proven. ADR-021 reasons it from the
  wire format — syncrepl is unchanged, the version skew is invisible — but we
  have never validated one end-to-end, and a rolling upgrade lives in exactly
  that state for its whole duration. → the feature's red-first e2e *is* that
  deferred validation: stand up an `-ol26` cluster, apply a
  `SlapdRollingReplace` toward the plain-tag images, assert the per-ordinal
  convergence gates hold while the mesh is mixed, and end all-2.7 with the data
  intact — witnessed by an entry written before the roll, the pattern
  `dataloss_recovery_test` uses. Test Discipline applies to that implementation,
  not to this ADR.
- Open question for the implementation: writing the new images onto the
  SlapdCluster spec updates the StatefulSet's pod template, and the StatefulSet's
  own `RollingUpdate` strategy will then start moving pods onto the new image
  ahead of their turn — onto volumes the new slapd cannot open, for a
  version-crossing roll. The machine therefore needs to own the rollout order
  (`updateStrategy.partition`, `OnDelete`, or an equivalent), not merely the
  PVC deletions. We have not decided which; whoever implements this decides it
  first and amends this ADR.
- Implementation is deliberately scheduled after v0.1.0. Until it lands, the
  supported 2.6 → 2.7 path is the ADR-014 runbook in
  `docs/OPENLDAP-VERSIONS.md`, unchanged.
- The BACKLOG observability item stands on its own: an explicit replacement
  request does not stop anyone from patching `spec.images` across a version
  boundary by hand and getting an unexplained crashloop. Surfacing the slapd
  startup error into a SlapdCluster condition is still worth doing, and is now
  also the place to point that user at this operation.

## Related

- ADR-012 — seed is one-shot; case 2 (pod loses its volumes, syncrepl restores
  the DIT) is the primitive this ADR turns into an operation.
- ADR-014 (+ the 2026-06-09 `SlapdRestore` amendment) — the imperative one-shot
  request pattern this API copies, and the dump-and-reload alternative that
  remains the fallback.
- ADR-016 (amendment 2026-08-26) — pod replacement invalidates every peer's
  syncrepl addresses; the measured 130–268 s recovery behind the settle gate.
- ADR-018 — a pod object is a PVC deletion lease; why the PVCs are deleted before
  the pod and why a leaked Job pod can stall a step.
- ADR-021 — dual images, the LMDB 1.0 break, ITS#9580 and the mixed-mesh gap.
- ADR-022 — the syncprov sessionlog, and why it does not help a from-empty
  refresh.
- `docs/OPENLDAP-VERSIONS.md` — the manual runbook this operation is the
  alternative to.
- `docs/BACKLOG.md` — "A version-crossing image change on a populated cluster
  fails without explanation"; the free-form-tag constraint that rules out
  inferring this operation.
