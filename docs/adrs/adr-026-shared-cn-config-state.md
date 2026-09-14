# ADR-026: What a SlapdDatabase reconcile may do to state it does not exclusively own

**Status:** Accepted
**Date:** 2026-09-14

## Context

ADR-004 makes `SlapdDatabase` a first-class multi-instance resource, and ADR-002
makes each pod's `cn=config` node-local and operator-converged per pod. Between
them, several `SlapdDatabase` reconciles write into one `cn=config` on one pod.
Most of what they write is exclusively theirs — their data database, their
accesslog database, their overlays, their ACLs. Some of it is not.

On 2026-09-14 the part that is not produced a permanently unbootable pod. The
ADR-019 R8 migration deleted a cluster-shared accesslog database on a
reference count taken from the accesslog overlays of *other* databases, an
overlay naming it was re-created between the count and the delete, and the
resulting dangling `olcAccessLogDB` was invisible to every reconcile and fatal at
the pod's next startup. Full mechanism in the ADR-019 amendment of the same date
and in `docs/reconcile-loop-fixes.md`.

Reconcile serialization did not help and could not have: `MaxConcurrentReconciles`
is unset, so controller-runtime's default of 1 already serialises every
`SlapdDatabase` reconcile against every other. **The competing writer was not a
reconcile.** It was a test harness; in production it is a human at an
`ldapmodify` prompt, which ADR-002 explicitly sanctions. A lock the other writer
does not take is not a lock, which is why the rule below is not about locking.

## Options considered

**Serialise mutation of shared `cn=config` state.** Rejected — it is already
serialised, and the writer that broke it is outside the operator entirely.

**Make the destructive decision atomic with its authorisation.** `cn=config`
offers no compare-and-delete, no transaction, and no way to hold a foreign entry
still. The only constructions are a per-pod ownership marker (new persisted
state, its own lifecycle and failure modes — and the project's own guidance is
that new persisted state is a smell) or not deleting. Rejected as machinery
disproportionate to any live need; see the ADR-019 amendment for the specific
cost/audience trade that decided it there.

**Write the rule down and delete the one path that breaks it.** Chosen.

## Decision

**R1 — `olcDatabase={N}` is a positional, cluster-wide namespace.** slapd
renumbers every database ordered after a deleted one, so any database delete
invalidates every DN ordered above it. A DN must not outlive a mutation: every
step that may delete a database reports that it did, and its caller re-resolves
on the spot. Do not reason about *which* DNs a renumber touches — one extra
search on a path that only fires after a delete is cheaper than that arithmetic
being subtly wrong.

*This clause describes implemented, correct behaviour.* It was learned on
2026-08-25 (a cached data DN slid onto another database's journal and attached an
accesslog overlay to it) and is enforced at both surviving delete sites in
`reconcilePodDatabase`. It is stated here so the next author inherits it as a
rule rather than as a ledger anecdote.

**R2 — A destructive action on shared `cn=config` state may not be authorised by
evidence the operator does not own.** Another database's overlays, anything a
human may hand-edit (ADR-002), anything a test fixture manufactures: none of it
is a safe basis for a delete, however correct the predicate over it is. The
observation and the execution are not atomic and the evidence is not stable
between them. Where the only available authorisation is such evidence, **do not
delete** — report the condition and leave it to a human.

The failure direction is chosen deliberately. Normally a guard that denies a
legitimate own state is a permanent stall while one that admits a bad state needs
a coincidence, which argues for admitting. It does not argue that way here,
because the bad state is *not loud*: slapd validates `olcAccessLogDB` when it
arrives over the wire but resolves it offline at `accesslog_db_open`, so the
damage is written silently and surfaces only as a server that will not start.
A stall you can see beats a destruction you cannot.

**R3 — A convergence step must be triggered by the condition it repairs, never
by a sibling artifact.** "Drop an overlay that names the wrong log" must be gated
on the overlay naming the wrong log — not on the existence of some other object
the same pass may delete. A trigger keyed on an artifact another path can remove
turns a retryable partial state into an absorbing one: the moment the trigger is
gone, the remainder is unreachable and no amount of reconciling fixes it.

