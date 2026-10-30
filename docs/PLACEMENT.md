# Pod Placement

Where a cluster's slapd pods land, who decides it, and the one thing slaptain
decides for you.

The design record is [ADR-043](adrs/adr-043-pod-placement.md); this is the
user-facing half.

---

## Read this first: the first scheduling decision is the only one

slapd's PVCs are `ReadWriteOnce`, and in every environment this project is run
in they are backed by local storage with `volumeBindingMode:
WaitForFirstConsumer`. The volume binds wherever the pod first lands, and from
that moment the PV's node affinity pins that pod to that node for the life of
the PVC.

So the usual answer — *scheduling is the cluster administrator's business, not
the workload's* — is weaker here than it is almost anywhere else. A bad spread
is not something the next reschedule corrects. Correcting it means deleting
that pod's PVCs and rebuilding the pod from replication, which is
[ADR-012](adrs/adr-012-seed-and-lifecycle.md) case 2 with everything that comes
with it.

That is why slaptain generates a default rather than leaving the field empty,
and it is why the default is deliberately soft.

## What you have to decide

Most deployments decide nothing: leave `spec.placement` unset and read the next
section to know what you got.

You have a decision to make when any of these is true:

- **Your nodes are not interchangeable.** Some carry the right disks, the right
  network, the right taint → `nodeSelector` / `nodeAffinity` / `tolerations`.
- **Your failure domain is bigger than a node** — racks, zones. The generated
  default spreads over `kubernetes.io/hostname` and knows nothing about zones;
  a zone-level constraint is yours to write, and it **replaces** the hostname
  one rather than joining it (see "Replace, never merge").
- **You want the read-write and the read-only fleet on different nodes.** That
  is not a spread constraint at all, and on most clusters it is infeasible —
  see "What a spread cannot express".
- **You want no constraint at all.** Say `topologySpreadConstraints: []`, which
  is not the same as leaving it out.

## What you get when you say nothing

One topology spread constraint per fleet, written into both pod templates:

```yaml
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: kubernetes.io/hostname
    whenUnsatisfiable: ScheduleAnyway
    labelSelector:
      matchLabels:
        app.kubernetes.io/name: slapd
        app.kubernetes.io/instance: <cluster>          # the read-write fleet
```

and the same again on the read-only StatefulSet with
`app.kubernetes.io/instance: <cluster>-readonly`.

**Two groups, not one.** Writers are spread evenly among writers and readers
among readers. A single combined group over both fleets constrains the per-node
*total* and says nothing about the mix: with three nodes, two writers and four
readers, the assignment `{W,W} {R,R} {R,R}` has totals 2/2/2 and satisfies
`maxSkew: 1` — with both writers on one node, which is the exact outcome the
feature exists to prevent.

**`ScheduleAnyway`, not `DoNotSchedule`** — and this is the storage again, not
timidity. `PodTopologySpread` and `VolumeBinding` are separate scheduler plugins
and both must pass. Once a pod's PV is bound, its node affinity leaves exactly
one node admissible; if a hard spread constraint says that node would exceed
the skew, the pod is `Pending` and **no rescheduling can resolve it**, because
the only node it may use is the one being refused. A soft constraint degrades
to a scoring preference instead, which on a healthy cluster is the same
placement and on an unhealthy one is a running pod.

Prefer `ScheduleAnyway` in anything you write too, unless your volumes can
actually move.

### What it does *not* do

**It does not improve an existing bad spread.** The release that introduces the
default rolls both fleets once — it is a pod-template change — and every pod
comes back on the node its PVC already pins it to. "We added spreading and
nothing moved" is the correct behaviour, not a defect. Rebalancing an existing
cluster is the ADR-012 case-2 rebuild, taken deliberately, one pod at a time.

**Spread is not enforced on scale-down.** That is upstream Kubernetes' own
stated limitation. Combined with the pinning above, a spread that degrades
stays degraded.

## Absent and empty are different answers

| `spec.placement.topologySpreadConstraints` | Means | Result |
|---|---|---|
| absent (or `null`) | you said nothing | the generated per-fleet constraint |
| `[]` | you said *none* | no constraint at all |
| a list | you said *this* | exactly your list |

The empty list is the only way to switch the generated default off. Without it
you would have to supply a constraint you did not want in order to displace one
you never asked for.

The Helm charts carry the distinction. `charts/slapd` and `charts/slapd-mesh`
render `topologySpreadConstraints: []` when you write `[]` and render nothing
when you leave the key out — the ordinary `{{- with … }}` idiom would have
dropped the `[]` and handed back the default, silently.

### Replace, never merge

A list you supply replaces the generated constraint entirely. It is not merged,
and that is forced by the API rather than chosen: Kubernetes rejects two
constraints on one pod sharing a `{topologyKey, whenUnsatisfiable}` pair, and
the check **ignores `labelSelector`** when deciding. A merge would therefore
make a perfectly reasonable hostname constraint of yours unschedulable, from a
value you cannot see.

