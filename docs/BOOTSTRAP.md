# Cluster Bootstrap

> **Partially outdated.** This document was written before ADR-003 (operator owns all syncrepl
> configuration). Key changes not yet reflected:
> - The init container no longer writes syncrepl stanzas or mirrormode — the operator applies
>   these at runtime via `reconcileReplication` (step 7b).
> - The operator now prepends a replication ACL rule automatically when replication is enabled.
> - The reconcile steps after bootstrap (ACLs, schemas, replication) and their timing relative
>   to `PhaseRunning` are not documented here. See `docs/reconcile-loop-fixes.md` for known
>   issues with the bootstrap→ready sequencing.
> - Cross-cluster replication (externalPeers) is not covered. See `docs/REPLICATION.md`.

This document explains how a new `SlapdCluster` is bootstrapped: from an empty namespace to a
running, fully-replicated LDAP directory. It also covers how to verify that replication is
working and what to check when it isn't.

If you are new to LDAP, read [CLAUDE.md](CLAUDE.md) first — it builds the mental
model you need to understand what this document is doing and why.

---

## Two Phases of Bootstrap

Bootstrap is split between two components: the **init container** (slapd configuration) and the
**operator** (initial directory data). The split is deliberate and necessary — see
[Why Not slapadd with Replication](#why-not-slapadd-with-replication).

### Phase 1: Init Container — slapd Configuration

The `slapd-init` init container runs before the main slapd process starts on every pod. Its
sole job is to produce a working `cn=config` tree (slapd's runtime configuration database) on
the `/ldap-config` PVC. It does this by:

1. Writing a temporary `slapd.conf` file from environment variables injected by the operator.
2. Running `slaptest -f slapd.conf -F /ldap-config` to convert the classic flat-file format
   into the modern `slapd.d` directory-of-LDIF format that OpenLDAP uses at runtime.
3. If persistence is enabled (the default), the resulting config lives on the PVC across pod
   restarts. If the config directory already exists, the entire generation is skipped.

The generated configuration includes:

- Module loading (`back_mdb`, plus `accesslog` + `syncprov` when replication is enabled)
- Schema includes (`core`, `cosine`, `inetorgperson`, `nis`)
- TLS directives (when `spec.ldap.tls.enabled=true`)
- Config database (`cn=config`) with `rootpw` set to a hash of the root password
- Accesslog database (`cn=accesslog`) — only when replication is enabled; this is a
  separate LMDB database that records every write to the main data database
- Main data database (`suffix`, `rootdn`, `rootpw`, `directory`)
- Replication overlays (`accesslog`, `syncprov`) and a `syncrepl` stanza for **every peer pod
  except itself** — only when replication is enabled (see [Replication Topology](#replication-topology))

**Idempotency:** if `$CONFIG_DIR/slapd.d/cn=config` already exists, the entire generation
block is skipped. The init container never wipes existing data — see ADR-012 for the cluster
lifecycle model (wipe and rebootstrap are done via `kubectl delete` of the CR + PVCs, not
via an in-operator switch).

**Password handling:** the operator passes `LDAP_ADMIN_PW` and `LDAP_ROOT_PW` as plaintext
env vars from Kubernetes Secrets. The init container hashes them at runtime using:
```bash
ADMIN_PW_HASH=$(slappasswd -s "$LDAP_ADMIN_PW" -h {SSHA})
ROOT_PW_HASH=$(slappasswd -s "$LDAP_ROOT_PW"  -h {SSHA})
```
The hashes are embedded in `slapd.conf` as `rootpw` values. Hashes are never stored anywhere
permanent — they are recomputed on every bootstrap run from the plaintext in the Secrets.

### Phase 2: Operator — Initial Directory Data

Once the main slapd container is running and pod-0 reports Ready, the operator's
`reconcileBootstrap` function connects to pod-0 via a live LDAP connection and adds the minimal
initial entries required for the directory and for replication to function:

| DN | objectClasses | Purpose |
|---|---|---|
| `<domain>` | `top`, `dcObject`, `organization` | Root / suffix entry — every directory tree must have this |
| `cn=admin,<domain>` | `simpleSecurityObject`, `organizationalRole` | Data admin account for day-to-day operations |
| `cn=replication,<domain>` | `simpleSecurityObject`, `organizationalRole` | Bind credential for inter-node replication (replicated clusters only) |

Going through a live slapd connection — rather than `slapadd` — is essential when replication
is enabled. See [Why Not slapadd with Replication](#why-not-slapadd-with-replication).

### Multi-site: exactly one site seeds (founder-only — ADR-025)

In a cross-cluster mesh, **exactly one site may apply the seed**. Every site
deploys the *same* `SlapdDatabase` — seed included — and the seed itself names
the site allowed to apply it:

```yaml
spec:
  seed:
    site: site-a          # only the operator whose SITE_NAME is site-a seeds
    entries: [...]
```

Each operator compares that name against its own identity, which comes from its
installation config (`SITE_NAME`, set from the operator chart's `siteName`) and
never from a CR. The named site seeds; every other site withholds and receives
the whole DIT via syncrepl, exactly like a fresh peer pod does. A site whose
operator has **no** identity configured withholds too and stays `Degraded` until
`siteName` is set — an unreadable identity never counts as a match.

Leaving `seed.site` unset means "no site restriction", which is what a
single-site deployment wants and is the behaviour every pre-existing CR keeps.

Until 2026-09-17 this was a per-site *edit* instead — the founder carried
`spec.seed` and every other site deployed the CR with the block removed. That
made a resource which must be byte-identical across sites differ per site, and
it put a hard correctness rule in a deployment procedure rather than in the
spec. ADR-028 §3 replaced it with the declaration above.

Why this is a hard rule and not a style preference: `entryUUID` is
server-generated, so two sites seeding the same DNs create two different
*identities* for the "same" entries. slapd's syncrepl conflict resolution can
then demote the losing pod's suffix entry to a permanent, **hidden glue
entry** — the pod answers base searches with nothing (only a ManageDSAIT
search reveals the glue), every backup taken from it is unrestorable, and no
CSN-based health signal ever notices. The operator additionally withholds a
seed whose suffix already has a foreign creator, but that belt cannot close
the race between two sites seeding simultaneously — the founder rule does.

Detection and the manual heal runbook for an existing glue live in ADR-025;
`slctl inspect` (`suffix-visibility`, `suffix-uuid-agreement`) and the
database's `DataPresent` condition surface it. `DataPresent` runs on peer sites
too, even though they carry no `spec.seed`: an unseeded database with no data
anywhere yet reads `Unknown/NoDataYet` (waiting for its first refresh — not an
alert), turns `True` once the suffix is visible on every pod, and never returns
to `NoDataYet` afterwards, so a later disappearance reads as the data-loss
alert it is.

**Read-only replicas are probed too** (`spec.readReplicas > 0`), because a glue
suffix propagates to a consumer that initial-synced from a glued provider. They
report under their own reason, `False/DataMissingOnReadOnlyPods`, which is
expected **transiently**: a freshly created or rebuilt read-only pod has not
received the suffix entry yet, so the condition goes False for the length of its
initial sync and returns to True when the DIT lands. Writable-pod divergence
keeps the existing `False/DataMissingOnPods`, and a confirmed glue on any pod —
read-only included — is `False/GlueSuffix`. Alert on `DataMissingOnPods` and
`GlueSuffix` immediately; give `DataMissingOnReadOnlyPods` a fuse longer than a
read-only replica's initial sync takes in your deployment. None of the three
affects the `SlapdDatabase` phase (ADR-012: observability only), and with
`readReplicas: 0` nothing changes at all.

---

## Why Not slapadd with Replication?

`slapadd` writes entries directly into the LMDB database files, **bypassing all overlays**
including the `accesslog` overlay. This means slapadd writes are invisible to the accesslog.

When delta-syncrepl replication is running, a consumer (replica) connects to a provider and
asks: "give me everything that changed since CSN X". The provider searches its accesslog for
write operations since that CSN. If the provider has entries that were loaded via `slapadd`
and never appeared in the accesslog, the accesslog and the data database are out of sync.
OpenLDAP detects this and refuses to replicate, logging:

```
consumer has state info but provider doesn't!
```

The fix is simple: **never use slapadd to load initial data when the accesslog overlay is
active**. Instead, insert via a live LDAP connection so every write is captured by the
overlay.

**Standalone mode** (single replica, `replication.enabled=false`) has no accesslog overlay.
`slapadd` is safe and fast there, and the init container uses it for initial data.

---

## Credential Flow

Bootstrap depends on Kubernetes Secrets managed by the operator's `reconcileSecret` function.
Passwords are stored as **plaintext** in Secrets — hashing happens at runtime inside the init
container, not in the operator.

### Secret layout

**`<name>-passwords`** — owned by operator SA; read by slapd-init and the operator
- `admin-password` — plaintext password for `cn=admin,<domain>` (data DB admin)
- `replication-password` — plaintext password for `cn=replication,<domain>` (syncrepl bind)

**`<name>-config-password`** — owned by operator SA only (Phase 3 topology management)
- `root-password` — plaintext password for `cn=admin,cn=config` (config DB admin)

Both Secrets are **create-only** — the operator writes them once and never modifies them.
Rotating passwords requires manual intervention (delete the Secrets and the SlapdCluster +
its PVCs, then redeploy). See ADR-012 for the cluster lifecycle model.

### Auto-generated credentials (default)

When `spec.ldap.credentialsSecretName` is not set, the operator generates random passwords on
first reconcile using 24 bytes of cryptographic randomness (base64-encoded). The replication
password uses 32 bytes.

### User-provided credentials

Set `spec.ldap.credentialsSecretName: my-secret` to supply your own credentials. The Secret
must contain `admin-password` and `root-password` keys (plaintext). `replication-password` is
optional — the operator generates one if it is absent.

```bash
kubectl create secret generic my-ldap-credentials \
  --from-literal=admin-password=changeme \
  --from-literal=root-password=changeme \
  --from-literal=replication-password=changeme
```

The operator reads the user-provided Secret and creates the two typed Secrets
(`<name>-passwords` and `<name>-config-password`) from it. The user-provided Secret is only
read, never modified.

### Why two separate Secrets?

Different components need different subsets of the credentials, and separation limits blast
radius:

- The slapd init container and the operator bootstrap both need `admin-password` and
  `replication-password` (both in `<name>-passwords`).
- `root-password` (config DB admin) is only needed by the operator for Phase 3 topology
  management — it has no business being readable by the bootstrap job or e2e tests.

---

## Replication Topology

This section explains how the three (or N) slapd nodes find each other and set up replication.
If you are not familiar with LDAP replication concepts, read the
[Replication Model](CLAUDE.md#replication-model) section of CLAUDE.md first.

### What "N-way multi-master mirrormode" means in practice

Every node is simultaneously a **provider** (source of changes) and a **consumer** (receiver of
changes) for every other node. There is no distinguished primary or leader — all nodes accept
writes at all times. If two nodes receive conflicting writes simultaneously (which is rare),
OpenLDAP resolves the conflict via CSN ordering.

This is OpenLDAP's `mirrormode on` directive. Without it, having multiple providers with
overlapping data would cause replication loops. Mirrormode makes slapd aware it is in a
multi-master setup and suppresses the loop detection.

### How nodes discover their peers — static configuration

There is **no dynamic peer discovery**. The init container bakes the full replication
topology into `cn=config` at pod startup time, before slapd ever starts.

The operator passes these environment variables to the init container:

| Variable | Example value | Purpose |
|---|---|---|
| `LDAP_REPLICATION_ENABLED` | `true` | Gates the entire replication code path |
| `LDAP_REPLICAS` | `3` | Total number of replicas |
| `LDAP_CLUSTER_NAME` | `slapd` | StatefulSet name (= pod name prefix) |
| `LDAP_CLUSTER_HEADLESS_SVC` | `slapd-headless` | Headless Service name for stable DNS |
| `LDAP_NAMESPACE` | `slaptain-testing` | Kubernetes namespace |
| `LDAP_REPLICATION_PASSWORD` | `(random)` | Plaintext bind password for `cn=replication` |

The init container loops over ordinals 0 to `LDAP_REPLICAS-1`, skips its own ordinal, and
writes one `syncrepl` stanza per peer into `slapd.conf`:

```
syncrepl rid=002
  provider=ldaps://slapd-1.slapd-headless.slaptain-testing.svc.cluster.local:1025
  type=refreshAndPersist
  searchbase="dc=chuck-chuck-chuck,dc=net"
  bindmethod=simple
  binddn="cn=replication,dc=chuck-chuck-chuck,dc=net"
  credentials=<LDAP_REPLICATION_PASSWORD>
  logbase="cn=accesslog"
  logfilter="(&(objectClass=auditWriteObject)(reqResult=0))"
  syncdata=accesslog
  ...
```

The peer hostname is deterministic because Kubernetes StatefulSets guarantee stable DNS for
each pod via the headless service:
`<pod-name>.<headless-service>.<namespace>.svc.cluster.local`

So `slapd-0` always resolves to pod-0, `slapd-1` to pod-1, etc.

After slaptest converts `slapd.conf` to `cn=config` format, these syncrepl stanzas become
`olcSyncRepl` entries under `olcDatabase={1}mdb,cn=config`. Once slapd starts, it reads them
and immediately begins attempting outbound connections to its peers.

### What `cn=replication,<domain>` is — and is not

The `cn=replication,<domain>` entry is a **bind credential** — an LDAP user account whose
password is used by the syncrepl consumer to authenticate to its provider. It does **not**
control peer discovery in any way. Peer discovery is entirely in the `syncrepl` stanzas.

The entry needs to exist in the directory (so the provider can authenticate the bind request)
and needs to have read access to both the data tree and the accesslog. Those permissions come
from two ACL rules written into `slapd.conf` by the init container:

```
# On the accesslog database:
access to *
  by dn.exact="cn=replication,<domain>" read
  by * none

# On the data database:
access to *
  by dn.exact="cn=replication,<domain>" read
  by users read
  by anonymous auth
```

The operator adds the `cn=replication` LDAP entry during Phase 2 bootstrap (live LDAP ADD),
so the accesslog captures it and it propagates to all replicas automatically.

### N² connection scaling

With N nodes in mirrormode, every node makes N-1 outbound syncrepl connections and accepts
N-1 inbound connections. Total connections in the cluster:

| Nodes | Total connections |
|---|---|
| 2 | 2 |
| 3 | 6 |
| 4 | 12 |
| 5 | 20 |

This O(N²) growth is why OpenLDAP multi-master is designed for small clusters. **We target a
maximum of 3 nodes.** Beyond that, the replication chatter becomes a meaningful fraction of
total traffic. For larger scale, a different topology (one small write cluster with many
read-only replicas) would be more appropriate — that is out of scope for this operator.

---

## Reading the Logs to Verify Replication

slapd logs every connection and operation. The timestamps in the log are **Unix epoch seconds
in hexadecimal**, which looks alarming but is straightforward:
`69a3eedd` = 0x69a3eedd ≈ the wall-clock time at that moment.

### What healthy startup looks like on slapd-0 (the seed pod)

slapd-0 starts first (StatefulSet ordered rollout) while slapd-1 and slapd-2 do not exist yet.
You will see connection failures for the first several seconds — this is expected:

```
# slapd-0 tries to connect to slapd-1 as a consumer — slapd-1 isn't up yet
slap_client_connect: URI=ldaps://slapd-1... ldap_sasl_bind_s failed (-1)
do_syncrepl: rid=002 rc -1 retrying (9 retries left)
```

`rc -1` means "connection refused / unreachable", not an authentication failure. The `(9
retries left)` countdown with exponential backoff is normal — slapd-0 will keep retrying.

Then the operator connects to perform bootstrap:

```
conn=1000 ACCEPT from IP=<operator-pod-ip>:... (IP=0.0.0.0:1024)   ← plaintext LDAP, port 1024
conn=1000 op=0 BIND dn="cn=admin,dc=chuck-chuck-chuck,dc=net" mech=SIMPLE
conn=1000 op=0 RESULT tag=97 err=0                                  ← bind succeeded

conn=1000 op=1 SEARCH RESULT tag=101 err=32 nentries=0              ← err=32 = NoSuchObject
                                                                       root entry doesn't exist yet
conn=1000 op=2 ADD dn="dc=chuck-chuck-chuck,dc=net"
conn=1000 op=2 RESULT tag=105 err=0                                 ← root entry added OK
conn=1000 op=3 ADD dn="cn=admin,..."    RESULT err=0
conn=1000 op=4 ADD dn="cn=replication,..." RESULT err=0
conn=1000 fd=17 closed (connection lost)                            ← operator disconnects cleanly
```

Then slapd-1 starts and connects to slapd-0 as a consumer. This is the key replication
success sequence:

```
# slapd-1 connecting to slapd-0 (slapd-0's perspective — incoming connection)
conn=1001 ACCEPT from IP=<slapd-1-pod-ip>:... (IP=0.0.0.0:1025)   ← LDAPS, port 1025
conn=1001 TLS established tls_ssf=256 ssf=256 tls_proto=TLSv1.3   ← encrypted channel OK

conn=1001 op=0 BIND dn="cn=replication,..."  mech=SIMPLE
conn=1001 op=0 RESULT tag=97 err=0                                  ← *** AUTH OK ***

conn=1001 op=1 SRCH base="dc=..." scope=2 filter="(objectClass=*)"
conn=1001 op=1 SEARCH RESULT err=0 nentries=3                       ← *** FULL SYNC: 3 entries pulled ***

conn=1001 op=2 SRCH base="cn=accesslog" filter="(&(objectClass=auditWriteObject)...)"
                                                                     ← *** DELTA MODE: watching accesslog ***
```

The three critical success lines are:
1. `RESULT tag=97 err=0` on the BIND — the replication password is accepted
2. `SEARCH RESULT err=0 nentries=3` on the data tree — full initial sync succeeded, 3
   entries replicated (root, admin, replication)
3. `SRCH base="cn=accesslog"` — consumer has switched to delta mode, watching for future
   incremental changes; this connection stays open indefinitely

### What healthy startup looks like on slapd-1 and slapd-2

Each non-seed pod's log shows two kinds of activity:

**Outbound (as consumer):** not visible in its own log — those connections show up in the
provider's log as inbound connections.

**Inbound (as provider):** two connections arrive, one from each of the other two nodes:

```
# slapd-1 log — two inbound replication connections
conn=1000 ACCEPT from <slapd-0-IP>    TLS established    BIND RESULT err=0
conn=1000 op=1 SRCH base="cn=accesslog" ...              ← slapd-0 watching slapd-1's accesslog

conn=1001 ACCEPT from <slapd-2-IP>    TLS established    BIND RESULT err=0
conn=1001 op=1 SRCH base="dc=..." nentries=3             ← slapd-2 did full sync from slapd-1
conn=1001 op=2 SRCH base="cn=accesslog" ...              ← slapd-2 now in delta mode
```

slapd-2's log shows only accesslog searches (no `nentries=3`) because by the time the other
two nodes connect to slapd-2, slapd-2 has already received its initial data from slapd-1 via
slapd-1's outbound syncrepl, and slapd-0 and slapd-1 go straight to accesslog monitoring.

### Cheat sheet: what to look for

| What you see | What it means |
|---|---|
| `ldap_sasl_bind_s failed (-1)` | Peer not reachable yet — normal during startup |
| `BIND RESULT tag=97 err=0` on `cn=replication` | Replication auth OK |
| `BIND RESULT tag=97 err=49` on `cn=replication` | Wrong password — check `<name>-passwords` secret |
| `SEARCH RESULT err=0 nentries=3` on data tree | Full initial sync succeeded |
| `SEARCH RESULT err=0 nentries=0` on data tree | Sync succeeded but no data yet — timing |
| `SRCH base="cn=accesslog"` | Node is in delta-syncrepl mode — healthy steady state |
| No accesslog SRCH ever | syncrepl stanzas not in cn=config — check init container logs |

---

## Verifying Replication Without Log Reading

If you cannot easily tell from logs whether replication is configured, query slapd directly:

```bash
# 1. Did the init container succeed and write syncrepl stanzas?
kubectl logs -n <namespace> <pod> -c init | grep -E "Replication|syncrepl|slaptest|ERROR"

# 2. Are syncrepl stanzas actually in cn=config right now?
kubectl exec -n <namespace> <pod> -c slapd -- \
  ldapsearch -H ldapi://%2frun%2fopenldap%2fslapd.ldapi \
  -Y EXTERNAL -b cn=config -s sub \
  "(objectClass=olcSyncRepl)" olcSyncRepl 2>/dev/null

# 3. Does the cn=replication bind DN exist in the data tree?
kubectl exec -n <namespace> <pod> -c slapd -- \
  ldapsearch -H ldap://localhost:1024 \
  -D "cn=admin,<domain>" -w "$ADMIN_PW" \
  -b "<domain>" "(cn=replication)" dn

# 4. How many entries does each pod have? (should all be equal after sync)
for pod in slapd-0 slapd-1 slapd-2; do
  echo -n "$pod: "
  kubectl exec -n <namespace> $pod -c slapd -- \
    ldapsearch -H ldap://localhost:1024 \
    -D "cn=admin,<domain>" -w "$ADMIN_PW" \
    -b "<domain>" -s sub "(objectClass=*)" dn 2>/dev/null | grep -c "^dn:"
done
```

If command 2 returns no results, the init container did not write syncrepl config — check
command 1 for slaptest errors. If command 3 returns no results, the operator bootstrap did not
complete — check the operator logs for bootstrap errors.

---

## The BootstrapComplete Flag

`status.bootstrapComplete` is a boolean on the `SlapdCluster` status subresource. Once `true`,
`reconcileBootstrap` returns immediately on every subsequent reconcile without contacting the
cluster.

`status.phase` is set to `Bootstrapping` (not `Running`) until both:
- All desired replicas are Ready
- `status.bootstrapComplete == true`

**Idempotency:** before adding any entries, `reconcileBootstrap` searches for the root entry
(`<domain>`) with a base-scope query. If it already exists, bootstrap is considered complete
and `status.bootstrapComplete` is set without adding anything. This makes the function safe to
call repeatedly and safe across operator restarts.

---

## Wiping a cluster

There is no in-operator "rebootstrap" switch. To intentionally discard all directory data
and start fresh, use the Kubernetes resource lifecycle:

```bash
kubectl delete slapdcluster <name> -n <ns>
kubectl delete pvc -n <ns> -l app.kubernetes.io/instance=<name>
```

Redeploy via your normal flow (helm / flux / kubectl apply). The operator will recreate
everything from spec, the init container will re-bootstrap `cn=config`, and the seed
runs again because `SeedApplied` doesn't survive CR deletion. See ADR-012 for why this
is the only supported wipe path (and why an earlier `forceRebootstrap` flag was removed).

---

## Standalone vs Replicated — Summary

| Step | Standalone (`replication.enabled=false`) | Replicated (`replication.enabled=true`) |
|---|---|---|
| Init: generate slapd.conf | Yes | Yes — includes accesslog + syncprov overlays + syncrepl stanzas per peer |
| Init: hash passwords | Yes (`slappasswd`) | Yes (`slappasswd`) |
| Init: slapadd base entries | **Yes** (direct, fast) | **No** (bypasses accesslog) |
| Operator: wait for pod-0 Ready | — | Yes |
| Operator: LDAP connect to pod-0 | — | Yes (pod IP, port 1024) |
| Operator: add root + admin entries | — | Yes |
| Operator: add `cn=replication` entry | — | Yes |
| Other pods: sync from pod-0 | — | Automatic via delta-syncrepl on startup |

---

## Connection Strategy

The operator connects to pod-0 **directly via its pod IP** on the container LDAP port (1024),
not through the ClusterIP Service. Reasons:

1. **Direct targeting.** Bootstrap must always reach pod-0 (the StatefulSet seed pod whose
   accesslog the other pods will sync from). Routing through a ClusterIP Service could route
   to any pod.

2. **No cluster-domain dependency.** Resolving `<name>.<namespace>.svc.cluster.local` from
   inside the operator pod can fail on clusters with non-standard domain suffixes. The pod IP
   is always available once the pod is scheduled.

The operator binds as the **data rootdn** (`cn=admin,<domain>`) using the plaintext password
from `<name>-passwords`. OpenLDAP's `rootdn`/`rootpw` mechanism is resolved at the slapd
configuration level — the bind succeeds even before the LDAP entry for `cn=admin,<domain>`
exists in the database, because it is checked against `slapd.conf`, not the directory.
