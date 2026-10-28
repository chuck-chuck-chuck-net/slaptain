# Upgrading slaptain

What an operator upgrade does to a **running** mesh, per release. Read the entry
for every version you cross, not only the one you land on. A fresh install never
needs this file.

For *what changed and why*, read `CHANGELOG.md` — it is the release note and
the place a change is argued. This file is the procedure: when each change
lands, what reports what while it does, and exactly what to set. Where the two
would say the same thing, it belongs here and `CHANGELOG.md` points at it.

---

## v0.5.1 — the operator's cache is filtered to what it manages (ADR-042)

Nothing to set and nothing rolls. What you will see, once, on the first
reconcile of each cluster after the upgrade:

- The cluster's Services, StatefulSets and ConfigMaps gain the label
  `app.kubernetes.io/name: slapd` through their ordinary server-side apply.
  A StatefulSet's own labels are not part of its pod template, so no pod
  moves; its UID is unchanged.
- `<cluster>-config-password` is patched with the same label, and the
  operator logs `labelled a pre-existing Secret for the operator's cache
  (ADR-042)`. Only a Secret the operator owns (controller reference) is
  touched. A `<database>-credentials` Secret is left as it is, whether the
  operator created it or `scripts/mesh-share-credentials.sh` did.
- Secrets you supply — TLS, `cnConfigCredentials`, bring-your-own credentials,
  S3 credentials, kubeconfigs — need no label and get none. They are read
  from the API server directly now, which also means a rotation is seen on
  the next pass rather than at informer lag.

If you had raised `resources.limits.memory` on the operator chart to survive
a large cluster, you can take it back to the default 128Mi: memory now grows
with your slapd deployments, not with your neighbours'. Watch
`go_memstats_heap_inuse_bytes` on the operator (`metrics.enabled=true`) for
a day before you do.

Jobs the operator creates from now on carry the label on their pod template
as well; a Job still running from before the upgrade does not, and the one
consequence is that its failure summary falls back to the Job condition alone
(`job_reap.go`), as it always did when a pod could not be read.

## v0.5.0 — the default slapd image becomes PURE, and `spec.images.select` replaces pinning by tag

Two changes to how a cluster gets its data-plane images. The second is the one
that changes what runs.

### `spec.images.select` — say what you need, not what it is called

```yaml
spec:
  images:
    select:
      storageFormat: lmdb1.0   # what can open your volumes
      flavour: pure            # whether you run slaptain's patches
```

It drives **both** the slapd and slapd-init images from one declaration, so
ADR-021's "pin both or neither" stops being a rule you can violate. An explicit
`spec.images.slapd.tag` still wins and is used verbatim — the selector picks a
build, it does not take a pin away from anyone who wants one.

Unset is `lmdb1.0` + `pure`, which resolves to the operator's own tag with no
suffix. **Nothing about a rendered pod template changes**: the image string an
unpinned cluster gets is the same string it got before this field existed.

### The default image is now the PURE build — this DOES change the binary

The unsuffixed tag used to carry slaptain's behaviour patches. It now carries
the pure build: the same upstream OpenLDAP 2.7.1, from the same packaging fork,
with none of our patches (ADR-040 decision 2). The patched pair moves to
`<tag>-ol2.7.1-slaptain`.

The tag string does not change, so an unpinned cluster rolls onto a different
binary on operator upgrade without its pod template looking any different.
`slapd -VV` is how you tell which you are running: a pure build reports
`2.7.1-0+pure<N>`, a patched one `2.7.1-0+slaptain<N>`.

**Who is affected.** Only clusters using `spec.rotation` with a
phase-staggering mode (`PurgeAndWrite`, the default, or `PurgeOnly`). The phase
is a third value of `olcAccessLogPurge` and is ours, not upstream's, so a pure
slapd refuses it: the operator writes the two-field value, the journal stays
bounded, and the pods purge in step instead of on the rotation grid. It is
reported, not silent — `AccesslogPurgeConverged=False/PurgePhaseUnsupported`.

Rotation's `WriteOnly` mode needs no patch and is unaffected. So is every
cluster not using rotation at all.

**What to do, if you use a phase-staggering rotation mode.** Say so before you
upgrade:

```yaml
spec:
  images:
    select:
      flavour: slaptain
```

That resolves to the patched pair and keeps today's behaviour. It rolls the RW
pods once, as any image change does.

**Why the default moved.** Nobody should have to run a patched slapd in
production to use slaptain, and until now everybody did without being told.
Running our patches is now a deliberate act, which is also what gives the
rotation a real two-act opt-in: turn the feature on, *and* ask for a build that
can express it.

### The data database's session log is sourced from the journal by default

