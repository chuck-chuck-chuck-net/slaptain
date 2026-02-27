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

---

## 2. Deploy slapd

```bash
make helm-install
```

Pass extra values via `HELM_VALUES_SLAPD`:

```bash
make helm-install HELM_VALUES_SLAPD="-f tests/values.slapd.yaml"
```

To do a full build-and-deploy from scratch (build images, generate certs, install):

```bash
make helm-deploy HELM_VALUES_SLAPD="-f tests/values.slapd.yaml"
```

To remove:

```bash
make helm-uninstall
```

---

## 3. Bootstrap and test with slapd-test

The `slapd-test` chart has two optional components, controlled independently:

| Component | Default | What it does |
|---|---|---|
| `bootstrap` | enabled | Runs a Job that applies schema, ACLs, and initial directory data |
| `toolkit` | disabled | Keeps a pod alive for interactive `kubectl exec` sessions |

### Default: bootstrap only

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

### Both together

Bootstrap runs once, toolkit stays available for follow-up inspection:

```bash
make testing-helm-install HELM_VALUES_SLAPD_TESTING="--set toolkit.enabled=true"
```

To remove:

```bash
make testing-helm-uninstall
```

---

## 4. Using the toolkit

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
