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
| `spec.replication.serverIDBase` | the site's `serverIDIndex × 100` |
| `spec.replication.externalPeers` | every other site the cluster spans |
| `spec.replication.network` | `mesh.network` |
| trust wiring (CA mounts) | each site's `caSecretName` |

So the chart has **no values for them**, and setting them by hand alongside
`meshRef` is refused by the operator outright: the cluster reports
`MeshResolved=False` and reconciles nothing. That refusal is deliberate.
Ambiguity about `serverIDBase` is how a serverID collision gets in quietly, and
slapd validates nothing — the only symptom is that some writes never propagate.

## serverIDIndex

Each site declares a `serverIDIndex`. The site's serverID decade is
`index × 100`, and ADR-017's `serverIDBase + ordinal + 1` then gives each pod
its own: index 2, pod 3 → serverID 203, which is how a CSN is read by eye.

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
- no two databases share a `ridBase` (ADR-003)

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
