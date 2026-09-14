# slctl — SlapdCluster CLI

`slctl` is a diagnostic and verification tool for SlapdCluster resources managed by the slaptain
operator. It queries the Kubernetes API and live LDAP instances to show cluster state, run
consistency checks, and collect debug artifacts.

## Build

```bash
make build-slctl    # → bin/slctl
```

## Global Flags

| Flag | Short | Description |
|---|---|---|
| `--namespace` | `-n` | Target namespace (default: kubeconfig context) |
| `--all-namespaces` | `-A` | List across all namespaces |
| `--kubeconfig` | | Path to kubeconfig |
| `--json` | | Machine-readable JSON output |

Tab completion for SlapdCluster names and namespaces works out of the box:
```bash
source <(slctl completion zsh)    # or bash/fish/powershell
```

---

## Commands

### `slctl status [name]`

Shows CR-level status: phase, replica counts, conditions, replication, external peers.

```
$ slctl status -n slaptain-testing slapd
SlapdCluster: slaptain-testing/slapd
────────────────────────────────────────
  Phase:              Running
  Replicas:           3/3 ready
  Read-Only:          1/1 ready
  Replication:        true
  ...
```

This reads only the SlapdCluster resource — no pod connections or LDAP queries. Use this for
a quick overview; use `inspect` for the full picture.

---

### `slctl inspect [name]`

The main diagnostic command. Port-forwards to each pod, queries its LDAP instance, then runs
automated consistency checks against the gathered data.

```
$ slctl inspect -n slaptain-testing slapd
SlapdCluster: slaptain-testing/slapd
────────────────────────────────────────

  Pod: slapd-0  [Running, ready]
    namingContexts: cn=accesslog-default, dc=chuck-chuck-chuck,dc=net
    contextCSN (dc=chuck-chuck-chuck,dc=net):
      20260416143539.536592Z#000000#000#000000
    syncRepl:
      {0}rid=002 provider=ldaps://slapd-1.slapd-headless...
      {1}rid=003 provider=ldaps://slapd-2.slapd-headless...
    multiProvider: TRUE

  ...

Checks:
  [OK  ] rw-replica-count           3/3 ready
  [OK  ] ro-replica-count           1/1 ready
  [OK  ] pod-readiness              all pods running and ready
  [OK  ] bootstrap                  complete
  [OK  ] naming-contexts            RW: data + 1 accesslog(s), RO: data only
  [OK  ] accesslog-consistency      2 database/pod pair(s): logbase = olcAccessLogDB = log olcSuffix
  [OK  ] csn-convergence            2 database(s): all 4 pods report identical contextCSN for each
  [OK  ] syncrepl-stanza-count      RW: 2 each, RO: 3 each
  [OK  ] syncrepl-skip-self         no RW pod replicates from itself
  [OK  ] multi-provider             TRUE on RW, absent on RO
  [OK  ] rid-uniqueness             all RIDs unique within each pod

10 passed, 0 warnings, 0 failed
```

Exits non-zero if any check fails.

#### `--short` flag

Suppresses per-pod detail, shows only the checks summary. Designed for CI pipelines:

```bash
slctl inspect --short -n slaptain slapd || echo "UNHEALTHY"
```

#### Per-Pod Fields

| Field | Source | What it tells you |
|---|---|---|
| **namingContexts** | rootDSE (anonymous) | Which databases the pod serves. RW pods should have the data suffix plus one `cn=accesslog-<database>` per replicated database (ADR-019). RO pods should have only the data suffix. |
| **contextCSN** | Each data suffix's base entry (anonymous) | The replication state vector, one section per database — see [Understanding contextCSN](#understanding-contextcsn). `none readable` means the pod serves that database but the probe got nothing back. |
| **syncRepl** | cn=config (config admin) | The `olcSyncRepl` stanzas — which peers this pod replicates from. |
| **multiProvider** | cn=config (config admin) | `TRUE` on RW pods (N-way multi-master). Absent on RO pods. |

The cn=config queries require the config admin password from the `<name>-config-password` Secret.
If the Secret is not readable (RBAC), those fields are omitted with an error message and the
corresponding checks are skipped.

#### Check Reference

| Check | Pass | Fail | Warn |
|---|---|---|---|
| **rw-replica-count** | `status.readyReplicas == spec.replicas` | Mismatch | — |
| **ro-replica-count** | `status.readOnlyReadyReplicas == spec.readReplicas` | Mismatch | — |
| **pod-readiness** | All pods Running + Ready condition True | Lists not-ready pods | — |
| **bootstrap** | `status.bootstrapComplete == true` | Not complete | — |
| **naming-contexts** | RW: one `cn=accesslog-<database>` per replicated database + data suffix. RO: data suffix only. | Missing or unexpected DB | Legacy shared `cn=accesslog` still present (mid-migration), or an orphan log with no `SlapdDatabase` |
| **accesslog-consistency** | Per replicated DB: `logbase` = `olcAccessLogDB` = log `olcSuffix`; no two DBs share a log | Local disagreement, missing overlay, absent log DB, or two DBs sharing one log (ADR-019) | Legacy shared `cn=accesslog` still in use by a single DB. External-peer `logbase` is reported as information — it is evaluated on the peer (ADR-019 R9) and never fails |
| **csn-convergence** | Per database: all pods report identical contextCSN vectors for that suffix | A database readable on some pods and not on others | Divergence on a database (groups + lag), or a database with no contextCSN anywhere |
| **syncrepl-stanza-count** | RW: `(replicas-1) + externalPeers` stanzas. RO: `replicas` stanzas. | Mismatch | — |
| **syncrepl-skip-self** | No RW pod has a syncrepl stanza pointing to itself | Self-replication detected | — |
| **multi-provider** | `TRUE` on all RW pods; absent/FALSE on RO pods | Wrong value | — |
| **rid-uniqueness** | All RIDs unique within each pod | Duplicate RIDs | — |
| **external-peers** | All external peers report `connected` in status | Lists disconnected peers | — |

