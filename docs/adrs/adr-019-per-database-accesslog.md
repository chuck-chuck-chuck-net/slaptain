# ADR-019: One accesslog database per replicated data database

**Status:** Accepted
**Date:** 2026-08-25

## Context

The operator provisions exactly **one** accesslog database per pod —
`cn=accesslog`, backed by the single `/accesslog` PVC — and shares it across
every `SlapdDatabase` CR. Each data DB's `overlay accesslog` sets
`olcAccessLogDB: cn=accesslog` (`slapddatabase_controller.go:881`) and every
delta-syncrepl consumer stanza reads `logbase="cn=accesslog"` (`:1850`, `:1988`).
`ensureAccesslogDB` (`:1099`) keys the DB on the suffix `cn=accesslog` alone, and
says so in its own doc comment: *"the accesslog DB is shared across all
SlapdDatabase CRs"*.

That is correct and e2e-covered for one data DB, which is the only shape any
fixture declares. Issue #2 raised the multi-DB case as a suspected latent risk
and asked whether the conventional OpenLDAP layout — one accesslog per data DB —
is actually required, or whether the shared log could be made correct with a
suffix-scoped `logfilter`.

It is required. The shared log does not degrade gracefully in the multi-DB case;
it destroys delta-syncrepl outright. Four facts, read out of the slapd sources
(OpenLDAP 2.6.x as shipped by Debian trixie; verified unchanged on `master`).

### Fact 1: the consumer's delta-sync search carries no DN scoping

In `SYNCLOG_LOGGING` state the consumer searches the *log*, not its own suffix
(`servers/slapd/syncrepl.c:741-770`):

```c
	/* Use the log parameters if we're in log mode */
	if ( si->si_syncdata && si->si_logstate == SYNCLOG_LOGGING ) {
		...
			filter = si->si_logfilterstr.bv_val;
			scope = LDAP_SCOPE_SUBTREE;
		...
		base = si->si_logbase.bv_val;
```

`base` is `logbase`, `filter` is `logfilter` verbatim, scope is subtree. Nothing
narrows the search to `si_base`. A consumer of `dc=a` therefore receives **every**
log entry newer than its cookie, including entries recording writes to `dc=b`.

### Fact 2: the foreign change is applied to the wrong backend, and the resulting error is a delta-sync kill switch

`syncrepl_message_to_op` takes the target DN straight from the log entry's
`reqDN` and the backend from the syncrepl stanza, with no suffix test between
them (`syncrepl.c:3229`):

```c
	op->o_bd = si->si_wbe;
	...
		if ( !ber_bvstrcasecmp( &bv, &ls->ls_dn ) ) {
			bdn = bvals[0];
			REWRITE_DN( si, bdn, bv2, dn, ndn );
			...
			op->o_req_dn = dn;
			op->o_req_ndn = ndn;
```

So an `ou=…,dc=b` DN is submitted to the `dc=a` mdb backend. Parent lookup
fails, `NO_SUCH_OBJECT` comes back — and that is one of five result codes slapd
treats as "the log is no longer a usable basis for incremental update"
(`syncrepl.c:1574-1592`):

```c
logerr:
					switch ( rc ) {
					case LDAP_ALREADY_EXISTS:
					case LDAP_NO_SUCH_OBJECT:
					case LDAP_NO_SUCH_ATTRIBUTE:
					case LDAP_TYPE_OR_VALUE_EXISTS:
					case LDAP_NOT_ALLOWED_ON_NONLEAF:
						rc = LDAP_SYNC_REFRESH_REQUIRED;
						si->si_logstate = SYNCLOG_FALLBACK;
						ldap_abandon_ext( si->si_ld, si->si_msgid, NULL, NULL );
						bdn.bv_val[bdn.bv_len] = '\0';
						Debug( LDAP_DEBUG_SYNC, "do_syncrep2: %s delta-sync lost sync on (%s), switching to REFRESH\n",
							si->si_ridtxt, bdn.bv_val );
						if (si->si_strict_refresh) {
							slap_suspend_listeners();
							connections_drop();
						}
```

The consumer abandons the search and falls back to a full refresh of its own
suffix. On success it returns to logging mode (`syncrepl.c:1813-1819`), so the
state does not stick — it *oscillates*.

**Net effect with two replicated DBs on one log: every write to DB-A forces every
DB-B consumer into a full refresh of DB-B, and vice versa.** Under sustained
writes on either DB, both DBs sit permanently in full-refresh syncrepl. This is
not a scoping inefficiency; it is the complete loss of delta-syncrepl, with
whole-DIT reloads as the steady state, on a cluster that reports itself healthy.

### Fact 3: log entries carry the *data* DB's CSN, and the refresh cookie is the *log* DB's contextCSN

