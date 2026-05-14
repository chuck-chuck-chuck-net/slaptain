# Slaptain Test Suite

Resources for deploying and testing the `slapd` LDAP server.
All commands run from the **project root**.

---

## Quick start

Assuming the operator is installed and a TLS certificate exists in the namespace (see
[One-time prerequisites](#one-time-prerequisites)):

```bash
# Deploy: SlapdCluster (operator) + SlapdDatabase / SlapdSchema test resources
make cluster-helm-install testing-apply

# Test
make e2e-run

# Tear down
make testing-delete cluster-helm-uninstall
```

The all-in-one wrapper does the same: `./tests/e2e-singlesite.sh all <kube-context>`.

---

## One-time prerequisites

### TLS certificate

`slapd` requires a TLS secret in the target namespace. Run once per namespace (idempotent —
skips silently if the secret already exists):

```bash
make gencert
```

Creates the `slaptain-testing` namespace if needed and generates the `slapd-tls` Secret.

### Operator

```bash
make operator-helm-install
```

### Optional: extra Helm values

`make cluster-helm-install` already passes `-f tests/values.slapd.yaml` (3 RW + 1 RO with
replication enabled). Pass additional `-f` flags via `HELM_VALUES_SLAPD_CLUSTER`:

```bash
HELM_VALUES_SLAPD_CLUSTER="-f tests/values.slapd-site-b.yaml" make cluster-helm-install
```

The standalone (non-operator) chart at `charts/slapd` is kept for reference but is no longer
exercised by the test suite — use the operator path.

---

## Deploy test resources

Test fixtures (`SlapdDatabase`, `SlapdSchema`, `slapd-test-passwords` Secret) are plain
manifests under `tests/resources/example/`. They get applied with:

```bash
make testing-apply    # kubectl apply tests/resources/$TEST_RESOURCES/
make testing-delete
```

`TEST_RESOURCES` defaults to `example` (open-source fixtures). Set it to `lab` for the
internal lab variant; see `tests/resources/lab/` for SOPS-encrypted secrets used there.

---

## Run the e2e tests

```bash
./tests/e2e-singlesite.sh all <kubectl-context>
```

The single-site script provisions NodePort Services for `slapd`, each RW pod, and
each RO pod, exports `LDAP_ADDR` / `E2E_NODE_IP` / `E2E_POD_NODEPORT_BASE` /
`E2E_RO_POD_NODEPORT_BASE` for the Go test process, runs `go test ./tests/e2e`,
then tears the NodePorts down. There is no `kubectl port-forward` and no
in-cluster runner Job — both were retired in favour of NodePorts (the previous
port-forward fallback was flaky during pod restarts; the in-cluster runner
duplicated a code path that itself was never e2e-tested). Cross-site replication
testing has always used NodePorts; single-site now matches.

The script auto-discovers the LDAP base DN from the server's rootDSE — no domain
env var needed. SlapdCluster and the test resources must already be deployed
(or use the `setup` subcommand which deploys them).

| Env var | Default | Description |
|---|---|---|
| `NAMESPACE_TESTING` | `slaptain-testing` | Namespace to test in |
| `LDAP_ADDR` | *(set by script)* | `<node-ip>:<nodeport>` — required when invoking `go test` directly |
| `E2E_RESILIENCE` | *(unset)* | Set to `1` to enable slow pod-restart and warm-start tests |

**Readpw ACL tests** require plaintext passwords for the readpw service accounts. The suite
reads them from the `slapd-test-passwords` Secret (`readpw-*` keys), which is provided by
`tests/resources/example/readpw-secret.yaml`. Tests skip gracefully when keys are absent.

---

## Cross-cluster replication tests

These tests verify bidirectional delta-syncrepl between two independent SlapdClusters
running in **separate Kubernetes clusters** (siteA and siteB). They are gated by
`E2E_EXTERNAL_REPL=1` and skipped by default in `make e2e-run`.

### Topology

```
┌─────────────────────────────┐         ┌─────────────────────────────┐
│  siteA (local cluster)      │         │  siteB (remote cluster)     │
│                             │         │                             │
│  operator (+ Multus)        │         │  operator (+ Multus)        │
│  SlapdCluster "slapd"       │ ◄─────► │  SlapdCluster "slapd"       │
│    replicas: 3              │ syncrepl│    replicas: 3              │
│    externalPeers:           │         │    externalPeers:           │
│      - name: site-b         │         │      - name: site-a         │
│        discovery:           │  Multus │        discovery:           │
│          kubeconfigSecret:  │ ◄─────► │          kubeconfigSecret:  │
│            name: siteB-kc   │  or URI │            name: siteA-kc   │
│                             │         │                             │
│  test resources             │         │  (data replicates in)       │
└─────────────────────────────┘         └─────────────────────────────┘
         ▲                                         ▲
         │                                         │
         └────── test runner (your workstation) ───┘
```

The test runner runs on your workstation. It connects to siteA via `LDAP_ADDR`
(NodePort, set by `e2e-multisite.sh`) and to siteB via `E2E_REMOTE_LDAP_ADDR`
(also a NodePort).

Three cross-site connectivity modes are supported:

| Mode | ExternalPeer config | When to use |
|---|---|---|
| **Dynamic discovery** | `discovery.kubeconfigSecret` | Default with Multus. Operator queries remote k8s API over replication network |
| **Static podAddresses** | `podAddresses: [IPs]` | Multus without remote API access. Manual IP management |
| **URI (NodePort/LB)** | `uri: ldaps://host:port` | No Multus. Single endpoint per site |

### Prerequisites

Both clusters need to exist and be reachable from your workstation. The setup below uses
`KUBECONFIG` to switch between clusters. All commands run from the **project root**.

#### 1. Shared credentials

Both clusters must use the **same LDAP domain** and the **same admin + replication passwords**.
The simplest approach: create a credentials secret with known passwords, and reference it
via `credentialsSecretName` in the Helm values.

```bash
# Create identical credentials on both clusters.
for KUBECONFIG in ~/.kube/config-siteA ~/.kube/config-siteB; do
  export KUBECONFIG
  kubectl create namespace slaptain-testing --dry-run=client -o yaml | kubectl apply -f -
  kubectl create secret generic slapd-credentials \
    -n slaptain-testing \
    --from-literal=admin-password=<ADMIN_PW> \
    --from-literal=root-password=<ROOT_PW> \
    --from-literal=replication-password=<REPL_PW> \
    --dry-run=client -o yaml | kubectl apply -f -
done
```

Then set `credentials.existingSecret: slapd-credentials` in both sites' Helm values (or
add it to `tests/values.slapd.yaml`).

#### 2. TLS certificates with cross-trust

Each site needs its own TLS cert **and** the other site's CA cert:

```bash
# Generate TLS certs for each site (run once per site).
KUBECONFIG=~/.kube/config-siteA make gencert
KUBECONFIG=~/.kube/config-siteB make gencert

# Extract CA certs.
kubectl --kubeconfig=~/.kube/config-siteA -n slaptain-testing \
  get secret slapd-tls -o jsonpath='{.data.ca\.crt}' | base64 -d > /tmp/site-a-ca.crt
kubectl --kubeconfig=~/.kube/config-siteB -n slaptain-testing \
  get secret slapd-tls -o jsonpath='{.data.ca\.crt}' | base64 -d > /tmp/site-b-ca.crt

# Create cross-trust secrets: siteA gets siteB's CA and vice versa.
kubectl --kubeconfig=~/.kube/config-siteA -n slaptain-testing \
  create secret generic site-b-ca --from-file=ca.crt=/tmp/site-b-ca.crt \
  --dry-run=client -o yaml | kubectl --kubeconfig=~/.kube/config-siteA apply -f -

kubectl --kubeconfig=~/.kube/config-siteB -n slaptain-testing \
  create secret generic site-a-ca --from-file=ca.crt=/tmp/site-a-ca.crt \
  --dry-run=client -o yaml | kubectl --kubeconfig=~/.kube/config-siteB apply -f -
```

#### 3. Expose LDAP services

Each site's LDAP must be reachable from the other site over the network. The exact
method depends on your infrastructure:

- **NodePort**: `kubectl expose svc/slapd --type=NodePort --name=slapd-external`
- **LoadBalancer**: set `service.type: LoadBalancer` in values
- **VPN/direct routing**: if your clusters share a flat network, ClusterIP may suffice

The URI used in `externalPeers` must resolve from inside the slapd pod (not from your
workstation). The URI in `E2E_REMOTE_LDAP_ADDR` must be reachable from your workstation.

Example with NodePort (assuming siteB node IP is `10.0.1.50`, NodePort is `30636`):

```yaml
# siteA values: point at siteB
replication:
  enabled: true
  externalPeers:
    - name: site-b
      uri: "ldaps://10.0.1.50:30636"
      tlsSecretName: "site-b-ca"
      bindDN: "cn=replication,dc=chuck-chuck-chuck,dc=net"
      bindPasswordSecretName: "slapd-credentials"
```

```yaml
# siteB values: point at siteA
replication:
  enabled: true
  externalPeers:
    - name: site-a
      uri: "ldaps://10.0.0.50:30636"
      tlsSecretName: "site-a-ca"
      bindDN: "cn=replication,dc=chuck-chuck-chuck,dc=net"
      bindPasswordSecretName: "slapd-credentials"
```

Note: `bindPasswordSecretName` points to a Secret containing a `password` or
`replication-password` key. If you use the shared `slapd-credentials` Secret, the
operator reads the `replication-password` key from it.

**Alternative: Dynamic discovery with Multus (recommended)**

Instead of manually specifying `uri` or `podAddresses`, configure the operator to discover
remote pod IPs automatically over the replication network:

```yaml
# siteA values: discover siteB pods via remote k8s API
replication:
  enabled: true
  network:
    multusNetwork: infra/replication-net
  externalPeers:
    - name: site-b
      discovery:
        kubeconfigSecret:
          name: siteB-kubeconfig     # Created by scripts/create-remote-kubeconfig.sh
      tlsSecretName: "site-b-ca"
      bindDN: "cn=replication,dc=chuck-chuck-chuck,dc=net"
      bindPasswordSecretName: "slapd-credentials"
```

Provision the kubeconfig Secrets:

```bash
./scripts/create-remote-kubeconfig.sh \
  -n slaptain-testing \
  siteA=https://192.168.99.1:6443 \
  siteB=https://192.168.99.2:6443
```

The operator pod also needs a Multus interface to reach remote APIs. Set `multus.network`
in the operator Helm values:

```bash
helm upgrade --install slaptain-operator charts/operator \
  --set multus.network=infra/replication-net
```

#### 4. Install operator + SlapdCluster on both sites

```bash
# siteA
KUBECONFIG=~/.kube/config-siteA make operator-helm-install
KUBECONFIG=~/.kube/config-siteA make cluster-helm-install

# siteB (with its own values pointing externalPeers at siteA)
KUBECONFIG=~/.kube/config-siteB make operator-helm-install
KUBECONFIG=~/.kube/config-siteB HELM_VALUES_SLAPD_CLUSTER="-f tests/values.slapd-site-b.yaml ..." make cluster-helm-install
```

#### 5. Apply test resources on siteA only

```bash
KUBECONFIG=~/.kube/config-siteA make testing-apply
```

This creates the `default` SlapdDatabase, schemas, and the `slapd-test-passwords` Secret on
siteA. The data replicates to siteB automatically via cross-cluster syncrepl.

#### 6. Wait for replication convergence

Before running the tests, verify that data has replicated from siteA to siteB:

```bash
# Check siteA
ldapsearch -x -H ldap://localhost:13891 -D "cn=admin,dc=chuck-chuck-chuck,dc=net" \
  -w <ADMIN_PW> -b "dc=chuck-chuck-chuck,dc=net" -LLL "(ou=People)"

# Check siteB (use the routable address)
ldapsearch -x -H ldap://10.0.1.50:30389 -D "cn=admin,dc=chuck-chuck-chuck,dc=net" \
  -w <ADMIN_PW> -b "dc=chuck-chuck-chuck,dc=net" -LLL "(ou=People)"
```

Both should return `ou=People`.

### Running the tests

```bash
export E2E_REMOTE_LDAP_ADDR="10.0.1.50:30389"   # siteB address reachable from workstation
export E2E_REMOTE_ADMIN_PW="<ADMIN_PW>"           # siteB admin password (or omit if same as siteA)

make e2e-external-replication
```

This sets `E2E_EXTERNAL_REPL=1` and runs only the `external-replication` labeled tests.

| Env var | Required | Default | Description |
|---|---|---|---|
| `E2E_EXTERNAL_REPL` | set by Makefile | *(unset)* | Enables external replication tests |
| `E2E_REMOTE_LDAP_ADDR` | **yes** | — | `host:port` of siteB's LDAP, reachable from test runner |
| `E2E_REMOTE_ADMIN_PW` | no | siteA's admin password | Plaintext admin password for siteB |
| `NAMESPACE_TESTING` | no | `slaptain-testing` | siteA namespace |

If `E2E_REMOTE_ADMIN_PW` is not set, the test falls back to using siteA's admin password
(read from `slapd-passwords` in the local cluster). This works when both sites share the
same `credentialsSecretName`.

### What the tests verify

| # | Test | What it checks |
|---|---|---|
| 1 | Syncrepl stanzas present | Every RW pod on siteA has `rid=101` in `olcSyncRepl` |
| 2 | siteA → siteB | Write on siteA appears on siteB within 60 s |
| 3 | siteB → siteA | Write on siteB appears on siteA within 60 s |
| 4 | Peer removal | *(skipped — requires live CR modification)* |
| 5 | Status reporting | *(skipped — requires typed CRD client)* |

### Troubleshooting

**Syncrepl stanzas not appearing:** Check the operator logs on siteA. The `reconcileReplication`
step connects to each pod via headless DNS. If the external peer's `bindPasswordSecretName`
Secret is missing or empty, the operator logs a warning and skips that peer.

```bash
kubectl logs -n slaptain deploy/slaptain-operator-controller-manager | grep -i replication
```

**Data not replicating:** Verify syncrepl status from inside a pod:

```bash
kubectl exec -n slaptain-testing slapd-0 -c slapd -- \
  ldapsearch -H ldapi://%2frun%2fopenldap%2fslapd.ldapi \
  -Y EXTERNAL -b "olcDatabase={2}mdb,cn=config" olcSyncRepl olcMirrorMode 2>/dev/null
```

Check that the external peer URI is reachable from the pod (not just from your workstation):

```bash
kubectl exec -n slaptain-testing slapd-0 -c slapd -- \
  ldapsearch -x -H ldaps://10.0.1.50:30636 -b "" -s base 2>&1 || echo "unreachable"
```

Note: the distroless slapd image has no shell. Use the toolkit pod for more advanced debugging.

---

## Automated multi-site testing

The `e2e-multisite.sh` script automates the entire cross-cluster workflow: it deploys the
operator, SlapdClusters with mutual `externalPeers`, test resources, and runs the full e2e
suite (including external replication tests) — all from a single command.

### Prerequisites

- N Kubernetes clusters (minimum 2) reachable via kubectl contexts
- Container images pushed to a registry accessible from all clusters
- Helm 3 and Go installed on the workstation

### Quick start

**NodePort mode** (no Multus — uses NodePort services for cross-cluster replication):

```bash
make e2e-multisite CONTEXTS="s1 s2"
```

**Multus dynamic discovery** (default when `MULTUS_NETWORK` is set — recommended):

```bash
MULTUS_NETWORK=infra/replication-net make e2e-multisite CONTEXTS="s1 s2"
```

The operator queries each remote cluster's k8s API over the replication network to discover
pod Multus IPs automatically. No IPs need to be known in advance. The script provisions
cross-site RBAC and kubeconfig Secrets via `scripts/create-remote-kubeconfig.sh`, then
configures `ExternalPeer.Discovery` on the SlapdCluster CRs.

**Multus static podAddresses** (legacy — explicit IP injection):

```bash
MULTUS_NETWORK=infra/replication-net STATIC_PODADDRESSES=1 make e2e-multisite CONTEXTS="s1 s2"
```

The script waits for pods to come up, reads their Multus IPs from annotations, and patches
the SlapdCluster CRs with static `podAddresses`. Use this if the operator pod does not have
a Multus interface or the remote k8s API is not reachable over the replication network.

**Step-by-step:**

```bash
make e2e-multisite-setup    CONTEXTS="s1 s2"
make e2e-multisite-test     CONTEXTS="s1 s2"
make e2e-multisite-teardown CONTEXTS="s1 s2"
```

### What setup does

```
┌──────────────────────┐   ┌──────────────────────┐   ┌──────────────────────┐
│ s1 (context[0])      │   │ s2 (context[1])      │   │ s3 (context[2])      │
│                      │   │                      │   │                      │
│ operator (+ Multus)  │   │ operator (+ Multus)  │   │ operator (+ Multus)  │
│ SlapdCluster "slapd" │◄─►│ SlapdCluster "slapd" │◄─►│ SlapdCluster "slapd" │
│   replicas: 3        │   │   replicas: 3        │   │   replicas: 3        │
│   externalPeers:     │   │   externalPeers:     │   │   externalPeers:     │
│     site-s2, site-s3 │   │     site-s1, site-s3 │   │     site-s1, site-s2 │
│                      │   │                      │   │                      │
│ test resources       │   │ (data replicates in) │   │ (data replicates in) │
│ slapd-external (NP)  │   │ slapd-external (NP)  │   │ slapd-external (NP)  │
└──────────────────────┘   └──────────────────────┘   └──────────────────────┘
        ▲                           ▲
        │       test runner         │
        └───── (workstation) ───────┘
```

1. Discovers each cluster's node IP
2. Generates random shared credentials (admin, root, replication passwords)
3. Per cluster: creates namespace, credentials Secret, TLS cert (with node IP SAN), operator
   (with Multus annotation in dynamic discovery mode)
4. Extracts each cluster's CA, creates cross-trust Secrets on every other cluster
5. **(Dynamic discovery only)** Runs `scripts/create-remote-kubeconfig.sh` to create RBAC and
   kubeconfig Secrets for cross-site API access over the replication network
6. Deploys SlapdCluster on each cluster **without** externalPeers (Multus) or **with**
   NodePort-based externalPeers (no Multus)
7. Waits for StatefulSets and SlapdCluster to reach Running
8. Configures externalPeers:
   - **Dynamic**: Helm upgrade with `discovery.kubeconfigSecret` references
   - **Static**: Discovers Multus IPs from pod annotations, Helm upgrade with `podAddresses`
   - **NodePort**: Already configured in step 6
9. Creates `slapd-external` NodePort services (always — needed for test runner access)
10. Applies test resources, waits for convergence

### Configuration

| Env var | Default | Description |
|---|---|---|
| `CONTEXTS` | *(required)* | Space-separated kubectl context names |
| `NAMESPACE` | `slaptain` | Operator namespace |
| `NAMESPACE_TESTING` | `slaptain-testing` | Testing namespace |
| `NODEPORT_LDAP` | `30389` | NodePort for plain LDAP (test runner access) |
| `NODEPORT_LDAPS` | `30636` | NodePort for LDAPS (cross-cluster syncrepl in NodePort mode) |
| `MULTUS_NETWORK` | *(unset)* | NAD reference (e.g. `infra/replication-net`). Enables Multus mode |
| `STATIC_PODADDRESSES` | *(unset)* | Set to `1` for legacy static podAddresses instead of dynamic discovery |
| `HELM_VALUES_SLAPD_CLUSTER` | *(unset)* | Extra values for slapd-cluster chart |
| `HELM_VALUES` | *(unset)* | Extra values for operator chart |

---

## Migration scenario tests (consumer-only ↔ peer, ADR-010 3f)

Exercises the hot-migration mode transitions in a single Kubernetes cluster by
standing up two SlapdClusters in separate namespaces:

| Cluster | Namespace | Role |
|---|---|---|
| `fakeprod` | `fakeprod` | Peer-mode slaptain with `deltaSync: false` — mimics a legacy non-slaptain provider that speaks plain syncrepl |
| `slaptain` | `slaptain-target` | Starts in `mode: consumer-only` with an externalPeer pointing at `fakeprod` via in-cluster DNS |

```bash
make e2e-migration                # full setup + test + teardown
# or step-by-step:
make e2e-migration-setup
make e2e-migration-test
make e2e-migration-teardown
```

The Ginkgo spec (`tests/e2e/migration_test.go`, labeled `migration`) opens
LDAP connections to **both** clusters (each via its own NodePort) and asserts:

1. The seeded entry from `fakeprod` reaches `slaptain` via plain syncrepl.
2. Operational attributes (`entryUUID`, `creatorsName`, `createTimestamp`)
   on slaptain's copy match fakeprod's byte-for-byte — direct source-vs-target
   comparison, not relying on protocol-correctness reasoning.
3. Writes to `slaptain` in consumer-only mode are rejected with
   `unwillingToPerform` (slapd's response when `olcReadOnly: TRUE`).
4. Patching `spec.replication.mode: peer` triggers in-place promotion —
   `status.replicationMode` converges to `peer` without recreating the pod
   (asserted via pod UID + slapd container `StartedAt` comparison).
5. Writes succeed post-promotion.
6. `entryUUID` on slaptain is unchanged across the promotion (different
   invariant from #2: proves no re-sync happened — metadata-only transition).

### Configuration env vars

| Env var | Default | Purpose |
|---|---|---|
| `NAMESPACE_OPERATOR` | `slaptain` | Operator namespace |
| `NAMESPACE_FAKEPROD` | `fakeprod` | Source-cluster namespace |
| `NAMESPACE_SLAPTAIN` | `slaptain-target` | Target-cluster namespace |
| `NODEPORT_SLAPTAIN` | `30389` | NodePort for slaptain LDAP access from the test runner |
| `NODEPORT_FAKEPROD` | `30390` | NodePort for fakeprod LDAP access (source-vs-target comparison) |
| `SUFFIX` | `dc=example,dc=org` | Shared base DN |
| `SHARED_REPL_PW` | random | Plaintext replication-bind password (must match across both clusters) |

---

## Debugging with the toolkit

The slapd image is distroless and has no shell. For interactive LDAP debugging there are two
options, both based on the `slapd-toolkit` image (ldap-utils, python3, ldap3, pyyaml).

### Option A — persistent toolkit pod (`charts/slapd-toolkit`)

A long-running Deployment pre-wired to operator-managed Secrets and the cluster CA:

```bash
make toolkit-install
kubectl rollout status deployment/toolkit -n slaptain-testing

kubectl exec -it -n slaptain-testing deploy/toolkit -- bash
```

Defaults assume `clusterName: slapd` and `dbName: default` (matches `tests/resources/example/`).
Override via `--set clusterName=... --set dbName=...` if your CRs are named differently.

Pre-set inside the pod:

| Variable | Source |
|---|---|
| `$SLAPD_HOST` | `clusterName` value (resolves to in-cluster service `slapd`) |
| `$LDAP_ADMIN_PW` | `<dbName>-credentials` / `root-password` (data admin) |
| `$LDAP_ROOT_PW` | `<clusterName>-config-password` / `root-password` (cn=config admin) |
| `$LDAPTLS_CACERT` | `/etc/ldap/tls/ca.crt` (mounted from `slapd-tls` Secret) |

### Option B — `tests/pod-debug.sh` (ephemeral debug container)

Wraps `kubectl debug` against a running slapd pod, attaching the toolkit image and a custom
profile (`tests/debug-profile.json`) that grants `SYS_PTRACE`, `SYS_ADMIN`, `NET_ADMIN`, and
`NET_RAW` for low-level diagnostics.

```bash
# Interactive shell with admin/root passwords pre-loaded as env vars
./tests/pod-debug.sh -i slapd-0

# Collect mode: dumps diagnostic artifacts under pod-debug-<pod>-<timestamp>/
./tests/pod-debug.sh slapd-0
```

Use `-n <namespace>` to target a non-default test namespace.

### Option C — raw `kubectl debug`

Bare equivalent of Option B without the helper script:

```bash
kubectl debug -n slaptain-testing slapd-0 \
  --image=ghcr.io/chuck-chuck-chuck-net/slaptain/slapd-toolkit:latest \
  --target=slapd -it -- bash
```

You then need to fetch credentials yourself, e.g.:

```bash
LDAP_ADMIN_PW=$(kubectl get secret -n slaptain-testing default-credentials \
  -o jsonpath='{.data.root-password}' | base64 -d)
```

### Useful commands

```bash
# Verify connectivity (anonymous, LDAPS)
ldapsearch -x -H ldaps://$SLAPD_HOST -LLL -s base

# Inspect config DB: find database entries with their suffix and rootDN
ldapsearch -x -H ldaps://$SLAPD_HOST -D "cn=admin,cn=config" -w "$LDAP_ROOT_PW" \
  -b "cn=config" -LLL -s sub "(olcSuffix=*)" olcSuffix olcRootDN

# Discover the data baseDN from rootDSE (anonymous)
ldapsearch -x -H ldaps://$SLAPD_HOST -b "" -s base namingContexts

# Verify a readpw ACL — replace <baseDN>, <user>, <password>
ldapsearch -x -H ldaps://$SLAPD_HOST \
  -D "uid=<user>,ou=Readpw,<baseDN>" -w "<password>" \
  -b "ou=Mail,<baseDN>" -LLL -s sub "(objectClass=*)" userPassword
```

### Reading slapd logs

slapd logs use hex epoch timestamps (`69aafab1.06e09e7d`). To convert them to ISO 8601:

```bash
kubectl logs -n slaptain-testing slapd-0 | ./tests/decode-slapd-ts.sh
```
