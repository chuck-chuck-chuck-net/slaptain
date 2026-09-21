# ADR-024: Where an OpenLDAP tunable lives

**Status:** Accepted
**Date:** 2026-09-12

## Context

A config review compared slaptain's generated `cn=config` against a large
production OpenLDAP platform's and produced 18 findings — five that turn a
working small cluster into a broken large one, seven that degrade it, five
hygiene gaps. Individually each is a one-attribute change. Collectively they are
the same question asked 18 times: *where does this attribute live?* Set by the
init container, or written by the operator? A CRD field, or an operator-owned
detail? Converged every reconcile, or written once at creation?

We have answered that question inconsistently, and the inconsistency is visible
in the API today:

- The operator read-compare-writes `spec.acls`, `spec.indices` and
  `spec.replication.syncprovSessionlog` on every pod, every reconcile (ADR-002,
  ADR-022 R3).
- It writes `spec.maxSize` and `spec.noSync` only when it creates the
  database. Editing either on a live `SlapdDatabase` is a **silent no-op**: the
  spec says one thing, `cn=config` keeps another, nothing reports the
  divergence.
- `olcDbDirectory` and `olcSuffix` are create-only because slapd will not
  change them — a real constraint, documented as ADR-019 R8's teardown path.

Only the third case has a reason. The second is an implementation that stopped
early and hardened into API semantics. Without a rule, the next 18 attributes
land wherever their author's afternoon suggested, and every one of them gets
argued from scratch.

This ADR fixes the doctrine. It deliberately does **not** design individual
fields — that is implementation, and the individual findings land separately.

## Options considered

**Decide per attribute, as we have been.** Rejected. That is the process that
produced two create-only fields nobody decided to make create-only. It also
scales badly in review: each PR re-litigates the same three questions.

**Make everything create-only; changing a tunable means recreating the
database.** Rejected. A data database holds data; recreating it to widen a map
size is a restore, not a configuration change. ADR-013 already accepts a rolling
restart as the price of adding a database — paying a reload for an attribute
slapd can take at runtime is a different order of cost entirely.

**Make everything converged, including layout-affecting attributes.** Rejected.
Some attributes bound on-disk structure, and slapd reads them when it
initialises the backend or writes an entry; the back-mdb IDL exponent is the
worked example. Writing it live either has no effect or has an effect only on
data written afterwards — a half-applied index layout is worse than a documented
reload.

**A raw `cn=config` passthrough (`spec.rawConfig`) and no typed surface at
all.** Rejected. `cn=config` is node-local and operator-owned (ADR-002,
ADR-003); a raw escape hatch collides with the attributes the operator writes
itself, has no convergence semantics of its own, and turns every support
question into "what did you paste in". Individual escape hatches, typed and
documented, are the cheaper version of the same flexibility.

## Decision

**Every OpenLDAP attribute slaptain sets belongs to exactly one of three
placement classes, and the class is chosen by a stated criterion — not by
convenience.**

- **R1 — Converged-per-pod is the default class.** The operator observes,
  compares and writes a runtime-mutable `cn=config` attribute on every
  reconcile, on every pod, exactly like ACLs, indices and the syncprov
  sessionlog. This is where an attribute goes unless R2 or R3 applies. The cost is one read
  per pod per reconcile in a pass the controller already makes; the benefit is
  that drift, hand edits and operator upgrades all converge without anyone
  noticing (ADR-001's idempotency requirement, ADR-002's per-pod model).

- **R2 — Bootstrap-time is for attributes slapd cannot take at runtime, or that
  must precede the data load.** Set by the init container from the cluster spec
  into the generated base config. Membership criterion: slapd rejects the
  runtime write, *or* the attribute governs on-disk layout so a late write
  applies to only part of the database. Every such field documents its change
  path — which is a reload, not an edit — in the field comment and in the user
  docs. Backend-level configuration (`olcBackend={0}mdb`) is bootstrap-time by
  construction, since the backend is initialised before any database.

