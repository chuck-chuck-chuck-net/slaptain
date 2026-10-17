# ADR-038: Delta-syncrepl is what slaptain builds; plain syncrepl is what it migrates from

**Status:** Accepted
**Date:** 2026-10-17

## Context

Every slaptain pod replicates with delta-syncrepl. The choice was made in
Phase 2, has never been written down, and five ADRs are built on top of it:
ADR-019 (one accesslog DB per data DB), ADR-020 (the journal's ACL), ADR-022
(where the sessionlog may live), ADR-026 (what a reconcile may do to shared
`cn=config` state — its origin case is a stranded `olcAccessLogDB`) and
ADR-034 (startup-only attributes, whose only member is
`olcSpSessionlogSource`). None of them states why the accesslog exists at all.

ADR-011 records the one place the alternative *is* decided: a legacy source may
speak plain syncrepl, so `externalPeers[].syncMode: delta|plain` exists and is
e2e-verified. That decision governs the **boundary**. Nothing governs the
interior.

The question keeps coming back because our failure ledger is accesslog-shaped.
The ITS#9398 refresh storm (`docs/ITS-9398-STORM-SIGNATURE.md`), the purge
stall that prompted ADR-037, ADR-019's withdrawn R8 migration, ADR-022's
sessionlog placement rule and ADR-034's whole apparatus are all downstream of
the log. Deleting the log deletes all of them in one move, which makes "use
plain syncrepl instead" the cheapest-looking idea in the project. It has now
been proposed twice with no document to answer it.

### What upstream actually says

No upstream source states a default. The 2.6 and 2.7 Administrator's Guides
frame delta-syncrepl as an optimisation for a workload *shape* — the worked
example is a consumer transferring 100 MB to process 200 KB of changes — and
present plain syncrepl as the base case. A distribution guide documents both
and says delta "sends less data over the network, but is more complex to set
up". A vendor knowledge base calls delta "the best possible choice for
replication". Other containerised OpenLDAP projects ship delta and give no
rationale.

So the field's answer is convention, not evidence about our workload. The
evidence has to come from the sources and from what we have measured.

### The mechanism, in the 2.7.1 sources we build

**1. Conflict resolution is built on the accesslog and does not exist without
it.** When a delta consumer receives a modification older than the local
entry's `entryCSN`, `syncrepl_op_modify` (`syncrepl.c:2947`, resolution from
`:3021`) searches **its own accesslog** — `op2.o_req_dn = si->si_logbase`
(`:3073`) — for newer modifications to the same DN, then rewrites the incoming
modlist against a truth table (`delete X` / `add Y` → keep both; `add X` /
`add X` → drop; and so on). The result is attribute-granular merging.

Plain syncrepl has no such path. `dn_callback` compares the incoming entry's
`entryCSN` with the local one and, if the incoming is older — or equal —
returns `LDAP_SUCCESS` having applied nothing: *"new entry is older than ours"*
(`syncrepl.c:6119`). That is entry-granular last-writer-wins.

Concretely: two sites modify different attributes of one entry inside the same
replication window. Under delta both survive. Under plain syncrepl the loser's
attribute change is discarded whole and the winner's entry propagates over it,
with nothing logged above sync level and no condition to observe. For a mesh
whose window is a WAN round trip plus `retry`, not a millisecond, that is the
decisive difference.

**2. The ITS#9398 storm is structurally unreachable without a log database —
and that is the only thing that becomes unreachable.** The refusal chain has
three arms (`syncprov.c:3445`, `:3451`, `:3457`): accesslog replay, sessionlog
replay, then the `minCSN` hint. The third is guarded by `si->si_nopres`, which
`slapo-syncprov(5)` says to set TRUE only "for a syncprov instance on top of a
log database". No log DB, no third arm, no storm.

**3. The dormancy class survives the switch.** Immediately below the chain,
`syncprov_findcsn( op, FIND_CSN, &mincsn )` (`:3520`) decides whether the
cookie can be honoured at all, and the code states its own weakness at `:3514`:

> *"Using mincsn only (rather than the whole cookie) will under-approximate the
> set of entries that haven't changed … As a result, dormant serverids in the
> cluster become mincsns and more likely to make `syncprov_findcsn(,FIND_CSN,)`
> fail -> triggering an expensive refresh"*

