# ADR-022: The syncprov sessionlog belongs on the data database, and is on by default

**Status:** Accepted
**Date:** 2026-09-11

## Context

Because an RFC 4533 refresh falls back to a present phase whenever the provider
cannot place the consumer's cookie, every such reconnect costs a walk of the whole
database — O(DB size), once per reconnecting consumer.

syncprov can skip that walk. With `olcSpSessionlog: <ops>` the overlay keeps the
last `<ops>` writes in memory — a CSN, an `entryUUID` and an operation tag each —
and replays them to a consumer whose cookie still sits inside the window. Cheap to
keep, cheap to serve.

We configure it nowhere, and OpenLDAP does not enable it by default. So every
provider we run takes the present-phase path for every reconnect whose cookie it
cannot place — and per the ADR-008 amendment of 2026-09-11 and ADR-021, that class
of reconnects is larger than it looks.

Two properties of the 2.6.13 implementation decide both the shape of this change
and where we must not apply it.

**The sessionlog bypasses the mincsn check entirely.** In `syncprov_op_search`
the branch chain is sessionlog-source → in-memory sessionlog → minCSN check →
`findcsn`, as mutually exclusive `else if`s (verified in 2.6.13, `syncprov.c`
~3395-3495). A successful replay therefore returns before either "sync cookie is
stale" emitter runs — the ITS#9059 (2020) fix. That is the benefit, and equally
the hazard: a replay that succeeds and hands over nothing useful looks, from the
consumer, exactly like being up to date.

**Its per-SID viability check does not under-approximate.**
`syncprov_play_sessionlog` (~2035-2051) treats an SID absent from the log as new
enough — correct, because an SID that wrote nothing inside the window has nothing
to replay. The mincsn path's equivalent decision lacks that property (ITS#9580,
and see ADR-021).
→ the sessionlog path is the better-behaved of the two, not merely the faster one.

## Options considered

**Leave it unset (OpenLDAP's default).** Rejected. Every unplaceable reconnect
pays a full present phase for nothing. The log costs a few hundred kilobytes of
provider memory; not having it costs an O(DB) scan per reconnect, on a mesh whose
reconnects are correlated — a rolling restart moves every consumer at once.

**Expose the field, default it off.** Rejected. The project is alpha (v0.0.x) and
our policy is best-config-by-default: a knob whose right value is "on, at a sane
size" for every deployment we can name does not earn an opt-in. Whoever wants
OpenLDAP's bare behaviour asks for it with `0`.

**`syncprov-sessionlog-source` (the accesslog-backed, persistent variant).**
Rejected for now. On paper it fixes the limitation below, because it survives a
restart. But it reads the change journal — the one artifact that is contaminated
or purged in exactly the scenarios where a persistent log would earn its keep (a
pod that has just been refreshed, a log that has just been purged). Revisit once
the accesslog's trustworthiness across those transitions is settled, not before.

**A sessionlog on the accesslog database's syncprov too.** Rejected — see R1. Part
of the decision, not an omission.

## Decision

**Every data database that gets a syncprov overlay gets an in-memory sessionlog,
default 5000 operations, sized by `spec.replication.syncprovSessionlog`. No
accesslog database ever gets one.**

- **R1 — Placement: the data DB's syncprov only. Never an accesslog DB's.** The
  load-bearing half of this ADR. Because the branch chain in
  `syncprov_op_search` is mutually exclusive, a sessionlog on a log DB's syncprov
  displaces the minCSN guard branch that protects that log's delta-sync consumers
  today. And a log DB's syncprov runs with `nopres` + `usehint` (ADR-019) against
  a database whose traffic is almost entirely overlay-written Adds plus purge
  Deletes, so a replay from it succeeds and carries nearly nothing — trading a
  loud `REFRESH_REQUIRED` for silent under-replication. The absence of the
  attribute there is a requirement, asserted in the e2e, not an oversight to tidy
  up later.

- **R2 — Tristate, no CRD default.** `spec.replication.syncprovSessionlog` is
  `*int32`: unset means the operator's default of 5000, `0` disables it (no
  attribute, and we remove an existing one), `>0` is that count verbatim. The
  default lives in the operator, not in a kubebuilder marker, so measurement can
  move it without a CRD migration — same reasoning as ADR-019 R10.

- **R3 — Converged on every reconcile.** We read `olcSpSessionlog` back from the
  overlay's own `{N}`-prefixed DN and write only on a difference: set at the
  overlay's creation, Replace on a change, Delete on disable. An overlay created
  by an operator predating this ADR must converge without being recreated —
  recreating a syncprov overlay is not free, and ADR-019's DN-reuse rule says
  don't churn a `cn=config` entry where an attribute write will do.

