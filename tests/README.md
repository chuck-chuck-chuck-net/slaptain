# Slaptain Test Suite

This directory contains resources for testing the `slaptain` standalone LDAP server chart `charts/slapd`.

## Passwords

The slapd chart expects password hashes in its values.yaml file, or an existing secret.

The slapd-test chart expects plain-text passwords in its values.yaml file, or an existing secret.

Recommended to generate passwords like this:

```bash
pass=$(LC_ALL=C tr -dc 'A-Za-z0-9' < /dev/urandom | head -c 32)
hash=$(slappasswd -s "$pass" -h {SSHA})
echo $pass $hash
```

## SOPS and helm-secrets

Software installation:
```bash
apt install sops age
helm plugin install https://github.com/jkroepke/helm-secrets
```

Initialize the key:
```bash
age-keygen -o age-key.txt
export SOPS_AGE_KEY_FILE=$(pwd)/age-key.txt
```

Pick a reasonable key location, put the env var into your shell startup.

Encrypt secret value files:
```bash
sops --encrypt --age $(age-keygen -y $SOPS_AGE_KEY_FILE) --in-place values.slapd.secret.yaml
sops --encrypt --age $(age-keygen -y $SOPS_AGE_KEY_FILE) --in-place values.slapd-test.secret.yaml
```

Subsequent editing:
```bash
sops values.slapd.secret.yaml
sops values.slapd-test.secret.yaml
```

Adding to `helm install` goes with the `secrets://` protocol, or with `helm secrets install`.

## Certs

```bash
./gencert.sh -n slaptain -t slapd -s slapd slapd-tls
```

## Installation

The files in this directory assume a `dc=as8,dc=lab,dc=test` LDAP_DOMAIN_DC. Make sure the
slapd chart has been installed with the matching domain:

```bash
make -C .. helm-install HELM_VALUES="-f tests/values.slapd.yaml"
```

When using SOPS:
```bash
helm upgrade --install slapd ./charts/slapd --namespace slaptain --create-namespace \
  --set global.registry=registry.internal --set global.project=slaptain \
  -f tests/values.slapd.yaml -f secrets://tests/values.slapd.secret.yaml
```

---

## Automated Bootstrap (slapd-test chart)

The `charts/slapd-test` chart creates a Job that waits for slapd, then applies schema, ACLs,
and initial directory data. This is the default mode (`bootstrap.enabled: true`).

### Run

```bash
make -C .. test
# or with SOPS:
helm upgrade --install slapd-test ./charts/slapd-test --namespace slaptain --create-namespace \
  -f secrets://tests/values.slapd-test.secret.yaml
```

### Watch logs

```bash
kubectl logs -n slaptain -l app.kubernetes.io/name=slapd-test -f
```

### Cleanup

```bash
make -C .. test-uninstall
```

---

## Toolkit — Interactive Testing Pod

The `toolkit` is a persistent Deployment with `ldap-utils` and `python3` (with `ldap3`,
`pyyaml`) pre-installed. It stays alive so you can `kubectl exec` into it at any time.

Useful for:
- Manual bootstrap or re-bootstrap
- Running ad-hoc `ldapsearch` / `ldapadd` / `ldapmodify` commands
- Inspecting config or data without needing tools installed locally
- Debugging connectivity or TLS issues

### Start the toolkit (with bootstrap disabled for a clean slate)

```bash
helm upgrade --install slapd-test ./charts/slapd-test --namespace slaptain --create-namespace \
  --set bootstrap.enabled=false \
  --set toolkit.enabled=true
```

Or with SOPS secrets:
```bash
helm upgrade --install slapd-test ./charts/slapd-test --namespace slaptain --create-namespace \
  --set bootstrap.enabled=false \
  --set toolkit.enabled=true \
  -f secrets://tests/values.slapd-test.secret.yaml
```

Wait for the toolkit pod to be ready (it installs packages on first start):
```bash
kubectl rollout status deployment/slapd-test-toolkit -n slaptain
```

### Exec in

```bash
kubectl exec -it -n slaptain deploy/slapd-test-toolkit -- bash
```

Inside the pod, environment variables are pre-set:
- `$LDAP_ADMIN_PW` — admin password
- `$LDAP_ROOT_PW` — rootDN (cn=config) password
- `$LDAPTLS_CACERT` — path to the cluster CA cert (`/etc/ldap/tls/ca.crt`)
- `$SLAPD_HOST` — slapd service name (e.g. `slapd`)
- `$LDAP_DOMAIN` — configured domain (e.g. `dc=as8,dc=lab,dc=test`)
- Config files and bootstrap scripts are at `/config/`

### Useful commands inside the toolkit

```bash
# Verify connectivity (anonymous, LDAPS)
ldapsearch -x -H ldaps://$SLAPD_HOST -LLL -s base

# Verify config DB access
ldapsearch -x -H ldaps://$SLAPD_HOST -D "cn=admin,cn=config" -w "$LDAP_ROOT_PW" \
  -b "cn=config" -LLL -s base

# Bootstrap schema and ACLs
python /config/ldap-bootstrap.py -d -H ldaps://$SLAPD_HOST/ \
  -D "cn=admin,cn=config" -w "$LDAP_ROOT_PW" \
  /config/ox-schema.json /config/slapd-readpw.json

# Bootstrap OUs and readpw users
python /config/ldap-bootstrap.py -d -H ldaps://$SLAPD_HOST/ \
  --domain "$LDAP_DOMAIN" -w "$LDAP_ADMIN_PW" \
  --auto-readpw /config/ldap-readpw-users.secret.yaml /config/slapd-ous.json

# Verify ACLs work (subtree may be empty but access should succeed)
ldapsearch -x -H ldaps://$SLAPD_HOST \
  -D "uid=appsuite,ou=Readpw,$LDAP_DOMAIN" -w "$LDAP_ADMIN_PW" \
  -b "ou=Mail,$LDAP_DOMAIN" -LLL -s sub
```

### Run both: bootstrap Job + toolkit

Both can be active simultaneously if you want the Job to bootstrap and the toolkit to stay
available for subsequent inspection:

```bash
helm upgrade --install slapd-test ./charts/slapd-test --namespace slaptain --create-namespace \
  --set bootstrap.enabled=true \
  --set toolkit.enabled=true \
  -f secrets://tests/values.slapd-test.secret.yaml
```

### Cleanup

```bash
make -C .. test-uninstall
```