`accesslog_response` stamps each log entry with the originating operation's CSN
rather than minting a fresh one (`overlays/accesslog.c:1710`,
`slap_queue_csn( &op2, &op->o_csn )`). That is precisely what makes delta-syncrepl
work at all: the consumer's cookie is its *data* DB's contextCSN, and it is
directly comparable to the log entries' `entryCSN`.

It also means one shared log multiplexes the CSN streams of every data DB
writing into it, under the same serverID. Two consequences on the provider side
(`overlays/syncprov.c`):

- per entry, the cookie is that entry's own CSN — correct (`:3067`);
- at refresh completion, the cookie is `ss->ss_ctxcsn`, the **log DB's**
  contextCSN — the maximum across all data DBs feeding it (`:3081`).

The consumer writes that cookie as its *data* DB's contextCSN
(`syncrepl.c:1809-1811`). A `dc=a` consumer can therefore ratchet `dc=a`'s
contextCSN past CSNs that were minted by `dc=b` writes; a `dc=a` change that
commits slightly later with a lower CSN is then discarded by `check_csn_age` as
*"CSN too old"* (`syncrepl.c:1261`). Silent divergence, and the falsified
contextCSN propagates through the mesh as `dc=a`'s state — which would also
corrupt our own `ReplicationConverged` condition and `slctl inspect`'s CSN
checks. The per-backend pending-CSN list that normally prevents contextCSN from
overtaking an uncommitted CSN (`ctxcsn.c`, `slap_get_commit_csn` on
`op->o_bd->bd_self`) is per-backend, so the ordering guarantee does not survive
two data DBs feeding one log.

`minCSN` on the log DB is likewise a single per-serverID vector
(`accesslog.c:2114-2165`, consumed at `syncprov.c:3457-3505`): purge thresholds
and staleness verdicts would be computed across both DBs' traffic.

### Fact 4: purge is configured per data DB but acts on the whole log

`logpurge` / `olcAccessLogPurge` is an attribute of the **overlay** — i.e. of the
data DB — while the entries it deletes live in the log DB. We already expose it
per CR as `SlapdDatabase.spec.replication.accesslogPurge`
(`slapddatabase_controller.go:885`). With a shared log, each data DB's overlay
runs its own purge task over the *whole* log, so the shortest retention wins
globally: setting a tight `accesslogPurge` on DB-A silently truncates DB-B's
journal, and DB-B's consumers fall back to full refresh. A per-CR field would be
quietly cluster-global. This one needs no source reading — it follows from where
the attribute lives.

### What upstream says, and what upstream tests

The guides are 1:1 throughout and state that *"an accesslog database is unique to
a given provider. It should never be replicated."* Zytrax is explicit that the
binding is per target DIT and that *"multiple accesslog DITs can also appear in a
single LDAP server"*, each target DIT *"reference[ing] a different accesslog
suffix"*.

The direct confirmation is an openldap-technical thread whose subject is Fact 2's
log line — *"Trouble with delta-syncrepl MMR: delta-sync lost sync on X,
switching to REFRESH"*. Two databases (`cn=config` and the primary DB), one
shared accesslog. Quanah Gibson-Mount's diagnosis, verbatim:

> Why do you have your cn=config db reading from the same accesslog for
> replication as your primary DB?
>
> If you are going to set up cn=config AND your primary db both as
> delta-syncrepl, you're going to need 2 different accesslog DBs.

The reporter confirmed replication worked for both databases after splitting the
logs.

Upstream's own regression suite has **zero** coverage of a shared log:
`tests/data/slapd-deltasync-{provider,consumer}.conf` and every delta-MMR script
(`test043`, `test063`, `test069`, `test070`, `test086`) configure exactly one
data DB and one `cn=log`. The shared layout is not a lightly-tested path — it is
a path upstream never exercises.

## Options considered

**Prove the shared log correct with a suffix-scoped `logfilter`.** Rejected on
three counts. It is *expressible* — `dnSubtreeMatch` exists
(`schema_init.c:6528`), so
`(&(objectClass=auditWriteObject)(reqResult=0)(reqDN:dnSubtreeMatch:=dc=a,…))`
parses and would fix Fact 2. But (a) it does nothing about Fact 3: the
refresh-completion cookie is still the shared log's contextCSN, trading a loud
failure for a silent-divergence class, which is a strictly worse trade; (b) it
does nothing about Fact 4; (c) it is unindexed — `back-mdb`'s `ext_candidates`
special-cases only `entryDN` and otherwise returns `MDB_IDL_ALL`
(`back-mdb/filterindex.c:493`), so every delta-sync refresh would scan the entire
accesslog. Paying an unindexed full-log scan to buy a silent-corruption class, in
order to save one LMDB directory, is not a trade worth making.

**Keep the shared log and forbid multiple replicated DBs in the CRD.** Rejected.
It is honest about today's state but bakes a slapd implementation detail into our
API surface, and ADR-004 deliberately makes `SlapdDatabase` a first-class
multi-instance resource. The constraint would also be invisible until replication
is enabled on the second CR, i.e. discovered in production.