- **R4 — Default 5000 operations.** One entry is a CSN, an entryUUID and a tag;
  5000 of them stay well under a megabyte resident per provider database. Large
  enough to cover a rolling restart's reconnect window, small enough that nobody
  has to think about it. Unmeasured against a production write rate — if it is the
  wrong order of magnitude, R2 is why changing it is cheap.

- **R5 — Read-only replicas stay untouched.** RO consumer pods carry no syncprov
  overlay at all (`wantsSyncProv` requires `!readOnly`), so there is nothing to
  configure and no code path reaching them. Stated so nobody adds one for
  symmetry.

## Consequences

- A provider answers a reconnect whose cookie falls inside the window from
  memory: no present phase, no O(DB) scan, and — because the replay path returns
  before the mincsn branch — no exposure to ITS#9580's under-approximation for
  those reconnects.

- **It buys nothing after a provider restart.** The log lives in memory and
  starts empty, so a restarted pod answers its consumers' first reconnects from
  the present-phase path exactly as before — and a pod restart is the common
  Kubernetes reconnect cause. Partial hardening for an idle mesh's reconnects,
  then, not a fix for the dataloss-recovery storm. Do not read a green sessionlog
  assertion as coverage of that scenario.

- **The log wipes itself on refresh-phase traffic.** `syncprov_add_slog`
  (~1670-1685) discards the whole sessionlog on any operation carrying no CSN —
  deliberate contamination protection, since a CSN-less write cannot be replayed
  as a delta. Consequence: a pod that has just been fully refreshed cannot serve
  its own consumers from the sessionlog either. The benefit concentrates where the
  mesh is quiet and evaporates where it churns.

- No new Secret, no new RBAC, one optional CRD field. We write the attribute in
  the pass that already owns the overlay (`ensureSyncProvOverlay`), so no extra
  per-pod round trip.

- `slapo-syncprov(5)` still documents `sessionlog` as recording "all write
  operations (except Adds)" — stale since ITS#6503 (2011); 2.6.13 records every
  write carrying a CSN, Adds included. Noted because the man page would otherwise
  argue against sizing the log for an Add-heavy workload.

## Related

- ADR-003 — the operator owns all syncrepl configuration; this is one more
  attribute it owns rather than leaves to the image.
- ADR-008 (amended 2026-09-11) — what CSN comparison can and cannot observe. The
  reconnect classes this ADR cheapens are the ones that amendment explains are
  invisible from the provider side.
- ADR-019 — one accesslog per data database, and the `nopres`/`usehint` syncprov
  on each log. R1 is a direct consequence of that topology.
- ADR-021 — the mincsn under-approximation this path sidesteps.
- ADR-001 — idempotent convergence; R3 is the usual read-compare-write.

## References

- `slapo-syncprov(5)` — `olcSpSessionlog` / `sessionlog` and
  `sessionlog-source`:
  <https://www.openldap.org/software/man.cgi?query=slapo-syncprov>
  and its source at
  <https://git.openldap.org/openldap/openldap/-/raw/OPENLDAP_REL_ENG_2_6_13/doc/man/man5/slapo-syncprov.5>
- syncprov sources, `OPENLDAP_REL_ENG_2_6_13`:
  <https://git.openldap.org/openldap/openldap/-/raw/OPENLDAP_REL_ENG_2_6_13/servers/slapd/overlays/syncprov.c>
  - `syncprov_op_search` ~3395-3495 — the mutually exclusive
    sessionlog-source / sessionlog / minCSN / `findcsn` chain (R1)
  - `syncprov_play_sessionlog` ~2035-2051 — "SID not present == new enough"
  - `syncprov_add_slog` ~1670-1685 — the whole-log wipe on a CSN-less operation
- ITS#9059 — a successful sessionlog replay skips the mincsn/`findcsn` check:
  <https://bugs.openldap.org/show_bug.cgi?id=9059>
- ITS#9580 — the mincsn under-approximation:
  <https://bugs.openldap.org/show_bug.cgi?id=9580>
- ITS#6503 — sessionlog began recording Adds; the man page was not updated:
  <https://bugs.openldap.org/show_bug.cgi?id=6503>
- RFC 4533 — the LDAP Content Synchronization Operation; present and delete
  phases: <https://www.rfc-editor.org/rfc/rfc4533>