The practical consequence: if you write a zone constraint and still want the
hostname one, write both — and give them different `whenUnsatisfiable` values,
or different topology keys, or you will be rejected at apply time.

## What a spread cannot express

**"The read-write fleet and the read-only fleet must not share a node" is not a
topology spread constraint.** The API distributes one selected set across
domains; it has no vocabulary for *set A and set B must not share a domain*.
The mechanism for that is `podAntiAffinity` on the read-only template selecting
read-write pods, and slaptain does not expose the pod half of the affinity
struct — it is reserved for the operator precisely so that this can be added
later without an API break (ADR-043 decision 2).

Before asking for it, check whether your cluster can host it. Even spreading
puts writers on `min(n_w, N)` distinct nodes and readers on `min(n_r, N)`, so
disjointness requires

```
N  ≥  n_w + n_r
```

nodes. A three-node site with 2 read-write and 3 read-only pods cannot do it at
all; a three-node site with `replicas: 3` cannot host a single read-only pod
disjointly. If you need the separation and your node count is short, the answer
is nodes, not configuration.

## Seeing what actually resolved

**Nothing in the `SlapdCluster` status reports what placement resolved to.**
You cannot tell from the CR whether your list replaced the default or the
default is in force. Read the StatefulSet, which is where the answer is:

```bash
kubectl -n <ns> get statefulset <cluster> \
  -o jsonpath='{.spec.template.spec.topologySpreadConstraints}' | jq .

kubectl -n <ns> get statefulset <cluster>-readonly \
  -o jsonpath='{.spec.template.spec.topologySpreadConstraints}' | jq .
```

A constraint whose `labelSelector` carries `app.kubernetes.io/instance:
<cluster>` (or `<cluster>-readonly`) and `maxSkew: 1` on
`kubernetes.io/hostname` is the generated one; anything else is yours. An empty
result means `[]` — you turned it off.

Where the pods actually went is a separate question, and the only one that
matters in the end:

```bash
kubectl -n <ns> get pods -l app.kubernetes.io/name=slapd \
  -o custom-columns=POD:.metadata.name,NODE:.spec.nodeName,FLEET:.metadata.labels.app\\.kubernetes\\.io/instance
```

With `ScheduleAnyway` the two can legitimately disagree: the constraint is a
preference, and a cluster that could not satisfy it schedules anyway and tells
you nothing. The pod list is the ground truth.

## The fields

All of them are raw Kubernetes pod-spec types, passed to **both** pod templates
verbatim. `kubectl explain slapdcluster.spec.placement` is authoritative.

| Field | Notes |
|---|---|
| `topologySpreadConstraints` | Above. `matchLabelKeys` is not something slaptain sets — it solves a rolling-update skew problem keyed on `pod-template-hash`, which belongs to Deployments; a StatefulSet pod keeps its identity and its PVC across an update. |
| `nodeSelector` | The simplest and the hardest: a selector matching no node leaves pods `Pending` with no fallback. |
| `nodeAffinity` | The **node** half of the Kubernetes affinity struct only. Unset renders no `affinity` field at all rather than an empty one, so turning other placement fields on does not roll pods for a structurally empty value. |
| `tolerations` | Removes a node's veto; it does **not** attract a pod to that node. Pair it with `nodeSelector` or `nodeAffinity` when you mean "run there, and only there". |
| `priorityClassName` | Names an existing PriorityClass — and on a mesh, one that exists at *every* site. |

`schedulerName`, `preemptionPolicy` and `schedulingGates` are not exposed.
Nothing about them is hard; nobody has needed them. Ask.

## On a mesh

`charts/slapd-mesh` applies one values file identically at every site
([ADR-028](adrs/adr-028-mesh-scoped-vs-site-scoped.md) §5), and the placement
pass-throughs are where a site can leak into it. A `nodeSelector` naming a
node, a rack, a zone or a PriorityClass that exists at one site is wrong at the
others — and nothing in the chart or the operator will say so. The sites will
simply differ, and the difference will be invisible to every parity check the
mesh has.

Select on labels every site carries. The generated default is site-safe by
construction: it names no node, only a topology key and the cluster's own fleet
labels. If one site genuinely needs its own placement, that is a per-site values
override, taken deliberately.

## Related

- [ADR-043](adrs/adr-043-pod-placement.md) — the decision, the options
  rejected, and the arithmetic above.
- [ADR-012](adrs/adr-012-seed-and-lifecycle.md) — the rebuild that correcting a
  bad spread goes through.
- [ADR-013](adrs/adr-013-defer-hot-database-management.md),
  [ADR-035](adrs/adr-035-adrs-conflict.md) — what may change a pod template,
  and when.
- [`TUNING.md`](TUNING.md) — sizing the pods you are placing.
- [`UPGRADING.md`](UPGRADING.md) — the one roll this feature costs.