- **R3 — Create-only-by-nature is a deliberately tiny class.** An attribute is
  create-only **only** when slapd forbids changing it on an existing entry —
  `olcSuffix` and `olcDbDirectory` are the class in full today. "The operator
  does not implement the update path" is never a membership criterion. Anything
  in this class states the slapd-side reason and the teardown path (ADR-019 R8
  is the pattern), and inherits ADR-019's rule that a deleted
  `olcDatabase={N}` DN is never reused.

- **R4 — A spec field that cannot be honoured is rejected or converged, never
  ignored.** This is the rule the other three lean on. Today's `maxSize` and
  `noSync` violate it: the API accepts an edit, the operator drops it, and the
  user has no signal.
  From here, a field either converges (R1), or is validated as immutable so the
  API refuses the edit (R3), or documents itself as bootstrap-time and reports
  the pending-reload state (R2). Silent divergence between spec and
  `cn=config` is a defect in every one of the three classes. `maxSize` and
  `noSync` are debt against this rule, not precedent for it.

- **R5 — Unset means slaptain's default, not slapd's.** Where upstream's default
  is sized for a demo and we have an opinion, we set our opinion. The project is
  alpha and its stance is best-config-by-default (ADR-022): a knob whose right
  value is the same for every deployment we can name does not earn an opt-in.
  Corollaries: defaults live in the operator, not in kubebuilder markers, so
  measurement can move them without a CRD migration (ADR-019 R10, ADR-022 R2);
  a tristate (`*T`) distinguishes unset-means-our-default from an explicit
  disable; and the way back to bare OpenLDAP behaviour is asking for it (`0`,
  `""`), never silence.

- **R6 — Per-database attributes go on `SlapdDatabase`; server and frontend
  attributes go on `SlapdCluster`.** A `spec.tuning` block on `SlapdCluster` is
  the accepted home for the server-global family rather than growing the
  top-level spec one attribute at a time. Where an attribute exists at both
  levels, the cluster value is the default and the database value overrides it —
  declared per field, not assumed.

- **R7 — Attributes that implement an operator-owned contract are not CRD
  fields.** The replication identity's DN, its ACL and its search limits are
  parts of one contract the operator owns end to end (ADR-003); exposing the
  limits as user configuration invites a value that breaks replication. Such
  attributes are operator-set and converged per pod, written by the same pass
  that writes the rest of the contract. The test is ownership, not importance:
  if the user cannot change the DN, they have no business changing its limits.

**Intended homes by finding category** — the placement, not the field design:

| Category | Class | Home |
|---|---|---|
| Replication identity limits | converged, operator-set | none (R7) |
| Database map size, checkpoint, sync mode, env flags, read-txn size | converged | `SlapdDatabase` |
| Search size/time limits; per-identity limit exemptions | converged | `SlapdDatabase` |
| Baseline indices required by syncrepl | converged, operator-set | none (R7) |
| back-mdb IDL exponent | bootstrap-time | `SlapdCluster` (backend config) |
| Connection timeouts, thread and buffer counts | converged | `SlapdCluster.spec.tuning` |
| Syncrepl stanza timeouts and keepalive | converged, operator-defaulted | `SlapdDatabase.spec.replication` |
| TLS protocol floor, cipher and CRL policy | converged | `SlapdCluster.spec.ldap.tls` |
| Password hashing policy | converged | `SlapdCluster` |
| Monitor backend and its identity | converged, operator-set | `SlapdCluster` (toggle) |
| Restore-time tool settings | neither — Job-builder detail | `SlapdRestore` escape hatch |
| Log level | converged | `SlapdCluster` (default changes under R5) |

## Consequences

