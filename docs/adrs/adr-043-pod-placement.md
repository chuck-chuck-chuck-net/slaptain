# ADR-043: Placement is the user's, with one default the user does not have to know about

**Status:** Accepted
**Date:** 2026-10-29

## Context

A three-node site was measured with two of three read-write pods on one node
and the read-only replica sharing a node with a writer (`docs/BACKLOG.md`,
"slapd pods cannot be spread across nodes"). Losing one node took two thirds of
the write fleet; losing the other took a writer and the entire RO fleet.
Nothing about that was chosen — the scheduler had no reason to prefer
otherwise, because **the operator exposes no placement control whatsoever**:
`SlapdCluster` has no `affinity`, no `tolerations`, no `nodeSelector`, no
`topologySpreadConstraints`, and the pod-template builder sets none.

**Why this is sharper here than for a typical workload.** The PVCs are RWO and,
in every environment this project runs in, backed by local storage with
`WaitForFirstConsumer`. The volume binds wherever the pod first lands and the
pod is pinned to that node for the life of the PVC. A bad spread is therefore
**permanent, not something the next reschedule fixes** — correcting it means
deleting a pod's PVCs and rebuilding from replication, which is ADR-012 case 2
with everything that comes with it. The usual answer — scheduling is the
cluster operator's business, not the workload's — is weaker than normal: the
first scheduling decision is the only one, and it is made before anyone looks.

### What the API offers, and what is current

Read from `k8s.io/api@v0.37.1`, the version the operator compiles against, in
preference to the documentation, which lags it. Nine `PodSpec` fields bear on
placement: `nodeSelector`, `affinity.{node,pod,podAnti}Affinity`,
`topologySpreadConstraints`, `tolerations`, `priorityClassName`,
`preemptionPolicy`, `schedulerName`, `schedulingGates` and `nodeName`.

**Nothing in that set is deprecated.** A grep of `core/v1` for `Deprecated`
against these fields returns no hits; they coexist by design. Within
`topologySpreadConstraints` every field is GA at 1.37 except `matchLabelKeys`,
which the type itself documents as *"a beta field … MatchLabelKeysInPodTopologySpread
feature gate (enabled by default)"*. In `PodAffinityTerm`, `matchLabelKeys`
and `mismatchLabelKeys` are GA.

Two of the nine are not placement in the sense this ADR means: `tolerations`
removes a veto rather than choosing a node, and `schedulingGates` decides
*when* scheduling may start. `nodeName` bypasses the scheduler and is never a
legitimate operator API.

## Options considered

### 1. Pass-through only, no default (Strimzi's shape)

Expose the raw k8s types and generate nothing. Cheap, unopinionated, and the
precedent is already here — `spec.resources`, `spec.securityContext` and
`spec.imagePullSecrets` are exactly this.

**Rejected as the whole answer**, not as a part of it: it solves nothing for
anyone who does not know to set it, and the deployment that was measured badly
spread was run by people who know this system well.

### 2. A generated pod anti-affinity (CloudNativePG's shape)

CNPG defaults `enablePodAntiAffinity` to true with `podAntiAffinityType:
preferred`.

**Rejected** because anti-affinity has no skew budget. `required` on
`kubernetes.io/hostname` means *at most one pod per node, full stop* — three
replicas on a two-node cluster stay Pending forever, and a three-node lab
running `replicas: 3` plus RO pods is a normal shape here. `preferred` is a
scoring weight with no evenness guarantee at all. `topologySpreadConstraints`
exists because those are the only two settings anti-affinity has.

### 3. One combined spread group across both fleets

A single constraint, carried by both pod templates, selecting every pod of the
cluster.

**Rejected, and it is the option that looks most attractive.** It constrains
the TOTAL per node and says nothing about the mix. Counter-example with `N=3`
nodes, 2 RW and 4 RO pods: totals of 2/2/2 satisfy `maxSkew: 1`, and the
assignment `{W,W} {R,R} {R,R}` is legal. **Both writers on one node** — the
exact catastrophe the feature exists to prevent. A combined group is not a
different flavour of the same protection; it is strictly weaker than option 4
for availability.

### 4. Two spread groups — one constraint per fleet

The RW pod template carries a constraint selecting RW pods; the RO template one
selecting RO pods. Writers land `⌊n_w/N⌋`–`⌈n_w/N⌉` per node and readers
likewise: **both fleets provably even.** Chosen; see below.

