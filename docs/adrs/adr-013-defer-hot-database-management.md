# ADR-013: Defer hot SlapdDatabase add/remove; require persistent storage

**Status:** Accepted
**Date:** 2026-05-14

## Context

Adding or removing a `SlapdDatabase` CR on a Running cluster currently triggers
a StatefulSet rolling restart of every slapd pod. The mechanism:

1. `SlapdCluster.reconcileStatefulSet` lists `SlapdDatabase` CRs in the
   namespace and embeds the result as `DATABASE_DIRS=<comma-separated>` in the
   slapd-init container's env block.
2. The init container `mkdir -p /data/<name>` for each entry, because slapd's
   back-mdb refuses to register an `olcDatabase` whose `olcDbDirectory` does
   not exist.
3. Any change to the SlapdDatabase set re-renders the STS pod template. The
   template hash changes. The STS controller rolls every pod.

Slapd itself does **not** require a restart to add or remove a database — the
operation is fully live over the wire protocol (`ldap-add` against `cn=config`).
The rolling restart is purely an artefact of our orchestration: we need a
directory on the pod filesystem before the live `ldap-add` will succeed, and our
only mechanism for creating it today is the init container, which runs at pod
start.

The behaviour was discovered on 2026-05-14 by the v0.0.14 ephemeral e2e fixture,
where the rolling restart on emptyDir storage caused total data loss. See
`docs/BUG-ANALYSIS-database-dirs-rolling-restart.md` for the full investigation.
On persistent storage the rolling restart still happens but is data-safe — the
PVCs survive the pod replacement and slapd resumes from on-disk state.

## What this ADR decides

Two related decisions:

1. **Persistent storage is required.** The `spec.persistence.enabled` field is
   removed from the v1alpha1 CRD entirely. The operator always provisions PVCs
   via the StatefulSet's volumeClaimTemplates. CRs that still carry the field
   are rejected at admission as an unknown field.
2. **The rolling restart on SlapdDatabase add/remove is accepted as-is.** We
   will not invest in eliminating it. Persistent storage makes it a UX wart
   (an unnecessary restart) rather than a data-correctness bug.

A separate but related question — should pod readiness reflect data convergence
rather than just "slapd is listening on a port" — is deferred for the same
symmetric reason (see below).

## Options considered for eliminating the rolling restart

### C' — bake a helper binary into the slapd image; operator `kubectl exec`s it

Ship a small statically-linked Go binary (`slaptools mkdir /data/<name>`) in
the slapd image. When the SlapdDatabase controller is about to issue
`ldap-add olcDatabase=...`, it `kubectl exec`s the helper to pre-create the
directory.