- **Five breaks-at-scale findings are unblocked and land separately**, each
  citing its class rather than arguing placement: the replication identity's
  limits exemption (R7 — a 500-entry silent replication cap today), the database
  map size on both data and accesslog databases (R1 plus R4 — the map size is
  also the worked example of a create-only field that had no business being
  one), server and per-identity search limits (R1/R6), the syncrepl baseline
  indices `entryCSN`/`entryUUID` (R7 — the accesslog database already has
  them, the data database never got the same treatment), and the IDL exponent
  (R2). The remaining findings are recorded in `docs/BACKLOG.md`.

- **R4 makes two shipped fields debt.** `maxSize` and `noSync` must converge or
  be made immutable; doing nothing is no longer an option the doctrine allows.
  `maxSize` carries a second defect of the same family — its doc comment
  promises Kubernetes quantity format while the value reaches `olcDbMaxSize`
  verbatim, which takes a bare byte count. Parse it or fix the comment; a
  documented form slapd rejects is the same silence R4 forbids.

- **Convergence is only as good as its coverage, and our coverage is
  fixture-sized.** Every breaks-at-scale finding is structurally invisible to a
  single-digit-entry LDIF, which is exactly how a 500-entry replication cap
  survived a green suite. The many-entries fixture class — a generated seed of
  well over a thousand entries, and a byte-volume variant exceeding the
  back-mdb default map size — is the standing verification vehicle for this
  ADR's converged class, and it lands with the first of the five.

- **The bootstrap-time class needs a documented recreate path before its first
  member ships.** An attribute nobody can change without a runbook is only
  acceptable while the runbook exists.

- **New tunables get cheaper to review.** The question becomes "which class, and
  why not the default one" — answerable in a sentence — instead of a fresh
  design discussion per attribute.

## Related

- ADR-001 — idempotent reconciliation; R1's read-compare-write is the standard shape.
- ADR-002 — `cn=config` is node-local and operator-managed per pod; R1 is that model applied to tunables.
- ADR-003 — the operator owns the replication contract end to end; R7 is its boundary rule.
- ADR-004 — which CR owns which kind of configuration; R6 extends it to tunables.
- ADR-013 — the cost model for restarts and rolling changes that R2 trades against.
- ADR-019 — per-database accesslog; R3's create-only class and the DN-reuse rule, R10's operator-side defaults.
- ADR-022 — best-config-by-default and the tristate/converge-always pattern R5 and R1 generalise.

## References

- `slapd-config(5)` — the `olc*` attribute surface and which attributes are
  fixed once set: <https://www.openldap.org/software/man.cgi?query=slapd-config>
- `slapd-mdb(5)` — back-mdb database and backend attributes, including the map
  size and the IDL exponent:
  <https://www.openldap.org/software/man.cgi?query=slapd-mdb>
- `slapd.access(5)` and `slapd.conf(5)` — the limits and search-limit
  vocabulary: <https://www.openldap.org/software/man.cgi?query=slapd.conf>

## Amendment (2026-09-12, same day): the map size is bootstrap-time after all

This ADR used `olcDbMaxSize` as the worked example of a field with no business
being create-only. Live evidence refuted that within hours: an `ldapmodify` of
`olcDbMaxSize` on a running back-mdb database is not rejected — **slapd dies on
it** (exit 139, reproduced on two pods mid-MOD; LMDB's `mdb_env_set_mapsize`
must not run with transactions active). "slapd forbids the change" turns out to
include "slapd crashes on the change", which R2/R3 must read as a forbidding.

So: the map size is written at database creation (operator defaults per R5),
and a later spec divergence is REPORTED — `TunablesConverged=False`,
reason `RecreateRequired`, with current value, desired value and the change
path — never written. R4 stands: the field is not ignored, it is answered.
The same condition is the home for any future tunable that turns out to be
recreate-only in practice.

## Amendment (2026-09-13): "slapd forbids the change" includes "slapd hangs on it"

The 2026-09-12 amendment widened R2/R3's membership criterion from "slapd
rejects the runtime write" to include "slapd crashes on it", on the evidence of
the `olcDbMaxSize` segfault. Implementing the degrades-and-hygiene half of the
findings widened it again, and this time the failure mode is worse than a crash.