**Document the hazard, change no code.** Rejected for the same reason ADR-018
rejected it: a documented "don't do that" whose violation manifests as silent
full-refresh thrash with a healthy-looking `status.phase` is not a mitigation.

**Split the log per data DB, one PVC.** Chosen.

**Split the log per data DB, one PVC *per* log.** Rejected. The issue assumed
per-DB logs would need an extra volume per DB; they do not. LMDB needs a
directory, not a mount, so `/accesslog/<dbname>` inside the existing accesslog
PVC is sufficient. Extra `volumeClaimTemplates` would add per-DB PVC lifecycle,
per-DB sizing decisions, and — via ADR-018 — more PVCs for co-located Jobs to
lease, all for no isolation we need.

## Decision

**Every replicated `SlapdDatabase` gets its own accesslog database. The accesslog
is a per-data-DB resource, owned by the `SlapdDatabase` that logs into it, never
a cluster-shared one.**

Derived rules, binding on all present and future accesslog handling:

- **R1 — Naming is keyed on the `SlapdDatabase` CR name.** Suffix
  `cn=accesslog-<dbname>`, backing directory `/accesslog/<dbname>`. The CR name
  is already the key for `<dbname>-credentials` and for `DATABASE_DIRS`; reusing
  it keeps one identity per database across Secrets, volumes and cn=config. Do
  not key on the LDAP suffix — it can contain characters that are awkward in a
  path and it is not the operator's identity for the object.
- **R2 — The layout is uniform, never conditional on cluster history.** A
  single-DB cluster uses `cn=accesslog-<dbname>` exactly like a five-DB one.
  Splitting only once a second replicated DB appears was considered and rejected:
  it would make the layout depend on the order CRs were created, and would
  perform the split *during* the very transition where the shared log is
  hazardous.
- **R3 — One PVC, per-DB directories.** `/accesslog` stays a single volume, with
  one LMDB directory per database beneath it. `SlapdCluster.NeedsAccesslogVolume`
  and the `volumeClaimTemplates` are unchanged. The init container keeps
  provisioning the mount and loading the `accesslog`/`syncprov` modules; per-DB
  directory creation belongs to whoever creates the DB.
- **R4 — Every per-DB knob stays per-DB.** `accesslogPurge` and the
  `olcDbIndex`/`olcDbMaxSize` of the log DB are properties of that database's
  log and must not be able to affect another database's journal (Fact 4). This is
  the rule that makes the existing CRD field mean what it says.
- **R5 — `logbase` and `olcAccessLogDB` are derived from the same key, in one
  place.** The overlay's `olcAccessLogDB`, the consumer stanzas' `logbase`
  (in-cluster *and* external — ADR-003 keeps the operator as the single source of
  truth for syncrepl), and the DB's own `olcSuffix` must never be able to drift
  apart. One helper computes the suffix from the CR; nothing else spells
  `cn=accesslog` literally.
- **R6 — `logfilter` stays the upstream-standard filter.** With per-DB logs the
  log is already scoped, so
  `(&(objectClass=auditWriteObject)(reqResult=0))` is correct *and* indexable. Do
  not add `reqDN` scoping as belt-and-braces: it would be dead weight on an
  unindexed attribute assertion (see the rejected option) and would mask a
  mis-derived `logbase` instead of failing loudly.
- **R7 — RID allocation is untouched.** ADR-003's scheme (`ridBase+i+1`
  in-cluster, `ridBase+50+j+1` external, unique `ridBase` per database) already
  separates stanzas per database. Only `logbase` changes inside a stanza.
