# TLS certificates for slaptain

slapd needs a server certificate, and `SlapdCluster` takes it as a Kubernetes
Secret with `tls.crt` and `tls.key` (plus an optional `ca.crt`). This document is
about how to get one, and — the part that is usually harder — how clients come to
trust it.

Since ADR-029 the operator checks that Secret before it starts anything. A
cluster whose certificate is missing reports it instead of leaving pods wedged:

```
$ kubectl get slapdcluster
NAME    PHASE           READY   REPLICAS   AGE
slapd   Bootstrapping   0       1          40s

$ kubectl describe slapdcluster slapd | grep -A3 TLSReady
  Type:     TLSReady
  Status:   False
  Reason:   SecretMissing
  Message:  Secret "slapd-tls" does not exist; slapd is not started until it does. …
```

It self-heals: create the Secret and the cluster proceeds within a reconcile
cycle, with no edit to the CR and no operator restart. And it only ever
*withholds* — if the Secret is deleted under a cluster that is already running,
that is reported and nothing else. The pods have their material mounted and keep
serving; a deleted prerequisite is not a reason to take a directory down.

## Choosing where certificates come from

The four options below are ordered by how much trust distribution they leave you
to do, which is the cost that actually bites. The first makes the question
disappear; the last is where people end up when they skip the question.

### 1. A CA your clients already trust — the shortest path

If your organisation has an internal PKI, or the directory has public DNS names
and can use a public CA, use it. There is no new trust to distribute: clients
validate against a root they already carry, and slaptain needs no CA bundle at
all — a public-CA certificate embeds its chain in `tls.crt`, the init container
omits `TLSCACertificateFile`, and slapd falls back to the system trust store
(ADR-007).

Put the issued material in a Secret and name it:

```yaml
spec:
  ldap:
    tls:
      enabled: true
      secretName: my-directory-tls   # keys: tls.crt, tls.key
```

For most institutions this is the answer, and the rest of this page is about
what to do when it is not available.

### 2. cert-manager — the Kubernetes-native answer

[cert-manager](https://cert-manager.io/) issues and, importantly, **renews**.
A self-signed CA issuer is a few lines, and it is the only option here that
solves expiry without a human remembering:

```yaml
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: slaptain-ca
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: slapd-tls
spec:
  secretName: slapd-tls          # exactly what spec.ldap.tls.secretName names
  issuerRef: {name: slaptain-ca, kind: Issuer}
  dnsNames:
    - slapd
    - slapd.<namespace>.svc
    - slapd.<namespace>.svc.cluster.local
    - "*.slapd-headless.<namespace>.svc.cluster.local"   # every pod's own name
```

The per-pod wildcard matters: the operator connects to pods individually over
the headless Service (`<name>-<ordinal>.<name>-headless.<ns>.svc.<domain>`), and
so does syncrepl.

**Distributing the CA is not a manual truststore.** That is what
[trust-manager](https://cert-manager.io/docs/trust/trust-manager/) is for: it
syncs a CA bundle into ConfigMaps across namespaces, and client workloads mount
the ConfigMap. This is also the mechanism ADR-028 §7 assumes for bootstrapping
cross-site trust in a mesh.

### 3. The cluster's own root CA, via the CSR API — free trust inside the cluster

Every namespace carries the cluster CA in the `kube-root-ca.crt` ConfigMap, and
every pod has it mounted. So a certificate signed by that CA is trusted by
in-cluster clients **with no distribution at all** — which is a real convenience
and why slaptain's own test suite uses it (`tests/gencert.sh`).

It works by asking the `kubernetes.io/kubelet-serving` signer for a certificate:
a CSR with subject `O=system:nodes`, `CN=system:node:<name>` and the real
hostnames in `subjectAltName`, approved with `kubectl certificate approve`, after
which the cluster CA issues it. `ca.crt` then comes from `kube-root-ca.crt`.

```bash
./tests/gencert.sh -n <namespace> -t slapd -s slapd -H slapd-headless \
    -i <node-ip-for-nodeport> slapd-tls
```

**Know what you are choosing.** This is an off-label use of a signer meant for
kubelets, and it has four sharp edges:

- **Not every cluster signs it.** `kubelet-serving` requires
  `--cluster-signing-kubelet-serving-*` on kube-controller-manager. kubeadm and
  most self-managed clusters have it; several managed providers do not.
- **Approval is a privilege.** These CSRs are not auto-approved for a non-node
  requester, and the right to approve them is not a right to hand out casually —
  which is also why slaptain's operator never issues certificates with its own
  ServiceAccount.
- **A strict cluster will deny it.** The ecosystem's approver for this signer,
  [kubelet-csr-approver](https://github.com/postfinance/kubelet-csr-approver),
  requires the CSR's CommonName to equal the *requester's* username and permits
  at most one DNS SAN. Our CSRs match neither, and by default that controller
  **denies** what it will not approve — a denied CSR cannot then be approved by
  hand. If it runs in your cluster, this path is closed.
- **No renewal.** One shot, valid for the cluster's signing duration (a year by
  default). Nothing renews it. This is the gap cert-manager exists to fill.

Cross-site meshes work fine here, incidentally: each site's certificate is signed
by *that* cluster's root CA, and the operator mounts each peer's CA separately
for its `tls_cacert` (ADR-007), so per-cluster roots need no unification.

### 4. Disabling verification — what it actually costs

A common reaction to private-CA friction is to stop verifying: encrypt, do not
authenticate. For a directory this is a worse trade than it is elsewhere. LDAP
simple binds put the password on the wire, so a connection that is encrypted but
unauthenticated hands credentials to anyone who can get in the middle — the
attack is credential theft, not traffic reading.

slaptain makes exactly one bounded version of this trade, deliberately:
IP-addressed cross-site peers use `tls_reqcert=allow` because a certificate
cannot carry a usable name for them (ADR-007). That is a narrow exception with a
stated reason, not a posture to generalise.

If you must run without TLS, say so explicitly — `spec.ldap.tls.enabled: false`
— rather than leaving verification off while believing TLS is protecting you.
The former is a choice with a known shape; the latter is a surprise waiting for
an incident review.

## What the Secret must contain

| Key | Required | Notes |
|---|---|---|
| `tls.crt` | yes | Server certificate. Include the chain for a private CA. |
| `tls.key` | yes | Private key. |
| `ca.crt` | no | CA bundle for verifying peers. Omit for a public CA — slapd uses the system trust store (ADR-007). |

The gate requires the first two to be present and non-empty. `ca.crt` is never
required, because demanding it would break the public-CA case.

## SANs the certificate needs

| Name | Why |
|---|---|
| `<cluster>` | The ClusterIP Service, i.e. what applications connect to. |
| `<cluster>.<ns>.svc`, `<cluster>.<ns>.svc.<domain>` | The same, fully qualified. |
| `*.<cluster>-headless.<ns>.svc.<domain>` | Every pod's own name — used by the operator and by syncrepl between pods. |
| Node IPs | Only if you reach slapd through a NodePort. |
| Peer-reachable addresses | Cross-site: whatever the peers connect to (ADR-016 pod IPs, or Multus addresses). |

`<domain>` is the cluster DNS domain, which slaptain discovers rather than
assumes (ADR-015) — usually `cluster.local`.

## Related

- ADR-029 — the certificate gate, and why TLS is not yet on by default
- ADR-007 — the cross-site TLS posture these certificates serve
- ADR-028 §7 — mesh trust bootstrap (cert-manager / trust-manager)
- `docs/MULTI-SITE.md` — cross-site trust in context