### 5. Options 3 and 4 together, user's choice

Attractive, and **the API forbids the obvious form of it.**
`ValidateSpreadConstraintNotRepeat` rejects two constraints on one pod sharing
a `{topologyKey, whenUnsatisfiable}` tuple, and it compares **only** those two
— `labelSelector` is not considered. Since `kubernetes.io/hostname` is the only
topology key that means anything to a single site, one pod can carry at most
one hostname constraint per `whenUnsatisfiable` value.

So "both at once" is legal only by spending the `whenUnsatisfiable` axis — one
group on `ScheduleAnyway`, the other on `DoNotSchedule`. And `DoNotSchedule` is
precisely the value our storage makes dangerous (see Consequences). Rejected as
a *generated* default; a user who wants it can still write it, which is what
option 1 is for.

## Decision

1. **`spec.placement`**, a block of its own, carrying
   `topologySpreadConstraints`, `nodeSelector`, `nodeAffinity`, `tolerations`
   and `priorityClassName`, each the raw Kubernetes type, applied to both pod
   templates.

   Not CNPG's layout, deliberately: they put most knobs under `spec.affinity.*`
   and `topologySpreadConstraints` at the top level, because spread constraints
   are not affinity and the block was already named. They are stuck with it; we
   are not. One block, honestly named for what it contains.

2. **`nodeAffinity`, not the whole `affinity` struct.** The node half is the
   user's; **the pod half is reserved for the operator.** We generate no pod
   affinity today, so exposing the whole struct would be safe now and more
   capable — but the cross-fleet separation this ADR defers is *operator-
   generated pod anti-affinity*, and CNPG narrows for exactly that reason. The
   alternative costs an API break later or a fragile merge of user-supplied and
   generated terms. Giving away the node half costs nothing.

3. **A default spread, generated when `spec.placement` says nothing about it:**
   one constraint per pod template, `maxSkew: 1`,
   `topologyKey: kubernetes.io/hostname`, **`whenUnsatisfiable: ScheduleAnyway`**,
   `labelSelector` selecting that fleet of that cluster. A user-supplied
   `topologySpreadConstraints` replaces it entirely rather than merging — a
   merge would reintroduce the duplicate-tuple rejection of option 5, from a
   value the user cannot see.

4. **`ScheduleAnyway`, not `DoNotSchedule`**, for the storage reason below.

5. **No cross-fleet separation in this ADR.** It is a different mechanism, not
   a spread mode, and it is infeasible on the clusters we run.

6. **`matchLabelKeys` out of scope.** It is the one non-GA field, and its
   purpose is the Deployment rolling-update skew problem keyed on
   `pod-template-hash`. StatefulSet pods keep their identity and their PVC
   across an update, so it buys us much less than it buys a Deployment.

7. **`schedulerName` and pod-level labels/annotations are not in this ADR.**
   The first is niche; the second is metadata rather than placement and
   collides with the operator's cache label scoping. Both are cheap to add
   later.

## Consequences

**`DoNotSchedule` can deadlock a pod permanently, and only here.** *Reasoned
from the scheduler's structure; not measured.* `PodTopologySpread` and
`VolumeBinding` are separate plugins and both must pass. Once a PV is bound its
node affinity forces the pod to one node; if a hard spread constraint says that
node would exceed the skew, the pod is Pending with no reschedule that can fix
it. This is why the generated default is `ScheduleAnyway`, and the field
documentation must say so where a user choosing `DoNotSchedule` will read it.