That is the ITS#9580 shape, in the **plain** path. Dropping delta does not
remove it; it relocates it, and the penalty becomes a present phase over the
whole DIT.

**4. The gate we are bitten by is a fix, not an oversight.** ITS#9823
("syncprov doesn't fallback when deltasync consumer's offline beyond accesslog
depth") is VERIFIED FIXED, milestone 2.5.13, committed to head/RE26/RE25 in
February 2024, with `test043` extended to cover recovery after a purge. The
stated principle is the one we would want: check whether the consumer's cookie
CSNs are still present in the log, and refresh from the main DB if not. Our
storm is that check made unsatisfiable by ITS#9398's unclean-shutdown artifact
(`minCSN` naming a sid `contextCSN` does not), not the check being wrong.

**5. deltaMPR's conflict semantics are open upstream.** ITS#10178 ("deltaMPR
conflict resolution issues", UNCONFIRMED, **milestone 3.0.0**) records three
strategies that coexist and disagree: deltaMPR providers reinterpret in CSN
order; delta consumers at `olcMultiProvider: FALSE` "accept the accesslog entry
as-is, but the entry represents the original, not reinterpreted write"; plain
syncrepl ignores the out-of-order version. **Our read-only fleet is the second
one.** So delta buys us a merge on the RW mesh and leaves a known divergence
shape on the RO fleet, which this ADR records rather than fixes.

### What the knob does today

`SlapdDatabase.spec.replication.deltaSync` already exists, is a tristate
defaulting to true, and `false` is wired end to end: no accesslog overlay
(`slapddatabase_controller.go:848`) and stanzas emitted without
`syncdata=accesslog` (`:2774`). It has exactly one user. `tests/e2e-migration.sh`
builds the **fake-prod** cluster at `deltaSync: false` so that a slaptain
cluster can impersonate a legacy plain-syncrepl provider for ADR-011's
migration lane.

That is the honest statement of our coverage: plain syncrepl is exercised in CI
as *the thing we migrate from*, and nothing anywhere validates it as a mode we
run.

## Options considered

**A. Delta-syncrepl internally (status quo).** Chosen — see Decision.

**B. Plain syncrepl internally; drop the accesslog entirely.** Rejected. It
costs the conflict resolver (1), makes every cookie miss a present phase over
the whole DIT from every peer — and in Kubernetes reconnects are routine, not
exceptional, since replacing a pod invalidates every peer's addresses
(ADR-016) — removes logged deletes, so a consumer that missed a delete can only
learn of it by a present sweep, and discards the forensic record (ADR-020's ACL
bypass was captured live from `reqMod`). It also does not fix (3), which is the
class it is usually proposed to fix.

**C. Plain syncrepl plus `syncprov-sessionlog-source` pointed at an
accesslog.** Rejected on balance. It keeps persistent recovery across restarts
and skips the `minCSN` arm, but it keeps the log database and its purge — so it
buys none of the simplification that motivates the question — and it still has
no resolver, because the resolver reads the *consumer's* `si_logbase`, which
only delta mode sets. The attribute is also ADR-034's startup-only case, live
modification of which we have already found to be unsafe.

**D. Support both internally, per database, via the existing `deltaSync`
field.** Rejected as a supported configuration. It doubles every replication
path in the e2e suite against ADR-028's one-path rule, and a single mesh
running two strategies at once is precisely the arrangement ITS#10178 calls
incompatible. "The field exists" is not the same as "the configuration is
supported", and this ADR says which it is.

## Decision

**R1 — Delta-syncrepl is the mode slaptain builds.** Every data database on
every pod, read-write and read-only, in-cluster and derived cross-site, uses
`syncdata=accesslog`. This is the default and it is the only configuration the
project validates.

**R2 — `deltaSync: false` is a migration-interop escape hatch, not a
deployment mode.** It stays, because ADR-011's lane needs a slaptain cluster
that can impersonate a plain provider, and because a database excluded from
replication must not grow a log. It carries no validation beyond that lane, and
the field documentation and `docs/REPLICATION.md` say so. Setting it on a
production database is unsupported, not forbidden — we do not add a rejection
for it, since the operator has no way to tell a migration fixture from a
production database.

**R3 — Plain syncrepl stays a boundary feature.** ADR-011's
`externalPeers[].syncMode: plain` is unchanged and is not a preference: it
exists because a plain-only source exposes no accesslog to pull deltas from.
The two ADRs split by scope, not by precedence (ADR-035 R1); neither overrides
the other.

**R4 — The accesslog apparatus is downstream of this ADR and falls only with
it.** ADR-019, ADR-020, ADR-022, ADR-026's origin case, ADR-034 and ADR-037
exist because the log exists. A proposal to remove the log is a proposal to
reopen this decision, and must answer R5 rather than argue the apparatus is
complex — it is, and that complexity is a consequence recorded here.

**R5 — What would reopen it, stated as numbers rather than sentiment.** Two,
and we have neither:

1. **The present-phase cost at the real scale** — entry count × average entry
   size × reconnect frequency, measured on the migration target, not on a
   1200-entry fixture. The big-DIT lane ADR-030 is also waiting on produces it.
2. **The conflicting-write rate** — how often two sites modify one entry inside
   one replication window in the real workload. If it is structurally zero
   (single-site writes, a rotating write target per ADR-037), the resolver
   argument in (1) weakens considerably and option B becomes arguable.

Until those exist, the comparison is measured on one side and reasoned on the
other, and this ADR says plainly which is which.

## Consequences

- The ITS#9398 storm stays ours to detect and live with. The `accesslog-mincsn`
  check in `slctl inspect` keeps its job, and the one-write cure stays the
  documented repair.
- ADR-037 remains live and necessary: we keep the log, so we keep the purge,
  so the purge needs somewhere to run that is not the write path.
- The read-only fleet runs ITS#10178's second strategy. We now record that;
  whether to answer it — by moving the RO fleet to plain syncrepl, where it
  loses a resolver it never had anyway and gains present-phase refreshes — is
  an open question, not a decision taken here. It needs the same two numbers.
- `docs/REPLICATION.md` stops presenting the two modes as a menu. Its table
  stays as a comparison and points here for the decision.
- No new e2e coverage. The migration lane remains the only exercise of plain
  stanzas, which R2 makes deliberate rather than an omission.
- The project's position is recorded against a moving target: ITS#10178 is
  milestone 3.0.0, and upstream's answer there may change what delta's merge is
  worth. Revisit when it lands.

## Related

- ADR-003 — the operator owns all syncrepl configuration; this ADR decides what
  that configuration says.
- ADR-011 — the boundary: `syncMode: plain` against a legacy source, and the
  migration lane that is `deltaSync: false`'s only user.
- ADR-016 — pod replacement invalidates every peer's addresses, which is why
  reconnect cost weighs as heavily as steady-state bandwidth here.
- ADR-019 / ADR-020 / ADR-022 / ADR-026 / ADR-034 — the apparatus R4 names as
  downstream.
- ADR-028 — one path, not two; the rule option D breaks.
- ADR-035 — how this ADR and ADR-011 coexist without either outranking the
  other.
- ADR-037 — the purge's placement; it exists because R1 keeps the log.
- `docs/ITS-9398-STORM-SIGNATURE.md` — the storm's mechanism and detector, the
  cost side of R1.
- `docs/REPLICATION.md` — the user-facing statement of what you get.

## References

- OpenLDAP Administrator's Guide 2.6 and 2.7, *Replication* —
  <https://www.openldap.org/doc/admin26/replication.html>,
  <https://www.openldap.org/doc/admin27/replication.html>
- `slapo-syncprov(5)`, 2.7.1 — `syncprov-sessionlog`,
  `syncprov-sessionlog-source`, `syncprov-nopresent`, `syncprov-reloadhint`
- ITS#9823, *syncprov doesn't fallback when deltasync consumer's offline beyond
  accesslog depth* — <https://bugs.openldap.org/show_bug.cgi?id=9823>
- ITS#10178, *deltaMPR conflict resolution issues* —
  <https://bugs.openldap.org/show_bug.cgi?id=10178>
- openldap-technical, *Stupid question about accesslog, delta-syncrepl and
  efficiency* (February 2026) — `reqOld` is logged but not replicated, so the
  bandwidth claim holds for multi-valued attributes —
  <https://www.mail-archive.com/openldap-technical@openldap.org/msg29566.html>
- OpenLDAP 2.7.1 sources: `servers/slapd/syncrepl.c`,
  `servers/slapd/overlays/syncprov.c`, `servers/slapd/overlays/accesslog.c`
