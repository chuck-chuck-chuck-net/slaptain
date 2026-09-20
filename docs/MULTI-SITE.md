# Multi-Site Deployments

How to run one LDAP directory across several Kubernetes clusters, and how to
tell whether it is healthy.

The design record is
[ADR-028](adrs/adr-028-mesh-scoped-vs-site-scoped.md); this is the user-facing
half. The chart reference is [`charts/slapd-mesh/README.md`](../charts/slapd-mesh/README.md).

---

## The model

A **mesh** is a set of **sites**. A site is one Kubernetes cluster carrying
slapd pods. The sites share no API server and no control plane — that is the
architecture, not a limitation to work around — and they exchange LDAP changes
directly with each other over delta-syncrepl.

Four layers, each referencing the one above:

```
SlapdMesh  ←── meshRef ──  SlapdCluster  ←── clusterRef ──  SlapdDatabase
                                          ←── clusterRef ──  SlapdSchema
```

| Layer | Describes | One per |
|---|---|---|
| `SlapdMesh` | the sites, the network between them, the trust | deployment (shared by every site) |
| `SlapdCluster` | the slapd StatefulSet at *this* site: replicas, storage, TLS | mesh, usually |
| `SlapdDatabase` | one data tree: suffix, ACLs, indices, seed, replication | database |
| `SlapdSchema` | schema definitions | schema set |

A `SlapdCluster` may span a **subset** of the mesh's sites (`spec.sites`), so
cluster X can run over sites A+B while cluster Y runs over A+B+C on the same
mesh.

### Mesh-scoped vs site-scoped

This is the rule the whole design turns on:

> **A mesh-scoped resource carries no per-site fields.**

All four kinds above are mesh-scoped. Every one of them must be
**byte-identical at every site**. Any per-site behaviour is expressed by
*naming* a site inside an otherwise identical object — never by editing the
object at one site.

