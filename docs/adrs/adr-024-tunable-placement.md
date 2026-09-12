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