**Corollary — R2 and R3 are forward-looking.** With the ADR-019 R8 migration
removed there is **no remaining path** on which a `SlapdDatabase` reconcile
destroys shared state on foreign evidence. They are guards on future code, not a
description of a live hazard. The audit that establishes that:

| Shared state a SlapdDatabase reconcile writes | Mechanism | Safe because |
|---|---|---|
| `olcDatabase={N}` DN namespace | deletes in `removeAccesslogDBAt`, `removeDataDBOverlay`, `deleteDatabaseFromPod` | R1: report-and-re-resolve, implemented |
| `olcServerID` on `cn=config` | full `Replace`, from every database's reconcile | the desired list is a pure function of (cluster, ordinal) — every writer computes the same value, so it is convergent, not contended |
| `cn=module{0},cn=config` | `ensureModulesLoaded` | additive desired-minimum, union semantics, never removes |
| `cn=replication,<suffix>` in the data tree | `ensureReplicationUser` | create-if-missing, never converged — a *known* open item ("the replication credential lives in two stores", `docs/BACKLOG.md`), out of scope here |

`cn=schema,cn=config` and the global `cn=config` tunables are written by the
SlapdSchema and SlapdCluster controllers respectively; each has a single writer.
Nothing touches the frontend database.

## Consequences

- The ADR-019 R8 migration is withdrawn (amendment of the same date). Its
  reference-counted delete is the worked example of an R2 violation, and its
  `DropOverlay` gate the worked example of an R3 violation.
- A future shared object on the replication path is not forbidden — it is
  forbidden to *destroy* one on foreign evidence. Creating, converging and
  reporting are unaffected.
- `slctl inspect` carries the reporting half of R2 for the one condition that is
  currently observable: an `olcAccessLogDB` naming a database the pod does not
  have is an issue, stating that slapd will refuse to start.
- Reviews of any new `cn=config` delete should ask three questions in order: who
  else may hold a DN above it (R1); what authorises the delete and who owns that
  evidence (R2); and what repairs the state this delete can leave behind, and is
  that repair reachable afterwards (R3).

## Related

- ADR-001 — double reconciliation is harmless; the idempotency requirement this
  builds on. Idempotence across *our own* repeated passes is necessary and, as
  this ADR shows, not sufficient: a second writer is not a second pass.
- ADR-002 — `cn=config` is node-local, operator-converged per pod, and
  hand-editable. The last of those is why R2 exists.
- ADR-004 — multi-resource CRDs; why several reconciles share one `cn=config`.
- ADR-010 — peer → consumer-only demotion reaps a database, the surviving live
  path that can renumber (R1).
- ADR-013 — hot `SlapdDatabase` add/remove is deferred, which is why R1's
  exposure is narrower than it looks — but not zero, because demotion is a mode
  flip on a live cluster.
- ADR-019 — per-database accesslog; R8's withdrawal is this rule's origin.
- ADR-024 — where a tunable lives; its amendment's "vet what the *running*
  server does with it" is the sibling discipline for non-destructive writes.

## References

All public. slapd line references are to OpenLDAP 2.7.1, the version this
operator ships (ADR-021).

- `servers/slapd/overlays/accesslog.c` — `accesslog_cf_gen`'s online `logdb`
  validation via `select_backend`, and `accesslog_db_open`'s offline resolution,
  whose failure returns 1 and exits the server
- `servers/slapd/bconfig.c` — `lineno` is faked to 0 for an LDAP add and 1 when
  reading the config directory, which is what selects between the two
- `servers/slapd/slap-config.h` — `#define CONFIG_ONLINE_ADD(ca) (!((ca)->lineno))`
- `slapd-config(5)` — `olcDatabase={N}` ordering semantics:
  <https://www.openldap.org/software/man.cgi?query=slapd-config>
