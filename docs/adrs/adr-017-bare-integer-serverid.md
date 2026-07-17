# ADR-017: olcServerID is a bare integer, not the URL-list form

**Status:** Accepted
**Date:** 2026-07-17

## Context

OpenLDAP's `olcServerID` accepts two forms (`slapd-config(5)`):

```
olcServerID: <integer>
olcServerID: <integer> <URL>
```

The bare integer *is* the server's identity — the `#serverID#` field stamped
into every CSN it writes. The URL form is a convenience for a **shared config**:
you write the same complete `<id> <URL>` list on every node, and each slapd
self-selects its own ID by matching a listed URL against its own listeners. The
man page is explicit that the multi-value list is optional ("*may* be specified
multiple times"); the only hard requirement is that each provider carry a unique
non-zero ID. There is no peer registry — `contextCSN` accumulates one value per
serverID *observed on the wire*, and unknown serverIDs are never rejected.

Up to now slaptain emitted the URL-list form on every RW pod
(`serverID <id> <url>` × N), replicating the pre-Kubernetes shared-config idiom:
one identical list, each pod self-matching by FQDN.

That idiom does not fit slaptain's model. Per ADR-002 `cn=config` is
**node-local** — never replicated — and the operator writes it per-pod, already
knowing the ordinal it is talking to. So the shared-list convenience buys
nothing here, while the URL self-match imposes a real cost:

- **It coupled identity to FQDN construction.** The self-match resolves the
  listed URL against the pod's `/etc/hosts` FQDN at slapd startup. slaptain
  builds that URL from scheme + pod name + headless service + namespace +
  cluster domain + port. When the cluster DNS domain was wrong, slapd exited at
  boot with `read_config: no serverID / URL match found` — the ADR-015 crash.
  serverID was the *first* thing to hard-fail, ahead of the retry-tolerant
  connection paths that shared the same bug.
- **It inverted the scale story.** With the full list on every pod, scale-up
  forced the operator to rewrite *every existing pod's* list to append the new
  entry. The bare form gives each pod one value that never changes, so existing
  pods are untouched on scale events.

The three benefits previously credited to the URL form — a stable ordinal-keyed
self-ID, scale-up as a cheap list edit, and sid ≥ 1 from birth (no sid-0 epoch)
— either hold identically with a bare integer or, in the scale case, describe
work the URL form *creates*. sid-1-per-default (ADR-011) is about the *value*
(≥ 1), orthogonal to the representation.

## Options considered

1. **Keep the URL-list form.** Rejected: it pays the FQDN self-match crash
   surface and the scale-up list-rewrite for a shared-config convenience that a
   node-local, operator-assigned model does not use.
2. **Bare integer per pod (chosen).** Each pod's `olcServerID` is the single
   integer `serverIDBase + ordinal + 1`. The operator/bootstrap already know the
   ordinal, so identity needs no FQDN, no domain, no peer list.

## Decision

Emit and reconcile `olcServerID` as a **bare integer** equal to
`serverIDBase + ordinal + 1`, one value per RW pod.

- **bootstrap.sh** derives the ordinal from `$HOSTNAME` (`${HOSTNAME##*-}`, the
  last segment of the StatefulSet pod name) and writes `serverID <sid>` — no
  URL. It fails loudly if the hostname yields no numeric ordinal rather than
  stamping a wrong identity. Gated on `LDAP_REPLICAS` (the "operator-managed RW
  pod" signal) and skipped for read-only replicas, unchanged.
- **The operator** (`desiredServerID` / `ensureServerIDs`, SlapdDatabase
  controller) computes the same bare value per pod and reconciles it at runtime,
  per-pod, over the headless service — the same addressing primitive it already
  uses for ACLs, schema, and overlays. The init-container env shrinks to
  `LDAP_REPLICAS` + `LDAP_SERVER_ID_BASE`; the four URL-only vars
  (`LDAP_CLUSTER_NAME`, `LDAP_CLUSTER_HEADLESS_SVC`, `LDAP_NAMESPACE`,
  `LDAP_CLUSTER_DOMAIN`) are dropped from that env.

**Identity is write-once-at-boot.** A pod's serverID is a function of its fixed
ordinal, so no lifecycle operation (scale-up, scale-down, N→M, replication
enable/disable) ever changes a *live* pod's own ID. `ensureServerIDs` is
therefore a no-op in every healthy case; it only Replaces on a pod that booted
under an older regime (sid 0) or still carries the pre-ADR-017 URL list. For any
correctly-booted pod the bare desired value **equals the sid it is already
running** (the URL form matched the same `base+ordinal+1`), so that Replace
rewrites the stored representation, not the running identity — safe regardless
of whether slapd re-derives serverID on a runtime `cn=config` modify or only at
restart. Renumbering a live pod is out of scope and is not something the
operator does.

## Consequences

- serverID no longer depends on cluster DNS domain, FQDN, TLS scheme, or port.
  The ADR-015 class of "wrong domain → slapd won't boot" cannot recur *via
  serverID* (the connection/syncrepl-URI paths still need the resolved domain —
  ADR-015 stands for those).
- Scale events touch only the pods that change; existing pods keep their value.
- **Upgrade path:** a pod carrying the old URL list keeps its live sid; the next
  reconcile rewrites the attribute to the bare integer of equal value. No slapd
  restart required for identity, because the value is unchanged.
- **Migration interop is unaffected (ADR-011).** slaptain never listed foreign
  serverIDs in its own `olcServerID` — `foreignServerIDs` is collision-*validation*
  only and stays out of the attribute. A source that runs the URL-list form adds
  slaptain's IDs to *its* list per its own convention; that is independent of how
  slaptain represents its own IDs.
- Value form is internal: `csn.go` and `slctl inspect` parse the `#serverID#`
  field of CSNs, never the `olcServerID` config attribute, so they are unchanged.

## Related

- ADR-002: cn=config is node-local — the premise that makes the shared-list form
  pointless here.
- ADR-003: Operator owns all syncrepl configuration (serverID scheme is
  operator-managed).
- ADR-011: sid-1-per-default and the `serverIDBase + ordinal + 1` scheme — this
  ADR changes only the *representation* of that value, not the value or the
  migration contract.
- ADR-015: the cluster-domain resolution bug whose serverID crash symptom this
  change removes at the source.