`spec.replication.syncprovSessionlogSource` unset now means `Accesslog` where
it meant `InMemory`. The in-memory session log does not survive a restart, so a
consumer reconnecting after one falls outside the window and takes a full-DIT
refresh it did not need. The journal is already on disk and already written.

**This lands gradually, not at upgrade, and that is the part to plan for.** The
attribute is startup-only (ADR-034): writing `olcSpSessionlogSource` to a
running slapd is a double free in every released version, so the operator never
writes it over LDAP. It reaches `cn=config` only through the init container's
offline convergence at pod start.

So after upgrading, every database will report

```
StartupConfigConverged=False/RestartRequired
```

naming the pods that have not restarted yet. **That is expected and is not an
error** — the condition exists to say a restart is outstanding. Nothing
restarts the pods for you, deliberately: restarting a provider fleet is a
topology event at every peer. Each pod picks the change up at its next restart.

Until then the pod keeps running its in-memory log, which is exactly what it
did before, so there is no window in which it has neither.

**To keep the old behaviour**, say so by name:

```yaml
spec:
  replication:
    syncprovSessionlogSource: InMemory
```

`syncprovSessionlog: 0` still means "no session log at all", and the new
default does not override it.

### The accesslog database gets a syncprov checkpoint, and it is on by default

Every accesslog DB's syncprov overlay now carries `olcSpCheckpoint: 1 1`. The
operator converges it on every reconcile, so it reaches overlays that already
exist — which is all of them: the attribute has never been written before.

**Why this is on rather than offered.** A log DB's `contextCSN` is written only
by a checkpoint or a clean shutdown, while its `minCSN` is durable from the
first write carrying a serverID the log has not seen. An unclean stop between
those two leaves `minCSN` naming a serverID `contextCSN` has never heard of,
and the pod then refuses **every** consumer of that log, permanently, with
`sync cookie is stale`. That is upstream's ITS#9398, fixed in no release, and
it is the failure this project has hit most often.

**What it costs**, measured on a load harness as its own arm: no throughput and
no latency cost — 270,277 ops/s at p99 16.9 ms. Replication stays current up to
**255,903 ops/s**, where the same setup without the checkpoint stays current to
roughly 7% higher before replication lag reaches one second.

That is a ceiling moving at a quarter of a million operations per second, not a
problem replicating: a checkpointed accesslog replicates normally, and if you
are nowhere near those rates this costs you nothing measurable. Measured on
bare metal with fast local storage. **Slower or shared storage is not
measured**, and if your write path is storage-bound this is the knob to look at
first.

**To opt out:**

```yaml
spec:
  replication:
    accesslogSyncprovCheckpoint: none
```

That removes the attribute and restores the pre-0.5.0 behaviour — including the
ITS#9398 exposure. Any explicit value is used verbatim, but **do not tune the
ops half above 1**: the checkpoint has to fire on the log entry's own operation
so that `contextCSN` reaches disk before `minCSN` gains the new serverID. A
larger value reopens the window between two adjacent operations, which is what
the `100 5` control measured storming.

---

## v0.4.0 — ADR-024 R6: `accesslogPurge` becomes an object, and a stored string stops the operator

`SlapdDatabase.spec.replication.accesslogPurge` was a two-token string,
`"7+00:00 1+00:00"`. It is now an object — `{enabled, retention}` — because the
interval half of that string belongs to the cluster, not the database (ADR-024 R6,
ADR-037). The new form is documented in [TUNING.md](TUNING.md).

**Clear the old value on every stored object BEFORE you upgrade.** A new apply of
the string form is refused with a clear message. An object *already in etcd* is not,
and that is the hazard: the CRD upgrade is accepted without a word, and the stored
string then fails to decode into the Go type.

The failure does not stay with the object that caused it. Measured against a real API
server (envtest, k8s 1.37, the pre-change CRD upgraded in place, one legacy object
alongside healthy ones):

- a typed **read** of the legacy object fails —
  `json: cannot unmarshal string into Go struct field
  DatabaseReplicationConfig.spec.replication.accesslogPurge of type
  v1alpha1.AccesslogPurgeConfig`;
- a typed **list** fails with the same error — per namespace and cluster-wide — so
  every healthy `SlapdDatabase` in the result set goes with it;
- the operator's **cache never syncs** `SlapdDatabase` (`WaitForCacheSync` still
  false after 20 s);
- `kubectl` and any unstructured read keep working throughout, showing the stored
  string verbatim.

The operator's informer lists `SlapdDatabase` cluster-wide, so **one un-rewritten CR
in any namespace stops `SlapdDatabase` reconciliation for the whole cluster**, and
nothing in the symptom points at the CR responsible.

### The order

