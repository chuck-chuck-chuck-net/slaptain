# ADR-042: The operator's cache holds what the operator manages, and nothing else

**Status:** Accepted
**Date:** 2026-10-28

## Context

Issue #28: on a shared cluster with 84 namespaces the operator was OOM-killed
at the chart's default 128Mi limit while managing one two-replica
`SlapdCluster`. With the limit raised it used 553Mi. Memory grew with the size
of the Kubernetes cluster, not with the number of slapd deployments.

The mechanism is controller-runtime's cache. A manager starts one informer per
kind that any controller `Owns()` or reads through the cached client, and an
unfiltered informer lists and watches **every** object of that kind in the
cluster and keeps a decoded copy of each. `operator/cmd/main.go` built the
manager with no `Cache` options, so:

| Kind | Why an informer existed |
|---|---|
| `Secret` | `Owns()` in the SlapdCluster controller, plus cached `Get`s of credentials, TLS, S3 and kubeconfig Secrets in five controllers |
| `Service`, `StatefulSet` | `Owns()` + cached `Get`s in the SlapdCluster controller |
| `Job` | `Owns()` in the SlapdBackup controller, cached `Get`s in the dir and restore machinery |
| `Pod` | `List` for readiness (`writerotation.go`), Multus IPs and Job pods (`job_reap.go`) |

Secrets are the expensive kind: a Helm release record is a Secret of up to a
megabyte, and a shared cluster is full of them. The comment above
`databaseResyncInterval` in `database_resync.go` declined a Secret watch for
exactly this reason, while `Owns(&corev1.Secret{})` had set one up all along.

Reproduced 2026-10-28 on a kind cluster running only the operator, with
96 Secrets of 768 KiB (72 MiB, `helm.sh/release.v1`-typed) created in a
namespace the operator has no business in:

| | heap in use (`go_memstats_heap_inuse_bytes`) | RSS |
|---|---|---|
| before this ADR | 7.4 MiB → 105.0 MiB (**+97.5 MiB**) | 51 MiB → 153 MiB |
| after | 5.7 MiB → 7.3 MiB (+1.6 MiB) | 46 MiB → 53 MiB |

The red arm holds more than the raw load: an informer stores decoded objects,
and the decode of a byte-valued `data` map is larger than the wire form.

## Options considered

1. **Scope the manager to namespaces** (`cache.Options.DefaultNamespaces`).
   Rejected: a `SlapdCluster` may live in any namespace and the operator is
   installed once per cluster; a namespace list is a second configuration to
   keep in step with every tenant, and it still caches every Secret inside
   those namespaces.
2. **Drop `Owns(&corev1.Secret{})` and read every Secret uncached.** Removes
   the Secret informer entirely, no label needed. Rejected on one case: a
   deleted `<cluster>-config-password` would be recreated only on the next
   reconcile, and a Running standalone cluster has no periodic requeue
   (reconcile step 7), so it would wait for an unrelated event. The watch is
   worth keeping; it only has to be narrow.
3. **A dedicated label** (`app.kubernetes.io/managed-by: slaptain`) on every
   object, filter on that. Rejected: the slapd pods do not carry it, and
   adding a label to a pod template rolls every fleet at the operator
   upgrade. ADR-035 permits one upgrade roll, but a roll bought nothing here
   that an existing label did not already buy.
4. **Filter every core-kind informer on a label the objects already carry,
   and read Secrets uncached.** Chosen; below.

## Decision

1. **Every core-kind informer is filtered** to `app.kubernetes.io/name=slapd`
   through `cache.Options.ByObject` (`controller.CacheOptions()`), for exactly
   the kinds that get an informer: Secret, Service, Pod, StatefulSet, Job. No
   `DefaultLabelSelector`: the CRDs carry no label of ours and are what the
   operator exists to watch. The filter is server-side — the LIST and WATCH
   requests carry the selector — so the operator never receives a foreign
   object, let alone stores one.
2. **The label is the one the slapd pods have carried since Phase 1**, so no
   pod template changes and nothing rolls (ADR-013). Every object the operator
   creates now carries it: Services, StatefulSets, ConfigMaps, the
   `<cluster>-config-password` and `<database>-credentials` Secrets, the Jobs
   (which already did) and — new — the Jobs' **pod templates**, because
   `job_reap.go` reads a Job's pods back. One helper, `managedLabels()`, and a
   unit test that builds every object and asserts the selector matches it.
