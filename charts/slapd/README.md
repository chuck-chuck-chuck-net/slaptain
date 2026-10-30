# slapd

Deploys one site's `SlapdCluster`, plus optional `SlapdDatabase` and
`SlapdSchema` objects declared under `databases:` / `schemas:`. The slaptain
operator (`charts/operator`) must be installed first. For a multi-site mesh use
`charts/slapd-mesh` instead.

```bash
helm upgrade --install slapd oci://ghcr.io/chuck-chuck-chuck-net/charts/slapd \
  -n slaptain-testing --create-namespace -f my-values.yaml
```

`values.yaml` is the reference: every key is commented there. The chart renders
only the fields you set — a commented-out key is the operator's own default,
left to the operator so that a later release's changed default still reaches an
existing cluster.

## Monitoring (ADR-033 Surface 3)

The slapd exporter sidecar: one container next to every slapd pod, read-write
and read-only alike, serving slapd's `cn=Monitor` as `openldap_*` metrics on
`:9330/metrics`. Off unless you turn it on. The full story — what it exports,
how it is scraped, resources measured on a lab — is `docs/MONITORING.md`.

| Value | Default | Renders | Notes |
|---|---|---|---|
| `monitoring.enabled` | unset | `spec.monitoring.enabled` | `true` adds the sidecar. Changing it changes the pod template, so the StatefulSets roll their pods once. Unset and `false` render the same pods. |
| `monitoring.resources` | unset | `spec.monitoring.resources` | The sidecar's own requests/limits, not `spec.resources`. No default; `docs/MONITORING.md` has the measured starting point. |
| `images.exporter.repository` | `""` | `spec.images.exporter.repository` | Empty: `slapd-exporter` beside the operator's image (its trailing path segment swapped, like `slapd` and `slapd-init`). |
| `images.exporter.tag` | `""` | `spec.images.exporter.tag` | Empty: the exporter release compiled into the operator, not the operator's tag. A tag alone keeps the derived repository. |
| `images.exporter.pullPolicy` | `""` | `spec.images.exporter.pullPolicy` | Once any `images.exporter` field is set, the CRD stores `IfNotPresent` unless you set another. |

`monitoring` is passed through verbatim when set and omitted when empty;
`images.exporter` is rendered only when one of its fields is non-empty.

**The exporter is not pinned in lockstep with `slapd` and `init`.** It is its
own image with its own version line (`docs/VERSIONING.md`), and the operator
defaults it to the exporter release it was built against. An operator upgrade
therefore changes the sidecar only when that release moved; leaving
`images.exporter` empty is the normal setting. Pin it to run a different
exporter version — a newer release, a mirror, a local build. The operator passes
the sidecar no command and no arguments, so any `slapd-exporter` image works.

The sidecar is only half of it: nothing scrapes it until this site's operator
chart has `slapdPodMonitor.enabled=true` and a Prometheus Operator is installed
(`docs/MONITORING.md`, "Install").

## Placement (ADR-043)

Where this cluster's pods are allowed to land. Every key is a raw Kubernetes
pod-spec field, rendered verbatim into `spec.placement` and applied by the
operator to **both** pod templates, read-write and read-only.

Unset is the normal setting, and it is not "no constraint": the operator
generates one spread constraint per fleet — `maxSkew: 1` over
`kubernetes.io/hostname`, `whenUnsatisfiable: ScheduleAnyway`, selecting that
fleet of this cluster alone. Writers spread among writers and readers among
readers. The reasoning, the sizing and what to do when it is not enough are in
`docs/PLACEMENT.md`.

| Value | Default | Renders | Notes |
|---|---|---|---|
| `placement.topologySpreadConstraints` | unset | `spec.placement.topologySpreadConstraints` | A list **replaces** the generated default; it is never merged into it. An explicit `[]` is the only way to ask for no constraint at all — the chart tells the two apart, so `[]` renders as `[]` rather than disappearing. Prefer `ScheduleAnyway`: `DoNotSchedule` can strand a pod permanently once its PV is bound. |
| `placement.nodeSelector` | unset | `spec.placement.nodeSelector` | Labels every node must carry. A selector matching no node leaves pods Pending. |
| `placement.nodeAffinity` | unset | `spec.placement.nodeAffinity` | The **node** half of the affinity struct only. The pod half is reserved for the operator (ADR-043 decision 2). Unset renders no `affinity` field at all, so existing pods do not roll for an empty value. |
| `placement.tolerations` | unset | `spec.placement.tolerations` | Removes a node's veto; does not attract a pod there. Pair with `nodeSelector`/`nodeAffinity` for "there, and only there". |
| `placement.priorityClassName` | unset | `spec.placement.priorityClassName` | Names an existing PriorityClass. |

**The release that introduces this rolls both fleets once, and nothing moves.**
The generated constraint is a pod-template change, so the StatefulSets roll —
but every pod comes back on the node its PVC already pins it to. See
`docs/UPGRADING.md`.
