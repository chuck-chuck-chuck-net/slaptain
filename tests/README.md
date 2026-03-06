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