`spec.seed` is the illustrative case. Exactly one site may create a database's
initial entries; the rest must receive them by replication. That is not
expressed by deleting the seed block at the other sites (which is how it used
to be done, and is how you get the silent corruption described under
[Seeding](#seeding-exactly-one-founder)) but by naming the founder:

```yaml
seed:
  site: site-a
  entries: [...]
```

Every site reads that same value, and every site but `site-a` finds it is not
itself and withholds.

The one genuinely per-site fact in the system is **"which of these sites am
I?"**, and it lives outside every CR — in the operator's own installation
config. See [Site identity](#site-identity-the-one-per-site-fact).

### What follows from it

Because the objects are identical everywhere, cross-site parity becomes a
question anyone can answer: *does it exist at this site, and does it hash the
same?* No field-by-field comparison to get wrong.

And because the operator knows the mesh and its own identity, it **derives**
the wiring rather than making you write it:

| Derived | From |
|---|---|
| `spec.replication.serverIDBase` | the site's `serverIDIndex × 100` |
| `spec.replication.externalPeers` | every other site the cluster spans |
| `spec.replication.network` | `mesh.network` |
| trust wiring (peer CA mounts) | each site's `caSecretName` |

Setting any of those by hand *alongside* `meshRef` is refused outright: the
cluster reports `MeshResolved=False` and reconciles nothing. That refusal is
deliberate — an ambiguous `serverIDBase` is how a serverID collision gets in
quietly, and slapd validates nothing there.

---

## The invariants

A multi-site directory works only while the sites agree on a set of facts.
Here they are, with who enforces each. The first four are **constructed** —
derived by the operator, so they cannot be wrong. The rest are **detected** —
properties of the artifact you apply, checked by hash and by tooling.

| Fact | Rule across sites | How it is held |
|---|---|---|
| `serverIDBase` | **differs** — one decade per site | derived from `serverIDIndex` |
| `externalPeers` | N×(N−1), full mesh | derived from `sites[]` |
| network mode | identical | derived from `mesh.network` |
| trust wiring (CA mounts) | N×(N−1) | derived from each site's `caSecretName` |
| `SlapdDatabase` name and `suffix` | identical | one chart, one values file |
| `spec.replication.ridBase` | identical per database across sites | one chart, one values file |
| `spec.replication.ridBase` | unique *between* databases within a site | chart render-time check |
| `<db>-credentials` / `replication-password` | identical everywhere | provisioned out of band; the chart never generates one |
| `SlapdSchema` set | identical, or a superset everywhere | one chart, one values file |
| `spec.seed` | exactly one site carries it | `seed.site` + the operator's evidence belt |

Two failure modes are worth stating plainly, because "the resource is simply
missing at one site" understates both:

- **A missing `SlapdDatabase` is not an absence, it is a broken link.** The
  other sites' syncrepl stanzas name that suffix; the search returns
  `noSuchObject`; a failed logbase search **halts** delta-syncrepl rather than
  degrading it. The link stops.
- **A missing `SlapdSchema` diverges the directory.** Entries replicate in
  carrying an objectClass that site does not know, the add fails, and that site
  silently lacks entries the others have.

Both are reasons to apply one chart everywhere rather than maintain per-site
manifests.

---

## Getting started

### 0. Prerequisites

- One Kubernetes cluster per site, each reachable with its own `kubectl`
  context.
- A network path between sites for replication — either natively routed pod
  CIDRs (`pod-routed`, the default) or a dedicated Multus network
  (`multus`). See [Networking](#networking).
- Each site's Kubernetes API reachable from the other sites over that same
  network: peer discovery reads the remote API to find pod addresses.

### 1. Name your sites

Pick logical, stable names — `site-a`, `ams`, `fra`. They are matched
**verbatim**: trimmed of whitespace, never case-folded, so `site-a` and
`SITE-A` are two different sites.

Assign each a `serverIDIndex`. The decade is `index × 100`, and each pod then
gets `serverIDBase + ordinal + 1`, so index 2, pod 3 reads as serverID `203` in
a CSN — which is how these are actually debugged.

Indices must be **unique**, are bounded `0..40`, and gaps are legal. Retiring a
decommissioned site's index, rather than reusing it, is how you remove a site
without renumbering its neighbours. The index is declared and never inferred
from list position, because `olcServerID` is baked into every CSN a pod has
ever written: a site whose number moves files its future writes under a
different sid from its history.

### 2. Bootstrap trust and shared secrets (once per site pair)

Cross-site trust is pairwise and circular — site A needs site B's CA before
site B exists — so it is an imperative, one-off step, not something the
reconcile loop can establish. The operator picks up the results by name.

At every site you need:

| Secret | Contents | Default name the operator looks for |
|---|---|---|
| this site's server cert | `tls.crt`, `tls.key`, optional `ca.crt` | whatever `cluster.ldap.tls.secretName` says |
| every **other** site's CA | `ca.crt` | `<site>-ca` |
| every **other** site's kubeconfig | `kubeconfig` | `<site>-kubeconfig` |
| each database's credentials | `root-password`, `replication-password` | `<database>-credentials` |

Both defaults are overridable per site (`caSecretName`, `kubeconfigSecret`) for
when something else owns the naming — trust-manager writes a Bundle where its
own resource says, External Secrets likewise.

The `<database>-credentials` Secret must be **identical at every site**: the
replication bind password is one value mesh-wide. Create it **before** the
`SlapdDatabase`, or the operator generates a different random one per site and
every cross-site bind fails with `err=49`.

### From a naked lab to a replicating mesh

Six commands, in this order. Each is re-runnable and each takes its site list
from `lab.yaml`, so nothing is typed twice. Every script has `--dry-run`.

```bash
# 1. Describe the lab once: sites, serverIDIndex, contexts, endpoints.
#    See lab.yaml.sample. Nothing below asks you for a site again.
$EDITOR lab.yaml

# 2. Credentials — per database, IDENTICAL at every site (ADR-008).
./scripts/mesh-share-credentials.sh -f values.directory.yaml -n slaptain

# 3+4. A certificate per site, then every site's CA to all the others.
./scripts/mesh-establish-trust.sh -n slaptain --cluster slapd

# 5. RBAC + one kubeconfig Secret per ordered site pair, for peer discovery.
./scripts/mesh-authorize-peers.sh -n slaptain

# 6. The operator, per site. The --set is the ONLY per-site argument anywhere.
for s in site-1 site-2 site-3; do
  helm --kube-context "$s" upgrade --install slaptain \
    oci://ghcr.io/chuck-chuck-chuck-net/charts/slaptain \
    -n slaptain-system --create-namespace --set "siteName=$s"
done

# 7. The mesh itself: topology generated from lab.yaml, directory hand-written.
./scripts/mesh-derive-topology.sh > values.topology.yaml
for s in site-1 site-2 site-3; do
  helm --kube-context "$s" upgrade --install ldap \
    oci://ghcr.io/chuck-chuck-chuck-net/charts/slapd-mesh \
    -n slaptain --create-namespace \
    -f values.topology.yaml -f values.directory.yaml
done
```

Then `slctl inspect -n slaptain slapd` at each site. `MeshResolved=True` means the
operator found its mesh and derived its wiring; `ReplicationConverged=True` means
the databases agree.

**Why the order.** Steps 2-5 are the imperative bootstrap: material the operator
needs to already exist. Step 7 is the declarative steady state. Within the
bootstrap, only one ordering is forced — CA distribution cannot run before every
site has a certificate, which is why `mesh-establish-trust.sh` does both and you do not
run them separately.

**Why credentials and certificates are separate scripts**, when both merely
create Secrets: the rules are opposite. A replication password must be
*identical* at every site, because the legacy `cn=replication,<suffix>` identity
lives inside the replicated tree and only one password can match. A certificate
must *differ* per site, because it carries that site's names. `mesh-share-credentials.sh`
therefore adopts whatever the mesh already uses rather than generating per site —
get that wrong and replication fails with `err=49`, an authentication error that
sends you looking at TLS and firewalls.

**What is not automated**, and is not an oversight: the directory values file.
The cluster shape, the databases and the schemas are choices, not facts about the
lab, so no script can derive them. That file is yours.

`scripts/mesh-authorize-peers.sh` provisions the RBAC and the kubeconfig
Secrets for a set of sites. **Run it with no arguments**: it reads `lab.yaml`
(`$E2E_CONFIG`, else the repo root, else `--from-lab FILE`) and takes each
site's `name`, its `context`, and its `endpoint` — the API address reachable
*from pods at the other sites*, which on dual-homed nodes is not the address in
your kubeconfig. The Secrets are then named `<site>-kubeconfig`, exactly what
the operator derives from the mesh, so nothing needs pinning.

Naming sites explicitly still works — `CONTEXT=API_URL` pairs override the file
— but with no site name available the Secrets are named after the **kubectl
context**, and you must then set `kubeconfigSecret.name` on the mesh site entry
to whatever came out.

**Read it before you run it.** `--dry-run` prints every object it would create,
grouped by the cluster it would land on, and contacts nothing:

```bash
./scripts/mesh-authorize-peers.sh --dry-run -n slaptain \
    site-1=https://api.site-1.k8s.example:6443 \
    site-2=https://api.site-2.k8s.example:6443
```

Manifests go to stdout and the progress log to stderr, so it redirects like
`helm template`. Per site you get a Namespace, a ServiceAccount, a Role granting
`get`/`list` on pods **and nothing else**, and a long-lived token Secret; then
one kubeconfig Secret per ordered pair — N×(N−1), because every site needs its
own credential for every other. Each kubeconfig Secret is followed by its
payload decoded into comments, since base64 hides precisely the thing worth
looking at.

The tokens are placeholders. Minting one requires an API server, so a dry run
cannot produce a working credential and does not pretend to: the output shows
the shape faithfully and the secrets not at all. Applying it will not give you a
mesh.

cert-manager plus trust-manager for the CAs, and External Secrets or a
documented SOPS flow for the shared password, are the intended long-term tools
for this step; nothing in the operator requires them.

### 3. Install the operator at every site, with its identity

```bash
# Site A:
helm upgrade --install slaptain ./charts/operator \
  -n slaptain-system --create-namespace --set siteName=site-a

# Site B:
helm upgrade --install slaptain ./charts/operator \
  -n slaptain-system --create-namespace --set siteName=site-b
```

This is the only command whose arguments differ between sites.

### 4. Apply one chart, with one values file, everywhere

```bash
for ctx in site-a site-b site-c; do
  helm --kube-context "$ctx" upgrade --install ldap ./charts/slapd-mesh \
    -n slaptain --create-namespace \
    -f my-mesh.yaml
done
```

`charts/slapd-mesh/examples/three-site.yaml` is a complete working values file.
The skeleton:

```yaml
mesh:
  name: slapd-mesh
  sites:
    - name: site-a
      serverIDIndex: 0
      endpoint: https://api.site-a.k8s.example:6443
    - name: site-b
      serverIDIndex: 1
      endpoint: https://api.site-b.k8s.example:6443
    - name: site-c
      serverIDIndex: 2
      endpoint: https://api.site-c.k8s.example:6443
  network:
    mode: pod-routed

cluster:
  name: slapd
  replicas: 3
  ldap:
    tls:
      enabled: true
      secretName: slapd-tls
  replication:
    enabled: true

databases:
  - name: example-db
    spec:
      suffix: "dc=example,dc=org"
      replication:
        ridBase: 100
        deltaSync: true
      seed:
        site: site-a          # the founder, named — not edited in
        entries: [...]
```

Notice what is **not** in there: no `serverIDBase`, no `externalPeers`, no
network block on the cluster, no Secret, nothing derived from the Helm release
name. The chart refuses to render several classes of incoherent bundle
(duplicate index, duplicate `ridBase`, a selector naming an unknown site, a
seeded database with no `seed.site` once a second site is declared), so those
mistakes never reach a cluster.

You can verify the identical-everywhere property yourself:

```bash
helm template a ./charts/slapd-mesh -n ns-one -f my-mesh.yaml | sha256sum
helm template b ./charts/slapd-mesh -n ns-two -f my-mesh.yaml | sha256sum
```

The hashes must match.

---

## Site identity: the one per-site fact

`--set siteName=…` on the **operator's** chart. Not in any CR — putting it in
`SlapdMesh` would destroy the byte-identical property the whole design rests
on.

What the operator does with it:

- picks its own `serverIDIndex` out of `mesh.sites`, and therefore its decade;
- builds its peer set as *every other site* the cluster spans;
- decides whether `seed.site` names it, and seeds or withholds accordingly.

**If it is wrong or missing**, in order of how bad:

| State | What happens |
|---|---|
| unset | `meshRef` cannot resolve: `MeshResolved=False` with reason `SiteIdentityMissing`, `phase: Error`, and nothing else is reconciled. A `seed.site` naming any site withholds — unreadable identity never counts as a match. |
| set to a name the mesh does not declare | The same shape, reason `SiteNotInMesh`. A loud error, never a silent default to the first site. |
| set to the *wrong* site's name (a name the mesh does declare) | **The dangerous one.** The site adopts another site's decade. Two sites sharing one `serverID` collide in every `contextCSN`: slapd keeps the highest CSN per serverID, so each other's writes read as already-seen and some simply never propagate. slapd validates nothing here and no condition goes red. |

A stalled cluster is recoverable; a collided decade is data quietly not
arriving. That is why the identity is the first thing to check when a mesh
misbehaves, and why the failure directions above are deliberately asymmetric:
anything the operator cannot read makes it do *nothing*, which is loud and
fixable.

---

## Networking

Set once, on the mesh, because two clusters on one fabric cannot sensibly
disagree about whether pod CIDRs are routed.

| Mode | Peers addressed by | Needs |
|---|---|---|
| `pod-routed` (default) | the pod's primary IP | pod CIDRs natively routed between sites ([ADR-016](adrs/adr-016-pod-routed-cross-cluster-replication.md)) |
| `multus` | the pod's `net1` IP | a NetworkAttachmentDefinition, referenced as `multusNetwork` ([ADR-007](adrs/adr-007-multus-replication-network.md)) |

Either way the operator **discovers** peer addresses by querying each remote
site's Kubernetes API with the kubeconfig Secret from step 2 — there is no
static address list to maintain, and none to go stale when a pod is replaced.
Discovery rides the operator's 60-second monitoring tick, so a replaced pod's
peers pick up its new address within roughly one to a few minutes rather than
instantly.

Cross-site connections are TLS: the consumer verifies the provider against the
distributed CA. It is not mutual TLS — peer *authentication* is the simple
bind, not the certificate.

---

## Seeding: exactly one founder

Exactly one site creates a database's initial entries. Name it with
`seed.site`; every other site withholds and receives the tree by replication.

If two sites seed the same suffix concurrently, one pod's suffix entry is
demoted to a permanent hidden **glue** entry. It is invisible to ordinary
searches (only a `ManageDsaIT` search reveals it), frozen at the winner's
`entryCSN` so every CSN health check reads clean — and every backup taken from
that pod is unrestorable. It does not heal, and there is no automatic repair;
the manual runbook is in
[ADR-025](adrs/adr-025-single-creator-seed-glue-suffix.md).

Three things stand between you and that:

1. **The chart refuses to render** a seeded database with no `seed.site` once
   the mesh declares more than one site.
2. **The operator's seed gate**: it compares `seed.site` against its own
   identity before opening any LDAP connection, so a non-founder never even
   dials a pod to seed.
3. **The operator's evidence belt**: a suffix entry whose creator is a foreign
   serverID withholds the seed regardless of what the spec says. The gate acts
   on what the spec *declares*; the belt on what the directory shows has
   already *happened*. Both fire.

`DataPresent` on each `SlapdDatabase` reports a glue suffix as
`False/GlueSuffix` if one ever does occur — see below.

---

## Checking that a mesh is healthy

Three layers, cheapest first. Run each **at every site**: the sites are
independent, so a green reading at one says nothing about its neighbours.

### 1. Is the mesh resolved?

```bash
kubectl get slapdcluster slapd -o \
  jsonpath='{.status.conditions[?(@.type=="MeshResolved")].status}{"\n"}'
```

`True` means this operator found the mesh, found itself in it, and derived its
wiring. Anything else and the cluster is reconciling **nothing** by design —
read the condition's `message`, which names the cause.

`slctl status` shows the same thing next to the phase and the resolved peers —
the mesh they came from, marked `(UNRESOLVED)` when they did not:

```
$ slctl status -n slaptain slapd
SlapdCluster: slaptain/slapd
────────────────────────────────────────
  Phase:              Running
  Age:                1m
  Replicas:           3/3 ready
  Read-Only:          1/1 ready
  TLS:                true
  Replication:        true
  Mesh:               slapd-mesh
    external peers are DERIVED from SlapdMesh "slapd-mesh" (ADR-028 §4) and do
    not appear in spec.replication.externalPeers; the 2 peer(s) below are read
    back from status.externalPeerStatuses, as of the operator's last status
    write.
  External Peers:
    site-2               discovery (3 pod(s))  Synced
    site-3               discovery (3 pod(s))  Synced
  Conditions:
    MeshResolved             True  (Derived)  cross-site wiring derived from SlapdMesh "slapd-mesh"
    Ready                    True  (AllReplicasReady)  3/3 replicas ready
    ReplicationConverged     True  (CSNsMatch)  all 3 pods report identical contextCSN for each of 2 databases
```

Its peer list is read back from `status.externalPeerStatuses`, i.e. from what
the operator actually applied, rather than re-derived by the CLI. That is the
point: a second opinion on a derivation whose output is baked into replicated
data would be free to disagree with the operator you are using it to debug. The
cost is freshness, bounded by the operator's last status write, and where the
identity cannot be determined the output says so (`LAST KNOWN`, `UNKNOWN, not
empty`) rather than showing an empty list.

### 2. Are the peers connected and current?

`status.externalPeerStatuses` carries one entry per derived peer:

```bash
kubectl get slapdcluster slapd -o jsonpath=\
'{range .status.externalPeerStatuses[*]}{.name}{"  "}{.replicationState}{"  "}{.discoveredAddresses}{"\n"}{end}'
```

Shape of the output on a healthy three-site mesh, read at site 1 (one line per
remote site, addresses elided):

```
site-2  Synced  ["<pod-ip>","<pod-ip>","<pod-ip>"]
site-3  Synced  ["<pod-ip>","<pod-ip>","<pod-ip>"]
```

One entry per remote site, one discovered address per remote pod. A site whose
list is empty has not been discovered yet — check the kubeconfig Secret and the
network path to that site's API server.

| `replicationState` | Means |
|---|---|
| `Synced` | the peer answered and every database read is current |
| `Lagging` | the peer is behind |
| `PartiallyVerified` | the peer answered and everything readable is current, but at least one database yielded no readable `contextCSN` — `lastError` names it |
| `Unreachable` | the peer did not answer |

One caveat worth internalising: peer status is **consumer-side and
one-directional**, and CSN lag is only observable while writes are flowing. An
idle database reads `Synced` across a link that is in fact broken. Treat a
quiet mesh's green as "nothing has contradicted it", not as a probe.

### 3. Is the data actually there, on every pod?

```bash
kubectl get slapddatabase -o custom-columns=\
'NAME:.metadata.name,DATA:.status.conditions[?(@.type=="DataPresent")].status,REASON:.status.conditions[?(@.type=="DataPresent")].reason'
```

`DataPresent` is judged **per pod**, read-only replicas included, which is what
makes it catch things a suffix-level check cannot:

| Status / reason | Means |
|---|---|
| `True` / `RootEntryVisible` | every reached pod at this site serves the suffix |
| `False` / `GlueSuffix` | a pod's suffix entry is a ManageDsaIT-confirmed hidden glue entry — the ADR-025 corruption. Never transient. |
| `False` / `DataMissingOnPods` | some pods hide or lack the suffix while others serve it |
| `False` / `DataMissingOnReadOnlyPods` | only read-only pods are short. Legitimately transient during an RO initial sync — it gets its own reason precisely so an alert rule can give it a longer fuse than the others. |
| `False` / `DataMissing` | no reachable pod serves the suffix, and this database *has* held data before |
| `Unknown` / `NoDataYet` | no pod serves it and the database has never been seeded, restored or observed holding data — normal at a non-founder site right after install |
| `Unknown` / `NoReachablePod` | no pod could be reached; unreadable evidence, not a pass |

`DataPresent` is informational: it never triggers operator action (ADR-012). It
is what you alert on, not what heals you.

`ReplicationConverged` on the `SlapdCluster` is the in-site counterpart:
`contextCSN` agreement, judged per database, with `CSNQueriesIncomplete` when a
pod × database pair could not be read. Unreadable evidence never counts toward
a pass.

### Deeper: `slctl inspect`

```bash
slctl inspect -n slaptain slapd            # full per-pod report
slctl inspect -n slaptain slapd --short    # checks only; non-zero exit on failure
```

`--short` on a healthy three-site mesh, read at one site (some checks elided):

```
SlapdCluster: slaptain/slapd
Mesh:         slapd-mesh
────────────────────────────────────────
Checks:
  [OK  ] mesh-resolution          2 external peer(s) derived from SlapdMesh "slapd-mesh"
  [OK  ] rw-replica-count         3/3 ready
  [OK  ] suffix-visibility        2 database suffix(es): base entry visible on every queried pod
  [OK  ] suffix-uuid-agreement    2 database suffix(es): all pods agree on the base entry's entryUUID
  [OK  ] csn-convergence          2 database(s): all 4 pods report identical contextCSN for each
  [OK  ] syncrepl-stanza-count    RW: 4 each, RO: 3 each
  [OK  ] external-syncrepl        all RW pods have stanzas for all 2 external peers
  [OK  ] cross-site-csn           all external peers report Synced
  ...
17 passed, 0 warnings, 0 failed
```

It queries every pod over LDAP and runs the consistency checks — CSN
convergence per database, topology, stanza counts, suffix visibility and suffix
UUID agreement (the ADR-025 probes), and mesh resolution. `--short` is the CI
shape; it exits non-zero when a check fails.

`slctl inspect` compares **runtime state within one site over LDAP**. It does
not compare specs across sites — that is a different job, and for now it is the
hash comparison in step 4 of the install plus reading the same `kubectl get`
at each site.

---

## Operating a mesh

### Adding a site

1. Bootstrap trust and the shared credential Secrets for the new pair(s).
2. Install the operator at the new site with its own `siteName`.
3. Add the site to `mesh.sites` with a fresh `serverIDIndex`, and apply the
   updated values file **everywhere**.

During the rollout the sites run different generations of the mesh — site A
already knows a site that site B does not. That is expected and tolerated:
mesh changes are additive, and a site that has not yet heard about a peer
simply does not have a stanza for it. Nothing breaks; the link comes up when
both ends know about each other.

Adding a peer changes the pod template (the new peer's CA mount), so the
`SlapdCluster` rolls its pods. Not a data event, but not invisible either.

### Removing a site

Drop it from `mesh.sites` **everywhere**; do not reuse its `serverIDIndex`.
Every remaining site's syncrepl stanza for it disappears on the next reconcile
— the operator watches the mesh object, so this does not wait for a resync —
and the pods roll to drop the now-unused CA mount.

### Changing a database or a schema

Edit the one values file and apply it at every site. The invariant table above
is the checklist for what must not end up differing.

### Blast radius

One bad mesh definition applied everywhere hits every site at once, where
per-site manifests would have localised the mistake. That is the trade: more
visible, and more dangerous. The chart's render-time checks exist to catch the
classes of mistake that are worth catching before they fan out.

---

## Backup and restore across a mesh

Backups are per-site and per-database — a `SlapdBackup` runs against the
cluster it names. See [Backup & Restore](BACKUP.md).

Two mesh-specific notes:

- **`SlapdRestore` is a rollback only on an isolated cluster.** On a mesh
  member, the peers immediately replay the newer changes you just rolled back,
  so it is a local re-seed rather than a rollback. A true mesh-wide rollback is
  a human runbook.
- **`bootstrapFrom` across a mesh is argued, not measured.** Restore loads
  every pod directly with offline `slapadd`, and `slapcat` preserves
  `entryUUID` and `entryCSN`, so N sites restoring one artifact should produce
  identical entries rather than the independent creations that cause an
  ADR-025 glue suffix. That reasoning has never been exercised end to end. Do
  not let a first production install be the thing that tests it.

---

## Related

- [ADR-028](adrs/adr-028-mesh-scoped-vs-site-scoped.md) — the mesh layer, and what "mesh-scoped" means
- [ADR-025](adrs/adr-025-single-creator-seed-glue-suffix.md) — single-creator seeding, and the glue suffix
- [ADR-017](adrs/adr-017-bare-integer-serverid.md) — the serverID arithmetic
- [ADR-016](adrs/adr-016-pod-routed-cross-cluster-replication.md), [ADR-007](adrs/adr-007-multus-replication-network.md) — the two network modes
- [ADR-008](adrs/adr-008-csn-monitoring-credentials.md) — what CSN monitoring can and cannot see
- [ADR-019](adrs/adr-019-per-database-accesslog.md) — why a missing database halts replication
- [`charts/slapd-mesh/README.md`](../charts/slapd-mesh/README.md) — chart reference
- [Backup & Restore](BACKUP.md), [Tuning & Sizing](TUNING.md)
