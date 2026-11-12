# ADR-044: The operator caches only what it labelled

**Status:** Accepted
**Date:** 2026-10-27

## Context

The operator built its manager without cache options,
so every kind it watched or read through the cached client
got a cluster-wide informer:
Secret, ConfigMap, Service, StatefulSet, Job and Pod.
Every Secret in the cluster sat in the operator's heap,
Helm release Secrets included.

On a shared cluster with 84 namespaces the operator was OOM-killed
at the chart's 128Mi limit before it reconciled a single `SlapdCluster`.
With the limit raised it measured 553Mi 30 s after start
and 417Mi a few minutes into an earlier run,
with one two-replica cluster and one database.
The slaptain objects account for almost none of that.

ADR-027 already turned down a Secret watch for this reason,
naming a label-scoped cache as the way to make one affordable.
`Owns(&corev1.Secret{})` on the cluster controller had set up the naive watch anyway.

## Options considered

| Option | Why not |
|---|---|
| `DisableFor` Secret and Pod, reading both from the API | Pod readiness is read every pass, and ADR-037 chose the Pod list for being served from the cache. Leaves Service, StatefulSet, Job and ConfigMap cluster-wide. |
| `WATCH_NAMESPACE` into `Cache.DefaultNamespaces` | Opt-in, so the default install keeps the problem, and a CR outside the list is ignored without a word. |
| Per-namespace caches, or swapping the cache, as namespaces gain CRs | controller-runtime fixes the cache's namespaces at start. A swap has to re-register every `Owns()` handler on the new informers, re-lists every active namespace and fires an Add for every object. A restart does the same thing in fewer lines. |
| A startup pass labelling existing objects | The fallback read below is needed for user Secrets anyway and covers existing objects too. Server-side apply relabels Services, StatefulSets and ConfigMaps on the first reconcile. |
| `app.kubernetes.io/managed-by` as the filter | It has one value, and slaptain's charts already set it to `Helm`, which Helm checks before it adopts an object. A chart cannot opt an object into the cache through it. |
| A boolean cache label beside the cluster label | Two labels that have to agree: an object carrying one and not the other is out of the cache, or claims no owner, without a word. |
| No value, or a list of clusters, for a shared object | JSON `null` is stored as `""` on create and deletes the label on every patch, which drops the object out of the cache. A label value cannot hold a comma, and a selector cannot match one element of a list. |

## Decision

1. **Every object the operator creates carries two labels**,
   `app.kubernetes.io/managed-by: slaptain` and `ldap.chuck-chuck-chuck.net/cluster: <SlapdCluster name>`,
   through `withOperatorLabels`.
   The cluster label names the owner, so `kubectl get -l ldap.chuck-chuck-chuck.net/cluster=<name>`
   lists what belongs to one cluster.
   A name longer than a label value is cut to 63 characters, without a trailing `-` or `.`.
   That includes the Pod templates of its StatefulSets and Jobs,
   so the Pods are labelled too.
   Selectors stay as they were: a StatefulSet's is immutable,
   and a Service needs neither label to find its Pods.
2. **The cache filters the six core kinds on the cluster label being present**,
   whatever its value, through `Cache.ByObject`.
   The slaptain CR kinds are cached unfiltered:
   a CR the user applied carries no label and must still reconcile.
   `DefaultLabelSelector` is not used, because it would filter the CRs as well.
3. **A cache miss on a filtered kind is read again from the API server**
   (`NewCacheFallbackClient`, `Get` only).
   That is how the operator reads a Secret the user brought,
   and an object an older operator created before the label existed.
4. **The operator writes labels only onto objects it owns on record**,
   meaning a controller reference to a slaptain CR.
   A user's Secret is never written to, even to add a label.
   `<db>-credentials` has no controller reference,
   and `scripts/mesh-share-credentials.sh` creates one under the same name,
   so an existing one stays unlabelled; a newly generated one is labelled at creation.
5. **Anyone may add the cluster label to an object the operator reads**:
   slaptain's scripts, a chart, a user.
   It names the cluster the object belongs to and puts it in the cache.
   `managed-by: slaptain` stays reserved for what the operator manages.
6. **An object several clusters share carries `cluster: multi_cluster`**:
   the peer kubeconfigs, or S3 credentials two clusters back up with.
   A cluster name cannot contain `_`, so the value can never name a cluster,
   and `-l ldap.chuck-chuck-chuck.net/cluster=multi_cluster` lists exactly the shared objects.

## Consequences

- **One roll per cluster on upgrade.**
  The Pod template gains two labels, so every StatefulSet replaces its Pods once.
  ADR-035 allows an operator upgrade to change the template once;
  the golden templates in `testdata/` record the change.
- **During that roll the Pod list misses the old Pods.**
  Write rotation (ADR-037) sees them as not ready until they are replaced.
  `jobPods` misses the Pods of Jobs started by the older operator.
- **`List` has no fallback.** An empty list is not an error.
  Jobs an older operator created are missing from the label-based reaps
  until their TTL removes them.
- **An unlabelled object costs one API read each time it is read**:
  user Secrets on every pass that reads them,
  and an existing `<db>-credentials` until someone labels it.
- **A new create site has to call `withOperatorLabels`.**
  An object created without it disappears from the cache and its `Owns()` watch.
  `operator_labels_test.go` drives the existing create paths against a fake API server;
  a new path is covered only once it is added there.
- **Cluster names are not length-checked.** Truncation keeps the label valid,
  but two names that share their first 63 characters get the same value.
  The names the operator derives already fail well below 63 characters,
  so a length rule on `SlapdCluster` would remove the case.
- **Not measured yet**: the operator's memory with the filtered cache.
  The 553Mi above is the number to compare against.

## Related

- ADR-027: the Secret watch it turned down, and the label-scoped cache it named
- ADR-035: an operator upgrade may change the Pod template once
- ADR-037: Pod readiness stays a cached read
- ADR-018: the Jobs whose reaps list by label
