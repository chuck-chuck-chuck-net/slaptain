# Slaptain Test Suite

This directory contains resources for testing the `slaptain` standalone LDAP server chart `charts/slapd`.

## Passwords

The slapd chart expects password hashes in its values.yaml file, or an existing secret.

The slapd-test chart expects passwords in its values.yaml file, or an existing secret.

Recommended to create this file by any tooling, like, 

```bash
pass=$(LC_ALL=C tr -dc 'A-Za-z0-9' < /dev/urandom | head -c 32)
hash=$(slappasswd -s "$pass" -h {SSHA})
echo $pass $hash
```
## SOPS and helm-secrets

A side topic for this repo. But we live and learn.

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

The files can be encrypted:
```bash
sops --encrypt --age $(age-keygen -y $SOPS_AGE_KEY_FILE) --in-place values.slapd.secret.yaml
sops --encrypt --age $(age-keygen -y $SOPS_AGE_KEY_FILE) --in-place values.slapd-test.secret.yaml
```

Subsequent editing goes e.g. like this:
```bash
sops values.slapd.secret.yaml
sops values.slapd-test.secret.yaml
```

Decrypting to stdout (for viewing):
```bash
sops -d values.slapd.secret.yaml
sops -d values.slapd-test.secret.yaml
```

Adding to `helm install` goes with the `secrets://` protocol, or with `helm secrets install`.

## Certs

```bash
% ./gencert.sh -n slaptain -t slapd -s slapd slapd-tls
```

## Installation

The files in this directory assume a `dc=as8,dc=lab,dc=test` LDAP_DOMAIN_DC. Therefore make sure the helm chart for the standalone slapd has been installed with

```bash
make -C .. helm-install HELM_VALUES="-f tests/values.slapd.yaml"
```

When using SOPS:
```bash
helm upgrade --install slapd ./charts/slapd --namespace slaptain --create-namespace --set global.registry=registry.internal --set global.project=slaptain -f tests/values.slapd.yaml -f secrets://tests/values.slapd.secret.yaml
helm upgrade --install slapd-test ./charts/slapd-test --namespace slaptain --create-namespace -f secrets://tests/values.slapd-test.secret.yaml
```

## Verification

To confirm that the LDAP server is running and that you can authenticate to the configuration database, use the following command:

```bash
kubectl exec -n slaptain slapd-0 -c slapd -- ldapsearch -x -H ldap://localhost:1024 -D "cn=admin,cn=config" -w admin -b "cn=config" -LLL -s base
```

Tests from the slapd-test pod and via TLS below.

## Automated Testing (Helm Chart)

The `charts/slapd-test` chart automates the bootstrap and verification process. It creates a Job that waits for `slapd` to be ready, then applies the schema and ACLs.

### Run the Test

```bash
# Make sure slapd is already installed (see Installation section)
make -C .. test
```

When using SOPS:
```bash
helm upgrade --install slapd-test ./charts/slapd-test --namespace slaptain --create-namespace -f secrets://tests/values.slapd-test.secret.yaml
```

### Check Test Results

```bash
# Watch the logs of the test job
kubectl logs -n slaptain -l app.kubernetes.io/name=slapd-test -f
```

### Cleanup

```bash
make -C .. test-uninstall
```

## Manual Bootstrapping (Simulation)
```bash
kubectl create -n slaptain cm slapd-test --from-file=cm
kubectl apply -n slaptain -f pod.yaml
kubectl exec -n slaptain slapd-test -- bash -c 'apt update && apt -y install ldap-utils'
kubectl exec -n slaptain slapd-test -- bash -c 'pip install pyyaml ldap3'
```

Verify the service with plain `ldapsearch`:
```bash
kubectl exec -n slaptain slapd-test -it -- bash
ldapsearch -x -H ldaps://slapd -D "cn=admin,cn=config" -w "$LDAP_ROOT_PW" -b "cn=config" -LLL -s base
LDAPTLS_CACERT=/etc/ldap/tls/ca.crt ldapsearch -x -H ldaps://slapd -D "cn=admin,cn=config" -w "$LDAP_ROOT_PW" -b "cn=config" -LLL -s base 
```

Bootstrap our custom schemas and ACLs:
```bash
kubectl exec -n slaptain slapd-test -- bash -c 'python /config/ldap-bootstrap.py -d -H ldap://slapd:389/ -D "cn=admin,cn=config" -w admin /config/ox-schema.json /config/slapd-readpw.json'
kubectl exec -n slaptain slapd-test -- bash -c 'python /config/ldap-bootstrap.py -d -H ldap://slapd:389/ --domain dc=as8,dc=lab,dc=test -w admin --auto-readpw /config/ldap-readpw-users.secret.yaml /config/slapd-ous.json'
```

Verify our ACLs work (note, the subtree will be empty at this time, but access should work):
```bash
kubectl exec -n slaptain slapd-test -- ldapsearch -x -H ldap://slapd:389 -D "uid=appsuite,ou=Readpw,dc=as8,dc=lab,dc=test" -w "REDACTED-LAB-PW" -b "ou=Mail,dc=as8,dc=lab,dc=test" -LLL -s sub
```
