# slaptain

Helm chart for the slaptain Kubernetes operator — manages `SlapdMesh`,
`SlapdCluster`, `SlapdDatabase`, `SlapdSchema`, `SlapdBackup`,
`SlapdScheduledBackup`, and `SlapdRestore` custom resources that make up a
multi-master OpenLDAP deployment. This chart installs the operator itself (CRDs,
RBAC, and the manager Deployment) — it does not deploy an LDAP cluster. For
that, see the `slapd-cluster` chart (one site) or `slapd-mesh` (a whole
multi-site mesh).

This README documents chart usage only. For concepts (replication model,
credential architecture, backup/restore, ADRs) see the project's top-level
`README.md` and `docs/` — not duplicated here.

## Installing

```bash
helm upgrade --install slaptain oci://ghcr.io/chuck-chuck-chuck-net/charts/slaptain \
  -n slaptain-system --create-namespace
```

This installs the CRDs, RBAC (ClusterRole/ClusterRoleBinding + Role/RoleBinding),
ServiceAccount, and the operator Deployment.

### CRDs are only installed on first install

Helm installs a chart's `crds/` directory **only on the first `helm install`**,
never on `helm upgrade`. If you upgrade the chart to a version that adds or
changes a CRD, the cluster's CRDs will be missing the change unless you apply
them yourself:

```bash
kubectl apply --server-side -f https://raw.githubusercontent.com/chuck-chuck-chuck-net/slaptain/main/charts/operator/crds/<file>.yaml
```

or, from a checked-out copy of the repository:

```bash
kubectl apply --server-side -f charts/operator/crds/
```

(In the project repository itself, `make operator-crd-apply` does this, and
`make operator-helm-install` runs it automatically before the Helm upgrade.)

## Values

| Key | Default | Description |
|---|---|---|
| `siteName` | `""` | **The single per-site value in the whole system**, and the only argument that differs between sites: which site of the `SlapdMesh` this operator runs at. It lives here rather than in any CR because every mesh-scoped resource is applied byte-identically everywhere (ADR-028). Empty is legal and simply disables mesh features — that is what a single-site deployment wants. Two sites sharing a name collide their serverID decades, which slapd does not validate. |
| `clusterDomain` | `""` | Kubernetes cluster DNS domain used to build pod FQDNs (serverID URLs, syncrepl provider URIs, operator→pod connections). Empty auto-discovers from the operator pod's `/etc/resolv.conf`; set explicitly for a non-standard domain or when running the operator off-cluster. |
| `image.repository` | `ghcr.io/chuck-chuck-chuck-net/slaptain/operator` | Operator image repository. |
| `image.tag` | `""` | Image tag. Empty falls back to the chart's `appVersion` (set at package time from the git tag, matching the published image tag). |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy. |
| `imagePullSecrets` | `[]` | Image pull secrets for private registries. |
| `replicas` | `1` | Operator Deployment replica count. |
| `resources` | `limits: {cpu: 500m, memory: 128Mi}`, `requests: {cpu: 10m, memory: 64Mi}` | Operator container resource requests/limits. |
| `serviceAccount.create` | `true` | Create a ServiceAccount for the operator. |
| `serviceAccount.name` | `""` | Override the ServiceAccount name (defaults to the release fullname). |
| `nodeSelector` | `{}` | Node selector for the operator pod. |
| `tolerations` | `[]` | Tolerations for the operator pod. |
| `affinity` | `{}` | Affinity rules for the operator pod. |
| `leaderElection.enabled` | `true` | Enable controller-runtime leader election (needed when `replicas > 1`). |
| `metrics.enabled` | `false` | Enable the operator's metrics endpoint. When enabled, a Service is created and `--metrics-bind-address` is set on the manager. |
| `metrics.port` | `8080` | Metrics endpoint port. |
| `metrics.serviceMonitor.enabled` | `false` | Create a Prometheus Operator `ServiceMonitor`. Requires `metrics.enabled=true`. |
| `metrics.serviceMonitor.interval` | `30s` | Scrape interval. |
| `metrics.serviceMonitor.scrapeTimeout` | `10s` | Scrape timeout. |
| `metrics.serviceMonitor.labels` | `{}` | Extra labels on the `ServiceMonitor` (e.g. to match a Prometheus Operator `serviceMonitorSelector`). |
| `multus.network` | `""` | Attach the operator pod to a Multus replication network (`namespace/name` or `name`). Required for `ExternalPeer` discovery mode. |
| `networkPolicy.enabled` | `false` | Create a `NetworkPolicy` restricting metrics port access to labeled namespaces. Requires `metrics.enabled=true`. |

See `values.yaml` for the authoritative defaults and inline comments.

## Further reading

- Local development and running the operator against a kubeconfig: `docs/DEVELOPMENT.md`
- Supported OpenLDAP versions: `docs/OPENLDAP-VERSIONS.md`
- Project `README.md` and `docs/adrs/` for architecture and design decisions