3. **Secrets are read uncached, always** — through `mgr.GetAPIReader()`, by
   every controller, at every call site (`secretReader`). One rule, no
   per-site classification of "ours" versus "the user's": a Secret the user
   supplies (TLS, `cnConfigCredentials`, bring-your-own credentials, S3
   credentials, kubeconfigs, external peer passwords) carries no label of
   ours and is absent from the filtered cache, and the operator never writes
   a label onto an object it does not own. The cost is one GET per Secret
   read per pass, a handful per cluster; the side benefit is that a rotated
   Secret is read fresh rather than at informer lag. The Secret informer
   exists only for the `Owns()` watch on `<cluster>-config-password`.
4. **Objects created by an older operator carry no label**, and the first
   reconcile after an upgrade has to cope with that. The SSA applies land
   the label on Services, StatefulSets and ConfigMaps in that pass.
   The reads that happen *before* the apply go through `managedGet`: cache
   first, and on NotFound one uncached GET before believing it. Without that,
   the immutable-field adoption would take an existing StatefulSet for a
   first create and re-assert `volumeClaimTemplates` — rejected by the API
   server, so the apply that would have labelled it never lands — and a
   `<cluster>-write` Service frozen on one pod would never be widened.
   `reconcileSecret` patches the label onto the config-password Secret it
   owns (controller reference), once. A `<database>-credentials` Secret is
   not labelled retroactively: it is not watched, it is read uncached, and
   the mesh scripts pre-create it under the operator's name, so the operator
   cannot tell its own from the user's.

Verified on kind 2026-10-28: a cluster created by the previous operator
(StatefulSet, two Services, the config Secret, two ConfigMaps, all unlabelled)
was adopted by the new one in a single pass — every object labelled, the
StatefulSet's UID unchanged, the Secret patch logged, no error in the
operator's log.

## Consequences

- Memory grows with the number of slapd deployments, which is the intended
  axis. A foreign object somebody labelled `app.kubernetes.io/name=slapd` is
  cached; it costs its own size and nothing else.
- **A new object builder must use `managedLabels()`.** An object the operator
  creates and cannot select is one it would create twice, never adopt, or —
  for the Secret it watches — never react to. The unit test
  `TestManagedObjectsCarryTheCacheLabel` is the gate; the e2e
  `cache_scope_test.go` asserts the same against a live cluster.
- **A new Secret read must go through `secrets()`**, never `r.Get`. A cached
  Secret read compiles, passes the unit suite's uncached clients, and finds
  nothing in production. `TestSecretReadsGoToTheAPIServerNotTheCache` holds
  every current call site to it with a cache that holds no Secret; the e2e's
  TLS gate holds the fixture's unlabelled TLS Secret to it live.
- The `database_resync.go` objection to a Secret watch is narrowed, not
  dissolved: a label-scoped watch is now cheap, but it would see only the
  Secrets the operator labelled, and the rotation case that matters is the
  user's bring-your-own Secret, which it would not see. The five-minute floor
  stays.
- `tests/e2e.sh` installs the operator with `metrics.enabled=true`, because
  the e2e's measurement is the operator's own `go_memstats` through the API
  server's pod proxy. Plain HTTP on a lab cluster, as the chart's values warn.
- The chart's default limit stays at 128Mi. The issue's 553Mi was the
  cluster's size, not the operator's need.
- Not done: `cache.Options.DefaultTransform` to strip `managedFields` from
  cached objects would shrink the remaining footprint further. Not measured,
  not worth the risk of a transform that touches what SSA reads back.

## Related

- ADR-002 (cn=config convergence is event-driven; the periodic trigger lives
  in `database_resync.go`, whose Secret-watch comment this narrows)
- ADR-013 (no side-effect rolls: the label choice exists so that no pod
  template changes)
- ADR-029 (the TLS gate reads the user's Secret — uncached now)
- ADR-033 (Surface 2 is the endpoint the e2e measures through)
- ADR-035 (an operator upgrade may change the pod template once; this one
  chose not to)
