# Slaptain — Team Onboarding and Background

This document is for team members who are new to LDAP, to OpenLDAP specifically, or to the
operator model. Its goal is to build the mental models you need to operate this system and
contribute to the codebase, without assuming prior LDAP experience.

**Source:** [github.com/chuck-chuck-chuck-net/slaptain](https://github.com/chuck-chuck-chuck-net/slaptain)

---

## Table of Contents

1. [What is LDAP and why do we use it?](#what-is-ldap-and-why-do-we-use-it)
2. [The Directory Information Tree — how LDAP data is structured](#the-directory-information-tree)
3. [How LDAP authentication works](#how-ldap-authentication-works)
4. [OpenLDAP specifics — cn=config, databases, overlays](#openldap-specifics)
5. [Replication model — multi-master delta-syncrepl](#replication-model)
6. [The operator model — what slaptain manages](#the-operator-model)
7. [Secret and credential model](#secret-and-credential-model)
8. [Common operations](#common-operations)
9. [Debugging mental model](#debugging-mental-model)

---

## What is LDAP and why do we use it?

LDAP (Lightweight Directory Access Protocol) is a protocol for reading and writing a
**directory** — a hierarchical, heavily-read-optimised database purpose-built for storing
user accounts, groups, and configuration that many applications need to look up constantly.

Think of it as the organisation's phonebook: optimised for "find the user named Alice and return her
email and group memberships" rather than for complex relational queries or high write
throughput.

**Why we use it:** applications like Dovecot (mail), Keycloak (identity), and OX App Suite
(groupware) all authenticate users against LDAP. A single LDAP directory serves as the
**source of truth** for user identities across all of them. This is the legacy the VMs were
running; we are moving it to Kubernetes.

**Why not a database?** You could store users in PostgreSQL. LDAP's advantages for this
specific use case are: a widely standardised protocol that every application knows how to
speak; optimised for millions of reads and few writes; hierarchical organisation that maps
naturally to organisations (departments, OUs); and a schema system that makes attribute types
consistent across all entries.

---

## The Directory Information Tree

LDAP data is organised as a tree called the **DIT (Directory Information Tree)**. Every item
in the tree is called an **entry**, and every entry is identified by its **DN (Distinguished
Name)** — a path from the entry down to the root of the tree, read right-to-left.

Example DN: `cn=alice,ou=People,dc=example,dc=org`

Reading right-to-left: the root of the tree is `dc=example,dc=org` (the organisation
"example.org"), inside it is `ou=People` (an organisational unit), inside that is `cn=alice`
(the user Alice).

**Key terms:**

| Term | Meaning |
|---|---|
| `dc` | Domain Component — used for the root entries, matches DNS labels (`example`, `org`) |
| `ou` | Organisational Unit — a folder-like grouping of entries |
| `cn` | Common Name — typically a person's name or a service account name |
| `dn` | Distinguished Name — the full path to an entry, its unique identifier |
| `objectClass` | A schema declaration that says what an entry represents and which attributes it must/may have |
| `attribute` | A key-value pair stored on an entry (`mail: alice@example.org`, `userPassword: {SSHA}...`) |

**Our tree structure:**

```
dc=chuck-chuck-chuck,dc=net          ← root / suffix entry
├── cn=admin                         ← data admin account
├── cn=replication                   ← syncrepl bind account (replication only)
├── ou=People                        ← user accounts
│   └── uid=alice,...
├── ou=Mail                          ← mail-specific entries
├── ou=Readpw                        ← read-only service accounts (Dovecot, Keycloak, etc.)
│   └── cn=dovecot,...
└── ou=SecondaryAccount              ← OX secondary accounts
```

**The `dc=` root entry** is mandatory — slapd refuses to store any entries unless the root
suffix entry exists first. This is why operator bootstrap adds it first.

---

## How LDAP Authentication Works

There are two completely separate password systems in OpenLDAP. Confusing them is a very
common source of errors.

### System 1: Data database authentication (`cn=admin,<domain>`)

This is the **application-level** admin. Applications (and the operator bootstrap) bind as
`cn=admin,dc=example,dc=org` with a password to read and write user data. This account exists
as a regular LDAP entry in the directory tree with a `userPassword` attribute containing an
SSHA hash.

```
dn: cn=admin,dc=chuck-chuck-chuck,dc=net
objectClass: simpleSecurityObject
objectClass: organizationalRole
userPassword: {SSHA}...
```

### System 2: Config database authentication (`cn=admin,cn=config`)

This is the **infrastructure-level** admin for slapd's own configuration database. It is
defined directly in `slapd.conf`/`cn=config` as `rootpw`, not as a directory entry. It is
used to modify slapd's runtime configuration (adding schemas, changing ACLs, etc.) without
needing to restart slapd.

These two passwords are **completely independent**. You can change the data admin password
without affecting the config admin, and vice versa.

### The rootdn shortcut

slapd has a special concept: the `rootdn` (root distinguished name) for each database. A bind
as the rootdn with the correct `rootpw` bypasses all ACL checks — the rootdn always has full
access. This is why the operator can bootstrap an empty directory: it binds as the data
rootdn (`cn=admin,<domain>`), and slapd accepts this even before the `cn=admin` entry exists
in the database, because the credential check happens against `slapd.conf`, not the DIT.

### Password hashing — {SSHA}

LDAP stores passwords as `{SSHA}base64encodedHashAndSalt`. SSHA is SHA-1 with a random salt
appended before hashing. The `slappasswd` command generates these:

```bash
slappasswd -h {SSHA} -s mysecretpassword
# {SSHA}WkJ3Y3...
```

Hashes stored in `slapd.conf` (rootpw) and in `userPassword` attributes are the same format,
but they serve different purposes: `rootpw` is for the configuration-level bind; `userPassword`
is for the directory entry bind.

---

## OpenLDAP Specifics

### cn=config — the runtime configuration database

Modern OpenLDAP stores its configuration in a special LDAP database called `cn=config`
(formerly in a flat file called `slapd.conf`). The configuration is itself a tree of LDAP
entries that can be queried and modified live using ldapmodify while slapd is running — no
restart needed for most changes.

```
cn=config                          ← global settings
├── cn=module{0},cn=config         ← loaded modules (back_mdb, syncprov, etc.)
├── cn=schema,cn=config            ← schema definitions
│   ├── cn={0}core,...
│   ├── cn={1}cosine,...
│   └── cn=ox,...                  ← our custom OX schema
├── olcDatabase={-1}frontend,...   ← the "frontend" pseudo-database (global ACLs)
├── olcDatabase={0}config,...      ← the config database itself
│   └── rootpw: {SSHA}...          ← config admin password
└── olcDatabase={1}mdb,...         ← the main data database
    ├── olcSuffix: dc=chuck-chuck-chuck,dc=net
    ├── olcRootDN: cn=admin,...
    ├── olcRootPW: {SSHA}...
    └── olcSyncRepl: ...           ← replication configuration (one per peer)
```

Our init container generates a flat `slapd.conf` file and then runs `slaptest` to convert it
to this `cn=config` format stored in the `/ldap-config` volume.

### Databases and backends

slapd can manage multiple databases. We use two:

- **`back_mdb`** (main data): the LMDB-backed database that stores the actual directory
  entries. Fast, crash-safe, memory-mapped. Our data lives here under `dc=chuck-chuck-chuck`.
- **`cn=accesslog`**: a second `back_mdb` database that the `accesslog` overlay writes to.
  Every write to the main database generates a structured entry here recording the DN, type
  of operation, and the changes made. This is what delta-syncrepl reads.

### Overlays

Overlays are slapd plugins that intercept operations and add functionality. We use two:

- **`syncprov`** (Sync Provider): makes this slapd instance act as a replication provider.
  It intercepts write operations and writes contextCSN (change sequence numbers) that
  consumers use to track their position.
- **`accesslog`**: writes a detailed log entry to `cn=accesslog` for every successful write.
  Required for delta-syncrepl (see [Replication Model](#replication-model)).

### ACLs — Access Control Lists

ACLs in slapd control who can read or write what. They are evaluated in order and the first
matching rule wins.

**Important:** ACLs live in `cn=config`, which is **node-local** — it is never replicated
between pods. Each pod has its own independent `cn=config`. A one-time `ldapmodify` to one
pod leaves all other pods unchanged, and a pod replacement (node failure, rolling update)
re-runs the init container, resetting `cn=config` to the generated defaults.

The operator solves this by managing ACLs centrally via `spec.ldap.acls` on the `SlapdCluster`
CR. On every reconcile loop the operator reads each pod's current `olcAccess` values and
patches any pod whose ACLs have drifted from the desired state. This means ACL changes are
applied to all pods simultaneously and are automatically re-applied after pod replacement.

The init-container default ACLs (used when `spec.ldap.acls` is empty):

On the accesslog database — only the replication account can read it:
```
access to *
  by dn.exact="cn=replication,<domain>" read
  by * none
```

On the main data database — `userPassword` is protected; everything else is readable:
```
access to attrs=userPassword
  by self write
  by anonymous auth
  by * none

access to *
  by dn.exact="cn=replication,<domain>" read
  by * read
```

Production deployments typically add tighter ACLs — for example, restricting `userPassword`
reads in specific OUs to named service accounts. These are declared in `spec.ldap.acls`; see
`operator/config/samples/ldap_v1alpha1_slapdcluster.yaml` for an example.

---

## Replication Model

### Why replication?

A single LDAP node is a single point of failure for every application that authenticates
through it. If slapd goes down, users cannot log in to anything. We replicate across three
nodes for high availability: the Kubernetes Service load-balances reads across all three, and
any node can serve writes.

### Delta-syncrepl: incremental change propagation

OpenLDAP uses a replication protocol called **syncrepl**. In `refreshAndPersist` mode (which we
use), every replica (consumer) opens a persistent connection to each provider, does an initial
catch-up, then the provider **pushes changes in real-time** over the same connection.
The CSN (Change Sequence Number) is a timestamp-based identifier that tracks the position in
the change stream.

**Delta-syncrepl** is the efficient variant: instead of sending the full entry on every
change, the provider sends only the diff (which attributes changed and to what values). The
diff is read from the `cn=accesslog` database rather than recomputed from the entry. This is
why the accesslog overlay is essential — without it, slapd would have to do a full entry
comparison on every sync.

### Multi-master mirrormode

In our setup, every node is both a provider and a consumer for every other node. A write to
any node is replicated to all others. This is called **multi-master** or **N-way multi-master**.

OpenLDAP calls its implementation **mirrormode**. Without `mirrormode on`, having multiple
slapds each claiming to be a provider for the same data would cause replication loops (node A
sends a change to node B, node B sends it back to node A, infinitely). Mirrormode suppresses
the loop: a change that arrives via replication is not re-replicated outward.

### The bootstrap challenge

When a new cluster is created, all nodes start with an empty database. The challenge is:
**who writes the initial entries?** We solve this by having the operator connect to pod-0
(the first pod in the StatefulSet, which starts before the others) via a live LDAP connection
and adding the root entries. Because this goes through the running slapd, the accesslog
captures every write. When pod-1 and pod-2 start and connect to pod-0 as consumers, they pull
those entries via the accesslog and their databases become identical to pod-0.

### Connection topology

Every node makes one persistent outbound connection per peer:

```
slapd-0 ──→ slapd-1  (slapd-0 as consumer pulling from slapd-1)
slapd-0 ──→ slapd-2
slapd-1 ──→ slapd-0
slapd-1 ──→ slapd-2
slapd-2 ──→ slapd-0
slapd-2 ──→ slapd-1
```

6 connections for 3 nodes. These connections stay open permanently; each consumer continuously
monitors its provider's accesslog for new changes.

---

## The Operator Model

### What the operator does

The `slaptain` operator watches `SlapdCluster` custom resources and ensures the actual cluster
state matches the desired state defined in the CR. It manages:

1. **Secrets** — generates passwords and creates `<name>-passwords` and
   `<name>-config-password` Secrets (create-only, never updated)
2. **Services** — headless Service (for stable pod DNS) and ClusterIP Service (for client
   access)
3. **StatefulSet** — the slapd pod deployment, including passing all configuration as env
   vars to the init container
4. **Bootstrap** — adds the initial LDAP entries to pod-0 via live LDAP once it is ready
5. **cn=config ACLs** — applies `spec.ldap.acls` to every pod's `cn=config` individually
   via headless service DNS on every reconcile; self-heals after pod replacement

The operator uses **server-side apply** (SSA) for all Kubernetes resource updates. This means
the operator sends its desired state and the API server merges it with whatever other
controllers have written — no "object has been modified" conflicts and no accidental
overwriting of fields owned by other controllers (like StatefulSet readiness conditions).

### The reconcile loop

The operator's reconcile loop runs:
- When the `SlapdCluster` CR changes
- When any owned resource (StatefulSet, Service, Secret) changes
- Every 10 seconds while the cluster is not fully Running (to poll for readiness)
- Never, once the cluster is Running (event-driven only after that)

The loop is idempotent — running it twice in a row produces the same result as running it
once. This is by design: Kubernetes controllers can be restarted at any time and must always
converge to the correct state.

### Read-only replicas

When `spec.readReplicas > 0`, the operator creates a second StatefulSet (`<name>-readonly`)
with pure consumer pods. These replicas pull data from all RW masters via delta-syncrepl but
never accept writes — they have no accesslog, no syncprov overlay, and no mirrormode. They
are useful for scaling read-heavy workloads (e.g. Dovecot auth lookups) without adding write
complexity. Each RO pod gets the same ACLs as RW pods (since cn=config is node-local).

### What the operator does not do

- **Schema management** — custom schemas are loaded by the slapd-test bootstrap job (which
  connects to cn=config directly). Because cn=config is node-local, this currently applies
  to one pod only. Operator-managed schema extensions (`spec.ldap.schemas`) are on the Phase 3
  backlog; in the interim schemas must be identical in the init-container's generated
  `slapd.conf` or applied manually to each pod.
- **User management** — creating, modifying, or deleting user accounts is the application's
  responsibility. The operator creates the framework (OUs, admin account, replication
  account); business data goes in via the application.
- **Scale-out after creation** — changing the replica count on a running cluster is Phase 3
  scope. Currently, the replica count is fixed at creation time.

---

## Secret and Credential Model

Three operator-managed credentials flow through the system, split across the cluster-level and
per-database scope (post-ADR-004). Understanding where each one is used prevents confusion when
something fails to authenticate.

| Password | Where defined | Who uses it | Secret / key |
|---|---|---|---|
| Config admin | `cn=config` rootpw | Operator: ACL, schema, replication, topology ops on `cn=config` | `<cluster>-config-password` / `root-password` |
| Data admin   | per-DB `olcRootPW` + `cn=admin,<suffix>` | Operator: bootstrap, ACL/seed/replication ops on the data DB | `<dbname>-credentials` / `root-password` |
| Replication bind | `cn=replication,<suffix>` `userPassword` | Inter-pod and cross-site syncrepl | `<dbname>-credentials` / `replication-password` |

The operator creates each of these Secrets create-only — the value is generated if the Secret
does not exist, and never updated thereafter. You can also pre-create a Secret with the schema
above to bring your own credentials (see below).

A fourth category — application accounts in the data tree (`readpw-*` users, regular user
passwords) — is the application's responsibility and outside the operator's scope.

**Why plaintext Secrets?** The operator needs to bind to slapd using the plaintext password
(LDAP SIMPLE bind sends the password in the clear over the wire — TLS protects it in transit
but the protocol itself does not hash). Hashing is slapd's job, done at storage time. Storing
pre-hashed passwords in Kubernetes Secrets would mean the operator can never verify a bind or
change a password without knowing the plaintext — making it unmanageable. Every database
operator (Percona, CloudNativePG, Strimzi) uses the same pattern.

### Bringing your own credentials (e.g. for migration)

To preserve an admin password through a migration from a legacy slapd, create the credentials
Secret **before** creating the SlapdDatabase / SlapdCluster CR — the operator's create-only
logic will adopt your Secret instead of generating one.

```bash
# Per-database (cn=admin,<suffix>) + replication bind:
kubectl create secret generic <dbname>-credentials \
  --from-literal=root-password='<plaintext-admin-pw>' \
  --from-literal=replication-password='<plaintext-replication-pw>'

# Cluster-level (cn=config admin) — only needed if you also want to pin this:
kubectl create secret generic <cluster>-config-password \
  --from-literal=root-password='<plaintext-config-pw>'
```

Then reference (or rely on the default name) from the CR:

```yaml
# SlapdDatabase
spec:
  credentials:
    secretName: <dbname>-credentials   # optional; default is "<metadata.name>-credentials"
# SlapdCluster
spec:
  ldap:
    cnConfigCredentials:
      secretName: <cluster>-config-password   # optional
```

**You must provide plaintext, not the legacy `{SSHA}` hash.** SSHA includes a random salt, so
"the same plaintext produces the same stored hash" was never true to begin with — clients bind
with plaintext and slapd verifies against its stored hash regardless of salt. Preserving the
plaintext (not the stored hash) is what keeps cached client credentials working through the
migration. The operator must know the plaintext to bind to slapd for ongoing ACL / schema /
replication management; an `{SSHA}` Secret cannot satisfy that.

---

## Common Operations

### Check cluster status

```bash
kubectl get slapdcluster -n slaptain-testing
# NAME    PHASE     READY   REPLICAS   AGE
# slapd   Running   3       3          5m

kubectl describe slapdcluster slapd -n slaptain-testing
# Shows conditions, bootstrapComplete, etc.
```

### Read the admin password (for manual ldap operations)

```bash
kubectl get secret -n slaptain-testing slapd-passwords \
  -o jsonpath='{.data.admin-password}' | base64 -d
```

### Run an ldapsearch against the cluster

```bash
ADMIN_PW=$(kubectl get secret -n slaptain-testing slapd-passwords \
  -o jsonpath='{.data.admin-password}' | base64 -d)

kubectl exec -n slaptain-testing slapd-0 -c slapd -- \
  ldapsearch -H ldap://localhost:1024 \
  -D "cn=admin,dc=chuck-chuck-chuck,dc=net" \
  -w "$ADMIN_PW" \
  -b "dc=chuck-chuck-chuck,dc=net" \
  "(objectClass=*)"
```

### Check that all three nodes have the same data

```bash
ADMIN_PW=$(kubectl get secret -n slaptain-testing slapd-passwords \
  -o jsonpath='{.data.admin-password}' | base64 -d)

for pod in slapd-0 slapd-1 slapd-2; do
  echo -n "$pod: "
  kubectl exec -n slaptain-testing $pod -c slapd -- \
    ldapsearch -H ldap://localhost:1024 \
    -D "cn=admin,dc=chuck-chuck-chuck,dc=net" \
    -w "$ADMIN_PW" \
    -b "dc=chuck-chuck-chuck,dc=net" \
    -s sub "(objectClass=*)" dn 2>/dev/null | grep -c "^dn:"
done
```

### Force rebootstrap (destroys all data)

```bash
# 1. Set the flag
kubectl patch slapdcluster slapd -n slaptain-testing \
  --type=merge -p '{"spec":{"ldap":{"forceRebootstrap":true}}}'

# 2. Restart pods so the init container runs
kubectl rollout restart statefulset/slapd -n slaptain-testing
kubectl rollout status statefulset/slapd -n slaptain-testing

# 3. Reset the flag (so next restart doesn't wipe again)
kubectl patch slapdcluster slapd -n slaptain-testing \
  --type=merge -p '{"spec":{"ldap":{"forceRebootstrap":false}}}'

# 4. Reset bootstrapComplete so the operator re-runs bootstrap
kubectl patch slapdcluster slapd -n slaptain-testing \
  --subresource=status --type=merge \
  -p '{"status":{"bootstrapComplete":false}}'
```

### Inspect slapd's running configuration

```bash
# List all syncrepl stanzas (shows who each pod is replicating from)
kubectl exec -n slaptain-testing slapd-0 -c slapd -- \
  ldapsearch -H ldapi://%2frun%2fopenldap%2fslapd.ldapi \
  -Y EXTERNAL -b cn=config -s sub \
  "(objectClass=olcSyncRepl)" olcSyncRepl 2>/dev/null

# Show ACLs on the data database
kubectl exec -n slaptain-testing slapd-0 -c slapd -- \
  ldapsearch -H ldapi://%2frun%2fopenldap%2fslapd.ldapi \
  -Y EXTERNAL -b "olcDatabase={1}mdb,cn=config" \
  -s base "(objectClass=*)" olcAccess 2>/dev/null
```

---

## Debugging Mental Model

When something is broken, the fault almost always lies in one of four places. Work through
them in this order:

**1. Init container — did configuration generate correctly?**
```bash
kubectl logs -n slaptain-testing slapd-0 -c init
```
Look for: `slaptest` errors, `ERROR:` lines, or "Replication enabled: ..." messages.
If slaptest failed, slapd will start with a broken or empty cn=config.

**2. slapd logs — is slapd accepting connections?**
```bash
kubectl logs -n slaptain-testing slapd-0 -c slapd
```
Look for: BIND errors (`err=49` = wrong password, `err=32` = no such object), TLS errors,
or no inbound replication connections at all after ~30 seconds.

**3. Operator logs — did bootstrap complete?**
```bash
kubectl logs -n slaptain -l app.kubernetes.io/name=slaptain-operator
```
Look for: "bootstrap complete", "pod-0 not yet ready" (if still waiting), or error messages
with stack traces.

**4. Secrets — are credentials consistent?**
```bash
# The admin password in the Secret must match what slapd was started with.
# If they differ, the operator bootstrap bind will get err=49.
kubectl get secret -n slaptain-testing slapd-passwords -o json | jq '.data | keys'
# Should show: admin-password, replication-password
kubectl get secret -n slaptain-testing slapd-config-password -o json | jq '.data | keys'
# Should show: root-password
```

**5. Ephemeral debug container — inspect the filesystem or run LDAP queries from inside the pod**

The slapd runtime image is distroless (no shell). Use an ephemeral debug container with the
`slapd-toolkit` image, which has `bash`, `ldapsearch`, `ldapmodify`, and `python3`:

```bash
kubectl debug -n slaptain-testing slapd-0 -it \
  --image=ghcr.io/chuck-chuck-chuck-net/slaptain/slapd-toolkit:latest \
  --image-pull-policy=Never \
  --target=slapd -- bash
```

Notes:
- `--image-pull-policy=Never` uses the image already present on the node (avoids pull credential
  issues).
- `--target=slapd` shares the process namespace with the slapd container so you can access its
  filesystem via `/proc/1/root/` (slapd is PID 1 in its container).
- You are not root in the debug container, but you have enough access for most tasks.

For root access (needed for e.g. `/proc/1/task/*/stack` thread traces), use the custom
debug profile in `tests/debug-profile.json`:
```bash
kubectl debug -n slaptain-testing slapd-0 -it \
  --image=ghcr.io/chuck-chuck-chuck-net/slaptain/slapd-toolkit:latest \
  --image-pull-policy=Never \
  --target=slapd \
  --custom=tests/debug-profile.json \
  -- bash
```

Example: inspect the cn=config schema files:
```bash
ls /proc/1/root/ldap-config/cn=config/cn=schema/
```

**Automated debug script (`tests/pod-debug.sh`)**

There is a convenience script that wraps the above into two modes:

```bash
# Collect all diagnostic artifacts (thread states, strace, LDAP smoke test, etc.)
./tests/pod-debug.sh [-n namespace] <pod-name>

# Interactive shell with credentials pre-loaded as $ROOT_PW, $ADMIN_PW, $BASE_DN
./tests/pod-debug.sh [-n namespace] -i <pod-name>
```

The `-i` flag fetches credentials from the Kubernetes secrets, launches a root debug container,
and drops you into a bash shell with `$ROOT_PW` (cn=config admin), `$ADMIN_PW` (data admin),
and `$BASE_DN` ready to use. It also prints example `ldapsearch` commands on entry.

Note: kubectl debug ephemeral containers cannot mount Secrets or use `envFrom`, so there is no
way to inject credentials via Helm or Pod spec. The script handles this by fetching credentials
via `kubectl get secret` before launching the container and embedding them in the startup command.

**6. Deep process inspection — diagnosing hangs and deadlocks**

When slapd appears running (pod is `Running, ready`) but is unresponsive to LDAP queries, use
the root debug container to inspect the process internals:

```bash
# Thread states — are threads sleeping normally or stuck on locks?
for t in /proc/1/task/*/; do tid=$(basename $t); printf "TID $tid: wchan="; cat $t/wchan 2>/dev/null; echo; done
```

Healthy slapd shows most threads in `ep_poll` (idle, waiting for connections) or `poll_schedule_timeout`.
If most threads show `futex_do_wait`, they are blocked on mutexes — likely a deadlock.

```bash
# Kernel stack traces — shows exactly where each thread is blocked
cat /proc/1/task/*/stack

# Attach strace to see if any syscalls are happening (5 second sample)
timeout 5 strace -p 1 -f -e trace=futex,write,recvmsg -t 2>&1 | tail -30

# Smoke test — does slapd respond to any LDAP query at all?
ldapsearch -x -H ldap://localhost:1024 -b "" -s base namingContexts

# TCP connections — who is connected?
cat /proc/1/net/tcp
```

**Interpreting results:**

| Observation | Diagnosis |
|---|---|
| All threads `futex_do_wait`, strace produces zero output in 5s, ldapsearch hangs | **Deadlock.** slapd is permanently stuck. Pod restart required to recover. |
| Threads in `ep_poll`, strace shows activity, ldapsearch responds | slapd is healthy; problem is elsewhere (network, operator, DNS). |
| Some threads `futex_do_wait`, strace shows some activity | Partial lock contention; may recover on its own or may be progressing slowly. |

**Known deadlock trigger (OpenLDAP 2.6.10):** Adding a schema entry to `cn=config` that
already exists (by a different DN, e.g. `cn=ox` vs `cn={4}ox`) while active syncrepl threads
are running can deadlock slapd. The `cn=config` write path and syncrepl threads compete for
internal locks with no timeout. See `docs/reconcile-loop-fixes.md` for the operator-side fix.

There is also an automated script that collects all of the above into a timestamped directory:
```bash
./tests/pod-debug.sh [-n namespace] <pod-name>
```

**Common failure modes:**

| Symptom | Likely cause |
|---|---|
| `BIND err=49` in slapd logs (for `cn=admin` from operator) | Operator Secret has different password than what was used when slapd.conf was generated — the Secrets and config are out of sync. Force rebootstrap. |
| `BIND err=49` in slapd logs (for `cn=replication` from peer) | Replication password in Secret differs from what was baked into cn=config. Force rebootstrap. |
| `BIND err=32` in slapd logs | The DN does not exist — bootstrap did not complete. Check operator logs. |
| No inbound replication connections ever | syncrepl stanzas not in cn=config — check init container logs for slaptest errors. |
| `consumer has state info but provider doesn't!` | Initial data was loaded via slapadd while accesslog was active. Force rebootstrap on all nodes. |
| `status.bootstrapComplete=true` but directory is empty | BootstrapComplete was set from a previous deployment. Patch it to false and allow operator to re-run. |
| Operator keeps logging "pod-0 not yet ready" | StatefulSet not healthy — check pod events and slapd init container logs. |