**Spread is not enforced on scale-down** (upstream's own stated limitation).
Combined with the pinning above, a spread that degrades stays degraded.

**Cross-fleet separation is not expressible in `topologySpreadConstraints` at
all.** The API distributes one selected set across domains; it has no
vocabulary for "set A and set B must not share a domain". That is
`podAntiAffinity` on the RO template selecting RW pods — and it is feasible
only when `N ≥ n_w + n_r`, because even spreading puts writers on `min(n_w, N)`
distinct nodes and readers on `min(n_r, N)`, and disjointness demands the sum.
On a three-node site with 2 RW and 3 RO it is impossible. Recorded so that
nobody later tries to express it as a spread.

**A combined group would need a new per-cluster label, and it is not the one
the cache work adds.** ADR-042's `managedLabels()` is
`app.kubernetes.io/name: slapd` — constant across every cluster, correct for
scoping an informer and wrong for scoping a spread, because a constraint keyed
on it would spread every SlapdCluster in the namespace against every other.
Two-group mode needs no new label: `app.kubernetes.io/instance` is already
`<cluster>` and `<cluster>-readonly`, which is per-cluster *and* per-fleet.

**One roll, accepted.** The default lands on both pod templates, so both fleets
roll once on the upgrade that introduces it. ADR-035 permits an operator
upgrade to change the template once; ADR-013 forbids side-effect rolls, and
this is neither — it is the release doing it deliberately, stated in
`UPGRADING.md`. **The roll does not improve an existing bad spread**: each pod
returns to the node its PVC already pins it to. "We added spreading and nothing
moved" is the correct behaviour and must be written down, or it reads as a
defect.

**Unset is byte-identical except for the one generated constraint.** Everything
else in `spec.placement` renders nothing when absent, as `spec.resources` and
`spec.imagePullSecrets` already do.

## Amendment, 2026-10-29: absent and empty are different answers

Decision 3 said the default is generated "when `spec.placement` says nothing
about it" and left the empty list unconsidered. The implementation had to
decide, and chose:

- **`topologySpreadConstraints` absent (nil)** — said nothing. The default is
  generated.
- **`topologySpreadConstraints: []`** — said *none*. No constraint is written,
  and this is the only way to switch the generated default off.

Accepted, and it is the honest reading of "replace, never merge": an explicit
empty list *is* a statement about spread constraints, and without it a user who
wants no constraint at all has no way to say so — they would have to supply a
constraint they did not want in order to displace one they did not ask for.

It is nearly free, but not free, and the first version of this paragraph got
the price wrong. It said JSON `[]` decodes to a non-nil zero-length slice and
an absent key decodes to nil, so the two are "already distinguishable after
decoding" — true, and **not sufficient**. Decoding keeps the distinction;
**encoding loses it**. `omitempty` drops a zero-length slice exactly as
readily as a nil one, so with a plain `[]T` any client doing Get → modify →
Update marshals the user's `[]` away and the generated default silently comes
back. The user-facing paths — `kubectl`, Helm, a YAML apply — send the bytes
as written and never saw it; a typed Go client could not express the opt-out
at all.

So the field is **`*[]corev1.TopologySpreadConstraint`**: a nil pointer is
omitted, a pointer to an empty slice marshals as `[]`, and the distinction
survives the round trip rather than surviving by luck of the path taken. Same
reasoning and the same shape as `SlapdBackup.podAffinity`'s `*bool`, which
carries the identical note for the identical reason. The predicate is a
nil-**pointer** check, never `len() > 0`. The rendered CRD schema is
unchanged — `*[]T` and `[]T` produce the same OpenAPI — so this is a Go-side
correction with no stored-object migration.

Both halves are pinned by tests. The decode/predicate half was
mutation-checked by swapping the predicate to `len() > 0` and observing
exactly that case go red (and again, later, against a live three-pod cluster
through the e2e). The encode half is a marshal/unmarshal round trip observed
red before the pointer landed, with the empty list arriving as `{}` on the
wire.

One smaller implementation fact worth recording, not a design change: an unset
`nodeAffinity` renders no `affinity` field at all rather than an empty one —
without which every existing pod would roll for a structurally empty value.
(`spec.placement` itself stays a value struct; the nil/empty distinction it
needs lives on the slice, which now has its own pointer.)

### Still open, deliberately

**Nothing in the CR reports what placement resolved to.** A user cannot tell
from `SlapdCluster` whether their list replaced the default or the default is
in force; they have to read the StatefulSet. Noted during implementation, not
built, and recorded here rather than in the code so it is costed as a design
question — a status field is persisted state, and this ADR has not argued that
it earns itself.

## Related

- `docs/BACKLOG.md`, "slapd pods cannot be spread across nodes" — the
  measurement that prompted this, and the two shapes it sketched.
- ADR-012 — the PVC-loss rebuild a bad spread has to be corrected through.
- ADR-013, ADR-035 — what may change a pod template, and when.
- ADR-018 — `SlapdBackup.podAffinity`, which pins a Job to its target's node:
  the only affinity in the API today, and it does the opposite job.
- ADR-042 — the cache label, which is not this label.