- **R8 — Migration is a teardown, not a rename.** `olcSuffix` and
  `olcDbDirectory` are not runtime-mutable, so converging a legacy
  `cn=accesslog` means removing it and creating the per-DB log. Order matters and
  mirrors the existing add/remove ordering (`slapddatabase_controller.go:461-467`):
  drop the data DB's accesslog overlay, delete the old log DB, create the new log
  DB, re-add the overlay pointing at it. Losing a change journal is
  cheap and self-healing — it costs each consumer exactly one full refresh, which
  is the same `SYNCLOG_FALLBACK` path slapd takes for a purged log.

  *Amendment, 2026-08-25 (implementation).* Two refinements found while building
  this, both keeping the four steps and the constraint that forced their order
  (slapd validates `olcAccessLogDB` against an existing database):

  1. **Create the per-DB log first**, then drop the overlay, then delete the old
     log, then re-add the overlay. Creating an as-yet-unreferenced database is
     safe, and it changes the failure mode when `/accesslog/<dbname>` does not
     exist yet — which happens on a pod whose init container predates the
     per-database directory creation and has not restarted. `back-mdb` will not
     create `olcDbDirectory`, so the add fails and the reconcile retries. Under
     the original order the pod would already have lost its overlay and old log
     and would sit journal-less until restarted; creating first means a failed
     add changes nothing and the pod keeps journalling into the legacy log until
     it rolls. Retry-and-converge, not wedging, and non-destructive.
  2. **Deleting the old log is reference-counted, not per-database.** On a
     cluster where two databases share the legacy log, an unconditional
     per-database delete tears the log out from under the other database's live
     overlay. The old log is deleted only once no accesslog overlay on that pod
     still names it. This makes the operation order-free — whichever database
     reconciles last reaps it — and self-healing: because the delete decision is
     independent of the overlay-drop decision, a log left orphaned (by a
     concurrent observation, or by a database demoted out of delta-sync) is
     reaped by the next reconcile of any database on that pod that still wants
     an accesslog. A pod where *every* database has been demoted keeps the
     orphan indefinitely; it is unreferenced and harmless.

  *Amendment, 2026-08-25 (first live run).* Two defects the design above did not
  anticipate, both found only by running the migration on a real cluster and both
  now constraints on future code:

  3. **Never reuse an `olcDatabase={N}` DN across a database delete.** slapd
     renumbers every database ordered after a deleted one, so a DN resolved
     before the delete may name a *different* database after it. This is ordinary
     history, not an edge case: in a legacy cluster the shared log is created on
     the first replicated database's reconcile, so a database added later sits
     above the log and slides down when it is reaped. The first implementation
     cached the data DB's DN and then added the accesslog overlay to whatever had
     slid into that slot — attaching a journal to a *journal*, which reproduces
     Fact 2 permanently between the two logs, on all pods, with nothing ever
     removing it. Every step that can delete a database now reports it and the
     caller re-resolves. The same staleness existed latently on the ADR-010
     demotion path.

     Because the mis-attached overlay is not self-correcting, `ensureAccesslogDB`
     also reaps an accesslog overlay found on an accesslog database — narrowly:
     only what is positively attributable to this bug, never "anything the
     operator did not put here", since `cn=config` is node-local and hand-editable
     (ADR-002).

  4. **A syncrepl stanza must not be written to a pod whose accesslog DB does not
     exist yet.** The stanza rewrite is per-database and sat outside the per-pod
     loop, so a pod whose log add was still failing (no `/accesslog/<dbname>`
     until it rolls) had its `logbase` repointed at a log it did not have — which
     halts its replication outright, per the Consequences correction below. Pods
     outside the healthy set now keep the stanzas they have and are retried. This
     is the order ADR-010 already prescribes for consumer-only → peer promotion
     (accesslog DB and overlays before in-cluster stanzas), and ADR-003 is
     untouched: the operator remains the sole author of every stanza. Deferring is
     the conservative direction — nothing is removed, and a graceful "fall back to
     plain syncrepl" was rejected as a topology decision ADR-011 reserves for
     humans.

  Detection requires `olcSuffix: cn=accesslog` **and**
  `olcDbDirectory: /accesslog` to match, exactly and case-folded. Suffix-only
  matching would delete a hand-made `cn=accesslog` this operator never created;
  prefix matching would destroy `cn=accesslog-<dbname>` on every healthy
  cluster. R5's "nothing spells `cn=accesslog` literally" governs *live* naming —
  a migration must be able to name what it migrates away from, so the legacy
  suffix is spelled once, in the migration code.