```bash
# 1. Find every SlapdDatabase still storing the string form, at EVERY site.
kubectl get slapddatabases -A -o yaml | yq '
  .items[] | select(.spec.replication.accesslogPurge | tag == "!!str")
  | .metadata.namespace + "/" + .metadata.name + "  " + .spec.replication.accesslogPurge'

# 2. Clear the field on each one. Clearing, not rewriting: under the OLD CRD the
#    field is still typed string, so patching in the object form is rejected
#    ("spec.replication.accesslogPurge in body must be of type string").
kubectl patch slapddatabase <name> -n <namespace> --type=merge \
  -p '{"spec":{"replication":{"accesslogPurge":null}}}'

# 3. Upgrade the CRDs and the operator as usual.

# 4. Set the retention you want, now that the schema can hold it. Unset is the
#    operator's 7-day default, so step 4 is optional.
kubectl patch slapddatabase <name> -n <namespace> --type=merge \
  -p '{"spec":{"replication":{"accesslogPurge":{"retention":"168h"}}}}'
```

Also move the interval, if you were relying on the one the string carried: it is now
`SlapdCluster.spec.accesslogPurge.interval` (or derived from `spec.rotation`), and an
unset one means the operator's 24h default.

### If you upgraded first

Nothing needs restoring and no data is at risk. The repair is step 1 followed
directly by step 4 — `kubectl` still reads and writes these objects, only the typed
decode fails, and the merge patch into the object form is accepted and clears the
decode immediately (measured: the typed list was clean on the very next call).

---

## v0.3.0 — ADR-028 amendment: the serverID stride becomes hex (100 → 16)

**There is no upgrade path, and this is the one entry in this file that says
so.** A site's `serverIDBase` is derived as `serverIDIndex × 16` instead of
`× 100`, so every pod of every site but index 0 would change its `olcServerID`.

`olcServerID` is stamped into every CSN a pod has ever written, and `contextCSN`
is a vector keyed by it. Renumbering a running pod leaves its entire history
filed under the old sid — which then sits in every peer's vector forever without
advancing — and starts a fresh lineage at zero. Nothing detects this and nothing
repairs it.

And the operator **will** write it. `ensureServerIDs` converges `olcServerID`
per pod on every reconcile (ADR-017), comparing the derived value against the
live one and `Replace`-ing when they differ — which until now was a no-op for
any correctly-booted pod, because the derivation never moved. It moves here. So
on an existing mesh the new operator renumbers the pods, once, silently, and
there is no undo.

Do not upgrade a live multi-site mesh across this release:

