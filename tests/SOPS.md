# Secret Management with SOPS and helm-secrets

## Passwords

The `slapd` chart expects **password hashes** in its `values.yaml` or an existing Secret.
The `slapd-test` chart expects **plain-text passwords** in its `values.yaml` or an existing Secret.

Generate a password and its hash:

```bash
pass=$(LC_ALL=C tr -dc 'A-Za-z0-9' < /dev/urandom | head -c 32)
hash=$(slappasswd -s "$pass" -h {SSHA})
echo $pass $hash
```

## Installation

```bash
apt install sops age
helm plugin install https://github.com/jkroepke/helm-secrets
```

## Key setup

```bash
age-keygen -o age-key.txt
export SOPS_AGE_KEY_FILE=$(pwd)/age-key.txt
```

Put the `export` line in your shell startup file and store `age-key.txt` somewhere safe outside
the repository.

## Encrypting secret value files

```bash
sops --encrypt --age $(age-keygen -y $SOPS_AGE_KEY_FILE) --in-place tests/values.slapd.secret.yaml
sops --encrypt --age $(age-keygen -y $SOPS_AGE_KEY_FILE) --in-place tests/values.slapd-test.secret.yaml
```

Subsequent editing:

```bash
sops tests/values.slapd.secret.yaml
sops tests/values.slapd-test.secret.yaml
```

## Using encrypted files with Helm

Pass encrypted files using the `secrets://` protocol provided by `helm-secrets`:

```bash
make helm-install HELM_VALUES_SLAPD="-f tests/values.slapd.yaml -f secrets://tests/values.slapd.secret.yaml"
make testing-helm-install HELM_VALUES_SLAPD_TESTING="-f secrets://tests/values.slapd-test.secret.yaml"
```
