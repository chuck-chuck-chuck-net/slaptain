# Slaptain Test Suite

Resources for deploying and testing the `slapd` standalone LDAP server.
All commands are run from the **project root**.

For secret management with SOPS and age, see [SOPS.md](SOPS.md).

---

## 1. Generate TLS certificates

The `slapd` chart requires a TLS secret in the target namespace. Generate it with:

```bash
make gencert
```

This creates the namespace (`slaptain-testing`) if it does not exist and runs `tests/gencert.sh`
to produce the `slapd-tls` Secret.

## 2. Prepare your environment

The make targets read environment variables for helm values arguments. You'll want to set something like

```
export HELM_VALUES_SLAPD="-f tests/values.slapd.yaml -f secrets://tests/values.slapd.secret.yaml"
export HELM_VALUES_SLAPD_CLUSTER="-f tests/values.slapd.yaml -f secrets://tests/values.slapd.secret.yaml"
export HELM_VALUES_SLAPD_TESTING="-f secrets://tests/values.slapd-test.secret.yaml"
```

The samples below assume the variables are provided by the environment. You could also pass
them on each make invocation command line explicitly, of course.

---

## 2. Deploy slapd

Two deployment paths are supported. They are mutually exclusive — pick one per test environment.

### Option A — Standalone Helm chart (`charts/slapd`)

No operator required. The chart deploys the StatefulSet directly.

```bash
make helm-install
```

Full build-and-deploy from scratch (build images, generate certs, install):

```bash
make helm-deploy
```

To remove:

```bash
make helm-uninstall
```

### Option B — Operator-managed SlapdCluster (`charts/slapd-cluster`)

Requires the operator to be running first:

```bash
make operator-helm-install
```

Then deploy a SlapdCluster instance:

```bash
make cluster-helm-install
```

`charts/slapd-cluster` creates a password Secret and a `SlapdCluster` CR; the operator
reconciles all other resources (StatefulSet, Services, PVCs).

To remove:

```bash
make cluster-helm-uninstall
```

---

## 3. Bootstrap and test with slapd-test

The `slapd-test` chart has two components, controlled independently:

| Component | Default | What it does |
|---|---|---|
| `bootstrap` | enabled | Runs a Job that applies schema, ACLs, and initial directory data |
| `toolkit` | enabled | Keeps a pod alive for interactive `kubectl exec` sessions |

### Default: bootstrap + toolkit

```bash
make testing-helm-install
```

Watch the bootstrap job run:

```bash
kubectl logs -n slaptain-testing -l app.kubernetes.io/name=slapd-test -f
```

### Toolkit only (no bootstrap)

Useful when you want a clean, unmodified slapd to inspect or test manually:

```bash
make testing-helm-install TOOLKIT_ONLY=true
```

`TOOLKIT_ONLY=true` sets `bootstrap.enabled=false` and `toolkit.enabled=true`.

To remove:

```bash
make testing-helm-uninstall
```

---

## 4. End-to-end tests

The e2e suite in `tests/e2e/` works against both deployment options.

The suite assumes the cluster is already set up (slapd + slapd-test both installed).
Run the same command regardless of whether slapd was deployed via the standalone chart or
the operator — the service name and password secret are identical either way:

```bash
make e2e-run
```

| Env var | Default | Description |
|---|---|---|
| `NAMESPACE_TESTING` | `slaptain-testing` | Namespace to test in |
| `LDAP_DOMAIN` | `dc=as8,dc=lab,dc=test` | Base DN of the LDAP tree |
| `LDAP_SVC` | `svc/slapd` | Service to port-forward for LDAP access |

---

## 5. Using the toolkit

The toolkit pod has `ldap-utils`, `python3`, `ldap3`, and `pyyaml` pre-installed.

Wait for it to be ready:

```bash
kubectl rollout status deployment/slapd-test-toolkit -n slaptain-testing
```

Exec in:

```bash
kubectl exec -it -n slaptain-testing deploy/slapd-test-toolkit -- bash
```

Inside the pod, all required environment variables are pre-set:

| Variable | Example value |
|---|---|
| `$SLAPD_HOST` | `slapd` |
| `$LDAP_DOMAIN` | `dc=as8,dc=lab,dc=test` |
| `$LDAP_ADMIN_PW` | admin password (from Secret) |
| `$LDAP_ROOT_PW` | rootDN password (from Secret) |
| `$LDAPTLS_CACERT` | `/etc/ldap/tls/ca.crt` |

Config files and bootstrap scripts are mounted at `/config/`.

### Useful commands

```bash
# Verify connectivity (anonymous, LDAPS)
ldapsearch -x -H ldaps://$SLAPD_HOST -LLL -s base

# Verify config DB access
ldapsearch -x -H ldaps://$SLAPD_HOST -D "cn=admin,cn=config" -w "$LDAP_ROOT_PW" \
  -b "cn=config" -LLL -s base

# Run the full bootstrap (same as the Job)
bash /config/bootstrap.sh

# Run with trace output
bash -x /config/bootstrap.sh

# Verify ACLs (subtree may be empty, but access should succeed)
ldapsearch -x -H ldaps://$SLAPD_HOST \
  -D "uid=appsuite,ou=Readpw,$LDAP_DOMAIN" -w "$LDAP_ADMIN_PW" \
  -b "ou=Mail,$LDAP_DOMAIN" -LLL -s sub
```