- **a single-site cluster at index 0** is unaffected — base 0 either way;
- **a standalone cluster** with a hand-set `spec.replication.serverIDBase` is
  unaffected: that field is a bare integer, is not derived, and any value in
  `0..4094` remains legal (ADR-011's 101-104 / 201-204 layout included);
- **a multi-site mesh at indices ≥ 1** must be rebuilt, not upgraded. The change
  was taken deliberately while every deployment was a recreatable lab.

Also new, and worth checking before you apply a mesh: `spec.ridLayout.podsPerSite`
is now capped at **15**. A site's pod ordinal 15 would take the serverID that is
the next site's base. The chart refuses it at render time and the API server at
admission.

Why it was done at all: a serverID is read in exactly one place, the `#sid#`
field of a CSN, and slapd renders that field as three hex digits. Under the
decimal stride the numbers everyone reasoned in were not the numbers anyone
could read — site 1 pod 0 was 101, shown as `065`. At stride 16 it is `011`.
See ADR-028's amendment of 2026-10-05.

---

## v0.3.0 — ADR-031: RIDs become coordinates (`ridBase` → `ridSegment`)

`spec.replication.ridBase` is gone. Each `SlapdDatabase` now declares
`spec.replication.ridSegment`, and every syncrepl RID on every pod is renumbered
once.

**Convert by dividing by 100:** `0` stays `0`, `100` becomes `1`, `200`
becomes `2`.

That conversion preserves your existing numbering, and because the `ridBase`
convention started at `100`, a converted mesh usually begins at segment 1 and
leaves **segment 0 unused**. Segment 0 is perfectly legal — it owns rids 1..20
at the default layout; it is `rid` 0 that is reserved, not the segment. Losing
it costs one segment of 49 and nothing else, so reclaim it only if you want to,
and only by renumbering deliberately.

Everything below was measured on a three-site, 12-pod, 2-database lab mesh,
upgrading `v0.2.2` to the ADR-031 build.

### What the renumber costs

- **one `cn=config` write per pod per database** — 8 per site, counted in the
  operator logs, no repeats;
- **no pod restart** — all 12 pod UIDs, `restartCount` and `startTime`
  unchanged;
- **no full refresh** — 0 empty-cookie refreshes across 9 RW pods; every
  consumer resumed incrementally with its full CSN vector. Sync position is
  keyed on SID and a RID is only a local handle, so changing one loses nothing;
- **no interruption to writes** — 24 canaries written at rotating sites
  throughout the window, all present everywhere afterwards;
- **never a short stanza set** — polled every 12 s, every sample held either
  the old four RIDs or the new four, never fewer.

Three sites converged **~20 s** after the new operator started. The whole
procedure took **64 s**.

### The order — follow it exactly

```bash
# 1. Scale every operator to 0, at EVERY site, before touching anything else.
kubectl --context=<ctx> -n slaptain-system scale deploy/slaptain --replicas=0

# 2. Apply the new CRDs.
make operator-crd-apply CONTEXT=<ctx>

# 3. Convert your manifests (ridBase → ridSegment, ÷100) and apply them.

# 4. Upgrade the operator — this scales it back up.
make operator-helm-install CONTEXT=<ctx> GIT_TAG=<tag> SITE_NAME=<site>
```

**Upgrade `slctl` too.** A pre-ADR-031 `slctl inspect` reports two confident
failures against a *correctly* upgraded cluster —
`syncrepl-stanza-count: 4 stanzas (want 2)` and
`external-syncrepl: missing stanza for site-N` — because it reads a status
field this release removed. The stanzas are fine; the tool is stale. Rebuild it
with `make build-slctl` before you use it to judge the upgrade.

### Why step 1 is not optional

Step 4 applies the CRDs itself, so it is tempting to skip step 1. Do not. If
the **old** operator is running against the **new** CRD — which it is, from the
moment the CRDs land until the new pod is ready — two things happen, both
silent, and they compound:

- **Colliding RIDs.** The API server prunes `ridBase` **on read**; no write is
  needed. The old operator sees it absent, falls back to base `0` for *every*
  database, and writes the same RIDs for all of them. slapd accepts a duplicate
  RID without a word.
- **Severed cross-site replication.** `status.externalPeerStatuses[].discoveredAddresses`
  no longer exists either, so the old operator's cluster controller fails its
  status patch on every pass. Status freezes, the database controller reads no
  peer addresses from it, and it writes stanza sets **with no cross-site links
  at all** — one site dropped from four stanzas per database to two.

Measured on a live site: **complete within 24 s** of the CRD apply, with
controller errors starting at 3 s. Throughout, the `SlapdCluster` reported
`phase: Running` and every peer reported `Synced`, while a write at one site no
longer reached the severed one. **The only signal that catches it is
`slctl inspect`**, where `rid-uniqueness` and `external-syncrepl` both fail.

**If you hit it, finish the documented procedure.** Recovery is automatic and
complete: the stanzas are rewritten correctly and the writes missed during the
severance back-fill. No manual repair.

### If you forget to convert

Every write path refuses. Nothing is silently pruned on the way in:

```
# kubectl apply
strict decoding error: unknown field "spec.replication.ridBase"

# kubectl apply --server-side, and helm upgrade
.spec.replication.ridBase: field not declared in schema
```

The charts refuse a duplicate `ridSegment` before rendering anything. The
operator refuses one again at reconcile, and also refuses a segment outside the
layout's range (`RIDSegmentOutOfRange`) — withholding that database's stanzas
rather than writing a short set, which under `olcSyncRepl`'s atomic replace
would be a deletion.

### Reading the new numbers

With the default layout (`podsPerSite: 8`, `maxSites: 12`, so `M = 20`):

```
rid = 1 + segment·M + offset
offset <  8   →  a pod at your own site, at that ordinal
offset >= 8   →  your link to the site whose serverIDIndex is offset − 8
```

Segment 1 is therefore `21, 22, 23` for local pods and `29, 30, 31` for links
to sites 0, 1, 2. **These mean the same thing at every site in the mesh**,
which the old scheme's `151` — "the first peer in *my* list" — did not.

`slctl rid decode <rid>` does this for you, offline or against a cluster.

### Changing `ridLayout` later

`SlapdMesh.spec.ridLayout` sets `podsPerSite` and `maxSites`. Changing it
renumbers the mesh and costs exactly what this upgrade costs — measured at
**8 s** to renumber and ~1 s to revert, zero restarts, zero full refreshes.

Set it in one values file and apply it unchanged everywhere: it is mesh-scoped,
and two sites running different layouts would decode the same RID differently
with nothing to detect the disagreement.

### Not yet verified

- **Upgrading sites one at a time.** All three were upgraded in lockstep. A RID
  is pod-local and peers need no agreement about each other's numbers, so a
  mixed-version mesh should be safe — but that is reasoned, not measured.
  **Until it is, upgrade all sites together.**
- **A large directory.** The renumber has only been measured at fixture size.
  "No full refresh" is the claim most worth re-checking where a refresh would
  actually hurt.