Replication checks are skipped when `spec.replication.enabled` is false.

---

### `slctl debug-dump <name>`

Collects artifacts into a timestamped directory:

- CR YAML + status JSON
- Per-pod: pod YAML, container logs (current, previous, init), rootDSE, contextCSN, syncrepl config, ACLs
- Services, StatefulSets, PVCs
- Secret names and keys (never content)
- Cluster events
- Operator pod logs (filtered to this cluster)

```
$ slctl debug-dump -n slaptain-testing slapd
Collecting debug artifacts in slctl-debug-slapd-20260416-163000/
  slapdcluster.yaml
  status.json
  pod-slapd-0.yaml
  logs-slapd-0.txt
  ...
Done. 28 artifacts collected
```

---

## Understanding contextCSN

The **contextCSN** (Context Change Sequence Number) is how OpenLDAP tracks replication state.
It's a vector clock: one CSN per server that has ever originated a write.

Format: `YYYYMMDDHHMMSS.µs#count#serverID#modcount`

Example from a 3-node cluster:
```
20260416143539.536592Z#000000#000#000000
```

### What "in sync" means

**All pods report identical contextCSN vectors.** Every pod has received and applied every write
from every originator. This is the steady state for an idle or lightly loaded cluster.

### What divergence means

A `contextCSN` vector belongs to **one database on one pod** (ADR-008 amendment
2026-09-14), so the check compares each suffix only against itself and reports a
verdict per database — the cluster verdict is the worst of them, and every
non-passing segment names its suffix. Two databases are never compared to each
other: their vectors differ by construction, and the "lag" such a comparison
produces is the age gap between two unrelated write histories.

If a database's pods report different CSNs, replication is lagging on that
database. `inspect` groups the pods by CSN vector and warns:

```
[WARN] csn-convergence  dc=example,dc=org: 2 distinct CSN vectors (slapd-1 behind slapd-0 by 39.5s): [slapd-0,slapd-2] vs [slapd-1]
```

A database that is readable on some pods but not on others **fails** the check
(exit code non-zero) and names the silent pods. The cause is not knowable from
here: an initial sync that never completed, a hidden glue suffix entry, which
takes `contextCSN` with it (ADR-025), or an ACL denying this probe's anonymous
read — `suffix-visibility` discriminates the second.

This means slapd-1 hasn't received the latest write yet. In practice:
- **Transient divergence** (seconds): Normal during active writes. Recheck after a pause.
- **Persistent divergence** (minutes+): Indicates a replication problem — network partition,
  TLS certificate issue, or a pod that can't reach its peers.

### CSN and read-only replicas

RO pods consume from all RW masters. They should converge to the same CSN as the RW cluster,
possibly with a brief lag after writes. Persistent RO divergence usually means the RO pod can't
reach one or more RW masters.

---

## Interpreting syncRepl Stanzas

Each `olcSyncRepl` entry tells this pod "pull changes from that peer." The key fields:

| Field | Meaning |
|---|---|
| `rid=NNN` | Replication ID — unique within this pod. In-cluster: 001–099. External: 101+. |
| `provider=ldaps://...` | The peer URI this pod replicates from. |
| `type=refreshAndPersist` | Long-lived connection; changes pushed as they happen. |
| `searchbase=...` | What subtree to replicate (the data suffix). |

### Expected topology

For a cluster with **N RW replicas** and **M external peers**:

| Pod type | Stanza count | Targets |
|---|---|---|
| Each RW pod | `N - 1 + M` | All other RW pods (skip self) + all external peers |
| Each RO pod | `N` | All RW pods (no external peers) |

### What to look for manually

- **Missing stanza**: A pod that should replicate from peer X but doesn't — that pod won't
  receive writes originating at X (or forwarded through X).
- **Self-replication**: A RW pod with a stanza pointing to itself — configuration error,
  wastes resources.
- **Duplicate RID**: Two stanzas with the same `rid=` on one pod — undefined behavior in
  OpenLDAP (one stanza silently ignored).

All of these are caught automatically by the checks.

---

## Example Workflows

### Post-deployment validation
```bash
slctl inspect --short -n slaptain slapd
# Exit code 0 = all checks pass; non-zero = failures found
```

### Investigating replication lag
```bash
slctl inspect -n slaptain slapd --json | jq '.[].pods[] | {name, contextCSNBySuffix}'
```

### Collecting a support bundle
```bash
slctl debug-dump -n slaptain slapd
tar czf ldap-debug.tar.gz slctl-debug-slapd-*/
```
