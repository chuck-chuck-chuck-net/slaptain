# slapd-mesh

One chart for a whole multi-site slaptain deployment: the `SlapdMesh`, the
`SlapdCluster` that references it, and the `SlapdDatabase` and `SlapdSchema`
objects that live on it.

**The same values file goes to every site, unchanged.** That is the point of
the chart, not a convenience — see [Why identical](#why-identical) below.

```bash
# At EVERY site, from the same values file:
helm upgrade --install ldap ./charts/slapd-mesh \
  -n slaptain --create-namespace \
  -f my-mesh-values.yaml
```

The single fact that differs per site is the operator's own identity, and it
lives on the **operator's** chart, never here:

```bash
# Site A:
helm upgrade --install slaptain ./charts/operator --set siteName=site-a
# Site B:
helm upgrade --install slaptain ./charts/operator --set siteName=site-b
```

`charts/slapd-mesh/examples/three-site.yaml` is a complete working example.

## What it renders

| Object | Name from | Notes |
|---|---|---|
| `SlapdMesh` | `mesh.name` | sites, network, trust |
| `SlapdCluster` | `cluster.name` | carries `meshRef`, and an optional `sites` subset selector |
| `SlapdDatabase` × N | `databases[].name` | `spec` passed through verbatim; `clusterRef` pinned by the chart |
| `SlapdSchema` × N | `schemas[].name` | same |

No Secrets. No generated passwords. Nothing derived from the Helm release name.

## Why identical

Every object here is *mesh-scoped*: it has one value that is true at all sites
simultaneously (ADR-028 §3). Keeping the rendered manifest byte-identical
everywhere is what turns cross-site parity into a check anyone can run —
"does it exist, and does it hash the same" — instead of a field-by-field
comparison somebody eventually gets wrong.

Two rules follow, and both are enforced by construction rather than by
convention:

1. **No per-site values.** If you find yourself wanting a value to differ
   between sites, the design has been violated, not the chart. The one genuine
   per-site fact — "which of these sites am I?" — is the operator's `siteName`.

2. **Nothing random, nothing release-derived.** Object names come from
   `.Values`, never from `.Release.Name`, and the chart creates no Secret. The
   replication password in particular must be identical mesh-wide (ADR-008); a
   chart that generated one would hand every site a different secret and the
   breakage would present as an auth bug. Secrets and CAs are bootstrapped out
   of band and referenced by name (ADR-028 §7).

You can check the property yourself:

```bash
helm template a ./charts/slapd-mesh -n ns-one -f my-mesh-values.yaml | sha256sum
helm template b ./charts/slapd-mesh -n ns-two -f my-mesh-values.yaml | sha256sum
```

The two hashes must match. If they ever diverge, something per-install has
crept into the chart.

## What the operator derives, and what you therefore cannot set

With `meshRef` set — which this chart always does — the operator derives four
things from the mesh plus its own `siteName` (ADR-028 §4):

| Derived | From |
|---|---|
| `spec.replication.serverIDBase` | the site's `serverIDIndex × 16` |
| `spec.replication.externalPeers` | every other site the cluster spans |
| `spec.replication.network` | `mesh.network` |
| trust wiring (CA mounts) | each site's `caSecretName` |

So the chart has **no values for them**, and setting them by hand alongside
`meshRef` is refused by the operator outright: the cluster reports
`MeshResolved=False` and reconciles nothing. That refusal is deliberate.
Ambiguity about `serverIDBase` is how a serverID collision gets in quietly, and
slapd validates nothing — the only symptom is that some writes never propagate.

## serverIDIndex

Each site declares a `serverIDIndex`. The site's `serverIDBase` is
`index × 16`, and ADR-017's `serverIDBase + ordinal + 1` then gives each pod its
own: index 2, pod ordinal 3 → serverID 36.

The stride is 16 so that the number is readable where it is actually seen. A
CSN renders the serverID field as three hex digits, so sid 36 appears as `024`
— site digit, then pod digit. Under the old decimal stride of 100 the values
everyone reasoned in were not the values anyone could read: site 1 pod 0 was
101, which a CSN shows as `065`.

It is **required and never inferred from list position**. `olcServerID` is baked
into every CSN a pod has ever written, so a site whose number moves files its
future writes under a different sid from its history. If the number came from
the position, inserting a site in the middle — or letting a formatter sort the
list — would silently renumber every site after it.

Indices must be unique, are bounded 0..40, and gaps are legal: retiring a
decommissioned site's index is how you remove a site without renumbering its
neighbours.

## seed.site

Exactly one site creates a database's initial entries; the others receive them
by replication. Name the founder:

```yaml
databases:
  - name: example-db
    spec:
      seed:
        site: site-a
        entries: [...]
```

Every site reads this same value, and every site but `site-a` finds it is not
its own identity and withholds (ADR-025, ADR-028 §3).

On a mesh with more than one site, **the chart refuses to render a seeded
database that does not name a site.** Without it, every site seeds, and one pod
is left with a permanent hidden glue suffix: invisible to ordinary searches,
clean on every CSN health read, and every backup taken from that pod
unrestorable. It is the exact failure this packaging exists to make unreachable,
so it is a render-time error rather than a runtime surprise.

## What the chart checks before it renders

All of it decidable from the values alone, so the verdict is the same at every
site:

- `mesh.sites` is non-empty; every site has a name and a `serverIDIndex`
- no duplicate site name, no duplicate `serverIDIndex`
- `cluster.sites` (the subset selector) names only sites the mesh declares
- every database has a name and a suffix
- a seeded database on a multi-site mesh names a `seed.site`, and that site exists
- no two databases share a `ridSegment` (ADR-031)

## Monitoring (ADR-033 Surface 3)

The slapd exporter sidecar — one container next to every slapd pod, serving
slapd's `cn=Monitor` as `openldap_*` metrics on `:9330/metrics` — is switched on
here like any other `SlapdCluster` field. Off unless you turn it on; the full
story is `docs/MONITORING.md`.

| Value | Default | Renders | Notes |
|---|---|---|---|
| `cluster.monitoring.enabled` | unset | `spec.monitoring.enabled` | `true` adds the sidecar. It changes the pod template, so every site's pods roll once. |
| `cluster.monitoring.resources` | unset | `spec.monitoring.resources` | The sidecar's own requests/limits, not `spec.resources`. No default. |
| `cluster.images.exporter.{repository,tag,pullPolicy}` | unset | `spec.images.exporter` | Empty: `slapd-exporter` beside the operator's image, at the exporter release compiled into the operator. A tag alone keeps that repository. |

`cluster.monitoring` and `cluster.images` are passed through verbatim when set.

Both are in the shared values file, so they are **mesh-wide by construction**:
one value turns the sidecar on at every site. That is harmless at a site with no
Prometheus — the sidecar runs and nothing scrapes it. Whether a site *collects*
the metrics is per-site and lives on that site's operator chart,
`slapdPodMonitor.enabled`. It cannot live here: a `PodMonitor` would fail this
chart's identical install at any site without the Prometheus Operator CRDs.

Leave `cluster.images.exporter` empty too, as this chart does for `slapd` and
`init`. The exporter is its own image and version line (`docs/VERSIONING.md`),
and the operator defaults it to the exporter release it was built against, so
an operator upgrade changes the sidecar only when that release moved. It is not
pinned in lockstep with `slapd` and `init`; pin it only to run a different
exporter version.

## Placement (ADR-043)

Where each site's pods are allowed to land. Every key is a raw Kubernetes
pod-spec field, rendered verbatim into `spec.placement` and applied by the
operator to **both** pod templates, read-write and read-only.

Unset is the normal setting, and it is not "no constraint": the operator
generates one spread constraint per fleet — `maxSkew: 1` over
`kubernetes.io/hostname`, `whenUnsatisfiable: ScheduleAnyway`, selecting that
fleet of that site's cluster alone. Writers spread among writers and readers
among readers. Reasoning and sizing: `docs/PLACEMENT.md`.

| Value | Default | Renders | Notes |
|---|---|---|---|
| `cluster.placement.topologySpreadConstraints` | unset | `spec.placement.topologySpreadConstraints` | A list **replaces** the generated default; it is never merged. An explicit `[]` is the only way to ask for no constraint at all, and the chart tells it apart from unset. Prefer `ScheduleAnyway`: `DoNotSchedule` can strand a pod permanently once its PV is bound. |
| `cluster.placement.nodeSelector` | unset | `spec.placement.nodeSelector` | Labels every node must carry. **Site-specific by nature — see below.** |
| `cluster.placement.nodeAffinity` | unset | `spec.placement.nodeAffinity` | The **node** half of the affinity struct only; the pod half is reserved for the operator. **Site-specific by nature — see below.** |
| `cluster.placement.tolerations` | unset | `spec.placement.tolerations` | Removes a node's veto; does not attract a pod there. |
| `cluster.placement.priorityClassName` | unset | `spec.placement.priorityClassName` | Names an existing PriorityClass — which must exist at every site. |

**This file is applied identically at every site**, which is the point of the
chart rather than a convenience (ADR-028 §5), and the placement pass-throughs
are the one place in this block where a site can leak in. A
`nodeSelector: {kubernetes.io/hostname: node-a}`, a zone, a rack or a
PriorityClass that exists at one site is wrong at the others — and nothing here
or in the operator will say so: the sites will simply differ. Select on labels
every site carries. If one site genuinely needs its own placement, that is a
per-site values override, taken deliberately.

The generated default is site-safe by construction: it names no node, only
`kubernetes.io/hostname` as a topology key and the cluster's own fleet labels,
which every site has.

**The release that introduces this rolls both fleets once at every site, and
nothing moves** — each pod returns to the node its PVC already pins it to. See
`docs/UPGRADING.md`.

## What it does not do

- **It does not bootstrap trust.** Cross-site trust is pairwise and circular —
  site A needs site B's CA before site B exists — and no declarative system
  establishes that without a management cluster. Bootstrap (CAs, kubeconfigs,
  the shared replication password) is a one-off per site pair; cert-manager /
  trust-manager and External Secrets or SOPS are the intended tools
  (ADR-028 §7). This chart references the resulting Secrets by name.
- **It does not check parity.** Coherence is established at apply time and
  verified afterwards; a fused single CR would have made it a property of etcd,
  and §5 knowingly gives that up in exchange for keeping the data model layered.
- **It does not install the operator or the CRDs.** Install `charts/operator`
  first, at every site, each with its own `siteName`.

## Related

- `docs/adrs/adr-028-mesh-scoped-vs-site-scoped.md` — the layering, §3 the
  no-per-site-fields rule, §5 why this is a chart
- `docs/adrs/adr-025-single-creator-seed-glue-suffix.md` — why `seed.site` exists
- `docs/adrs/adr-017-bare-integer-serverid.md` — the serverID arithmetic
- `docs/adrs/adr-016-pod-routed-cross-cluster-replication.md`,
  `docs/adrs/adr-007-multus-replication-network.md` — the two network modes
- `charts/slapd` — a single-site `SlapdCluster` without the mesh layer