- **Pro:** Synchronous; no race. STS pod template never churns.
- **Pro:** Small change; one binary, no new container.
- **Con:** Operator needs `pods/exec` RBAC. Not a meaningful security
  regression (operator already has cluster-wide RBAC on Secrets/STS/Services
  and a network channel to slapd's `cn=config`), but it is a new verb.
- **Con:** Slapd image grows by one binary. Distroless-in-spirit preserved
  (still no shell), but the image is no longer purely "slapd and its
  dependencies".

### G — management sidecar with an authenticated mkdir API

Add a distroless/static sidecar to every slapd pod, sharing the `/data`
volume. The sidecar exposes an authenticated HTTPS endpoint (reusing the
existing TLS Secret for mTLS). The SlapdDatabase controller calls
`POST /mkdir { path: /data/<name> }` synchronously before `ldap-add`.

- **Pro:** Synchronous; no race.
- **Pro:** Slapd image unchanged.
- **Pro:** Sidecar becomes a natural extension point for future in-pod
  operations (cleanup, diagnostics, debug dumps).
- **Con:** One extra container per pod, permanently. Resource overhead, even
  if small.
- **Con:** New API surface to design and maintain (auth, protocol, error
  handling, observability).
- **Con:** Coupling between operator and sidecar must stay in sync across
  upgrades.

### F — ConfigMap-mounted database list with a reconciler sidecar

The SlapdCluster controller writes the database list to a ConfigMap (not the
STS pod template). A sidecar in every pod watches the ConfigMap mount and
reconciles directories. The SlapdDatabase controller issues `ldap-add` and
relies on its existing retry loop to bridge the ConfigMap propagation lag.

- **Pro:** Fully declarative; no operator-side RPC; no `pods/exec` RBAC.
- **Pro:** Slapd image unchanged.
- **Con:** Asynchronous. Propagation lag is ~60s (kubelet ConfigMap sync). In
  that window, `ldap-add` fails with `LDAP code 80: olcDbDirectory invalid
  path` — the exact error we already see during bootstrap, recoverable via
  the existing retry loop but produces transient status flap.
- **Con:** Same sidecar resource overhead as G, plus a less crisp failure
  semantics.

### Cost summary

| | C' | G | F |
|---|---|---|---|
| New code | ~100 LoC helper + ~200 LoC operator exec wiring | ~300 LoC sidecar (HTTPS, mTLS, mkdir handler) + ~200 LoC operator client | ~150 LoC sidecar reconciler + ~100 LoC operator ConfigMap writer |
| Image changes | slapd +1 binary | new sidecar image | new sidecar image |
| RBAC | + `pods/exec` | none | none |
| Pod overhead | none | +1 container | +1 container |
| Synchronous? | yes | yes | no (~60s race) |
| Ongoing maintenance | low | medium (auth, protocol drift) | low-medium |

All three options cost real engineering effort. None is a one-line fix.

## Why we decline to ship any of them now

Two arguments, both pointing the same direction:

1. **Frequency.** Adding or removing a SlapdDatabase on a Running cluster is
   not a routine production operation. LDAP data models are designed up front
   and provisioned once. The hot-add/hot-remove pattern is primarily a
   dev/test convenience. We're being asked to invest persistent engineering
   complexity (a new container, a new API, or a new RBAC verb) to preserve a
   UX property that is exercised maybe once per cluster lifetime.

2. **Severity on persistent storage.** With persistence now mandatory
   (decision 1 above; the spec field is gone), the rolling restart is
   data-safe. It is a UX wart — an unexpected restart when the user
   applies a CR — but not a correctness bug. Production users see a
   brief endpoint flap during the rolling update, not data loss.

The architectural fix is not wrong. It is correctly identified, designed, and
costed. We are deferring it because the value/cost ratio does not justify the
investment today. Reconsider if:

- We see real-world reports of users hitting the rolling restart and being
  surprised or hurt by it.
- The operator gains a sidecar for other reasons (debug ops, custom metrics
  collection, log shipping), at which point adding mkdir to its responsibility
  is nearly free.

## Data-convergence readiness probe — same decision, same reasoning

A related question surfaced during the discussion: should pod readiness reflect
"slapd has converged data" rather than "slapd is listening on a port"? Today
the readiness check is effectively "port open", which means a freshly-restarted
empty pod is considered Ready before syncrepl has filled its database. This is
what made the rolling-restart bug catastrophic on ephemeral storage — the STS
controller proceeded to the next pod before the previous one had caught up.

A real data-convergence readiness probe would solve this class of bug — not
just the DATABASE_DIRS trigger but any future STS pod template churn that
causes pods to restart faster than syncrepl can converge.

The standard chicken-and-egg objection (readiness gates pod membership in
Services; convergence requires Service reachability) is addressable: set
`publishNotReadyAddresses: true` on the headless service so peer-to-peer DNS
ignores readiness state, while the client-facing ClusterIP service continues
to gate on readiness. The operator already talks to pods via headless DNS for
all per-pod operations (cn=config is node-local), so this works.

The real obstacles are:

- **Definition of "converged".** What does the probe actually check? Base-scope
  on the suffix? `contextCSN` within some delta of the cluster max? Per-DB
  entry count above a threshold? Each option has corner cases.
- **Implementation surface.** kubelet probe options are HTTPGet, TCPSocket,
  Exec. Slapd doesn't speak HTTP, TCPSocket is what we have, Exec requires a
  binary in the distroless image — the same architectural cost as Option C'.

The symmetric argument applies: with persistent storage required, the class of
bug the convergence probe protects against is "UX wart during rolling updates"
(brief stale-or-empty reads), not "data correctness". Real-world frequency of
STS template churn is low. Defer.

If we ever pick this up, it should be its own ADR — the design surface (probe
definition, edge cases during initial bootstrap, interaction with consumer-only
replication) is non-trivial.

## Consequences

### Required follow-up

- **CRD field removal.** Drop `spec.persistence.enabled` from the v1alpha1
  CRD entirely. Existing CRs that carry the field will be rejected at
  admission as an unknown field (the same kind of v1alpha1 breakage already
  precedented by the `forceRebootstrap` removal under ADR-012). The "in-memory
  dev cluster" use case is well-served by k3s + local-path-provisioner —
  we don't need to support an unsafe path to serve it.

- **e2e suite:** drop the ephemeral fixture (`tests/values.slapd-ephemeral.yaml`,
  `ephemeral-only` label, dual-fixture orchestration in `tests/e2e.sh`).
  Replace `dataloss_recovery_test.go` with a `kubectl delete pod/<name>
  pvc/data-<name>` test against the persistent fixture. This more accurately
  simulates the real failure mode (node disk loss, accidental PVC deletion)
  than ephemeral-everywhere.

- **Unified `tests/e2e.sh`** stays. Its consolidation of single-site and
  multi-site logic is independently valuable and survives the descope.

- **Documentation:** update CLAUDE.md's "Test Fixture" section, BOOTSTRAP.md,
  and `docs/MIGRATION-PLAN.md` if they reference ephemeral testing.

### Behavioural changes for users

- Adding or removing a `SlapdDatabase` CR on a Running cluster triggers a
  rolling restart of all slapd pods. This is documented as expected
  behaviour.
- `spec.persistence.enabled` is gone from the v1alpha1 CRD. CRs still
  carrying it are rejected at admission.

## Related

- ADR-004 — multi-resource CRD architecture (SlapdCluster / SlapdDatabase /
  SlapdSchema).
- ADR-012 — seed is one-shot; cluster wipe is a Kubernetes resource lifecycle
  operation. Case 2 ("multi-pod: lose one pod's data, syncrepl recovers")
  retains its meaning but is now triggered by `kubectl delete pod + pvc` on
  persistent storage, not by pod restart on ephemeral.
- `docs/BUG-ANALYSIS-database-dirs-rolling-restart.md` — the detailed
  investigation that produced this ADR.