Three attributes the config review proposed as converged (R1) failed the
live-modifiability check that every attribute in this batch was put through
before it was classified. All three were verified on a running OpenLDAP 2.7.1
pod, and the check is now a precondition of the class, not a nicety:

- **`olcDbEnvFlags`** — adding `writemap` to it on a running back-mdb database
  **segfaults slapd** (exit 139, captured mid-MOD). `MDB_WRITEMAP` is an
  `mdb_env_open` flag and LMDB cannot add it to an open environment.
  `nometasync`/`nosync` alone modify cleanly, but slapd's granularity is the
  attribute, so the whole attribute is create-only-and-reported, exactly like the
  map size. → **R2/R3**, `TunablesConverged=False` / `RecreateRequired`.

- **`olcIdleTimeout` and `olcWriteTimeout`** — modifying either on a running
  slapd **hangs the process**. Not an error, not a crash: the modify's CSN is
  queued, never graduates, and from that moment the pod answers nothing, not even
  an anonymous rootDSE. `SIGTERM` sticks in `slapd shutdown: waiting for 2
  operations/tasks to finish`. Reproduced twice on a healthy three-pod cluster;
  recovery was restarting every pod. Both values are fine when they come from
  the boot config. → **R2**, bootstrap-time, and unimplemented until the init
  container carries them; no CRD field in the meantime, because R4 forbids
  offering one that cannot be honoured.

- **`cn=monitor`** — every individual step works live (module load, database
  add, ACL, `cn=Monitor` answering the replication identity, anonymous denied),
  but modifying an existing monitor database's `olcAccess` hangs slapd with the
  same signature. Withdrawn to `docs/BACKLOG.md`.

Three consequences for the doctrine:

1. **R1 membership is empirical, and the test is per attribute.** The criterion
   is no longer "slapd-config(5) does not say it is fixed" but "we modified it on
   a running pod and the pod was still serving afterwards". Of nine attributes
   put through it in this batch, three failed — a third. Reasoning from the
   manual would have shipped all three.

2. **The hang is the dangerous class, not the crash.** A crashing pod restarts
   and rejoins; Kubernetes handles it. A hung pod keeps its listener open, passes
   a TCP readiness probe, reports `1/1 Running`, and answers nothing — and
   because the operator converges every pod on every reconcile, it hangs *all* of
   them within a minute. An operator-written attribute that can hang slapd is a
   cluster-wide outage with no self-healing, which is a different risk tier from
   anything R1 previously contained.

3. **A converged attribute must reach a proven no-write steady state.** The
   monitor ACL was rewritten every reconcile because the operator's comparison of
   what it wrote against what slapd stores never matched. On any other attribute
   that is a wasteful log line; on this one it was the difference between "a
   modify that hangs slapd" being a one-off risk and being a certainty. Verifying
   that a new converged attribute stops writing once it matches is therefore part
   of landing it, and the check is cheap: the operator's own "aligning" log lines
   must go quiet.

## Amendment (2026-09-13, same day): the vetting standard extends to what a value DOES at runtime

The amendment above established that R1 membership is empirical: an attribute is
converged only after a live modify left the pod serving. The syncrepl stanza
hardening (finding 11) passed that test — writing `olcSyncRepl` with
`timeout=300` in every stanza modifies cleanly — and still shipped a
cluster-freezing defect, because the test answers only "does the *write*
hurt slapd", not "does the *written value* hurt slapd afterwards".

