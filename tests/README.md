# Slaptain Test Suite

Resources for deploying and testing the `slapd` LDAP server.
All commands run from the **project root**.
For SOPS secret management, see [SOPS.md](SOPS.md).

---

## Quick start

Assuming the operator is installed and a TLS certificate exists in the namespace (see
[One-time prerequisites](#one-time-prerequisites)):

```bash
# Deploy
make cluster-helm-install testing-helm-install

# Test
make e2e-run

# Tear down
make testing-helm-uninstall cluster-helm-uninstall
```

The standalone path (no operator) is equivalent: replace `cluster-helm-install` /
`cluster-helm-uninstall` with `helm-install` / `helm-uninstall`.

---

## One-time prerequisites

### TLS certificate

`slapd` requires a TLS secret in the target namespace. Run once per namespace (idempotent —
skips silently if the secret already exists):

```bash
make gencert
```

Creates the `slaptain-testing` namespace if needed and generates the `slapd-tls` Secret.

### Operator (operator path only)

Only needed if you want to test against an operator-managed `SlapdCluster`:

```bash
make operator-helm-install
```

### Environment variables

The Helm install targets read values from environment variables. Set them in your shell or
`.envrc`:

```bash
# Values for the slapd / slapd-cluster chart
export HELM_VALUES_SLAPD="-f tests/values.slapd.yaml -f secrets://tests/values.slapd.secret.yaml"
export HELM_VALUES_SLAPD_CLUSTER="-f tests/values.slapd.yaml -f secrets://tests/values.slapd.secret.yaml"

# Values for the slapd-test chart.
# The non-secret file holds deployment-specific overrides (custom schema, extra OUs, …).
# The secret file holds plaintext passwords — copy from *.sample and fill in.
export HELM_VALUES_SLAPD_TESTING="-f tests/values.slapd-test.yaml -f secrets://tests/values.slapd-test.secret.yaml"
```

Secret templates to fill in (copy, rename, populate, encrypt with SOPS):

| Template | Contents |
|---|---|
| `tests/values.slapd.secret.yaml.sample` | Admin/root SSHA password hashes for slapd |
| `tests/values.slapd-test.secret.yaml.sample` | Admin/root plaintext passwords + readpw user hashes and plaintext passwords |

---

## Deploy slapd

Two deployment paths are available — pick one per environment. Both produce identical service
names (`slapd`) so the test suite and slapd-test chart work identically with either.

### Option A — Standalone Helm chart (`charts/slapd`)

```bash
make helm-install    # deploy
make helm-uninstall  # tear down
```

### Option B — Operator-managed `SlapdCluster` (`charts/slapd-cluster`)

```bash
make cluster-helm-install    # deploy (operator must be running)
make cluster-helm-uninstall  # tear down
```

`charts/slapd-cluster` creates a `SlapdCluster` CR; the operator reconciles the StatefulSet,
Services, and PVCs.

---

## Deploy slapd-test

`slapd-test` bootstraps the LDAP directory and provides a persistent toolkit pod. Install
after slapd is ready:

```bash
make testing-helm-install
make testing-helm-uninstall  # tear down
```

| Component | Default | What it does |
|---|---|---|
| `bootstrap` | enabled | One-shot Job: loads custom schema (if configured), creates OUs and readpw service accounts |
| `toolkit` | enabled | Long-running pod for interactive `kubectl exec` sessions |

Watch the bootstrap Job complete:

```bash
kubectl logs -n slaptain-testing -l app.kubernetes.io/name=slapd-test -f
```

To install the toolkit only (skip bootstrap — useful for a clean, unmodified slapd):

```bash
make testing-helm-install TOOLKIT_ONLY=true
```

---

## Run the e2e tests

### Option A — Local (kubectl port-forward)

```bash
make e2e-run
```

Runs from your workstation. The suite starts a `kubectl port-forward svc/slapd` tunnel
automatically. Works well for quick iteration but can be flaky due to port-forward reconnect
latency after pod restarts (especially in resilience tests).

### Option B — In-cluster (recommended for CI and resilience tests)

```bash
make e2e-in-cluster
```

Builds and pushes an `e2e-runner` image, deploys it as a Kubernetes Job in the test namespace,
and streams logs. The test pod connects directly to `slapd.slaptain-testing.svc.cluster.local`
— no port-forward, so pod restarts don't break the LDAP connection. Resilience tests (pod
restarts, warm start) are **always enabled** in this mode since the port-forward flakiness
that motivated the `E2E_RESILIENCE` gate doesn't apply in-cluster.

The in-cluster runner requires a one-time RBAC setup (applied automatically by the target)
and the `e2e-runner` image in the registry.

### Common options

Both modes auto-discover the LDAP base DN from the server's rootDSE — no domain env var
needed. Both assume slapd and slapd-test are already installed.

| Env var | Default | Description |
|---|---|---|
| `NAMESPACE_TESTING` | `slaptain-testing` | Namespace to test in |
| `LDAP_SVC` | `svc/slapd` | Service to port-forward for LDAP access (local mode only) |
| `E2E_RESILIENCE` | *(unset)* | Set to `1` to enable slow pod-restart and warm-start tests |

**Readpw ACL tests** require plaintext passwords for the readpw service accounts. Set
`bootstrap.readpwPasswords` in your `tests/values.slapd-test.secret.yaml` (see `.sample`).
These tests skip gracefully when not configured.

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
│  operator                   │         │  operator                   │
│  SlapdCluster "slapd"       │ ◄─────► │  SlapdCluster "slapd"       │
│    replicas: 3              │ syncrepl│    replicas: 3              │
│    externalPeers:           │         │    externalPeers:           │
│      - name: site-b         │         │      - name: site-a         │
│        uri: ldaps://siteB   │         │        uri: ldaps://siteA   │
│                             │         │                             │
│  slapd-test (bootstrap+OUs) │         │  (no slapd-test needed)     │
└─────────────────────────────┘         └─────────────────────────────┘
         ▲                                         ▲
         │                                         │
         └────── test runner (your workstation) ───┘
```

The test runner runs on your workstation (or in siteA). It connects to siteA via
`kubectl port-forward` (or direct if `LDAP_ADDR` is set) and to siteB via
`E2E_REMOTE_LDAP_ADDR` (a routable address — NodePort, LoadBalancer, or VPN).

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

#### 4. Install operator + SlapdCluster on both sites

```bash
# siteA
KUBECONFIG=~/.kube/config-siteA make operator-helm-install
KUBECONFIG=~/.kube/config-siteA make cluster-helm-install

# siteB (with its own values pointing externalPeers at siteA)
KUBECONFIG=~/.kube/config-siteB make operator-helm-install
KUBECONFIG=~/.kube/config-siteB HELM_VALUES_SLAPD_CLUSTER="-f tests/values.slapd-site-b.yaml ..." make cluster-helm-install
```

#### 5. Install slapd-test on siteA only

```bash
KUBECONFIG=~/.kube/config-siteA make testing-helm-install
```

The bootstrap job creates OUs and test users on siteA. These replicate to siteB
automatically via the cross-cluster syncrepl.

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

## Using the toolkit

The toolkit pod has `ldap-utils`, `python3`, `ldap3`, and `pyyaml` pre-installed.

```bash
# Wait for it to be ready
kubectl rollout status deployment/slapd-test-toolkit -n slaptain-testing

# Exec in
kubectl exec -it -n slaptain-testing deploy/slapd-test-toolkit -- bash
```

Inside the pod, all required environment variables are pre-set by the chart:

| Variable | Example value |
|---|---|
| `$SLAPD_HOST` | `slapd` |
| `$LDAP_DOMAIN` | `dc=as8,dc=lab,dc=test` |
| `$READPW_OU` | `Readpw` |
| `$LDAP_ADMIN_PW` | data admin password (plaintext, from Secret) |
| `$LDAP_ROOT_PW` | config admin password (plaintext, from Secret) |
| `$LDAPTLS_CACERT` | `/etc/ldap/tls/ca.crt` |

Config files and bootstrap scripts are mounted at `/config/`.

### Useful commands

```bash
# Verify connectivity (anonymous, LDAPS)
ldapsearch -x -H ldaps://$SLAPD_HOST -LLL -s base

# Inspect config DB: find database entries with their suffix and rootDN
ldapsearch -x -H ldaps://$SLAPD_HOST -D "cn=admin,cn=config" -w "$LDAP_ROOT_PW" \
  -b "cn=config" -LLL -s sub "(olcSuffix=*)" olcSuffix olcRootDN

# Run the full bootstrap (same as the Job)
bash /config/bootstrap.sh

# Verify readpw ACL: a readpw user should be able to read userPassword from ou=Mail
# (replace <user> and <password> with a configured readpw account)
ldapsearch -x -H ldaps://$SLAPD_HOST \
  -D "uid=<user>,ou=$READPW_OU,$LDAP_DOMAIN" -w "<password>" \
  -b "ou=Mail,$LDAP_DOMAIN" -LLL -s sub "(objectClass=*)" userPassword
```

### Reading slapd logs

slapd logs use hex epoch timestamps (`69aafab1.06e09e7d`). To convert them to ISO 8601:

```bash
kubectl logs -n slaptain-testing slapd-0 | ./tests/decode-slapd-ts.sh
```