- **R9 — `logbase` names the *remote* log, so it is peer-facing configuration,
  and it belongs to the database.** `logbase` is a search base sent to the
  provider (Fact 1), so an external-peer stanza must spell the *peer's* accesslog
  suffix, not the local one. While the suffix was the constant `cn=accesslog`
  this was uniform by luck — and only by luck: `cn=accesslog` is a convention,
  not a rule (upstream's own regression suite uses `cn=log`). ADR-011 explicitly
  supports `syncMode: delta` against a non-slaptain source, so deriving the
  remote suffix from a local CR name would *regress* a supported configuration.

  The rule: the external stanza's `logbase` is derived from this database's own
  accesslog suffix by default, and overridable per database via
  `SlapdDatabase.spec.replication.externalAccesslogSuffix`. It goes on the
  database, not on `ExternalPeer`: `spec.replication.externalPeers[]` lives on
  `SlapdCluster` and carries no database selector — the same peer list is
  resolved and applied to every `SlapdDatabase` — so a per-peer field would be
  one value spanning all databases, which is the wrong axis for a per-database
  journal. Plain-syncrepl peers (ADR-011) are unaffected; they emit no `logbase`.

  Accepted limitation: one value per database, so a database consuming delta
  from two *different* foreign providers with differing log suffixes is not
  expressible. ADR-011's migration topology is one `ExternalPeer` per data DB
  pointing at the source, so this is not a live case; the escape, if it ever
  becomes one, is per-peer-per-database, and it is deliberately not pre-built.

- **R10 — The derived default is computed at the point of use and never written
  back to `.spec`.** No `+kubebuilder:default=` on the override field (a
  kubebuilder default is a constant and this one depends on the CR's own name),
  and no controller write-back into the spec. Empty means "derive"; the
  derivation lives in the same single helper as R5 and is called from the stanza
  builder. This is the `spec.images` pattern (operator-side defaulting, no
  CRD-baked default) and it is chosen specifically to avoid the failure mode
  where a controller-populated spec field diverges from what the user applied and
  drives reconcile churn through server-side-apply field ownership.

## Consequences

- **Multi-DB replicated clusters become correct by construction** rather than
  latent-risk. ADR-013's deferral of *hot* `SlapdDatabase` add/remove is
  unchanged and unrelated; this removes a correctness landmine from the path
  ADR-013 defers the UX of.
- **The accesslog stops being a cross-CR shared resource**, which is a strict
  improvement for ADR-004: `ensureAccesslogDB`/`removeAccesslogDB` become
  per-CR operations with no convergence-on-a-shared-object caveat, and the
  ADR-010 peer→consumer demotion teardown no longer risks removing a log another
  CR is still using.
- **One-time migration cost on existing clusters:** one full refresh per
  consumer per database, at first reconcile after upgrade. On a single-DB cluster
  that is the whole DIT once. Acceptable, and cheaper than any alternative that
  preserves the journal.
- **Stale LMDB files are left behind at the `/accesslog` root** after migration
  (the operator has no filesystem access to that volume — ADR-018 —
  and a co-located Job to delete two files is not worth the lease). They are
  bounded by the old DB's `olcDbMaxSize` and reclaimed whenever the accesslog PVC
  is recycled, which is always safe: the accesslog holds only a journal.
- **A new peer-facing failure mode to keep visible.** A `logbase` that names a
  suffix the provider does not have stops that consumer's replication outright.
  R9's derivation plus the `slctl` check below are what keep it visible; a
  mismatched mesh, or a migration source with a `cn=log`-style suffix, must be
  diagnosable without reading provider logs.

  *Correction, 2026-08-25 (observed live).* This bullet originally predicted a
  graceful degradation — "the remote search finds nothing, the consumer falls
  back, and the peer degrades to full-refresh syncrepl exactly as in Fact 2".
  **That is wrong, and it is wrong in the dangerous direction.** A missing
  `logbase` is a missing *search base*: the log-mode search returns
  `noSuchObject` and the session aborts before any fallback can run —

  ```
  do_syncrep2: rid=101 LDAP_RES_SEARCH_RESULT (32) No such object
  do_syncrepl: rid=101 rc -101 retrying
  ```

  — so replication **halts and retries indefinitely; no data moves at all**.
  Fact 2's `SYNCLOG_FALLBACK` is reached only from the `logerr` switch, i.e. when
  the log search *succeeds* and applying an entry fails; a base that does not
  exist never gets there. Measured on a cluster in this state: an entry written
  on pod-0 was still absent on the other two pods minutes later. Mechanically
  this is louder than predicted, but it is still silent in `status.phase` terms,
  which is why the `slctl` check matters. It is also why the operator must never
  write a stanza to a pod whose accesslog DB it has not yet managed to create —
  see the stanza-deferral note under R8.
- **`SlapdDatabase` gains one optional field**,
  `spec.replication.externalAccesslogSuffix` (R9/R10). CRD regeneration and chart
  CRD sync follow; no defaulting webhook, no spec write-back.
- **`slctl inspect` gains a real check here.** Its stanza/topology consistency
  checks should assert that every replicated database's `logbase`,
  `olcAccessLogDB` and log-DB `olcSuffix` agree, and that no two databases share
  a log — the exact condition this ADR forbids, cheaply observable per pod.
- **e2e needs a two-replicated-database fixture.** Per Test Discipline this is
  the red-first artefact: against current code, two replicated `SlapdDatabase`
  CRs on a multi-replica cluster must reproduce Fact 2 (`delta-sync lost sync on
  …, switching to REFRESH`, and DIT divergence under interleaved writes). No
  existing fixture declares more than one `SlapdDatabase`, so this is new
  coverage, not an adjusted assertion.
- **Access control on the log is handled by ADR-020**, implemented in the same
  pass. The gap it closes — a log with no `olcAccess` inherits slapd's default
  *read*, so a data DB's ACLs are bypassable through its own journal — is
  independent of this ADR, but per-database logs are what make "the ACL of *this*
  log" well-defined.
- **The init container creates `/accesslog/<name>` for every database in
  `DATABASE_DIRS`, replicated or not.** `back-mdb` does not create
  `olcDbDirectory`, so the directory must exist before the operator adds the log
  DB; the init container is the only component with filesystem access to that
  volume (ADR-018). Filtering the set to replicated databases only would need a
  second, replication-filtered env var, and would couple the init container to
  per-DB replication state that can change without the pod restart ADR-013
  guarantees for a DB add/remove. A non-replicated database therefore gets an
  empty, unused directory — the cheaper and more robust contract.
- **Out of scope, tracked separately:** the log DB's index set is `default eq` plus
  `reqEnd,reqResult,reqStart eq`, where upstream indexes
  `entryCSN,objectClass,reqEnd,reqResult,reqStart,reqDN`. `reqDN` in particular
  is on a hot path — MMR out-of-order modify resolution searches the local log
  with `(&(entryCSN>=…)(reqDN=…)…)` on every conflicting write
  (`syncrepl.c:3084-3105`). Worth fixing; independent of this ADR.

## Related

- ADR-002 — cn=config is node-local; the accesslog DB and its overlay are
  per-pod cn=config state, applied over the headless service like everything else
  here.
- ADR-003 — the operator owns all syncrepl configuration. R5's single-source-of-truth
  requirement for `logbase` is that principle applied to the log reference.
- ADR-004 — multi-resource CRD architecture. This ADR removes the last
  cluster-shared, cross-`SlapdDatabase` resource from the replication path.
- ADR-010 — replication modes; peer→consumer demotion is what calls
  `removeAccesslogDB`, and becomes per-database here.
- ADR-011 — hot migration topology. `syncMode: delta` against a non-slaptain
  source is a supported configuration, which is what makes R9's override
  mandatory rather than speculative; `syncMode: plain` peers emit no `logbase`.
- ADR-013 — persistence is required and hot DB add/remove is deferred. The
  deferral is why this was latent; it is not a reason to leave it latent.
- ADR-018 — a pod object is a PVC deletion lease; the reason R3 keeps one
  accesslog PVC rather than one per database.
- ADR-020 — an accesslog is at least as restrictive as the database it journals;
  independent gap, implemented in the same pass, and made expressible by the
  per-database split decided here.

## References

All public. slapd source line numbers are from `master` at the time of writing
and are stable enough to locate the code; the mechanism is identical in the
2.6 release branch.

- OpenLDAP Administrator's Guide 2.7, *Replication* (delta-syncrepl provider and
  consumer configuration; "an accesslog database is unique to a given provider"):
  <https://www.openldap.org/doc/admin27/replication.html>
- OpenLDAP Administrator's Guide 2.6, *Overlays* — Access Logging:
  <https://www.openldap.org/doc/admin26/overlays.html>
- `slapo-accesslog(5)` — `logdb`, `logops`, `logsuccess`, `logpurge`, `logbase`:
  <https://www.openldap.org/software/man.cgi?query=slapo-accesslog>
- Zytrax, *LDAP for Rocket Scientists* ch. 6, accesslog overlay — per-target-DIT
  binding, multiple accesslog DITs per server:
  <https://www.zytrax.com/books/ldap/ch6/accesslog.html>
- openldap-technical, *"Trouble with delta-syncrepl MMR: delta-sync lost sync on
  X, switching to REFRESH"* (Oct 2013) — shared accesslog across two
  delta-syncrepl'd databases, and the "you're going to need 2 different accesslog
  DBs" diagnosis:
  <https://www.openldap.org/lists/openldap-technical/201310/msg00274.html>
- slapd sources, `git.openldap.org/openldap/openldap`:
  - `servers/slapd/syncrepl.c` — log-mode search parameters (`:741-770`),
    `check_csn_age` (`:1261`), the `logerr` fallback switch (`:1574-1592`),
    return to logging mode (`:1813-1819`), `syncrepl_message_to_op` (`:3180`),
    MMR conflict resolution's `reqDN` search (`:3084-3105`)
  - `servers/slapd/overlays/accesslog.c` — log entry inherits the data op's CSN
    (`:1710`), `minCSN` maintenance (`:2114-2165`)
  - `servers/slapd/overlays/syncprov.c` — per-entry cookie (`:3067`) vs.
    refresh-completion cookie from the log DB's contextCSN (`:3081`), `minCSN`
    staleness gate (`:3457-3505`)
  - `servers/slapd/ctxcsn.c` — `slap_get_commit_csn`, the per-backend pending-CSN
    ordering guarantee
  - `servers/slapd/schema_init.c` — `dnSubtreeMatch` (`:6528`)
  - `servers/slapd/back-mdb/filterindex.c` — `ext_candidates` indexes only
    `entryDN` (`:493`)
- Upstream regression suite, one data DB + one log throughout:
  `tests/data/slapd-deltasync-provider.conf`,
  `tests/data/slapd-deltasync-consumer.conf`,
  `tests/scripts/test063-delta-multiprovider`

## Amendment (2026-09-12): the log DB's index set

Log DBs are created with
`olcDbIndex: entryCSN,objectClass,reqEnd,reqResult,reqStart,reqDN eq` —
upstream's delta-syncrepl set. The original `default eq` +
`reqEnd,reqResult,reqStart eq` was incomplete: `index default <type>` sets only
the fallback *type* for attributes listed without one and indexes nothing by
itself (slapd-mdb(5)), and `reqDN` is asserted by multi-provider out-of-order
modify resolution — `(&(entryCSN>=…)(reqDN=…)…)` against the *local* log on
every conflicting write (`syncrepl.c`) — so a write-contended mesh was doing
unindexed scans on a hot path.

The operator converges the set on every reconcile (add-missing only, never
replace: back-mdb rejects a duplicate definition for an already-indexed
attribute), which upgrades log DBs created by earlier operators in place — per
slapd-mdb(5), a cn=config `olcDbIndex` modify rebuilds the indices online in a
background task, so no restart and no `slapindex`. Performance, not
correctness: this ADR's decisions are unchanged.

## Amendment (2026-09-13): the external stanza's bind identity is per-database too

R9 established that `logbase` is per-database because `externalPeers[]` lives on
`SlapdCluster` and carries no database selector — the same peer list is applied
to every `SlapdDatabase`, so a per-peer field is one value spanning all
databases, the wrong axis for per-database semantics. The bind identity has the
identical disease and was not fixed with it: external stanzas bound as
`ExternalPeer.bindDN` with the password from `bindPasswordSecretName`, one
identity for every database, while in-cluster stanzas derive
`cn=replication,<suffix>` + the database's own secret.

With two replicated databases at most one of them can bind as its own
replication identity. The other's consumers bind as a *foreign* database's
identity — which ADR-020's log ACL rightly denies on this database's accesslog,
so the log-mode search fails `noSuchObject` and, per this ADR's corrected
Consequences, **its delta-syncrepl halts outright** (err=32 → rc -101, retried
forever). The data-DB refresh half-works in the meantime and strips every
attribute the database's ACLs deny that foreign DN — measured live: a
`cn=replication,<suffix>` entry replicated cross-site *without its
`userPassword`* (the 2026-04-17 strip class). Found on a three-site mesh where
the second fixture database had been silently non-replicating cross-site since
the fixture landed; full record in `docs/reconcile-loop-fixes.md` (2026-09-13).

The rule, extending R9/R10 to the bind identity: **when
`ExternalPeer.bindDN` / `bindPasswordSecretName` are unset, the external
stanza's bind identity derives per database — `cn=replication,<suffix>` with
that database's replication password, the identity ADR-020 R2 already requires
external consumers to present and the one the in-cluster stanzas use.** Set
fields win verbatim: they remain the ADR-011 escape for a foreign source with
its own bind account (where one peer serves one database, so the cluster-level
axis is harmless). Derived at the point of use, never written back (R10). One
deliberate asymmetry: a `bindPasswordSecretName` that was named but yielded no
password keeps the empty credential rather than borrowing the per-database
one — "could not read it" is not "not set", and a silent substitution would
mask the broken override instead of failing loudly at the consumer.

Same accepted limitation as R9: the override is one value per peer across all
databases, so a mesh peer override cannot be right for two databases at once —
in a slaptain↔slaptain mesh, leave the fields unset. Per-peer-per-database is
deliberately not pre-built.

## Amendment (2026-09-14): R8 is withdrawn — the operator no longer migrates a legacy shared log

R8 said migration is a teardown and gave it four ordered steps. Two later
amendments hardened it: create the new log first, and reference-count the delete
of the old one so a pod with two databases on one log could not have the log
yanked out from under a live overlay. **Both hold as analysis. The
reference-counted delete is nevertheless unsafe, and it is withdrawn along with
the rest of the automation.**

### What went wrong, mechanically

Measured 2026-09-14 on a three-site lab: one pod left in CrashLoopBackOff, 16
downstream specs failed.

```
accesslog: "logdb <suffix>" missing or invalid.
backend_startup_one (type=mdb, suffix="…"): bi_db_open failed! (1)
```

Its persisted `cn=config` held a data database whose `olcAccessLogDB` named
`cn=accesslog` — a database that was no longer there. Its own per-database log
existed, orphaned, and the pod had served normally for ~45 minutes before the
restart that killed it.

Three facts compose it:

1. **The delete authorises itself from evidence the operator does not own.**
   `remainingRefs` counted the accesslog overlays of *other* databases, observed
   at one instant and acted on later — `planAccesslogMigration` produced a plan,
   then `removeDataDBOverlay` ran, then `removeAccesslogDBAt`. Between the
   snapshot and the delete another writer re-created a reference. `cn=config` is
   node-local and hand-editable (ADR-002), so "nobody else will touch it" was
   never a property this design could assume; here the other writer was the R8
   e2e's own pre-state manufacture, which drops and re-adds both overlays.
2. **The repair is keyed on the artifact the delete removes.** `DropOverlay` —
   the step that would have fixed the surviving overlay — fired only when the
   legacy *database* was found. Once it is gone the plan is empty by
   construction, and `ensureAccesslogOverlay` deliberately does not converge
   `olcAccessLogDB` on an existing overlay. So the state "an overlay names a
   database that does not exist" was **absorbing**: nothing observed it, nothing
   reported it, nothing repaired it.
3. **slapd validates `logdb` in two places with two strictnesses.** Online
   (LDAP add/modify) `accesslog_cf_gen` calls `select_backend` and rejects a
   suffix with no backend — `CONFIG_ONLINE_ADD(ca)` is `!(ca->lineno)` and
   `bconfig.c` sets `lineno = 0` exactly when the write arrives as an LDAP
   operation. Offline (reading `slapd.d` at startup) it only stashes the suffix,
   and `accesslog_db_open` resolves it — a NULL backend prints the line above and
   returns 1, which exits the server. **You cannot create a dangling reference
   over the wire; you can strand an existing one by deleting its target, and
   nothing notices until the next restart, which it then prevents.**

The generalised rule is ADR-026.

### Why removal rather than repair

Fixing (2) is cheap — key `DropOverlay` on the overlay's reference rather than on
the legacy database's existence. Fixing (1) is not: `cn=config` offers no
compare-and-delete and no transaction, so an authorisation that stays true
through the delete needs either a per-pod ownership marker in `cn=config` (new
persisted state, own lifecycle, own failure modes) or a delete that never
happens. That is real machinery bought for one population: a slaptain cluster
created before this ADR (2026-08-25) that still carries a shared `cn=accesslog`.

Against that: the project is alpha at v0.1.0; ADR-012 already makes a cluster
wipe an ordinary Kubernetes resource-lifecycle operation; and ADR-011's hot
migration never needed R8 — the slaptain side is built fresh with per-database
logs, and the legacy system's own accesslog is never converged by us. Removal
also deletes the fast-path `(&(objectClass=olcMdbConfig)(olcSuffix=cn=accesslog))`
search that every healthy reconcile paid on every pod, and a nondeterministic way
to make a pod unbootable.

### What replaces it

**Diagnosis, not automation.** `slctl inspect` keeps recognising the legacy
suffix and now separates two cases: an `olcAccessLogDB` naming the legacy log
*that still exists* is a note pointing at the runbook below (unsupported layout,
behaviourally correct, just misnamed); an `olcAccessLogDB` naming a database the
pod does **not** have is an **issue** stating the consequence — slapd will refuse
to start. That is the check that would have turned this incident into one red
line instead of 45 silent minutes.

**The manual runbook**, for anyone who does meet such a cluster. Scale the
operator to zero first — the concurrent writer is the entire defect — then, per
pod, as `cn=admin,cn=config`:

1. create `cn=accesslog-<dbname>` at `/accesslog/<dbname>` (the directory must
   exist; `back-mdb` will not create it, so the pod's init container must have
   run with that database in `DATABASE_DIRS`);
2. delete the data database's `olcOverlay=…accesslog` entry;
3. delete the `cn=accesslog` database — **children first**, slapd does not
   cascade — once no overlay on that pod still names it;
4. re-add the accesslog overlay with `olcAccessLogDB: cn=accesslog-<dbname>`.

Then scale the operator back up; it converges the ACL (ADR-020), the index set,
the limits and the syncprov overlay on the new log. Every `olcDatabase={N}` DN
you hold is invalidated by step 3 — re-read them (ADR-026 R1). Recreating the
cluster (ADR-012) is the supported alternative and is usually cheaper.

### Recorded and deliberately not taken

`ensureAccesslogOverlay` used to claim an existing overlay's `olcAccessLogDB`
"cannot be" converged. That is **wrong**: `olcSuffix`/`olcDbDirectory` on a
*database* are immutable, but the overlay attribute is live-modifiable and is
validated on the online path. A live repoint is therefore expressible — and it is
not taken, because it changes what a running slapd does with an already-resolved
`li_db` (`accesslog_db_open` has registered an `accesslog_db_root` runqueue task
against it), which is exactly the class ADR-024's amendment requires be vetted on
a running server, and the only thing it would buy is the path being withdrawn
here. Written down so nobody re-derives it as an obvious fix.

### What is unchanged

R1-R7, R9 and R10 stand. R8's *analysis* stays on this page as the record of why
a rename is impossible and why the four steps are ordered as they are — the
manual runbook is those steps. The per-database reap on ADR-010 peer →
consumer-only demotion (`removeAccesslogDB`) is untouched, including its
`(bool, error)` contract and the caller's re-resolve; so is the reaper for an
accesslog overlay mis-attached to an accesslog database, whose rationale now
cites the renumbering class (ADR-026 R1) rather than this migration.

**Regressed, knowingly:** nothing automatically converges a pre-ADR-019 cluster
any more.
