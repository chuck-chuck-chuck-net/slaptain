# Slaptain Test Suite

This directory contains resources for testing the `slaptain` standalone LDAP server chart `charts/slapd`.

## Installation

The files in this directory assume a `dc=as8,dc=lab,dc=test` LDAP_DOMAIN_DC. Therefore make sure the helm chart for the standalone slapd has been installed with

```bash
make -C .. helm-install HELM_VALUES="-f tests/values.yaml"
```

## Verification

To confirm that the LDAP server is running and that you can authenticate to the configuration database, use the following command:

```bash
kubectl exec -n slaptain slapd-0 -c slapd -- ldapsearch -x -H ldap://localhost:1024 -D "cn=admin,cn=config" -w admin -b "cn=config" -LLL -s base
```

## Bootstrapping

To simulate a pypod for an as8 bootstrap procedure:
```bash
kubectl create -n slaptain cm slapd-test --from-file=cm
kubectl apply -n slaptain -f pod.yaml
kubectl exec -n slaptain slapd-test -- bash -c 'apt update && apt -y install ldap-utils'
kubectl exec -n slaptain slapd-test -- bash -c 'pip install pyyaml ldap3'
```

Verify the service with plain `ldapsearch`:
```bash
kubectl exec -n slaptain slapd-test -- ldapsearch -x -H ldap://slapd:389 -D "cn=admin,cn=config" -w admin -b "cn=config" -LLL -s base
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