What `timeout=` actually does, verified in the 2.7.1 source after three
measured ~300 s full-server freezes on a live three-site mesh: supplying it
flips slapd's refresh-phase waiting discipline from a non-blocking peek to a
blocking wait. `do_syncrep2` polls with `tout={0,0}` only in the persist phase
and otherwise blocks a threadpool thread inside `ldap_result` for up to the
timeout (`servers/slapd/syncrepl.c:1357-1362`); the task can honour a pause
only between messages (`syncrepl.c:2128`); and every external `cn=config` MOD
pauses the whole pool with listeners suspended (`bconfig.c:6512`). So while any
consumer is in refresh phase — every bring-up, pod replacement, and
stale-cookie full refresh, which is precisely when the operator writes
`cn=config` most — each config write froze the entire server for the remainder
of the blocking wait: `RESULT err=0 etime≈300`, nothing logged, nothing
accepted, cascading across pods and sites whose consumers were mid-refresh
against the frozen provider. The wait bought nothing in exchange: expiry maps
to `SYNC_TIMEOUT` ("nothing to read, listen for more", `syncrepl.c:2352`),
never an abort or a retry, so it was not even a failure detector. The landing
analysis had source-verified the question it feared — "will `timeout=` abort
the persistent search?" (it will not) — and never asked the question that
mattered: "may a syncrepl pool task block at all?" Under slapd's
cooperative-pause regime the answer is no; a blocking wait inside a pool task
turns every config write into a server freeze of that wait's length, at any
value, so no smaller number is safe either.

The fix is the unconfigured default: no `timeout=` in any stanza, failure
detection left to the socket-level mechanisms that act without occupying a
pool thread — `network-timeout=10` for connect and TLS handshake, the
keepalive triple for established flows. Accepted residual, documented rather
than designed against: the synchronous syncrepl bind (`ldap_sasl_bind_s`) is
unbounded against a provider that completes the TLS handshake and then hangs.
That was the status quo for the project's entire pre-regression life; every
hang state observed so far stalls the handshake itself, which
`network-timeout` bounds; and the only bound available is `timeout=`, i.e.
the defect.

Consequence for the doctrine: **the empirical test is two-sided.** "We
modified it on a running pod and the pod was still serving afterwards" clears
the write; a value that changes slapd's *runtime execution behaviour* — a
timeout, a wait, anything a worker thread consults while holding pool
resources — must also be vetted for what the running system does with it, and
the interaction to check first is the `cn=config` pause, because the operator
converges every pod on every reconcile and therefore *will* write config while
the value's worst case is live. The syncrepl ledger entry of the same date in
`docs/reconcile-loop-fixes.md` carries the measurements.

## Amendment, 2026-09-21: R1 says when a tunable converges, not into which entry

`olcPasswordHash` was converged onto `cn=config`, the global entry, which
OpenLDAP 2.7 deprecates — it says so on every pod start:

    olcPasswordHash: value #0: setting password scheme in the global entry is
    deprecated. The server may refuse to start if it is provided by a loadable
    module, please move it to the frontend database instead

It now goes to `olcDatabase={-1}frontend,cn=config`, which `slaptest` conversion
already creates on every pod we bootstrap (verified live, alongside
`olcDatabase={0}config` and the data databases).

**Why it mattered despite being a warning.** With the default `{SSHA}` — built
in — nothing can refuse to start, and nothing did. The failure this removes is
the one the backlog contemplates: ship `pw-argon2`, let someone set `{ARGON2}`,
and the value sits in the entry slapd says it may reject *at startup*, on a pod
that was serving a moment earlier. A deprecation warning naming a future hard
failure is worth acting on before the feature that triggers it, not after.

**What this adds to the placement model.** R1 (converged-per-pod) answered
*when* a tunable is written and said nothing about *where*. Three classes tell
you whether the operator may write an attribute at all; none of them tell you
which entry it belongs in, and for exactly one attribute slapd cares. The rule:
placement is per attribute, expressed as data (`tunableEntryDN`) rather than as
a literal at the call site, so the answer is testable and visible in one place.

**The migration is a delete on cn=config**, which ADR-026 R2 would forbid if the
value belonged to someone else. It does not: this operator wrote it and
converges it, so retiring its own copy is not "destroying state it does not
own". It runs as its own convergence step rather than as a tail on the write —
inside the write branch it would fire only on the pass that changed the value,
so a delete that failed once would never be retried, because the following pass
takes the already-correct early return.
